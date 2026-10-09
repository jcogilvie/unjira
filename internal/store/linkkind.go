package store

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/jcogilvie/unjira/internal/events"
)

// LinkKind is what a narrative_events link says about its event: that the event is
// this narrative's work, or relevant background to it. See the schema comment on
// narrative_events.kind and docs/superpowers/specs/2026-10-02-shared-context-design.md
// §1 for why these names and not primary/supporting (narrative_issues.role already owns
// "primary").
type LinkKind string

// The two link kinds.
const (
	// LinkMember says the event is part of this narrative's work. Every linked event has
	// exactly one member link (one_member_link_per_event, and Persist's commit check),
	// and member links are the only kind any delta, watermark, matching or drafting path
	// reads.
	LinkMember LinkKind = "member"
	// LinkContext says the event is relevant background for this narrative and is some
	// other narrative's work. Never read by the reconciler or matching.
	LinkContext LinkKind = "context"
)

// memberLink is the SQL predicate restricting a narrative_events row `ne` to member
// links. One const so the predicates that must agree — the reconciler's delta, both
// examination watermarks, the candidate selector — cannot spell it differently.
const memberLink = `ne.kind = '` + string(LinkMember) + `'`

// ReviewerMemberConfidence is the member_confidence a link gets when a REVIEWER placed
// it rather than the model — today, triage's merge, where the reviewer has ruled that
// the source's work belongs to the target. 1.0 because it is a human's ruling, not a
// stated guess, and because a reviewer-placed attribution must not resurface in triage
// as one to confirm. The same reasoning gives a reviewer-made narrative_issues link
// confidence 1.0 (triage.relinkPrimary).
const ReviewerMemberConfidence = 1.0

// IdentityMemberConfidence is the member_confidence of a link the clustering
// pre-filter placed by exact pull-request identity (PlacedByIdentity): the event
// carries the same events.ArtifactPullRequest as a member of the one open narrative it
// joined. 1.0 because it is a fact about the events' structure, not anyone's estimate,
// and for ReviewerMemberConfidence's second reason: it must not resurface in triage as
// an attribution to confirm.
const IdentityMemberConfidence = 1.0

// MemberPlacement is who placed a member link: narrative_events.member_placement.
//
// Recorded because member_confidence cannot say it — identity and reviewer placements
// record 1.0, and so may the model — and because the one planned reader of
// member_confidence needs the difference: correlator.member_confidence_floor is to be
// calibrated against reviewer rulings, which measures the MODEL's stated confidence,
// and counting identity placements as the model being right would inflate it.
type MemberPlacement string

// The three placements.
const (
	// PlacedByModel is a clustering answer (Persist), including a split's.
	PlacedByModel MemberPlacement = "model"
	// PlacedByIdentity is the clustering pre-filter's pull-request identity join: the
	// event was never shown to the model.
	PlacedByIdentity MemberPlacement = "identity"
	// PlacedByReviewer is triage's merge.
	PlacedByReviewer MemberPlacement = "reviewer"
)

