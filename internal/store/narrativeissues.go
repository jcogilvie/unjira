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

// NarrativesWithoutIssueKey returns up to limit narratives that have not yet
// been attributed to a primary issue, ordered by (window_start, id) — the
// backlog a matching pass works through. InsertNarrative leaves issue_key
// NULL (the column has no NOT NULL constraint and is omitted from the insert
// column list), so "IS NULL" alone would suffice; the "= ”" half of the
// predicate is defensive belt-and-suspenders in case a future writer ever
// persists an empty string instead of leaving it NULL.
func (s *Store) NarrativesWithoutIssueKey(limit int) ([]NarrativeRow, error) {
	rows, err := s.db.Query(
		`SELECT id, window_start, window_end, title, summary, issue_key, confidence, status,
		        compaction_boundary, compaction_boundary_event_id
		 FROM narratives
		 WHERE issue_key IS NULL OR issue_key = ''
		 ORDER BY window_start, id
		 LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("querying narratives without an issue key: %w", err)
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

// NarrativesWithActionableLinks returns up to limit narratives having at
// least one narrative_issues link whose role is in roles, ordered by
// (window_start, id) — the reconciler's input backlog.
//
// Deliberately NOT keyed on the denormalized narratives.issue_key, which
// MatchConfig.ConfidenceFloor only promotes above the floor: a real but
// low-confidence primary has narrative_issues rows and a NULL issue_key, and
// selecting on issue_key would leave the reconciler permanently blind to it
// — exactly the class of bug behind the 08-21 discovery that
// narratives.issue_key was never written at all. Roles are passed in rather
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

	query := `SELECT n.id, n.window_start, n.window_end, n.title, n.summary, n.issue_key,
	                 n.confidence, n.status, n.compaction_boundary, n.compaction_boundary_event_id
	          FROM narratives n
	          WHERE EXISTS (
	              SELECT 1 FROM narrative_issues ni
	              WHERE ni.narrative_id = n.id AND ni.role IN (` + strings.Join(placeholders, ",") + `)
	          )
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

// SetNarrativeIssueLink denormalizes a narrative's primary issue onto
// narratives.issue_key/confidence, so callers that only need "what issue is
// this" (digest/status output, NarrativesWithoutIssueKey's backlog filter)
// don't have to join narrative_issues. AddNarrativeIssues is what actually
// records the primary relationship; call both when setting a primary.
func (s *Store) SetNarrativeIssueLink(id int64, issueKey string, confidence float64) error {
	return setNarrativeIssueLinkImpl(s.db, id, issueKey, confidence)
}

// SetNarrativeIssueLink is the *Tx-scoped variant of
// (*Store).SetNarrativeIssueLink.
func (t *Tx) SetNarrativeIssueLink(id int64, issueKey string, confidence float64) error {
	return setNarrativeIssueLinkImpl(t.tx, id, issueKey, confidence)
}

func setNarrativeIssueLinkImpl(c dbConn, id int64, issueKey string, confidence float64) error {
	if _, err := c.Exec(
		`UPDATE narratives SET issue_key = ?, confidence = ? WHERE id = ?`,
		issueKey, confidence, id,
	); err != nil {
		return fmt.Errorf("setting issue link for narrative %d: %w", id, err)
	}

	return nil
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
// Distinct from NarrativesWithoutIssueKey, and the difference is the whole point.
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
//	NarrativesWithoutIssueKey (matching's backlog)       -> 1 rows
//
// which is why adding "create" to the drafting prompt would have changed nothing.
func (s *Store) NarrativesWithNoIssueLink(limit int) ([]NarrativeRow, error) {
	rows, err := s.db.Query(
		`SELECT n.id, n.window_start, n.window_end, n.title, n.summary, n.issue_key,
		        n.confidence, n.status, n.compaction_boundary, n.compaction_boundary_event_id
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
