# Phase 1: correlator, reconciler, review, and learning

Status: approved (design brainstorm). Scope: this spec covers the full phase-1 vertical —
narrative clustering, action proposal, human review with in-session rework, auto-commit
graduation, and rule learning — designed end to end. Implementation lands in slices (this spec's
own "Implementation slices" section), each feeding real usage back into design revisions before
the next slice starts. First slice: `claude_code` events only, through the full loop.

## Why this design, and what it replaces

Phase 0 (merged) built collectors, the event log, a deterministic digest, and — as a late
addition — a backend-agnostic `TaskTracker` seam (`internal/tasktracker`, `clients/jira`,
`clients/local`) specifically so phase 1 wouldn't have to retrofit multi-backend support onto a
Jira-only design. This spec is that phase-1 design, built on top of that seam.

The `narratives`/`actions` tables have existed as schema-only placeholders since phase 0
(`internal/store`'s package doc: "only events and cursors are written in phase 0"). This spec is
what starts writing to them.

## Command surface

Two verbs, plus two machine-facing primitive families:

```
unjira watch [--dry-run]              background pipeline: collect -> narrate -> reconcile ->
                                       auto-commit whatever clears the confidence-floor +
                                       graduated bar. Interval-driven (cron/launchd/cloud
                                       scheduler calls this repeatedly); no human runs it
                                       directly. --dry-run: full pipeline runs in-memory,
                                       nothing persisted, nothing written to a real tracker.

unjira actions list [--json]          machine-facing primitives, scriptable
unjira actions decide <id> --approve|--reject|--edit <text>

unjira rules list [--json]
unjira rules decide <id> --keep|--modify <text>|--reject

unjira triage [--auto-approve] [--refresh] [--dry-run]
                                       the only command a human runs by hand. Built on the
                                       primitives above: lists pending actions, presents as a
                                       batch ("does this look good?"), applies on approval;
                                       free-text correction on any line re-derives affected
                                       actions via the LLM and re-presents. If the configured
                                       learn-interval has elapsed, also lists+prompts pending
                                       rules the same way (keep/modify/reject).
                                       --auto-approve: still runs the full flow and prints the
                                       batch, but calls decide(approve) for everything instead
                                       of prompting.
                                       --refresh: blocks on any in-flight watch pass (see
                                       "Lease lock" under Data flow below) rather than using
                                       stale data, then proceeds with that pass's result.
                                       --dry-run: walks the batch, shows what would be
                                       approved/rejected, never calls TaskTracker or writes a
                                       rule file or flips actions.status.
```

`watch` never blocks on a human. `triage` is the one and only interactive surface, and it
surfaces both actions (every run) and rules (on the configured interval) in the same session,
per the same keep/modify/reject shape.

## Architecture

```
┌──────────────────────────────────────────────────────────────────────┐
│  unjira watch  (interval-driven; TryAcquire lease lock, skip tick   │
│  if held)                                                            │
│                                                                       │
│    collect (existing) -> narrate (compute+persist) ->                │
│    reconcile (compute+persist) -> auto-commit gate                   │
└──────────────────────────────────────────────────────┬───────────────┘
                                                         │
                                          pending actions accumulate in
                                          the `actions` table (status=proposed)
                                                         │
                                                         ▼
┌──────────────────────────────────────────────────────────────────────┐
│  unjira triage  (--refresh: Acquire lease lock, blocking)            │
│                                                                       │
│    1. actions list -> print batch -> per item: decide (or            │
│       auto-approve) -> commit inline on approve; free-text on        │
│       reject/edit -> LLM re-derives -> re-present                    │
│    2. if learn-interval elapsed: rules list -> print -> per item:    │
│       decide (keep writes rules/*.md, modify edits then writes,      │
│       reject discards)                                               │
└────────────────────────────────────────────────────────────────────────┘
```

## Components

**`internal/clients/openai`** (revised from an earlier Anthropic-Messages-API-shaped design — see
below) — thin facade over the official `openai-go` (root package only; no Azure/AWS-auth
subpackages, since those exist for Azure AD and Bedrock-native auth flows unjira doesn't use).
One method to start: `Complete(ctx, systemPrompt, userPrompt string) (string, error)` —
non-streaming, single-turn, against `Chat.Completions.New`. Config is unjira's own — a base URL
and credential, **not** read from ambient `ANTHROPIC_*`/`OPENAI_*` env vars — following the same
"our own explicit config, not reused ambient credentials" precedent as
`UNJIRA_JIRA_CREDENTIALS`. Verified (not assumed): importing the root package only resolves to 4
small `tidwall` JSON packages, no AWS/Azure weight — confirmed with a throwaway `go mod tidy`
against a minimal program, same verification method used for the earlier (superseded) Anthropic
SDK check.

Config also carries `Model string` and `ContextWindowTokens int` — **required**, not defaulted or
looked up. Not every OpenAI-compatible endpoint's model has the same context-window size, and
there's no reliable way to query it generically across gateways; a maintained
model-name→context-window lookup table would need constant upkeep and would fail *silently
wrong* for any model we hadn't added yet (the exact "erroring loudly" invariant this design
already leans on elsewhere). Requiring it explicitly means an unrecognized/misconfigured model is
a loud config error at startup, not a silent overflow risk at run time. `config/unjira.example.json`
gets a populated, working value for a known model so day-one setup isn't "go look this number
up" — see Data flow below for how it's consumed.

*Why OpenAI-shaped Chat Completions, not Anthropic's Messages API, despite this environment's own
`ANTHROPIC_BASE_URL`:* that env var is Claude Code's own interactive-session config, pointing at
this org's specific litellm deployment — coupling unjira (a separate, unattended background
service) to it would mean every other production user needs an Anthropic-Messages-API-speaking
endpoint specifically. Research into the two specs' actual divergence and adoption found OpenAI's
Chat Completions shape, not Anthropic's, is the real ecosystem lingua franca: it's litellm's own
*native* interface ("every response follows the OpenAI Chat Completions format, regardless of
provider" — from litellm's own docs), and it's what nearly every other self-hosted or third-party
gateway already speaks natively (Azure OpenAI, OpenRouter, Ollama, vLLM, DeepSeek, Qwen,
Moonshot/Kimi, GLM). Anthropic's Messages API is comparatively narrow — native only to Anthropic
itself, requiring a translation layer everywhere else. Building unjira's own Anthropic↔OpenAI
translation layer would just reimplement what litellm/every gateway already does; speaking the
already-universal shape gets the same breadth for free. The SDK provides typed request/response
structs, automatic retry-with-backoff on 429s, and a typed error hierarchy — matching the existing
"buy over build, behind our seam" rationale already used for `go-jira`.

**`internal/correlator`** (new package, sibling to `correlator/refs`/`correlator/fanout`) — the
narrative-clustering unit, split into compute and persist:

- `Cluster(events []Event, existingNarratives []Narrative, llmClient, window TimeRange) ([]ClusterResult, error)` —
  pure compute. Input is the *full* available context: every event already linked to a narrative
  whose window overlaps or sits adjacent to `window`, plus every unlinked event within `window`
  (temporal proximity is real clustering signal; excluding already-narrated events to "dedupe"
  would starve the model of exactly what makes clustering correct). Prompts the model to cluster
  this combined set, each resulting cluster tagged `new` or `extends <narrative_id>` based on
  whether it contains any already-narrated events. Returns in-memory results — no store access.
  **Context-budget overflow (input side):** before calling the LLM, estimate the assembled
  context's token count against `config.LLM.ContextWindowTokens` (a fixed characters-per-token
  approximation is enough for a pre-flight check; exactness isn't needed, only a conservative
  margin). If it doesn't fit, split `window` by time (matching design-notes #9's already-locked
  "page by time, never silently drop" rule for exactly this failure shape — the same mechanism,
  applied here for the first time to narrative context rather than collector scan windows) and
  recurse on each half, merging the resulting narratives afterward. If a single, minimal
  irreducible unit — one event plus one existing narrative's summary — still doesn't fit even at
  the smallest possible split, that's the same "error loudly rather than silently truncate" floor:
  return an error naming the offending narrative/event rather than dropping content to force a
  fit. **Context-budget overflow (cumulative side):** see `Persist`'s tail-summarization step
  below — a long-running narrative's own accreted history is a distinct overflow source from a
  too-wide *window*, and needs a different fix (compaction, not splitting).
