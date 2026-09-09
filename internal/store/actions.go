package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// ErrActionNotFound is returned by GetAction when no row matches — `unjira
// actions decide` needs to distinguish "no such action" from every other
// failure mode, since the former is a user-facing error (a typo'd id), not
// a store bug.
var ErrActionNotFound = errors.New("action not found")

// -- actions ---------------------------------------------------------------

// ActionRow is one row of the actions table — a proposed, decided, or
// executed change to a tracker issue.
//
// IssueKey is empty for a `create` action (there is no key until it is
// applied). DecidedAt/ExecutedAt are *string rather than string because NULL
// is meaningful: NULL decided_at means "no human has ruled yet", which is
// distinct from any timestamp. They stay strings rather than time.Time to
// match how the rest of this package stores timestamps (SQLite TEXT), and
// because nothing orders by them.
// JSON tags mirror the actions table's own column names (snake_case), not
// Go's default CamelCase field names — `unjira actions list --json` is a
// machine-facing surface built for scripting/jq, and a reader piping this
// output should see the same names they'd see in a `sqlite3` query, not a
// second, Go-flavored vocabulary for the same columns.
type ActionRow struct {
	ID          int64   `json:"id"`
	NarrativeID int64   `json:"narrative_id"`
	Type        string  `json:"type"` // comment | transition | create | estimate
	IssueKey    string  `json:"issue_key"`
	Payload     string  `json:"payload"` // JSON; shape depends on Type
	Confidence  float64 `json:"confidence"`
	Rationale   string  `json:"rationale"`
	Status      string  `json:"status"` // proposed | approved | edited | rejected | applied | failed | declined
	Feedback    string  `json:"feedback"`
	// Error is the tracker error's message when Status is "failed" — see the
	// actions.error column comment in the schema string (store.go) for why
	// this is a separate field from Feedback, never conflated. Empty for
	// every other status, including a since-cleared failure that was later
	// retried and applied (see UpdateActionStatusAndError).
	Error      string  `json:"error"`
	DecidedAt  *string `json:"decided_at"`
	ExecutedAt *string `json:"executed_at"`
	CreatedAt  string  `json:"created_at"`
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
	defer func() { _ = rows.Close() }()

	return scanActions(rows)
}

// ActionsByStatus returns every action in one workflow state, oldest first —
// the review queue's read path (slice 6's `triage`).
func (s *Store) ActionsByStatus(status string) ([]ActionRow, error) {
	rows, err := s.db.Query(actionSelect+` WHERE status = ? ORDER BY created_at, id`, status)
	if err != nil {
		return nil, fmt.Errorf("querying actions with status %q: %w", status, err)
	}
	defer func() { _ = rows.Close() }()

	return scanActions(rows)
}

// GetAction returns the action with the given id, or ErrActionNotFound.
//
// `unjira actions decide` is the first caller: it is handed a bare id on the
// command line and must load the full row (status, for the double-post
// guard; type/issue_key/payload, for Applier.Apply) before it can act on it
// at all. ActionsForNarrative/ActionsByStatus both require already knowing
// something else about the row (its narrative, its status) — this is the
// first accessor that takes only an id, matching how a human refers to a
// row in this table (they see an id in `actions list`, not a narrative id).
func (s *Store) GetAction(id int64) (ActionRow, error) {
	rows, err := s.db.Query(actionSelect+` WHERE id = ?`, id)
	if err != nil {
		return ActionRow{}, fmt.Errorf("querying action %d: %w", id, err)
	}
	defer func() { _ = rows.Close() }()

	found, err := scanActions(rows)
	if err != nil {
		return ActionRow{}, err
	}
	if len(found) == 0 {
		return ActionRow{}, fmt.Errorf("getting action %d: %w", id, ErrActionNotFound)
	}

	return found[0], nil
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
	defer func() { _ = rows.Close() }()

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
	return updateActionStatusImpl(s.db, id, status, nil, nil)
}

