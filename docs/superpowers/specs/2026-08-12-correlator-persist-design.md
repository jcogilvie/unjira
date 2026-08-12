# Phase-1 slice 3: `correlator.Persist` + narrative persistence + tail-summarization + lease lock

Status: approved (design brainstorm). Scope: slice 3 of
`docs/superpowers/specs/2026-08-11-phase1-correlator-design.md`'s "Implementation slices" —
persisting `Cluster`'s results to real `narratives`/`narrative_events` rows, tail-summarization of a
narrative's accreted history, the narrative→events hydration accessor the reworked `Cluster` needs,
and the leased pipeline lock. No CLI command, no reconciler. First slice that writes to the
`narratives`/`narrative_events` tables (schema-only since phase 0).

**Executed as one combined effort with the slice-2 rework**
(`2026-08-12-correlator-hydrated-context-rework.md`): that rework makes `Cluster` consume each
narrative's raw events as context, and this slice provides both the hydration accessor that feeds it
and the tail-summarization that keeps that context bounded. The two are entangled and land together.

## Path B, and what it reverses from the earlier draft

An earlier draft of this spec took "Path A" — narratives carried forward by summary only — and, on
that basis, *dropped* tail-summarization in favor of a simple `MaxSummaryTokens` guard and a
pure-store-I/O `Persist` with no LLM. That draft has been withdrawn. We are restoring the phase-1
spec's original "Path B" intent (raw narrative events as clustering context, chosen for accuracy;
see the rework doc for the full rationale and the reversibility analysis showing this stays a cheap
decision to revisit). Consequences for this slice, relative to that withdrawn draft:

- **`Persist` takes an `llmClient`** (for tail-summarization compaction) — it is no longer pure
  store I/O.
- **Tail-summarization is back in scope**, replacing the `MaxSummaryTokens`-only guard.
  `config.Correlator` carries `{TailSummarizeThresholdTokens, RecentEventsKept}`.
- **A compaction boundary** is tracked per narrative, so hydration and future clustering see the
  recent raw tail plus a recap of the compacted-away older events.

The invariant that keeps Path B ↔ Path A reversible is preserved: `narrative_events` links are
always written in full, and the cumulative summary is always kept current. Nothing here destroys the
substrate needed to switch representation later.

## `Persist` — signature and contract

```go
// in internal/correlator/correlator.go (same package as Cluster)

// Persist writes Cluster's results to the narratives/narrative_events
// tables: ClusterNew inserts a fresh narrative row, ClusterExtends updates
// an existing one (extends window_end, overwrites cumulative summary, links
// new events). After an extend, if the narrative's accreted history crosses
// the tail-summarize threshold, older events are compacted into the summary's
// recap prefix via one LLM call (their narrative_events rows are never
// deleted). Returns the narratives touched this run. All-or-nothing per call:
// any failure aborts the whole pass with a loud error and (via a transaction)
// persists nothing.
func Persist(
	ctx context.Context,
	s *store.Store,
	llm llmClient,
	results []ClusterResult,
	cfg CorrelatorConfig,
) ([]Narrative, error)
```

Processing, per result, in slice order, within a single SQL transaction (see Error handling):

1. **Resolve event IDs:** for each event in `result.Events`, look up its DB row id via
   `store.EventIDByExternalID(event.Source, event.ExternalID)`. A cluster event with no matching row
   is a loud error (should never happen — `Cluster` only operates on already-persisted events — but
   caught, never silently skipped).
2. **`ClusterNew`:** insert a `narratives` row: `window_start`/`window_end` = min/max `OccurredAt`
   across `result.Events`; `title`/`summary` from the result; `status = 'open'`;
   `compaction_boundary` = NULL (nothing compacted yet). `issue_key`/`confidence` left NULL
   (reconciler work, later slice). Insert one `narrative_events` row per resolved event id.
