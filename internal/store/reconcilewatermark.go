package store

import "fmt"

// reconcileExaminationsSchema records that a reconcile pass looked at a narrative and
// could act on nothing in its delta — the watermark that keeps that backlog draining.
// Two outcomes write it, each naming itself in reason: the whole delta was
// self-authored (finding F26), or every link the narrative has is `mentioned`, so no
// link may receive an action. Both are decided in Go after selection, so neither is
// visible to the selector's SQL without this row.
//
// This is F22's mechanism applied to the reconciler side. F12 fixed the reconciler's
// first outcome-leaves-no-trace path (a drafted-then-suppressed action) by writing a
// StatusSuppressed action row, because a draft existed to attach the watermark to.
// F26's path has no draft: reconciler.dropSelfAuthored empties the delta BEFORE
// verifyLinks or draft ever runs, so there is nothing to persist as an action. A side
// table mirrors what F22 already built for the identical shape on the matching side,
// rather than inventing a third variant of the same mechanism.
//
// A separate table rather than a column, for the same reason match_examinations is
// one: this package has no migration mechanism, so a new TABLE lands on an existing
// database via CREATE TABLE IF NOT EXISTS while a new COLUMN would need an ALTER
// nothing here runs.
//
// examined_link_seq is the watermark itself, exactly as match_examinations'
// is: the link sequence's high-water mark at the moment of examination, compared
// against narrative_events.link_seq. examined_at is DISPLAY ONLY and must never again
// decide anything — compared as `linked_at > examined_at` it was a tombstone for a link
// made in the same millisecond as the examination (finding F30; see
// matchExaminationsSchema for the full history, including the earlier %S/%f inversion).
const reconcileExaminationsSchema = `
CREATE TABLE IF NOT EXISTS reconcile_examinations (
    narrative_id INTEGER PRIMARY KEY REFERENCES narratives (id),
    examined_at  DATETIME NOT NULL,
    examined_link_seq INTEGER NOT NULL,
    reason       TEXT NOT NULL
);`

// reconcileExaminationPredicate excludes narratives the reconciler already examined
// and could act on nothing in (see reconcileExaminationsSchema), UNLESS an event has
// been linked since — the watermark half of F26's fix.
//
// A shared const, interpolated into both NarrativesWithActionableLinks and
// CountNarrativesWithDelta, for the reason matchExaminationPredicate is shared
// between its own two callers: those two queries answering "is there a delta"
// drifting apart is F10's failure mode a third time, a count describing a population
// the pass never actually examines.
//
// Comparing link_seq > examined_link_seq makes this a watermark, not a tombstone: a
// narrative that gains an event after the examination is re-admitted, because that event
// might not be self-authored — dropSelfAuthored runs per-pass on whatever DeltaEvents
// returns, so a fresh non-unjira event reopens the narrative exactly as a fresh
// candidate key reopens one in match_examinations.
//
// MEMBER links only (shared-context spec §2): DeltaEvents never returns a context
// event, so a context link cannot change what the reconciler would find, and
// re-admitting on one would spend a model call on a delta that is unchanged.
const reconcileExaminationPredicate = `
		 AND NOT EXISTS (
		     SELECT 1 FROM reconcile_examinations re
		     WHERE re.narrative_id = n.id
		       AND NOT EXISTS (
		           SELECT 1 FROM narrative_events ne
		           WHERE ne.narrative_id = n.id AND ` + memberLink + `
		             AND ne.link_seq > re.examined_link_seq
		       )
		 )`

// RecordReconcileExamined marks narrativeID as examined by a reconcile pass at a
// moment when it could act on nothing in its delta — the delta was entirely
// self-authored, or every link was `mentioned` — so the selector yields to the
// narratives behind it. reason says which.
//
// The mentioned-only case is the same shape one step earlier: SelectionRoles selects
// such a narrative deliberately, so it gets a result row, and reconcileOne then finds
// no actionable link. Unrecorded, one narrative citing two tickets was re-selected and
// counted as unexamined work on every pass of a real 30-day store.
//
// WHY THIS EXISTS. NarrativesWithActionableLinks already filters out narratives with
// no unexamined delta at the SQL level (hasUnexaminedDelta), but
// reconciler.dropSelfAuthored is a Go-side filter the selector cannot see: unjira's
// own authored_by_unjira events are indistinguishable from real work to SQL alone,
// and a narrative whose delta survives hasUnexaminedDelta but is entirely
// self-authored is selected, emptied by dropSelfAuthored, and hits SkippedNoDelta —
// writing nothing. Stable order plus an outcome that leaves no trace is a livelock
// (design-notes #29), the third instance after F12 and F22. Measured: draining
// converges to 7 narratives matching this shape exactly, re-selected every pass.
//
// A WATERMARK, NOT A TOMBSTONE, for the identical reason RecordMatchExamined and
// StatusSuppressed both are: a narrative can gain events later (a human comments, a
// status changes, more code lands) that are NOT self-authored, and those must
// re-admit it. Recording "never reconcile this" would make an early self-authored-
// only delta permanent.
//
// Upserts rather than inserts, keeping one row per narrative regardless of how many
// passes re-examine it — see CountReconcileExaminations' test.
func (s *Store) RecordReconcileExamined(narrativeID int64, reason string) error {
	if _, err := s.db.Exec(
		`INSERT INTO reconcile_examinations (narrative_id, examined_at, examined_link_seq, reason)
		 VALUES (?, ?, `+linkSeqHighWater+`, ?)
		 ON CONFLICT(narrative_id) DO UPDATE SET
		     examined_at = excluded.examined_at,
		     examined_link_seq = excluded.examined_link_seq,
		     reason = excluded.reason`,
		narrativeID, s.now(), reason,
	); err != nil {
		return fmt.Errorf("recording reconcile examination for narrative %d: %w", narrativeID, err)
	}

	return nil
}

// CountReconcileExaminations is how many watermark rows narrativeID has. Exists for
// the test that pins one-row-per-narrative, mirroring CountMatchExaminations.
func (s *Store) CountReconcileExaminations(narrativeID int64) (int, error) {
	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM reconcile_examinations WHERE narrative_id = ?`, narrativeID,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting reconcile examinations for narrative %d: %w", narrativeID, err)
	}

	return n, nil
}
