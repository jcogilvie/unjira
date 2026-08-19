// Package openai_test exercises the facade against a real HTTP server
// (httptest), rather than stubbing the SDK's internals — the SDK's own wire
// behavior is covered by its own tests; these tests cover the logic that is
// unjira's own: request construction, response extraction, error
// translation.
package openai_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	upstreamopenai "github.com/openai/openai-go/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/clients/openai"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) *openai.Client {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	return openai.New(server.URL, "test-key", "gpt-5-2")
}

func TestNew_ReturnsUsableClient(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	require.NotNil(t, client)
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, body any) {
	t.Helper()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// assert, not require: this runs inside an httptest handler goroutine,
	// where require's FailNow (runtime.Goexit) wouldn't fail the test itself.
	assert.NoError(t, json.NewEncoder(w).Encode(body))
}

func TestComplete_ReturnsMessageContent(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{
			"id":      "chatcmpl-1",
			"object":  "chat.completion",
			"created": 1,
			"model":   "gpt-5-2",
			"choices": []map[string]any{
				{
					"index":         0,
					"finish_reason": "stop",
					"message": map[string]any{
						"role":    "assistant",
						"content": "the answer is 4",
					},
				},
			},
		})
	})

	result, _, err := client.Complete(t.Context(), "You are helpful.", "What is 2+2?")

	require.NoError(t, err)
	assert.Equal(t, "the answer is 4", result)
}

func TestComplete_SendsSystemAndUserMessages(t *testing.T) {
	var gotBody map[string]any

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		writeJSON(t, w, http.StatusOK, map[string]any{
			"id": "chatcmpl-1", "object": "chat.completion", "created": 1, "model": "gpt-5-2",
			"choices": []map[string]any{
				{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": "ok"}},
			},
		})
	})

	_, _, err := client.Complete(t.Context(), "sys prompt", "user prompt")

	require.NoError(t, err)
	messages, ok := gotBody["messages"].([]any)
	require.True(t, ok)
	require.Len(t, messages, 2)

	sysMsg, ok := messages[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "system", sysMsg["role"])
	assert.Equal(t, "sys prompt", sysMsg["content"])

	userMsg, ok := messages[1].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "user", userMsg["role"])
	assert.Equal(t, "user prompt", userMsg["content"])

	assert.Equal(t, "gpt-5-2", gotBody["model"])
}

func TestComplete_ErrorTranslatedToOpenAIError(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusTooManyRequests, map[string]any{
			"error": map[string]any{
				"message": "rate limited",
				"type":    "rate_limit_exceeded",
				"code":    "rate_limit_exceeded",
			},
		})
	})

	_, _, err := client.Complete(t.Context(), "sys", "user")

	require.Error(t, err)
	var apiErr *upstreamopenai.Error
	require.ErrorAs(t, err, &apiErr)
	// Assert against the struct's own Message field, not Error()'s string
	// rendering — the SDK docs confirm Message is populated from the
	// response body's error message, but don't specify Error()'s exact
	// format, so asserting on the field directly is the more reliable check.
	assert.Equal(t, "rate limited", apiErr.Message)
	assert.Equal(t, 429, apiErr.StatusCode)
}

func TestComplete_ReturnsUsageFromResponse(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{
			"id":     "chatcmpl-1",
			"object": "chat.completion",
			"model":  "gpt-5-2-served",
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": "4"},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{
				"prompt_tokens":     31,
				"completion_tokens": 7,
				"total_tokens":      38,
			},
		})
	})

	text, usage, err := client.Complete(t.Context(), "You are helpful.", "What is 2+2?")

	require.NoError(t, err)
	assert.Equal(t, "4", text)
	assert.Equal(t, int64(31), usage.PromptTokens)
	assert.Equal(t, int64(7), usage.CompletionTokens)
	assert.Equal(t, "gpt-5-2-served", usage.Model,
		"the served model can differ from the requested one behind a gateway")
}

func TestComplete_MissingUsageIsZeroNotAnError(t *testing.T) {
	// A backend that omits usage must not fail a completion that succeeded.
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{
			"id":     "chatcmpl-2",
			"object": "chat.completion",
			"model":  "gpt-5-2",
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": "ok"},
				"finish_reason": "stop",
			}},
		})
	})

	text, usage, err := client.Complete(t.Context(), "sys", "user")

	require.NoError(t, err)
	assert.Equal(t, "ok", text)
	assert.Zero(t, usage.PromptTokens)
	assert.Zero(t, usage.CompletionTokens)
}