3. **`ClusterExtends`:** load the narrative by `result.NarrativeID`. **If no row exists, loud error
   naming the missing id** (the hallucinated/stale-`narrative_id` validation deferred from slice 2's
   review — a bad id would orphan a link or lose events, so it fails the pass rather than guessing or
   silently creating a new narrative). Otherwise: advance `window_end` to max(existing, this batch's
   latest event) — never backward; overwrite `summary` with `result.Summary`; insert new
   `narrative_events` rows (`INSERT OR IGNORE` — re-linking is a harmless no-op).
4. **Tail-summarization (extends only, after the link write):** compute the narrative's post-boundary
   history size (recap prefix already in `summary`, plus the raw events newer than
   `compaction_boundary`). If it crosses `cfg.TailSummarizeThresholdTokens`, compact: keep the newest
   `cfg.RecentEventsKept` events raw, send the rest (older post-boundary events + the existing recap)
   to `llm` for a fresh recap, store that recap as the summary's leading prefix, and advance
   `compaction_boundary` to the occurred_at of the newest compacted event. `narrative_events` rows
   are **never deleted** — only the *context assembled for future Cluster calls* shrinks. Every
   compaction logs what it folded (which events → what recap) so the lossy step is auditable, not
   silent.

Empty `results` → `(nil, nil)`, no-op.

`Persist` uses the same consumer-owned `llmClient` interface `Cluster` already declares — one fake
serves both in tests.

## Event-ID resolution

`ClusterResult.Events` carry no DB row id (the `Event` struct dedupes on `Source`+`ExternalID`; no
`ID` field). `Persist` resolves each to `events(id)` via a store method against the existing
`UNIQUE(source, external_id)` index — cheap, unambiguous, and it keeps the reworked `Cluster` free of
any id-threading (it still passes whole `Event` values around, now including narrative context
events).

```go
// EventIDByExternalID returns the row id of the event with the given
// (source, external_id). Returns ErrEventNotFound if none — a loud-error
// case for callers, since clustering an event implies it was persisted first.
func (s *Store) EventIDByExternalID(source, externalID string) (int64, error)
```

New sentinel `store.ErrEventNotFound` (`errors.Is`-checkable, mirroring `ErrLocalIssueNotFound`).

## Store: schema additions

```sql
-- new column on the existing narratives table
ALTER TABLE narratives ADD COLUMN compaction_boundary TEXT;  -- occurred_at of newest compacted event; NULL = never compacted
```

Since `internal/store` applies its schema with `CREATE TABLE IF NOT EXISTS` at `Open` (no migration
framework), and phase 0 never wrote narrative rows, the column is added directly to the `narratives`
`CREATE TABLE` in the `schema` string. (A fresh DB gets it; there is no populated production DB to
migrate — narratives have never been written. If that assumption is wrong at implementation time,
fall back to an idempotent `ALTER TABLE … ADD COLUMN` guard, but the create-time addition is expected
to suffice.)

## Store accessors (new)

House style: parameterized SQL, wrapped errors, no business logic. `store` exposes its own row types
so it never imports `correlator` (dependency direction stays correlator→store).

```go
type NarrativeRow struct {
	ID                 int64
	WindowStart        time.Time
	WindowEnd          time.Time
	Title              string
	Summary            string
	IssueKey           string     // "" when NULL
	Confidence         float64    // 0 when NULL
	Status             string
	CompactionBoundary *time.Time // nil = never compacted
}

var ErrNarrativeNotFound = errors.New("narrative not found") // mirrors ErrLocalIssueNotFound

func (s *Store) InsertNarrative(windowStart, windowEnd time.Time, title, summary string) (int64, error)
func (s *Store) GetNarrative(id int64) (NarrativeRow, error) // ErrNarrativeNotFound if absent
func (s *Store) ExtendNarrative(id int64, windowEnd time.Time, summary string) error
func (s *Store) SetCompactionBoundary(id int64, boundary time.Time, recapSummary string) error
func (s *Store) AddNarrativeEvents(narrativeID int64, eventIDs []int64) error // INSERT OR IGNORE
func (s *Store) EventIDByExternalID(source, externalID string) (int64, error)

// NarrativeEventsForContext returns the events a narrative contributes as
// Cluster context: those with occurred_at strictly after compaction_boundary
// (all of them when boundary is NULL), ordered by occurred_at. The caller
// attaches these to correlator.Narrative.Events before calling Cluster; the
// recap of everything at/before the boundary already lives in the summary.
func (s *Store) NarrativeEventsForContext(narrativeID int64) ([]events.Event, error)
```

