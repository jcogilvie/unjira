package store

import "fmt"

// StatusSplit marks a narrative whose events were all moved to other narratives
// by triage's [s]plit.
//
// The row is kept rather than deleted. Its actions still reference it via
// actions.narrative_id — including any APPLIED action, whose record of "unjira
// posted this comment about this work" must survive — and events is append-only,
// so deleting the narrative would strand rows that name it. A status is the
// smaller, reversible change.
const StatusSplit = "split"

// StatusOpen is the default every narrative starts at (the schema's DEFAULT
// 'open'). Named so the two live in one place rather than as bare strings.
const StatusOpen = "open"

// SetNarrativeStatus records a narrative's lifecycle state.
//
// Exists for split: an emptied narrative that stays 'open' is still selected by
// NarrativesOverlapping, so it is rendered into every subsequent Cluster prompt as
// an existing narrative holding zero events. Verified by probe — after an all-new
// split the source held 0 events and was still returned by that query. A model
// shown an empty narrative as context is being asked to reason about nothing, and
// it also shows up in status output as a story with no substance.
func (s *Store) SetNarrativeStatus(id int64, status string) error {
	res, err := s.db.Exec(`UPDATE narratives SET status = ? WHERE id = ?`, status, id)
	if err != nil {
		return fmt.Errorf("setting narrative %d status to %q: %w", id, status, err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking rows affected setting narrative %d status: %w", id, err)
	}
	if affected == 0 {
		// Loud rather than silent: a caller naming a narrative that does not
		// exist has a bug, and a no-op UPDATE would hide it.
		return fmt.Errorf("setting narrative %d status to %q: no such narrative", id, status)
	}

	return nil
}
