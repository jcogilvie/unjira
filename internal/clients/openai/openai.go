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
	upstream        openai.Client
	model           string
	maxOutputTokens int
}

// Compile-time proof the facade satisfies the contract the correlator uses.
var _ llm.Client = (*Client)(nil)

// New constructs a Client pointed at baseURL, authenticating with apiKey,
// making every completion call against model.
//
// maxOutputTokens caps the response length. Zero sends no cap, letting the
// backend choose — kept possible because some gateways reject an explicit cap
// above their own ceiling. But note what "let the backend choose" cost in
// practice: a litellm-fronted claude-sonnet-5 silently imposed 4096 while
// advertising max_output_tokens=128000, truncating a clustering response
// mid-JSON. Setting this explicitly is strongly preferred; see Complete's
// truncation check for why an unnoticed cap is dangerous.
func New(baseURL, apiKey, model string, maxOutputTokens int) *Client {
	upstream := openai.NewClient(
		option.WithBaseURL(baseURL),
		option.WithAPIKey(apiKey),
	)

	return &Client{upstream: upstream, model: model, maxOutputTokens: maxOutputTokens}
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
	params := openai.ChatCompletionNewParams{
		Model: c.model,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(systemPrompt),
			openai.UserMessage(userPrompt),
		},
	}
	if c.maxOutputTokens > 0 {
		// MaxCompletionTokens, not MaxTokens: the SDK marks the latter
		// deprecated and incompatible with reasoning models. Both were verified
		// honoured by the litellm proxy in use, so the current field wins.
		params.MaxCompletionTokens = openai.Int(int64(c.maxOutputTokens))
	}

	resp, err := c.upstream.Chat.Completions.New(ctx, params)
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

	// A "length" finish means the model was cut off mid-answer. Erroring here
	// rather than returning the fragment is the whole point: every caller in
	// this codebase parses the reply as JSON, and a parse failure is the LUCKY
	// outcome. A truncation landing on a syntactically-valid boundary parses
	// cleanly and silently discards everything after it — dropped narratives,
	// matches, or proposed actions, with no error anywhere. That is the failure
	// class CLAUDE.md calls the hardest to notice, so this fails loudly instead
	// (the same reasoning as refs.ParsePRRefs erroring on an over-max_span
	// range rather than truncating).
	//
	// Usage is still returned alongside the error: the call consumed real
	// tokens, and a caller aggregating cost must not lose that because the
	// response was unusable.
	if resp.Choices[0].FinishReason == "length" {
		return "", usage, fmt.Errorf(
			"completing chat prompt: response truncated after %d completion tokens "+
				"(finish_reason=length): raise llm.max_output_tokens, or reduce the prompt — "+
				"a truncated reply is not safe to parse",
			resp.Usage.CompletionTokens,
		)
	}

	return resp.Choices[0].Message.Content, usage, nil
}
