# `unjira dev narrate` — design

Runs one real narration pass — collect → `Cluster` → `Persist` under the pipeline lease — and prints
the narratives it wrote in enough detail for a human to judge whether the clustering is any good.

**Why this exists.** Slice 3 landed `correlator.Persist`, tail-summarization compaction, and the
`pipeline_lock` lease, but nothing in `cmd/unjira` calls any of them. Every test to date runs against
fakes and temp SQLite files. This command is the first execution of that machinery against real
Claude Code history and a real LLM, and the first exercise of the Path-B hydration round trip
(`Persist` writes links → a later `Cluster` reads them back as prompt context).

## Command placement: `dev`, not a top-level verb

`docs/superpowers/specs/2026-08-11-phase1-correlator-design.md` commits to a two-verb product
surface — `watch` and `triage` — plus the `actions`/`rules` primitive families. `narrate` appears
there only as a *stage inside* `watch`'s pipeline, never as something a user runs. A top-level
`unjira narrate` would therefore be scaffolding slated for removal.

`unjira dev` already exists for inspection tooling that isn't product surface (`dev seed`,
`dev reset`, `dev workflow` — the last of which mines and prints the observed workflow graph, the
same "run a real stage and show me the output" shape). `dev narrate` belongs there and survives
indefinitely: when `watch` lands it calls the same orchestration function, and `dev narrate` remains
the way to run a single pass and inspect it.

## Scope

In scope:

- Two new `internal/store` accessors that assemble `Cluster`'s inputs (see below) — a gap slice 3
  left open.
- `pipeline.RunNarrate` — the reusable orchestration `watch` will call.
- The `dev narrate` CLI command, its output rendering, and `--dry-run`.
- `UNJIRA_LLM_API_KEY` wiring in `cmd/unjira`, deferred from slice 1 and needed here for the first
  time.

Not in scope: the reconciler, actions, the auto-commit gate, `watch` itself.

## The gap this closes

`Cluster`'s signature needs two inputs the store could not produce:

```go
func Cluster(ctx, evts []Event, existing []Narrative, llm, window TimeRange, contextWindowTokens int)
```

Slice 3 specified how to *hydrate* a narrative's events (`NarrativeEventsForContext`) and how to
*persist* results, but never how a caller finds the events to cluster or the narratives to pass as
context. Nothing selected "events not yet linked to a narrative" (no `NOT EXISTS`/`LEFT JOIN`
anywhere in the store), and every `FROM narratives` query was `WHERE id = ?`. The hole was invisible
while all tests built their inputs by hand.

Both accessors below are permanent: `watch` needs exactly the same inputs.

### `UnlinkedEventsInRange(start, end time.Time) ([]events.Event, error)`

```sql
SELECT e.source, e.external_id, e.occurred_at, e.actor, e.summary, e.artifacts, e.raw_ref
FROM events e
WHERE e.occurred_at >= ? AND e.occurred_at < ?
  AND NOT EXISTS (SELECT 1 FROM narrative_events ne WHERE ne.event_id = e.id)
ORDER BY e.occurred_at, e.id
```

- **"Unlinked" means no `narrative_events` row at all** — not "linked to a narrative outside this
  window." An event belongs to exactly one narrative; once linked it is never a clustering candidate
  again. It can still reach a prompt as *context*, via its narrative's hydration — that is the
  Path-B design, and it is why deduping candidates this way does not starve the model.
- **Half-open `[start, end)`**, matching `TimeRange`'s documented semantics and
  `filterEventsInWindow`.
- `ORDER BY occurred_at, id` — the same composite ordering `NarrativeEventsForContext` uses, because
  `occurred_at` is stored via `time.RFC3339` (whole seconds) and cannot uniquely order events.

### `NarrativesOverlapping(start, end time.Time) ([]NarrativeRow, error)`

