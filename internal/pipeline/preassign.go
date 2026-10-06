package pipeline

// preassign.go is the clustering pre-filter's pull-request identity join (finding F43).
//
// "These events are the same pull request" is a fact about event structure, not a
// judgment — the same kind of fact as "this event is a tracker record", which
// events.PartitionByTrackerRecord already settles before the model is called. So an
// unplaced event whose events.ArtifactPullRequest matches a member of exactly one open
// narrative joins that narrative here, and the model never sees it.
//
// Why it is needed: slice 1's acceptance run cut its window at 2026-09-21, and in all
// six two-pass reps #72's and #73's :merged/:closed events landed in a NEW narrative
// rather than the one holding the same PR's :opened, though both summaries began with
// the identical "jcogilvie/unjira#72:". Under `watch` a PR is opened in one pass and
// merged in another, so that was the steady state, not an edge.
//
// THE LINE IT DRAWS: exact identity, never issue keys. events.ArtifactPullRequest is
// set only where the PR's identity is exact (every github PR event; a claudecode anchor
// whose `gh pr create` result named exactly one PR). Issue <-> PR is many-to-many in
// both directions, so "these share a ticket, so they are the same work" is matching's
// judgment and stays the model's. Nor does this group in-window events among
// themselves: a PR's :opened and :merged arriving together, with no narrative holding
// either, go to the model together, which coalesced them in every measured rep.

import (
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/store"
)

// PRFallbackReason says why an event carrying a pull-request identity was NOT joined by
// it, and went to the model instead.
type PRFallbackReason string

// The reasons a pull-request identity does not name one home.
const (
	// PRNoHolder is the normal path for a PR's first event: nothing holds it yet.
	PRNoHolder PRFallbackReason = "no narrative holds this pull request yet"
	// PRSeveralHolders is F43's damage already persisted: the PR is split, and joining
	// either half would be a guess.
	PRSeveralHolders PRFallbackReason = "several narratives already hold this pull request's work"
	// PRHolderNotOpen: the one holder is split, or in a status this code has never
	// seen. Open is an allowlist, so an unknown lifecycle state is not treated as open.
	PRHolderNotOpen PRFallbackReason = "the one narrative holding this pull request is not open"
)

// PRPreAssignment is one event joined to a narrative by pull-request identity.
type PRPreAssignment struct {
	Event events.Event
	// PullRequest is the events.ArtifactPullRequest the join matched on.
	PullRequest string
	NarrativeID int64
}

// PRFallback is one event that carried a pull-request identity and was sent to the
// model anyway, because the identity did not name exactly one open home.
type PRFallback struct {
	Event       events.Event
	PullRequest string
	Reason      PRFallbackReason
	// Holders is every narrative holding the PR's work, with its status, ascending by
	// id: empty for PRNoHolder, two or more for PRSeveralHolders, the one for
	// PRHolderNotOpen. Reported so the reason can be checked against the store.
	Holders []store.PullRequestHolder
}

// planPRIdentity decides, for each candidate, whether its pull-request identity names
// exactly one open home. A pure function of the candidates and the store's answer
// about who holds each PR, so the rule is testable and lives in one place.
//
// remaining keeps candidates' order and holds every event not placed: the ones with no
// identity, which are not fallbacks because they never had one to fall back from, and
// every fallback.
func planPRIdentity(
	candidates []events.Event, holders map[string][]store.PullRequestHolder,
) (placed []PRPreAssignment, fallbacks []PRFallback, remaining []events.Event) {
	for _, e := range candidates {
		pr := events.PullRequestOf(e)
		if pr == "" {
			remaining = append(remaining, e)

			continue
		}

		hs := holders[pr]
		switch {
		case len(hs) == 1 && hs[0].Status == store.StatusOpen:
			placed = append(placed, PRPreAssignment{Event: e, PullRequest: pr, NarrativeID: hs[0].NarrativeID})

			continue
		case len(hs) == 0:
			fallbacks = append(fallbacks, PRFallback{Event: e, PullRequest: pr, Reason: PRNoHolder})
		case len(hs) > 1:
			fallbacks = append(fallbacks, PRFallback{Event: e, PullRequest: pr, Reason: PRSeveralHolders, Holders: hs})
		default:
			fallbacks = append(fallbacks, PRFallback{Event: e, PullRequest: pr, Reason: PRHolderNotOpen, Holders: hs})
		}
		remaining = append(remaining, e)
	}

	return placed, fallbacks, remaining
}

