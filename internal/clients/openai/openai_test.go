// Package openai_test exercises the facade against a real HTTP server
// (httptest), rather than stubbing the SDK's internals — the SDK's own wire
// behavior is covered by its own tests; these tests cover the logic that is
// unjira's own: request construction, response extraction, error
// translation.
package openai_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	upstreamopenai "github.com/openai/openai-go/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/clients/openai"
	"github.com/jcogilvie/unjira/internal/llm"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) *openai.Client {
	t.Helper()

	return newTestClientWithMaxTokens(t, 0, handler)
}

// newTestClientWithMaxTokens is newTestClient with an explicit output cap.
// Zero means "send no cap", which is what newTestClient uses.
func newTestClientWithMaxTokens(t *testing.T, maxOutputTokens int, handler http.HandlerFunc) *openai.Client {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	return openai.New(server.URL, llm.StaticCredential("test-key"), "gpt-5-2", maxOutputTokens)
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

// TestComplete_LengthTruncationIsAnError is the regression test for a bug that
// manual end-to-end verification found and every offline test had missed: the
// facade returned a truncated body as though it were complete.
//
// Observed against a real litellm-fronted claude-sonnet-5, which caps output at
// 4096 tokens by default despite advertising max_output_tokens=128000: the model
// emitted well-formed JSON, was cut off mid-string, and the caller reported
// "unexpected end of JSON input" — a parse error blaming the model for what was
// really a silently-dropped half of the response.
//
// Erroring here rather than at the parser matters because a parse failure is the
// LUCKY case. A truncation landing on a syntactically-valid boundary parses
// cleanly and silently discards whatever came after it, which for this pipeline
// means dropped narratives, matches, or proposed actions — the failure mode
// CLAUDE.md calls the hardest class of bug to notice.
func TestComplete_LengthTruncationIsAnError(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{
			"id":     "chatcmpl-3",
			"object": "chat.completion",
			"model":  "gpt-5-2",
			"choices": []map[string]any{{
				"index": 0,
				// Valid JSON prefix, cut off mid-array — exactly the shape the
				// live failure produced.
				"message":       map[string]any{"role": "assistant", "content": `[{"kind":"new","title":"trunc`},
				"finish_reason": "length",
			}},
			"usage": map[string]any{"prompt_tokens": 13395, "completion_tokens": 4096},
		})
	})

	text, usage, err := client.Complete(t.Context(), "sys", "user")

	require.Error(t, err,
		"a length-truncated response must fail loudly here, not reach a parser as if whole")
	assert.Contains(t, err.Error(), "truncated",
		"the message must name truncation, or the operator debugs the wrong layer")
	assert.Contains(t, err.Error(), "max_output_tokens",
		"and must name the config knob that fixes it")
	assert.Empty(t, text,
		"returning the partial text invites a caller to use it anyway")
	assert.Equal(t, int64(4096), usage.CompletionTokens,
		"usage is still returned: the call cost real tokens even though it failed")
}

func TestComplete_SendsMaxOutputTokensWhenConfigured(t *testing.T) {
	var body map[string]any
	client := newTestClientWithMaxTokens(t, 8192, func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		writeJSON(t, w, http.StatusOK, map[string]any{
			"id":     "chatcmpl-4",
			"object": "chat.completion",
			"model":  "gpt-5-2",
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": "ok"},
				"finish_reason": "stop",
			}},
		})
	})

	_, _, err := client.Complete(t.Context(), "sys", "user")
	require.NoError(t, err)

	// max_completion_tokens, not the deprecated max_tokens: the SDK marks the
	// latter deprecated and incompatible with reasoning models. Verified both
	// are honoured by the litellm proxy in use, so the current one wins.
	assert.EqualValues(t, 8192, body["max_completion_tokens"],
		"an unset cap is what let the proxy silently impose its own 4096 default")
	assert.NotContains(t, body, "max_tokens", "the deprecated field must not be sent")
}