`NarrativeEventsForContext` joins `narrative_events` → `events`, filtering by the narrative's
`compaction_boundary`. This is the accessor the reworked `Cluster`'s caller uses to hydrate
`Narrative.Events`.

### Transaction wrapping

`Persist`'s all-or-nothing contract runs its writes (and the compaction update) in one transaction.
Add a transaction seam — either a `store.WithTx(func(tx *store.Tx) error) error` helper exposing the
same accessor methods on a `*Tx`, or accessors taking a `dbtx` interface satisfied by both `*sql.DB`
and `*sql.Tx`. Shape is an implementation-plan detail; the contract is: **all results persist, or
none.** Note the tail-summarization LLM call happens *inside* the pass — if it fails, the whole pass
rolls back (no half-compacted narrative). This means the LLM call is on the transaction's critical
path; acceptable for a batch pipeline, and the alternative (compact out-of-band later) is deferred.

## Lease lock (new)

Per the phase-1 spec's "Lease lock" section. Built and tested now, no caller yet — the seam slice
5's `watch`/`triage --refresh` hang off; crash-recovery correctness shouldn't wait for the caller.

```sql
CREATE TABLE IF NOT EXISTS pipeline_lock (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    run_id     TEXT NOT NULL,
    held_since TEXT NOT NULL,
    expires_at TEXT NOT NULL
);
```

```go
// TryAcquire: non-blocking. Succeeds if unheld or lease expired (expires_at <
// now); else false immediately (no queuing/retry, so a slow pass never stacks
// a backlog). Stealing an expired lease logs a warning naming the stale run_id
// and how long it was held (crash-recovery path). now passed explicitly for
// deterministic tests.
func (s *Store) TryAcquire(runID string, now time.Time, ttl time.Duration) (bool, error)

// Acquire: TryAcquire's blocking sibling (triage --refresh). Same steal-if-
// expired logic, polls until acquirable, honors ctx cancellation. Takes a
// clock func since it loops.
func (s *Store) Acquire(ctx context.Context, runID string, now func() time.Time, ttl, poll time.Duration) error

// ReleaseLock: releases iff currently held by runID (no-op otherwise, so it
// never clobbers a newer holder that stole an expired lease).
func (s *Store) ReleaseLock(runID string) error
```

## Config additions

```go
// CorrelatorConfig configures narrative persistence + compaction.
type CorrelatorConfig struct {
	// TailSummarizeThresholdTokens: once a narrative's post-boundary history
	// (recap + raw tail) estimates above this, Persist compacts the old tail.
	TailSummarizeThresholdTokens int `json:"tail_summarize_threshold_tokens"`
	// RecentEventsKept: how many of the newest events stay raw (uncompacted)
	// after a compaction — a count, so it's well-defined regardless of event
	// density.
	RecentEventsKept int `json:"recent_events_kept"`
}

func (c CorrelatorConfig) Validate() error // both must be positive; threshold sanity vs LLM context window is a runtime concern, see below
```

