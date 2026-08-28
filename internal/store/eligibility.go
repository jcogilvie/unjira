package store

import "fmt"

// EligibleEventIDs returns the narrative's event links that may still be
// reshuffled by a reviewer-driven re-cluster: those linked AFTER the
// narrative's most recent committed action.
//
// The unit is the event link, not the narrative, and that distinction is the
// whole design. A narrative can hold both an applied and a proposed action —
// watch applies A, the next tick proposes B against the same narrative. If a
// single applied action froze the entire narrative, triage would refuse to
// merge or split it. But the reviewer's objection in that situation is
// precisely that the events behind B do not belong with the events behind A,
// so a narrative-level freeze would reject the correction exactly when the
// correction is right.
//
// What stays frozen is what a posted comment already described: an unposted
// comment can be redrafted, a posted one cannot be unposted. The watermark for
// "the past" is therefore the last commit, not the narrative's existence.
//
// The comparison is lexical on TEXT columns and safe because linked_at and
// executed_at share the identical strftime('%Y-%m-%dT%H:%M:%fZ') format —
// deliberately, and the same property DeltaEvents depends on. Mixing %f with
// %S would silently invert it ('.' 0x2E sorts before 'Z' 0x5A), which is why
// the actions.created_at schema comment spells that out.
//
// Only status='applied' counts, not merely executed_at being set.
// UpdateActionStatus stamps executed_at for failed writes too — a failed
// attempt still attempted one — but a failed write posted no comment, moved no
// status, and created no issue, so there is nothing for the watermark to
// protect. Freezing on failure would leave a narrative whose only write failed
// permanently unrestructurable, for a reason no human could act on. This
// matches triage's hasCommittedAction deliberately: the two disagreeing was a
// real inconsistency caught while planning, not a subtlety worth keeping.
//
// A narrative with no committed action has no watermark, so every link is
// eligible — expressed as "max(executed_at) IS NULL" rather than a separate
// query, so there is one code path rather than two that could disagree.
func (s *Store) EligibleEventIDs(narrativeID int64) ([]int64, error) {
	rows, err := s.db.Query(
		`SELECT ne.event_id
		 FROM narrative_events ne
		 WHERE ne.narrative_id = ?
		   AND (
		     (SELECT max(executed_at) FROM actions
		       WHERE narrative_id = ? AND status = 'applied') IS NULL
		     OR ne.linked_at > (SELECT max(executed_at) FROM actions
		       WHERE narrative_id = ? AND status = 'applied')
		   )
		 ORDER BY ne.event_id`,
		narrativeID, narrativeID, narrativeID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying eligible event links for narrative %d: %w", narrativeID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning eligible event id for narrative %d: %w", narrativeID, err)
		}
		out = append(out, id)
	}

	return out, rows.Err()
}

// UnlinkNarrativeEvents removes specific event links from a narrative.
func (s *Store) UnlinkNarrativeEvents(narrativeID int64, eventIDs []int64) error {
	return unlinkNarrativeEventsImpl(s.db, narrativeID, eventIDs)
}

func (t *Tx) UnlinkNarrativeEvents(narrativeID int64, eventIDs []int64) error {
	return unlinkNarrativeEventsImpl(t.tx, narrativeID, eventIDs)
}

func unlinkNarrativeEventsImpl(c dbConn, narrativeID int64, eventIDs []int64) error {
	if len(eventIDs) == 0 {
		return nil
	}

	for _, eventID := range eventIDs {
		res, err := c.Exec(
			`DELETE FROM narrative_events WHERE narrative_id = ? AND event_id = ?`,
			narrativeID, eventID,
		)
		if err != nil {
			return fmt.Errorf("unlinking event %d from narrative %d: %w", eventID, narrativeID, err)
		}

		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("checking rows affected unlinking event %d from narrative %d: %w", eventID, narrativeID, err)
		}
		if affected == 0 {
			return fmt.Errorf("unlinking event %d from narrative %d: no such link", eventID, narrativeID)
		}
	}

	return nil
}

// UnlinkEventFromOtherNarratives removes this event's links to every narrative
// except keepNarrativeID.
//
// Exists because correlator.Persist assigns events with AddNarrativeEvents
// (INSERT OR IGNORE), which adds a link and never removes one. That is right for
// a first assignment and wrong for a REASSIGNMENT — and reassignment is
// reachable now that uncommitted events stay eligible for re-clustering as later
// passes learn more. Without this, a re-clustered event is linked to two
// narratives at once: the source narrative looks alive, keeps being fed to future
// Cluster calls as context, and its post-boundary history double-counts the
// event, so compaction folds the wrong number.
//
// Silent when there is nothing to remove, unlike UnlinkNarrativeEvents: a first
// assignment legitimately has no prior link, and this runs on every assignment.
func (t *Tx) UnlinkEventFromOtherNarratives(keepNarrativeID, eventID int64) error {
	if _, err := t.tx.Exec(
		`DELETE FROM narrative_events WHERE event_id = ? AND narrative_id != ?`,
		eventID, keepNarrativeID,
	); err != nil {
		return fmt.Errorf("unlinking event %d from narratives other than %d: %w",
			eventID, keepNarrativeID, err)
	}

	return nil
}
