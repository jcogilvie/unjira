# `internal/reconciler` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn each touched narrative into a proposed `comment` / `transition` / `create` action, written to the `actions` table with `status=proposed`, verified against the live tracker and never applied.

**Architecture:** A new `internal/reconciler` package mirrors `internal/correlator`'s shape: pure-ish computation over store rows, one LLM call per narrative, per-narrative error isolation via `errors.Join`, and an all-or-nothing `Persist` inside a single `store.WithTx`. Two schema columns and a new `tasktracker` method are prerequisites. `internal/pipeline` gains `RunReconcile` plus a renderer; `dev narrate` wires it in behind `--dry-run`.

**Tech Stack:** Go 1.26, `modernc.org/sqlite`, testify, Kong CLI, `gopkg.in/yaml.v3` (unrelated), Earthly.

**Spec:** `docs/superpowers/specs/2026-08-25-reconciler-design.md`

---

## READ THIS FIRST — environment, and three spec corrections

### Environment: every Go command needs `env -u GOROOT`

This machine exports `GOROOT=~/.asdf/installs/golang/1.25.7/go` while the `go` shim resolves to
1.26.0. Every Go command fails with
`compile: version "go1.25.7" does not match go tool version "go1.26.0"` unless prefixed:

```sh
env -u GOROOT go test ./...
env -u GOROOT go build ./...
env -u GOROOT golangci-lint run ./...
```

Shell state does **not** persist between tool calls — include the prefix every time. Do not try to
"fix" the environment. Also note: a pre-built `golangci-lint` may itself be compiled against go 1.25
and fail on this module; if so, `env -u GOROOT go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest`
and use that build.

### Correction 1: the delta needs SUB-SECOND timestamps, and BOTH columns must match

The spec says to add `narrative_events.linked_at` and compute the delta as
`linked_at > (last action's created_at)`. As literally specified **that silently loses events.**
Measured against `modernc.org/sqlite` v1.56.0 in this repo:

- Existing tables use `strftime('%Y-%m-%dT%H:%M:%SZ', 'now')` — **whole seconds**. Two rows written
  in the same second get byte-identical timestamps.
- With second precision, an event linked in the same second as the last action yields
  `delta = 0 events` under `>` — invisible **forever**, since the action's `created_at` never moves.
  Under `>=` it is found, but every previously-covered event sharing that second re-proposes on
  every pass.
- With millisecond precision (`%f`), `>` yields the correct 1 event.

So `linked_at` uses `%f`. **And `actions.created_at` must be changed to `%f` too.** These columns are
`TEXT` and compared **lexically**, and mixing the formats inverts the comparison:

```
linked_at   = "2026-08-25T18:47:10.597Z"   (%f)
created_at  = "2026-08-25T18:47:10Z"       (%S)
linked_at > created_at  ->  0   (FALSE — wrong!)
```

`.` is 0x2E and `Z` is 0x5A, so `.597Z` sorts *before* `Z`. A genuinely-later event compares as
earlier. Both columns must use the identical format string. This is the same bug class as slice 3's
compaction-boundary tiebreaker (`NarrativeRow.CompactionBoundaryEventID`'s doc comment explains that
`occurred_at` alone cannot order events); the fix here is precision rather than a tiebreaker, because
`linked_at` is written by unjira rather than parsed from a source.

### Correction 2: transition legality needs a NEW `tasktracker` method

The spec says legality is checked "against the live issue's available transitions, not a mined
`workflow.Graph`," citing that `tasktracker.SetStatus` already works that way. True — but
`GetTransitions` exists **only on `*jira.Client`** (`internal/clients/jira/jira.go:282`). It is
**not** on the `tasktracker.TaskTracker` interface, and `internal/clients/local.Tracker` has no
equivalent at all. The reconciler talks to the interface, so it cannot reach it. Task 3 adds the
seam. This is a real addition to the slice's scope that the spec does not mention.

### Correction 3: `actions.feedback` and the delta query are additive to a table with zero Go code

The `actions` table exists (`internal/store/store.go:110-123`) but **nothing in Go touches it** — no
row type, no insert, no query. `feedback` is absent from the DDL. Tasks 1–2 build all of it.

### Greenfield: no migration machinery

unjira is pre-release. Schema changes edit the `CREATE TABLE` DDL in `internal/store/store.go`
directly. Do **not** add migration scaffolding. Deleting a local `data/unjira.db` is the accepted
upgrade path. (Changing `actions.created_at`'s default per Correction 1 is safe for exactly this
reason — nothing has ever written that table.)

---

## File Structure

| File | Responsibility |
| --- | --- |
| `internal/store/store.go` (modify) | `narrative_events.linked_at` + `actions.feedback` DDL; `ActionRow`; action accessors; `DeltaEvents` |
| `internal/store/store_test.go` (modify) | Tests for the above |
| `internal/tasktracker/tasktracker.go` (modify) | `AvailableStatusCategories` added to the interface |
| `internal/clients/jira/tracker.go` (modify) | Jira impl via `client.GetTransitions` |
| `internal/clients/local/local.go` (modify) | Local impl |
| `internal/config/config.go` (modify) | `ReconcilerConfig` + `Validate` |
| `internal/reconciler/reconciler.go` (create) | `Reconcile`, per-narrative orchestration, delta, verification |
| `internal/reconciler/draft.go` (create) | LLM prompt, response parsing, confidence flooring |
| `internal/reconciler/persist.go` (create) | All-or-nothing `Persist` |
| `internal/reconciler/types.go` (create) | `ProposedAction`, `ActionType`, `ReconcileResult` |
| `internal/reconciler/*_test.go` (create) | Offline tests + the recording `fakeTracker` |
| `internal/pipeline/reconcile.go` (create) | `RunReconcile` |
| `internal/pipeline/render.go` (modify) | Proposal renderer |
| `cmd/unjira/main.go` (modify) | `dev narrate` wiring |
| `internal/live/jira_test.go` (modify) | Live-tier transition-gating test |

Split rationale: `reconciler.go` / `draft.go` / `persist.go` separate the deterministic pass, the
model interaction, and the write — mirroring how `internal/correlator` splits `match.go` from
`match_types.go`, and keeping the LLM-facing code isolated so a test can exercise flooring without a
store.

---

## Task 1: `narrative_events.linked_at` + `DeltaEvents`

**Files:**
- Modify: `internal/store/store.go` (the `narrative_events` DDL; `addNarrativeEventsImpl`; the `actions` DDL's `created_at`)
- Test: `internal/store/store_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `internal/store/store_test.go`:

```go
// TestDeltaEventsIsEmptyWhenAnActionAlreadyCoversEveryEvent is the
// duplicate-suppression guarantee: a second reconcile pass over an unchanged
// narrative must find nothing to propose.
func TestDeltaEventsIsEmptyWhenAnActionAlreadyCoversEveryEvent(t *testing.T) {
	s := openStore(t)

	base := time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)
	nid, err := s.InsertNarrative(base, base.Add(time.Hour), "t", "s")
	require.NoError(t, err)

	eid, err := s.InsertEvent(makeEvent("e1"))
	require.NoError(t, err)
	require.NoError(t, s.AddNarrativeEvents(nid, []int64{eid}))

	// No action yet: the whole narrative is the delta.
	delta, err := s.DeltaEvents(nid)
	require.NoError(t, err)
	require.Len(t, delta, 1, "with no prior action every linked event is new")

	_, err = s.InsertAction(ActionRow{
		NarrativeID: nid,
		Type:        "comment",
		IssueKey:    "PROJ-1",
		Payload:     `{"body":"x"}`,
		Confidence:  0.9,
		Status:      "proposed",
	})
	require.NoError(t, err)

	delta, err = s.DeltaEvents(nid)
	require.NoError(t, err)
	assert.Empty(t, delta,
		"every event predates the action, so a re-run must proposely nothing")
}

// TestDeltaEventsFindsAnEventLinkedInTheSameSecondAsTheAction pins the
// precision decision. At whole-second granularity this event is invisible
// forever: the action's created_at never advances, so `linked_at > created_at`
// stays false on every future pass. Measured on modernc.org/sqlite v1.56.0.
func TestDeltaEventsFindsAnEventLinkedInTheSameSecondAsTheAction(t *testing.T) {
	s := openStore(t)

	base := time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)
	nid, err := s.InsertNarrative(base, base.Add(time.Hour), "t", "s")
	require.NoError(t, err)

	old, err := s.InsertEvent(makeEvent("old"))
	require.NoError(t, err)
	require.NoError(t, s.AddNarrativeEvents(nid, []int64{old}))

	_, err = s.InsertAction(ActionRow{
		NarrativeID: nid,
		Type:        "comment",
		IssueKey:    "PROJ-1",
		Payload:     `{"body":"x"}`,
		Status:      "proposed",
	})
	require.NoError(t, err)

	// Linked immediately after — same wall-clock second in practice.
	fresh, err := s.InsertEvent(makeEvent("fresh"))
	require.NoError(t, err)
	require.NoError(t, s.AddNarrativeEvents(nid, []int64{fresh}))

	delta, err := s.DeltaEvents(nid)
	require.NoError(t, err)

	require.Len(t, delta, 1,
		"an event linked in the same second as the action must still be in the delta; "+
			"at whole-second precision it would be lost permanently")
	assert.Equal(t, "fresh", delta[0].ExternalID)
}

// TestLinkedAtAndActionCreatedAtUseTheSameFormat guards the lexical-comparison
// trap: these are TEXT columns, so mixing %S and %f inverts the ordering
// ("...10.597Z" < "...10Z" because '.' is 0x2E and 'Z' is 0x5A). A later event
// would compare as earlier and silently drop out of the delta.
func TestLinkedAtAndActionCreatedAtUseTheSameFormat(t *testing.T) {
	s := openStore(t)

	base := time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)
	nid, err := s.InsertNarrative(base, base.Add(time.Hour), "t", "s")
	require.NoError(t, err)
	eid, err := s.InsertEvent(makeEvent("e1"))
	require.NoError(t, err)
	require.NoError(t, s.AddNarrativeEvents(nid, []int64{eid}))
	_, err = s.InsertAction(ActionRow{
		NarrativeID: nid, Type: "comment", IssueKey: "PROJ-1",
		Payload: `{"body":"x"}`, Status: "proposed",
	})
	require.NoError(t, err)

	var linkedAt, createdAt string
	require.NoError(t, s.db.QueryRow(
		`SELECT linked_at FROM narrative_events WHERE narrative_id = ?`, nid).Scan(&linkedAt))
	require.NoError(t, s.db.QueryRow(
		`SELECT created_at FROM actions WHERE narrative_id = ?`, nid).Scan(&createdAt))

	assert.Contains(t, linkedAt, ".", "linked_at must carry sub-second precision")
	assert.Contains(t, createdAt, ".",
		"actions.created_at must use the SAME sub-second format as linked_at, or the "+
			"lexical TEXT comparison in DeltaEvents inverts")
	assert.Equal(t, len(linkedAt), len(createdAt),
		"identical format strings produce identical lengths")
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `env -u GOROOT go test ./internal/store/ -run 'TestDeltaEvents|TestLinkedAt' -v`

Expected: FAIL to compile — `s.DeltaEvents`, `s.InsertAction`, and `store.ActionRow` are all
undefined. (Task 2 defines `ActionRow`/`InsertAction`; implement enough of them here to compile, or
do Tasks 1 and 2 as one commit. If you split, note that these three tests span both.)

- [ ] **Step 3: Add the column and fix both timestamp formats**

In `internal/store/store.go`, change the `narrative_events` DDL to:

```sql
CREATE TABLE IF NOT EXISTS narrative_events (
    narrative_id INTEGER NOT NULL REFERENCES narratives (id),
    event_id     INTEGER NOT NULL REFERENCES events (id),
    -- Sub-second (%f, milliseconds), not %S. The reconciler's delta is
    -- `linked_at > (last action's created_at)`; at whole-second granularity an
    -- event linked in the same second as the action is invisible forever,
    -- because that action's created_at never advances. actions.created_at uses
    -- this identical format on purpose: both are TEXT and compared lexically,
    -- and mixing %f with %S inverts the comparison ('.' 0x2E sorts before
    -- 'Z' 0x5A), so a later event would read as earlier.
    linked_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (narrative_id, event_id)
);
```

In the `actions` DDL, change `created_at` to the matching format:

```sql
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
```

Leave `addNarrativeEventsImpl`'s statement as-is — it omits `linked_at`, so the DEFAULT applies:

```go
`INSERT OR IGNORE INTO narrative_events (narrative_id, event_id) VALUES (?, ?)`
```

`INSERT OR IGNORE` is correct here and must stay: re-linking an already-linked event must **not**
refresh `linked_at`, or an event would re-enter the delta on every pass.

- [ ] **Step 4: Add `DeltaEvents`**

Add to `internal/store/store.go`, near `AllNarrativeEvents`:

```go
// DeltaEvents returns the events linked to narrativeID since the most recent
// action proposed for it — "what's new since a reviewer last saw this."
//
// Bounded by the last action's created_at (not decided_at or executed_at)
// deliberately: created_at is always set, so a proposal sitting unreviewed in
// the queue still suppresses re-proposing its delta. decided_at is NULL while
// unreviewed, which would make every pass re-propose the same thing; and a
// rejected action never gets an executed_at, so bounding on that would
// re-propose a rejected action identically forever, giving the reviewer's "no"
// no weight. See the design spec's comparison table.
//
// With no prior action every linked event is returned (COALESCE to ""; all
// real timestamps sort above the empty string), which is the first-pass case:
// the whole narrative is the delta.
func (s *Store) DeltaEvents(narrativeID int64) ([]events.Event, error) {
	rows, err := s.db.Query(
		`SELECT e.source, e.external_id, e.occurred_at, e.actor, e.summary, e.artifacts, e.raw_ref
		 FROM narrative_events ne
		 JOIN events e ON e.id = ne.event_id
		 WHERE ne.narrative_id = ?
		   AND ne.linked_at > COALESCE(
		       (SELECT MAX(created_at) FROM actions WHERE narrative_id = ?), '')
		 ORDER BY e.occurred_at, e.id`,
		narrativeID, narrativeID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying delta events for narrative %d: %w", narrativeID, err)
	}
	defer rows.Close()

	return scanEvents(rows)
}
```

Reuse whatever row-scanning helper `AllNarrativeEvents` already uses rather than writing a second
scanner; read that function first and match it exactly (including how `artifacts` JSON is
unmarshalled). If it scans inline instead of via a helper, extract the shared helper as part of this
step so the two cannot drift.

- [ ] **Step 5: Run tests**

Run: `env -u GOROOT go test ./internal/store/ -v 2>&1 | tail -30`

Expected: the three new tests PASS, and **every pre-existing store test still passes**. If a
pre-existing test fails, that is a regression in this change — fix the change, do not weaken the
test.

- [ ] **Step 6: Commit**

```bash
git add internal/store/store.go internal/store/store_test.go
git commit -m "store: add narrative_events.linked_at with sub-second precision

