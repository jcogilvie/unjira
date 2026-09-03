package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jcogilvie/unjira/internal/events"
)

// ErrNarrativeNotFound is returned by GetNarrative when no row matches.
var ErrNarrativeNotFound = errors.New("narrative not found")

// -- narratives ----------------------------------------------------------

// NarrativeRow mirrors a narratives table row. correlator.Persist maps this
// to/from its own correlator.Narrative domain type (keeping store free of any
// correlator import — the dependency runs correlator -> store).
type NarrativeRow struct {
	ID                 int64
	WindowStart        time.Time
	WindowEnd          time.Time
	Title              string
	Summary            string
	IssueKey           string
	Confidence         float64
	Status             string
	CompactionBoundary *time.Time
	// CompactionBoundaryEventID pairs with CompactionBoundary to break ties:
	// occurred_at alone cannot uniquely order events (stored via
	// time.RFC3339 — whole seconds only), so NarrativeEventsForContext
	// filters on the (occurred_at, event_id) pair rather than occurred_at
	// alone. nil iff CompactionBoundary is nil (never compacted).
	CompactionBoundaryEventID *int64
}

// InsertNarrative inserts a new narrative row (status 'open', no compaction
// boundary) and returns its id.
func (s *Store) InsertNarrative(windowStart, windowEnd time.Time, title, summary string) (int64, error) {
	return insertNarrativeImpl(s.db, windowStart, windowEnd, title, summary)
}

// InsertNarrative is the *Tx-scoped variant of (*Store).InsertNarrative.
func (t *Tx) InsertNarrative(windowStart, windowEnd time.Time, title, summary string) (int64, error) {
	return insertNarrativeImpl(t.tx, windowStart, windowEnd, title, summary)
}

