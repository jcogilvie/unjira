package store

import "fmt"

// SupersedeAction records a reviewer's ruling on one action and replaces it with
// a freshly drafted one, atomically. It is what triage's [e]dit and [t]arget
// call, and it closes two defects that shipped together in PR #26.
//
// First: triage never persisted a reviewer's ruling at all. Session recorded
// reject/edit text in memory and cmd/unjira only ever read Approved(), so a
// rejected action stayed at status=proposed and was re-presented every session,
// and actions.feedback stayed NULL. The triage spec claimed "actions.feedback is
// already persisted by [r]eject/[e]dit, so slice 7 will have its input waiting"
// — that was false. Slice 7's rules.Distill reads actions.feedback, so it would
// have found nothing from any triage session and silently learned nothing.
//
// Second: a replacement action that is never inserted has no id. Probed:
//
//	approved 1 action(s); first ID=0
//
// gate.Applier calls UpdateActionStatusAndError(action.ID, ...), which matches
// zero rows for id 0 and fails — but only at APPLY time, after the reviewer
// already approved the text. Meanwhile the original row sits at proposed
// forever, orphaned. So a redraft must be persisted to be approvable, and the
// row it replaces must be closed out in the same breath.
//
// One transaction for exactly that reason: the old row's ruling and the new
// row's existence are one fact. A crash between them would either lose the
// reviewer's words or leave two live proposals for the same work — and the
// second would let unjira post both.
//
// oldStatus is the caller's ruling on the row being replaced ("edited" for a
// redraft, "rejected" for a retarget whose old link was wrong), not a fixed
// value: updateActionStatusImpl stamps decided_at from it, and a caller that
// meant "rejected" must not be recorded as "edited".
func (s *Store) SupersedeAction(
	oldID int64, oldStatus, feedback string, replacement ActionRow,
) (newID int64, err error) {
	err = s.WithTx(func(tx *Tx) error {
		if err := updateActionStatusImpl(tx.tx, oldID, oldStatus, &feedback, nil); err != nil {
			return err
		}

		id, err := tx.InsertAction(replacement)
		if err != nil {
			return err
		}
		newID = id

		return nil
	})
	if err != nil {
		// Return 0 rather than a partially-assigned id: on rollback the insert
		// never happened, so handing back an id that names no row is the
		// "usable-looking partial result" trap reconciler.Persist documents.
		return 0, fmt.Errorf("superseding action %d: %w", oldID, err)
	}

	return newID, nil
}

// RecordRuling persists a reviewer's disposition on an action without replacing
// it — triage's [r]eject.
//
// Thin by design: it exists so cmd/unjira has one obvious call for "the reviewer
// ruled on this" rather than reaching for UpdateActionStatusAndFeedback directly
// and having to remember that feedback is a *string. The empty-feedback case is
// legitimate here (a reviewer may simply not want the action, with nothing to
// teach), so unlike reconciler.Redraft this does not reject blank text.
func (s *Store) RecordRuling(id int64, status, feedback string) error {
	return updateActionStatusImpl(s.db, id, status, &feedback, nil)
}
