package llm

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// UnstagedStage is the stage recorded for a call whose context carries none.
const UnstagedStage = "unstaged"

type stageKey struct{}

// WithStage returns ctx labeled with the pipeline stage its LLM calls belong to.
func WithStage(ctx context.Context, stage string) context.Context {
	return context.WithValue(ctx, stageKey{}, stage)
}

// StageFrom returns the stage set by WithStage, or UnstagedStage when there is none.
func StageFrom(ctx context.Context) string {
	if s, ok := ctx.Value(stageKey{}).(string); ok && s != "" {
		return s
	}

	return UnstagedStage
}

// CallRecord is one completion unjira itself made. Cached is nil when the
// backend did not report cached prompt tokens.
type CallRecord struct {
	At         time.Time
	Stage      string
	Model      string
	Prompt     int64
	Completion int64
	Cached     *int64
}

// Recorder persists one CallRecord. It is declared here so this package never
// imports the store that implements it.
type Recorder interface {
	RecordLLMCall(call CallRecord) error
}

// RecordingClient records every completion's usage, success or failure, before
// returning the inner client's result.
type RecordingClient struct {
	inner Client
	rec   Recorder
	model string
	now   func() time.Time
}

var _ Client = (*RecordingClient)(nil)

// NewRecordingClient wraps inner so each call is recorded through rec under the
// configured model name (not the server-reported one: own calls are priced by
// the name configured).
func NewRecordingClient(inner Client, rec Recorder, model string, now func() time.Time) Client {
	return &RecordingClient{inner: inner, rec: rec, model: model, now: now}
}

// Complete calls the inner client and records one row whatever it returned. A
// recorder failure is returned alongside the completion text, joined with any
// completion error: the store it could not write is the one the pass writes next.
func (c *RecordingClient) Complete(ctx context.Context, systemPrompt, userPrompt string) (string, Usage, error) {
	text, usage, err := c.inner.Complete(ctx, systemPrompt, userPrompt)

	recErr := c.rec.RecordLLMCall(CallRecord{
		At:         c.now(),
		Stage:      StageFrom(ctx),
		Model:      c.model,
		Prompt:     usage.PromptTokens,
		Completion: usage.CompletionTokens,
		Cached:     usage.CachedPromptTokens,
	})
	if recErr != nil {
		return text, usage, errors.Join(err, fmt.Errorf("recording llm call: %w", recErr))
	}

	return text, usage, err
}