- `Persist(store, results []ClusterResult) ([]Narrative, error)` — writes: for `extends`, updates
  the existing row (extends `window_end`, adds new `narrative_events` rows, rewrites `summary` to
  the now-larger cumulative story); for `new`, inserts a fresh row. Returns the narratives
  touched this run (both extended and new) — not the whole table. Skipped entirely under
  `--dry-run`. **Cumulative overflow (tail summarization):** after extending a narrative, if its
  full linked-event history (as `Cluster` would assemble it next run) crosses
  `config.Correlator.TailSummarizeThresholdTokens`, compact everything older than a configured
  recent cutoff (`config.Correlator.RecentEventsKept`, a count, not a duration — a fixed number of
  the most recent events, so this is well-defined regardless of event density) into a shorter
  recap via one LLM call, storing that recap as `narratives.summary`'s older-history prefix.
  Full event rows in `narrative_events` are **never deleted** — only what's fed into future
  `Cluster` context calls is compacted; the actual event log stays intact, so nothing is
  irrecoverably lost even though a future clustering pass sees a summarized version of the old
  tail rather than every raw event. This is a real, scrutinized lossy step (a recap could omit
  something a future correction needs), not a default to take lightly — first-slice
  implementation should log every compaction it performs (which events, into what recap) so it's
  auditable, not silent.