The reconciler's delta is 'events linked since the last action'. That was
not computable: narrative_events had no link timestamp, and events.occurred_at
is not a substitute (a backfill links an event long after it occurred).

linked_at uses %f rather than the %S used elsewhere, and actions.created_at is
changed to match. Measured: at whole-second granularity an event linked in the
same second as the action yields a zero-length delta under '>' and is invisible
forever, since that action's created_at never advances. And because both are
TEXT compared lexically, mixing %f with %S inverts the comparison ('.' 0x2E
sorts before 'Z' 0x5A), making a later event read as earlier."
```

---

## Task 2: `actions.feedback`, `store.ActionRow`, and the action accessors

**Files:**
- Modify: `internal/store/store.go`
- Test: `internal/store/store_test.go`

- [ ] **Step 1: Write the failing tests**

```go
func TestInsertActionRoundTripsEveryField(t *testing.T) {
	s := openStore(t)

	base := time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)
	nid, err := s.InsertNarrative(base, base.Add(time.Hour), "t", "s")
	require.NoError(t, err)

	id, err := s.InsertAction(ActionRow{
		NarrativeID: nid,
		Type:        "comment",
		IssueKey:    "PROJ-1",
		Payload:     `{"body":"drafted text"}`,
		Confidence:  0.75,
		Rationale:   "delta shows the work landed",
		Status:      "proposed",
	})
	require.NoError(t, err)
	require.NotZero(t, id)

	got, err := s.ActionsForNarrative(nid)
	require.NoError(t, err)
	require.Len(t, got, 1)

	assert.Equal(t, id, got[0].ID)
	assert.Equal(t, nid, got[0].NarrativeID)
	assert.Equal(t, "comment", got[0].Type)
	assert.Equal(t, "PROJ-1", got[0].IssueKey)
	assert.JSONEq(t, `{"body":"drafted text"}`, got[0].Payload)
	assert.InDelta(t, 0.75, got[0].Confidence, 0.0001)
	assert.Equal(t, "delta shows the work landed", got[0].Rationale)
	assert.Equal(t, "proposed", got[0].Status)
	assert.NotEmpty(t, got[0].CreatedAt, "created_at must come back populated")
	assert.Empty(t, got[0].Feedback, "feedback is unwritten until slice 6")
}

func TestActionsByStatusFiltersAndLatestActionPicksMostRecent(t *testing.T) {
	s := openStore(t)

	base := time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)
	nid, err := s.InsertNarrative(base, base.Add(time.Hour), "t", "s")
	require.NoError(t, err)

	first, err := s.InsertAction(ActionRow{
		NarrativeID: nid, Type: "comment", IssueKey: "PROJ-1",
		Payload: `{"body":"first"}`, Status: "proposed",
	})
	require.NoError(t, err)
	second, err := s.InsertAction(ActionRow{
		NarrativeID: nid, Type: "transition", IssueKey: "PROJ-1",
		Payload: `{"target":"done"}`, Status: "approved",
	})
	require.NoError(t, err)

	proposed, err := s.ActionsByStatus("proposed")
	require.NoError(t, err)
	require.Len(t, proposed, 1)
	assert.Equal(t, first, proposed[0].ID)

	latest, found, err := s.LatestActionForNarrative(nid)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, second, latest.ID,
		"ties on created_at break by id, so the later insert wins")

	_, found, err = s.LatestActionForNarrative(nid + 999)
	require.NoError(t, err)
	assert.False(t, found, "a narrative with no actions reports found=false, not an error")
}

func TestUpdateActionStatusSetsDecidedAndExecutedTimestamps(t *testing.T) {
	s := openStore(t)

	base := time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)
	nid, err := s.InsertNarrative(base, base.Add(time.Hour), "t", "s")
	require.NoError(t, err)
	id, err := s.InsertAction(ActionRow{
		NarrativeID: nid, Type: "comment", IssueKey: "PROJ-1",
		Payload: `{"body":"x"}`, Status: "proposed",
	})
	require.NoError(t, err)

	require.NoError(t, s.UpdateActionStatus(id, "approved"))
	got, err := s.ActionsForNarrative(nid)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "approved", got[0].Status)
	require.NotNil(t, got[0].DecidedAt, "a human ruling sets decided_at")
	assert.Nil(t, got[0].ExecutedAt, "approval is not execution")

	require.NoError(t, s.UpdateActionStatus(id, "applied"))
	got, err = s.ActionsForNarrative(nid)
	require.NoError(t, err)
	assert.Equal(t, "applied", got[0].Status)
	require.NotNil(t, got[0].ExecutedAt, "applying sets executed_at")
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `env -u GOROOT go test ./internal/store/ -run 'TestInsertAction|TestActionsByStatus|TestUpdateActionStatus' -v`

Expected: compile failure — none of `ActionRow`, `InsertAction`, `ActionsForNarrative`,
`ActionsByStatus`, `LatestActionForNarrative`, `UpdateActionStatus` exist.

- [ ] **Step 3: Add `feedback` to the DDL**

In the `actions` `CREATE TABLE`, add before `created_at`:

```sql
    -- Written by slice 6's triage/rework loop, nothing today. Added now so
    -- that slice does not need a second schema change (see the phase-1 spec's
    -- schema-additions section, where it was specified but never landed).
    feedback     TEXT,
```

- [ ] **Step 4: Add `ActionRow` and the five accessors**

```go
// ActionRow is one row of the actions table — a proposed, decided, or
// executed change to a tracker issue.
//
// IssueKey is empty for a `create` action (there is no key until it is
// applied). DecidedAt/ExecutedAt are *string rather than string because NULL
// is meaningful: NULL decided_at means "no human has ruled yet", which is
// distinct from any timestamp. They stay strings rather than time.Time to
// match how the rest of this package stores timestamps (SQLite TEXT), and
// because nothing orders by them.
type ActionRow struct {
	ID          int64
	NarrativeID int64
	Type        string // comment | transition | create | estimate
	IssueKey    string
	Payload     string // JSON; shape depends on Type
	Confidence  float64
	Rationale   string
	Status      string // proposed | approved | edited | rejected | applied | failed
	Feedback    string
	DecidedAt   *string
	ExecutedAt  *string
	CreatedAt   string
}

// InsertAction writes one proposed action and returns its id. created_at
// comes from the column DEFAULT so it shares narrative_events.linked_at's
// format exactly — see DeltaEvents.
func (s *Store) InsertAction(a ActionRow) (int64, error) {
	return insertActionImpl(s.db, a)
}

// InsertAction is the Tx-scoped form, so reconciler.Persist can write a whole
// pass atomically.
func (t *Tx) InsertAction(a ActionRow) (int64, error) {
	return insertActionImpl(t.tx, a)
}

func insertActionImpl(c dbConn, a ActionRow) (int64, error) {
	res, err := c.Exec(
		`INSERT INTO actions
		   (narrative_id, type, issue_key, payload, confidence, rationale, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		a.NarrativeID, a.Type, nullable(a.IssueKey), a.Payload,
		a.Confidence, nullable(a.Rationale), a.Status,
	)
	if err != nil {
		return 0, fmt.Errorf("inserting %s action for narrative %d: %w", a.Type, a.NarrativeID, err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("reading inserted action id: %w", err)
	}

	return id, nil
}

// ActionsForNarrative returns every action ever proposed for a narrative,
// oldest first.
func (s *Store) ActionsForNarrative(narrativeID int64) ([]ActionRow, error) {
	rows, err := s.db.Query(actionSelect+` WHERE narrative_id = ? ORDER BY created_at, id`, narrativeID)
	if err != nil {
		return nil, fmt.Errorf("querying actions for narrative %d: %w", narrativeID, err)
	}
	defer rows.Close()

	return scanActions(rows)
}

// ActionsByStatus returns every action in one workflow state, oldest first —
// the review queue's read path (slice 6's `triage`).
func (s *Store) ActionsByStatus(status string) ([]ActionRow, error) {
	rows, err := s.db.Query(actionSelect+` WHERE status = ? ORDER BY created_at, id`, status)
	if err != nil {
		return nil, fmt.Errorf("querying actions with status %q: %w", status, err)
	}
	defer rows.Close()

	return scanActions(rows)
}

// LatestActionForNarrative returns the most recent action for a narrative.
//
// "Most recent" is by (created_at, id): created_at alone cannot order two
// actions written in the same millisecond, and id is monotonic. found is
// false with a nil error when the narrative has no actions at all — the
// first-pass case, not an error.
func (s *Store) LatestActionForNarrative(narrativeID int64) (ActionRow, bool, error) {
	rows, err := s.db.Query(
		actionSelect+` WHERE narrative_id = ? ORDER BY created_at DESC, id DESC LIMIT 1`,
		narrativeID,
	)
	if err != nil {
		return ActionRow{}, false, fmt.Errorf("querying latest action for narrative %d: %w", narrativeID, err)
	}
	defer rows.Close()

	found, err := scanActions(rows)
	if err != nil {
		return ActionRow{}, false, err
	}
	if len(found) == 0 {
		return ActionRow{}, false, nil
	}

	return found[0], true, nil
}