// MoveMember makes eventID a member of narrativeID, removing its member link from
// wherever else it is: what an event's appearance in a cluster's event_indices means.
// confidence and by are recorded on the new member link (member_confidence,
// member_placement).
//
// Its counterpart is AddContext. Two operations with different names, replacing the
// old relinkEvents, so that no caller can express "add another home" while meaning
// "move this event" or the reverse (shared-context spec §4). Cases:
//
//   - eventID is already narrativeID's member: nothing changes. The row, its link_seq,
//     its member_confidence and its member_placement are kept, so a re-linked frozen
//     event stays frozen
//     (TestPersist_ExtendRelinkingAFrozenEventKeepsItFrozen).
//   - eventID is another narrative's member: that link is deleted FIRST — the partial
//     unique index rejects a second member row — and must be ELIGIBLE (linked after its
//     narrative's last applied action). A frozen member is never numbered in a
//     clustering prompt and is never on a merge or split's eligible list, so finding one
//     here is a bug, reported as an error rather than silently moving committed work
//     (design-notes #13's laundering shape).
//   - narrativeID holds eventID as CONTEXT: that row is replaced by a member row with a
//     NEW link_seq. A link's position means when it acquired its current kind, and
//     background that becomes this narrative's work today is new work today — keeping
//     the old position would put it below the last action's created_link_seq and the
//     reconciler would never draft about it.
//
// Context links held by other narratives are untouched.
func (t *Tx) MoveMember(narrativeID, eventID int64, confidence float64, by MemberPlacement) error {
	var (
		holder   int64
		eligible int
	)
	err := t.tx.QueryRow(
		`SELECT ne.narrative_id, `+linkedSinceLastCommit+`
		 FROM narrative_events ne
		 WHERE ne.event_id = ? AND `+memberLink,
		eventID,
	).Scan(&holder, &eligible)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		// No member home yet: a first assignment.
	case err != nil:
		return fmt.Errorf("reading the member link of event %d: %w", eventID, err)
	case holder == narrativeID:
		return nil
	case eligible == 0:
		return fmt.Errorf(
			"moving event %d onto narrative %d: its member link on narrative %d is frozen (linked "+
				"before that narrative's last applied action), so a posted tracker mutation already "+
				"describes it there. A frozen member is never offered for reassignment, so reaching "+
				"this is a bug", eventID, narrativeID, holder)
	default:
		if _, err := t.tx.Exec(
			`DELETE FROM narrative_events WHERE narrative_id = ? AND event_id = ? AND kind = ?`,
			holder, eventID, string(LinkMember),
		); err != nil {
			return fmt.Errorf("moving event %d off narrative %d: %w", eventID, holder, err)
		}
	}

	if _, err := t.tx.Exec(
		`DELETE FROM narrative_events WHERE narrative_id = ? AND event_id = ? AND kind = ?`,
		narrativeID, eventID, string(LinkContext), t.now(),
	); err != nil {
		return fmt.Errorf("replacing narrative %d's context link to event %d: %w", narrativeID, eventID, err)
	}

	if _, err := t.tx.Exec(
		`INSERT INTO narrative_events (narrative_id, event_id, kind, member_confidence, member_placement, linked_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		narrativeID, eventID, string(LinkMember), confidence, string(by), t.now(),
	); err != nil {
		return fmt.Errorf("linking event %d to narrative %d as a member: %w", eventID, narrativeID, err)
	}

	return nil
}

// AddContext gives narrativeID a context link to eventID: what an event's appearance in
// a cluster's context_indices means. Reports whether a row was inserted.
//
// NEVER deletes anything — that is the whole contract, and why it is a separate
// operation from MoveMember. If narrativeID already holds eventID under either kind it
// does nothing: a member link wins over context (the event is this narrative's work,
// which says more than "relevant background"), and a repeated context link keeps its
// position.
//
// The conflict target is named explicitly rather than INSERT OR IGNORE, which would
// also swallow CHECK and NOT NULL failures — a context row that violated the schema
// would vanish instead of failing.
func (t *Tx) AddContext(narrativeID, eventID int64) (bool, error) {
	res, err := t.tx.Exec(
		`INSERT INTO narrative_events (narrative_id, event_id, kind, linked_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT (narrative_id, event_id) DO NOTHING`,
		narrativeID, eventID, string(LinkContext), t.now(),
	)
	if err != nil {
		return false, fmt.Errorf("linking event %d to narrative %d as context: %w", eventID, narrativeID, err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("checking rows affected linking event %d to narrative %d as context: %w",
			eventID, narrativeID, err)
	}

	return n > 0, nil
}

// EventsWithoutMemberHome returns those of eventIDs that hold no member link, ascending.
//
// The "at least one" half of the every-event-has-one-member-home invariant, which a
// partial unique index cannot express. Persist calls it at commit for every event it
// gave a context link: a context-only event would never be in any narrative's delta, so
// the work it records would never be reconciled — a silent drop.
func (t *Tx) EventsWithoutMemberHome(eventIDs []int64) ([]int64, error) {
	var out []int64
	for _, id := range eventIDs {
		var n int
		if err := t.tx.QueryRow(
			`SELECT COUNT(*) FROM narrative_events WHERE event_id = ? AND kind = ?`, id, string(LinkMember),
		).Scan(&n); err != nil {
			return nil, fmt.Errorf("checking event %d for a member link: %w", id, err)
		}
		if n == 0 {
			out = append(out, id)
		}
	}

	return out, nil
}

// MemberHolders returns, ascending and each once, the narratives holding any of
// eventIDs as a member. An event with no member link contributes nothing, and a context
// link never makes its narrative a holder.
//
// Persist reads it before moving a pass's members: a narrative can only be emptied by
// the pass that moves a member off it, so these are the narratives it must check
// afterwards (MarkSplitIfEmptied).
func (t *Tx) MemberHolders(eventIDs []int64) ([]int64, error) {
	seen := make(map[int64]bool)
	var out []int64
	for _, id := range eventIDs {
		var holder int64
		err := t.tx.QueryRow(
			`SELECT ne.narrative_id FROM narrative_events ne WHERE ne.event_id = ? AND `+memberLink, id,
		).Scan(&holder)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			continue
		case err != nil:
			return nil, fmt.Errorf("reading the member home of event %d: %w", id, err)
		}
		if !seen[holder] {
			seen[holder] = true
			out = append(out, holder)
		}
	}
	slices.Sort(out)

	return out, nil
}

// LinkMembers links each event to narrativeID as a member, at confidence, recorded as
// PlacedByModel.
//
// For seeding a store directly, which is what every caller does: each seeds links
// standing in for a clustering answer, hence PlacedByModel. A production caller placing
// events by any other means uses Tx.MoveMember, which makes it name its placement. It
// ADDS and never moves: an event that is another
// narrative's member fails on one_member_link_per_event rather than being taken from
// it, and an event this narrative holds as context fails on the (narrative_id,
// event_id) uniqueness rather than being silently upgraded. Use Tx.MoveMember for a
// move. An event already this narrative's member is left as it is.
func (s *Store) LinkMembers(narrativeID int64, eventIDs []int64, confidence float64) error {
	for _, eid := range eventIDs {
		var kind string
		err := s.db.QueryRow(
			`SELECT kind FROM narrative_events WHERE narrative_id = ? AND event_id = ?`, narrativeID, eid,
		).Scan(&kind)
		if err == nil && kind == string(LinkMember) {
			continue
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("reading narrative %d's link to event %d: %w", narrativeID, eid, err)
		}

		if _, err := s.db.Exec(
			`INSERT INTO narrative_events (narrative_id, event_id, kind, member_confidence, member_placement, linked_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			narrativeID, eid, string(LinkMember), confidence, string(PlacedByModel), s.now(),
		); err != nil {
			return fmt.Errorf("linking event %d to narrative %d as a member: %w", eid, narrativeID, err)
		}
	}

	return nil
}

