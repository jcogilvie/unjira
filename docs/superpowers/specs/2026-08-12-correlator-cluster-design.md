# Phase-1 slice 2: `internal/correlator.Cluster` (compute only)

Status: approved (design brainstorm). Scope: this spec covers slice 2 of
`docs/superpowers/specs/2026-08-11-phase1-correlator-design.md`'s "Implementation slices" —
`Cluster`, the narrative-clustering compute function, against `claude_code` events only, with no
persistence and no CLI command. `Persist` and real `narratives`/`narrative_events` table wiring
land in slice 3, per the phase-1 spec's own sequencing.

## Why this slice, and what it builds on

Slice 1 (`internal/clients/openai`, landed) gave unjira a `Complete(ctx, systemPrompt, userPrompt
string) (string, error)` call against any OpenAI-Chat-Completions-compatible endpoint, plus
`config.LLM.{Model, ContextWindowTokens}`. Nothing calls it yet. This slice is the first real
caller: `Cluster` groups a window of events (plus any already-narrated events whose window
overlaps or sits adjacent to it) into narratives, tagging each result `new` or `extends
<narrative_id>`. It is pure compute — no store access, in-memory results only — matching the
phase-1 spec's compute/persist split for every component in this vertical.

## Types

```go
// internal/correlator/correlator.go

// TimeRange is a half-open [Start, End) window over events.Event.OccurredAt.
type TimeRange struct {
	Start, End time.Time
}

// Narrative mirrors internal/store's `narratives` table row shape (see
// store.go's schema) so slice 3's Persist doesn't have to reshape this type
// when it starts writing real rows — Cluster only reads a subset of these
// fields (WindowStart/End for overlap/adjacency, Title/Summary/ID for
// prompt context), but defining the full shape now means nothing here
// changes when persistence lands.
type Narrative struct {
	ID          int64
	WindowStart time.Time
	WindowEnd   time.Time
	Title       string
	Summary     string
	IssueKey    string
	Confidence  float64
	Status      string
}

// ClusterKind distinguishes a brand-new narrative from one extending an
// existing row.
type ClusterKind int

const (
	ClusterNew ClusterKind = iota
	ClusterExtends
)

// ClusterResult is one narrative-shaped grouping of events, held in memory
// only — Cluster never writes to the store. NarrativeID is set only when
// Kind == ClusterExtends.
type ClusterResult struct {
	Kind        ClusterKind
	NarrativeID int64
	Title       string
	Summary     string
	Events      []events.Event
}
```

## `llmClient` interface (consumer-owned, mirrors `workflow.projectMiner`)

```go
// llmClient is the narrow capability Cluster needs from an LLM backend — a
// consumer-owned interface (Go idiom, same pattern as internal/workflow's
// projectMiner) so a fake can exercise Cluster in tests without a real HTTP
// server. *openai.Client already satisfies this with zero adapter code.
type llmClient interface {
	Complete(ctx context.Context, systemPrompt, userPrompt string) (string, error)
}
```

## `Cluster`'s signature and algorithm

```go
func Cluster(
	ctx context.Context,
	evts []events.Event,
	existing []Narrative,
	llm llmClient,
	window TimeRange,
	contextWindowTokens int,
) ([]ClusterResult, error)
```

1. **Assemble context.** Filter `evts` to those inside `window`. Filter `existing` to narratives
   whose `[WindowStart, WindowEnd)` overlaps `window` or sits immediately adjacent to it —
   temporal proximity is real clustering signal, so already-narrated events in the assembled
   prompt are never excluded to "dedupe."
2. **Estimate token budget.** Build the full prompt text (system + user) that step 4 would send,
   and estimate its token count as `ceil(len(text) / 4)` — the standard rough
   characters-per-token approximation for English text against OpenAI-family tokenizers. This is
   a conservative pre-flight check, not an exact count; exactness isn't the goal, only a margin
   safe enough to decide whether to split before spending a real call.
3. **Compare against `contextWindowTokens`.**
   - **Fits:** proceed to step 4.
   - **Doesn't fit, and `window` can still be split** (see step 6 for what "can't" means):
     bisect `window`'s `[Start, End)` at its midpoint, recurse on each half independently (each
     half re-runs step 1's overlap/adjacency filter against its own narrower window), then merge
     per step 5.
   - **Doesn't fit, and `window` is already irreducible** (see step 6): return a loud error.
