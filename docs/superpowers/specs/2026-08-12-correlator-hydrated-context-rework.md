# Slice-2 rework: hydrated narrative-event context (restoring the original Path B intent)

Status: approved (design brainstorm). Scope: reworks the already-merged `internal/correlator.Cluster`
(slice 2) so that clustering sees the **raw events** of overlapping/adjacent narratives, not just
their title+summary. Pairs with the revised slice-3 Persist design
(`2026-08-12-correlator-persist-design.md`, being updated alongside this) and is executed as one
combined effort with it. Supersedes the "Prompt/response contract" and "Assemble context" sections
of `2026-08-12-correlator-cluster-design.md` where they conflict.

## Why this rework exists

The phase-1 spec (`2026-08-11-phase1-correlator-design.md`) deliberately specified `Cluster`'s input
as *"every event already linked to a narrative whose window overlaps or sits adjacent to `window`,
plus every unlinked event within `window`,"* with an explicit accuracy rationale: *"excluding
already-narrated events to 'dedupe' would starve the model of exactly what makes clustering
correct."*

Slice 2 as built diverged from that **silently**: `buildClusterPrompt` feeds each existing
narrative as `narrative_id + title + summary + window` — its summary, never its constituent events.
An ongoing narrative's earlier raw events never reach the model. This was not a considered decision
(it was a default inside `buildClusterPrompt`, never surfaced as a design choice), and the slice-2
design doc itself is internally contradictory as a result: its prose echoes the "don't starve the
model" rationale while its prompt contract lists only summaries.

The accuracy concern behind the original intent is real: a summary is lossy, and for a borderline
event that could extend narrative A, extend B, or be new, the raw constituent events of A and B give
finer disambiguation signal than a compressed summary that may have elided the deciding detail. The
counter-argument (raw events are noisier; summaries are denoised; summary-carry-forward is more
recompute-stable) is real too — but run-to-run stability matters little here because humans review
in batches, so relitigating intervening work between batches has little cost. We are choosing
accuracy over stability, restoring the original design.

**Reversibility check (why this is safe to choose now):** events are the source of truth
(`events` is append-only) and `narrative_events` links every narrative to its events under *both*
designs. So switching *back* to summary-only later is a cheap query change (stop reading the events
you were reading), and switching *to* events was always cheap (start reading a join you were
ignoring). The one irreplaceable substrate — complete `narrative_events` links — is written under
both designs. As long as we keep writing those links and keep regenerating each narrative's summary
(needed for the reconciler and human display regardless), the decision stays reversible in either
direction. Nothing here paints us into a corner.

## The load-bearing decision, made explicit this time

**How prior narrative context is represented to the model is an accuracy-load-bearing decision, and
is hereby an explicit design choice — not a `buildPrompt` default.** The choice: each
overlapping/adjacent narrative is presented to the model as **title + cumulative summary/recap +
its (bounded) raw events**, with those events shown as **context only** — the model may read them
to judge extends-vs-new, but may not reassign them to a different cluster.

## Cluster's reworked contract

### Caller hydrates; Cluster stays pure

`Cluster` remains pure compute (no store access). The caller (slice 5's `watch`, eventually;
slice 3 provides the store accessor) is responsible for hydrating each candidate narrative's events
*before* the call and passing them in as data. This keeps `Cluster` unit-testable with a fake
`llmClient` and no store, exactly as today.

The narrative's events are attached to the `Narrative` value:

```go
// Narrative gains an Events field. The caller populates it (see slice-3's
// NarrativeEventsForContext accessor) with the narrative's context events —
// bounded per the hydration policy below — before passing it to Cluster.
// Cluster reads Events for the context section of its prompt; it never
// fetches them itself.
type Narrative struct {
	ID          int64
	WindowStart time.Time
	WindowEnd   time.Time
	Title       string
	Summary     string   // cumulative summary; for a compacted narrative, its leading portion is the recap of old events
	IssueKey    string
	Confidence  float64
	Status      string
	Events      []Event  // NEW: caller-hydrated context events (recap covers anything older)
}
```

`Cluster`'s signature is otherwise unchanged (`ctx, evts, existing, llm, window, contextWindowTokens`).
`evts` is still the pool of in-window events to cluster; `existing` narratives now arrive with
`Events` populated.

### Hydration policy (bounded by construction)

The caller hydrates each candidate narrative with:
- its cumulative `Summary` (which, for a compacted narrative, already begins with a recap of the
  old events that were summarized away — see slice-3 Persist), and
- its raw events **not yet folded into the recap** — i.e. everything after the narrative's
  compaction boundary. For a narrative that has never been compacted, that is all its events; for a
  long-running one, it is the recent tail (the recap covers the rest).

This makes each narrative's context contribution **bounded by construction**: recap (bounded by the
`MaxSummaryTokens`-style guard on the summary) + the post-boundary events (bounded because Persist
compacts once history crosses the threshold). Cluster therefore does not need its own per-narrative
compaction step — the boundedness is maintained upstream in Persist and applied by the caller's
hydration. Cluster only has to handle a too-wide *window* (see overflow below).

### Two-section prompt contract

The user prompt has two clearly separated sections:

```
Events to cluster (assign each to exactly one cluster by its index):
0. [claude_code] "started investigating flaky test" (occurred_at=2026-08-01T12:00:00Z)
1. [claude_code] "found root cause: race in the cache" (occurred_at=2026-08-01T12:01:00Z)

Existing narratives (CONTEXT ONLY — do not reassign these events; use them to decide
whether an event above extends one of these narratives or starts a new one):
narrative_id=9 title="Cache rework" window=[2026-07-30T09:00:00Z, 2026-08-01T11:00:00Z)
  recap: "earlier: introduced the cache layer, added metrics"   (present only if compacted)
  summary: "Reworking the shared cache for correctness"
  events:
    - [github] "PR #412 add cache layer" (occurred_at=2026-07-30T09:10:00Z)
    - [claude_code] "debugging cache eviction" (occurred_at=2026-08-01T10:55:00Z)
```

Key properties:
- **Only the in-window events are indexed (0..N-1) and assignable.** The response contract's
  `event_indices` reference this list only. Narrative context events have **no integer handles**, so
  the model structurally cannot put them in `event_indices` — this enforces "context only" by
  construction, not by instruction alone.
- Narrative context events are grouped under their `narrative_id` (not flattened) so the model can
  reason "this in-window event resembles narrative 9's events → it extends 9."
- All event summaries rendered with `%q` (the injection-hardening from slice 2's later commit), for
  both sections.

The system prompt is updated to describe the two sections and the context-only rule. The response
JSON contract is otherwise **unchanged** (`[{"kind","narrative_id","title","summary","event_indices"}]`,
index-based) — that decision stands; only the *input* side gains the context section.

## Overflow model (revised)

Two distinct overflow sources, matching the phase-1 spec's framing:

1. **Window too wide** (too many in-window events, or too many overlapping narratives with their
   hydrated context, for `contextWindowTokens`). Handled by the **existing split-by-time-and-merge**
   path, unchanged in mechanism. When the window bisects, each half re-filters overlapping
   narratives and carries their (already-bounded) context; a narrative overlapping both halves
   appears as context in both — harmless, since its events are context-only and any resulting
   duplicate `extends <same id>` results are already reconciled by `mergeSplitResults`' extends-merge.
2. **A single narrative's own history too large** — a *cumulative* overflow, distinct from a wide
   window. This is **Persist's** responsibility (slice 3): once a narrative's accreted history
   crosses `config.Correlator.TailSummarizeThresholdTokens`, Persist compacts everything older than
   `config.Correlator.RecentEventsKept` into the summary's recap prefix and advances the narrative's
   compaction boundary. `narrative_events` rows are **never deleted** — only what the caller hydrates
   into future Cluster context is bounded. This restores the tail-summarization that the (now
   withdrawn) Path-A slice-3 draft had dropped.

**Irreducible-unit floor (unchanged intent):** if, even after per-narrative bounding, the smallest
possible unit — one in-window event plus one narrative's bounded context — still exceeds
`contextWindowTokens`, that is a loud error naming the offending narrative/event, never a silent
drop. In practice this means a misconfigured `RecentEventsKept`/`MaxSummaryTokens` too large relative
to `ContextWindowTokens`; erroring loudly surfaces the misconfiguration.

## What changes in the merged slice-2 code

- `Narrative`: add `Events []Event`.
- `buildClusterPrompt`: emit the two-section layout above; narrative context events rendered
  (grouped, `%q`, context-only), plus recap line when present. In-window events unchanged
  (numbered, assignable).
- `clusterSystemPrompt`: rewritten to describe both sections and the context-only rule.
- `estimateTokens` budget check: now includes the hydrated narrative events (already does, since it
  estimates the full assembled prompt string — no code change, but the input is now larger, which is
  the point).
- Split/merge, response parsing, `checkSameStory`: **unchanged**.
- Tests: extend the window/adjacency test to assert narrative *events* (not just titles) appear in
  the prompt and are outside the assignable index space; add a test that a hydrated narrative's
  events inform an `extends` decision; overflow tests updated for the larger context.

## What this hands to slice 3 (revised Persist)

- **Hydration accessor**: `store.NarrativeEventsForContext(narrativeID) ([]events.Event, error)`
  (specified in the slice-3 doc) returning the post-compaction-boundary events for a narrative, for
  the caller to attach to `Narrative.Events`.
- **Compaction in Persist**: after an extend, if the narrative's history crosses
  `TailSummarizeThresholdTokens`, compact the old tail (older than `RecentEventsKept`) into the
  summary's recap prefix via one LLM call and advance the compaction boundary. This means **Persist
  now does take an `llmClient`** (reversing the Path-A draft's "pure store I/O" simplification) and
  `config.Correlator` carries `{TailSummarizeThresholdTokens, RecentEventsKept}` (plus the summary
  guard if retained) rather than only `MaxSummaryTokens`. The compaction-boundary bookkeeping (a
  marker on the narrative row distinguishing recapped-vs-raw events) is a slice-3 schema/design
  detail, specified there.
- **`narrative_events` links + regenerated summary always written** — preserving the cheap
  B→A reversibility described above.

## Non-goals (unchanged)

Reconciler, actions, issue-matching, CLI command — all later slices. This rework touches only
`internal/correlator` (the `Cluster` side) and is scoped strictly to restoring raw-event context;
Persist's side lands in the revised slice-3 spec and plan.
