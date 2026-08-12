# Phase-1 slice 3: `correlator.Persist` + narrative persistence + lease lock

Status: approved (design brainstorm). Scope: slice 3 of
`docs/superpowers/specs/2026-08-11-phase1-correlator-design.md`'s "Implementation slices" —
persisting `Cluster`'s results to real `narratives`/`narrative_events` rows, and the leased
pipeline lock. No CLI command, no reconciler. This is the first slice that writes to the
`narratives`/`narrative_events` tables that have existed schema-only since phase 0.

## Revision to the phase-1 spec's slice-3 definition

The phase-1 spec's slice-3 bullet lists **tail-summarization** (`config.Correlator.
{TailSummarizeThresholdTokens, RecentEventsKept}`, compacting a narrative's accreted event history
into a recap) as part of this slice. This design **replaces that with a simpler summary-length
guard**, for a concrete reason grounded in what slice 2 actually built:

`Cluster` (slice 2, landed) feeds only each existing narrative's **title + summary** into its
prompt — never that narrative's raw constituent event list — and *regenerates* the summary on
every extend rather than appending to it. So a narrative's forward-context contribution is already
bounded by its summary length, and stays roughly constant-size across extends on its own. The
"linked-event history grows unbounded as `Cluster` reassembles it next run" premise that
tail-summarization was designed to solve does not occur through the context path slice 2
implemented. Building append-and-trim compaction machinery for an overflow that can't happen would
be speculative.

The replacement invariant: a single `config.Correlator.MaxSummaryTokens` budget, checked in
`Persist`; a summary over budget is a **loud error** naming the narrative, not a silent truncation
or a lossy re-compaction call. If real usage later shows summaries genuinely drifting larger over
many extends, summary-size *management* (as opposed to this guard) can be its own later slice,
informed by real data rather than speculation. The phase-1 spec's slice-3 bullet will be updated
to reflect this when slice 3 lands.

Consequence: `Persist` needs **no `llmClient`** — it is pure store I/O. (Contrast `Cluster`, which
owns all LLM interaction.)

## `Persist` — signature and contract

```go
// in internal/correlator/correlator.go (same package as Cluster)

// Persist writes Cluster's results to the narratives/narrative_events
// tables: ClusterNew inserts a fresh narrative row, ClusterExtends updates
// an existing one (extends window_end, overwrites summary, links new
// events). Returns the narratives touched this run (new + extended), not the
// whole table. Pure store I/O — no LLM calls (Cluster already synthesized
// each result's summary). All-or-nothing: any failure aborts the whole pass
// with a loud error and (via a transaction) persists nothing.
func Persist(s *store.Store, results []ClusterResult, maxSummaryTokens int) ([]Narrative, error)
```

Processing, per result, in slice order, inside a single SQL transaction (see Error handling):

1. **Summary-length guard, before any write for that result:** if
   `estimateTokens(result.Summary) > maxSummaryTokens`, return a loud error naming the offending
   narrative (title, and NarrativeID if `extends`). `estimateTokens` is the existing
   `(len+3)/4` helper from slice 2, reused.
2. **Resolve event IDs:** for each event in `result.Events`, look up its DB row id via
   `store.EventIDByExternalID(event.Source, event.ExternalID)`. A cluster event with no matching
   row is a loud error (should never happen — `Cluster` only ever operates on already-persisted
   events — but caught, never silently skipped).
3. **`ClusterNew`:** insert a `narratives` row with `window_start`/`window_end` = the min/max
   `OccurredAt` across `result.Events`, `title`/`summary` from the result, `status = 'open'`.
   `issue_key` and `confidence` are left NULL (issue-matching/confidence is reconciler work, a
   later slice — `Persist` never sets them). Then insert one `narrative_events` row per resolved
   event id.
4. **`ClusterExtends`:** load the narrative by `result.NarrativeID`. **If no such row exists,
   return a loud error naming the missing id** (this is the hallucinated/stale-`narrative_id`
   validation deferred from slice 2's review — a bad id would otherwise orphan a link or lose
   events, so it fails the pass rather than guessing or silently creating a new narrative).
   Otherwise: set `window_end` to the max of the existing `window_end` and this batch's latest
   event `OccurredAt` (never move it backward); overwrite `summary` with `result.Summary`; insert
   the new `narrative_events` rows (`INSERT OR IGNORE`, so re-linking an already-linked event is a
   harmless no-op, not an error).

Empty `results` returns `(nil, nil)` — a no-op, consistent with `Cluster` returning empty on no
events.

## Event-ID resolution

`ClusterResult.Events` are `events.Event` values carrying no DB row id (the `Event` struct dedupes
on `Source`+`ExternalID`; it has no `ID` field — confirmed unchanged since phase 0). `Persist`
resolves each event to its `events(id)` via a new store method against the existing
`UNIQUE(source, external_id)` index — cheap and unambiguous. This keeps slice 2's pure-compute
`Cluster`/`ClusterResult` untouched (no id field threaded through the compute path).

```go
// in internal/store/store.go

