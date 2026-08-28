package triage

import (
	"fmt"

	"github.com/jcogilvie/unjira/internal/store"
)

// commitState is whether unjira has already mutated the tracker for a
// narrative. A struct rather than a bare bool pair so the both-committed case
// gets named treatment instead of an inline `if a && b` a reader has to decode.
type commitState struct {
	NarrativeID int64
	Committed   bool
}

// resolveMergeTarget applies direction-by-commitment: the committed narrative
// absorbs the other, because unjira has already mutated the tracker on its
// behalf and that makes it the workstream of record.
//
// "Mutated" covers three things, and a comment is the mildest of them. A
// transition destroyed the prior status (Discovery -> Done loses "it was in
// Discovery" outside the changelog), and a create produced an object other
// people now reference. unjira can undo none of the three.
//
// Refuses both-committed for that reason: two committed workstreams means two
// tracker issues already claim this work, and choosing which is authoritative
// is an org-level decision, not one a review loop should make silently.
//
// This is also what keeps the per-narrative watermark sound under
// restructuring. EligibleEventIDs compares against the narrative's OWN
// max(executed_at), so relinking a frozen event onto a never-committed
// narrative would make it eligible again — probed directly while designing
// this: frozen on A, eligible on B. Because the committed narrative is always
// the target, frozen events are never relinked at all, so that hazard is
// unreachable rather than merely forbidden.
func resolveMergeTarget(a, b commitState) (target, source int64, err error) {
	switch {
	case a.Committed && b.Committed:
		return 0, 0, fmt.Errorf(
			"cannot merge narratives %d and %d: both have committed actions, so two tracker "+
				"issues already claim this work. unjira cannot retract a comment, un-transition a "+
				"status, or un-create an issue — decide which issue is authoritative in the "+
				"tracker first", a.NarrativeID, b.NarrativeID)
	case a.Committed:
		return a.NarrativeID, b.NarrativeID, nil
	case b.Committed:
		return b.NarrativeID, a.NarrativeID, nil
	default:
		// Neither committed: the reviewer's first-named narrative wins, keeping
		// `m 1 3` predictable. Nothing is at stake either way, since no tracker
		// mutation references either narrative yet.
		return a.NarrativeID, b.NarrativeID, nil
	}
}

// hasCommittedAction reports whether any action for this narrative actually
// reached the tracker.
//
// Filters on status == "applied", NOT on executed_at being set:
// UpdateActionStatus stamps executed_at for both applied AND failed, because a
// failed attempt still attempted a write. But a failed write mutated nothing,
// so it must not make a narrative the workstream of record.
func hasCommittedAction(s *store.Store, narrativeID int64) (bool, error) {
	actions, err := s.ActionsForNarrative(narrativeID)
	if err != nil {
		return false, fmt.Errorf("checking commit state of narrative %d: %w", narrativeID, err)
	}

	for _, a := range actions {
		if a.Status == "applied" {
			return true, nil
		}
	}

	return false, nil
}

// StoreHandler is the production Handler: it performs redrafts and
// restructures against the real store, correlator, and reconciler.
//
// Lives here rather than in cmd/ so the operations are testable without
// driving a terminal, and so cmd/unjira/triage.go stays a pure I/O shell.
type StoreHandler struct {
	store *store.Store
}

// NewStoreHandler builds the production Handler.
func NewStoreHandler(s *store.Store) *StoreHandler {
	return &StoreHandler{store: s}
}

// MergeNarratives moves the source narrative's eligible events onto the target,
// unlinking them from the source so nothing is double-linked.
//
// Direction is resolved by commitment, never by argument order — see
// resolveMergeTarget. Only ELIGIBLE events move: a frozen event stays on the
// narrative whose tracker mutation already describes it, which is what makes the
// per-narrative watermark sound under restructuring.
//
// Runs in one transaction. A crash between the link and the unlink would leave
// events attached to both narratives, which is the exact double-link this
// function exists to avoid.
func (h *StoreHandler) MergeNarratives(targetID, sourceID int64) (moved []int64, err error) {
	eligible, err := h.store.EligibleEventIDs(sourceID)
	if err != nil {
		return nil, err
	}

	if len(eligible) == 0 {
		return nil, fmt.Errorf(
			"narrative %d has no uncommitted events to merge: every event it holds is already "+
				"described by a tracker mutation and cannot be reattributed", sourceID)
	}

	if err := h.store.WithTx(func(tx *store.Tx) error {
		if err := tx.AddNarrativeEvents(targetID, eligible); err != nil {
			return err
		}

		return tx.UnlinkNarrativeEvents(sourceID, eligible)
	}); err != nil {
		return nil, fmt.Errorf("merging narrative %d into %d: %w", sourceID, targetID, err)
	}

	return eligible, nil
}

// ResolveMergeTarget reads both narratives' commit state and returns which
// absorbs which, per direction-by-commitment.
//
// Returns the decision rather than the commit states themselves: a caller wants
// the answer, and exposing commitState would leak an internal type through an
// exported signature for no benefit. Refuses both-committed — see
// resolveMergeTarget for why that is an org-level decision rather than a
// review-loop one.
func (h *StoreHandler) ResolveMergeTarget(aID, bID int64) (target, source int64, err error) {
	aCommitted, err := hasCommittedAction(h.store, aID)
	if err != nil {
		return 0, 0, err
	}

	bCommitted, err := hasCommittedAction(h.store, bID)
	if err != nil {
		return 0, 0, err
	}

	return resolveMergeTarget(
		commitState{NarrativeID: aID, Committed: aCommitted},
		commitState{NarrativeID: bID, Committed: bCommitted},
	)
}

// Redraft satisfies Handler. Not yet implemented: wiring it needs an llm.Client
// and the narrative's verified links, which cmd/unjira has but this handler is
// not yet given. Returns a clear error rather than a silent no-op so a reviewer
// asking for an edit learns it is unavailable instead of believing it worked.
func (h *StoreHandler) Redraft(_ store.ActionRow, _ string) (store.ActionRow, error) {
	return store.ActionRow{}, fmt.Errorf(
		"edit is not wired yet: reconciler.Redraft exists but this handler has no LLM client")
}

// Restructure satisfies Handler, dispatching merge/split/retarget.
//
// Only merge is implemented. Split and retarget report themselves unavailable
// rather than silently doing nothing — a reviewer must not believe a
// restructure happened when it did not.
func (h *StoreHandler) Restructure(d Decision, batch []store.ActionRow) ([]store.ActionRow, error) {
	if d.Verb != VerbMerge {
		return nil, fmt.Errorf("%s is not wired yet", d.Verb)
	}

	if len(d.Positions) != 2 {
		return nil, fmt.Errorf("merge needs two batch positions, got %d", len(d.Positions))
	}

	aIdx, bIdx := d.Positions[0]-1, d.Positions[1]-1
	if aIdx < 0 || aIdx >= len(batch) || bIdx < 0 || bIdx >= len(batch) {
		return nil, fmt.Errorf("merge positions must be within 1..%d", len(batch))
	}

	target, source, err := h.ResolveMergeTarget(batch[aIdx].NarrativeID, batch[bIdx].NarrativeID)
	if err != nil {
		return nil, err
	}

	if _, err := h.MergeNarratives(target, source); err != nil {
		return nil, err
	}

	// The merged narrative's actions are now stale: their delta changed. Return
	// nothing, so the affected action is dropped from the batch and the next
	// reconcile pass drafts fresh ones. Re-deriving here would need a
	// reconciler with a live tracker, which this handler does not hold — and
	// silently keeping the stale action would present text describing the wrong
	// story.
	return nil, nil
}