**`internal/reconciler`** (new package) — turns each touched narrative into a proposed action,
same compute/persist split:

- `Reconcile(narratives []Narrative, lastActions map[int64]Action, tracker tasktracker.TaskTracker) ([]ProposedAction, error)` —
  for each narrative, computes the **delta**: `narrative_events` rows added since the last action
  whose `decided_at`/`executed_at` covers this narrative (first pass, no prior action → the whole
  narrative is the delta). This delta is what gets reported/proposed — never the full cumulative
  narrative, so a reviewer sees "what's new since you last saw this," not a repeated history. If
  the narrative has an issue link, calls `tracker.GetIssue` to verify current state before
  drafting anything — never propose off inferred state (the existing `intent-not-outcome` rule,
  now enforced in code, not just documented). Drafts a typed action from the delta only
  (`comment`/`transition`/`create`) with a confidence score. Verification failures (e.g.
  `GetIssue` 404 on a link that no longer resolves) downgrade the narrative to unlinked rather
  than failing the pass, per the existing `verify-correlations` rule.
- `Persist(store, proposed []ProposedAction) ([]Action, error)` — inserts into `actions`
  (`status=proposed`). Skipped under `--dry-run`.

**Auto-commit gate** (inside `watch`, after `reconciler.Persist`) — for each freshly-proposed
action: read `config.AutoCommit[actionType]` (`{ConfidenceFloor float64, Graduated bool}`); if
`action.Confidence >= ConfidenceFloor && Graduated`, apply immediately via the resolved
`TaskTracker` and set `status=applied` (or `failed` on a write error — never silently dropped);
otherwise leave `status=proposed` for `triage`. All-or-nothing per `watch` invocation: if
reconciliation fails partway through a pass, nothing from that pass auto-commits until a clean
pass succeeds. `Graduated` is set only by explicit human action (a config edit, or answering a
graduation prompt surfaced in `triage`) — never flipped by the system on its own, even once
approval history looks clean.

**`internal/rules`** (new package) — the distillation unit `triage` calls on its learn-interval.
`Distill(corrections []Action, llmClient, since time.Time) ([]ProposedRule, error)`: reads
`actions.feedback` (see schema below) for rejected/edited actions since the last learn-check,
prompts the model to draft candidate `rules/*.md` frontmatter+body per correction (or per cluster
of related corrections), returns them for `triage`'s keep/modify/reject loop. No file I/O in this
package — writing `rules/*.md` happens in `triage`, only on `keep`/`modify`.