Wired into `Config` as `Correlator CorrelatorConfig` (JSON `correlator`), with a populated example in
`config/unjira.example.json` (e.g. `{"tail_summarize_threshold_tokens": 6000, "recent_events_kept":
20}`), same day-one-setup precedent as the `llm` block. `Validate` is invoked wherever config is
loaded for the pipeline (a later slice's caller); defined now, exercised by unit tests now.

**Misconfiguration floor:** if `TailSummarizeThresholdTokens` (or `RecentEventsKept` × typical event
size) is set so large relative to `config.LLM.ContextWindowTokens` that even one narrative's bounded
context can't fit, `Cluster`'s irreducible-unit path (rework doc) errors loudly — the two configs
interact, and a bad pairing surfaces as a loud runtime error, not a silent overflow.

## Error handling

- Every failure path returns a wrapped loud error (`fmt.Errorf("...: %w", ...)`), per
  `docs/go-conventions.md` and design-notes #9.
- **All-or-nothing per `Persist` call** via one transaction: a hallucinated `narrative_id`, an
  unresolvable event id, or a failed compaction LLM call aborts and rolls back — no partial narrative
  state, no half-compacted summary, ever left for a later pass to trip on.
- Compaction is lossy by construction (a recap can omit a detail a future correction wanted); every
  compaction logs which events folded into what recap so it's auditable. Raw events survive in
  `narrative_events` regardless.
- Lease-lock steal-on-expiry logs a warning (not an error) — expected crash recovery.

## Testing strategy

testify; real `t.TempDir()`-backed `Store` for store/persist tests (matching every existing store
test); fake `llmClient` (the slice-2 fake, reused) for the compaction call.

- **`Persist` (real store + fake llm):**
  - new-narrative round-trip: correct window/title/summary/status, NULL issue_key/confidence/
    compaction_boundary, correct `narrative_events` links.
  - extend round-trip: window_end advances to new max (and asserted *not* moved backward for an older
    batch); summary overwritten; new events linked; old links intact.
  - extend-unknown-id → loud error, nothing persisted.
  - event-not-found → loud error.
  - empty results → `(nil, nil)`.
  - **compaction:** seed a narrative whose post-boundary history exceeds
    `TailSummarizeThresholdTokens`; an extend triggers exactly one compaction LLM call; assert
    `compaction_boundary` advanced, summary now leads with the recap, the newest `RecentEventsKept`
    events remain after the boundary, and **all `narrative_events` rows still present** (nothing
    deleted). A below-threshold extend triggers **no** compaction call.
  - all-or-nothing: a batch whose later result trips an error leaves an earlier result's narrative
    unwritten (transaction rollback), including no orphaned compaction.
- **Store accessors:** insert/get round-trip; `GetNarrative` miss → `ErrNarrativeNotFound`;
  `ExtendNarrative`/`SetCompactionBoundary` update only the intended row; `AddNarrativeEvents`
  `INSERT OR IGNORE` idempotency; `EventIDByExternalID` hit + `ErrEventNotFound` miss;
  `NarrativeEventsForContext` returns post-boundary events only (seed a compacted narrative, assert
  pre-boundary events excluded, post-boundary included, ordered).
- **Lease lock (real SQLite, driven clock):** unheld→true; concurrent second acquire pre-expiry→
  false; post-expiry→true + steal warning; `Acquire` blocks then succeeds on expiry; `ReleaseLock`
  by holder frees, by non-holder no-op; `Acquire` honors ctx cancel.
- **Config:** `CorrelatorConfig.Validate` table (zero/negative each field → error; positive → ok);
  `Load` parses a `correlator` block; the real `unjira.example.json` parses (existing on-disk-example
  smoke test extended, as in slice 1).

## Non-goals

- No CLI command. Pipeline wiring (collect → hydrate context → `Cluster` → `Persist`, building a real
  `openai.Client`, window selection) and command surface are slice 5's `watch`. Slice 3 (+ the
  rework) is package + store only, caller-free.
- No reconciler / `actions` writes (slice 4+).
- No issue-matching: `Persist` leaves `narratives.issue_key`/`confidence` NULL.

## Carried-forward notes (from slice-2 review, still open)

- `checkSameStory`'s boundary prompt sends titles/summaries, not full event lists — revisit if
  boundary-merge quality proves insufficient. (Note: after this rework the *main* cluster prompt does
  carry raw narrative events; the boundary same-story check is a separate, narrower call and is left
  as-is for now.)
- `mergeSplitResults` boundary-pair selection is by response-array order, not event chronology —
  faithful to the design text; a future hardening pass could select by timestamp.
