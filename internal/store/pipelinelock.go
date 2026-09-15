package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jcogilvie/unjira/internal/logging"
)

// -- pipeline lock ---------------------------------------------------------

// lockTimeFormat is the timestamp layout used for pipeline_lock.held_since
// and pipeline_lock.expires_at only — NOT the layout used elsewhere in this
// package. events/narratives/cursors stay on time.RFC3339; switching those
// would mean re-auditing every query that string-compares a timestamp
// (EventsOn, the digest range scans, NarrativeEventsForContext), which is
// why only the lock — where sub-second TTLs are load-bearing — uses this.
//
// It differs from time.RFC3339 in two ways, both required for TryAcquire's
// atomic steal guard (a SQL string comparison, not a time comparison):
//
//   - Fixed sub-second width. time.RFC3339 has no fractional-second
//     placeholder at all, so Format silently truncates to whole seconds —
//     any sub-second TTL becomes indistinguishable from "already expired."
//     time.RFC3339Nano fixes precision but strips trailing zeros, which
//     breaks the lexicographic-order property below.
//   - Lexicographic order equals chronological order. A fixed-width
//     fractional part (always 9 digits) means comparing the formatted
//     strings byte-by-byte gives the same answer as comparing the times.
//     "...12:00:00.100000000Z" < "...12:00:00.150000000Z" holds as strings
//     exactly because both are always 9 digits; RFC3339Nano would format
//     these as ".1" and ".15", where the string comparison is wrong.
//
// TryAcquire normalizes now to UTC before formatting so two callers with
// the same instant but different offsets can't produce different Z07:00
// suffixes and defeat the ordering.
const lockTimeFormat = "2006-01-02T15:04:05.000000000Z07:00"

// TryAcquire attempts to take the singleton pipeline lock without blocking,
// in a single atomic statement (safe across concurrent processes/connections,
// unlike a separate read-then-write: two callers observing "unheld" could
// otherwise both upsert and both be told they acquired it). It succeeds when
// the lock is unheld or its lease has expired (expires_at at or before now),
// replacing the row with a fresh lease for runID; otherwise it returns false
// immediately. now is passed in (not time.Now()) so steal-on-expiry is
// deterministically testable.
//
// Stealing an expired lease logs a warning naming the stale run_id and how
// long it was held — the crash-recovery path. That diagnostic comes from a
// plain SELECT taken just before the atomic statement; it is best-effort
// (another acquirer could race between the SELECT and the write) and never
// gates the outcome — only the atomic statement's own RowsAffected does that.
func (s *Store) TryAcquire(runID string, now time.Time, ttl time.Duration) (bool, error) {
	now = now.UTC()
	nowStr := now.Format(lockTimeFormat)

	var (
		priorRunID string
		priorExp   string
	)
	if err := s.db.QueryRow(
		`SELECT run_id, expires_at FROM pipeline_lock WHERE id = 1`,
	).Scan(&priorRunID, &priorExp); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("reading pipeline lock for diagnostics: %w", err)
	}

	res, err := s.db.Exec(
		`INSERT INTO pipeline_lock (id, run_id, held_since, expires_at) VALUES (1, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		     run_id = excluded.run_id, held_since = excluded.held_since, expires_at = excluded.expires_at
		 WHERE pipeline_lock.expires_at <= ?`,
		runID, nowStr, now.Add(ttl).Format(lockTimeFormat), nowStr,
	)
	if err != nil {
		return false, fmt.Errorf("acquiring pipeline lock for %s: %w", runID, err)
	}

	acquired, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("checking rows affected acquiring pipeline lock for %s: %w", runID, err)
	}
	if acquired == 0 {
		return false, nil // still held by someone else
	}

	if priorRunID != "" && priorRunID != runID {
		if priorExpTime, perr := time.Parse(lockTimeFormat, priorExp); perr == nil {
			logging.For(s.log, "store").Warn("stealing expired pipeline lease",
				"prior_run_id", priorRunID, "expired_ago", now.Sub(priorExpTime).String())
		}
	}

	return true, nil
}

// Acquire is TryAcquire's blocking sibling: it polls every poll interval
// until it can take the lock (unheld or lease expired), honoring ctx
// cancellation. now is a clock func since Acquire loops. poll must be
// positive — a zero or negative poll turns this into a hot loop.
func (s *Store) Acquire(ctx context.Context, runID string, now func() time.Time, ttl, poll time.Duration) error {
	for {
		ok, err := s.TryAcquire(runID, now(), ttl)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("acquiring pipeline lock for %s: %w", runID, ctx.Err())
		case <-time.After(poll):
		}
	}
}

// ReleaseLock releases the pipeline lock iff it is currently held by runID.
// Releasing when runID is not the holder is a no-op — not an error, since it
// never clobbers a newer holder that stole an expired lease — but it is
// logged, for the same reason TryAcquire logs a steal: it is a noteworthy
// runtime condition (a run trying to release a lock it no longer holds,
// e.g. because its lease already expired and was stolen out from under it)
// that operators should be able to see without it failing the caller.
func (s *Store) ReleaseLock(runID string) error {
	res, err := s.db.Exec(`DELETE FROM pipeline_lock WHERE id = 1 AND run_id = ?`, runID)
	if err != nil {
		return fmt.Errorf("releasing pipeline lock for %s: %w", runID, err)
	}

	released, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking rows affected releasing pipeline lock for %s: %w", runID, err)
	}
	if released == 0 {
		logging.For(s.log, "store").Warn("pipeline lease release was a no-op",
			"run_id", runID, "reason", "not the current holder")
	}

	return nil
}