```sql
SELECT id, window_start, window_end, title, summary, issue_key, confidence, status,
       compaction_boundary, compaction_boundary_event_id
FROM narratives
WHERE window_end >= ? AND window_start <= ?
ORDER BY window_start, id
```

- **Mirrors `filterAdjacentOrOverlapping` exactly**: keep unless
  `window_end < start || window_start > end`. Touching endpoints count as adjacent — deliberate,
  since temporal proximity is real clustering signal.
- Returns `NarrativeRow`, not `correlator.Narrative`: `internal/store` must never import
  `internal/correlator`. The pipeline layer maps and hydrates.
- **`status` is ignored.** Every narrative is `'open'` today and no code sets otherwise, so
  filtering on it would be speculative. When `status` becomes meaningful, this query is where the
  predicate goes.

### Why SQL filters when `Cluster` also filters

The two bound different things, and neither is redundant.

**SQL bounds fetch and hydration cost.** Hydration is one `NarrativeEventsForContext` call per
narrative returned, so an unbounded `NarrativesOverlapping` would hydrate every narrative ever
written.

**`Cluster`'s Go filters bound each prompt, and are re-derived at every bisection level.**
`clusterWithSplit` recurses with the *original* `evts` and `existing` slices, narrowing only the
window:

```go
firstResults, err := Cluster(ctx, evts, existing, llm, firstHalf, contextWindowTokens)
//                              ^^^^  ^^^^^^^^  the full sets, not the halves
```

The recursive call's first two lines are what actually narrow it. Remove them and bisection breaks:
each recursion would rebuild the identical full-window prompt, overflow again, and bisect until the
irreducible-unit check fires — the window shrinking while the content never does. The same
re-derivation is what makes each half's *context* correct: a narrative adjacent to the first half may
be distant from the second, and `filterAdjacentOrOverlapping` re-evaluates that per level.

They look duplicative only at the top-level call, where SQL has already narrowed to the same window
and the Go filter passes everything through. At depth 1+ the Go filter is doing all the work.

**Consequence for orchestration:** `RunNarrate` fetches **once** for the outer window and hands the
full slices to `Cluster`. It must not re-query per bisected sub-window — bisection happens inside
`Cluster`, which has no store by design.

## Orchestration: `pipeline.RunNarrate`

`internal/pipeline` already owns this role: `RunCollect(config, store, registry, exclusions)` and
`RenderDigest(store, day)` both orchestrate a stage, return results, and leave printing to the CLI.
`RunNarrate` sits beside them.

```go
// NarrateOptions configures one narration pass.
type NarrateOptions struct {
    // DryRun runs the full pass — including the real LLM calls — but skips
    // Persist, so nothing is written. The printed narratives are exactly what
    // would have been persisted.
    DryRun bool
}

// NarrateResult is one pass's outcome, rich enough for a human to judge the
// clustering without re-querying.
type NarrateResult struct {
    Window            correlator.TimeRange
    UnlinkedEvents    int   // candidates considered
    ContextNarratives int   // passed to Cluster as context
    LLMCalls          int   // includes bisection splits and merge checks
    Narratives        []NarratedNarrative
    Compactions       []Compaction
}

// NarratedNarrative is one narrative Cluster produced, with the member events
// that justify the grouping.
type NarratedNarrative struct {
    Kind         correlator.ClusterKind // new | extends
    ID           int64                  // 0 when DryRun (never persisted)
    PriorWindowEnd time.Time            // zero unless Kind == extends
    WindowStart  time.Time
    WindowEnd    time.Time
    Title        string
    Summary      string
    Events       []correlator.Event
}

// Compaction records one tail-summarization, so the lossy step is visible.
type Compaction struct {
    NarrativeID  int64
    EventsFolded int
    Boundary     time.Time
}

func RunNarrate(
    ctx context.Context,
    s *store.Store,
    llm correlator.LLMClient,
    cfg config.Config,
    window correlator.TimeRange,
    opts NarrateOptions,
) (NarrateResult, error)
```