// UpdateActionStatus moves an action to a new workflow state, stamping
// decided_at and/or executed_at as that state implies.
//
// Both stamps are set with the same strftime format the columns default to, so
// every timestamp in this table remains directly comparable. Terminal-state
// semantics: any human ruling sets decided_at; only a write that actually
// reached the tracker sets executed_at.
func (s *Store) UpdateActionStatus(id int64, status string) error {
	const ts = `strftime('%Y-%m-%dT%H:%M:%fZ', 'now')`

	query := `UPDATE actions SET status = ?`
	switch status {
	case "approved", "edited", "rejected":
		query += `, decided_at = COALESCE(decided_at, ` + ts + `)`
	case "applied", "failed":
		// An applied action was necessarily decided, but decided_at may
		// already be set from an earlier approval — COALESCE preserves the
		// original ruling time rather than overwriting it.
		query += `, decided_at = COALESCE(decided_at, ` + ts + `), executed_at = ` + ts
	}
	query += ` WHERE id = ?`

	res, err := s.db.Exec(query, status, id)
	if err != nil {
		return fmt.Errorf("updating action %d to status %q: %w", id, status, err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking rows affected updating action %d: %w", id, err)
	}
	if affected == 0 {
		return fmt.Errorf("updating action %d to status %q: no such action", id, status)
	}

	return nil
}
```

Plus the shared select-list and scanner:

```go
// actionSelect is the column list every ActionRow query shares, so a new
// column cannot be added to one query and forgotten in another.
const actionSelect = `SELECT id, narrative_id, type, issue_key, payload, confidence,
	rationale, status, feedback, decided_at, executed_at, created_at FROM actions`

func scanActions(rows *sql.Rows) ([]ActionRow, error) {
	var out []ActionRow

	for rows.Next() {
		var (
			a                            ActionRow
			issueKey, rationale, feedback sql.NullString
			confidence                   sql.NullFloat64
			decidedAt, executedAt        sql.NullString
		)

		if err := rows.Scan(
			&a.ID, &a.NarrativeID, &a.Type, &issueKey, &a.Payload, &confidence,
			&rationale, &a.Status, &feedback, &decidedAt, &executedAt, &a.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scanning action row: %w", err)
		}

		a.IssueKey = issueKey.String
		a.Rationale = rationale.String
		a.Feedback = feedback.String
		a.Confidence = confidence.Float64
		if decidedAt.Valid {
			a.DecidedAt = &decidedAt.String
		}
		if executedAt.Valid {
			a.ExecutedAt = &executedAt.String
		}

		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating action rows: %w", err)
	}

	return out, nil
}
```

Check before writing: confirm `dbConn` and `nullable` exist with these names (`nullable` is around
`internal/store/store.go:1251` and returns nil for `""`). If `dbConn` is named differently, match the
existing convention used by `addNarrativeEventsImpl`.

- [ ] **Step 5: Run tests**

Run: `env -u GOROOT go test ./internal/store/ -v 2>&1 | tail -40`

Expected: all action tests PASS, all three Task-1 tests now PASS, no pre-existing test regresses.

- [ ] **Step 6: Commit**

```bash
git add internal/store/store.go internal/store/store_test.go
git commit -m "store: add actions.feedback, ActionRow, and action accessors

The actions table existed since the port with zero Go code touching it: no row
type, no insert, no query. This adds the five accessors the reconciler needs
plus a Tx-scoped InsertAction for its all-or-nothing Persist.

feedback is added now though nothing writes it until slice 6's rework loop, so
that slice needs no second schema change."
```

---

## Task 3: `tasktracker` gains `AvailableStatusCategories`

The spec requires transition legality checked against the live issue. `GetTransitions` is only on
`*jira.Client`; the interface has no such method and the local backend has no equivalent. This adds
the seam.

**Design note — why normalized categories, not Jira transition IDs.** `tasktracker` is deliberately
backend-agnostic: `SetStatus` takes a normalized `StatusCategory` precisely because "GitHub Issues
only has open/closed" (its own doc comment). Returning Jira transition IDs would leak Jira's
named-transition model into an interface that exists to hide it, and the reconciler only ever needs
to answer "can this issue reach Done?" — the same question `SetStatus` answers at execution time.

**Files:**
- Modify: `internal/tasktracker/tasktracker.go`
- Modify: `internal/clients/jira/tracker.go`
- Modify: `internal/clients/local/local.go`
- Test: `internal/clients/jira/tracker_test.go`, `internal/clients/local/local_test.go`

- [ ] **Step 1: Write the failing test (Jira)**

Add to `internal/clients/jira/tracker_test.go`, matching that file's existing fake-transport setup
(read it first and reuse its helper rather than inventing a second one):

```go
func TestAvailableStatusCategoriesNormalizesLiveTransitions(t *testing.T) {
	// Two legal transitions: one landing in an in-progress status, one in done.
	// "new" is deliberately absent — the point of this method is that the
	// reconciler learns Todo is NOT reachable from here.
	tracker, cleanup := trackerWithTransitions(t, []map[string]any{
		{"id": "11", "to": map[string]any{
			"name": "In Progress",
			"statusCategory": map[string]any{"key": "indeterminate"},
		}},
		{"id": "31", "to": map[string]any{
			"name": "Done",
			"statusCategory": map[string]any{"key": "done"},
		}},
	})
	defer cleanup()

	got, err := tracker.AvailableStatusCategories("PROJ-1")
	require.NoError(t, err)

	assert.ElementsMatch(t,
		[]tasktracker.StatusCategory{tasktracker.StatusInProgress, tasktracker.StatusDone},
		got)
	assert.NotContains(t, got, tasktracker.StatusTodo,
		"no transition lands in a 'new' status, so Todo must not be reported reachable")
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `env -u GOROOT go test ./internal/clients/jira/ -run TestAvailableStatusCategories -v`

Expected: FAIL — `tracker.AvailableStatusCategories` undefined.

- [ ] **Step 3: Add the interface method**

In `internal/tasktracker/tasktracker.go`, add to the `TaskTracker` interface:

```go
	// AvailableStatusCategories reports which normalized status categories the
	// issue can legally move to right now, per the live backend.
	//
	// Normalized categories rather than backend-native transition identifiers,
	// for the same reason SetStatus takes a category: Jira's named,
	// admin-configurable transitions have no counterpart in GitHub Issues'
	// open/closed model, and callers only need to know whether a target is
	// reachable.
	//
	// This must be a live per-issue read, not a lookup against a mined
	// workflow.Graph. The graph is statistically observed changelog history, so
	// HasEdge answers "has this ever been seen" — a proxy for legality, not
	// ground truth (see internal/workflow's package doc on its three tiers).
	// Proposing a transition the backend will refuse is the failure this
	// prevents.
	AvailableStatusCategories(key string) ([]StatusCategory, error)
```

- [ ] **Step 4: Implement for Jira**

In `internal/clients/jira/tracker.go`:

```go
// AvailableStatusCategories maps the issue's currently-legal transitions to
// normalized categories, deduplicated (several named transitions routinely
// land in the same category).
func (t *Tracker) AvailableStatusCategories(key string) ([]tasktracker.StatusCategory, error) {
	transitions, err := t.client.GetTransitions(key)
	if err != nil {
		return nil, fmt.Errorf("fetching transitions for jira issue %s: %w", key, err)
	}

	seen := make(map[tasktracker.StatusCategory]bool, len(transitions))
	var out []tasktracker.StatusCategory

	for _, transition := range transitions {
		to, ok := transition["to"].(map[string]any)
		if !ok {
			continue
		}
		category, ok := to["statusCategory"].(map[string]any)
		if !ok {
			continue
		}
		categoryKey, _ := category["key"].(string)

		normalized := normalizedStatusCategory(categoryKey)
		if seen[normalized] {
			continue
		}
		seen[normalized] = true
		out = append(out, normalized)
	}

	return out, nil
}
```

Note this deliberately reuses `normalizedStatusCategory` and mirrors `SetStatus`'s traversal, so the
two cannot disagree about what a transition's category is.

- [ ] **Step 5: Implement for the local backend**

In `internal/clients/local/local.go`:

```go
// AvailableStatusCategories reports every category as reachable.
//
// The local backend is a test/offline double with no workflow restrictions —
// SetStatus accepts any target — so reporting everything reachable is the
// honest answer, not a stub. A narrower answer would make offline tests
// disagree with the backend's actual behaviour.
func (t *Tracker) AvailableStatusCategories(_ string) ([]tasktracker.StatusCategory, error) {
	return []tasktracker.StatusCategory{
		tasktracker.StatusTodo,
		tasktracker.StatusInProgress,
		tasktracker.StatusDone,
	}, nil
}
```

Add a matching test in `internal/clients/local/local_test.go`:

```go
func TestLocalAvailableStatusCategoriesReportsEveryCategory(t *testing.T) {
	tracker := newTestTracker(t) // match this file's existing constructor

	got, err := tracker.AvailableStatusCategories("anything")
	require.NoError(t, err)

	assert.ElementsMatch(t, []tasktracker.StatusCategory{
		tasktracker.StatusTodo,
		tasktracker.StatusInProgress,
		tasktracker.StatusDone,
	}, got, "the local backend imposes no workflow, and SetStatus accepts any target")
}
```

- [ ] **Step 6: Fix every other implementer**

Adding a method to the interface breaks all implementers. Find them:

```sh
cd /Users/jonathan.ogilvie/workspace/unjira/.claude/worktrees/slice4-reconciler
env -u GOROOT go build ./... 2>&1 | head -20
grep -rn "tasktracker.TaskTracker = " --include="*.go" .
```

At minimum this includes the `fakeTracker` in `internal/correlator/match_test.go`. Give it the
narrowest thing that keeps matching's tests honest — matching never proposes transitions:

```go
func (f *fakeTracker) AvailableStatusCategories(string) ([]tasktracker.StatusCategory, error) {
	return nil, nil
}
```

- [ ] **Step 7: Run the full suite**

Run: `env -u GOROOT go test ./... 2>&1 | grep -Ev '^ok|no test files'`

Expected: no output (all green).

- [ ] **Step 8: Commit**

```bash
git add internal/tasktracker/ internal/clients/jira/ internal/clients/local/ internal/correlator/
git commit -m "tasktracker: add AvailableStatusCategories for live transition legality

The reconciler must not propose a transition the backend will refuse, and the
spec requires checking the live issue rather than a mined workflow.Graph (which
is observed history, so HasEdge is a proxy for legality, not ground truth).

GetTransitions existed only on *jira.Client — not on the TaskTracker interface
the reconciler talks to, and with no local-backend equivalent at all. This adds
the seam, returning normalized categories rather than Jira transition IDs for
the same reason SetStatus takes a category: Jira's named-transition model has
no counterpart in GitHub Issues' open/closed world."
```

---

## Task 4: `config.ReconcilerConfig`

**Files:**
- Modify: `internal/config/config.go`
- Modify: `config/unjira.example.json`
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing tests**

```go
func TestReconcilerConfigValidateRejectsUnusableValues(t *testing.T) {
	tests := map[string]struct {
		cfg     ReconcilerConfig
		wantErr string
	}{
		"negative max narratives": {
			cfg:     ReconcilerConfig{MaxNarrativesPerPass: -1, MinConfidenceToPropose: 0.5},
			wantErr: "max_narratives_per_pass",
		},
		"confidence above one": {
			cfg:     ReconcilerConfig{MaxNarrativesPerPass: 10, MinConfidenceToPropose: 1.5},
			wantErr: "min_confidence_to_propose",
		},
		"confidence below zero": {
			cfg:     ReconcilerConfig{MaxNarrativesPerPass: 10, MinConfidenceToPropose: -0.1},
			wantErr: "min_confidence_to_propose",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			err := tc.cfg.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestReconcilerConfigZeroMaxNarrativesDefaults(t *testing.T) {
	cfg := ReconcilerConfig{MinConfidenceToPropose: 0.5}

	require.NoError(t, cfg.Validate(), "zero means 'use the default', not 'invalid'")
	assert.Equal(t, DefaultMaxNarrativesPerPass, cfg.NarrativeLimit())
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `env -u GOROOT go test ./internal/config/ -run TestReconcilerConfig -v`

Expected: compile failure — `ReconcilerConfig` undefined.

- [ ] **Step 3: Implement**

Add to `internal/config/config.go`, following `MatchConfig`'s shape exactly:

```go
// DefaultMaxNarrativesPerPass bounds how many narratives one reconcile pass
// examines. A limit is required rather than optional: each narrative costs at
// least one GetIssue per link plus one LLM drafting call, so an unbounded pass
// is unbounded spend.
const DefaultMaxNarrativesPerPass = 20

// ReconcilerConfig tunes proposal drafting. See
// docs/superpowers/specs/2026-08-25-reconciler-design.md.
type ReconcilerConfig struct {
	// MaxNarrativesPerPass caps narratives examined per pass. Zero means
	// DefaultMaxNarrativesPerPass. Reaching the cap is logged, never silent —
	// a silent cap presents as a clean pass that quietly ignored work.
	MaxNarrativesPerPass int `json:"max_narratives_per_pass"`
	// MinConfidenceToPropose governs what unjira *asserts*, not what it
	// *records*: below it the action is still written, carrying its low score.
	// Slice 6's triage must be able to see weak proposals in order to judge
	// them, and a dropped proposal is indistinguishable from "nothing to do."
	// Same reasoning as MatchConfig.ConfidenceFloor.
	MinConfidenceToPropose float64 `json:"min_confidence_to_propose"`
}

// NarrativeLimit returns the effective per-pass narrative cap.
func (c ReconcilerConfig) NarrativeLimit() int {
	if c.MaxNarrativesPerPass == 0 {
		return DefaultMaxNarrativesPerPass
	}

	return c.MaxNarrativesPerPass
}

// Validate rejects configurations that would fail silently at runtime. A
// MinConfidenceToPropose above 1 is the important case: confidence is a 0..1
// score, so every proposal would land below the threshold and the reconciler
// would look broken rather than misconfigured.
func (c ReconcilerConfig) Validate() error {
	if c.MaxNarrativesPerPass < 0 {
		return fmt.Errorf(
			"reconciler.max_narratives_per_pass is %d: must be positive, or omitted for the default of %d",
			c.MaxNarrativesPerPass, DefaultMaxNarrativesPerPass,
		)
	}

	if c.MinConfidenceToPropose < 0 || c.MinConfidenceToPropose > 1 {
		return fmt.Errorf(
			"reconciler.min_confidence_to_propose is %v: must be within [0, 1] — confidence is a "+
				"0..1 score, so a threshold above 1 would suppress every proposal",
			c.MinConfidenceToPropose,
		)
	}

	return nil
}
```

Add the field to `Config`:

```go
	Reconciler ReconcilerConfig `json:"reconciler"`
```

And to `config/unjira.example.json`, alongside the existing `match` block:

```json
  "reconciler": {
    "max_narratives_per_pass": 20,
    "min_confidence_to_propose": 0.5
  },
```

- [ ] **Step 4: Run tests**

Run: `env -u GOROOT go test ./internal/config/ -v 2>&1 | tail -20`

Expected: PASS. Also confirm the example config still parses — if `internal/config` has a test that
loads `config/unjira.example.json`, it must stay green.

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go config/unjira.example.json
git commit -m "config: add ReconcilerConfig

MinConfidenceToPropose gates what unjira asserts, not what it records: a
below-threshold action is still written with its low score, because slice 6's
triage needs to see weak proposals to judge them and a dropped proposal is
indistinguishable from 'nothing to do'."
```

---

## Task 5: `internal/reconciler` — types, delta, and verification

This task deliberately stops short of the LLM call: it establishes the deterministic pass and the
recording `fakeTracker`, so Task 6 can test drafting in isolation.

**Files:**
- Create: `internal/reconciler/types.go`
- Create: `internal/reconciler/reconciler.go`
- Create: `internal/reconciler/reconciler_test.go`

- [ ] **Step 1: Write `types.go`**

```go
// Package reconciler turns each touched narrative into a proposed action — a
// comment, a transition, or a new issue — written to the actions table with
// status=proposed.
//
// This package proposes and never applies. It holds no authorization to write
// to a tracker: per CLAUDE.md's architecture invariants, "writes are gated by
// the pipeline, not the client." Its only tracker calls are reads (GetIssue,
// AvailableStatusCategories) used to verify current state before drafting
// anything, per rules/intent-not-outcome.md.
//
// See docs/superpowers/specs/2026-08-25-reconciler-design.md.
package reconciler

import (
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// ActionType is the closed set of actions this slice can propose.
//
// estimate is deliberately absent: tasktracker has no method for it, so it is
// phase 2+ (see the spec's "What this slice does NOT do").
type ActionType string

const (
	// ActionComment posts narrative prose to an existing issue.
	ActionComment ActionType = "comment"
	// ActionTransition moves an existing issue to a new status category.
	ActionTransition ActionType = "transition"
	// ActionCreate opens a new issue. Proposed only when a narrative has no
	// verified link at all — never alongside one, or unjira would manufacture
	// duplicate tickets for work already tracked.
	ActionCreate ActionType = "create"
)

// ProposedAction is one drafted, not-yet-persisted action.
type ProposedAction struct {
	Type ActionType
	// IssueKey is empty for ActionCreate.
	IssueKey string
	// Body is the comment text for ActionComment and the description for
	// ActionCreate; empty for ActionTransition.
	Body string
	// Summary is the issue title, set only for ActionCreate.
	Summary string
	// TargetStatus is set only for ActionTransition.
	TargetStatus tasktracker.StatusCategory
	// Confidence is the model's self-reported score AFTER deterministic
	// flooring (see floorConfidence). Never the raw model number.
	Confidence float64
	Rationale  string
}

// verifiedLink pairs a stored narrative→issue link with the live issue read
// while verifying it, so drafting never needs a second GetIssue for the same
// key. AvailableStatus is the live transition legality for this issue,
// populated only when a transition is plausible.
type verifiedLink struct {
	Link           store.NarrativeIssue
	Issue          tasktracker.Issue
	AvailableStatus []tasktracker.StatusCategory
}

// ReconcileResult is one narrative's outcome, for rendering and tests.
type ReconcileResult struct {
	NarrativeID int64
	Proposed    []ProposedAction
	// Unverified holds link keys the tracker reported as not-found. They are
	// recorded and not acted on (rules/verify-correlations.md) rather than
	// failing the narrative.
	Unverified []string
	// Suppressed explains why a plausible action was NOT proposed — an empty
	// delta, a duplicate open proposal on a shared issue, an illegal
	// transition. Kept because "nothing proposed" is otherwise
	// indistinguishable from "nothing considered."
	Suppressed []string
	// SkippedNoDelta is true when every linked event predates the last
	// action, which is the steady state for an unchanged narrative.
	SkippedNoDelta bool
	// LowConfidence names proposals scoring below
	// ReconcilerConfig.MinConfidenceToPropose. They are still in Proposed and
	// still persisted — the threshold governs what unjira asserts, not what it
	// records. See noteLowConfidence.
	LowConfidence []string
}
```

- [ ] **Step 2: Write the failing tests, plus the recording fake**

Create `internal/reconciler/reconciler_test.go`. The `fakeTracker` here is **new** and deliberately
different from `internal/correlator/match_test.go`'s: that one stubs `AddComment`/`SetStatus`/
`CreateIssue` as silent no-ops because matching never writes. A reconciler test must be able to prove
this package never wrote, so these record and can fail.

```go
package reconciler

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/clients/jira"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// fakeTracker records every call, including writes.
//
// Distinct from internal/correlator/match_test.go's fakeTracker, which stubs
// the write methods as silent no-ops because matching never writes. This slice
// proposes actions and must never apply them, so a test needs to be able to
// assert that no write happened — a silent no-op cannot express that.
type fakeTracker struct {
	issues     map[string]tasktracker.Issue
	getErr     map[string]error
	categories map[string][]tasktracker.StatusCategory

	getCalls   []string
	writeCalls []string // any mutating call, for the never-writes assertion
}

func (f *fakeTracker) GetIssue(key string) (tasktracker.Issue, error) {
	f.getCalls = append(f.getCalls, key)

	if err, ok := f.getErr[key]; ok {
		return tasktracker.Issue{}, err
	}

	issue, ok := f.issues[key]
	if !ok {
		return tasktracker.Issue{}, &jira.Error{Status: 404, Message: "issue does not exist"}
	}

	return issue, nil
}

func (f *fakeTracker) AvailableStatusCategories(key string) ([]tasktracker.StatusCategory, error) {
	if c, ok := f.categories[key]; ok {
		return c, nil
	}

	return nil, nil
}

func (f *fakeTracker) SearchIssues(string, int) ([]tasktracker.Issue, error) { return nil, nil }

func (f *fakeTracker) AddComment(key, _ string) error {
	f.writeCalls = append(f.writeCalls, "AddComment:"+key)

	return nil
}

func (f *fakeTracker) SetStatus(key string, _ tasktracker.StatusCategory) error {
	f.writeCalls = append(f.writeCalls, "SetStatus:"+key)

	return nil
}

func (f *fakeTracker) CreateIssue(project, _, _, _ string, _ []string) (string, error) {
	f.writeCalls = append(f.writeCalls, "CreateIssue:"+project)

	return "NEW-1", nil
}

var _ tasktracker.TaskTracker = (*fakeTracker)(nil)

// reconcileStore opens a fresh temp-file store.
func reconcileStore(t *testing.T) *store.Store {
	t.Helper()

	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	return s
}

// seedLinkedNarrative inserts a narrative, links evts to it, and attaches one
// primary issue link. Returns the narrative id.
func seedLinkedNarrative(t *testing.T, s *store.Store, issueKey string, evts ...events.Event) int64 {
	t.Helper()

	base := time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)
	id, err := s.InsertNarrative(base, base.Add(time.Hour), "did the work", "summary of the work")
	require.NoError(t, err)

	ids := make([]int64, 0, len(evts))
	for _, e := range evts {
		eid, err := s.InsertEvent(e)
		require.NoError(t, err)
		ids = append(ids, eid)
	}
	require.NoError(t, s.AddNarrativeEvents(id, ids))

	if issueKey != "" {
		require.NoError(t, s.WithTx(func(tx *store.Tx) error {
			return tx.AddNarrativeIssues(id, []store.NarrativeIssue{{
				IssueKey:   issueKey,
				Role:       store.Role("primary"),
				Provenance: "branch",
				Confidence: 0.9,
				Connection: "dev",
			}})
		}))
	}

	return id
}

// codeEvent builds a non-Jira event (so authored_by_unjira is absent).
func codeEvent(externalID, summary string) events.Event {
	e := events.NewEvent("claude_code", externalID,
		time.Date(2026, 8, 25, 9, 30, 0, 0, time.UTC), summary)

	return e
}

// unjiraComment builds a Jira event unjira itself authored.
func unjiraComment(externalID string) events.Event {
	e := events.NewEvent("jira", externalID,
		time.Date(2026, 8, 25, 9, 45, 0, 0, time.UTC), "a comment unjira posted")
	e.Artifacts["authored_by_unjira"] = true
	e.Artifacts["issue_key"] = "PROJ-1"

	return e
}

func TestReconcileSkipsANarrativeWithNoLinks(t *testing.T) {
	s := reconcileStore(t)
	tracker := &fakeTracker{}
	llm := &fakeLLM{}

	seedLinkedNarrative(t, s, "", codeEvent("e1", "wrote some code"))

	results, _, err := Reconcile(t.Context(), s, tracker, llm, testConfig())
	require.NoError(t, err)

	require.Len(t, results, 1)
	assert.Empty(t, results[0].Proposed,
		"an unlinked narrative is matching's concern, not the reconciler's")
	assert.Empty(t, tracker.getCalls, "no links means no tracker reads")
	assert.Empty(t, llm.prompts, "and no LLM spend")
}

func TestReconcileSkipsWhenTheDeltaIsEmpty(t *testing.T) {
	s := reconcileStore(t)
	tracker := &fakeTracker{
		issues: map[string]tasktracker.Issue{
			"PROJ-1": {Key: "PROJ-1", Summary: "the ticket", StatusCategory: tasktracker.StatusInProgress},
		},
	}

	nid := seedLinkedNarrative(t, s, "PROJ-1", codeEvent("e1", "wrote some code"))

	// A prior action already covers every linked event.
	_, err := s.InsertAction(store.ActionRow{
		NarrativeID: nid, Type: "comment", IssueKey: "PROJ-1",
		Payload: `{"body":"already said this"}`, Status: "proposed",
	})
	require.NoError(t, err)

	llm := &fakeLLM{}
	results, _, err := Reconcile(t.Context(), s, tracker, llm, testConfig())
	require.NoError(t, err)

	require.Len(t, results, 1)
	assert.True(t, results[0].SkippedNoDelta)
	assert.Empty(t, results[0].Proposed)
	assert.Empty(t, llm.prompts,
		"an empty delta must short-circuit BEFORE the LLM call — this is what stops a "+
			"re-run proposing the same comment twice")
}

func TestReconcileDropsAnUnverifiableLinkWithoutFailingTheNarrative(t *testing.T) {
	s := reconcileStore(t)
	// PROJ-404 resolves nowhere: fakeTracker returns a 404 for unknown keys.
	tracker := &fakeTracker{issues: map[string]tasktracker.Issue{}}

	seedLinkedNarrative(t, s, "PROJ-404", codeEvent("e1", "wrote some code"))

	llm := &fakeLLM{}
	results, _, err := Reconcile(t.Context(), s, tracker, llm, testConfig())

	require.NoError(t, err, "a not-found link is recorded, not an error")
	require.Len(t, results, 1)
	assert.Equal(t, []string{"PROJ-404"}, results[0].Unverified)
	assert.Empty(t, results[0].Proposed)
	assert.Empty(t, llm.prompts,
		"verification runs BEFORE drafting: nothing verified means nothing to draft against")
}

func TestReconcileFailsTheNarrativeOnATransportError(t *testing.T) {
	s := reconcileStore(t)
	tracker := &fakeTracker{
		getErr: map[string]error{
			"PROJ-1": &jira.Error{Status: 503, Message: "service unavailable"},
		},
	}

	seedLinkedNarrative(t, s, "PROJ-1", codeEvent("e1", "wrote some code"))

	llm := &fakeLLM{}
	_, _, err := Reconcile(t.Context(), s, tracker, llm, testConfig())

	require.Error(t, err,
		"an unreachable tracker must fail this narrative for retry, not record a "+
			"conclusion drawn from an unreachable tracker")
	assert.Empty(t, llm.prompts)
}

// TestReconcileRecordsButDoesNotDropALowConfidenceProposal pins
// MinConfidenceToPropose's semantics. Dropping the action would make a weak
// proposal indistinguishable from "nothing to do", and slice 6's triage needs
// to see it in order to judge it.
func TestReconcileRecordsButDoesNotDropALowConfidenceProposal(t *testing.T) {
	s := reconcileStore(t)
	tracker := &fakeTracker{
		issues: map[string]tasktracker.Issue{
			"PROJ-1": {Key: "PROJ-1", Summary: "the ticket"},
		},
	}

	seedLinkedNarrative(t, s, "PROJ-1", codeEvent("e1", "did some work"))

	llm := &fakeLLM{responses: []string{
		`[{"issue_key":"PROJ-1","type":"comment","body":"maybe related","confidence":0.2,"rationale":"weak signal"}]`,
	}}

	cfg := config.ReconcilerConfig{MaxNarrativesPerPass: 10, MinConfidenceToPropose: 0.5}
	results, _, err := Reconcile(t.Context(), s, tracker, llm, cfg)
	require.NoError(t, err)

	require.Len(t, results, 1)
	require.Len(t, results[0].Proposed, 1,
		"a below-threshold action is still proposed and still persisted; the threshold "+
			"governs what unjira asserts, not what it records")
	assert.InDelta(t, 0.2, results[0].Proposed[0].Confidence, 0.0001)
	require.Len(t, results[0].LowConfidence, 1,
		"and it must be flagged, or a weak proposal reads as a confident one")
	assert.Contains(t, results[0].LowConfidence[0], "min_confidence_to_propose")
}

func TestReconcileNeverWritesToTheTracker(t *testing.T) {
	s := reconcileStore(t)
	tracker := &fakeTracker{
		issues: map[string]tasktracker.Issue{
			"PROJ-1": {Key: "PROJ-1", Summary: "the ticket", StatusCategory: tasktracker.StatusInProgress},
		},
	}

	seedLinkedNarrative(t, s, "PROJ-1", codeEvent("e1", "wrote some code"))

	llm := &fakeLLM{responses: []string{
		`[{"issue_key":"PROJ-1","type":"comment","body":"the work landed","confidence":0.9,"rationale":"delta shows it"}]`,
	}}

	_, _, err := Reconcile(t.Context(), s, tracker, llm, testConfig())
	require.NoError(t, err)

	assert.Empty(t, tracker.writeCalls,
		"this slice proposes and never applies; any write here is an architecture violation")
}
```

Add a `fakeLLM` in `internal/reconciler/draft_test.go` (Task 6) mirroring
`internal/correlator/correlator_test.go:32`'s shape — `responses []string`, captured `prompts` and
`systemPrompts`, an `err`, and `usagePerCall llm.Usage`. Read that type and copy its structure rather
than inventing a different one. Also add `testConfig()` returning a valid `config.ReconcilerConfig`
(e.g. `{MaxNarrativesPerPass: 10, MinConfidenceToPropose: 0.5}`).

- [ ] **Step 3: Run to verify they fail**

Run: `env -u GOROOT go test ./internal/reconciler/ -v`

Expected: compile failure — `Reconcile` undefined.

- [ ] **Step 4: Implement `reconciler.go`**

```go
package reconciler

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/llm"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// Reconcile drafts proposed actions for narratives with at least one issue
// link and at least one event newer than their last action.
//
// Acquires no lease; the caller holds one, matching RunNarrate's convention
// for Cluster/Persist/Match. Failures are per narrative via errors.Join: one
// narrative's tracker outage leaves it unreconciled and the rest still get a
// chance, since "no proposal" is a valid resting state, so a failed pass costs
// a retry and nothing else.
func Reconcile(
	ctx context.Context,
	s *store.Store,
	tracker tasktracker.TaskTracker,
	client llm.Client,
	cfg config.ReconcilerConfig,
) ([]ReconcileResult, correlator.Stats, error) {
	limit := cfg.NarrativeLimit()

	narratives, err := s.NarrativesWithIssueKey(limit + 1)
	if err != nil {
		return nil, correlator.Stats{}, fmt.Errorf("listing linked narratives: %w", err)
	}

	// Reaching the cap is logged, never silent: a silent cap presents as a
	// clean pass that quietly ignored work.
	if len(narratives) > limit {
		log.Printf(
			"reconciler: %d or more narratives are eligible but this pass examines %d "+
				"(reconciler.max_narratives_per_pass); the remainder wait for the next pass",
			len(narratives), limit,
		)
		narratives = narratives[:limit]
	}

	var (
		results []ReconcileResult
		stats   correlator.Stats
		errs    error
	)

	for _, n := range narratives {
		result, oneStats, err := reconcileOne(ctx, s, tracker, client, n, cfg)
		stats.Add(oneStats)
		results = append(results, result)
		if err != nil {
			errs = errors.Join(errs, err)
		}
	}

	return results, stats, errs
}

// reconcileOne handles a single narrative: load links, compute the delta,
// verify every link against the live tracker, then draft.
//
// Order matters and is load-bearing. The delta check precedes verification,
// which precedes drafting: an unchanged narrative must cost neither a tracker
// read nor an LLM call, and nothing may be drafted against state this pass has
// not confirmed (rules/intent-not-outcome.md, rules/verify-correlations.md).
func reconcileOne(
	ctx context.Context,
	s *store.Store,
	tracker tasktracker.TaskTracker,
	client llm.Client,
	narrative store.NarrativeRow,
	cfg config.ReconcilerConfig,
) (ReconcileResult, correlator.Stats, error) {
	result := ReconcileResult{NarrativeID: narrative.ID}

	links, err := s.NarrativeIssues(narrative.ID)
	if err != nil {
		return result, correlator.Stats{}, fmt.Errorf("loading links for narrative %d: %w", narrative.ID, err)
	}

	actionable := actionableLinks(links)
	if len(actionable) == 0 {
		// Either no links at all (matching's concern) or only `mentioned`
		// links, which by definition get nothing.
		return result, correlator.Stats{}, nil
	}

	delta, err := s.DeltaEvents(narrative.ID)
	if err != nil {
		return result, correlator.Stats{}, fmt.Errorf("computing delta for narrative %d: %w", narrative.ID, err)
	}

	delta = dropSelfAuthored(delta)
	if len(delta) == 0 {
		result.SkippedNoDelta = true

		return result, correlator.Stats{}, nil
	}

	verified, unverified, err := verifyLinks(tracker, narrative.ID, actionable)
	result.Unverified = unverified
	if err != nil {
		return result, correlator.Stats{}, err
	}

	if len(verified) == 0 {
		// Every link was unresolvable. A `create` is NOT proposed here: the
		// narrative does have links, they just did not resolve this pass, and
		// manufacturing a ticket for work that is probably already tracked is
		// worse than proposing nothing.
		result.Suppressed = append(result.Suppressed,
			"no link verified against the live tracker; nothing drafted")

		return result, correlator.Stats{}, nil
	}

	drafted, stats, err := draft(ctx, client, narrative, delta, verified)
	if err != nil {
		return result, stats, fmt.Errorf("drafting for narrative %d: %w", narrative.ID, err)
	}

	kept, suppressed := suppressDuplicates(s, narrative.ID, drafted)
	result.Proposed = kept
	result.Suppressed = append(result.Suppressed, suppressed...)
	noteLowConfidence(&result, cfg.MinConfidenceToPropose)

	return result, stats, nil
}

// noteLowConfidence records which proposals fell below the configured
// threshold, WITHOUT dropping them.
//
// The threshold governs what unjira *asserts*, not what it *records*: slice 6's
// triage must be able to see a weak proposal in order to judge it, and a
// dropped proposal is indistinguishable from "nothing to do." Same reasoning as
// MatchConfig.ConfidenceFloor gating promotion rather than recording.
//
// So this only annotates the result, for rendering. Every action is still
// returned and still persisted carrying its low score. Slice 5's auto-commit
// gate is what will consult the score to decide whether to apply without
// review — which is why the threshold has to be visible somewhere now rather
// than silently unused.
func noteLowConfidence(result *ReconcileResult, threshold float64) {
	for _, a := range result.Proposed {
		if a.Confidence < threshold {
			result.LowConfidence = append(result.LowConfidence, fmt.Sprintf(
				"%s %s at confidence %.2f is below reconciler.min_confidence_to_propose %.2f "+
					"(recorded, not fit to auto-apply)",
				a.Type, a.IssueKey, a.Confidence, threshold))
		}
	}
}

// actionableLinks returns the links that may receive an action: the primary
// and every same_work co-representation.
//
// `mentioned` links get nothing — that is what the role means (a citation, a
// "caused by", a "discovered while"). Each returned link gets its OWN action,
// drafted separately: the motivating case is one body of work recorded twice
// for two audiences (an engineering ticket plus a paired change-management
// ticket), where identical text on both would defeat the point.
func actionableLinks(links []store.NarrativeIssue) []store.NarrativeIssue {
	var out []store.NarrativeIssue
	for _, l := range links {
		if l.Role == store.Role(correlator.RolePrimary) ||
			l.Role == store.Role(correlator.RoleSameWork) {
			out = append(out, l)
		}
	}

	return out
}

// dropSelfAuthored removes events unjira itself produced.
//
// The Jira collector sets artifacts["authored_by_unjira"], and its doc comment
// states exactly why: "without it the reconciler proposes the same comment
// every pass." That artifact has been written since the collector landed and
// read by nothing — this is its first consumer. Without this step the loop is
// unstable: unjira comments, collects its own comment, and proposes commenting
// about it.
func dropSelfAuthored(evts []events.Event) []events.Event {
	var out []events.Event
	for _, e := range evts {
		if authored, _ := e.Artifacts["authored_by_unjira"].(bool); authored {
			continue
		}
		out = append(out, e)
	}

	return out
}

// verifyLinks confirms every link's issue still exists, and reads its legal
// transitions in the same pass.
//
// A not-found key is recorded as unverified and skipped; a transport error
// aborts this narrative so the next pass retries it. correlator.IsTransportError
// makes that distinction and is reused rather than reimplemented — getting it
// backwards in either direction is a real failure mode documented at length on
// that function.
func verifyLinks(
	tracker tasktracker.TaskTracker,
	narrativeID int64,
	links []store.NarrativeIssue,
) (verified []verifiedLink, unverified []string, err error) {
	for _, l := range links {
		issue, getErr := tracker.GetIssue(l.IssueKey)
		if getErr != nil {
			if correlator.IsTransportError(getErr) {
				return nil, unverified, fmt.Errorf(
					"verifying link %s for narrative %d: %w", l.IssueKey, narrativeID, getErr,
				)
			}
			unverified = append(unverified, l.IssueKey)

			continue
		}

		categories, catErr := tracker.AvailableStatusCategories(l.IssueKey)
		if catErr != nil {
			if correlator.IsTransportError(catErr) {
				return nil, unverified, fmt.Errorf(
					"reading transitions for %s on narrative %d: %w", l.IssueKey, narrativeID, catErr,
				)
			}
			// Not-found here is odd (GetIssue just succeeded) but survivable:
			// no known-legal transition means no transition is proposed, which
			// floorConfidence enforces.
			categories = nil
		}

		verified = append(verified, verifiedLink{
			Link: l, Issue: issue, AvailableStatus: categories,
		})
	}

	return verified, unverified, nil
}

// suppressDuplicates drops any action whose issue already has an open
// proposal from a DIFFERENT narrative, via store.NarrativesForIssue.
//
// The shared-ticket case: two narratives both linked to one change-management
// ticket should not stack two comments on it. That accessor was built for this
// slice (its doc comment says so) and has had no caller until now.
func suppressDuplicates(
	s *store.Store,
	narrativeID int64,
	drafted []ProposedAction,
) (kept []ProposedAction, suppressed []string) {
	for _, a := range drafted {
		if a.Type == ActionCreate {
			kept = append(kept, a)

			continue
		}

		refs, err := s.NarrativesForIssue(a.IssueKey)
		if err != nil {
			// A failed reverse lookup must not silently drop a proposal:
			// keep it and say so. A duplicate comment is recoverable; a
			// silently-vanished proposal is not.
			log.Printf(
				"reconciler: narrative %d: checking other narratives on %s failed (%v); "+
					"keeping the proposal rather than dropping it",
				narrativeID, a.IssueKey, err,
			)
			kept = append(kept, a)

			continue
		}

		if openProposalFromAnother(s, refs, narrativeID, a.IssueKey) {
			suppressed = append(suppressed, fmt.Sprintf(
				"%s: another narrative already has an open proposal on this issue", a.IssueKey))

			continue
		}

		kept = append(kept, a)
	}

	return kept, suppressed
}

// openProposalFromAnother reports whether a narrative other than narrativeID
// already has a status=proposed action on issueKey.
func openProposalFromAnother(
	s *store.Store,
	refs []store.NarrativeIssueRef,
	narrativeID int64,
	issueKey string,
) bool {
	for _, ref := range refs {
		if ref.NarrativeID == narrativeID {
			continue
		}

		actions, err := s.ActionsForNarrative(ref.NarrativeID)
		if err != nil {
			log.Printf("reconciler: reading actions for narrative %d: %v", ref.NarrativeID, err)

			continue
		}

		for _, a := range actions {
			if a.IssueKey == issueKey && a.Status == "proposed" {
				return true
			}
		}
	}

	return false
}
```

- [ ] **Step 5: Add the missing store accessor**

`Reconcile` calls `s.NarrativesWithIssueKey(limit)`, which does not exist — the store has only
`NarrativesWithoutIssueKey`. Add its complement to `internal/store/store.go`, modelled on that
function:

```go
// NarrativesWithIssueKey returns up to limit narratives that HAVE been
// attributed to a primary issue, ordered by (window_start, id) — the
// reconciler's input backlog, the exact complement of
// NarrativesWithoutIssueKey (matching's backlog).
func (s *Store) NarrativesWithIssueKey(limit int) ([]NarrativeRow, error) {
	rows, err := s.db.Query(
		narrativeSelect+` WHERE issue_key IS NOT NULL AND issue_key != ''
		 ORDER BY window_start, id LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("querying narratives with an issue key: %w", err)
	}
	defer rows.Close()

	return scanNarratives(rows)
}
```

Read `NarrativesWithoutIssueKey` first and match its actual select-list/scanner names — if it inlines
its columns rather than using a `narrativeSelect` const, do the same here rather than refactoring it.
Add a test asserting it returns only linked narratives and respects `limit`.

- [ ] **Step 6: Run tests**

Run: `env -u GOROOT go test ./internal/reconciler/ ./internal/store/ -v 2>&1 | tail -40`

Expected: every Task-5 test PASSES except the two that need Task 6's `draft`
(`TestReconcileNeverWritesToTheTracker` needs it). If `draft` is not written yet, stub it to return
`nil, correlator.Stats{}, nil` so this task's tests compile and pass, and let Task 6 replace the stub.

- [ ] **Step 7: Commit**

```bash
git add internal/reconciler/ internal/store/
git commit -m "reconciler: delta computation and live verification

