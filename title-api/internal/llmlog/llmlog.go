// Package llmlog keeps each request to Claude in title-api's state
// database: when it was made, what for and of what, the model, the tokens
// and web searches it took, the dollars they cost, how long it took and
// how it ended.
package llmlog

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

const schema = `
	CREATE TABLE IF NOT EXISTS llm_requests (
		t INTEGER NOT NULL, purpose TEXT NOT NULL, subject TEXT, model TEXT NOT NULL,
		input_tokens INTEGER NOT NULL, output_tokens INTEGER NOT NULL,
		cache_read INTEGER NOT NULL, cache_write INTEGER NOT NULL,
		web_searches INTEGER NOT NULL, cost REAL NOT NULL, ms INTEGER NOT NULL,
		stop TEXT, error TEXT
	);
	CREATE INDEX IF NOT EXISTS llm_requests_t ON llm_requests (t);`

// Log keeps the requests in the llm_requests table.
type Log struct {
	db *sql.DB
}

func Open(ctx context.Context, db *sql.DB) (*Log, error) {
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return nil, fmt.Errorf("initialize the log of Claude's requests: %w", err)
	}
	return &Log{db: db}, nil
}

// Request is a request to Claude: what for (as "tmdb" or "marking head")
// and of what (a title, an episode), the model asked, when, and the answer
// it got (its usage, its stop reason and the model that answered, if one
// did), or the error.
type Request struct {
	Purpose, Subject string
	Model            string
	Started          time.Time
	Response         *anthropic.Message
	Err              error
}

// Add keeps a request; a nil Log keeps none. One that can't be kept is
// told in the log.
func (l *Log) Add(r Request) {
	if l == nil {
		return
	}
	var usage anthropic.Usage
	var stop sql.NullString
	if r.Response != nil {
		usage = r.Response.Usage
		if r.Response.Model != "" {
			r.Model = string(r.Response.Model)
		}
		stop = sql.NullString{String: string(r.Response.StopReason), Valid: r.Response.StopReason != ""}
	}
	var failure sql.NullString
	if r.Err != nil {
		failure = sql.NullString{String: r.Err.Error(), Valid: true}
	}
	_, err := l.db.ExecContext(context.Background(), `
		INSERT INTO llm_requests (t, purpose, subject, model, input_tokens, output_tokens,
			cache_read, cache_write, web_searches, cost, ms, stop, error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Started.Unix(), r.Purpose, r.Subject, r.Model, usage.InputTokens, usage.OutputTokens,
		usage.CacheReadInputTokens, usage.CacheCreationInputTokens, usage.ServerToolUse.WebSearchRequests,
		Cost(r.Model, usage), time.Since(r.Started).Milliseconds(), stop, failure)
	if err != nil {
		log.Printf("llmlog %s %s: %v", r.Purpose, r.Subject, err)
	}
}

// Prices are the dollars a million input and output tokens of a model cost.
var Prices = map[string][2]float64{
	"claude-opus-5-5":   {4, 20},
	"claude-sonnet-5-5": {2, 10},
	"claude-haiku-4-5":  {1, 5},
	// For prompts up to 100k tokens, which these are.
	"claude-haiku-5-5": {0.10, 0.50},
}

// The dollars a web search costs.
const searchPrice = 0.01

// Cost is the dollars usage of a model took: tokens written to the cache
// cost a quarter more than those read afresh, and those read from it a
// tenth.
func Cost(model string, usage anthropic.Usage) float64 {
	price := Prices[model]
	return float64(usage.InputTokens)/1e6*price[0] +
		float64(usage.CacheCreationInputTokens)/1e6*price[0]*1.25 +
		float64(usage.CacheReadInputTokens)/1e6*price[0]/10 +
		float64(usage.OutputTokens)/1e6*price[1] +
		float64(usage.ServerToolUse.WebSearchRequests)*searchPrice
}
