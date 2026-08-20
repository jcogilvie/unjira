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
- `config.Span`, a duration type accepting day/week units (delegating to
  `github.com/xhit/go-str2duration/v2`, a new direct dependency), since `time.ParseDuration`
  rejects them.
- A new `internal/llm` contract package (`llm.Client`, `llm.Usage`), mirroring
  `internal/tasktracker`'s backend-agnostic shape.
- LLM observability: `Complete` returns `llm.Usage` (currently discarded) and `Cluster`/`Persist`
  return a `correlator.Stats`. This touches slice-1/2/3 call sites deliberately.
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
    Stats             correlator.Stats // calls, splits, tokens; see below
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
    client llm.Client,
    cfg config.Config,
    window correlator.TimeRange,
    opts NarrateOptions,
) (NarrateResult, error)
```

Sequence:

1. `s.UnlinkedEventsInRange(window.Start, window.End)` → candidates.
2. `s.NarrativesOverlapping(window.Start, window.End)` → map each `NarrativeRow` to
   `correlator.Narrative` and hydrate `.Events` via `s.NarrativeEventsForContext(row.ID)`.
3. `correlator.Cluster(ctx, events, narratives, client, window, cfg.LLM.ContextWindowTokens)`.
4. Unless `opts.DryRun`: `correlator.Persist(ctx, s, client, results, cfg.Correlator)`.
5. Assemble `NarrateResult`.

Early return: zero unlinked events → a `NarrateResult` with counts populated, zero-valued `Stats`, no LLM call, no error.
An empty window is a normal outcome, not a failure.

### The lease is the caller's responsibility

`RunNarrate` does **not** acquire `pipeline_lock`. `dev narrate` wraps one stage in a lease; `watch`
will wrap collect → narrate → reconcile in a *single* lease. If `RunNarrate` acquired it internally,
`watch` would deadlock against itself or need an "already held" parameter — a smell. Keeping
acquisition in the caller gives both call sites identical semantics.

`dev narrate` acquires the lease **even under `--dry-run`**, so a dry run cannot interleave with a
real pass and read half-written state.

### LLM observability: token usage threaded through, not a call counter

An earlier draft wrapped the client in a decorator that counted `Complete` calls. That was too
little, for three reasons:

1. **We already discard the data that matters.** `openai.Client.Complete` returns only
   `resp.Choices[0].Message.Content` — `resp.Usage` (prompt, completion, and total tokens, all
   `int64`) and `resp.Model` are thrown away. Token counts, not call counts, are what govern cost and
   reveal whether `LLM.ContextWindowTokens` is tuned correctly.
2. **`estimateTokens` has never been validated.** It is `(len(text)+3)/4` — a heuristic that decides
   *when `Cluster` bisects*. Nothing has ever compared it against a real tokenizer. If it is off by
   30%, bisection fires at the wrong threshold, which is a correctness problem, not just missing
   telemetry. Capturing actual usage alongside the estimate makes the heuristic checkable.
3. **The metric set is already growing** — calls, splits, merge checks, compactions, and now tokens,
   with per-run cost accounting needed by slice 5's `watch` gate. A bare `int` counter gets discarded
   at that point.

So usage flows from the client through the correlator, rather than being inferred outside it.

**`internal/clients/openai`:**

#### `internal/llm` — the backend-agnostic contract

The interface and the usage type go in their own contract package, **not** in `clients/openai` and
**not** in `correlator`. This mirrors `internal/tasktracker` exactly, whose package doc states the
rule this repo already follows:

> Package tasktracker defines the backend-agnostic interface phase-1's correlator/reconciler/applier
> use [...] plus the normalized types every backend (Jira, GitHub Issues, a local no-op-tracker)
> speaks. **It has no imports of `internal/clients`** — like `internal/events`, it's a shared contract
> with multiple producers and no single owning consumer.

The dependency direction there is the inverse of a client-owned type: `clients/jira/tracker.go` and
`clients/local/local.go` both import `internal/tasktracker`, and nothing imports back. `clients/local`
was added alongside `clients/jira` with no upstream changes, which is the property we want here.

```go
// Package llm defines the backend-agnostic interface the correlator uses to
// reach a language model, plus the normalized usage every backend reports.
// It has no imports of internal/clients — like internal/tasktracker and
// internal/events, it's a shared contract with multiple producers
// (clients/openai today, an Anthropic-shaped client later) and no single
// owning consumer.
package llm

