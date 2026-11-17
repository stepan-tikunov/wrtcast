package marker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/anthropics/anthropic-sdk-go"
)

// Marker asks Claude through client, with model at effort.
type Marker struct {
	Client anthropic.Client
	Model  string
	Effort anthropic.OutputConfigEffort

	mu    sync.Mutex
	usage Usage
}

// Usage is what the requests of a Marker took.
type Usage struct {
	Requests                  int
	InputTokens, OutputTokens int64
	CacheRead, CacheWrite     int64
}

// Usage is what the requests so far took.
func (m *Marker) Usage() Usage {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.usage
}

// ask sends one user message of content under system and reads the answer,
// which the schema shapes, into result.
func (m *Marker) ask(ctx context.Context, system string, content []anthropic.ContentBlockParamUnion, schema map[string]any, result any) error {
	response, err := m.Client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(m.Model),
		MaxTokens: 16000,
		System:    []anthropic.TextBlockParam{{Text: system}},
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(content...)},
		OutputConfig: anthropic.OutputConfigParam{
			Effort: m.Effort,
			Format: anthropic.JSONOutputFormatParam{Schema: schema},
		},
	})
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.usage.Requests++
	m.usage.InputTokens += response.Usage.InputTokens
	m.usage.OutputTokens += response.Usage.OutputTokens
	m.usage.CacheRead += response.Usage.CacheReadInputTokens
	m.usage.CacheWrite += response.Usage.CacheCreationInputTokens
	m.mu.Unlock()
	switch response.StopReason {
	case anthropic.StopReasonRefusal:
		return fmt.Errorf("refused (%s): %s", response.StopDetails.Category, response.StopDetails.Explanation)
	case anthropic.StopReasonMaxTokens:
		return errors.New("the answer ran out of tokens")
	}
	var text strings.Builder
	for _, block := range response.Content {
		if block, ok := block.AsAny().(anthropic.TextBlock); ok {
			text.WriteString(block.Text)
		}
	}
	if err := json.Unmarshal([]byte(text.String()), result); err != nil {
		return fmt.Errorf("answer %q: %w", text.String(), err)
	}
	return nil
}

// object is a JSON schema of an object whose properties are all required,
// as structured outputs want them.
func object(properties map[string]any) map[string]any {
	required := make([]string, 0, len(properties))
	for name := range properties {
		required = append(required, name)
	}
	sort.Strings(required)
	return map[string]any{
		"type":                 "object",
		"properties":           properties,
		"required":             required,
		"additionalProperties": false,
	}
}

func typed(kind, description string) map[string]any {
	return map[string]any{"type": kind, "description": description}
}