// pullRequestKeys is the distinct pull-request identities candidates carry.
func pullRequestKeys(candidates []events.Event) []string {
	seen := make(map[string]bool)
	var out []string
	for _, e := range candidates {
		if pr := events.PullRequestOf(e); pr != "" && !seen[pr] {
			seen[pr] = true
			out = append(out, pr)
		}
	}

	return out
}

// preassignByPullRequest runs the join over this pass's clustering candidates and
// returns the candidates left for the model, what it placed, and what fell back.
//
// It WRITES before clustering, unless dryRun, in one transaction that reads the holders
// it decides on, so the decision and the write see one state. Before rather than after
// Persist for two reasons. A placement must not depend on the model: a pass whose
// clustering fails (F44) still places what identity settles, rather than leaving it for
// a later pass whose window may have moved past it. And the alternative — deciding now,
// writing after Persist — would have to re-decide against whatever the model then did,
// and an identity that had become ambiguous meanwhile could only be left unplaced.
// The placed events are then kept out of this pass's prompt by hidePreAssigned.
//
// Each placement is what an extend would write, through the same store operations: a
// member link (Tx.MoveMember, so a fresh link_seq — the reconciler sees the join as new
// work and can propose the transition a merge implies, even on a narrative already
// committed) at store.IdentityMemberConfidence, recorded as store.PlacedByIdentity, and
// window_end moved forward to cover the event (never back, and window_start never
// moved, as Persist's extend does).
//
// A dry run decides the same placements and writes none of them. The window_end
// extension still matters to it, because it can bring a holder that ended before the
// window into it, making it clustering context. So RunNarrate hydrates context from
// plannedWindowEnds in both modes, and a dry run clusters against what the real pass
// would.
//
// ACCEPTED COST: the narrative's summary is not rewritten, because the model never saw
// the event, and no compaction is considered. The reconciler drafts from the new member
// link itself, not from the summary, so this is cosmetic; the summary catches up the
// next time the model extends the narrative.
func preassignByPullRequest(
	s *store.Store, candidates []events.Event, dryRun bool,
) ([]events.Event, []PRPreAssignment, []PRFallback, error) {
	keys := pullRequestKeys(candidates)
	if len(keys) == 0 {
		return candidates, nil, nil, nil
	}

	var (
		placed    []PRPreAssignment
		fallbacks []PRFallback
		remaining []events.Event
	)

	if dryRun {
		holders, err := s.PullRequestMemberHolders(keys)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("finding narratives holding this pass's pull requests: %w", err)
		}
		placed, fallbacks, remaining = planPRIdentity(candidates, holders)

		return remaining, placed, fallbacks, nil
	}

	err := s.WithTx(func(tx *store.Tx) error {
		holders, err := tx.PullRequestMemberHolders(keys)
		if err != nil {
			return fmt.Errorf("finding narratives holding this pass's pull requests: %w", err)
		}
		placed, fallbacks, remaining = planPRIdentity(candidates, holders)

		for _, p := range placed {
			if err := linkPRPreAssignment(tx, p); err != nil {
				return err
			}
		}

		return extendForPreAssignments(tx, placed)
	})
	if err != nil {
		return nil, nil, nil, err
	}

	return remaining, placed, fallbacks, nil
}

// linkPRPreAssignment links p's event to its narrative as an identity-placed member.
func linkPRPreAssignment(tx *store.Tx, p PRPreAssignment) error {
	eventID, err := tx.EventIDByExternalID(p.Event.Source, p.Event.ExternalID)
	if err != nil {
		return fmt.Errorf("resolving %s/%s to join narrative %d by pull request %s: %w",
			p.Event.Source, p.Event.ExternalID, p.NarrativeID, p.PullRequest, err)
	}

	if err := tx.MoveMember(p.NarrativeID, eventID, store.IdentityMemberConfidence, store.PlacedByIdentity); err != nil {
		return fmt.Errorf("joining %s/%s to narrative %d by pull request %s: %w",
			p.Event.Source, p.Event.ExternalID, p.NarrativeID, p.PullRequest, err)
	}

	return nil
}