Establishes the deterministic half: actionable-link selection (primary +
same_work, never mentioned), the delta from DeltaEvents, self-authored-event
filtering, and mandatory GetIssue verification before anything is drafted.

authored_by_unjira gets its first consumer since the Jira collector started
writing it — without that filter unjira comments, collects its own comment, and
proposes commenting about it."
```

---

## Task 6: drafting, and confidence floored by facts

**Files:**
- Create: `internal/reconciler/draft.go`
- Create: `internal/reconciler/draft_test.go`

- [ ] **Step 1: Write the failing tests**

```go
func TestDraftProducesOneActionPerActionableLinkWithDistinctBodies(t *testing.T) {
	// The motivating workflow: an engineering ticket plus a paired
	// change-management ticket for the same deploy. Two audiences, so two
	// separately-drafted bodies.
	client := &fakeLLM{responses: []string{`[
		{"issue_key":"PAAS-1","type":"comment","body":"Shipped the retry logic; see the PR.","confidence":0.9,"rationale":"delta is code work"},
		{"issue_key":"SUMO-9","type":"comment","body":"Change deployed to production, no customer impact.","confidence":0.8,"rationale":"change-management audience"}
	]`}}

	narrative := store.NarrativeRow{ID: 1, Title: "retry logic", Summary: "added retries"}
	verified := []verifiedLink{
		{Link: store.NarrativeIssue{IssueKey: "PAAS-1", Role: store.Role("primary")},
			Issue: tasktracker.Issue{Key: "PAAS-1", Summary: "add retries"}},
		{Link: store.NarrativeIssue{IssueKey: "SUMO-9", Role: store.Role("same_work")},
			Issue: tasktracker.Issue{Key: "SUMO-9", Summary: "prod change"}},
	}

	got, stats, err := draft(t.Context(), client, narrative,
		[]events.Event{codeEvent("e1", "added retry logic")}, verified)
	require.NoError(t, err)

	require.Len(t, got, 2)
	assert.Equal(t, 1, stats.Calls, "one LLM call per narrative, not per link")
	assert.NotEqual(t, got[0].Body, got[1].Body,
		"identical text on both tickets would defeat the point of the audiences differing")
}