// EventIDByExternalID returns the row id of the event with the given
// (source, external_id) — the same pair InsertEvent dedupes on. Returns
// ErrEventNotFound if no such event exists; callers treat that as a loud
// error, since anything clustering an event implies it was persisted first.
func (s *Store) EventIDByExternalID(source, externalID string) (int64, error)
```

New sentinel `store.ErrEventNotFound` (`errors.Is`-checkable, mirroring `ErrLocalIssueNotFound`).

## Store accessors (new, in `internal/store`)

Following the existing parameterized-SQL, wrapped-error, no-business-logic house style. To avoid
`store` importing `correlator` (which would invert the dependency direction — `correlator` is the
consumer), the store exposes its own small row/params types and `Persist` maps between them and
`correlator.Narrative`.

```go
// NarrativeRow mirrors a narratives table row. correlator.Persist maps this
// to/from its own correlator.Narrative domain type.
type NarrativeRow struct {
	ID          int64
	WindowStart time.Time
	WindowEnd   time.Time
	Title       string
	Summary     string
	IssueKey    string  // "" when NULL
	Confidence  float64 // 0 when NULL
	Status      string
}

// ErrNarrativeNotFound is returned by GetNarrative when no row matches —
// "not found" is meaningful (a stale/hallucinated narrative_id), never
// swallowed. Mirrors ErrLocalIssueNotFound.
var ErrNarrativeNotFound = errors.New("narrative not found")

func (s *Store) InsertNarrative(windowStart, windowEnd time.Time, title, summary string) (int64, error)
func (s *Store) GetNarrative(id int64) (NarrativeRow, error) // ErrNarrativeNotFound if absent
func (s *Store) ExtendNarrative(id int64, windowEnd time.Time, summary string) error
func (s *Store) AddNarrativeEvents(narrativeID int64, eventIDs []int64) error // INSERT OR IGNORE
func (s *Store) EventIDByExternalID(source, externalID string) (int64, error)
```

Transaction wrapping: `Persist`'s all-or-nothing contract needs these to run inside one
transaction. Add a `store.WithTx(fn func(*Tx) error) error`-style helper (or expose `BeginTx` and
give the accessors `Tx`-based variants) so `Persist` can group its writes atomically — a mid-pass
failure (bad id, event-not-found, guard trip) rolls back everything. The exact shape (a `*Tx`
wrapper type exposing the same accessor methods vs. accessors taking a `dbtx` interface satisfied
by both `*sql.DB` and `*sql.Tx`) is an implementation-plan detail; the *contract* is: `Persist`
persists all results or none.

## Lease lock (new, in `internal/store`)

Per the phase-1 spec's "Lease lock" section. Built now, tested now, with no caller yet — it is the
seam slice 5's `watch`/`triage --refresh` hang off, the same "build the primitive before its first
caller" pattern used for `taskTracker()` in phase 0. Crash-recovery correctness (a killed process
leaving the row locked forever) shouldn't be deferred to when the caller lands.

```sql
CREATE TABLE IF NOT EXISTS pipeline_lock (
    id         INTEGER PRIMARY KEY CHECK (id = 1),  -- singleton row
    run_id     TEXT NOT NULL,
    held_since TEXT NOT NULL,
    expires_at TEXT NOT NULL
);
```

Added to `internal/store`'s existing `schema` string (same pattern as every other table).

```go
// TryAcquire attempts to take the pipeline lock non-blocking. It succeeds
// (returns true) if the lock is unheld or its lease has expired (expires_at
// < now); otherwise returns false immediately — no queuing, no retry, so a
// slow pass never stacks a backlog. Stealing an expired lease logs a warning
// naming the stale run_id and how long it was held (crash-recovery path).
// now is passed explicitly (not time.Now()) so steal-on-expiry is
// deterministically testable.
func (s *Store) TryAcquire(runID string, now time.Time, ttl time.Duration) (bool, error)

// Acquire is TryAcquire's blocking sibling (for triage --refresh): same
// steal-if-expired logic, but polls every poll interval until it can take
// the lock rather than giving up. Honors ctx cancellation.
func (s *Store) Acquire(ctx context.Context, runID string, now func() time.Time, ttl, poll time.Duration) error

// ReleaseLock releases the lock iff it is currently held by runID (a
// no-op if someone else already stole an expired lease, to avoid clobbering
// a newer holder).
func (s *Store) ReleaseLock(runID string) error
```

`Acquire` takes `now func() time.Time` (a clock) rather than a single timestamp, since it loops;
`TryAcquire` takes a single `now time.Time` since it's a one-shot check. Both keep `time.Now()`
out of the store internals so tests drive the clock.

## Config additions

```go
// in internal/config/config.go

