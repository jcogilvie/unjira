package triage

import (
	"context"
	"fmt"

	"github.com/jcogilvie/unjira/internal/correlator"
)

// splitInstruction is what the reviewer's "this is two stories" becomes in the
// clustering prompt.
//
// Phrased as an instruction rather than a hint because the reviewer has already
// judged: they looked at the drafted text and the events and concluded the cluster
// is wrong. Cluster's own prompt still decides WHERE the seam falls — the reviewer
// is not asked to partition events at a terminal, which would be an unreasonable
// thing to ask and is information the model is better placed to supply.
//
// "at least two" rather than "exactly two": a reviewer saying "this is more than
// one story" has not committed to a count, and a model that finds three should say
// three rather than forcing a seam it does not believe in.
const splitInstruction = `A reviewer has judged that the events below do NOT belong to a single narrative. ` +
	`Cluster them into at least two separate narratives, each a coherent story on its own. ` +
	`Do not return a single cluster containing all of them.`

// SplitNarrative re-clusters one narrative's ELIGIBLE events into separate
// narratives, at the reviewer's instruction.
//
// Only eligible events move, which is the same rule merge follows and for the same
// reason: an event a posted comment already describes cannot be reattributed,
// because unjira cannot unpost the comment. A narrative holding one committed and
// two uncommitted events therefore splits into "the committed one stays" plus
// whatever the model makes of the rest — the posted comment stays true about
// exactly what it described. Refusing the whole split in that case was considered
// and rejected: merge deliberately supports mixed state, and refusing here would
// make the reviewer's correction impossible precisely when the narrative has
// already been partly acted on.
//
// The window handed to Cluster spans the eligible events rather than the
// narrative's stored window. A narrative whose committed half sits at the start
// would otherwise get a window reaching back before anything Cluster may assign,
// and filterEventsInWindow would then drop nothing while filterAdjacentOrOverlapping
// pulled in unrelated neighbours.
//
// Returns the narratives the split produced. The source is marked
// store.StatusSplit when it ends up empty — see sourceContextOutcome.
//
// MEMBER events only (shared-context spec §6). The split re-clusters the source's
// work; its Cluster call gets no context narratives, so everything numbered is the
// source's, and Persist's MoveMember deletes only the source's member links.
// context_indices between the halves are allowed — one half's investigation may
// genuinely be background for the other. The source's OWN context links are not
// redistributed (more model judgment inside a reviewer-attended command, for
// background nobody asked about) nor copied to every half (over-sharing): they stay
// if the source keeps any member, and are deleted if the split empties it, which
// loses nothing because each such event keeps its member home elsewhere. Both are
// counted in the result.
func (h *StoreHandler) SplitNarrative(
	ctx context.Context, narrativeID int64,
) (SplitResult, error) {
	if h.client == nil {
		return SplitResult{}, fmt.Errorf(
			"split is unavailable: this session has no LLM client")
	}

	eligible, err := h.store.EligibleMemberEvents(narrativeID)
	if err != nil {
		return SplitResult{}, err
	}

	if len(eligible) < 2 {
		// One event cannot be two stories, and zero means everything is frozen.
		// Both are refusals a reviewer can act on, unlike a "split" that silently
		// returns the narrative unchanged.
		return SplitResult{}, fmt.Errorf(
			"narrative %d has %d uncommitted event(s), so there is nothing to split: a split needs "+
				"at least two events that no tracker mutation already describes",
			narrativeID, len(eligible))
	}

	opts := append([]correlator.ClusterOption{
		correlator.WithClusterRules(h.rules),
		correlator.WithInstruction(splitInstruction),
	}, h.clusterReaskOptions()...)
	results, _, err := correlator.Cluster(
		ctx, eligible, nil, h.client,
		windowSpanning(eligible), h.contextTokens, opts...)
	if err != nil {
		return SplitResult{}, fmt.Errorf("re-clustering narrative %d for a split: %w", narrativeID, err)
	}

	if len(results) < 2 {
		// The model was told to produce at least two and did not. Reported rather
		// than forced: the reviewer may be wrong, and a seam invented to satisfy
		// the instruction would be worse than saying so.
		return SplitResult{}, fmt.Errorf(
			"the model kept narrative %d as one story despite the split instruction; "+
				"nothing was changed", narrativeID)
	}

	// Every result is forced to ClusterNew. Cluster was given no existing
	// narratives, so it cannot legitimately return ClusterExtends — but a model
	// that invents a narrative_id anyway would have Persist extend an unrelated
	// narrative, moving this work somewhere nobody asked for. Normalizing here is
	// cheaper than trusting the response.
	for i := range results {
		results[i].Kind = correlator.ClusterNew
		results[i].NarrativeID = 0
	}

	touched, stats, err := correlator.Persist(
		ctx, h.store, h.client, results, h.correlator)
	if err != nil {
		return SplitResult{}, fmt.Errorf("persisting the split of narrative %d: %w", narrativeID, err)
	}

	out := SplitResult{Narratives: touched}
	if out.SourceContextLinks, out.SourceContextLinksDeleted, err = h.sourceContextOutcome(narrativeID, stats); err != nil {
		return SplitResult{}, err
	}

	return out, nil
}

// SplitResult is what a split produced, and what became of the source's own
// context links.
type SplitResult struct {
	Narratives []correlator.Narrative
	// SourceContextLinks is how many context links the source held after the split
	// moved its members. They were deleted when SourceContextLinksDeleted is true
	// (the split emptied the source of members) and kept otherwise.
	SourceContextLinks        int
	SourceContextLinksDeleted bool
}

// sourceContextOutcome reports how many context links the source held after the split
// moved its members, and whether they were deleted because the split emptied it.
//
// The marking itself is Persist's, inside the split's own transaction: the source is
// the narrative the split's results take every eligible member from, so Persist checks
// it with store.Tx.MarkSplitIfEmptied once the members have moved, as it does for any
// narrative a pass empties (F40). That function is also where the rule lives — members
// are what count, so a source left holding only background holds no work (shared-
// context spec §6), and deleting its context links loses nothing, since every linked
// event keeps its member home elsewhere.
//
// Conditional, not unconditional: a narrative that kept its committed events is still
// a live story with a tracker mutation describing it, and marking it split would hide
// it from future clustering while its issue is still being worked. Its context links
// stay with it, and are counted here.
func (h *StoreHandler) sourceContextOutcome(narrativeID int64, stats correlator.Stats) (contextLinks int, deleted bool, err error) {
	for _, e := range stats.Emptied {
		if e.NarrativeID == narrativeID {
			return e.ContextLinksDeleted, true, nil
		}
	}

	background, err := h.store.ContextEventIDs(narrativeID)
	if err != nil {
		return 0, false, fmt.Errorf("reading context links left on narrative %d after a split: %w", narrativeID, err)
	}

	return len(background), false, nil
}

// windowSpanning returns the smallest TimeRange covering evts.
//
// End is the last event's timestamp plus a nanosecond because Cluster's window is
// half-open [Start, End) — filterEventsInWindow would otherwise drop the very last
// event, which is both the newest and the one a reviewer most likely just saw.
func windowSpanning(evts []correlator.Event) correlator.TimeRange {
	lo, hi := evts[0].OccurredAt, evts[0].OccurredAt
	for _, e := range evts[1:] {
		if e.OccurredAt.Before(lo) {
			lo = e.OccurredAt
		}
		if e.OccurredAt.After(hi) {
			hi = e.OccurredAt
		}
	}

	return correlator.TimeRange{Start: lo, End: hi.Add(1)}
}