// Client is the narrow capability the correlator needs from any LLM backend.
type Client interface {
    Complete(ctx context.Context, systemPrompt, userPrompt string) (string, Usage, error)
}

// Usage reports what one completion actually consumed, as the server counted
// it — distinct from correlator.estimateTokens's len/4 heuristic, which only
// has to be good enough to decide whether to split before spending a call.
// Comparing the two is how that heuristic gets validated.
type Usage struct {
    PromptTokens     int64
    CompletionTokens int64
    // Model is what the server reported serving, which can differ from the
    // model requested when a gateway (litellm, OpenRouter) remaps it.
    Model string
}
```

`clients/openai` imports `internal/llm` and returns `llm.Usage`; `correlator` imports `internal/llm`.
Neither knows the other exists, so a future `clients/anthropic` satisfies `llm.Client` with no change
upstream. Had `Usage` lived in `clients/openai`, an Anthropic client would have had to import a
*competing provider's* package to return its own token counts.

This also replaces an earlier draft's plan to export `correlator.llmClient` as
`correlator.LLMClient`. Consumer-defined interfaces are idiomatic Go in general, but not when there
are already two prospective implementations plus a normalized value type they must agree on — that is
the case a shared contract package exists for, and the case `tasktracker` is already an instance of.
`correlator.llmClient` is deleted; `Cluster`/`Persist` take `llm.Client`.

```go
func (c *Client) Complete(ctx context.Context, systemPrompt, userPrompt string) (string, llm.Usage, error)
```

**`internal/correlator`:** `Cluster` and `Persist` aggregate usage across every call they make —
including recursive bisection calls and the compaction call.

```go
// Stats is what one Cluster or Persist call spent and why. Splits and
// MergeChecks are what distinguish an expensive-but-correct pass (a wide
// window that bisected) from a pathological one.
type Stats struct {
    Calls            int
    Splits           int   // window bisections
    MergeChecks      int   // same-story checks at split seams
    Compactions      int   // Persist only
    PromptTokens     int64
    CompletionTokens int64
    EstimatedTokens  int   // estimateTokens's guess, for comparison
}

func Cluster(...) ([]ClusterResult, Stats, error)
func Persist(...) ([]Narrative, Stats, error)
```

`Stats` values from recursive `Cluster` calls sum into the parent's, so a top-level call reports the
whole tree. `RunNarrate` merges `Cluster`'s and `Persist`'s into `NarrateResult.Stats`.

**Call-site churn:** `Cluster` has 4 call sites (itself twice in `clusterWithSplit`, `checkSameStory`,
and `RunNarrate`), `Persist` 1, plus `fakeLLM` and every correlator test asserting on returns. The
user confirmed this churn is acceptable given the utility, and that Serena makes the mechanical part
cheap.

## CLI: `unjira dev narrate`

```
unjira dev narrate [--since 7d] [--dry-run]
```

- `--since` — a `config.Span` (see below), default `24h`. Accepts day and week units, so `--since 7d`
  works. The window is `TimeRange{now-since, now}` in UTC.

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

### `config.Span` — durations that accept days and weeks

`time.ParseDuration` stops at `h`: `d`, `w`, and `y` are all rejected. That is deliberate in the
stdlib — a calendar day is not reliably 24h once DST is involved, so Go refuses to guess. There is no
stdlib type that accepts day units, and no existing dependency provides one.

The stdlib's objection does not apply here. unjira's windows are **UTC** spans measured back from
now, and UTC has no DST, so a day is exactly 24h and a week exactly 168h. Requiring operators to
write `168h` for "a week" is a needless arithmetic burden on the tool's most common use.

**Do not hand-roll the parser.** `github.com/xhit/go-str2duration/v2` does this properly, and better
than a local implementation would: it handles *compound* forms (`7d12h`, `1w2d3h4m`), which a simple
"strip the trailing `d`, multiply" approach cannot. It is single-purpose, has no transitive
dependencies beyond the stdlib, and delegates to `time.ParseDuration` for the units the stdlib already
covers.

Worth noting what the ecosystem does here, since cert-manager was raised as a possible precedent: it
does **not** solve this. Its docs state the `duration`/`renewBefore` fields "must be specified using a
Go `time.Duration` string format, which does not allow the `d` (days) suffix," and its own examples
read `duration: 2160h # 90d` — a comment doing the arithmetic. That is the outcome to avoid, not
imitate.

