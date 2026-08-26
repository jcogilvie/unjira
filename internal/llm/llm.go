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
	"encoding/json"
	"fmt"
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

// JSONArrayPayload normalizes a model reply that should be a JSON array: it
// strips a Markdown fence, wraps a lone object into a one-element array, and
// joins a run of concatenated objects (NDJSON) into one. Anything else is
// returned unchanged.
//
// Three tolerances, each added only after a live model produced that shape for a
// prompt that forbade it. They compose, because nothing stops one reply doing all
// three.
//
// Every array-shaped prompt in this repo says "return ONLY a JSON array", and
// models still answer a single-item batch with a bare object — observed live
// when a clustering pass had exactly one story left to report:
//
//	{"kind":"extends","narrative_id":9,...}
//
// which failed with `cannot unmarshal object into Go value of type
// []clusterResponseItem`. That is the same class as the markdown fences
// StripJSONFence absorbs: a property of the interface, not a prompt bug. The
// cost of not tolerating it is losing a narrative the model identified
// correctly, so the parsers absorb it rather than failing a whole pass over
// punctuation.
//
// Tolerance stops at shape. Malformed JSON is returned untouched so the
// caller's own json.Unmarshal still fails loudly naming the raw response, and
// a scalar or null is NOT wrapped — `42` is not a plausible one-element
// response, and turning it into `[42]` would manufacture something that parses
// while meaning nothing.
func JSONArrayPayload(raw string) string {
	unfenced := StripJSONFence(raw)

	trimmed := strings.TrimSpace(unfenced)
	if !strings.HasPrefix(trimmed, "{") || !strings.HasSuffix(trimmed, "}") {
		return unfenced
	}

	// Confirm it really is one well-formed object before wrapping. Without
	// this, a truncated `{"kind":"new"` would become `[{"kind":"new"]` — still
	// an error, but one whose message points at the wrapper rather than at the
	// model's actual output.
	if !json.Valid([]byte(trimmed)) {
		// Not a single object — but it may be several concatenated, the NDJSON
		// shape a model produces when it reasons a line at a time.
		if joined, ok := joinConcatenatedObjects(trimmed); ok {
			return joined
		}

		return unfenced
	}

	return "[" + trimmed + "]"
}

// joinConcatenatedObjects turns a run of back-to-back JSON objects into one
// array, reporting false if the input is anything else.
//
// Two live shapes motivate it, both from prompts demanding an array:
//
//	{"issue_key":"A-1",...}
//	{"issue_key":"B-2",...}      → "invalid character '{' after top-level value"
//
//	{"issue_key":"A-1",...},{"issue_key":"B-2",...}
//	                             → "invalid character ',' after top-level value"
//
// Neither bracketing alone nor stream-decoding alone handles both, and the reason
// is worth recording because both shortcuts look right:
//
//   - Bracketing `[` + s + `]` fixes the comma form but NOT the newline form:
//     JSON requires a comma between array elements, so `[{...}\n{...}]` is
//     invalid ("Expecting ',' delimiter"). Whitespace is only ignored *around*
//     separators, not *instead of* them.
//   - json.Decoder in a loop fixes the newline form but NOT the comma form: it
//     reads a stream of top-level values and rejects the separators outright
//     ("invalid character ',' looking for beginning of value").
//
// So: split into values with a Decoder, skipping the commas a stream does not
// expect, then rejoin with the commas an array requires. A reply mixing both
// separators works as a consequence rather than a special case.
//
// Requires EVERY value to be a complete object. A trailing comma, a truncated
// final object, a stray scalar, or interleaved prose all yield false, so the
// caller reports the model's real output instead of something assembled here —
// the same boundary the single-object path draws.
func joinConcatenatedObjects(s string) (string, bool) {
	var objects []string

	// Commas between top-level values are not valid in a JSON stream, so strip
	// them before decoding and re-add them when rejoining. Only separators are
	// affected: commas inside an object live within a value the Decoder consumes
	// whole.
	remaining := strings.TrimSpace(s)

	for remaining != "" {
		decoder := json.NewDecoder(strings.NewReader(remaining))

		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return "", false
		}

		trimmed := strings.TrimSpace(string(raw))
		if !strings.HasPrefix(trimmed, "{") {
			// Not a record. Guessing at intent here would manufacture something
			// that parses while meaning nothing.
			return "", false
		}
		objects = append(objects, trimmed)

		// Advance past the value just consumed, then past any separator.
		remaining = strings.TrimSpace(remaining[decoder.InputOffset():])
		remaining = strings.TrimSpace(strings.TrimPrefix(remaining, ","))
	}

	// A single object is the caller's other path; fewer than two here means
	// there was nothing to join and the input was malformed some other way.
	if len(objects) < 2 {
		return "", false
	}

	return "[" + strings.Join(objects, ",") + "]", true
}

// CredentialSource yields the bearer credential for one completion.
//
// A source rather than a string because a credential can expire mid-run. This
// environment's LLM gateway issues short-lived OIDC tokens, and a client that
// captured one at construction failed partway through a pass — after already
// spending money on earlier stages. See
// docs/superpowers/specs/2026-08-26-llm-credential-helper-design.md.
//
// Implementations must be safe for concurrent use: `watch` will plausibly run
// completions in parallel, and two callers racing to refresh must not each
// perform the refresh.
type CredentialSource interface {
	Credential(ctx context.Context) (string, error)
}

// StaticCredential is a CredentialSource that never changes — what a plain
// UNJIRA_LLM_API_KEY becomes.
//
// Exists so the static path is a special case of the general one rather than a
// parallel branch through the client: there is exactly one way a credential
// reaches a request, whether or not it can expire.
type StaticCredential string

// Credential returns the fixed value. Never errors, and ignores ctx — there is
// nothing to cancel.
func (s StaticCredential) Credential(context.Context) (string, error) {
	if s == "" {
		return "", fmt.Errorf("llm credential is empty")
	}

	return string(s), nil
}

// Invalidator is the optional capability of a CredentialSource that caches: it
// discards whatever it holds so the next Credential call fetches afresh.
//
// Optional rather than part of CredentialSource because StaticCredential has
// nothing to invalidate, and forcing it to implement a no-op would make callers
// believe an invalidation had an effect. Consumers type-assert for it — the same
// pattern workflow.GraphProvider uses, and for the same reason: not every
// implementation should be made to have an opinion.
//
// Its caller is a 401-handling path. An opaque (non-JWT) token has no expiry a
// source can judge for itself, so a 401 from the server is the only evidence
// that the cached credential is dead.
type Invalidator interface {
	Invalidate()
}
