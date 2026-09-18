package store

import (
	"database/sql"
	"fmt"
	"strings"
)

// -- narrative issues (narrative -> issue matching) -----------------------

// Role is which relationship a narrative has to an issue. A closed set: an
// unrecognized value is a parse error upstream, never persisted.
type Role string

// RolePrimary is the one role this package names itself.
//
// The vocabulary is otherwise correlator's: NarrativesWithActionableLinks takes
// its roles as a PARAMETER precisely so "what is actionable" has one definition,
// in the caller, and this package need not import internal/correlator. That
// remains true.
//
// The exception is narrow and forced. "Has a primary" is a structural fact about
// the schema, not a policy choice: store.go's own partial unique index
// one_primary_per_narrative hardcodes `WHERE role = 'primary'`, so this package
// already depends on the value. Declaring it once here, and referencing it from
// the two backlog predicates, means the schema and the queries that rely on it
// cannot drift to different spellings. correlator.RolePrimary is an alias of
// store.Role, so the two are the same value by construction.
const RolePrimary Role = "primary"

// NarrativeIssue is one (narrative, issue) link — the narrative_issues row
// shape, mirroring how NarrativeRow mirrors narratives.
//
// There is deliberately no issue-to-issue column: every row relates a
// narrative to an issue, so co-representation of one body of work across two
// tickets is expressed by two rows sharing a narrative_id, and symmetry is
// derived rather than stored in two places that could disagree.
type NarrativeIssue struct {
	IssueKey string
	Role     Role
	// Provenance is a plain string rather than a typed enum: the typed
	// Provenance lives in internal/correlator, which this package must not
	// import, and a value read back from SQLite is untyped text regardless.
	Provenance string
	Confidence float64
	// Connection is the config.JiraConnection.Name that resolved this key,
	// recorded because a co-representation can live on a different site than
	// the primary.
	Connection string
}

// NarrativeIssueRef is one row of the reverse lookup: the caller already
// knows the issue key and needs the narrative.
type NarrativeIssueRef struct {
	NarrativeID int64
	Role        Role
	Confidence  float64
}