func TestComplete_OmitsMaxOutputTokensWhenZero(t *testing.T) {
	// Zero means "let the backend decide" — some gateways reject an explicit
	// cap above their own ceiling, so unjira must be able to say nothing.
	var body map[string]any
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		writeJSON(t, w, http.StatusOK, map[string]any{
			"id":     "chatcmpl-5",
			"object": "chat.completion",
			"model":  "gpt-5-2",
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": "ok"},
				"finish_reason": "stop",
			}},
		})
	})

	_, _, err := client.Complete(t.Context(), "sys", "user")
	require.NoError(t, err)

	assert.NotContains(t, body, "max_completion_tokens")
	assert.NotContains(t, body, "max_tokens")
}

// rotatingCredential yields a different token on each call, so a test can prove
// the header is resolved per request rather than captured at construction.
type rotatingCredential struct {
	calls int
}

func (r *rotatingCredential) Credential(context.Context) (string, error) {
	r.calls++

	return fmt.Sprintf("token-%d", r.calls), nil
}

// TestComplete_ResolvesTheCredentialPerRequest is the whole point of taking a
// CredentialSource instead of a string: a short-lived token must be re-read for
// every call, or a long pass dies partway through when the one captured at
// startup expires. That is a real observed failure, not a hypothetical.
func TestComplete_ResolvesTheCredentialPerRequest(t *testing.T) {
	var seen []string
	cred := &rotatingCredential{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		writeJSON(t, w, http.StatusOK, map[string]any{
			"id": "c", "object": "chat.completion", "model": "gpt-5-2",
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": "ok"},
				"finish_reason": "stop",
			}},
		})
	}))
	t.Cleanup(server.Close)

	client := openai.New(server.URL, cred, "gpt-5-2", 0)

	for range 2 {
		_, _, err := client.Complete(t.Context(), "sys", "user")
		require.NoError(t, err)
	}

	require.Len(t, seen, 2)
	assert.Equal(t, []string{"Bearer token-1", "Bearer token-2"}, seen,
		"each request must carry a freshly-resolved credential")
}

// TestComplete_SendsExactlyOneAuthorizationHeader guards the specific hazard of
// setting auth in middleware: if the SDK also set its own, the proxy would see
// two values and could pick the stale one. Verified against openai-go v1.12.0
// that Header.Set in middleware replaces rather than appends — asserted here so
// an SDK upgrade that changes it fails loudly.
func TestComplete_SendsExactlyOneAuthorizationHeader(t *testing.T) {
	var values []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values = r.Header.Values("Authorization")
		writeJSON(t, w, http.StatusOK, map[string]any{
			"id": "c", "object": "chat.completion", "model": "gpt-5-2",
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": "ok"},
				"finish_reason": "stop",
			}},
		})
	}))
	t.Cleanup(server.Close)

	client := openai.New(server.URL, llm.StaticCredential("only-this"), "gpt-5-2", 0)

	_, _, err := client.Complete(t.Context(), "sys", "user")
	require.NoError(t, err)

	require.Len(t, values, 1, "two Authorization values let a gateway choose the stale one")
	assert.Equal(t, "Bearer only-this", values[0])
}

// failingCredential stands in for a helper that could not produce a token.
type failingCredential struct{}

func (failingCredential) Credential(context.Context) (string, error) {
	return "", fmt.Errorf("helper exited 1")
}

// TestComplete_CredentialFailureAbortsWithoutSendingTheRequest: an
// unauthenticated request would come back 401 and send the operator hunting a
// credential problem at the proxy, when the real cause was local. So the
// request is never sent, and the underlying reason survives in the message.
func TestComplete_CredentialFailureAbortsWithoutSendingTheRequest(t *testing.T) {
	var reached bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	client := openai.New(server.URL, failingCredential{}, "gpt-5-2", 0)

	_, _, err := client.Complete(t.Context(), "sys", "user")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "helper exited 1", "the real cause must survive")
	assert.False(t, reached, "no request may be sent without a credential")
}

// invalidatingCredential records Invalidate calls and rotates its token, so a
// test can prove the 401 path both discarded the dead credential and sent a
// different one on the retry.
type invalidatingCredential struct {
	fetches     int
	invalidated int
}

func (c *invalidatingCredential) Credential(context.Context) (string, error) {
	c.fetches++

	return fmt.Sprintf("token-%d", c.fetches), nil
}

func (c *invalidatingCredential) Invalidate() { c.invalidated++ }