4. **Call the model.** Build the prompt (see "Prompt/response contract" below), call
   `llm.Complete`, parse the JSON response, map `event_indices` back into `evts`, return
   `[]ClusterResult`.
5. **Merge split halves** (only reached via step 3's split path):
   - Concatenate both halves' `[]ClusterResult`.
   - Any two `ClusterExtends` results sharing the same `NarrativeID` merge into one: union their
     `Events`, keep either `Title`/`Summary` (they describe the same existing narrative, so both
     halves' LLM calls should already agree closely; keep the one from the earlier half for
     determinism).
   - The adjacent boundary between halves — the last `ClusterNew` result of half A and the first
     `ClusterNew` result of half B, if both exist — gets one additional `llm.Complete` call: given
     both clusters' titles/summaries/event lists, ask whether they're the same emerging story. If
     yes, merge into one `ClusterNew` with unioned events and the title/summary the model returns
     from that same call. If no (or if either half has no `ClusterNew` result at the boundary),
     leave both as separate results. This reuses the same judgment the model applies everywhere
     else in this design rather than inventing a separate deterministic heuristic — a real
     same-story split at the boundary is exactly the kind of contextual judgment call an LLM is
     already trusted for elsewhere in this vertical.
6. **Irreducible unit.** A window is irreducible — splitting must stop and, if still over budget,
   error — once bisecting it further can no longer separate the filtered event set into two
   non-empty halves. Concretely: after filtering to `window` (step 1), if the filtered event count
   is 0 or 1, no further time-bisection can shrink the assembled context (a single event's own
   text plus the existing-narrative context is the smallest possible unit; an empty window is
   never oversized in the first place, since the token estimate over an empty event list is
   always well under budget). Bisecting by the window's *time midpoint* rather than its *event
   count* means a run of events with identical or near-identical timestamps could all land in one
   half even after a time-split — so after each bisection, if a half's filtered event count
   didn't actually decrease from its parent's, treat that half as irreducible immediately rather
   than recursing again on an unchanged set (which would otherwise recurse forever). Whenever an
   irreducible unit is still over budget, return an error naming the offending event/narrative
   rather than truncating anything. This is the same "error loudly rather than silently drop"
   floor design-notes #9 already locks in for the collector-scan-window case, applied here for
   narrative context for the first time.

## Prompt/response contract

**System prompt** (fixed instructions, not user-configurable in this slice):

> Cluster the given events into narratives. Every event index belongs to exactly one cluster.
> Tag each cluster `"new"` or `"extends"`; if `"extends"`, include the `narrative_id` of the
> existing narrative it continues. Return ONLY a JSON array matching this shape, no prose, no
> markdown fences:
> `[{"kind":"new"|"extends","narrative_id":<int, only if extends>,"title":"...","summary":"...","event_indices":[0,2,5]}]`

**User prompt**: the numbered event list (`index`, `source`, `summary`, `occurred_at`) followed
by the existing-narrative list (`narrative_id`, `title`, `summary`, `window_start`, `window_end`).

**Why index-based, not echoed identifiers:** assigning each event a small integer index (0..N-1)
in the prompt and having the model reference clusters by `event_indices` is deterministic and
cheap to validate — `Cluster` maps indices straight back to the `evts` slice it sent, no
string-matching fragility. Two alternatives were considered and rejected: having the model echo
back `(source, external_id)` pairs is more verbose per event and introduces a
typo/truncation failure mode on every single reference; a separate classify-then-describe
two-pass call (one call to assign events to clusters, a second to write titles/summaries) doubles
LLM calls for no clear benefit over asking for both in one structured response.

**Why prompt-JSON, not `response_format:json_schema`:** the openai-go SDK does support
OpenAI's structured-outputs feature (`ChatCompletionNewParams.ResponseFormat` +
`shared.ResponseFormatJSONSchemaParam{Strict: true}`), but that feature isn't reliably
implemented across every OpenAI-compatible gateway this facade targets — litellm passthrough to a
non-OpenAI backend, Ollama, and various self-hosted models may ignore `response_format` and
silently degrade to unstructured prose, which `Cluster` would still need to defensively parse
either way. Strict-JSON-in-prompt works identically across every endpoint since it needs no
gateway feature support, and keeps `internal/clients/openai`'s facade surface unchanged from
slice 1. Revisit this if a later slice needs stricter adherence on a known-capable endpoint.