// LinkContext gives narrativeID a context link to each event, with AddContext's
// semantics. Like LinkMembers, for seeding a store directly. It does not check the
// member-home invariant: that is Persist's commit check, and a seeded store may build
// the member half afterwards.
func (s *Store) LinkContext(narrativeID int64, eventIDs []int64) error {
	return s.WithTx(func(tx *Tx) error {
		for _, eid := range eventIDs {
			if _, err := tx.AddContext(narrativeID, eid); err != nil {
				return err
			}
		}

		return nil
	})
}

// NarrativeLink is one narrative_events row as a test or a report reads it.
type NarrativeLink struct {
	Kind    LinkKind
	LinkSeq int64
	// MemberConfidence is nil for a context link.
	MemberConfidence *float64
	// Placement is who placed a member link; empty for a context link.
	Placement MemberPlacement
}

// NarrativeEventLink returns the (narrativeID, eventID) link, or an error wrapping
// sql.ErrNoRows when there is none. A test-support introspection accessor, on the
// precedent of NarrativeEventLinkedAt: it is how a test outside this package proves an
// upgrade issued a new position or a re-link kept the old one.
func (s *Store) NarrativeEventLink(narrativeID, eventID int64) (NarrativeLink, error) {
	var (
		out        NarrativeLink
		kind       string
		confidence sql.NullFloat64
		placement  sql.NullString
	)
	if err := s.db.QueryRow(
		`SELECT kind, link_seq, member_confidence, member_placement
		 FROM narrative_events WHERE narrative_id = ? AND event_id = ?`,
		narrativeID, eventID,
	).Scan(&kind, &out.LinkSeq, &confidence, &placement); err != nil {
		return NarrativeLink{}, fmt.Errorf("reading narrative %d's link to event %d: %w", narrativeID, eventID, err)
	}

	out.Kind = LinkKind(kind)
	out.Placement = MemberPlacement(placement.String)
	if confidence.Valid {
		c := confidence.Float64
		out.MemberConfidence = &c
	}

	return out, nil
}

// ContextEvents returns every event narrativeID holds as context, ordered by
// (occurred_at, id), ignoring the compaction boundary.
//
// The accessor for context, kept apart from every member reader so that no reconciler
// or matching path can reach context by accident (shared-context spec §2; incident 13's
// lesson that an unsafe read should be inexpressible rather than filtered by each
// caller). Read by reporting — the narration pass summary and triage — and by nothing
// that drafts or attributes.
func (s *Store) ContextEvents(narrativeID int64) ([]events.Event, error) {
	return queryLinkedEvents(s.db, "context events", narrativeID, `ne.kind = '`+string(LinkContext)+`'`)
}

