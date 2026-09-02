package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// StatusEvent is one observed status change on an issue, as recorded by a
// collector rather than read live.
//
// Distinct from workflow.StatusChange, which is a (from, to) pair for graph
// mining with no timestamp: this carries when the change happened, which is the
// whole point of it — the reconciler compares it against how old the work
// evidence is.
type StatusEvent struct {
	// From and To are the transition's endpoints, as the tracker names them.
	// From may be empty (an issue's first status has no predecessor); To is
	// never empty, since LatestStatusEvent reports a missing destination as
	// absence rather than returning one.
	From string
	To   string
	// OccurredAt is when the change happened on the tracker, not when unjira
	// collected it.
	OccurredAt time.Time
}

// LatestStatusEvent returns the most recent collected status change for
// issueKey, reporting ok=false when none has been collected.
//
// Two facts the reconciler needs, from one query:
//
//   - To, compared against the live issue's current status name. Equal means
//     unjira's event history is current for this issue, so OccurredAt can be
//     trusted. Unequal means somebody moved the issue since the last collect and
//     unjira cannot know when or why.
//   - OccurredAt, compared against the newest work evidence. Work older than the
//     last status change has been superseded by whoever made that change.
//
// ok=false is deliberately NOT an error and deliberately NOT a zero-valued
// StatusEvent passed off as data. An issue with no collected status history is
// ordinary — the collector may be disabled, or the issue may never have moved —
// and a zero value would read as a real destination of "", which no live status
// equals, silently suppressing every transition on that issue forever. Absence
// must be visible to the caller so it can decide; see the reconciler's own
// handling and task #174 on making a guard that cannot run visible rather than
// merely absent.
//
// Ordering is (occurred_at DESC, id DESC). The id tiebreaker is load-bearing:
// Jira's changelog timestamps are second-granular and a bulk transition moves
// many issues in one second, so without it the winner is whatever SQLite returns
// and the guard's verdict could flip between passes over unchanged data.
func (s *Store) LatestStatusEvent(issueKey string) (StatusEvent, bool, error) {
	var (
		from       sql.NullString
		to         sql.NullString
		occurredAt string
	)

	// json_extract rather than a LIKE over the artifacts blob: a substring match
	// on a JSON document would also match an issue key appearing in some other
	// field's value. The driver (modernc.org/sqlite) ships JSON1, verified by
	// probe before this query was written.
	err := s.db.QueryRow(
		`SELECT json_extract(artifacts, '$.status_from'),
		        json_extract(artifacts, '$.status_to'),
		        occurred_at
		   FROM events
		  WHERE json_extract(artifacts, '$.issue_key') = ?
		    AND json_extract(artifacts, '$.field') = 'status'
		    AND json_extract(artifacts, '$.status_to') IS NOT NULL
		  ORDER BY occurred_at DESC, id DESC
		  LIMIT 1`,
		issueKey,
	).Scan(&from, &to, &occurredAt)

	if errors.Is(err, sql.ErrNoRows) {
		return StatusEvent{}, false, nil
	}
	if err != nil {
		return StatusEvent{}, false, fmt.Errorf("reading latest status event for %s: %w", issueKey, err)
	}

	// A JSON null or an empty destination is no evidence, not evidence of "".
	// Filtered in SQL above for the null case; this covers "".
	if !to.Valid || to.String == "" {
		return StatusEvent{}, false, nil
	}

	at, err := time.Parse(time.RFC3339, occurredAt)
	if err != nil {
		return StatusEvent{}, false, fmt.Errorf(
			"parsing occurred_at %q for latest status event on %s: %w", occurredAt, issueKey, err,
		)
	}

	return StatusEvent{From: from.String, To: to.String, OccurredAt: at}, true, nil
}