func insertNarrativeImpl(c dbConn, windowStart, windowEnd time.Time, title, summary string) (int64, error) {
	res, err := c.Exec(
		`INSERT INTO narratives (window_start, window_end, title, summary) VALUES (?, ?, ?, ?)`,
		windowStart.Format(time.RFC3339), windowEnd.Format(time.RFC3339), title, summary,
	)
	if err != nil {
		return 0, fmt.Errorf("inserting narrative %q: %w", title, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("getting inserted narrative id for %q: %w", title, err)
	}

	return id, nil
}

// GetNarrative returns the narrative with the given id, or
// ErrNarrativeNotFound.
func (s *Store) GetNarrative(id int64) (NarrativeRow, error) {
	return getNarrativeImpl(s.db, id)
}

// GetNarrative is the *Tx-scoped variant of (*Store).GetNarrative.
func (t *Tx) GetNarrative(id int64) (NarrativeRow, error) {
	return getNarrativeImpl(t.tx, id)
}

func getNarrativeImpl(c dbConn, id int64) (NarrativeRow, error) {
	row, err := scanNarrativeRow(c.QueryRow(
		`SELECT id, window_start, window_end, title, summary, issue_key, confidence, status,
		        compaction_boundary, compaction_boundary_event_id
		 FROM narratives WHERE id = ?`, id,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return NarrativeRow{}, ErrNarrativeNotFound
	}
	if err != nil {
		return NarrativeRow{}, fmt.Errorf("querying narrative %d: %w", id, err)
	}

	return row, nil
}

// scanNarrativeRow scans one narratives row (in the column order
// id, window_start, window_end, title, summary, issue_key, confidence,
// status, compaction_boundary, compaction_boundary_event_id) and parses its
// RFC3339 timestamps. Shared by GetNarrative (single row, via *sql.Row) and
// NarrativesOverlapping (many, via *sql.Rows) so the nullable-column handling
// exists once. Takes scanRow rather than a concrete type for exactly that
// reason — both *sql.Row and *sql.Rows satisfy it, mirroring scanEvent.
func scanNarrativeRow(row scanRow) (NarrativeRow, error) {
	var (
		out                NarrativeRow
		windowStart        string
		windowEnd          string
		issueKey           sql.NullString
		confidence         sql.NullFloat64
		compactionBoundary sql.NullString
		compactionEventID  sql.NullInt64
	)

	if err := row.Scan(&out.ID, &windowStart, &windowEnd, &out.Title, &out.Summary,
		&issueKey, &confidence, &out.Status, &compactionBoundary, &compactionEventID); err != nil {
		return NarrativeRow{}, err
	}

	var err error
	if out.WindowStart, err = time.Parse(time.RFC3339, windowStart); err != nil {
		return NarrativeRow{}, fmt.Errorf("parsing window_start for narrative %d: %w", out.ID, err)
	}
	if out.WindowEnd, err = time.Parse(time.RFC3339, windowEnd); err != nil {
		return NarrativeRow{}, fmt.Errorf("parsing window_end for narrative %d: %w", out.ID, err)
	}

	out.IssueKey = issueKey.String
	if confidence.Valid {
		out.Confidence = confidence.Float64
	}
	if compactionBoundary.Valid {
		parsed, perr := time.Parse(time.RFC3339, compactionBoundary.String)
		if perr != nil {
			return NarrativeRow{}, fmt.Errorf("parsing compaction_boundary for narrative %d: %w", out.ID, perr)
		}
		out.CompactionBoundary = &parsed
	}
	if compactionEventID.Valid {
		id := compactionEventID.Int64
		out.CompactionBoundaryEventID = &id
	}

	return out, nil
}

// ExtendNarrative advances a narrative's window_end and overwrites its
// summary.
func (s *Store) ExtendNarrative(id int64, windowEnd time.Time, summary string) error {
	return extendNarrativeImpl(s.db, id, windowEnd, summary)
}

// ExtendNarrative is the *Tx-scoped variant of (*Store).ExtendNarrative.
func (t *Tx) ExtendNarrative(id int64, windowEnd time.Time, summary string) error {
	return extendNarrativeImpl(t.tx, id, windowEnd, summary)
}

func extendNarrativeImpl(c dbConn, id int64, windowEnd time.Time, summary string) error {
	_, err := c.Exec(
		`UPDATE narratives SET window_end = ?, summary = ? WHERE id = ?`,
		windowEnd.Format(time.RFC3339), summary, id,
	)
	if err != nil {
		return fmt.Errorf("extending narrative %d: %w", id, err)
	}

	return nil
}

// SetCompactionBoundary records the occurred_at and row id of the newest
// compacted event and stores the recap-prefixed summary. boundaryEventID is
// required alongside boundary: occurred_at alone cannot uniquely order
// events sharing a stored second (time.RFC3339 truncates to whole seconds —
// see the comment on NarrativeEventsForContext), so the pair is what
// NarrativeEventsForContext's row-value comparison uses to avoid dropping a
// tied event from future context.
func (s *Store) SetCompactionBoundary(id int64, boundary time.Time, boundaryEventID int64, recapSummary string) error {
	return setCompactionBoundaryImpl(s.db, id, boundary, boundaryEventID, recapSummary)
}

// SetCompactionBoundary is the *Tx-scoped variant of
// (*Store).SetCompactionBoundary.
func (t *Tx) SetCompactionBoundary(id int64, boundary time.Time, boundaryEventID int64, recapSummary string) error {
	return setCompactionBoundaryImpl(t.tx, id, boundary, boundaryEventID, recapSummary)
}

func setCompactionBoundaryImpl(c dbConn, id int64, boundary time.Time, boundaryEventID int64, recapSummary string) error {
	_, err := c.Exec(
		`UPDATE narratives SET compaction_boundary = ?, compaction_boundary_event_id = ?, summary = ? WHERE id = ?`,
		boundary.Format(time.RFC3339), boundaryEventID, recapSummary, id,
	)
	if err != nil {
		return fmt.Errorf("setting compaction boundary for narrative %d: %w", id, err)
	}

	return nil
}

// AddNarrativeEvents links events to a narrative (INSERT OR IGNORE, so
// re-linking an already-linked event is a harmless no-op).
func (s *Store) AddNarrativeEvents(narrativeID int64, eventIDs []int64) error {
	return addNarrativeEventsImpl(s.db, narrativeID, eventIDs)
}

// AddNarrativeEvents is the *Tx-scoped variant of
// (*Store).AddNarrativeEvents.
func (t *Tx) AddNarrativeEvents(narrativeID int64, eventIDs []int64) error {
	return addNarrativeEventsImpl(t.tx, narrativeID, eventIDs)
}

func addNarrativeEventsImpl(c dbConn, narrativeID int64, eventIDs []int64) error {
	for _, eid := range eventIDs {
		if _, err := c.Exec(
			`INSERT OR IGNORE INTO narrative_events (narrative_id, event_id) VALUES (?, ?)`,
			narrativeID, eid,
		); err != nil {
			return fmt.Errorf("linking event %d to narrative %d: %w", eid, narrativeID, err)
		}
	}

	return nil
}

// NarrativeEventsForContext returns a narrative's events strictly after its
// compaction boundary (all of them when the boundary is NULL), ordered by
// (occurred_at, event id) — the events the caller hydrates into
// correlator.Narrative.Events. The recap of anything at/before the boundary
// already lives in the summary.
//
// The boundary comparison is on the pair (compaction_boundary,
// compaction_boundary_event_id), not occurred_at alone: occurred_at is
// stored via time.RFC3339 (whole seconds only — see InsertEvent), so two
// events in the same second are indistinguishable by timestamp. A bare
// "occurred_at > boundary" filter would then either include or exclude
// *both* tied events depending on which one the boundary happened to be set
// from, silently dropping whichever tied event was meant to stay visible.
// events.id is a monotonic INTEGER PRIMARY KEY, so pairing it with
// occurred_at makes the ordering exact regardless of timestamp collisions
// (this is also why compaction picks its boundary event's row id, not just
// its timestamp — see correlator.compactNarrativeTail).
//
// Uses a SQL row-value comparison ("(a, b) > (x, y)"), verified against
// modernc.org/sqlite (the driver this package uses) with a standalone
// scratch program before relying on it here; modernc.org/sqlite is well
// past the SQLite 3.15 baseline that introduced row values. The explicit
// equivalent ("a > x OR (a = x AND b > y)") is the documented fallback if a
// future driver swap ever regresses this.
//
// A narrative id with no matching row (or one with no linked events)
// returns (nil, nil), not an error — callers only invoke this with an id
// they already obtained from the store.
func (s *Store) NarrativeEventsForContext(narrativeID int64) ([]events.Event, error) {
	rows, err := s.db.Query(
		`SELECT e.source, e.external_id, e.occurred_at, e.actor, e.summary, e.artifacts, e.raw_ref
		 FROM events e
		 JOIN narrative_events ne ON ne.event_id = e.id
		 WHERE ne.narrative_id = ?
		   AND (
		     (SELECT compaction_boundary FROM narratives WHERE id = ?) IS NULL
		     OR (e.occurred_at, e.id) > (
		       (SELECT compaction_boundary FROM narratives WHERE id = ?),
		       (SELECT compaction_boundary_event_id FROM narratives WHERE id = ?)
		     )
		   )
		 ORDER BY e.occurred_at, e.id`,
		narrativeID, narrativeID, narrativeID, narrativeID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying context events for narrative %d: %w", narrativeID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []events.Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning context event row: %w", err)
		}
		out = append(out, e)
	}

	return out, rows.Err()
}