// chatCompletionBody is the minimal successful response shape, with the usage
// numbers a test wants to assert on.
func chatCompletionBody(promptTokens, completionTokens int) map[string]any {
	return map[string]any{
		"id": "c", "object": "chat.completion", "model": "gpt-5-2",
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": "ok"},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens": promptTokens, "completion_tokens": completionTokens,
		},
	}
}

// TestComplete_RetriesOnceAfterA401 is the reactive half of credential refresh.
// An opaque token carries no expiry a CredentialSource can judge, so a 401 is
// the only evidence it has died — and a pass that has already paid for earlier
// stages must not be lost to it.
func TestComplete_RetriesOnceAfterA401(t *testing.T) {
	var sent []string
	cred := &invalidatingCredential{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent = append(sent, r.Header.Get("Authorization"))
		if len(sent) == 1 {
			writeJSON(t, w, http.StatusUnauthorized, map[string]any{
				"error": map[string]any{"message": "token expired"},
			})

			return
		}
		writeJSON(t, w, http.StatusOK, chatCompletionBody(10, 5))
	}))
	t.Cleanup(server.Close)

	client := openai.New(server.URL, cred, "gpt-5-2", 0)

	text, usage, err := client.Complete(t.Context(), "sys", "user")

	require.NoError(t, err, "a recoverable 401 must not fail the call")
	assert.Equal(t, "ok", text)
	assert.Equal(t, 1, cred.invalidated, "the dead credential must be discarded, not reused")
	require.Len(t, sent, 2)
	assert.NotEqual(t, sent[0], sent[1], "the retry must carry a different credential")
	assert.EqualValues(t, 5, usage.CompletionTokens)
}

// TestComplete_DoesNotRetryA401Twice pins "exactly once". Looping on a
// genuinely-bad credential would hammer the gateway and present as a hang rather
// than the misconfiguration it is.
func TestComplete_DoesNotRetryA401Twice(t *testing.T) {
	var attempts int
	cred := &invalidatingCredential{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		writeJSON(t, w, http.StatusUnauthorized, map[string]any{
			"error": map[string]any{"message": "nope"},
		})
	}))
	t.Cleanup(server.Close)

	client := openai.New(server.URL, cred, "gpt-5-2", 0)

	_, _, err := client.Complete(t.Context(), "sys", "user")

	require.Error(t, err)
	assert.Equal(t, 2, attempts, "one original attempt plus exactly one retry — never a third")
}

// TestComplete_DoesNotDoubleCountUsageAcrossA401Retry: the failed attempt
// consumed nothing, and folding a phantom usage in would corrupt the server-side
// accounting that estimateTokens is validated against (see Stats.AddUsage).
func TestComplete_DoesNotDoubleCountUsageAcrossA401Retry(t *testing.T) {
	var attempts int
	cred := &invalidatingCredential{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		if attempts == 1 {
			// A gateway may report usage even on a 401. It must not be counted.
			writeJSON(t, w, http.StatusUnauthorized, map[string]any{
				"error": map[string]any{"message": "expired"},
				"usage": map[string]any{"prompt_tokens": 999, "completion_tokens": 999},
			})

			return
		}
		writeJSON(t, w, http.StatusOK, chatCompletionBody(10, 5))
	}))
	t.Cleanup(server.Close)

	client := openai.New(server.URL, cred, "gpt-5-2", 0)

	_, usage, err := client.Complete(t.Context(), "sys", "user")
	require.NoError(t, err)

	assert.EqualValues(t, 10, usage.PromptTokens, "only the successful attempt's usage counts")
	assert.EqualValues(t, 5, usage.CompletionTokens)
}

// TestComplete_DoesNotRetryA401WithoutAnInvalidator: a StaticCredential cannot
// produce a different token, so retrying would send the identical dead value and
// waste a request — these prompts run to tens of thousands of tokens.
func TestComplete_DoesNotRetryA401WithoutAnInvalidator(t *testing.T) {
	var attempts int

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		writeJSON(t, w, http.StatusUnauthorized, map[string]any{
			"error": map[string]any{"message": "bad key"},
		})
	}))
	t.Cleanup(server.Close)

	client := openai.New(server.URL, llm.StaticCredential("static"), "gpt-5-2", 0)

	_, _, err := client.Complete(t.Context(), "sys", "user")

	require.Error(t, err)
	assert.Equal(t, 1, attempts,
		"a credential that cannot change must not be retried with the same value")
}