// UpdateActionStatusAndFeedback moves an action to a new workflow state
// while persisting the reviewer's free-text feedback in the SAME statement
// as the status change — `unjira actions decide --edit` needs both to land
// atomically, not as two separate writes a caller could half-apply (e.g. by
// forgetting the second call, or hitting an error between the two).
//
// feedback is written as-is: it is free prose from a human, and quoting or
// escaping it here would just be a second, easier-to-forget place for the
// exact payload-encoding bug class slice 4 already hit once. The driver's
// placeholder binding is what actually protects this from SQL injection or
// truncation, not any transformation of this function's own.
func (s *Store) UpdateActionStatusAndFeedback(id int64, status, feedback string) error {
	return updateActionStatusImpl(s.db, id, status, &feedback, nil)
}

// UpdateActionStatusAndError moves an action to a new workflow state while
// persisting (or clearing) the machine-written tracker-failure reason in the
// SAME statement as the status change — gate.Applier.Apply needs both to
// land atomically, matching UpdateActionStatusAndFeedback's own rationale for
// doing so.
//
// errText is *string, not string, because "leave the existing error alone"
// and "clear it to empty" are both real, distinct callers need here and a
// bare string cannot express the first: nil means leave whatever is already
// in the column untouched (e.g. a caller only changing status for an
// unrelated reason), while a non-nil pointer — including one pointing at ""
// — overwrites it. Apply always passes a non-nil pointer: err.Error() on
// failure, or a pointer to "" on success, because "a subsequent success must
// clear a stale reason" is the design's own stale-reason requirement — a
// retried-and-fixed action must never keep reporting the reason it failed
// for last time.
func (s *Store) UpdateActionStatusAndError(id int64, status string, errText *string) error {
	return updateActionStatusImpl(s.db, id, status, nil, errText)
}

// updateActionStatusImpl backs UpdateActionStatus, UpdateActionStatusAndFeedback,
// and UpdateActionStatusAndError so the decided_at/executed_at stamping rules
// live in exactly one place regardless of which optional column (feedback,
// error, neither) a caller also wants to set in the same statement. feedback
// and errText are independent nil-checked pointers — a caller can set either,
// both, or neither — so no code path can conflate a human's free-text
// correction with a machine-written tracker error (see the actions.error
// schema comment for why that conflation would be a real problem, not a
// theoretical one).
func updateActionStatusImpl(c dbConn, id int64, status string, feedback, errText *string) error {
	const ts = `strftime('%Y-%m-%dT%H:%M:%fZ', 'now')`

	query := `UPDATE actions SET status = ?`
	args := []any{status}

	if feedback != nil {
		query += `, feedback = ?`
		args = append(args, *feedback)
	}

	if errText != nil {
		query += `, error = ?`
		args = append(args, *errText)
	}

	switch status {
	case StatusApproved, StatusEdited, StatusRejected:
		query += `, decided_at = COALESCE(decided_at, ` + ts + `)`
	case StatusApplied, StatusFailed:
		// An applied action was necessarily decided, but decided_at may
		// already be set from an earlier approval — COALESCE preserves the
		// original ruling time rather than overwriting it.
		query += `, decided_at = COALESCE(decided_at, ` + ts + `), executed_at = ` + ts
	}
	query += ` WHERE id = ?`
	args = append(args, id)

	res, err := c.Exec(query, args...)
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

// actionSelect is the column list every ActionRow query shares, so a new
// column cannot be added to one query and forgotten in another.
const actionSelect = `SELECT id, narrative_id, type, issue_key, payload, confidence,
	rationale, status, feedback, error, decided_at, executed_at, created_at FROM actions`

func scanActions(rows *sql.Rows) ([]ActionRow, error) {
	var out []ActionRow

	for rows.Next() {
		var (
			a                                     ActionRow
			issueKey, rationale, feedback, errCol sql.NullString
			confidence                            sql.NullFloat64
			decidedAt, executedAt                 sql.NullString
		)

		if err := rows.Scan(
			&a.ID, &a.NarrativeID, &a.Type, &issueKey, &a.Payload, &confidence,
			&rationale, &a.Status, &feedback, &errCol, &decidedAt, &executedAt, &a.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scanning action row: %w", err)
		}

		a.IssueKey = issueKey.String
		a.Rationale = rationale.String
		a.Feedback = feedback.String
		a.Error = errCol.String
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
