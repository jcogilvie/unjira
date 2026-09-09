package triage

import (
	"context"
	"fmt"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/llm"
	"github.com/jcogilvie/unjira/internal/reconciler"
	"github.com/jcogilvie/unjira/internal/rules"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
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
// Filters on status == store.StatusApplied, NOT on executed_at being set:
// UpdateActionStatus stamps executed_at for both applied AND failed, because a
// failed attempt still attempted a write. But a failed write mutated nothing,
// so it must not make a narrative the workstream of record.
func hasCommittedAction(s *store.Store, narrativeID int64) (bool, error) {
	actions, err := s.ActionsForNarrative(narrativeID)
	if err != nil {
		return false, fmt.Errorf("checking commit state of narrative %d: %w", narrativeID, err)
	}

	for _, a := range actions {
		if a.Status == store.StatusApplied {
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
//
// tracker is a TaskReader, not a TaskWriter or TaskTracker, and that is the
// whole safety story for this type. Redrafting and retargeting must verify live
// state (rules/intent-not-outcome.md) and must never apply anything — batch
// apply's guarantee is that nothing reaches a tracker until the single
// gate.Applier call after Confirm. Typing the field as a reader makes a write
// from here fail to compile rather than relying on this comment.
type StoreHandler struct {
	store   *store.Store
	tracker tasktracker.TaskReader
	client  llm.Client
	rules   []rules.Rule
	// correlator and contextTokens are split's inputs: it re-runs
	// correlator.Cluster and correlator.Persist, which need the compaction
	// thresholds and the model's context window respectively. Held as resolved
	// values rather than a whole config.Config, matching how every other
	// consumer of these takes them — internal/pipeline reads config, and the
	// packages below it see only what they use.
	correlator    config.CorrelatorConfig
	contextTokens int
}

// NewStoreHandler builds the production Handler.
//
// tracker/client may be nil, and that is a supported configuration rather than
// an oversight: `--dry-run` passes no Handler at all, but a caller that could not
// resolve a tracker (no default project configured, say) should still get a
// working session for approve/reject/skip rather than no session. The verbs that
// need those dependencies check for them and report themselves unavailable, the
// same way a nil Handler already does.
//
// No context field: Handler's methods take one per call instead. A context stored
// on a long-lived dependency outlives the call it was made for and cannot be
// cancelled per-operation — golangci-lint's containedctx flags exactly that, and
// heeding it here is right rather than suppressed, since a redraft is a
// cancellable LLM round-trip.
func NewStoreHandler(
	s *store.Store,
	tracker tasktracker.TaskReader,
	client llm.Client,
	learnedRules []rules.Rule,
	correlatorCfg config.CorrelatorConfig,
	contextTokens int,
) *StoreHandler {
	return &StoreHandler{
		store:         s,
		tracker:       tracker,
		client:        client,
		rules:         learnedRules,
		correlator:    correlatorCfg,
		contextTokens: contextTokens,
	}
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

// Redraft satisfies Handler: re-draft this action against the reviewer's
// correction and return the PERSISTED replacement.
//
// Persisted, not just returned, and that is load-bearing. An in-memory
// replacement has no id, and gate.Applier calls
// UpdateActionStatusAndError(action.ID, ...) — which matches zero rows for id 0.
// The failure would surface at APPLY time, after the reviewer already approved
// the new text, while the original row sat at proposed forever. Probed:
// "approved 1 action(s); first ID=0". So the write is part of the operation, not
// a step a caller might forget.
//
// SupersedeAction does both halves in one transaction: it rules on the old row
// (recording the reviewer's words in actions.feedback for slice 7's
// rules.Distill) and inserts the replacement. Two live proposals for the same
// work would let unjira post both.
//
// A narrative whose redraft yields several actions (a same_work pair) keeps only
// the one matching this action's issue: the reviewer edited ONE action, and
// silently replacing its sibling with text they never asked about would be a
// worse surprise than declining.
func (h *StoreHandler) Redraft(
	ctx context.Context, action store.ActionRow, feedback string,
) (store.ActionRow, error) {
	if h.client == nil || h.tracker == nil {
		return store.ActionRow{}, fmt.Errorf(
			"edit is unavailable: this session has no LLM client or tracker")
	}

	drafted, _, err := reconciler.ReworkOne(
		ctx, h.store, h.tracker, h.client, action.NarrativeID, feedback, h.rules)
	if err != nil {
		return store.ActionRow{}, err
	}

	replacement, err := pickForIssue(drafted, action.IssueKey)
	if err != nil {
		return store.ActionRow{}, err
	}

	row, err := h.persistReplacement(action, store.StatusEdited, feedback, replacement)
	if err != nil {
		return store.ActionRow{}, err
	}

	return row, nil
}

// pickForIssue selects the redrafted action for issueKey.
//
// A redraft covers the whole narrative, so a same_work pair yields two actions
// while the reviewer edited only one. Returning the wrong one would post text
// about a different audience's ticket; returning both would replace an action
// nobody asked to change. Erroring when the model dropped the edited issue
// entirely beats substituting a sibling.
func pickForIssue(drafted []reconciler.ProposedAction, issueKey string) (reconciler.ProposedAction, error) {
	for _, d := range drafted {
		if d.IssueKey == issueKey {
			return d, nil
		}
	}

	return reconciler.ProposedAction{}, fmt.Errorf(
		"the redraft produced no action for %s (it returned %d action(s) for other issues); "+
			"nothing was changed", issueKey, len(drafted))
}

// persistReplacement rules on the old row and inserts the new one atomically,
// returning the replacement with its assigned id.
//
// Encodes the payload via reconciler.ActionPayload rather than building JSON
// here: gate.Applier decodes with typed structs mirroring that exact shape, so a
// second encoder would be a silent divergence — a triage-written payload the
// applier could not read.
func (h *StoreHandler) persistReplacement(
	old store.ActionRow, ruling, feedback string, replacement reconciler.ProposedAction,
) (store.ActionRow, error) {
	payload, err := reconciler.ActionPayload(replacement)
	if err != nil {
		return store.ActionRow{}, err
	}

	row := store.ActionRow{
		NarrativeID: old.NarrativeID,
		Type:        string(replacement.Type),
		IssueKey:    replacement.IssueKey,
		Payload:     payload,
		Confidence:  replacement.Confidence,
		Rationale:   replacement.Rationale,
		Status:      store.StatusProposed,
	}

	id, err := h.store.SupersedeAction(old.ID, ruling, feedback, row)
	if err != nil {
		return store.ActionRow{}, err
	}
	row.ID = id

	return row, nil
}

// Retarget moves a narrative's primary link to a different issue and redrafts
// against it — triage's [t]arget, for "right work, wrong ticket."
//
// Verifies the new issue before linking it. That is not defensive politeness: the
// old link was verified by the pass that drafted this action, the new one never
// was, and rules/verify-correlations.md is explicit that a stated key is not
// trusted until confirmed. A typo'd key would otherwise become a stored link
// pointing at nothing.
//
// The link is REPLACED, not added: the partial unique index
// one_primary_per_narrative rejects a second primary row, so RemoveNarrativeIssue
// must run first. Both in one transaction — a crash between them would leave the
// narrative with no primary at all, which reads as untracked work and would put
// it back in matching's backlog.
//
// provenance is ProvenanceReviewer: a human's assertion is not the same kind of
// claim as a model's inference from a branch name, and later passes should be
// able to tell them apart rather than treating this as just another guess.
func (h *StoreHandler) Retarget(
	ctx context.Context, action store.ActionRow, newKey string,
) (store.ActionRow, error) {
	if h.client == nil || h.tracker == nil {
		return store.ActionRow{}, fmt.Errorf(
			"target is unavailable: this session has no LLM client or tracker")
	}

	if newKey == action.IssueKey {
		return store.ActionRow{}, fmt.Errorf(
			"action %d already targets %s; nothing to retarget", action.ID, newKey)
	}

	if _, err := h.tracker.GetIssue(newKey); err != nil {
		return store.ActionRow{}, fmt.Errorf(
			"cannot retarget to %s: %w (the issue must exist before unjira will link work to it)",
			newKey, err)
	}

	if err := h.relinkPrimary(action.NarrativeID, action.IssueKey, newKey); err != nil {
		return store.ActionRow{}, err
	}

	drafted, _, err := reconciler.ReworkOne(
		ctx, h.store, h.tracker, h.client, action.NarrativeID,
		fmt.Sprintf("The reviewer reattributed this work from %s to %s. Draft for %s.",
			action.IssueKey, newKey, newKey),
		h.rules)
	if err != nil {
		return store.ActionRow{}, err
	}

	replacement, err := pickForIssue(drafted, newKey)
	if err != nil {
		return store.ActionRow{}, err
	}

	// store.StatusRejected, not store.StatusEdited: the old action named the
	// wrong issue, so it was not reworded — it was ruled against. slice 7
	// should learn from that distinction rather than seeing every triage
	// correction as a wording tweak.
	return h.persistReplacement(action, store.StatusRejected,
		fmt.Sprintf("retargeted from %s to %s", action.IssueKey, newKey), replacement)
}

// relinkPrimary swaps the narrative's primary link, in one transaction.
func (h *StoreHandler) relinkPrimary(narrativeID int64, oldKey, newKey string) error {
	if err := h.store.WithTx(func(tx *store.Tx) error {
		if oldKey != "" {
			if err := tx.RemoveNarrativeIssue(narrativeID, oldKey); err != nil {
				return err
			}
		}

		return tx.AddNarrativeIssues(narrativeID, []store.NarrativeIssue{{
			IssueKey:   newKey,
			Role:       correlator.RolePrimary,
			Provenance: string(correlator.ProvenanceReviewer),
			Confidence: 1.0,
			Connection: "",
		}})
	}); err != nil {
		return fmt.Errorf("retargeting narrative %d from %s to %s: %w",
			narrativeID, oldKey, newKey, err)
	}

	return nil
}

// Restructure satisfies Handler, dispatching merge and split.
//
// Both return no replacement actions, and that is the shared reason they belong
// here rather than alongside redraft: a restructure invalidates the affected
// narrative's actions by changing which events it holds, and re-deriving text
// requires verified links this handler does not hold. Dropping the action lets the
// next reconcile pass draft against the new shape.
func (h *StoreHandler) Restructure(
	ctx context.Context, d Decision, batch []store.ActionRow,
) ([]store.ActionRow, error) {
	switch d.Verb {
	case VerbTarget:
		return nil, fmt.Errorf(
			"target is dispatched by Session directly, not through Restructure: this is a bug")

	case VerbSplit:
		return h.restructureSplit(ctx, d, batch)

	case VerbMerge:
		return h.restructureMerge(d, batch)

	case VerbApprove, VerbReject, VerbEdit, VerbSkip, VerbQuit:
		return nil, fmt.Errorf("%s is not a restructure", d.Verb)
	}

	return nil, fmt.Errorf("unknown verb %q", d.Verb)
}

// restructureSplit resolves the reviewer's batch position and splits that
// narrative.
//
// Returns no replacement actions: every action on the source described a story
// that no longer exists as one story. The next reconcile pass drafts for the
// narratives the split produced, which is the only way the text can match the new
// clustering.
func (h *StoreHandler) restructureSplit(
	ctx context.Context, d Decision, batch []store.ActionRow,
) ([]store.ActionRow, error) {
	if len(d.Positions) != 1 {
		return nil, fmt.Errorf("split needs one batch position, got %d", len(d.Positions))
	}

	idx := d.Positions[0] - 1
	if idx < 0 || idx >= len(batch) {
		return nil, fmt.Errorf("split position must be within 1..%d", len(batch))
	}

	if _, err := h.SplitNarrative(ctx, batch[idx].NarrativeID); err != nil {
		return nil, err
	}

	return nil, nil
}

// restructureMerge resolves two batch positions and merges those narratives.
func (h *StoreHandler) restructureMerge(
	d Decision, batch []store.ActionRow,
) ([]store.ActionRow, error) {
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