**In-`triage` rework loop** (distinct from `learn`/rules distillation): when a reviewer rejects
or corrects a specific pending action with free-text feedback ("1 and 3 are actually the same
narrative"), that feedback is sent back to the correlator/reconciler as a follow-up prompt against
the current in-memory batch, producing a revised set of actions, re-presented in the same
session — not deferred to the next `watch` tick. This is separate from `learn`: rework fixes
*this batch* before commit; `learn`/rules teaches *future* runs.

## Data flow: schema additions

- **Config**: `config.LLM.{Model, ContextWindowTokens}` (both required — see the
  `internal/clients/openai` component above for why these aren't looked up or defaulted) plus
  `config.Correlator.{TailSummarizeThresholdTokens, RecentEventsKept}` (see the `Cluster`/`Persist`
  overflow handling above). `config/unjira.example.json` ships populated, working values for a
  known model so setup doesn't require looking these numbers up cold.
- **`actions.feedback TEXT`** (new column) — the reviewer's free-text correction on
  reject/edit, persisted regardless of whether it later becomes a rule. Read by both the
  in-`triage` rework loop (immediately) and `rules.Distill` (later, batched).
- **`actions.confidence`** (already exists) — used by the auto-commit gate.
- **Lease lock**: a new small table (or a single-row sentinel table), not the existing `cursors`
  table (locking is a different concern from watermarking — a lock needs `expires_at`/steal
  semantics `cursors` was never designed for):

  ```sql
  CREATE TABLE IF NOT EXISTS pipeline_lock (
      id         INTEGER PRIMARY KEY CHECK (id = 1),  -- singleton row
      run_id     TEXT NOT NULL,
      held_since TEXT NOT NULL,
      expires_at TEXT NOT NULL
  );
  ```

  `TryAcquire` (used by `watch`): non-blocking; if the row is absent or `expires_at < now()`,
  insert/replace with a new `run_id`/lease window and proceed; otherwise return "locked, skip
  this tick" immediately — no queuing, no retry, to avoid a slow pass cascading into a stacked
  backlog. `Acquire` (used by `triage --refresh`): same steal-if-expired logic, but blocks
  (polling) until it can take the lock rather than giving up. Stealing an expired lock logs a
  warning naming the stale `run_id` and how long it was held — this is the crash-recovery path
  (a killed `watch` process leaves the row locked forever otherwise); TTL sized generously
  relative to observed real pipeline duration.
- **Watermarks**: "since last run" (for `watch`'s window start) and "last learn-check" (for
  `triage`'s interval) both reuse the existing `cursors` table (`collector="watch"`/
  `collector="triage-learn"`, a fixed `resource`) — these are genuinely watermark-shaped
  (a position that only moves forward on success), unlike the lock above.

## Error handling

- **LLM call failures** (rate limit, timeout, malformed response) during
  `Cluster`/`Reconcile`/`Distill`: wrapped and surfaced loudly, never silently truncated —
  `watch` aborts the current pass and retries on the next scheduled invocation rather than
  persisting a partial/corrupt clustering.
- **Context-budget overflow**: see `Cluster`'s split-by-time-and-merge (window too wide) and
  `Persist`'s tail-summarization (one narrative's own history too large) above. Both are bounded
  operations with an explicit floor: if a minimal irreducible unit still doesn't fit, that's a
  loud error, never a silent drop — the same invariant as every other overflow case in this
  design.
- **`TaskTracker` write failures during auto-commit**: `status` flips to `failed` (already in
  the documented status enum), surfaced by `triage` alongside `proposed` actions — a human sees
  write failures, not just pending decisions.
- **Verification-before-action failures**: see reconciler component above — downgrade to
  unlinked, don't fail the pass.
- **Concurrent invocation**: see lease lock above.

## Testing strategy

Following `docs/go-conventions.md` (testify, table-driven, `httptest`-based facades, no
live-network in the offline suite):

- `internal/clients/openai`: `httptest.Server` standing in for the litellm/OpenAI-compatible
  endpoint, same pattern as `clients/jira` — request-shape assertions, error-translation tests,
  no real API calls.
- `internal/correlator`: `Cluster`'s compute half is pure given a fake `llmClient`
  interface (consumer-owned, mirroring the `projectMiner` pattern in `internal/workflow`) — tests
  supply canned model responses and assert on returned structs, no store needed. Separate tests
  cover the extend-vs-new decision and the overlap/adjacency window-selection logic in isolation.
  Overflow-specific tests: a table-driven case asserting the split point given a configured
  `ContextWindowTokens` and an oversized synthetic input (assert the fake `llmClient` is called
  once per split, not once for the whole window); a case asserting a single irreducible
  over-budget unit returns a loud error, not a truncated call. `Persist`'s tail-summarization gets
  its own tests: crossing `TailSummarizeThresholdTokens` triggers exactly one compaction call
  keeping `RecentEventsKept` events verbatim; `narrative_events` rows are asserted to remain
  present in the store after compaction (never deleted), only the *context assembled for the next
  `Cluster` call* is shorter.
- `internal/reconciler`: fake `tasktracker.TaskTracker` (interface already exists) supplies
  canned `GetIssue` responses; tests assert delta computation and action drafting independent of
  whether the LLM call is faked or real.
- Auto-commit gate: pure function of `(action, config) -> commit-now | queue`, table-driven, no
  I/O to fake.
- Lease lock: real SQLite (`t.TempDir()`-backed `Store`, matching every existing store test) —
  timing-sensitive `TryAcquire`/`Acquire`/steal-on-expiry logic is worth testing against the real
  DB, not mocked.
- `internal/rules`: same fake-LLM-client pattern as correlator/reconciler for `Distill`; no real
  file I/O in this package's tests (file writes are `triage`'s job, tested there with a temp
  dir).