// CorrelatorConfig configures narrative persistence limits. MaxSummaryTokens
// bounds a persisted narrative summary — a result whose summary exceeds it is
// a loud error in Persist, not a silent truncation (see the slice-3 design
// doc for why this replaces the originally-specced tail-summarization).
type CorrelatorConfig struct {
	MaxSummaryTokens int `json:"max_summary_tokens"`
}

func (c CorrelatorConfig) Validate() error // MaxSummaryTokens must be a positive int, mirroring LLMConfig.Validate
```

Wired into `Config` as a `Correlator CorrelatorConfig` field (JSON `correlator`), and a populated,
working value added to `config/unjira.example.json` (e.g. `"correlator": {"max_summary_tokens":
2000}`), so day-one setup doesn't require looking a number up cold — same precedent as the `llm`
block.

## Error handling

- Every failure path returns a wrapped loud error (`fmt.Errorf("...: %w", ...)`), consistent with
  `docs/go-conventions.md` and the design-notes #9 error-loudly invariant.
- **All-or-nothing per `Persist` call**, enforced by a single transaction: a summary-guard trip, a
  hallucinated `narrative_id`, or an unresolvable event id aborts the whole pass and rolls back —
  no partial narrative state is ever left behind for a later pass to trip over. This matches the
  phase-1 spec's stated intent that a partial/failed pass persists nothing.
- Lease-lock steal-on-expiry logs a warning (not an error) naming the stale `run_id` — it is the
  expected crash-recovery path, not a failure.

## Testing strategy

Per `docs/go-conventions.md` (testify, table-driven where shapes match, real `t.TempDir()`-backed
`Store` for store-level tests — matching every existing store test; no mocks for SQLite):

- **`Persist` (correlator package, real store):**
  - new-narrative round-trip: `ClusterNew` result → one `narratives` row with correct
    window_start/end (min/max event time), title, summary, `status='open'`, NULL issue_key/
    confidence; correct `narrative_events` links.
  - extend round-trip: seed a narrative, `ClusterExtends` result → window_end advanced to the new
    max (and asserted *not* moved backward when the batch is older), summary overwritten, new
    events linked, old links intact.
  - extend-unknown-id → loud error naming the id, nothing persisted.
  - over-budget summary → loud error naming the narrative, nothing persisted (assert the row was
    NOT written — proves the transaction rollback / pre-write guard).
  - event-not-found (a `ClusterResult` event whose (source, external_id) isn't in the store) →
    loud error.
  - empty results → `(nil, nil)`, no writes.
  - all-or-nothing: a batch whose second result trips the guard leaves the first result's
    narrative unwritten (transaction rollback).
- **Store accessors:** `InsertNarrative`/`GetNarrative` round-trip; `GetNarrative` missing →
  `ErrNarrativeNotFound`; `ExtendNarrative` updates only the intended row; `AddNarrativeEvents`
  `INSERT OR IGNORE` idempotency (re-linking is a no-op); `EventIDByExternalID` hit and
  `ErrEventNotFound` miss.
- **Lease lock (real SQLite, driven clock):** `TryAcquire` on unheld → true; second concurrent
  `TryAcquire` (different run_id, before expiry) → false; `TryAcquire` after `expires_at` →
  true + steal-warning; `Acquire` blocks then succeeds once the held lease expires; `ReleaseLock`
  by the holder frees it, `ReleaseLock` by a non-holder is a no-op; `Acquire` honors `ctx`
  cancellation.
- **Config:** `CorrelatorConfig.Validate` table (zero/negative → error, positive → ok);
  `Load` parses a `correlator` block; the real `unjira.example.json` parses with the populated
  value (the existing on-disk-example smoke test extended, as in slice 1's Task 3).

## Non-goals for this slice

Explicitly deferred, per the phase-1 spec's sequencing:

- No CLI command. The pipeline wiring (collect → `Cluster` → `Persist`, constructing a real
  `openai.Client` from config, selecting the event window) and its command surface are slice 5's
  `watch`. Slice 3 is package + store only, caller-free — same as slices 1-2.
- No reconciler, no `actions` writes (slice 4+).
- No issue-matching: `Persist` leaves `narratives.issue_key`/`confidence` NULL.
- Tail-summarization: replaced by the summary-length guard (see the revision section above);
  `config.Correlator.{TailSummarizeThresholdTokens, RecentEventsKept}` from the phase-1 spec are
  intentionally NOT implemented.

## Carried-forward notes (from slice-2 review, still open, not this slice's job)

- `checkSameStory`'s boundary prompt sends only titles/summaries, not full event lists
  (design-text vs. implementation discrepancy) — revisit if boundary-merge quality proves
  insufficient in real use.
- `mergeSplitResults` boundary-pair selection is by response-array order, not event chronology —
  faithful to the design text; a future hardening pass could select by timestamp.
