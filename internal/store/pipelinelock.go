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

// The lease's held_since and expires_at are DATETIME columns bound as time.Time, like
// every stored time (see the package doc). TryAcquire's atomic steal guard is a SQL
// comparison of the stored expires_at with a bound now, and that is an instant
// comparison at full precision: both sides are the driver's UTC text, whose text
// order is instant order, offsets and trailing-zero trimming included
// (TestStoredTimes_TextOrderIsInstantOrder). Sub-second precision is load-bearing
// here: a sub-second TTL truncated to whole seconds would be indistinguishable from
// "already expired".

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
	var (
		priorRunID string
		priorExp   time.Time
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
		runID, now, now.Add(ttl), now,
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
		logging.For(s.log, "store").Warn("stealing expired pipeline lease",
			"prior_run_id", priorRunID, "expired_ago", now.Sub(priorExp).String())
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