// ContextEventsAfterBoundary is ContextEvents bounded by the narrative's compaction
// boundary, exactly as MemberEventsAfterBoundary bounds members: what a clustering
// prompt hydrates under a context narrative. The boundary filters by EVENT position
// whatever the link's kind (shared-context spec §7), so context accumulated before a
// compaction stops being re-sent with every later prompt.
func (s *Store) ContextEventsAfterBoundary(narrativeID int64) ([]events.Event, error) {
	return queryLinkedEvents(s.db, "post-boundary context events", narrativeID,
		`ne.kind = '`+string(LinkContext)+`' AND `+afterCompactionBoundary)
}

// afterCompactionBoundary restricts event `e`, linked via `ne`, to those strictly after
// ne's narrative's compaction boundary — every one when the boundary is NULL. See
// MemberEventsAfterBoundary for why the comparison is on the (occurred_at, id) pair.
const afterCompactionBoundary = `(
	(SELECT compaction_boundary FROM narratives WHERE id = ne.narrative_id) IS NULL
	OR (e.occurred_at, e.id) > (
	  (SELECT compaction_boundary FROM narratives WHERE id = ne.narrative_id),
	  (SELECT compaction_boundary_event_id FROM narratives WHERE id = ne.narrative_id)
	)
)`

// queryLinkedEvents returns narrativeID's linked events matching predicate (over `ne`
// and `e`, with no bound parameters), ordered by (occurred_at, id). what names the
// population in errors.
func queryLinkedEvents(c dbConn, what string, narrativeID int64, predicate string) ([]events.Event, error) {
	rows, err := c.Query(
		`SELECT e.source, e.external_id, e.occurred_at, e.actor, e.summary, e.artifacts, e.raw_ref
		 FROM narrative_events ne
		 JOIN events e ON e.id = ne.event_id
		 WHERE ne.narrative_id = ?
		   AND `+predicate+`
		 ORDER BY e.occurred_at, e.id`,
		narrativeID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying %s for narrative %d: %w", what, narrativeID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []events.Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning %s row for narrative %d: %w", what, narrativeID, err)
		}
		out = append(out, e)
	}

	return out, rows.Err()
}

// MemberAttribution is one member link and the confidence it was placed with.
type MemberAttribution struct {
	Event      events.Event
	Confidence float64
}

// MembersBelowConfidence returns narrativeID's member links whose member_confidence is
// strictly below floor, ordered by (occurred_at, id) — the attributions triage asks a
// reviewer to confirm (correlator.member_confidence_floor). A floor of 0 or less returns
// nothing, since no confidence is below it: the knob ships off.
func (s *Store) MembersBelowConfidence(narrativeID int64, floor float64) ([]MemberAttribution, error) {
	if floor <= 0 {
		return nil, nil
	}

	rows, err := s.db.Query(
		`SELECT e.source, e.external_id, e.occurred_at, e.actor, e.summary, e.artifacts, e.raw_ref,
		        ne.member_confidence
		 FROM narrative_events ne
		 JOIN events e ON e.id = ne.event_id
		 WHERE ne.narrative_id = ? AND `+memberLink+` AND ne.member_confidence < ?
		 ORDER BY e.occurred_at, e.id`,
		narrativeID, floor,
	)
	if err != nil {
		return nil, fmt.Errorf("querying below-floor member links for narrative %d: %w", narrativeID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []MemberAttribution
	for rows.Next() {
		var a MemberAttribution
		e, err := scanEventWithExtra(rows, &a.Confidence)
		if err != nil {
			return nil, fmt.Errorf("scanning below-floor member link for narrative %d: %w", narrativeID, err)
		}
		a.Event = e
		out = append(out, a)
	}

	return out, rows.Err()
}

// MemberEventCount is how many member links narrativeID has: whether it still holds
// any work, which a narrative holding only background does not. A test-support
// accessor; the production form of the question is asked inside the transaction that
// moved the members, by Tx.MarkSplitIfEmptied.
func (s *Store) MemberEventCount(narrativeID int64) (int, error) {
	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM narrative_events ne WHERE ne.narrative_id = ? AND `+memberLink, narrativeID,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting member links for narrative %d: %w", narrativeID, err)
	}

	return n, nil
}

// trailingScan scans an event row that carries extra columns AFTER scanEvent's seven,
// so a query can select an event plus link metadata without duplicating scanEvent's
// parsing.
type trailingScan struct {
	row   scanRow
	extra []any
}

func (x trailingScan) Scan(dest ...any) error {
	return x.row.Scan(append(dest, x.extra...)...)
}

// scanEventWithExtra is scanEvent for a row whose trailing columns scan into extra.
func scanEventWithExtra(row scanRow, extra ...any) (events.Event, error) {
	return scanEvent(trailingScan{row: row, extra: extra})
}