// UnlinkedEventsInRange returns events in [start, end) that are not yet
// linked to any narrative, ordered by (occurred_at, id) — the clustering
// candidates a narration pass considers.
//
// "Unlinked" means no narrative_events row at all, not "linked to a narrative
// outside this range": an event belongs to exactly one narrative, so once
// linked it is never a candidate again. Such an event can still reach a
// prompt as context via its narrative's hydration
// (NarrativeEventsForContext), which is why excluding it here does not starve
// the model.
//
// Ordering is composite because occurred_at is stored via time.RFC3339
// (whole seconds — see InsertEvent) and cannot uniquely order events.
func (s *Store) UnlinkedEventsInRange(start, end time.Time) ([]events.Event, error) {
	rows, err := s.db.Query(
		`SELECT e.source, e.external_id, e.occurred_at, e.actor, e.summary, e.artifacts, e.raw_ref
		 FROM events e
		 WHERE e.occurred_at >= ? AND e.occurred_at < ?
		   AND NOT EXISTS (SELECT 1 FROM narrative_events ne WHERE ne.event_id = e.id)
		 ORDER BY e.occurred_at, e.id`,
		start.Format(time.RFC3339), end.Format(time.RFC3339),
	)
	if err != nil {
		return nil, fmt.Errorf("querying unlinked events in [%s, %s): %w",
			start.Format(time.RFC3339), end.Format(time.RFC3339), err)
	}
	defer func() { _ = rows.Close() }()

	var out []events.Event
	for rows.Next() {
		event, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning unlinked event row: %w", err)
		}
		out = append(out, event)
	}

	return out, rows.Err()
}

