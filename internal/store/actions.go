package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
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
// applied). DecidedAt/ExecutedAt are pointers because NULL is meaningful: NULL
// decided_at means "no human has ruled yet", which is distinct from any
// timestamp. Every time is a time.Time in UTC, as the store reads every
// DATETIME column (see the package doc).
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
	Error      string     `json:"error"`
	DecidedAt  *time.Time `json:"decided_at"`
	ExecutedAt *time.Time `json:"executed_at"`
	// CreatedAt is set by InsertAction from the store's clock; a value passed in is
	// ignored.
	CreatedAt time.Time `json:"created_at"`
}

// InsertAction writes one proposed action and returns its id.
//
// created_link_seq records the link sequence's high-water mark in the same
// statement, and is what DeltaEvents bounds on: links made after this insert
// are the next pass's delta. created_at is the store's clock at insert, for display
// and ordering only (finding F30).
func (s *Store) InsertAction(a ActionRow) (int64, error) {
	return insertActionImpl(s.db, s.now(), a)
}

// InsertAction is the Tx-scoped form, so reconciler.Persist can write a whole
// pass atomically.
func (t *Tx) InsertAction(a ActionRow) (int64, error) {
	return insertActionImpl(t.tx, t.now(), a)
}

func insertActionImpl(c dbConn, createdAt time.Time, a ActionRow) (int64, error) {
	res, err := c.Exec(
		`INSERT INTO actions
		   (narrative_id, type, issue_key, payload, confidence, rationale, status, created_at, created_link_seq)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, `+linkSeqHighWater+`)`,
		a.NarrativeID, a.Type, nullable(a.IssueKey), a.Payload,
		a.Confidence, nullable(a.Rationale), a.Status, createdAt,
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
// Both stamps are one reading of the store's clock, bound as a time.Time like every
// stored time, so a statement that sets both sets them equal. Terminal-state semantics:
// any human ruling sets decided_at; only a write that actually reached the
// tracker sets executed_at — and, in the same statement, executed_link_seq,
// which is what the freeze rule compares (EligibleMemberEventIDs, EligibleContextEventIDs). executed_at itself
// decides nothing (finding F30).
func (s *Store) UpdateActionStatus(id int64, status string) error {
	return s.updateActionStatus(id, status, nil, nil)
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
	return s.updateActionStatus(id, status, &feedback, nil)
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
	return s.updateActionStatus(id, status, nil, errText)
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
//
// It takes a transaction, not any dbConn, because a ruling that makes the row a correction
// is two writes (markCorrection's insert, then this update) and they are one fact.
// now is the store's clock reading for every stamp the ruling makes.
func updateActionStatusImpl(c *sql.Tx, now time.Time, id int64, status string, feedback, errText *string) error {
	query := `UPDATE actions SET status = ?`
	args := []any{status}

	seq, marked, err := markCorrection(c, now, id, status, feedback)
	if err != nil {
		return err
	}
	if marked {
		query += `, corrected_seq = ?`
		args = append(args, seq)
	}

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
		query += `, decided_at = COALESCE(decided_at, ?)`
		args = append(args, now)
	case StatusApplied, StatusFailed:
		// An applied action was necessarily decided, but decided_at may
		// already be set from an earlier approval — COALESCE preserves the
		// original ruling time rather than overwriting it.
		//
		// executed_link_seq is restamped with executed_at on every execution, so
		// a retried write freezes against the moment it last ran, as executed_at
		// always recorded.
		query += `, decided_at = COALESCE(decided_at, ?), executed_at = ?, executed_link_seq = ` + linkSeqHighWater
		args = append(args, now, now)
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

// updateActionStatus runs updateActionStatusImpl in its own transaction, for callers that
// are not already inside one.
func (s *Store) updateActionStatus(id int64, status string, feedback, errText *string) error {
	return s.WithTx(func(tx *Tx) error {
		return updateActionStatusImpl(tx.tx, tx.now(), id, status, feedback, errText)
	})
}

// markCorrection issues the next correction sequence for action id when this ruling makes
// it a correction, and reports whether it did. A ruling makes a row a correction when its
// new status is rejected or edited and its feedback (the new text, or the stored text when
// the caller leaves feedback alone) is non-empty after trimming, UNLESS the row already was
// one with exactly that feedback. So adding a lesson to a silent rejection marks it (F42),
// rewording a lesson marks it again, and switching reject to edit with identical words does
// not, since status is not part of what a correction says.
//
// The condition is evaluated in SQL against the row as it is before the update, in the
// caller's transaction, so no other writer can change the row between the read and the
// write.
func markCorrection(c *sql.Tx, now time.Time, id int64, status string, feedback *string) (int64, bool, error) {
	if status != StatusRejected && status != StatusEdited {
		return 0, false, nil
	}

	var newFeedback any
	if feedback != nil {
		newFeedback = *feedback
	}

	res, err := c.Exec(
		`INSERT INTO correction_marks (action_id, marked_at)
		 SELECT id, ? FROM actions
		  WHERE id = ?
		    AND TRIM(COALESCE(?, feedback, '')) != ''
		    AND NOT (status IN (?, ?)
		             AND corrected_seq IS NOT NULL
		             AND COALESCE(feedback, '') = COALESCE(?, feedback, ''))`,
		now, id, newFeedback, StatusRejected, StatusEdited, newFeedback,
	)
	if err != nil {
		return 0, false, fmt.Errorf("marking action %d as a correction: %w", id, err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return 0, false, fmt.Errorf("checking whether action %d became a correction: %w", id, err)
	}
	if n == 0 {
		return 0, false, nil
	}

	seq, err := res.LastInsertId()
	if err != nil {
		return 0, false, fmt.Errorf("reading action %d's correction sequence: %w", id, err)
	}

	return seq, true, nil
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
			decidedAt, executedAt                 sql.NullTime
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
			a.DecidedAt = &decidedAt.Time
		}
		if executedAt.Valid {
			a.ExecutedAt = &executedAt.Time
		}

		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating action rows: %w", err)
	}

	return out, nil
}