- Command-level (`watch`, `triage`, `actions`, `rules`): `cmd/unjira`'s existing `main_test.go`
  precedent (direct `appContext` construction, not CLI parsing) applies here too, plus a few true
  end-to-end tests wiring a fake LLM client + fake tracker + real temp-dir store through the full
  `watch` pipeline, to catch integration seams unit tests can't see.
- Live tier: nothing new needed for this slice — LLM calls are fully faked throughout the offline
  suite. A live LLM-integration tier (real prompt-quality testing) is a separate, later decision.

## Implementation slices

Each slice is implemented, then real usage feeds back into design revisions before the next
slice starts — this is not a fixed waterfall plan.

1. **`internal/clients/openai`** — ✅ landed. Facade + tests, no callers yet. Includes
   `config.LLM.{Model, ContextWindowTokens}` and the populated `config/unjira.example.json`
   values. See `docs/superpowers/plans/2026-08-11-openai-client-facade.md` for the implementation
   plan.
2. **`internal/correlator`** (compute only) — ✅ landed. `Cluster` against `claude_code` events,
   unit-tested with a fake LLM client, including the split-by-time-and-merge overflow path. No
   persistence, no CLI command yet. See
   `docs/superpowers/specs/2026-08-12-correlator-cluster-design.md` for this slice's design and
   `docs/superpowers/plans/2026-08-12-correlator-cluster-implementation.md` for the implementation
   plan.
3. **Persistence + `narrate` groundwork** — `Persist`, the extend-vs-new logic against real
   `narratives`/`narrative_events` rows, tail-summarization overflow handling (`config.Correlator.
   {TailSummarizeThresholdTokens, RecentEventsKept}`), the lease lock (needed even for a
   single-command first cut, since crash-recovery correctness shouldn't be deferred).
4. **`internal/reconciler`** (compute + persist) — delta computation, verification via
   `TaskTracker`, action drafting. Unit-tested with fake trackers.
5. **Auto-commit gate + `watch`** — wires collect → correlator → reconciler → gate into one
   command, `--dry-run` support.
6. **`actions`/`rules` primitives + `triage`** — the human-facing surface, built on the
   primitives; in-session rework loop; `--auto-approve`/`--refresh`/`--dry-run`.
7. **`internal/rules` (Distill) + learn-interval surfacing in `triage`** — rule proposal
   generation and the keep/modify/reject write path.

First slice scope (per the "one collector through the full loop" decision): everything above
runs against `claude_code` events only. A second collector (GitHub, Slack) is explicitly future
work, not blocking this vertical.
