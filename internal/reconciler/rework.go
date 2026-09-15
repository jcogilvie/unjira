package reconciler

import (
	"context"
	"fmt"

	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/llm"
	"github.com/jcogilvie/unjira/internal/rules"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// ReworkOne re-drafts one narrative's actions against a reviewer's correction,
// verifying live tracker state first. It is what triage's [e]dit and [t]arget
// actually call.
//
// This exists because Redraft, despite being exported, was NOT callable from
// another package: its `verified []verifiedLink` parameter names an unexported
// type. Probed rather than assumed — naming the type fails to compile:
//
//	name verifiedLink not exported by package reconciler
//
// and the one form that DOES compile, passing nil, is silently useless:
//
//	err=<nil> actions=0
//	reconciler: narrative 1 redraft named unrecognized issue_key "DEVSBX-9", ignoring
//
// Because actionsFromVerdicts drops every verdict whose issue_key is not in the
// verified set, a nil `verified` discards the model's entire response and returns
// no error. A caller reaching for Redraft directly therefore gets a successful
// no-op — an LLM call spent, a reviewer told nothing, no action replaced. So the
// unexported type was not merely inconvenient; it made the only compiling call
// shape a silent failure.
//
// Keeping verifiedLink unexported is right — it pairs a store row with live
// tracker state read under a specific pass, and letting a caller construct one
// would let it assert verification that never happened, which is precisely what
// rules/verify-correlations.md forbids. The fix is a function that performs the
// verification itself rather than accepting a claim of it.
//
// So this re-verifies rather than reusing anything. Redraft's own doc comment
// says it deliberately reuses the caller's already-verified links to avoid a
// second round-trip, which is sound inside one Reconcile pass — but triage is a
// SEPARATE process from the watch pass that drafted these actions, possibly
// minutes or days later. There is no verification from "this same pass" to
// reuse, so a fresh GetIssue is the only way the invariant holds at all.
func ReworkOne(
	ctx context.Context,
	s *store.Store,
	tracker tasktracker.TaskReader,
	client llm.Client,
	narrativeID int64,
	feedback string,
	learnedRules []rules.Rule,
) ([]ProposedAction, correlator.Stats, error) {
	narrative, err := s.GetNarrative(narrativeID)
	if err != nil {
		return nil, correlator.Stats{}, fmt.Errorf("loading narrative %d for rework: %w", narrativeID, err)
	}

	links, err := s.NarrativeIssues(narrativeID)
	if err != nil {
		return nil, correlator.Stats{}, fmt.Errorf("loading links for narrative %d: %w", narrativeID, err)
	}

	actionable := actionableLinks(links)
	if len(actionable) == 0 {
		return nil, correlator.Stats{}, fmt.Errorf(
			"narrative %d has no actionable link to redraft against: retarget it to an issue first",
			narrativeID)
	}

	// nil logger: ReworkOne is triage's synchronous path (see Redraft's own doc
	// comment on this call chain having no logger to thread today), unlike
	// Reconcile's unattended watch-pass caller.
	verified, _, err := verifyLinks(s, tracker, narrativeID, actionable, nil)
	if err != nil {
		return nil, correlator.Stats{}, err
	}

	if len(verified) == 0 {
		return nil, correlator.Stats{}, fmt.Errorf(
			"narrative %d: no linked issue verified against the live tracker, so there is nothing "+
				"to redraft against", narrativeID)
	}

	// EligibleEvents, NOT DeltaEvents. DeltaEvents is bounded by
	// max(actions.created_at), so once the action being edited exists it returns
	// nothing at all — see store.EligibleEvents' doc comment for the probe. The
	// commit watermark is both correct here and the same bound every restructure
	// uses, so an edit and a merge agree on which events are in play.
	delta, err := s.EligibleEvents(narrativeID)
	if err != nil {
		return nil, correlator.Stats{}, err
	}

	delta = dropSelfAuthored(delta)
	if len(delta) == 0 {
		return nil, correlator.Stats{}, fmt.Errorf(
			"narrative %d has no uncommitted events left to describe: every event it holds is "+
				"already covered by an applied action", narrativeID)
	}

	// suppressStaleTransitions is deliberately NOT applied here, though
	// verifyLinks populated the state for it.
	//
	// Reconcile's guard exists because unjira proposes transitions on its own
	// initiative from work evidence that can go stale. Rework is a human at the
	// triage prompt saying "redraft this, here is what was wrong with it" — the
	// initiative is theirs and it is current by construction. Filtering the result
	// would answer an explicit request with silence, and the reviewer would see
	// their edit produce nothing with no way to tell why.
	//
	// They still approve whatever comes back, and the applier still validates
	// every transition against live state before executing it.
	return Redraft(ctx, client, narrative, delta, verified, learnedRules, feedback)
}