// NarrativesWithoutPrimaryLink returns up to limit narratives that have no
// primary link, ordered by (window_start, id) — the backlog a matching pass
// works through.
//
// The predicate asks the LINK TABLE, not the old denormalized
// narratives.issue_key column, and that distinction was finding F11. The column
// was only ever written for a primary that cleared match.confidence_floor, while
// persistLinks writes the link row regardless — deliberately, because
// config.MatchConfig.ConfidenceFloor governs "what unjira asserts, not what it
// records", and dropping the rows would make a low-confidence match
// indistinguishable from finding nothing at all.
//
// So a narrative with a REAL but low-confidence primary had link rows and a NULL
// column, and a column-based query returned it forever: every pass re-matched it,
// and a re-match that picked a DIFFERENT primary tripped the partial unique index
// one_primary_per_narrative and aborted the entire pass. That crashed a drain.
//
// A primary link at ANY confidence means attributed. Deliberately narrower than
// NarrativesWithNoIssueLink's "any link at all": a `mentioned` link is a citation,
// not an attribution, so a narrative carrying only those is still matching's work.
// Two questions, two predicates — see that method for why the create path needs
// the broader one.
func (s *Store) NarrativesWithoutPrimaryLink(limit int) ([]NarrativeRow, error) {
	rows, err := s.db.Query(
		`SELECT n.id, n.window_start, n.window_end, n.title, n.summary, n.status,
		        n.compaction_boundary, n.compaction_boundary_event_id
		 FROM narratives n
		 WHERE NOT EXISTS (
		     SELECT 1 FROM narrative_issues ni
		     WHERE ni.narrative_id = n.id AND ni.role = ?
		 )
		 `+matchExaminationPredicate+`
		 ORDER BY n.window_start, n.id
		 LIMIT ?`,
		string(RolePrimary), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("querying narratives without a primary link: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []NarrativeRow
	for rows.Next() {
		row, err := scanNarrativeRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning unmatched narrative row: %w", err)
		}
		out = append(out, row)
	}

	return out, rows.Err()
}

// CountNarrativesWithoutPrimaryLink is how many narratives matching would examine if
// it had no cap — the backlog depth behind correlator.Match's per-pass limit.
//
// EXISTS to make a truncated pass legible. Both per-pass caps already log when
// they truncate, but the logger writes to stderr while the rendered summary goes
// to stdout, so a bounded pass ends looking complete. A 42-narrative backlog was
// misdiagnosed as a clustering defect on exactly that basis, and the misdiagnosis
// survived three passes because the session piped output through `tail` and
// discarded the warning.
//
// The predicate MUST stay identical to NarrativesWithoutPrimaryLink's. A count that
// describes a different population than the pass examined is worse than no count:
// it would report progress against a backlog that was never the one being drained.
func (s *Store) CountNarrativesWithoutPrimaryLink() (int, error) {
	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM narratives n
		  WHERE NOT EXISTS (
		      SELECT 1 FROM narrative_issues ni
		      WHERE ni.narrative_id = n.id AND ni.role = ?
		  )
		  `+matchExaminationPredicate,
		string(RolePrimary),
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting narratives without a primary link: %w", err)
	}

	return n, nil
}

// hasUnexaminedDelta is the SQL predicate for "this narrative carries work the
// reconciler has not examined", shared verbatim by NarrativesWithActionableLinks
// (what a pass selects) and CountNarrativesWithDelta (what the remainder reports).
//
// A single constant rather than two hand-written copies, because those two
// diverging is a specific, already-experienced failure: a count that describes a
// different population than the pass examined reports progress against a backlog
// nobody is draining (F10), and F12 was that same defect a second time. The
// correlated `n.id` makes it a per-narrative test, so it drops into either query's
// WHERE clause unchanged.
//
// The bound MUST stay identical to DeltaEvents': MAX(created_at) over actions of
// ANY status, COALESCEd to ” so a narrative with no action has no watermark and
// every link counts. Any status is deliberate — a declined or suppressed row still
// means the narrative was examined and nothing new has happened since. Contrast
// EligibleEvents, which bounds on status='applied' only because it asks a different
// question ("what has no tracker mutation claimed yet").
//
// Also carries reconcileExaminationPredicate (finding F26): hasUnexaminedDelta alone
// cannot see that a narrative's surviving delta is entirely self-authored, because
// that filter is Go-side (reconciler.dropSelfAuthored), not SQL-side. Without it, a
// narrative in that shape passes this EXISTS check, gets selected, gets emptied, and
// hits SkippedNoDelta forever — the same livelock hasUnexaminedDelta itself was built
// to close, one layer up. Appending the predicate here rather than duplicating it at
// each of this const's two call sites keeps the two queries this const already
// unifies from also disagreeing about F26's exclusion.
const hasUnexaminedDelta = `EXISTS (
	              SELECT 1 FROM narrative_events ne
	              WHERE ne.narrative_id = n.id
	                AND ne.linked_at > COALESCE(
	                    (SELECT MAX(created_at) FROM actions WHERE narrative_id = n.id), '')
	          )` + reconcileExaminationPredicate

// CountNarrativesWithDelta is how many linked narratives a reconcile pass would
// actually ACT on — the honest backlog depth behind reconciler's per-pass cap.
//
// Two predicates, both required, because the reconciler applies both. The role
// filter is NarrativesWithActionableLinks' (what a pass SELECTS); the delta test is
// DeltaEvents' (what a pass does not skip). reconcileOne returns early with
// SkippedNoDelta when the delta is empty, so those narratives cost no model call
// and cannot produce an action.
//
// Counting the selection alone was finding F12: the remainder said 55 where 20 had
// nothing new, so it told an operator to re-run for work that did not exist, and
// each re-run bills for the sweep.
//
// The delta bound MUST stay identical to DeltaEvents': MAX(created_at) over actions
// of ANY status, COALESCEd to ” so a narrative with no action has no watermark and
// every link counts. Any status is deliberate rather than sloppy — a DECLINED
// proposal still means the narrative was examined and nothing new has happened
// since, which is exactly the state this must not report as outstanding. (Contrast
// EligibleEvents, which bounds on status='applied' only: that one asks "what has no
// tracker mutation claimed yet", a different question, and the difference is
// load-bearing.) Two queries answering "is there a delta" that drift apart produce
// a count describing a different population than the pass examined.
func (s *Store) CountNarrativesWithDelta(roles []Role) (int, error) {
	if len(roles) == 0 {
		return 0, nil
	}

	placeholders := make([]string, len(roles))
	args := make([]any, 0, len(roles))
	for i, r := range roles {
		placeholders[i] = "?"
		args = append(args, string(r))
	}

	query := `SELECT COUNT(*) FROM narratives n
	          WHERE EXISTS (
	              SELECT 1 FROM narrative_issues ni
	              WHERE ni.narrative_id = n.id AND ni.role IN (` + strings.Join(placeholders, ",") + `)
	          )
	            AND ` + hasUnexaminedDelta

	var n int
	if err := s.db.QueryRow(query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting narratives with an unexamined delta: %w", err)
	}

	return n, nil
}

// NarrativesWithActionableLinks returns up to limit narratives having at
// least one narrative_issues link whose role is in roles, ordered by
// (window_start, id) — the reconciler's input backlog.
//
// Keyed on the link table, which every narrative accessor now is. This one always
// was: a narratives.issue_key column used to denormalize the primary, and because
// MatchConfig.ConfidenceFloor decided whether to write it, a real but
// low-confidence primary had link rows and a NULL column. Selecting on the column
// would have left the reconciler permanently blind to that narrative. The column
// is gone (finding F11) after the same disagreement crashed a matching pass from
// the other direction, so the hazard this comment warned about cannot recur.
// Roles are passed in rather
// than hardcoded so the caller (reconciler.actionableLinks) stays the single
// definition of "actionable" — this package must not import
// internal/correlator.
//
// An empty roles slice returns an empty result (not an error): "nothing is
// actionable" is a valid caller configuration, not a malformed query.
func (s *Store) NarrativesWithActionableLinks(limit int, roles []Role) ([]NarrativeRow, error) {
	if len(roles) == 0 {
		return nil, nil
	}

	placeholders := make([]string, len(roles))
	args := make([]any, 0, len(roles)+1)
	for i, r := range roles {
		placeholders[i] = "?"
		args = append(args, string(r))
	}
	args = append(args, limit)

	query := `SELECT n.id, n.window_start, n.window_end, n.title, n.summary,
	                 n.status, n.compaction_boundary, n.compaction_boundary_event_id
	          FROM narratives n
	          WHERE EXISTS (
	              SELECT 1 FROM narrative_issues ni
	              WHERE ni.narrative_id = n.id AND ni.role IN (` + strings.Join(placeholders, ",") + `)
	          )
	            AND ` + hasUnexaminedDelta + `
	          ORDER BY n.window_start, n.id
	          LIMIT ?`

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying narratives with actionable links: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []NarrativeRow
	for rows.Next() {
		row, err := scanNarrativeRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning actionable-linked narrative row: %w", err)
		}
		out = append(out, row)
	}

	return out, rows.Err()
}

// AddNarrativeIssues records narrative_issues rows for a narrative, one per
// link. Each is an explicit upsert keyed on (narrative_id, issue_key) — NOT
// INSERT OR IGNORE. That distinction is load-bearing: measured against
// modernc.org/sqlite, INSERT OR IGNORE against the partial unique index on
// role='primary' returns a nil error and silently keeps the FIRST primary
// when a second primary is attempted under a different issue_key. A wrong
// early match would then quietly outrank a later, correct one while the
// write reported success. The explicit "ON CONFLICT ... DO UPDATE" instead
// lets the composite-key conflict path refresh role/provenance/confidence on
// a re-run (matching improves as better information becomes available) while
// still letting the database's partial unique index reject a genuine second
// primary with a real error.
func (s *Store) AddNarrativeIssues(narrativeID int64, links []NarrativeIssue) error {
	return addNarrativeIssuesImpl(s.db, narrativeID, links)
}

// AddNarrativeIssues is the *Tx-scoped variant of
// (*Store).AddNarrativeIssues.
func (t *Tx) AddNarrativeIssues(narrativeID int64, links []NarrativeIssue) error {
	return addNarrativeIssuesImpl(t.tx, narrativeID, links)
}

func addNarrativeIssuesImpl(c dbConn, narrativeID int64, links []NarrativeIssue) error {
	for _, link := range links {
		if _, err := c.Exec(
			`INSERT INTO narrative_issues
			   (narrative_id, issue_key, role, provenance, confidence, connection)
			 VALUES (?, ?, ?, ?, ?, ?)
			 ON CONFLICT (narrative_id, issue_key) DO UPDATE SET
			   role       = excluded.role,
			   provenance = excluded.provenance,
			   confidence = excluded.confidence,
			   connection = excluded.connection`,
			narrativeID, link.IssueKey, string(link.Role), link.Provenance,
			link.Confidence, nullable(link.Connection),
		); err != nil {
			return fmt.Errorf("adding issue link %s (%s) to narrative %d: %w",
				link.IssueKey, link.Role, narrativeID, err)
		}
	}

	return nil
}

// NarrativeIssues returns every issue link recorded for a narrative — the
// join-table rows a caller needs to see all roles (primary, same_work,
// mentioned) at once, which narratives.issue_key alone cannot express.
func (s *Store) NarrativeIssues(narrativeID int64) ([]NarrativeIssue, error) {
	rows, err := s.db.Query(
		`SELECT issue_key, role, provenance, confidence, connection
		 FROM narrative_issues WHERE narrative_id = ? ORDER BY issue_key`,
		narrativeID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying issue links for narrative %d: %w", narrativeID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []NarrativeIssue
	for rows.Next() {
		var (
			link       NarrativeIssue
			role       string
			confidence sql.NullFloat64
			connection sql.NullString
		)
		if err := rows.Scan(&link.IssueKey, &role, &link.Provenance, &confidence, &connection); err != nil {
			return nil, fmt.Errorf("scanning issue link row for narrative %d: %w", narrativeID, err)
		}
		link.Role = Role(role)
		if confidence.Valid {
			link.Confidence = confidence.Float64
		}
		link.Connection = connection.String
		out = append(out, link)
	}

	return out, rows.Err()
}

// NarrativeIssueKeysByNarrative returns, for each id in narrativeIDs that has
// at least one narrative_issues row, the set of issue keys it is linked to —
// every role, not only primary. One query for many narratives, for a caller
// ranking a batch (correlator.SelectContextNarratives, finding F16's
// context-narrative bound) rather than answering one narrative at a time the
// way NarrativeIssues does.
//
// Every role counts because the question this answers is "is this narrative
// ABOUT the same issue as something else", which is a fact about the story,
// not about which role Match promoted — that judgment is already made and
// irrelevant here.
//
// A narrative with no links has NO entry in the returned map, not an entry
// with an empty slice — the caller (SelectContextNarratives) only ever asks
// "does this key exist in the map for this id", so the distinction is never
// observed today, but an absent key is what "I don't know of any" should look
// like, matching NarrativesWithoutPrimaryLink's own map-vs-empty-slice
// convention elsewhere in this package.
//
// A nil or empty narrativeIDs returns an empty, non-nil map — not an error:
// "rank nothing" is a valid caller state (an idle pass with no context
// narratives at all), not a malformed query.
func (s *Store) NarrativeIssueKeysByNarrative(narrativeIDs []int64) (map[int64][]string, error) {
	out := make(map[int64][]string)
	if len(narrativeIDs) == 0 {
		return out, nil
	}

	placeholders := make([]string, len(narrativeIDs))
	args := make([]any, len(narrativeIDs))
	for i, id := range narrativeIDs {
		placeholders[i] = "?"
		args[i] = id
	}

	rows, err := s.db.Query(
		`SELECT narrative_id, issue_key FROM narrative_issues
		 WHERE narrative_id IN (`+strings.Join(placeholders, ",")+`)
		 ORDER BY narrative_id, issue_key`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("querying issue keys for %d narrative(s): %w", len(narrativeIDs), err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			narrativeID int64
			issueKey    string
		)
		if err := rows.Scan(&narrativeID, &issueKey); err != nil {
			return nil, fmt.Errorf("scanning issue key row: %w", err)
		}
		out[narrativeID] = append(out[narrativeID], issueKey)
	}

	return out, rows.Err()
}

// NarrativesForIssue returns every narrative linked to issueKey across all
// roles — the reverse lookup the reconciler needs so it doesn't act twice on
// one issue when two narratives share it (the SUMO co-representation case).
func (s *Store) NarrativesForIssue(issueKey string) ([]NarrativeIssueRef, error) {
	rows, err := s.db.Query(
		`SELECT narrative_id, role, confidence
		 FROM narrative_issues WHERE issue_key = ? ORDER BY narrative_id`,
		issueKey,
	)
	if err != nil {
		return nil, fmt.Errorf("querying narratives for issue %s: %w", issueKey, err)
	}
	defer func() { _ = rows.Close() }()

	var out []NarrativeIssueRef
	for rows.Next() {
		var (
			ref        NarrativeIssueRef
			role       string
			confidence sql.NullFloat64
		)
		if err := rows.Scan(&ref.NarrativeID, &role, &confidence); err != nil {
			return nil, fmt.Errorf("scanning narrative ref row for issue %s: %w", issueKey, err)
		}
		ref.Role = Role(role)
		if confidence.Valid {
			ref.Confidence = confidence.Float64
		}
		out = append(out, ref)
	}

	return out, rows.Err()
}

// RemoveNarrativeIssue deletes one narrative→issue link. It exists for
// `triage`'s retarget disposition, and it has to exist because
// AddNarrativeIssues only upserts: the partial unique index
// one_primary_per_narrative rejects a second primary while the first is
// present, so "this belongs on a different ticket" is remove-then-add rather
// than an update.
//
// A missing link is an error, not a silent success. Retarget's caller uses the
// error to abort before adding the replacement — otherwise a typo'd key would
// report a successful retarget having changed nothing, and the reviewer would
// believe an attribution moved when it did not.
func (s *Store) RemoveNarrativeIssue(narrativeID int64, issueKey string) error {
	return removeNarrativeIssueImpl(s.db, narrativeID, issueKey)
}

// RemoveNarrativeIssue is the *Tx-scoped variant of
// (*Store).RemoveNarrativeIssue. Retarget uses it so the remove and the
// replacing add land in one transaction: a crash between them would leave a
// narrative with no primary at all.
func (t *Tx) RemoveNarrativeIssue(narrativeID int64, issueKey string) error {
	return removeNarrativeIssueImpl(t.tx, narrativeID, issueKey)
}

func removeNarrativeIssueImpl(c dbConn, narrativeID int64, issueKey string) error {
	res, err := c.Exec(
		`DELETE FROM narrative_issues WHERE narrative_id = ? AND issue_key = ?`,
		narrativeID, issueKey,
	)
	if err != nil {
		return fmt.Errorf("removing issue link %s from narrative %d: %w", issueKey, narrativeID, err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking rows affected removing %s from narrative %d: %w", issueKey, narrativeID, err)
	}
	if affected == 0 {
		return fmt.Errorf("removing issue link %s from narrative %d: no such link", issueKey, narrativeID)
	}

	return nil
}

// NarrativesWithNoIssueLink returns up to limit narratives having NO
// narrative_issues row of any role — genuinely untracked work.
//
// Distinct from NarrativesWithoutPrimaryLink, and the difference is the whole point.
// That accessor selects on the denormalized narratives.issue_key, which
// MatchConfig.ConfidenceFloor only promotes above the floor — so a narrative with
// a real but low-confidence primary has narrative_issues rows AND a NULL
// issue_key, and appears in its results. Proposing a create for one of those
// would open a duplicate ticket for work that IS tracked, just not confidently.
//
// So this asks the stricter question: does any link exist at all? Only a NOT
// EXISTS answer means nobody filed anything.
//
// Exists because the reconciler could not see untracked narratives at all.
// NarrativesWithActionableLinks requires a link by construction, so reconcileOne
// was never invoked for an unlinked narrative — proven by probe:
//
//	NarrativesWithActionableLinks (reconciler's backlog) -> 0 rows
//	NarrativesWithoutPrimaryLink (matching's backlog)    -> 1 rows
//
// which is why adding "create" to the drafting prompt would have changed nothing.
func (s *Store) NarrativesWithNoIssueLink(limit int) ([]NarrativeRow, error) {
	rows, err := s.db.Query(
		`SELECT n.id, n.window_start, n.window_end, n.title, n.summary,
		        n.status, n.compaction_boundary, n.compaction_boundary_event_id
		 FROM narratives n
		 WHERE NOT EXISTS (
		     SELECT 1 FROM narrative_issues ni WHERE ni.narrative_id = n.id
		 )
		 ORDER BY n.window_start, n.id
		 LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("querying narratives with no issue link: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []NarrativeRow
	for rows.Next() {
		row, err := scanNarrativeRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning unlinked narrative row: %w", err)
		}
		out = append(out, row)
	}

	return out, rows.Err()
}