// plannedWindowEnds is how far the join moves each narrative it places into: to the
// latest event placed there. The one rule for the extension, read by both its writer
// (extendForPreAssignments) and a dry run's context query, so a dry run clusters
// against the narratives the real pass would (F46). Moving only forward is the store
// query's half of the rule and the writer's: neither moves a window_end back.
func plannedWindowEnds(placed []PRPreAssignment) map[int64]time.Time {
	if len(placed) == 0 {
		return nil
	}

	out := make(map[int64]time.Time, len(placed))
	for _, p := range placed {
		if at, ok := out[p.NarrativeID]; !ok || p.Event.OccurredAt.After(at) {
			out[p.NarrativeID] = p.Event.OccurredAt
		}
	}

	return out
}

// extendForPreAssignments moves each placed-into narrative's window_end forward to
// plannedWindowEnds' answer, never back, and leaves window_start alone, as Persist's
// extend does. Ascending by narrative id, so a failure names the same narrative every run.
func extendForPreAssignments(tx *store.Tx, placed []PRPreAssignment) error {
	ends := plannedWindowEnds(placed)
	for _, id := range slices.Sorted(maps.Keys(ends)) {
		row, err := tx.GetNarrative(id)
		if err != nil {
			return fmt.Errorf("reading narrative %d to extend its window: %w", id, err)
		}
		if !ends[id].After(row.WindowEnd) {
			continue
		}

		// row.Summary, unchanged: see preassignByPullRequest's accepted cost.
		if err := tx.ExtendNarrative(id, ends[id], row.Summary); err != nil {
			return fmt.Errorf("extending narrative %d over the event(s) joined to it by pull request: %w", id, err)
		}
	}

	return nil
}

// hidePreAssigned removes every event this pass placed by identity from the hydrated
// context narratives, so the model never sees it, and returns how many member events it
// hid from each narrative. collectCompactions adds those back to the narrative's
// pre-pass visible count: hidden from the prompt, they were still in the store when
// Persist compacted.
//
// Needed because the join writes first: a placed event is now a member of its
// narrative, linked after any applied action, so hydration would otherwise render it
// as that narrative's ELIGIBLE member — numbered, and so movable by the model. The
// narrative itself stays in context (the join usually extends it into this window),
// with its other members, as before.
//
// What the model can still do is move one of the narrative's OTHER eligible members —
// the PR's :opened, say — somewhere else, which would split the PR again. That is the
// reshuffle every eligible member has always been open to, not something this join
// introduces; see docs/architecture-findings.md F45.
func hidePreAssigned(
	existing []correlator.Narrative, placed []PRPreAssignment,
) ([]correlator.Narrative, map[int64]int) {
	if len(placed) == 0 {
		return existing, nil
	}

	hidden := make(map[string]bool, len(placed))
	for _, p := range placed {
		hidden[correlator.EventKey(p.Event)] = true
	}

	removedMembers := make(map[int64]int)
	keep := func(evts []correlator.Event) ([]correlator.Event, int) {
		var out []correlator.Event
		for _, e := range evts {
			if !hidden[correlator.EventKey(e)] {
				out = append(out, e)
			}
		}

		return out, len(evts) - len(out)
	}

	for i := range existing {
		n := &existing[i]
		var frozen, eligible int
		n.Events, frozen = keep(n.Events)
		n.EligibleEvents, eligible = keep(n.EligibleEvents)
		if frozen+eligible > 0 {
			removedMembers[n.ID] = frozen + eligible
		}
		// Never non-empty for a placed event: it had no member home, so it can hold no
		// context link (the one-member-home invariant). Filtered all the same, so the
		// guarantee does not rest on that.
		n.ContextEvents, _ = keep(n.ContextEvents)
	}

	return existing, removedMembers
}
