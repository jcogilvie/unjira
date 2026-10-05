package store

import "fmt"

// StatusSplit marks a narrative left holding no member link because its work was
// moved to other narratives: by triage's [s]plit or [m]erge, or by a clustering pass
// that placed every eligible member in other clusters. Tx.MarkSplitIfEmptied is the
// one place that decides it.
//
// The row is kept rather than deleted. Its actions still reference it via
// actions.narrative_id — including any APPLIED action, whose record of "unjira
// posted this comment about this work" must survive — and so do its narrative_issues
// links, and events is append-only, so deleting the narrative would strand rows that
// name it. A status is the smaller, reversible change.
const StatusSplit = "split"

// StatusOpen is the default every narrative starts at (the schema's DEFAULT
// 'open'). Named so the two live in one place rather than as bare strings.
const StatusOpen = "open"

// SetNarrativeStatus records a narrative's lifecycle state, unconditionally. For
// seeding a store; production code marks a narrative split only through
// Tx.MarkSplitIfEmptied, which first decides that it holds no work.
//
// The status exists because an emptied narrative that stays 'open' is still selected
// by NarrativesOverlapping, so it is rendered into every subsequent Cluster prompt as
// an existing narrative holding zero events. Verified by probe — after an all-new
// split the source held 0 events and was still returned by that query. A model
// shown an empty narrative as context is being asked to reason about nothing, and
// it also shows up in status output as a story with no substance.
func (s *Store) SetNarrativeStatus(id int64, status string) error {
	return setNarrativeStatusImpl(s.db, id, status)
}

// MarkSplitIfEmptied marks narrativeID StatusSplit when it holds no MEMBER link, and
// in that case deletes every context link it holds. Reports how many context links it
// held and whether it was marked.
//
// The one answer to "what does a narrative become once its work has moved", shared by
// every path that moves members: correlator.Persist (a clustering pass that placed
// every eligible member of a context narrative in other clusters, finding F40), and
// triage's split and merge. Each used to decide this for itself, and only split did,
// so the other two left a narrative open with a title and summary describing work it
// no longer held, returned by NarrativesOverlapping into every later prompt.
//
// Members, not links of any kind: a narrative holding only background holds no work
// (shared-context spec §6). Its context links are deleted rather than left on a row
// nothing reads, which loses nothing, since every linked event keeps its member home
// elsewhere — frozen ones included, as split has always done. Everything else on the
// row is kept: its narrative_issues links, and its actions, applied ones among them
// (see StatusSplit).
//
// Only an open narrative is marked, and an already-split one is re-marked, which
// changes nothing. Any other status is refused rather than overwritten: it is a
// lifecycle state this function has never seen, and whoever adds one must decide what
// emptying it means.
func (t *Tx) MarkSplitIfEmptied(narrativeID int64) (contextLinks int, emptied bool, err error) {
	row, err := t.GetNarrative(narrativeID)
	if err != nil {
		return 0, false, fmt.Errorf("checking whether narrative %d was emptied: %w", narrativeID, err)
	}

	var members int
	if err := t.tx.QueryRow(
		`SELECT COUNT(*) FROM narrative_events ne WHERE ne.narrative_id = ? AND `+memberLink, narrativeID,
	).Scan(&members); err != nil {
		return 0, false, fmt.Errorf("counting member links for narrative %d: %w", narrativeID, err)
	}

	background, err := linkIDs(t.tx, narrativeID, LinkContext, "1 = 1")
	if err != nil {
		return 0, false, err
	}

	if members > 0 {
		return len(background), false, nil
	}

	if row.Status != StatusOpen && row.Status != StatusSplit {
		return 0, false, fmt.Errorf(
			"narrative %d holds no member link, but its status %q is neither %q nor %q, so marking it %q "+
				"would overwrite a lifecycle state this code does not know", narrativeID, row.Status,
			StatusOpen, StatusSplit, StatusSplit)
	}

	if err := unlinkNarrativeEventsImpl(t.tx, narrativeID, background); err != nil {
		return 0, false, err
	}
	if err := setNarrativeStatusImpl(t.tx, narrativeID, StatusSplit); err != nil {
		return 0, false, err
	}

	return len(background), true, nil
}

func setNarrativeStatusImpl(c dbConn, id int64, status string) error {
	res, err := c.Exec(`UPDATE narratives SET status = ? WHERE id = ?`, status, id)
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