// NarrativesOverlapping returns narratives whose window overlaps or merely
// touches [start, end), ordered by (window_start, id) — the context a
// narration pass passes to Cluster.
//
// The predicate mirrors correlator's own adjacency filter: keep unless
// strictly disjoint (window_end < start || window_start > end). Touching
// endpoints count as adjacent deliberately — temporal proximity is real
// clustering signal, so a narrative ending exactly when this window opens is
// the most likely thing an early event extends.
//
// status IS filtered now, excluding StatusSplit. This is the change the previous
// version of this comment anticipated ("when status becomes meaningful, the
// predicate belongs here") — triage's [s]plit moves every event off a narrative
// and marks it split, and without this predicate that emptied row keeps being
// returned here. Probed before adding it: after an all-new split the source held
// 0 events and was still selected.
//
// The consequence of not filtering is not cosmetic. buildClusterPrompt renders
// each of these as an existing narrative under "CONTEXT ONLY", so the model would
// be shown a titled narrative with no events and asked whether the numbered events
// extend it — reasoning about a story whose substance moved elsewhere. Every
// subsequent pass would carry the same dead weight.
//
// Only 'split' is excluded, not "anything that is not open": a status this query
// has never seen should not silently drop a narrative from clustering. An unknown
// value is more likely a new lifecycle state than a reason to hide work.
func (s *Store) NarrativesOverlapping(start, end time.Time) ([]NarrativeRow, error) {
	rows, err := s.db.Query(
		`SELECT id, window_start, window_end, title, summary, issue_key, confidence, status,
		        compaction_boundary, compaction_boundary_event_id
		 FROM narratives
		 WHERE window_end >= ? AND window_start <= ?
		   AND status != '`+StatusSplit+`'
		 ORDER BY window_start, id`,
		start.Format(time.RFC3339), end.Format(time.RFC3339),
	)
	if err != nil {
		return nil, fmt.Errorf("querying narratives overlapping [%s, %s): %w",
			start.Format(time.RFC3339), end.Format(time.RFC3339), err)
	}
	defer func() { _ = rows.Close() }()

	var out []NarrativeRow
	for rows.Next() {
		row, err := scanNarrativeRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning overlapping narrative row: %w", err)
		}
		out = append(out, row)
	}

	return out, rows.Err()
}

// AllNarrativeEvents returns every event ever linked to a narrative,
// ignoring the compaction boundary — deliberately NOT
// NarrativeEventsForContext, which exists to hide pre-boundary events from
// the model once their content is captured in the recap summary. Matching
// candidate keys come from event artifacts, and the git_branch artifact
// carrying the strongest provenance signal typically sits on a narrative's
// oldest events — exactly the ones compaction hides. A matching pass that
// used NarrativeEventsForContext would silently lose that signal for any
// narrative old enough to have been compacted.
func (s *Store) AllNarrativeEvents(narrativeID int64) ([]events.Event, error) {
	rows, err := s.db.Query(
		`SELECT e.source, e.external_id, e.occurred_at, e.actor, e.summary, e.artifacts, e.raw_ref
		 FROM events e
		 JOIN narrative_events ne ON ne.event_id = e.id
		 WHERE ne.narrative_id = ?
		 ORDER BY e.occurred_at, e.id`,
		narrativeID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying all events for narrative %d: %w", narrativeID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []events.Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning narrative event row: %w", err)
		}
		out = append(out, e)
	}

	return out, rows.Err()
}

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
	defer func() { _ = rows.Close() }()

	var out []events.Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning delta event row: %w", err)
		}
		out = append(out, e)
	}

	return out, rows.Err()
}

// NarrativeEventLinkedAt returns the linked_at timestamp for one
// (narrativeID, eventID) narrative_events row. This is a test-support
// introspection accessor — same precedent as NarrativeEventCount — so tests
// outside this package (store_test) can assert on linked_at's format without
// reaching into *sql.DB directly.
func (s *Store) NarrativeEventLinkedAt(narrativeID, eventID int64) (string, error) {
	var linkedAt string
	if err := s.db.QueryRow(
		`SELECT linked_at FROM narrative_events WHERE narrative_id = ? AND event_id = ?`,
		narrativeID, eventID,
	).Scan(&linkedAt); err != nil {
		return "", fmt.Errorf("querying linked_at for narrative %d event %d: %w", narrativeID, eventID, err)
	}

	return linkedAt, nil
}

// NarrativeEventCount returns how many events are linked to a narrative,
// ignoring its compaction boundary — unlike NarrativeEventsForContext, which
// returns only the post-boundary tail. This is a test-support introspection
// accessor: it's what lets a test prove the "narrative_events rows are never
// deleted" invariant, since compaction shrinks the assembled context, never
// the links.
func (s *Store) NarrativeEventCount(narrativeID int64) (int, error) {
	var count int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM narrative_events WHERE narrative_id = ?`, narrativeID,
	).Scan(&count); err != nil {
		return 0, fmt.Errorf("counting linked events for narrative %d: %w", narrativeID, err)
	}

	return count, nil
}
