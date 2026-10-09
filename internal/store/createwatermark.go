package store

import "fmt"

// ActionTypeCreate is the actions.type value of a create action: opening a new issue
// for a narrative that has none.
//
// Declared here because awaitingCreate, below, has to name it in SQL and this package
// must not import internal/reconciler. reconciler.ActionCreate is defined as this
// constant, so the predicate and the rows it reads cannot spell it differently.
const ActionTypeCreate = "create"

// createExaminationsSchema records that the create path looked at an untracked
// narrative and found every member event to be unjira's own output — the watermark that
// keeps the create backlog draining.
//
// reconciler.proposeCreateOne skips such a narrative before any model call
// (dropSelfAuthored empties it), and nothing else about that outcome is persisted: no
// create is drafted, so there is no action row to carry a watermark the way a decline
// does. Unrecorded, the narrative is selected again next pass under the stable
// (window_start, id) order and keeps its slot forever — F26's livelock on the create
// path. Same mechanism as reconcile_examinations and match_examinations.
//
// ITS OWN TABLE, not a row in reconcile_examinations, because the two record different
// facts and a shared row would let one stand in for the other. A reconcile examination
// says the reconciler could act on nothing in a linked narrative's DELTA (the links
// after its latest action): it was all unjira's own, or the narrative's every link was
// `mentioned`. Its earlier member events may be real work. A create examination says EVERY
// member event was. reconcile_examinations is keyed on narrative_id alone, so a
// narrative that was reconciled while linked and later lost its links (a triage
// retarget, a merge) would be skipped by the create path on the strength of a
// delta-only judgment, and real work would never be offered as a ticket. In the other
// direction, a create examination would hide a narrative from the reconcile backlog the
// moment matching linked it. Two tables keep each watermark answerable only to the
// question its writer asked.
//
// A table rather than a column, and examined_link_seq rather than examined_at deciding
// anything, for the reasons reconcileExaminationsSchema gives: no migration mechanism,
// and timestamps are display-only since finding F30.
const createExaminationsSchema = `
CREATE TABLE IF NOT EXISTS create_examinations (
    narrative_id INTEGER PRIMARY KEY REFERENCES narratives (id),
    examined_at  DATETIME NOT NULL,
    examined_link_seq INTEGER NOT NULL,
    reason       TEXT NOT NULL
);`

// createExaminationPredicate excludes narratives the create path already examined and
// found entirely self-authored, UNLESS a member event has been linked since.
//
// A watermark, not a tombstone, exactly as reconcileExaminationPredicate is: a member
// event linked after the examination may be real work, and proposeCreateOne would then
// ask the model. MEMBER links only, because AllMemberEvents and DeltaEvents never return
// a context link, so one cannot change what the pass would find.
const createExaminationPredicate = `
		 AND NOT EXISTS (
		     SELECT 1 FROM create_examinations ce
		     WHERE ce.narrative_id = n.id
		       AND NOT EXISTS (
		           SELECT 1 FROM narrative_events ne
		           WHERE ne.narrative_id = n.id AND ` + memberLink + `
		             AND ne.link_seq > ce.examined_link_seq
		       )
		 )`

// RecordCreateExamined marks narrativeID as examined by the create path at a moment when
// it held no member event that was not unjira's own, so NarrativesAwaitingCreate yields
// to the narratives behind it until a member event is linked after this call.
//
// Upserts, keeping one row per narrative however many passes re-examine it.
func (s *Store) RecordCreateExamined(narrativeID int64, reason string) error {
	if _, err := s.db.Exec(
		`INSERT INTO create_examinations (narrative_id, examined_at, examined_link_seq, reason)
		 VALUES (?, ?, `+linkSeqHighWater+`, ?)
		 ON CONFLICT(narrative_id) DO UPDATE SET
		     examined_at = excluded.examined_at,
		     examined_link_seq = excluded.examined_link_seq,
		     reason = excluded.reason`,
		narrativeID, s.now(), reason,
	); err != nil {
		return fmt.Errorf("recording create examination for narrative %d: %w", narrativeID, err)
	}

	return nil
}