Sequence:

1. `s.UnlinkedEventsInRange(window.Start, window.End)` → candidates.
2. `s.NarrativesOverlapping(window.Start, window.End)` → map each `NarrativeRow` to
   `correlator.Narrative` and hydrate `.Events` via `s.NarrativeEventsForContext(row.ID)`.
3. `correlator.Cluster(ctx, events, narratives, countingLLM, window, cfg.LLM.ContextWindowTokens)`.
4. Unless `opts.DryRun`: `correlator.Persist(ctx, s, countingLLM, results, cfg.Correlator)`.
5. Assemble `NarrateResult`.

Early return: zero unlinked events → a `NarrateResult` with counts populated, no LLM call, no error.
An empty window is a normal outcome, not a failure.

### The lease is the caller's responsibility

`RunNarrate` does **not** acquire `pipeline_lock`. `dev narrate` wraps one stage in a lease; `watch`
will wrap collect → narrate → reconcile in a *single* lease. If `RunNarrate` acquired it internally,
`watch` would deadlock against itself or need an "already held" parameter — a smell. Keeping
acquisition in the caller gives both call sites identical semantics.

`dev narrate` acquires the lease **even under `--dry-run`**, so a dry run cannot interleave with a
real pass and read half-written state.

### Pass stats via a counting decorator

`Cluster` returns only `[]ClusterResult` — LLM call count and split/merge activity are not
observable. Rather than change correlator code that just completed two review cycles, the pipeline
layer wraps the client:

```go
// countingLLM counts Complete calls so a pass can report how many LLM round
// trips it took — the signal that reveals bisection or compaction activity a
// caller would otherwise not see.
type countingLLM struct {
    inner correlator.LLMClient
    calls int
}
```

This yields a total, not a breakdown (a bisect split is indistinguishable from a merge check). That
is enough for its purpose: spotting a pathological pass. `Compactions` is populated separately, from
`Persist`'s returned narratives compared against their pre-pass `compaction_boundary`.

**Required change:** `correlator.llmClient` is unexported, so `internal/pipeline` cannot name it.
Export it as `correlator.LLMClient` (same single-method shape). This is a rename of an existing
interface, not a new abstraction — `Cluster` and `Persist` keep taking it.

## CLI: `unjira dev narrate`

```
unjira dev narrate [--since 7d] [--dry-run]
```

- `--since` — a `time.Duration`, default `24h`. Kong parses this natively, which means
  `time.ParseDuration` rules apply: `24h` and `168h` are valid, **`7d` is not** (`time: unknown unit
  "d"`). Days are not a `ParseDuration` unit. Accept that rather than hand-rolling a parser: the help
  text gives `168h` as the week-long example, so the constraint is discoverable instead of a runtime
  surprise. The window is `TimeRange{now-since, now}` in UTC.

  **One window**, not a loop: `Cluster` bisects only when the prompt would overflow
  `cfg.LLM.ContextWindowTokens`, so splits are adaptive rather than falling on arbitrary calendar
  boundaries, and the model sees the widest context available.
- `--dry-run` — real LLM calls, `Persist` skipped, lease still taken. Matches the phase-1 spec's
  definition of `watch --dry-run` ("full pipeline runs in-memory, nothing persisted").

Flow in `devNarrateCmd.Run(app *appContext)`:

1. Build the LLM client: `openai.New(cfg.LLM.BaseURL, apiKey, cfg.LLM.Model)`.
2. `app.store.Acquire(ctx, runID, time.Now, ttl, poll)` — blocking, so a concurrent pass makes this
   wait rather than fail. `defer app.store.ReleaseLock(runID)`.
3. `pipeline.RunCollect(...)` — reusing `collectCmd`'s existing path, so narration sees events
   collected in the same invocation.
4. `pipeline.RunNarrate(...)`.
5. Render.

