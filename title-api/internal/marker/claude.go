package marker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"title-api/internal/llmlog"

	"github.com/anthropics/anthropic-sdk-go"
)

// Marker asks Claude through client: each stage the model Stages has for
// it, else Model. Record, if set, is told of each request: its stage, the
// model asked, when, and the answer, as much of it as came, or the error.
type Marker struct {
	Client anthropic.Client
	Model  Model
	Stages map[Stage]Model
	Record func(stage Stage, model string, started time.Time, response *anthropic.Message, err error)

	mu    sync.Mutex
	usage map[string]Usage
}

// Model is a model and the effort it is asked at (none for its default).
type Model struct {
	ID     string
	Effort anthropic.OutputConfigEffort
}

// Stage is a step of marking.
type Stage string

const (
	StageSubtitles Stage = "subtitles"
	StageHead      Stage = "head"
	StageTail      Stage = "tail"
	StageRefine    Stage = "refine"
)

func (m *Marker) model(stage Stage) Model {
	if model, ok := m.Stages[stage]; ok {
		return model
	}
	return m.Model
}

// Usage is what the requests of a Marker took.
type Usage struct {
	Requests                  int
	InputTokens, OutputTokens int64
	CacheRead, CacheWrite     int64
}

// Usage is what the requests so far took, by model.
func (m *Marker) Usage() map[string]Usage {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make(map[string]Usage, len(m.usage))
	for id, usage := range m.usage {
		result[id] = usage
	}
	return result
}

// ask sends one user message of content under system to the stage's model
// and reads the answer, which the schema shapes, into result.
func (m *Marker) ask(ctx context.Context, stage Stage, system string, content []anthropic.ContentBlockParamUnion, schema map[string]any, result any) error {
	model := m.model(stage)
	started := time.Now()
	// Streamed, so that the thinking of a high effort has room without
	// the request timing out.
	stream := m.Client.Messages.NewStreaming(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(model.ID),
		MaxTokens: 64000,
		System:    []anthropic.TextBlockParam{{Text: system}},
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(content...)},
		OutputConfig: anthropic.OutputConfigParam{
			Effort: model.Effort,
			Format: anthropic.JSONOutputFormatParam{Schema: schema},
		},
	})
	response := anthropic.Message{}
	var err error
	for err == nil && stream.Next() {
		err = response.Accumulate(stream.Current())
	}
	if err == nil {
		err = stream.Err()
	}
	if m.Record != nil {
		m.Record(stage, model.ID, started, &response, err)
	}
	if err != nil {
		return err
	}
	m.mu.Lock()
	if m.usage == nil {
		m.usage = map[string]Usage{}
	}
	usage := m.usage[model.ID]
	usage.Requests++
	usage.InputTokens += response.Usage.InputTokens
	usage.OutputTokens += response.Usage.OutputTokens
	usage.CacheRead += response.Usage.CacheReadInputTokens
	usage.CacheWrite += response.Usage.CacheCreationInputTokens
	m.usage[model.ID] = usage
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

// Cost is the dollars usage of a model took.
func (u Usage) Cost(model string) float64 {
	return llmlog.Cost(model, anthropic.Usage{
		InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
		CacheReadInputTokens: u.CacheRead, CacheCreationInputTokens: u.CacheWrite,
	})
}
