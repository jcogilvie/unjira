package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ActionTypeWorklog is the action type the worklog sink proposes.
const ActionTypeWorklog = "worklog"

// The worklog action's ops: one type covers adds and their corrections (R3).
const (
	WorklogOpAdd    = "add"
	WorklogOpUpdate = "update"
	WorklogOpDelete = "delete"
)

// WorklogPayload is a worklog action's payload, written by the worklog sink and read by
// gate.Applier. One definition for both (R5), here because store is the cycle-free home.
type WorklogPayload struct {
	Op               string          `json:"op"`
	WorklogID        string          `json:"worklog_id,omitempty"` // update, delete
	Started          time.Time       `json:"started"`
	TimeSpentSeconds int             `json:"time_spent_seconds"`
	Comment          string          `json:"comment"`
	MessageIDs       []string        `json:"message_ids"` // add: to mark; update: that remain
	IdempotencyKey   string          `json:"idempotency_key"`
	Property         json.RawMessage `json:"property,omitempty"`
}

// MarkWorklogged records on each message that it is logged in worklogID on issueKey,
// in one transaction: any message unknown, or already logged in another worklog, fails
// the whole batch. Re-marking with the same pair is a no-op. Another worklog's marker is
// never overwritten (R9): that would lose the record of time Jira already holds.
func (s *Store) MarkWorklogged(issueKey, worklogID string, messageIDs []string) error {
	return s.WithTx(func(tx *Tx) error {
		for _, id := range messageIDs {
			res, err := tx.tx.Exec(`UPDATE token_messages SET worklog_id = ?, worklog_issue_key = ?
				WHERE message_id = ? AND (worklog_id IS NULL OR (worklog_id = ? AND worklog_issue_key = ?))`,
				worklogID, issueKey, id, worklogID, issueKey)
			if err != nil {
				return fmt.Errorf("marking message %s logged in %s/%s: %w", id, issueKey, worklogID, err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("marking message %s logged in %s/%s: %w", id, issueKey, worklogID, err)
			}
			if n == 1 {
				continue
			}
			var heldKey, heldID string
			err = tx.tx.QueryRow(`SELECT worklog_issue_key, worklog_id FROM token_messages WHERE message_id = ?`, id).
				Scan(&heldKey, &heldID)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("marking message %s logged in %s/%s: no such ledgered message", id, issueKey, worklogID)
			}
			if err != nil {
				return fmt.Errorf("reading marker of message %s: %w", id, err)
			}
			return fmt.Errorf("marking message %s logged in %s/%s: already logged in %s/%s",
				id, issueKey, worklogID, heldKey, heldID)
		}

		return nil
	})
}

// ClearWorklogMarkers clears the marker of every message logged in worklogID on
// issueKey except those in keep, and returns how many it cleared. Messages on any other
// worklog are untouched.
func (s *Store) ClearWorklogMarkers(issueKey, worklogID string, keep []string) (int, error) {
	q := `UPDATE token_messages SET worklog_id = NULL, worklog_issue_key = NULL
		WHERE worklog_issue_key = ? AND worklog_id = ?`
	args := []any{issueKey, worklogID}
	if len(keep) > 0 {
		q += ` AND message_id NOT IN (` + placeholders(len(keep)) + `)`
		for _, k := range keep {
			args = append(args, k)
		}
	}
	res, err := s.db.Exec(q, args...)
	if err != nil {
		return 0, fmt.Errorf("clearing worklog markers of %s/%s: %w", issueKey, worklogID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("clearing worklog markers of %s/%s: %w", issueKey, worklogID, err)
	}

	return int(n), nil
}

// LoggedAmong returns, for each of messageIDs already logged, "<issue key>/<worklog id>".
// Unlogged and unknown ids are absent.
func (s *Store) LoggedAmong(messageIDs []string) (map[string]string, error) {
	out := map[string]string{}
	if len(messageIDs) == 0 {
		return out, nil
	}
	args := make([]any, len(messageIDs))
	for i, id := range messageIDs {
		args[i] = id
	}
	rows, err := s.db.Query(`SELECT message_id, worklog_issue_key, worklog_id FROM token_messages
		WHERE worklog_id IS NOT NULL AND message_id IN (`+placeholders(len(messageIDs))+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("querying logged messages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var id, key, worklog string
		if err := rows.Scan(&id, &key, &worklog); err != nil {
			return nil, fmt.Errorf("scanning logged message: %w", err)
		}
		out[id] = key + "/" + worklog
	}

	return out, rows.Err()
}

// SinkEnabledAt returns the first pass the sink ran for the tracker, recording now as
// that pass when none is held: the sink's no-backfill floor (D-19). A later now never
// moves it.
func (s *Store) SinkEnabledAt(tracker, sink string, now time.Time) (time.Time, error) {
	if _, err := s.db.Exec(`INSERT INTO attribution_sink_state (tracker, sink, enabled_at) VALUES (?, ?, ?)
		ON CONFLICT DO NOTHING`, tracker, sink, now); err != nil {
		return time.Time{}, fmt.Errorf("recording enable time of sink %s on %s: %w", sink, tracker, err)
	}
	var at time.Time
	if err := s.db.QueryRow(`SELECT enabled_at FROM attribution_sink_state WHERE tracker = ? AND sink = ?`,
		tracker, sink).Scan(&at); err != nil {
		return time.Time{}, fmt.Errorf("reading enable time of sink %s on %s: %w", sink, tracker, err)
	}

	return at, nil
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
