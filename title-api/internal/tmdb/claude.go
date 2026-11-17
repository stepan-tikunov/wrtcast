package tmdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"title-api/internal/marker"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// The model that settles a title the search can't, and its effort.
const (
	model  = "claude-sonnet-5-5"
	effort = anthropic.OutputConfigEffortMedium
)

const (
	// The turns of the conversation at most, and the web searches.
	maxTurns    = 12
	maxSearches = 5
	// The dollars a web search costs.
	searchPrice = 0.01
)

const system = `You match a title of a Russian streaming site with its entry on TMDB (themoviedb.org). You are given what the site tells of the title and the candidates TMDB's search found by its names; the site's names and year may differ a little from TMDB's (ё written as е, another translation, the year of another release).

Find the one TMDB movie or TV show that is this title. Search TMDB yourself with tmdb_search when the candidates don't have it (by the original or English name, other spellings, without articles), and search the web when the site's data is not enough to tell the candidates apart: the IMDb, Kinopoisk or Wikipedia page of the title usually settles it. The site also lists a spin-off as a later season of its main show; a title of its own is matched as named, by its own year.

Finish by calling answer, once: with the TMDB ID and type of the title, or type none when TMDB has no such title or you can't tell which it is.`

var (
	searchTool = anthropic.ToolParam{
		Name:        "tmdb_search",
		Description: anthropic.String("Searches TMDB's movies or TV shows by name and returns the first page of what it finds, Russian names where TMDB has them."),
		Strict:      anthropic.Bool(true),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{
				"query": map[string]any{"type": "string", "description": "The name to search for."},
				"type":  map[string]any{"type": "string", "enum": []string{KindMovie, KindTV}},
				"year":  map[string]any{"type": "string", "description": "The year the title came out, or empty for any."},
			},
			Required:    []string{"query", "type", "year"},
			ExtraFields: map[string]any{"additionalProperties": false},
		},
	}
	answerTool = anthropic.ToolParam{
		Name:        "answer",
		Description: anthropic.String("Gives the TMDB title that is the site's title, or type none when there is none."),
		Strict:      anthropic.Bool(true),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{
				"id":     map[string]any{"type": "integer", "description": "The TMDB ID, 0 with type none."},
				"type":   map[string]any{"type": "string", "enum": []string{KindMovie, KindTV, "none"}},
				"reason": map[string]any{"type": "string", "description": "Why, in a sentence."},
			},
			Required:    []string{"id", "type", "reason"},
			ExtraFields: map[string]any{"additionalProperties": false},
		},
	}
)

// answer is what Claude settled on.
type answer struct {
	ID     uint64 `json:"id"`
	Kind   string `json:"type"`
	Reason string `json:"reason"`
}

// ask has Claude settle which of the candidates, or of what it finds
// itself, is the title: its answer and the dollars it cost.
func ask(ctx context.Context, claude anthropic.Client, tmdb *client, t Title, candidates []Candidate) (answer, float64, error) {
	input, _ := json.Marshal(map[string]any{"title": t, "candidates": candidates})
	messages := []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(string(input)))}
	tools := []anthropic.ToolUnionParam{
		{OfWebSearchTool20260209: &anthropic.WebSearchTool20260209Param{MaxUses: anthropic.Int(maxSearches)}},
		{OfTool: &searchTool},
		{OfTool: &answerTool},
	}
	cost := 0.0
	for turn := 0; turn < maxTurns; turn++ {
		response, err := claude.Messages.New(ctx, anthropic.MessageNewParams{
			Model:        model,
			MaxTokens:    16000,
			System:       []anthropic.TextBlockParam{{Text: system}},
			Messages:     messages,
			Tools:        tools,
			OutputConfig: anthropic.OutputConfigParam{Effort: effort},
		},
			// A refusal is retried on another model.
			option.WithHeaderAdd("anthropic-beta", "server-side-fallback-2026-07-01"),
			option.WithJSONSet("fallbacks", "default"),
		)
		if err != nil {
			return answer{}, cost, err
		}
		price := marker.Prices[model]
		cost += float64(response.Usage.InputTokens+response.Usage.CacheCreationInputTokens)/1e6*price[0] +
			float64(response.Usage.CacheReadInputTokens)/1e6*price[0]/10 +
			float64(response.Usage.OutputTokens)/1e6*price[1] +
			float64(response.Usage.ServerToolUse.WebSearchRequests)*searchPrice
		switch response.StopReason {
		case anthropic.StopReasonRefusal:
			return answer{}, cost, fmt.Errorf("refused (%s)", response.StopDetails.Category)
		case anthropic.StopReasonMaxTokens:
			return answer{}, cost, errors.New("the answer ran out of tokens")
		}
		messages = append(messages, response.ToParam())
		var results []anthropic.ContentBlockParamUnion
		for _, block := range response.Content {
			use, ok := block.AsAny().(anthropic.ToolUseBlock)
			if !ok {
				continue
			}
			switch use.Name {
			case answerTool.Name:
				var result answer
				if err := json.Unmarshal([]byte(use.JSON.Input.Raw()), &result); err != nil {
					return answer{}, cost, fmt.Errorf("answer %s: %w", use.JSON.Input.Raw(), err)
				}
				return result, cost, nil
			case searchTool.Name:
				results = append(results, runSearch(ctx, tmdb, use))
			default:
				results = append(results, anthropic.NewToolResultBlock(use.ID, "unknown tool", true))
			}
		}
		switch {
		case len(results) > 0:
			messages = append(messages, anthropic.NewUserMessage(results...))
		// The server paused its own loop of searches: sent back as it is,
		// the turn goes on.
		case response.StopReason == anthropic.StopReasonPauseTurn:
		default:
			messages = append(messages, anthropic.NewUserMessage(anthropic.NewTextBlock("Call answer with what you found.")))
		}
	}
	return answer{}, cost, errors.New("no answer")
}

func runSearch(ctx context.Context, tmdb *client, use anthropic.ToolUseBlock) anthropic.ContentBlockParamUnion {
	var input struct {
		Query string `json:"query"`
		Kind  string `json:"type"`
		Year  string `json:"year"`
	}
	if err := json.Unmarshal([]byte(use.JSON.Input.Raw()), &input); err != nil || input.Kind != KindMovie && input.Kind != KindTV {
		return anthropic.NewToolResultBlock(use.ID, "invalid input", true)
	}
	found, err := tmdb.search(ctx, input.Kind, input.Query, yearOf(input.Year))
	if err != nil {
		return anthropic.NewToolResultBlock(use.ID, err.Error(), true)
	}
	text, _ := json.Marshal(found[:min(len(found), 10)])
	return anthropic.NewToolResultBlock(use.ID, string(text), false)
}
