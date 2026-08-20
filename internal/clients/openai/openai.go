// Package openai is a thin facade over the official openai-go SDK, speaking
// OpenAI-Chat-Completions-compatible wire format — the shape litellm, Azure
// OpenAI, OpenRouter, Ollama, and most other self-hosted/third-party
// gateways already speak natively, unlike Anthropic's narrower Messages
// API. See docs/superpowers/specs/2026-08-11-phase1-correlator-design.md
// for why this shape was chosen over Anthropic's.
//
// Every client is constructed with an explicit base URL and API key —
// never the SDK's own default OPENAI_API_KEY/OPENAI_BASE_URL env-var
// loading — so unjira's own config is always what's used, matching the
// "our own explicit config, never reused ambient credentials" precedent
// set by UNJIRA_JIRA_CREDENTIALS.
package openai

import (
	"context"
	"fmt"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/jcogilvie/unjira/internal/llm"
)

// Client is a facade over openai-go, exposing only the surface unjira needs.
type Client struct {
	upstream openai.Client
	model    string
}

// Compile-time proof the facade satisfies the contract the correlator uses.
var _ llm.Client = (*Client)(nil)

// New constructs a Client pointed at baseURL, authenticating with apiKey,
// making every completion call against model.
func New(baseURL, apiKey, model string) *Client {
	upstream := openai.NewClient(
		option.WithBaseURL(baseURL),
		option.WithAPIKey(apiKey),
	)

	return &Client{upstream: upstream, model: model}
}

// Complete sends one non-streaming, single-turn chat completion request and
// returns the assistant's reply text plus what the call consumed.
//
// Usage comes from the server's own accounting, which is why it is returned
// rather than estimated: it is the ground truth the correlator's own
// token-estimate heuristic gets validated against. A response omitting usage
// yields a zero Usage, not an error — missing telemetry must never fail a
// completion that otherwise succeeded.
func (c *Client) Complete(ctx context.Context, systemPrompt, userPrompt string) (string, llm.Usage, error) {
	resp, err := c.upstream.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model: c.model,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(systemPrompt),
			openai.UserMessage(userPrompt),
		},
	})
	if err != nil {
		return "", llm.Usage{}, fmt.Errorf("completing chat prompt: %w", err)
	}

	if len(resp.Choices) == 0 {
		return "", llm.Usage{}, fmt.Errorf("completing chat prompt: response had no choices")
	}

	usage := llm.Usage{
		PromptTokens:     resp.Usage.PromptTokens,
		CompletionTokens: resp.Usage.CompletionTokens,
		Model:            resp.Model,
	}

	return resp.Choices[0].Message.Content, usage, nil
}