func TestDraftFloorsConfidenceForAnIllegalTransition(t *testing.T) {
	// The model claims high confidence in a Done transition. The live issue
	// offers only In Progress.
	client := &fakeLLM{responses: []string{
		`[{"issue_key":"PROJ-1","type":"transition","target_status":"done","confidence":0.95,"rationale":"work is finished"}]`,
	}}

	narrative := store.NarrativeRow{ID: 1, Title: "t", Summary: "s"}
	verified := []verifiedLink{{
		Link:            store.NarrativeIssue{IssueKey: "PROJ-1", Role: store.Role("primary")},
		Issue:           tasktracker.Issue{Key: "PROJ-1", StatusCategory: tasktracker.StatusTodo},
		AvailableStatus: []tasktracker.StatusCategory{tasktracker.StatusInProgress},
	}}

	got, _, err := draft(t.Context(), client, narrative,
		[]events.Event{codeEvent("e1", "finished it")}, verified)
	require.NoError(t, err)

	require.Len(t, got, 1)
	assert.Zero(t, got[0].Confidence,
		"a transition the live issue does not offer is floored to zero regardless of the "+
			"model's self-report — rules/verify-correlations.md: a hallucinated match can be "+
			"high-confidence")
}

func TestDraftIgnoresAnUnrecognizedIssueKey(t *testing.T) {
	// The model names a key that was never presented as a verified link.
	client := &fakeLLM{responses: []string{
		`[{"issue_key":"GHOST-1","type":"comment","body":"x","confidence":0.9,"rationale":"invented"}]`,
	}}

	narrative := store.NarrativeRow{ID: 1, Title: "t", Summary: "s"}
	verified := []verifiedLink{{
		Link:  store.NarrativeIssue{IssueKey: "PROJ-1", Role: store.Role("primary")},
		Issue: tasktracker.Issue{Key: "PROJ-1"},
	}}

	got, _, err := draft(t.Context(), client, narrative,
		[]events.Event{codeEvent("e1", "did work")}, verified)
	require.NoError(t, err)

	assert.Empty(t, got,
		"a key this pass never verified must not receive an action, whatever confidence "+
			"the model stated")
}