Kong decodes any type implementing `encoding.TextUnmarshaler`, so `Span` is a thin wrapper that
delegates parsing and adds one validation. The same type works for JSON config fields.

```go
// Span is a duration that also accepts day ("7d") and week ("2w") units,
// including compound forms ("7d12h"), which time.ParseDuration rejects — it
// stops at "h", deliberately, since a calendar day is not reliably 24h under
// DST. unjira measures spans backward from now in UTC, where a day is exactly
// 24h, so that ambiguity cannot arise here.
//
// Parsing is delegated to github.com/xhit/go-str2duration; this type exists
// to satisfy encoding.TextUnmarshaler (which Kong and encoding/json both
// honor) and to reject non-positive spans.
type Span time.Duration

func (s *Span) UnmarshalText(text []byte) error
func (s Span) Duration() time.Duration
```

Parsing behavior, verified against the library:

| Input | Result | Note |
| --- | --- | --- |
| `7d`, `2w` | `168h`, `336h` | the point of the type |
| `7d12h`, `1w2d3h4m` | `180h`, `219h4m` | compound forms — why we use the library |
| `36h`, `90m`, `30s` | unchanged | stdlib units still work |
| `1y` | error | years are not supported (ambiguous, and rightly so) |
| `7D` | error | case-sensitive, matching the stdlib (`1H` also fails) |
| `""` | error | empty is not a duration |
| `-3d`, `0s` | **error — added by `Span`** | see below |

**Non-positive spans are rejected by `Span`, not the library.** `str2duration` parses `-3d` happily as
`-72h`, which would make `window.Start` later than `now` and yield an empty window with no error — a
pass that silently does nothing. `0s` is equally meaningless. `UnmarshalText` rejects both with an
error naming the value, per this repo's never-silently-drop-data invariant. This is the one piece of
behavior `Span` adds beyond delegation, and the one that most needs a test.

Used by `--since` now; `watch --interval` and any future lookback/retention setting reuse it, so
duration syntax stays consistent across every flag and config field.

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
llm      4 call(s), 2 split(s), 1 merge check
tokens   38,412 prompt + 1,205 completion (estimated 41,000)
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
  narratives reach `Cluster`'s prompt (the Path-B round trip); `Stats` aggregates correctly across
  a bisected pass (a `fakeLLM` returning canned usage proves the sums, including the recursive
  `Cluster` calls).
- **Rendering**: a pure function from `NarrateResult` to string, table-tested — including the
  zero-narrative and dry-run variants.
- **`config.Span`**: table-driven over every row in the parsing table above. The library's own
  behavior needs only light coverage (it has its own tests); what must be covered is the behavior
  `Span` adds — rejecting `-3d` and `0s`. Without that, a backwards window silently produces an
  empty pass.
- **`llm.Usage` plumbing**: the existing `httptest`-based `clients/openai` test asserts usage is
  populated from the response body rather than dropped. `internal/llm` itself needs no tests — it
  is types and an interface only, exactly like `internal/tasktracker`.

Per this project's practice, each regression test is validated by deliberately breaking the behavior
it guards and confirming the test fails. Three tests in slice 3 looked like they covered a failure
mode but did not; this is the check that caught them.

The live tier needs nothing new — real-LLM narration quality is judged by running the command, which
is the command's purpose.