**Parse failure handling:** invalid JSON, an out-of-range `event_indices` value, or an unknown
`kind` value are all treated identically — a wrapped error that includes the raw response body,
never a partial or best-effort result. This mirrors slice 1's `Complete` "no choices" guard and
design-notes #9's error-loudly invariant.

## Error handling

- **LLM call failure** (any layer, including the merge-boundary call in step 5): wrapped with
  `fmt.Errorf("...: %w", err)` per `docs/go-conventions.md`, returned immediately. No partial
  results are ever returned from a failed pass — this slice never persists anyway, but the
  contract holds regardless, since slice 3's `Persist` will call `Cluster` and needs a clean
  all-or-nothing compute result to persist against.
- **Irreducible over-budget unit** (step 6): loud error naming the specific event/narrative that
  can't fit, never a silent truncation.
- **Malformed model response** (step 4/5's parse step): loud error with the raw response text
  attached, so a real failure is debuggable from the error message alone.

## Testing strategy

Following `docs/go-conventions.md` (testify, table-driven where scenarios share the same
input/output shape, `httptest`-free here since the LLM client is a hand-written fake, not an HTTP
stub):

- **Fake `llmClient`**: a hand-written type implementing the interface, configurable per test with
  either a canned response string or a response-selecting function (needed for the multi-call
  split/merge tests, where different calls must return different cluster assignments).
- **Table-driven response-parsing tests**: single-cluster-new, single-cluster-extends, a batch
  producing both kinds in one response, malformed-JSON error case, out-of-range-index error case,
  unknown-`kind` error case — one table, one `Cluster` invocation per row, asserting either the
  returned `[]ClusterResult` or the wrapped error's content.
- **Window/adjacency selection test**: existing narratives outside `window` and not adjacent to it
  are excluded from the assembled prompt; ones overlapping or adjacent are included — asserted by
  inspecting the prompt text the fake `llmClient.Complete` actually received, not just the final
  result.
- **Overflow split test**: configure a small `contextWindowTokens`, feed an oversized synthetic
  event list, assert the fake `llmClient.Complete` is called once per split half (not once for the
  whole window) plus exactly one merge-boundary call when both halves produce a `ClusterNew` at
  the boundary, and that the final merged result is correct (unioned events, one `ClusterResult`
  where the two halves described the same story).
- **Irreducible-unit test**: a single event whose own text alone (combined with the smallest
  possible existing-narrative context) exceeds the configured budget — assert a loud error is
  returned and no `Complete` call is made past the point of detection (i.e., the facade is never
  invoked with a doomed-to-fail oversized request).
- **Degenerate-split test**: multiple events sharing the exact same (or near-identical)
  `OccurredAt` timestamp, oversized enough as a group to trigger a split, where a naive
  time-midpoint bisection would put all of them in one half. Assert `Cluster` detects the
  unchanged-event-count case and returns a loud error rather than recursing indefinitely — this
  is the regression test for the infinite-recursion risk step 6 calls out.
- **Extends-merge test**: two `ClusterExtends` results from different split halves sharing the
  same `NarrativeID` merge into one result with the union of both halves' events, no duplicate
  merge-boundary LLM call triggered for this case (that call is only for `ClusterNew` pairs).

No store, no `httptest` server, and no live network calls in this slice's test suite — `Cluster`
is pure compute over a fake `llmClient`, matching the phase-1 spec's own testing-strategy section
for this component.

## Non-goals for this slice

Explicitly deferred to later slices, per the phase-1 spec's own sequencing — none of this is
built or stubbed in slice 2:

- `Persist` and real `narratives`/`narrative_events` table writes (slice 3).
- Tail-summarization overflow handling for a narrative's own accreted history
  (`config.Correlator.{TailSummarizeThresholdTokens, RecentEventsKept}`) — a distinct overflow
  source from the window-too-wide case this slice handles; slice 3.
- The lease lock (slice 3).
- Any CLI command surfacing `Cluster` (`watch`/`triage` land in slices 5-6).
- `internal/reconciler` and everything downstream of a persisted narrative (slice 4+).
