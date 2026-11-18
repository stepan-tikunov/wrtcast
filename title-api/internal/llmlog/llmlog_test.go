package llmlog

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	_ "modernc.org/sqlite"
)

// A request is kept with the model that answered, its tokens and their
// dollars; one that failed with its error.
func TestAdd(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	ctx := context.Background()
	requests, err := Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Unix(1_800_000_000, 0)
	requests.Add(Request{Purpose: "tmdb", Subject: "fight", Model: "claude-sonnet-5-5", Started: started, Response: &anthropic.Message{
		Model: "claude-opus-5-5", StopReason: anthropic.StopReasonToolUse,
		Usage: anthropic.Usage{InputTokens: 1_000_000, OutputTokens: 100_000, CacheReadInputTokens: 1_000_000,
			ServerToolUse: anthropic.ServerToolUsage{WebSearchRequests: 2}},
	}})
	requests.Add(Request{Purpose: "marking head", Model: "claude-sonnet-5-5", Started: started, Err: errors.New("overloaded")})
	(*Log)(nil).Add(Request{Purpose: "nowhere"})

	rows, err := db.Query(`SELECT t, purpose, model, input_tokens, output_tokens, cache_read, web_searches, cost,
		coalesce(stop, ''), coalesce(error, '') FROM llm_requests ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type row struct {
		t                                  int64
		purpose, model                     string
		input, output, cacheRead, searches int64
		cost                               float64
		stop, failure                      string
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.t, &r.purpose, &r.model, &r.input, &r.output, &r.cacheRead, &r.searches, &r.cost, &r.stop, &r.failure); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if len(got) != 2 {
		t.Fatalf("rows = %+v", got)
	}
	// 4 + 0.4 + 2 + 0.02 dollars at Opus' prices.
	if r := got[0]; r.t != started.Unix() || r.purpose != "tmdb" || r.model != "claude-opus-5-5" || r.input != 1_000_000 ||
		r.output != 100_000 || r.cacheRead != 1_000_000 || r.searches != 2 || math.Abs(r.cost-6.42) > 1e-9 || r.stop != "tool_use" || r.failure != "" {
		t.Errorf("first = %+v", r)
	}
	if r := got[1]; r.purpose != "marking head" || r.model != "claude-sonnet-5-5" || r.cost != 0 || r.stop != "" || r.failure != "overloaded" {
		t.Errorf("second = %+v", r)
	}
}