func TestDraftPromptCarriesTheDeltaAndEachLinksLiveState(t *testing.T) {
	client := &fakeLLM{responses: []string{
		`[{"issue_key":"PROJ-1","type":"comment","body":"ok","confidence":0.7,"rationale":"r"}]`,
	}}

	narrative := store.NarrativeRow{ID: 1, Title: "the title", Summary: "the summary"}
	verified := []verifiedLink{{
		Link: store.NarrativeIssue{IssueKey: "PROJ-1", Role: store.Role("primary")},
		Issue: tasktracker.Issue{
			Key: "PROJ-1", Summary: "live ticket summary", StatusName: "In Progress",
		},
		AvailableStatus: []tasktracker.StatusCategory{tasktracker.StatusDone},
	}}

	_, _, err := draft(t.Context(), client, narrative,
		[]events.Event{codeEvent("e1", "the delta event summary")}, verified)
	require.NoError(t, err)

	require.Len(t, client.prompts, 1)
	prompt := client.prompts[0]

	assert.Contains(t, prompt, "the delta event summary", "the delta is what gets proposed on")
	assert.Contains(t, prompt, "PROJ-1")
	assert.Contains(t, prompt, "primary", "the role determines the framing")
	assert.Contains(t, prompt, "In Progress", "live status, not inferred status")
	assert.Contains(t, prompt, "done", "the legal transitions bound what may be proposed")
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `env -u GOROOT go test ./internal/reconciler/ -run TestDraft -v`

Expected: FAIL — `draft` is the Task-5 stub returning nil, so every assertion on content fails.

- [ ] **Step 3: Implement `draft.go`**

```go
package reconciler

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/llm"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// draftSystemPrompt instructs the model to produce one action per actionable
// link. Worded to match types.go's ActionType doc comments so the two cannot
// drift apart.
const draftSystemPrompt = `You are drafting proposed updates to tracker issues from a record of work that was actually done.

You will be shown WHAT'S NEW since a reviewer last saw this work (the delta — never the full history), and every issue the work is attributed to, each with its role and its CURRENT live state.

Draft exactly one action per issue shown. Roles:
- "primary": the record the work is principally tracked in. Write for an engineering audience.
- "same_work": the same body of work recorded again for a different audience (for example a change-management ticket paired with an engineering one). Write for THAT audience — do not repeat the primary's text.

Action types:
- "comment": post prose describing what changed. The default.
- "transition": move the issue to a different status. ONLY propose a target listed under "legal transitions" for that issue. If the status you believe is correct is not listed, propose a comment instead.

Report confidence honestly in 0.0-1.0. Do not describe work that is not evidenced in the delta.

Return ONLY a bare JSON array, no prose, no markdown fences:
[{"issue_key":"...","type":"comment"|"transition","body":"...","target_status":"todo"|"in_progress"|"done","confidence":0.0-1.0,"rationale":"..."}]`

// draftVerdict is one entry of the model's response.
type draftVerdict struct {
	IssueKey     string  `json:"issue_key"`
	Type         string  `json:"type"`
	Body         string  `json:"body"`
	TargetStatus string  `json:"target_status"`
	Confidence   float64 `json:"confidence"`
	Rationale    string  `json:"rationale"`
}

// draft makes ONE LLM call for the whole narrative and returns one
// ProposedAction per recognized, actionable link.
//
// One call rather than one per link is deliberate: the same_work case requires
// each draft to be written in awareness of the others (so the two audiences get
// genuinely different text), which a per-link call cannot do.
func draft(
	ctx context.Context,
	client llm.Client,
	narrative store.NarrativeRow,
	delta []events.Event,
	verified []verifiedLink,
) ([]ProposedAction, correlator.Stats, error) {
	var stats correlator.Stats

	prompt := buildDraftPrompt(narrative, delta, verified)

	raw, usage, err := client.Complete(ctx, draftSystemPrompt, prompt)
	if err != nil {
		return nil, stats, fmt.Errorf("drafting actions for narrative %d: %w", narrative.ID, err)
	}
	stats.Calls++
	stats.addUsage(usage)

	verdicts, err := parseDraftResponse(raw)
	if err != nil {
		return nil, stats, err
	}

	byKey := make(map[string]verifiedLink, len(verified))
	for _, v := range verified {
		byKey[v.Link.IssueKey] = v
	}

	var out []ProposedAction
	for _, verdict := range verdicts {
		v, ok := byKey[verdict.IssueKey]
		if !ok {
			// Not verified against the live tracker under this pass, so per
			// rules/verify-correlations.md it is not trusted regardless of
			// stated confidence. Log loudly and skip.
			log.Printf(
				"reconciler: narrative %d draft named unrecognized issue_key %q, ignoring",
				narrative.ID, verdict.IssueKey,
			)

			continue
		}

		out = append(out, toProposedAction(verdict, v))
	}

	return out, stats, nil
}

// toProposedAction converts one verdict, flooring its confidence against
// deterministic facts.
func toProposedAction(verdict draftVerdict, v verifiedLink) ProposedAction {
	action := ProposedAction{
		Type:       ActionType(verdict.Type),
		IssueKey:   verdict.IssueKey,
		Body:       verdict.Body,
		Confidence: verdict.Confidence,
		Rationale:  verdict.Rationale,
	}

	if action.Type == ActionTransition {
		action.TargetStatus = tasktracker.StatusCategory(verdict.TargetStatus)
	}

	action.Confidence = floorConfidence(action, v)

	return action
}

// floorConfidence caps the model's self-reported score using facts this
// package established.
//
// The model's number is evidence, not a verdict — rules/verify-correlations.md:
// "Confidence scores are not enough; a hallucinated match can be
// high-confidence." So deterministic checks can only ever LOWER it, never
// raise it.
//
// A transition to a category the live issue does not offer is floored to zero
// rather than merely reduced: the backend would refuse it outright, so there is
// no confidence level at which proposing it is correct.
func floorConfidence(action ProposedAction, v verifiedLink) float64 {
	confidence := action.Confidence

	// Clamp a model that returned something outside the documented range.
	if confidence < 0 {
		confidence = 0
	}
	if confidence > 1 {
		confidence = 1
	}

	if action.Type != ActionTransition {
		return confidence
	}

	for _, available := range v.AvailableStatus {
		if available == action.TargetStatus {
			return confidence
		}
	}

	return 0
}

// parseDraftResponse decodes the model's JSON array, tolerating a markdown
// fence the model may add despite the system prompt forbidding it.
func parseDraftResponse(raw string) ([]draftVerdict, error) {
	cleaned := stripJSONFence(raw)

	var verdicts []draftVerdict
	if err := json.Unmarshal([]byte(cleaned), &verdicts); err != nil {
		return nil, fmt.Errorf("parsing draft response %q: %w", truncateForError(cleaned), err)
	}

	for i, v := range verdicts {
		switch ActionType(v.Type) {
		case ActionComment, ActionTransition, ActionCreate:
		default:
			return nil, fmt.Errorf(
				"draft response entry %d has unknown action type %q: must be one of comment, transition, create",
				i, v.Type,
			)
		}
		if v.IssueKey == "" && ActionType(v.Type) != ActionCreate {
			return nil, fmt.Errorf("draft response entry %d has no issue_key", i)
		}
	}

	return verdicts, nil
}

// buildDraftPrompt renders the delta and each link's live state.
//
// The delta only — never the full cumulative narrative — so a reviewer sees
// "what's new since you last saw this" rather than a repeated history.
func buildDraftPrompt(narrative store.NarrativeRow, delta []events.Event, verified []verifiedLink) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Narrative: title=%q\nsummary: %q\n\n", narrative.Title, narrative.Summary)

	b.WriteString("What's new since the last proposal (the delta):\n")
	for _, e := range delta {
		fmt.Fprintf(&b, "- [%s] %s: %s\n",
			e.OccurredAt.Format("2006-01-02 15:04"), e.Source, e.Summary)
	}

	b.WriteString("\nIssues this work is attributed to:\n")
	for _, v := range verified {
		fmt.Fprintf(&b, "- issue_key=%s role=%s\n", v.Link.IssueKey, v.Link.Role)
		fmt.Fprintf(&b, "  current status: %s\n", v.Issue.StatusName)
		fmt.Fprintf(&b, "  summary: %q\n", v.Issue.Summary)
		fmt.Fprintf(&b, "  description: %q\n", v.Issue.Description)

		if len(v.AvailableStatus) == 0 {
			b.WriteString("  legal transitions: none available — propose a comment, not a transition\n")

			continue
		}

		targets := make([]string, 0, len(v.AvailableStatus))
		for _, c := range v.AvailableStatus {
			targets = append(targets, string(c))
		}
		fmt.Fprintf(&b, "  legal transitions: %s\n", strings.Join(targets, ", "))
	}

	return b.String()
}
```

- [ ] **Step 4: Reuse, do not duplicate, the shared helpers**

`stripJSONFence`, `truncateForError`, and `Stats.addUsage` already exist in `internal/correlator` and
are **unexported**, so this package cannot reach them. Before writing new copies:

```sh
grep -rn "func stripJSONFence\|func truncateForError\|func (s \*Stats) addUsage" internal/correlator/
```

Choose one, and state the choice in a comment:
- **Preferred:** move `stripJSONFence` and `truncateForError` into a small shared home both packages
  import (they are pure string functions with no dependencies), and export `Stats.AddUsage` on
  `correlator.Stats` alongside the already-exported `Add`.
- If that turns out to widen the change more than expected, copy them into `draft.go` with a comment
  naming the original and why it was copied. Do **not** leave two silently-diverging copies unexplained.

`correlator.Stats` is reused rather than a new `reconciler.Stats` because `internal/pipeline` already
merges stats across stages into one pass total (`Stats.Add`'s doc comment says so).

- [ ] **Step 5: Run tests**

Run: `env -u GOROOT go test ./internal/reconciler/ -v 2>&1 | tail -30`

Expected: all Task-5 and Task-6 tests PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/reconciler/
git commit -m "reconciler: LLM drafting with confidence floored by facts

One call per narrative, not per link: the same_work case needs each draft
written in awareness of the others so the two audiences get different text.

Deterministic facts can only lower the model's self-reported confidence, never
raise it. A transition the live issue does not offer floors to zero, since the
backend would refuse it outright and no confidence level makes proposing it
correct."
```

---

## Task 7: all-or-nothing `Persist`

**Files:**
- Create: `internal/reconciler/persist.go`
- Create: `internal/reconciler/persist_test.go`

- [ ] **Step 1: Write the failing tests**

```go
func TestPersistWritesEveryProposedActionAsProposed(t *testing.T) {
	s := reconcileStore(t)
	nid := seedLinkedNarrative(t, s, "PROJ-1", codeEvent("e1", "did work"))

	err := Persist(s, []ReconcileResult{{
		NarrativeID: nid,
		Proposed: []ProposedAction{
			{Type: ActionComment, IssueKey: "PROJ-1", Body: "the work landed",
				Confidence: 0.8, Rationale: "delta shows it"},
			{Type: ActionTransition, IssueKey: "PROJ-1",
				TargetStatus: tasktracker.StatusDone, Confidence: 0.6, Rationale: "finished"},
		},
	}})
	require.NoError(t, err)

	got, err := s.ActionsForNarrative(nid)
	require.NoError(t, err)
	require.Len(t, got, 2)

	for _, a := range got {
		assert.Equal(t, "proposed", a.Status,
			"this slice proposes only; nothing is approved or applied here")
		assert.Nil(t, a.DecidedAt)
		assert.Nil(t, a.ExecutedAt)
	}

	assert.Equal(t, "comment", got[0].Type)
	assert.JSONEq(t, `{"body":"the work landed"}`, got[0].Payload)
	assert.Equal(t, "transition", got[1].Type)
	assert.JSONEq(t, `{"target_status":"done"}`, got[1].Payload)
}

func TestPersistWritesNothingWhenOneActionFails(t *testing.T) {
	s := reconcileStore(t)
	nid := seedLinkedNarrative(t, s, "PROJ-1", codeEvent("e1", "did work"))

	// The second action names a narrative that does not exist, violating the
	// actions.narrative_id foreign key.
	err := Persist(s, []ReconcileResult{
		{NarrativeID: nid, Proposed: []ProposedAction{
			{Type: ActionComment, IssueKey: "PROJ-1", Body: "would be fine", Confidence: 0.8},
		}},
		{NarrativeID: nid + 99999, Proposed: []ProposedAction{
			{Type: ActionComment, IssueKey: "PROJ-2", Body: "breaks the FK", Confidence: 0.8},
		}},
	})
	require.Error(t, err)

	got, err := s.ActionsForNarrative(nid)
	require.NoError(t, err)
	assert.Empty(t, got,
		"Persist is all-or-nothing per pass: the good action must roll back with the bad "+
			"one, or slice 5's auto-commit gate could apply against a half-written set")
}

func TestPersistIsANoOpForResultsWithNoProposals(t *testing.T) {
	s := reconcileStore(t)
	nid := seedLinkedNarrative(t, s, "PROJ-1", codeEvent("e1", "did work"))

	require.NoError(t, Persist(s, []ReconcileResult{
		{NarrativeID: nid, SkippedNoDelta: true},
	}))

	got, err := s.ActionsForNarrative(nid)
	require.NoError(t, err)
	assert.Empty(t, got)
}
```

Note the FK test depends on SQLite enforcing foreign keys. Verify first:

```sh
grep -n "foreign_keys" internal/store/store.go
```

If `PRAGMA foreign_keys = ON` is **not** set, the insert will succeed and this test will fail for the
wrong reason. In that case either enable the pragma (check nothing else depends on it being off) or
force the failure differently — e.g. a `Type` violating a CHECK constraint, or inject a closed store.
State in a comment which mechanism you used and why.

- [ ] **Step 2: Run to verify it fails**

Run: `env -u GOROOT go test ./internal/reconciler/ -run TestPersist -v`

Expected: compile failure — `Persist` undefined.

- [ ] **Step 3: Implement `persist.go`**

```go
package reconciler

import (
	"encoding/json"
	"fmt"

	"github.com/jcogilvie/unjira/internal/store"
)

// Persist writes every proposed action from a whole pass, atomically.
//
// All-or-nothing per pass, in one store.WithTx, because slice 5's auto-commit
// gate requires it (the phase-1 spec: "if reconciliation fails partway through
// a pass, nothing from that pass auto-commits until a clean pass succeeds").
// Half-written proposals would let that gate auto-apply against an
// inconsistent set.
//
// Nothing here is applied to a tracker: every row lands with status=proposed.
func Persist(s *store.Store, results []ReconcileResult) error {
	return s.WithTx(func(tx *store.Tx) error {
		for _, result := range results {
			for _, action := range result.Proposed {
				payload, err := actionPayload(action)
				if err != nil {
					return fmt.Errorf(
						"encoding payload for %s action on narrative %d: %w",
						action.Type, result.NarrativeID, err,
					)
				}

				if _, err := tx.InsertAction(store.ActionRow{
					NarrativeID: result.NarrativeID,
					Type:        string(action.Type),
					IssueKey:    action.IssueKey,
					Payload:     payload,
					Confidence:  action.Confidence,
					Rationale:   action.Rationale,
					Status:      "proposed",
				}); err != nil {
					return err
				}
			}
		}

		return nil
	})
}

// actionPayload encodes the type-specific half of an action as JSON, since
// actions.payload's shape depends on actions.type.
//
// Encoded via a map rather than string concatenation so a body containing
// quotes or newlines cannot corrupt the column — comment bodies are free prose
// and routinely contain both.
func actionPayload(action ProposedAction) (string, error) {
	var payload map[string]any

	switch action.Type {
	case ActionComment:
		payload = map[string]any{"body": action.Body}
	case ActionTransition:
		payload = map[string]any{"target_status": string(action.TargetStatus)}
	case ActionCreate:
		payload = map[string]any{"summary": action.Summary, "description": action.Body}
	default:
		return "", fmt.Errorf("unknown action type %q", action.Type)
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	return string(encoded), nil
}
```

- [ ] **Step 4: Run tests**

Run: `env -u GOROOT go test ./internal/reconciler/ -v 2>&1 | tail -30`

Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/reconciler/
git commit -m "reconciler: all-or-nothing Persist

One WithTx for the whole pass, because slice 5's auto-commit gate requires that
nothing from a failed pass be visible: half-written proposals would let it
auto-apply against an inconsistent set.

Payloads are JSON-encoded via a map rather than concatenated, since comment
bodies are free prose that routinely contains quotes and newlines."
```

---

## Task 8: `pipeline.RunReconcile` + renderer

**Files:**
- Create: `internal/pipeline/reconcile.go`
- Modify: `internal/pipeline/narrate_render.go`
- Test: `internal/pipeline/reconcile_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestRunReconcileValidatesConfigBeforeAnyWork(t *testing.T) {
	s := reconcileTestStore(t)

	cfg := config.Config{Reconciler: config.ReconcilerConfig{MinConfidenceToPropose: 1.5}}

	_, err := RunReconcile(t.Context(), s, &stubTracker{}, &stubLLM{}, cfg, ReconcileOptions{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "min_confidence_to_propose",
		"a bad config must fail before any tracker or LLM call")
}

func TestRunReconcileDryRunPersistsNothing(t *testing.T) {
	s := reconcileTestStore(t)
	nid := seedReconcilableNarrative(t, s)

	cfg := config.Config{Reconciler: config.ReconcilerConfig{
		MaxNarrativesPerPass: 10, MinConfidenceToPropose: 0.5,
	}}

	result, err := RunReconcile(t.Context(), s, reconcileStubTracker(),
		reconcileStubLLM(), cfg, ReconcileOptions{DryRun: true})
	require.NoError(t, err)

	assert.True(t, result.DryRun)
	assert.NotEmpty(t, result.Results, "the pass still ran, including the real LLM call")

	got, err := s.ActionsForNarrative(nid)
	require.NoError(t, err)
	assert.Empty(t, got, "--dry-run skips Persist")
}

func TestRenderReconcileResultNamesWhatWasSuppressedAndWhy(t *testing.T) {
	out := RenderReconcileResult(ReconcileRunResult{
		Results: []reconciler.ReconcileResult{
			{NarrativeID: 1, Proposed: []reconciler.ProposedAction{
				{Type: reconciler.ActionComment, IssueKey: "PROJ-1",
					Body: "the work landed", Confidence: 0.8},
			}},
			{NarrativeID: 2, SkippedNoDelta: true},
			{NarrativeID: 3, Unverified: []string{"PROJ-404"}},
			{NarrativeID: 4, Suppressed: []string{"PROJ-9: another narrative already has an open proposal"}},
		},
	})

	assert.Contains(t, out, "PROJ-1")
	assert.Contains(t, out, "0.8")
	assert.Contains(t, out, "no new events", "an unchanged narrative must be explained, not omitted")
	assert.Contains(t, out, "PROJ-404")
	assert.Contains(t, out, "already has an open proposal",
		"'nothing proposed' is otherwise indistinguishable from 'nothing considered'")
}
```

Reuse `internal/pipeline`'s existing test doubles if it has them (`grep -n "stubTracker\|stubLLM"
internal/pipeline/*_test.go`); if not, add minimal ones next to the existing `match_test.go` fakes and
match their naming.

- [ ] **Step 2: Run to verify it fails**

Run: `env -u GOROOT go test ./internal/pipeline/ -run 'TestRunReconcile|TestRenderReconcile' -v`

Expected: compile failure.

- [ ] **Step 3: Implement `reconcile.go`**

```go
package pipeline

import (
	"context"
	"fmt"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/llm"
	"github.com/jcogilvie/unjira/internal/reconciler"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// ReconcileOptions configures one reconcile pass.
type ReconcileOptions struct {
	// DryRun runs the full pass — including the real LLM calls — but skips
	// Persist, matching NarrateOptions.DryRun's treatment.
	DryRun bool
}

// ReconcileRunResult is one reconcile pass, shaped for rendering.
type ReconcileRunResult struct {
	Results []reconciler.ReconcileResult
	Stats   correlator.Stats
	DryRun  bool
}

// RunReconcile runs one reconcile pass: validate cfg.Reconciler, draft
// proposals, and (unless DryRun) persist them.
//
// Acquires no lease, for the same reason RunNarrate and RunMatch do not: the
// scope differs per caller, and taking one here would make `watch` contend with
// itself.
//
// On partial failure this returns both the accumulated result AND a non-nil
// error, because Reconcile isolates failures per narrative — the narratives that
// drafted cleanly are still worth showing. Note the asymmetry with Persist: a
// partial pass still persists whatever it produced, since each narrative's
// proposals are internally consistent even when a sibling narrative failed.
func RunReconcile(
	ctx context.Context,
	s *store.Store,
	tracker tasktracker.TaskTracker,
	client llm.Client,
	cfg config.Config,
	opts ReconcileOptions,
) (ReconcileRunResult, error) {
	if err := cfg.Reconciler.Validate(); err != nil {
		return ReconcileRunResult{}, fmt.Errorf("invalid reconciler config: %w", err)
	}

	results, stats, reconcileErr := reconciler.Reconcile(ctx, s, tracker, client, cfg.Reconciler)
	result := ReconcileRunResult{Results: results, Stats: stats, DryRun: opts.DryRun}

	if opts.DryRun {
		return result, reconcileErr
	}

	if err := reconciler.Persist(s, results); err != nil {
		return result, fmt.Errorf("persisting proposed actions: %w", err)
	}

	return result, reconcileErr
}
```

- [ ] **Step 4: Add the renderer**

Append to `internal/pipeline/narrate_render.go`, matching `RenderMatchResult`'s style (read
`internal/pipeline/narrate_render.go:111` first and mirror its header/section helpers):

```go
// RenderReconcileResult formats a reconcile pass for a terminal.
//
// Every narrative appears, including ones that produced nothing: an unchanged
// narrative, an unresolvable link, and a suppressed duplicate are all
// legitimate outcomes, and omitting them would make "nothing proposed"
// indistinguishable from "nothing considered."
func RenderReconcileResult(r ReconcileRunResult) string {
	var b strings.Builder

	b.WriteString("\nreconcile\n")
	if r.DryRun {
		b.WriteString("  (dry run — nothing persisted)\n")
	}
	fmt.Fprintf(&b, "  llm calls: %d  tokens: %d prompt / %d completion\n",
		r.Stats.Calls, r.Stats.PromptTokens, r.Stats.CompletionTokens)

	if len(r.Results) == 0 {
		b.WriteString("\nno linked narratives to reconcile\n")

		return b.String()
	}

	for _, result := range r.Results {
		fmt.Fprintf(&b, "\nnarrative %d\n", result.NarrativeID)

		for _, a := range result.Proposed {
			fmt.Fprintf(&b, "  propose %s", a.Type)
			if a.IssueKey != "" {
				fmt.Fprintf(&b, " on %s", a.IssueKey)
			}
			fmt.Fprintf(&b, " (confidence %.2g)\n", a.Confidence)
			if a.Body != "" {
				fmt.Fprintf(&b, "    %s\n", firstLine(a.Body))
			}
			if a.TargetStatus != "" {
				fmt.Fprintf(&b, "    -> %s\n", a.TargetStatus)
			}
		}

		if result.SkippedNoDelta {
			b.WriteString("  no new events since the last proposal\n")
		}

		for _, key := range result.Unverified {
			fmt.Fprintf(&b, "  unverified: %s (not found on the tracker)\n", key)
		}

		for _, reason := range result.Suppressed {
			fmt.Fprintf(&b, "  suppressed: %s\n", reason)
		}

		for _, note := range result.LowConfidence {
			fmt.Fprintf(&b, "  low confidence: %s\n", note)
		}
	}

	return b.String()
}

// firstLine returns s up to its first newline, so a multi-paragraph comment
// body renders as one scannable line.
func firstLine(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		return s[:idx] + " …"
	}

	return s
}
```

- [ ] **Step 5: Run tests**

Run: `env -u GOROOT go test ./internal/pipeline/ -v 2>&1 | tail -30`

Expected: PASS, no pre-existing pipeline test regresses.

- [ ] **Step 6: Commit**

```bash
git add internal/pipeline/
git commit -m "pipeline: RunReconcile and its renderer

Every narrative renders, including ones that proposed nothing: an unchanged
narrative, an unresolvable link, and a suppressed duplicate are all legitimate
outcomes, and omitting them makes 'nothing proposed' indistinguishable from
'nothing considered'."
```

---

## Task 9: wire `dev narrate` to reconcile

**Files:**
- Modify: `cmd/unjira/main.go`

- [ ] **Step 1: Extend `devNarrateCmd.Run`**

`dev narrate` currently runs collect → narrate → match. Add reconcile after matching. In
`cmd/unjira/main.go`, first add the config validation next to the existing `Correlator.Validate()`
call, so a bad reconciler config fails before any collector work:

```go
	if err := app.config.Reconciler.Validate(); err != nil {
		return err
	}
```

Then, in the `--dry-run` early-return branch, extend the existing message so the operator knows both
stages were skipped:

```go
	if c.DryRun {
		// Matching writes narrative_issues rows and may set
		// narratives.issue_key, so it has no dry-run mode: skipping is
		// stated rather than silent, or the operator is left wondering why
		// nothing matched. Reconcile is skipped with it, since it has nothing
		// to work from until matching has linked something.
		fmt.Println("matching skipped (--dry-run)")
		fmt.Println("reconcile skipped (--dry-run)")

		return nil
	}
```

Finally, after the existing `matchErr` handling and before `return nil`, add:

```go
	// Reconcile reuses the tracker matching resolved. Task #130's
	// single-tracker limitation applies identically here: a link whose
	// Connection differs from this one is verified against the wrong site.
	reconcileResult, reconcileErr := pipeline.RunReconcile(
		ctx, app.store, tracker, client, app.config, pipeline.ReconcileOptions{})

	// Render before returning the error, matching the matching stage above:
	// Reconcile isolates failures per narrative, so healthy narratives produced
	// proposals the operator should see alongside whatever failed.
	fmt.Print(pipeline.RenderReconcileResult(reconcileResult))

	if reconcileErr != nil {
		return fmt.Errorf("reconciling narratives: %w", reconcileErr)
	}

	return nil
```

Note the existing code returns early on `matchErr`. Keep that: reconcile depends on matching's output,
so running it after a failed matching pass would reconcile against a half-updated link set.

- [ ] **Step 2: Verify the CLI builds and the command still works**

```sh
env -u GOROOT go build ./... && env -u GOROOT go run ./cmd/unjira dev narrate --help
```

Expected: build succeeds; help text lists `--since` and `--dry-run`.

- [ ] **Step 3: Run the cmd tests**

Run: `env -u GOROOT go test ./cmd/... -v 2>&1 | tail -20`

Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add cmd/unjira/main.go
git commit -m "cmd: run reconcile at the end of dev narrate

Reconcile runs only after a clean matching pass, since it reconciles against
matching's link set; --dry-run skips both and says so."
```

---

## Task 10: break-it drills

Four behaviours whose failure mode is silence rather than an error. For each: make the change, confirm
the named test **fails**, then restore. Nothing is committed by this task — its output is a written
record.

- [ ] **Drill 1: stop filtering `authored_by_unjira`**

In `internal/reconciler/reconciler.go`, make `dropSelfAuthored` return `evts` unchanged.

Run: `env -u GOROOT go test ./internal/reconciler/ -run TestReconcile -v`

Expected: a test covering self-authored deltas FAILS. If **nothing** fails, the drill has found a real
coverage gap — the loop-stability guard is unprotected. Add this test, then re-run the drill:

```go
func TestReconcileIgnoresADeltaOfOnlyItsOwnComments(t *testing.T) {
	s := reconcileStore(t)
	tracker := &fakeTracker{
		issues: map[string]tasktracker.Issue{
			"PROJ-1": {Key: "PROJ-1", Summary: "the ticket"},
		},
	}

	// The only new event is a comment unjira itself posted.
	nid := seedLinkedNarrative(t, s, "PROJ-1", unjiraComment("c1"))
	_ = nid

	llm := &fakeLLM{}
	results, _, err := Reconcile(t.Context(), s, tracker, llm, testConfig())
	require.NoError(t, err)

	require.Len(t, results, 1)
	assert.True(t, results[0].SkippedNoDelta)
	assert.Empty(t, llm.prompts,
		"without this filter unjira comments, collects its own comment, and proposes "+
			"commenting about it — an unstable loop that never errors, only compounds")
}
```

Restore `dropSelfAuthored`. Confirm green.

- [ ] **Drill 2: let model confidence through unfloored**

In `internal/reconciler/draft.go`, make `floorConfidence` return `action.Confidence` directly.

Run: `env -u GOROOT go test ./internal/reconciler/ -run TestDraftFloorsConfidence -v`

Expected: `TestDraftFloorsConfidenceForAnIllegalTransition` FAILS (confidence 0.95, not 0).

Restore. Confirm green.

- [ ] **Drill 3: ignore the empty delta**

In `internal/reconciler/reconciler.go`, delete the `if len(delta) == 0` early return.

Run: `env -u GOROOT go test ./internal/reconciler/ -run TestReconcileSkipsWhenTheDeltaIsEmpty -v`

Expected: FAILS — `SkippedNoDelta` is false and `llm.prompts` is non-empty, i.e. a re-run would
propose the same comment again.

Restore. Confirm green.

- [ ] **Drill 4: skip verification before drafting**

In `internal/reconciler/reconciler.go`, make `reconcileOne` build `verified` directly from
`actionable` without calling `verifyLinks`.

Run: `env -u GOROOT go test ./internal/reconciler/ -run TestReconcileDropsAnUnverifiableLink -v`

Expected: FAILS — a nonexistent `PROJ-404` receives a drafted action.

Restore. Confirm green: `env -u GOROOT go test ./internal/reconciler/`

- [ ] **Step 5: Record the results**

Append a short section to `docs/superpowers/specs/2026-08-25-reconciler-design.md` under Testing,
recording for each drill: what was broken, which test failed, and the assertion message. If any drill
failed to produce a failure, say so explicitly — that is the finding, not a footnote.

```bash
git add docs/superpowers/specs/2026-08-25-reconciler-design.md internal/reconciler/
git commit -m "reconciler: record break-it drill results

Four guards whose failure mode is silence rather than an error, each confirmed
to be covered by a test that fails when the guard is removed."
```

---

## Task 11: live-tier transition gating

**Files:**
- Modify: `internal/live/jira_test.go`

- [ ] **Step 1: Write the test**

Append to `internal/live/jira_test.go`. This is the one thing no fixture can establish: that the real
Jira workflow actually withholds a category, so the reconciler's flooring has teeth.

```go
// TestLiveAvailableStatusCategoriesReflectsTheRealWorkflow is why
// AvailableStatusCategories exists. The offline fakes assert what we BELIEVE
// Jira reports as legal; only a live call establishes that a freshly-created
// issue genuinely cannot reach every category, which is what makes
// floorConfidence's transition check meaningful rather than vacuous.
//
// Asserted as a property, not against a hardcoded category list: a Jira
// project's workflow is admin-configurable, so pinning "a new Task cannot go
// straight to Done" would be pinning this instance's configuration rather than
// the behaviour under test.
func TestLiveAvailableStatusCategoriesReflectsTheRealWorkflow(t *testing.T) {
	client := testClient(t)
	tracker := jira.NewTracker(client)

	key, err := client.CreateIssue(
		testProject(),
		"[seed] live transition gating probe",
		"Task",
		"Created by internal/live; deleted by this test's cleanup.",
		[]string{jira.SeedLabel},
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.DeleteIssue(key) })

	categories, err := tracker.AvailableStatusCategories(key)
	require.NoError(t, err)

	require.NotEmpty(t, categories,
		"a new issue must offer at least one legal transition, or every transition proposal "+
			"would be floored to zero and this check would be vacuous")

	// Cross-check against the raw API: every category reported must trace back
	// to a real transition Jira offers. This is what catches a normalization
	// bug that invented a category.
	raw, err := client.GetTransitions(key)
	require.NoError(t, err)

	rawCategories := make(map[string]bool, len(raw))
	for _, transition := range raw {
		to, ok := transition["to"].(map[string]any)
		require.True(t, ok, "a transition with no 'to' object: %v", transition)
		category, ok := to["statusCategory"].(map[string]any)
		if !ok {
			continue
		}
		categoryKey, _ := category["key"].(string)
		rawCategories[categoryKey] = true
	}

	require.NotEmpty(t, rawCategories,
		"GetTransitions returned transitions but none carried a statusCategory — the shape "+
			"AvailableStatusCategories depends on has changed")

	for _, c := range categories {
		assert.Contains(t, []tasktracker.StatusCategory{
			tasktracker.StatusTodo, tasktracker.StatusInProgress, tasktracker.StatusDone,
		}, c, "normalization produced a category outside the closed set")
	}

	assert.LessOrEqual(t, len(categories), len(rawCategories),
		"categories are deduplicated from raw transitions, so there cannot be more of them")
}
```

Add `"github.com/jcogilvie/unjira/internal/tasktracker"` to that file's imports.

- [ ] **Step 2: Confirm it compiles and skips without credentials**

```sh
env -u GOROOT go vet -tags=live ./internal/live/
env -u GOROOT go test -tags=live ./internal/live/ -run TestLiveAvailableStatusCategories -v
```

Expected: vet clean; the test SKIPS (no `UNJIRA_LIVE=1`).

- [ ] **Step 3: Run against the dev instance**

```sh
UNJIRA_LIVE=1 env -u GOROOT go test -tags=live ./internal/live/ -run TestLiveAvailableStatusCategories -v
```

Expected: PASS, creating and deleting one issue in the sandbox project.

If credentials are unavailable, say so and leave the test in place — it is gated to skip. Do **not**
weaken an assertion to make an unrun test look passing.

**Cleanup discipline:** the `t.Cleanup` above deletes exactly the key this test created. Never write
query-driven cleanup ("delete everything matching this JQL") against the shared sandbox — it can
delete issues this test did not create.

- [ ] **Step 4: Commit**

```bash
git add internal/live/jira_test.go
git commit -m "live: assert AvailableStatusCategories matches the real workflow

Offline fakes encode what we believe Jira reports as legal. Only a live call
establishes that a real issue's legal categories are a proper subset of all
three — which is what makes the reconciler's transition flooring meaningful
rather than vacuous."
```

---

## Task 12: full regression, lint, and spec status

**Files:**
- Modify: `docs/superpowers/specs/2026-08-25-reconciler-design.md`
- Modify: `docs/superpowers/specs/2026-08-11-phase1-correlator-design.md`

- [ ] **Step 1: Full offline suite**

```sh
env -u GOROOT go test ./... 2>&1 | grep -Ev '^ok|no test files'
```

Expected: no output. Any failure is a regression to fix, not to explain away.

- [ ] **Step 2: Lint**

```sh
env -u GOROOT golangci-lint run ./...
```

Expected: 0 issues. If the binary itself fails with a toolchain mismatch, rebuild it:
`env -u GOROOT go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest`.

- [ ] **Step 3: The authoritative gate**

```sh
earthly +reviewable
```

This is the only gate that matters before a PR (`CLAUDE.md`). It runs lint + test in a controlled
container, so it is also immune to the local `GOROOT` problem.

- [ ] **Step 4: Verify the pipeline end to end by hand**

Slice 6's `triage` does not exist, so there is no way to *view* the queue yet — read the table
directly:

```sh
env -u GOROOT go run ./cmd/unjira dev narrate --since 48h
sqlite3 data/unjira.db 'SELECT id, narrative_id, type, issue_key, confidence, status FROM actions ORDER BY id;'
```

Expected: proposed rows, all with `status=proposed`, and **no** writes to Jira. Confirm the second run
proposes nothing new:

```sh
env -u GOROOT go run ./cmd/unjira dev narrate --since 48h
```

Expected: narratives report "no new events since the last proposal". This is the duplicate-suppression
guarantee working against real data — the single most valuable manual check in this slice.

- [ ] **Step 5: Mark the slice landed**

In `docs/superpowers/specs/2026-08-25-reconciler-design.md`, add a status note recording what landed
and the three deviations from the spec as written:

1. `linked_at` and `actions.created_at` use sub-second (`%f`) timestamps; the spec's delta was not
   implementable at whole-second granularity.
2. `tasktracker.AvailableStatusCategories` was added — the spec assumed live transition legality was
   already reachable through the interface; it was not.
3. `store.NarrativesWithIssueKey` was added as the reconciler's input backlog.

In `docs/superpowers/specs/2026-08-11-phase1-correlator-design.md`, mark slice 4 landed, following how
slices 1–3 are annotated.

- [ ] **Step 6: Commit and open the PR**

```bash
git add docs/superpowers/specs/
git commit -m "docs: mark phase-1 slice 4 (internal/reconciler) landed

Records three deviations from the spec as written: sub-second timestamps (the
delta was not implementable at second granularity), the new
tasktracker.AvailableStatusCategories seam, and store.NarrativesWithIssueKey."

git push -u origin worktree-reconciler
gh pr create --title "phase-1 slice 4: internal/reconciler" --body "$(cat <<'BODY'
## Summary

Adds `internal/reconciler`: turns each touched narrative into a proposed
`comment` / `transition` / `create` action in the `actions` table with
`status=proposed`. Proposes only — no Jira writes.

Implements `docs/superpowers/specs/2026-08-25-reconciler-design.md`.

## Three deviations from the spec

- **Sub-second timestamps.** The spec's delta (`linked_at > last action's
  created_at`) is not implementable at the whole-second granularity every other
  table uses: an event linked in the same second as the action is invisible
  forever. Both columns now use `%f`, and must match — mixing the formats
  inverts the lexical TEXT comparison.
- **`tasktracker.AvailableStatusCategories`.** The spec assumed live transition
  legality was reachable through the tracker interface. `GetTransitions` was
  only on `*jira.Client`, with no local-backend equivalent.
- **`store.NarrativesWithIssueKey`.** The reconciler's input backlog, the
  complement of matching's `NarrativesWithoutIssueKey`.

## Verification

- `earthly +reviewable` green
- Four break-it drills confirmed each silent-failure guard is test-covered
- Live tier asserts the real Jira workflow withholds at least one category
- Manual: a second `dev narrate` proposes nothing new

## Known consequence

Nothing can *view* the queue until slice 6's `triage`; end-to-end verification is
`dev narrate` output plus reading the `actions` table.
BODY
)"
```

Then open it in the browser:

```sh
open "$(gh pr view --json url -q .url)"
```

---

## Notes for the implementer

**Dependency order.** Tasks 1→2 are the same file and should probably be one commit. Task 3 is
independent of 1–2 and can run in parallel. Tasks 5–7 are strictly sequential (5 stubs `draft`, 6
implements it, 7 persists its output). Task 8 needs 4–7. Tasks 9–12 are sequential after 8.

**Do not weaken a pre-existing test to make a change pass.** If one fails, the change is wrong. If you
believe the test encodes wrong behaviour, stop and say so rather than editing it — that judgment call
belongs to the human.

**Things this slice deliberately does not do** (from the spec — do not add them):
- Apply anything. No Jira writes.
- The auto-commit gate (`config.AutoCommit`) — slice 5.
- `triage`, the rework loop, `rules.Distill` — slices 6–7. `feedback` stays unwritten.
- `estimate` actions — `tasktracker` has no method for them.
- Honour `rules/review-staleness.md` / `rules/bot-pr-noise.md` — both are GitHub-PR-shaped and there
  is no GitHub collector, so their data is not in the event stream.
- Wire `scope: reconciler` rules into the drafting prompt — task #119, a follow-up now that the rules
  loader has landed.
