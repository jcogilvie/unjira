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
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// Client is a facade over openai-go, exposing only the surface unjira needs.
type Client struct {
	upstream openai.Client
	model    string
}

// New constructs a Client pointed at baseURL, authenticating with apiKey,
// making every completion call against model.
func New(baseURL, apiKey, model string) *Client {
	upstream := openai.NewClient(
		option.WithBaseURL(baseURL),
		option.WithAPIKey(apiKey),
	)

	return &Client{upstream: upstream, model: model}
}