`runID` is `fmt.Sprintf("dev-narrate-%d", os.Getpid())` — identifies the holder in the lease's
steal-on-expiry log without needing a UUID dependency.

### `UNJIRA_LLM_API_KEY`

Slice 1 deferred this wiring; this command is the first thing that needs it. Add to the Kong CLI
struct, following the `UNJIRA_JIRA_CREDENTIALS` precedent — credentials come from the environment,
never config files:

```go
LLMAPIKey string `env:"UNJIRA_LLM_API_KEY" help:"API key for the LLM backend."`
```

Missing or empty when `dev narrate` runs → a loud error naming the variable, before any collect work.

### Output

```
== narration pass ==
window   2026-08-12T00:00:00Z .. 2026-08-19T00:00:00Z
events   47 unlinked candidates, 3 narratives as context
llm      4 call(s)
compact  narrative 9: folded 12 event(s) up to 2026-08-14T08:00:00Z

[NEW #14] 2026-08-14T09:12:00Z .. 2026-08-14T17:40:00Z
  "Fix flaky correlator tests"
  Chased an intermittent failure in the split/merge path; root-caused to a
  shared fixture clock and switched to per-test injection.
  events (5):
    - [claude_code] unjira: 3 user messages.          2026-08-14T09:12:00Z
    - [claude_code] unjira: 11 user messages.         2026-08-14T11:30:00Z
    ...

[EXTENDS #9] 2026-08-10T14:00:00Z .. 2026-08-18T16:20:00Z  (window_end was 2026-08-16T09:00:00Z)
  "Cache rework"
  ...
```

`--dry-run` prints `[NEW  -]` for the id (nothing was persisted) and a `dry run: nothing persisted`
line under the header. Zero narratives prints the header plus `no narratives produced`.

Rationale for the detail level: member events are what make the grouping judgeable — the whole point
is deciding whether the clustering is defensible, which a title and count cannot show. The stats
line catches pathological passes (an unexpected call count means bisection fired; a compaction on a
small narrative means the threshold is mistuned).

## Error handling

- Missing `UNJIRA_LLM_API_KEY`, or `cfg.LLM`/`cfg.Correlator` failing `Validate()` → error before
  any work.
- Lease acquisition honors `ctx` cancellation (Ctrl-C during a wait exits cleanly).
- `ReleaseLock` is deferred and its error logged, not returned — it must not mask a real failure from
  the pass. Releasing when not the holder is already a logged no-op.
- `Cluster`'s irreducible-unit error (a single event too large to fit the context budget) propagates
  unchanged. It names the window and event; that is the actionable signal, and swallowing it would
  silently drop work.
- `Persist` is all-or-nothing; a failure means nothing from the pass was written, and the command
  reports the error rather than a partial dump.

## Testing

Offline, no network, following the repo's existing patterns:

- **Store accessors** (`internal/store`): real temp-file SQLite. `UnlinkedEventsInRange` — excludes
  linked events, respects the half-open boundary at both ends, orders by `(occurred_at, id)` across
  a same-second tie. `NarrativesOverlapping` — includes overlapping and exactly-touching windows,
  excludes strictly disjoint ones.
- **`RunNarrate`** (`internal/pipeline`): real store + the `fakeLLM` pattern from
  `correlator_test.go`. Cases: new narrative persisted end to end; `DryRun` writes nothing while
  still reporting what it would have written; zero candidates makes no LLM call; hydrated context
  narratives reach `Cluster`'s prompt (the Path-B round trip); `LLMCalls` reflects actual calls.
- **Rendering**: a pure function from `NarrateResult` to string, table-tested — including the
  zero-narrative and dry-run variants.

Per this project's practice, each regression test is validated by deliberately breaking the behavior
it guards and confirming the test fails. Three tests in slice 3 looked like they covered a failure
mode but did not; this is the check that caught them.

The live tier needs nothing new — real-LLM narration quality is judged by running the command, which
is the command's purpose.
