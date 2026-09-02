package reconciler

// unguarded_test.go exercises noteUnguarded, the annotation that makes task
// #174's known gap visible per narrative: suppressStaleTransitions degrades to
// proposing when it has no collected status history for an issue
// (verifiedLink.HaveLastStatus false), and until this existed that degradation
// was silent — a transition proposed with no staleness check at all looked
// identical, in ReconcileResult, to one the guard actually cleared.
//
// It reads the SAME verifiedLink.HaveLastStatus the guard itself consults rather
// than re-deriving the fact from the store. That is not merely cheaper: it makes
// the two structurally unable to disagree about whether a check happened, where
// two independent lookups could drift.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/store"
)

// unguardedLink builds a verifiedLink for key whose collected-history state is
// haveHistory — the exact input suppressStaleTransitions branches on.
func unguardedLink(key string, haveHistory bool) verifiedLink {
	return verifiedLink{
		Link:           store.NarrativeIssue{IssueKey: key, Role: store.Role("primary")},
		HaveLastStatus: haveHistory,
	}
}

// TestNoteUnguarded_FlagsATransitionWithNoCollectedHistory is the core case: a
// transition reached ReconcileResult.Proposed (so it already survived
// suppressStaleTransitions), but unjira has never collected a status change for
// this issue.
//
// Without this, that fact is invisible: a reviewer sees an ordinary-looking
// proposed transition with no signal that the staleness guard never ran. If this
// regresses to empty, a real handoff-fighting case (docs/design-notes.md incident
// 19) is silently unguarded on every issue with no collected history, and nobody
// reviewing the queue knows to look harder.
func TestNoteUnguarded_FlagsATransitionWithNoCollectedHistory(t *testing.T) {
	result := ReconcileResult{
		NarrativeID: 1,
		Proposed: []ProposedAction{
			{Type: ActionTransition, IssueKey: "PAAS-1", TargetStatus: "In Review", Confidence: 0.9},
		},
	}

	noteUnguarded(&result, []verifiedLink{unguardedLink("PAAS-1", false)})

	require.Len(t, result.Unguarded, 1)
	assert.Equal(t, "PAAS-1", result.Unguarded[0].IssueKey)
	assert.Equal(t, "In Review", result.Unguarded[0].TargetStatus)
	assert.Equal(t, int64(1), result.Unguarded[0].NarrativeID)
	assert.Contains(t, result.Unguarded[0].Reason(), "no staleness check",
		"the reason must say the check did not run, not merely that something is unusual")
}

// TestNoteUnguarded_LeavesAGuardedTransitionAlone: when history exists the guard
// DID run and either cleared the transition or suppressed it. Flagging it anyway
// would cry wolf on every transition, which trains a reviewer to ignore the
// signal — worse than not having it.
func TestNoteUnguarded_LeavesAGuardedTransitionAlone(t *testing.T) {
	result := ReconcileResult{
		NarrativeID: 1,
		Proposed: []ProposedAction{
			{Type: ActionTransition, IssueKey: "PAAS-1", TargetStatus: "In Review", Confidence: 0.9},
		},
	}

	noteUnguarded(&result, []verifiedLink{unguardedLink("PAAS-1", true)})

	assert.Empty(t, result.Unguarded)
}

// TestNoteUnguarded_IgnoresNonTransitionActions: the staleness guard only ever
// judges transitions, so a comment or create was never going to be guarded, and
// flagging one would attach a warning to an action the guard has no opinion
// about.
func TestNoteUnguarded_IgnoresNonTransitionActions(t *testing.T) {
	result := ReconcileResult{
		NarrativeID: 1,
		Proposed: []ProposedAction{
			{Type: ActionComment, IssueKey: "PAAS-1", Body: "what changed", Confidence: 0.9},
			{Type: ActionCreate, Summary: "new work", Body: "d", Confidence: 0.9},
		},
	}

	noteUnguarded(&result, []verifiedLink{unguardedLink("PAAS-1", false)})

	assert.Empty(t, result.Unguarded)
}

// TestNoteUnguarded_JudgesEachIssueSeparately: one narrative can hold a same_work
// pair where only one side has collected history. Flagging both, or neither,
// would misreport which specific proposal needs scrutiny — and the whole value of
// this signal is that it points at one action.
func TestNoteUnguarded_JudgesEachIssueSeparately(t *testing.T) {
	result := ReconcileResult{
		NarrativeID: 7,
		Proposed: []ProposedAction{
			{Type: ActionTransition, IssueKey: "PAAS-1", TargetStatus: "In Review", Confidence: 0.9},
			{Type: ActionTransition, IssueKey: "SUMO-9", TargetStatus: "Done", Confidence: 0.9},
		},
	}

	noteUnguarded(&result, []verifiedLink{
		unguardedLink("PAAS-1", true),  // guarded
		unguardedLink("SUMO-9", false), // not
	})

	require.Len(t, result.Unguarded, 1)
	assert.Equal(t, "SUMO-9", result.Unguarded[0].IssueKey)
}

// TestNoteUnguarded_SkipsAnUnverifiedIssueKey: an action whose key is not among
// the verified links has no HaveLastStatus to read. actionsFromVerdicts already
// drops unrecognized keys, so reaching here means a bug elsewhere — claiming such
// an action is unguarded would fabricate a finding from a different defect and
// point the reviewer at the wrong thing.
func TestNoteUnguarded_SkipsAnUnverifiedIssueKey(t *testing.T) {
	result := ReconcileResult{
		NarrativeID: 1,
		Proposed: []ProposedAction{
			{Type: ActionTransition, IssueKey: "GHOST-1", TargetStatus: "In Review", Confidence: 0.9},
		},
	}

	noteUnguarded(&result, []verifiedLink{unguardedLink("PAAS-1", false)})

	assert.Empty(t, result.Unguarded)
}

// TestNoteUnguarded_AgreesWithTheGuardItself is the property that makes reading
// HaveLastStatus better than re-querying the store: the two must never disagree
// about whether a check happened.
//
// The same verifiedLink goes into both. suppressStaleTransitions passes the
// transition through untouched (it cannot judge without history), and
// noteUnguarded flags exactly that pass-through. If either side ever derived the
// fact independently, this is where the drift would show.
func TestNoteUnguarded_AgreesWithTheGuardItself(t *testing.T) {
	v := unguardedLink("PAAS-1", false)
	action := ProposedAction{
		Type: ActionTransition, IssueKey: "PAAS-1", TargetStatus: "In Review", Confidence: 0.9,
	}

	kept, suppressed := suppressStaleTransitions(nil, []verifiedLink{v}, []ProposedAction{action})

	require.Len(t, kept, 1, "no history means the guard degrades to proposing")
	assert.Empty(t, suppressed)

	result := ReconcileResult{NarrativeID: 1, Proposed: kept}
	noteUnguarded(&result, []verifiedLink{v})

	require.Len(t, result.Unguarded, 1,
		"and the same input must make that degradation visible")
}
