// Package llm defines the backend-agnostic interface the correlator uses to
// reach a language model, plus the normalized usage every backend reports.
// It has no imports of internal/clients — like internal/tasktracker and
// internal/events, it's a shared contract with multiple producers
// (internal/clients/openai today, an Anthropic-shaped client later) and no
// single owning consumer.
//
// The contract lives here rather than in the consumer or in a specific
// client for a concrete reason: a second backend must be addable without
// importing a competing provider's package to report its own token counts.
package llm

import (
	"context"
	"strings"
)

// Client is the narrow capability the correlator needs from any LLM backend:
// one non-streaming, single-turn completion. Deliberately minimal — anything
// a backend can't express this way doesn't belong behind this seam.
type Client interface {
	Complete(ctx context.Context, systemPrompt, userPrompt string) (string, Usage, error)
}

// Usage reports what one completion actually consumed, as the server counted
// it. This is distinct from correlator's own estimateTokens heuristic, which
// only has to be good enough to decide whether to split a window before
// spending a call; comparing the two is how that heuristic gets validated.
//
// A backend that reports no usage returns the zero value rather than an
// error — missing telemetry must never fail a completion that succeeded.
type Usage struct {
	PromptTokens     int64
	CompletionTokens int64
	// Model is what the server reported serving, which can differ from the
	// model requested when a gateway (litellm, OpenRouter) remaps it.
	Model string
}

// StripJSONFence removes a Markdown code fence wrapping an LLM's JSON reply,
// returning the payload unchanged when there is no fence.
//
// Both system prompts say "no markdown fences", and models emit them anyway —
// a live litellm-fronted Claude model returned "```json\n[]\n```" for a
// prompt that forbade exactly that. Fencing is a property of the interface,
// not a prompt bug, so the parsers tolerate it rather than failing a pass over
// formatting. Everything past the fence stays strict: malformed JSON inside
// one is still a loud error naming the raw response.
//
// Lives here, not in internal/correlator, because internal/reconciler also
// parses fenced JSON responses from the same kind of model and needs the
// same tolerance — moved rather than duplicated once a second consumer
// appeared.
func StripJSONFence(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if !strings.HasPrefix(trimmed, "```") {
		return raw
	}

	// Drop the opening fence and its optional language tag ("```json"), which
	// runs to the end of that first line.
	if newline := strings.IndexByte(trimmed, '\n'); newline >= 0 {
		trimmed = trimmed[newline+1:]
	} else {
		// A fence with no newline carries no payload to parse; let the caller
		// report the original text rather than inventing a valid-looking one.
		return raw
	}

	if closing := strings.LastIndex(trimmed, "```"); closing >= 0 {
		trimmed = trimmed[:closing]
	}

	return strings.TrimSpace(trimmed)
}
