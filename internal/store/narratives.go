package store

import (
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
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
	Status             string
	CompactionBoundary *time.Time
	// CompactionBoundaryEventID pairs with CompactionBoundary to break ties:
	// occurred_at alone cannot uniquely order events (events can share an
	// instant — see MemberEventsAfterBoundary), so MemberEventsAfterBoundary
	// filters on the (occurred_at, event_id) pair rather than occurred_at
	// alone. nil iff CompactionBoundary is nil (never compacted).
	CompactionBoundaryEventID *int64
}

// InsertNarrative inserts a new narrative row (status 'open', no compaction
// boundary) and returns its id.
func (s *Store) InsertNarrative(windowStart, windowEnd time.Time, title, summary string) (int64, error) {
	return insertNarrativeImpl(s.db, s.now(), windowStart, windowEnd, title, summary)
}

// InsertNarrative is the *Tx-scoped variant of (*Store).InsertNarrative.
func (t *Tx) InsertNarrative(windowStart, windowEnd time.Time, title, summary string) (int64, error) {
	return insertNarrativeImpl(t.tx, t.now(), windowStart, windowEnd, title, summary)
}

func insertNarrativeImpl(c dbConn, createdAt, windowStart, windowEnd time.Time, title, summary string) (int64, error) {
	res, err := c.Exec(
		`INSERT INTO narratives (window_start, window_end, title, summary, created_at) VALUES (?, ?, ?, ?, ?)`,
		windowStart, windowEnd, title, summary, createdAt,
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
		`SELECT id, window_start, window_end, title, summary, status,
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
// status, compaction_boundary, compaction_boundary_event_id), its times as
// time.Time. Shared by GetNarrative (single row, via *sql.Row) and
// NarrativesOverlapping (many, via *sql.Rows) so the nullable-column handling
// exists once. Takes scanRow rather than a concrete type for exactly that
// reason — both *sql.Row and *sql.Rows satisfy it, mirroring scanEvent.
func scanNarrativeRow(row scanRow) (NarrativeRow, error) {
	var (
		out                NarrativeRow
		compactionBoundary sql.NullTime
		compactionEventID  sql.NullInt64
	)

	if err := row.Scan(&out.ID, &out.WindowStart, &out.WindowEnd, &out.Title, &out.Summary,
		&out.Status, &compactionBoundary, &compactionEventID); err != nil {
		return NarrativeRow{}, err
	}

	if compactionBoundary.Valid {
		boundary := compactionBoundary.Time
		out.CompactionBoundary = &boundary
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
		windowEnd, summary, id,
	)
	if err != nil {
		return fmt.Errorf("extending narrative %d: %w", id, err)
	}

	return nil
}

// SetCompactionBoundary records the occurred_at and row id of the newest
// compacted event and stores the recap-prefixed summary. boundaryEventID is
// required alongside boundary: occurred_at alone cannot uniquely order
// events sharing an instant (see the comment on MemberEventsAfterBoundary),
// so the pair is what
// MemberEventsAfterBoundary's row-value comparison uses to avoid dropping a
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
		boundary, boundaryEventID, recapSummary, id,
	)
	if err != nil {
		return fmt.Errorf("setting compaction boundary for narrative %d: %w", id, err)
	}

	return nil
}

// MemberEventsAfterBoundary returns a narrative's MEMBER events strictly after its
// compaction boundary (all of them when the boundary is NULL), ordered by
// (occurred_at, event id) — the events the caller hydrates into
// correlator.Narrative.Events and EligibleEvents, and what compaction folds. The
// recap of anything at/before the boundary already lives in the summary.
//
// Members only, and renamed to say so: compaction folds member events only, so a
// recap never absorbs another stream's work (shared-context spec §2). Context links
// have their own accessor, ContextEventsAfterBoundary. (Links are written by
// Tx.MoveMember and Tx.AddContext, in linkkind.go.)
//
// The boundary comparison is on the pair (compaction_boundary,
// compaction_boundary_event_id), not occurred_at alone: occurred_at is
// stored at the precision its source gave, and a second-granular source
// (Jira's changelog; a bulk transition moves many issues in one second)
// gives many events one instant, indistinguishable by timestamp. A bare
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
// A narrative id with no matching row (or one with no member events)
// returns (nil, nil), not an error — callers only invoke this with an id
// they already obtained from the store.
func (s *Store) MemberEventsAfterBoundary(narrativeID int64) ([]events.Event, error) {
	return queryLinkedEvents(s.db, "post-boundary member events", narrativeID,
		memberLink+` AND `+afterCompactionBoundary)
}

// UnlinkedEventsInRange returns events in [start, end) that have no MEMBER link,
// ordered by (occurred_at, id) — the clustering candidates a narration pass
// considers.
//
// "No member link", not "no link at all", because that is the question a candidate
// answers — does this event have a home yet (design-notes #28: ask the question you
// mean). Every linked event has exactly one member home, so once placed an event is
// never a candidate again, and a context link elsewhere does not give it one. A
// placed event can still reach a prompt via its member narrative's hydration
// (MemberEventsAfterBoundary), which is why excluding it here does not starve the
// model.
//
// Ordering is composite because events can share an instant (see
// MemberEventsAfterBoundary), so occurred_at cannot uniquely order them.
func (s *Store) UnlinkedEventsInRange(start, end time.Time) ([]events.Event, error) {
	rows, err := s.db.Query(
		`SELECT e.source, e.external_id, e.occurred_at, e.actor, e.summary, e.artifacts, e.raw_ref
		 FROM events e
		 WHERE e.occurred_at >= ? AND e.occurred_at < ?
		   AND NOT EXISTS (SELECT 1 FROM narrative_events ne WHERE ne.event_id = e.id AND `+memberLink+`)
		 ORDER BY e.occurred_at, e.id`,
		start, end,
	)
	if err != nil {
		return nil, fmt.Errorf("querying unlinked events in [%s, %s): %w",
			start.Format(time.RFC3339Nano), end.Format(time.RFC3339Nano), err)
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
	return s.NarrativesOverlappingExtended(start, end, nil)
}

// NarrativesOverlappingExtended is NarrativesOverlapping answered as if each narrative
// in extendTo already had its window_end moved forward to the time given: never back,
// as ExtendNarrative's callers move it, and with window_start untouched. Each returned
// row carries the window_end it would have. It writes nothing, and an id that names no
// narrative is ignored.
//
// It exists so a narration pass decides its clustering context from one query whether
// or not it has written (F46's fix). The pull-request identity join extends the
// narratives it places events into before clustering, which can bring a narrative that
// ended before the window into it. A real pass has written that extension by now; a dry
// run has not, and passes the same planned ends here so both see the same set. Passing
// an end the store already holds changes nothing, which is what lets the real pass go
// through this query too, rather than through a second one that could drift.
//
// "Forward" is decided by instant, as the join's writer decides it with time.After:
// each planned end is bound as a time.Time, so it is the same UTC text as a stored
// window_end (see the package doc) and the SQL comparison between them is an instant
// comparison. The end that wins is compared with the window exactly as a stored one
// would be. The row's window_end is then moved forward in Go rather than selected
// from SQL, because a CASE over two times has no declared type and would come back
// as text rather than a time.Time.
func (s *Store) NarrativesOverlappingExtended(start, end time.Time, extendTo map[int64]time.Time) ([]NarrativeRow, error) {
	planned := `SELECT NULL, NULL WHERE 0`
	args := make([]any, 0, 2*len(extendTo)+2)
	if len(extendTo) > 0 {
		ids := slices.Sorted(maps.Keys(extendTo))
		values := make([]string, 0, len(ids))
		for _, id := range ids {
			values = append(values, `(?, ?)`)
			args = append(args, id, extendTo[id])
		}
		planned = `VALUES ` + strings.Join(values, `, `)
	}
	args = append(args, start, end)

	rows, err := s.db.Query(
		`WITH planned(id, window_end) AS (`+planned+`)
		 SELECT n.id, n.window_start, n.window_end, n.title, n.summary, n.status,
		        n.compaction_boundary, n.compaction_boundary_event_id
		 FROM narratives n LEFT JOIN planned p ON p.id = n.id
		 WHERE CASE WHEN p.window_end > n.window_end THEN p.window_end ELSE n.window_end END >= ?
		   AND n.window_start <= ?
		   AND n.status != '`+StatusSplit+`'
		 ORDER BY n.window_start, n.id`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("querying narratives overlapping [%s, %s): %w",
			start.Format(time.RFC3339Nano), end.Format(time.RFC3339Nano), err)
	}
	defer func() { _ = rows.Close() }()

	var out []NarrativeRow
	for rows.Next() {
		row, err := scanNarrativeRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning overlapping narrative row: %w", err)
		}
		// In UTC, as a stored end reads back; UTC also drops a monotonic reading.
		if at, ok := extendTo[row.ID]; ok && at.After(row.WindowEnd) {
			row.WindowEnd = at.UTC()
		}
		out = append(out, row)
	}

	return out, rows.Err()
}

// AllMemberEvents returns every MEMBER event of a narrative, ignoring the
// compaction boundary — deliberately NOT MemberEventsAfterBoundary, which
// exists to hide pre-boundary events from the model once their content is
// captured in the recap summary. Matching candidate keys come from event
// artifacts, and the git_branch artifact carrying the strongest provenance
// signal typically sits on a narrative's oldest events — exactly the ones
// compaction hides. A matching pass that used MemberEventsAfterBoundary would
// silently lose that signal for any narrative old enough to have been
// compacted.
//
// Members only, and RENAMED from AllNarrativeEvents rather than quietly
// re-scoped, so each caller failed to compile and was revisited (shared-context
// spec §2, incident 13). Its two callers are matching's gatherCandidates and the
// create path, and both must never see context: a shared root segment carries up
// to 37 scm_command keys, which would outrank jira_event for every narrative it
// supports, and a create must describe this narrative's work, not another's.
// Context events have their own accessor, ContextEvents, which neither calls.
func (s *Store) AllMemberEvents(narrativeID int64) ([]events.Event, error) {
	return queryLinkedEvents(s.db, "member events", narrativeID, memberLink)
}

// linkedSinceLastAction is the reconciler's delta test for one narrative_events row
// `ne`: was this link made after the narrative's most recent action, of ANY status?
//
// Shared verbatim by DeltaEvents (which events a pass drafts from) and
// hasUnexaminedDelta (which narratives a pass selects, and what the remainder
// counts), because those two drifting apart is F10's failure mode: a count describing
// a population the pass never examines. Correlated on ne.narrative_id, so it drops
// into either query whatever the outer query calls its narrative, and it carries no
// bound parameters.
//
// Compares link SEQUENCE positions, not timestamps (finding F30):
// actions.created_link_seq is the link high-water mark recorded when the action was
// inserted, so a link made after it has a strictly greater link_seq however little
// time passed. The old `linked_at > MAX(created_at)` compared two millisecond
// timestamps, and a link made in the same millisecond as the action was invisible
// forever — the failure the narrative_events schema comment once documented at
// whole-second resolution, recurring one tick finer.
//
// With no prior action the COALESCE gives 0, below every link_seq, so every link is
// delta.
//
// MEMBER links only (shared-context spec §2): the delta is "new work for this
// narrative", and a context link is somebody else's work. Admitting one would let
// suppressTrackerEcho be satisfied by another ticket's work evidence and
// suppressStaleTransitions date this ticket's work by another's — and, since this const
// is also hasUnexaminedDelta's, every context link would re-admit its narrative to the
// reconcile backlog at model cost. In this const and not at its two call sites, so the
// selector, the count and the delta cannot drift on it either.
const linkedSinceLastAction = memberLink + ` AND ne.link_seq > COALESCE(
	(SELECT MAX(a.created_link_seq) FROM actions a WHERE a.narrative_id = ne.narrative_id), 0)`

// DeltaEvents returns the events linked to narrativeID since the most recent
// action proposed for it — "what's new since a reviewer last saw this."
//
// Bounded by the last action's CREATION (not its decision or execution)
// deliberately: every action has one, so a proposal sitting unreviewed in the
// queue still suppresses re-proposing its delta. decided_at is NULL while
// unreviewed, which would make every pass re-propose the same thing; and a
// rejected action never gets an executed_at, so bounding on that would
// re-propose a rejected action identically forever, giving the reviewer's "no"
// no weight. See the design spec's comparison table.
//
// The predicate is linkedSinceLastAction, shared with hasUnexaminedDelta. With no
// prior action every linked event is returned, which is the first-pass case: the
// whole narrative is the delta.
func (s *Store) DeltaEvents(narrativeID int64) ([]events.Event, error) {
	rows, err := s.db.Query(
		`SELECT e.source, e.external_id, e.occurred_at, e.actor, e.summary, e.artifacts, e.raw_ref
		 FROM narrative_events ne
		 JOIN events e ON e.id = ne.event_id
		 WHERE ne.narrative_id = ?
		   AND `+linkedSinceLastAction+`
		 ORDER BY e.occurred_at, e.id`,
		narrativeID,
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
// outside this package (store_test) can assert on linked_at without reaching
// into *sql.DB directly.
func (s *Store) NarrativeEventLinkedAt(narrativeID, eventID int64) (time.Time, error) {
	var linkedAt time.Time
	if err := s.db.QueryRow(
		`SELECT linked_at FROM narrative_events WHERE narrative_id = ? AND event_id = ?`,
		narrativeID, eventID,
	).Scan(&linkedAt); err != nil {
		return time.Time{}, fmt.Errorf("querying linked_at for narrative %d event %d: %w", narrativeID, eventID, err)
	}

	return linkedAt, nil
}

// NarrativeEventCount returns how many events are linked to a narrative, of
// EITHER kind, ignoring its compaction boundary — unlike
// MemberEventsAfterBoundary, which returns only the post-boundary member tail.
// MemberEventCount counts members alone. This is a test-support introspection
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
