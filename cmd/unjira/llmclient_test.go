package main

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/llm"
	"github.com/jcogilvie/unjira/internal/store"
)

// TestLLMClient_RecordsEveryCall: the one construction point wraps the backend in
// the recording decorator, so no caller can make an unrecorded call.
func TestLLMClient_RecordsEveryCall(t *testing.T) {
	app := &appContext{
		config: config.Config{LLM: config.LLMConfig{
			Model: "test-model", ContextWindowTokens: 128000, BaseURL: "http://localhost:4000/v1",
		}},
		store:     openTestStore(t),
		llmAPIKey: "test-key",
	}

	client, err := app.llmClient()

	require.NoError(t, err)
	assert.IsType(t, &llm.RecordingClient{}, client)
}

func TestLLMCallRow_PassesCachedThroughUnchanged(t *testing.T) {
	at := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	cached := int64(7)

	for _, c := range []*int64{nil, &cached} {
		got := llmCallRow(llm.CallRecord{At: at, Stage: "match", Model: "m", Prompt: 10, Completion: 3, Cached: c})

		want := store.LLMCall{CalledAt: at, Stage: "match", Model: "m", PromptTokens: 10, CompletionTokens: 3, CachedTokens: c}
		assert.Empty(t, cmp.Diff(want, got))
	}
}
