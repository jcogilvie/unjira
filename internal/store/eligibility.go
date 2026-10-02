package store

import (
	"fmt"

	"github.com/jcogilvie/unjira/internal/events"
)

// linkedSinceLastCommit is the freeze rule for one narrative_events row `ne`: was
// this link made after the narrative's most recent APPLIED action? A link that was
// not is frozen — a posted comment already describes it.
//
// Shared verbatim by EligibleMemberEventIDs/EligibleContextEventIDs (what a restructure
// may move), MoveMember (what Persist may move) and EligibleMemberEvents (what a
// redraft describes), so they cannot disagree about which
// events are still in play. Correlated on ne.narrative_id and free of bound
// parameters, so it drops into either query unchanged.
//
// Compares link SEQUENCE positions, not timestamps (finding F30).
// actions.executed_link_seq is the link high-water mark stamped in the same statement
// as executed_at, so a link made after the commit has a strictly greater link_seq.
// This rule used to read `linked_at > max(executed_at)`: two millisecond timestamps,
// so an event linked in the same millisecond as the commit read as linked BEFORE it
// and was frozen — permanently unmovable by any restructure, for a reason no reviewer
// could see. The sequence is only meaningful because executed_link_seq is recorded
// against the SAME counter link_seq is drawn from; comparing a link's position with a
// different table's clock was the whole problem.
//
// COALESCE to 0 when no action is applied: below every link_seq, so every link is
// eligible. MAX ignores an applied row whose executed_link_seq is NULL (one inserted
// as 'applied' directly, never executed), exactly as the old max(executed_at) ignored
// its NULL executed_at.
const linkedSinceLastCommit = `ne.link_seq > COALESCE(
	(SELECT MAX(a.executed_link_seq) FROM actions a
	  WHERE a.narrative_id = ne.narrative_id AND a.status = '` + StatusApplied + `'), 0)`

// EligibleMemberEventIDs returns the narrative's MEMBER links that may still be
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
// The predicate is linkedSinceLastCommit, shared with EligibleMemberEvents, and it
// compares link SEQUENCE positions rather than timestamps — see that const for
// why (finding F30).
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
// eligible — expressed inside the one predicate rather than as a separate
// query, so there is one code path rather than two that could disagree.
//
// KIND-AWARE (shared-context spec §6): the freeze rule itself is per link and
// kind-agnostic, but what a caller may DO with an eligible link depends on its kind —
// a merge moves an eligible member with Tx.MoveMember and re-homes an eligible context
// link with Tx.AddContext — so the two kinds are separate accessors rather than one
// list a caller would have to partition. Renamed from EligibleEventIDs, which returned
// both kinds mixed, so each caller was revisited.
func (s *Store) EligibleMemberEventIDs(narrativeID int64) ([]int64, error) {
	return eligibleLinkIDs(s.db, narrativeID, LinkMember)
}

// EligibleContextEventIDs is EligibleMemberEventIDs for context links: the context
// links a merge re-homes onto its target. A frozen context link stays where it is, like
// any frozen link (spec §4's table).
func (s *Store) EligibleContextEventIDs(narrativeID int64) ([]int64, error) {
	return eligibleLinkIDs(s.db, narrativeID, LinkContext)
}

// ContextEventIDs returns the event id of every context link narrativeID holds,
// eligible or frozen, ascending. For triage's split, which deletes all of an emptied
// source's context links and reports how many there were.
func (s *Store) ContextEventIDs(narrativeID int64) ([]int64, error) {
	return linkIDs(s.db, narrativeID, LinkContext, "1 = 1")
}

func eligibleLinkIDs(c dbConn, narrativeID int64, kind LinkKind) ([]int64, error) {
	return linkIDs(c, narrativeID, kind, linkedSinceLastCommit)
}

// linkIDs returns narrativeID's event ids of one kind whose link satisfies predicate
// (over `ne`, with no bound parameters), ascending.
func linkIDs(c dbConn, narrativeID int64, kind LinkKind, predicate string) ([]int64, error) {
	rows, err := c.Query(
		`SELECT ne.event_id
		 FROM narrative_events ne
		 WHERE ne.narrative_id = ? AND ne.kind = ?
		   AND `+predicate+`
		 ORDER BY ne.event_id`,
		narrativeID, string(kind),
	)
	if err != nil {
		return nil, fmt.Errorf("querying %s links for narrative %d: %w", kind, narrativeID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning %s link event id for narrative %d: %w", kind, narrativeID, err)
		}
		out = append(out, id)
	}

	return out, rows.Err()
}

// UnlinkNarrativeEvents removes specific event links from a narrative.
func (s *Store) UnlinkNarrativeEvents(narrativeID int64, eventIDs []int64) error {
	return unlinkNarrativeEventsImpl(s.db, narrativeID, eventIDs)
}

// UnlinkNarrativeEvents is the *Tx-scoped variant of
// (*Store).UnlinkNarrativeEvents. A restructure uses it so the unlink and the
// replacing link land in one transaction: a crash between them would leave the
// events attached to neither narrative.
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

// EligibleMemberEvents returns the same links EligibleMemberEventIDs selects,
// hydrated into full events and ordered for reading (occurred_at, then id).
//
// Members only, and renamed from EligibleEvents so each caller was revisited: its
// two callers are redraft's delta (reconciler.ReworkOne) and triage's split, and
// neither may see context. A redraft describing a context event would draft another
// ticket's work onto this one (shared-context spec §2); a split re-clusters the
// source's WORK, and its context links are handled separately (§6).
//
// This is the redraft delta, and it is deliberately NOT DeltaEvents. DeltaEvents
// is bounded by the narrative's latest action of any status, so once ANY action
// exists for a narrative it returns none of the events already linked — which is
// right for "should we propose again" and exactly wrong for "redraft the action
// that already exists," because the action being edited is itself what
// suppresses its own source events. Verified
// by probe: 1 event before inserting an action, 0 after.
//
// The commit watermark is the correct bound instead, and not merely as a
// workaround: a redraft should describe every piece of work no tracker mutation
// has claimed yet, which is what "linked after the last applied action" means.
// It is the same invariant triage's restructures already run on, so an edit and
// a merge agree about which events are still in play.
func (s *Store) EligibleMemberEvents(narrativeID int64) ([]events.Event, error) {
	return queryLinkedEvents(s.db, "eligible member events", narrativeID,
		memberLink+` AND `+linkedSinceLastCommit)
}
