package reconciler

// unguarded_test.go exercises FindUnguardedTransitions, the seam that makes
// task #174's known gap visible per narrative: suppressStaleTransitions
// degrades to proposing when it has no collected status history for an
// issue (verifiedLink.HaveLastStatus false), and until this existed that
// degradation was silent — a transition proposed with no staleness check at
// all looked identical, in ReconcileResult, to one the guard actually
// cleared.
//
// This lives in its own file rather than editing recency.go/types.go/
// reconciler.go/recency_test.go: those four files are owned by a parallel,
// in-flight change this session must not conflict with (per the task
// brief). FindUnguardedTransitions therefore recomputes "was there
// collected history" itself, via the same store.LatestStatusEvent
// verifyLinks already calls, rather than reading a HaveLastStatus field
// ReconcileResult does not (and, for the length of this constraint, cannot)
// expose.

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/store"
)

// unguardedStore opens a fresh temp-file store, mirroring reconcileStore in
// reconciler_test.go (unexported there, reused here since both files share
// package reconciler).
func unguardedStore(t *testing.T) *store.Store {
	t.Helper()

	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	return s
}

// TestFindUnguardedTransitions_FlagsATransitionWithNoCollectedHistory is the
// core case: a transition reached ReconcileResult.Proposed (so it already
// survived suppressStaleTransitions), but unjira has never collected a
// status change for this issue. Without this function, that fact is
// invisible: a reviewer sees an ordinary-looking proposed transition with no
// signal that the staleness guard never ran at all. If this test regresses
// to empty, a real handoff-fighting case (docs/design-notes.md incident 19)
// could be silently unguarded on every issue with no jira collector
// history, and nobody reviewing the queue would know to look harder.
func TestFindUnguardedTransitions_FlagsATransitionWithNoCollectedHistory(t *testing.T) {
	s := unguardedStore(t)

	results := []ReconcileResult{{
		NarrativeID: 1,
		Proposed: []ProposedAction{
			{Type: ActionTransition, IssueKey: "PAAS-1", TargetStatus: "In Review", Confidence: 0.9},
		},
	}}

	got, err := FindUnguardedTransitions(s, results)
	require.NoError(t, err)

	require.Len(t, got, 1)
	assert.Equal(t, int64(1), got[0].NarrativeID)
	assert.Equal(t, "PAAS-1", got[0].IssueKey)
	assert.Equal(t, "In Review", got[0].TargetStatus)
}

// TestFindUnguardedTransitions_LeavesAGuardedTransitionAlone: once unjira has
// collected at least one status change for the issue, the guard could run
// (whatever it concluded — a proposal only reaches ReconcileResult.Proposed
// after surviving it), so this transition must NOT be reported as unguarded.
// If this regressed to always-flag, every transition would read as
// suspect and the signal this function exists to add would be worthless
// noise.
func TestFindUnguardedTransitions_LeavesAGuardedTransitionAlone(t *testing.T) {
	s := unguardedStore(t)

	_, err := s.InsertEvent(jiraStatusEvent("PAAS-1:status:1", "PAAS-1", "Ready for Dev", "In Progress",
		time.Date(2026, 8, 25, 8, 0, 0, 0, time.UTC)))
	require.NoError(t, err)

	results := []ReconcileResult{{
		NarrativeID: 1,
		Proposed: []ProposedAction{
			{Type: ActionTransition, IssueKey: "PAAS-1", TargetStatus: "In Review", Confidence: 0.9},
		},
	}}

	got, err := FindUnguardedTransitions(s, results)
	require.NoError(t, err)
	assert.Empty(t, got, "collected history exists, so the guard could run for this issue")
}

// TestFindUnguardedTransitions_IgnoresNonTransitionActions: the guard is
// about transitions only (suppressStaleTransitions leaves comments and
// creates alone unconditionally — see recency_test.go's
// TestSuppressStaleTransitions_LeavesCommentsAlone). Flagging a comment as
// "unguarded" would be a category error: there was never a guard for it to
// bypass.
func TestFindUnguardedTransitions_IgnoresNonTransitionActions(t *testing.T) {
	s := unguardedStore(t)

	results := []ReconcileResult{{
		NarrativeID: 1,
		Proposed: []ProposedAction{
			{Type: ActionComment, IssueKey: "PAAS-1", Body: "the work landed", Confidence: 0.9},
		},
	}}

	got, err := FindUnguardedTransitions(s, results)
	require.NoError(t, err)
	assert.Empty(t, got, "a comment was never subject to the staleness guard")
}

// TestFindUnguardedTransitions_ScansEveryNarrativeIndependently: a real pass
// reconciles many narratives, and one issue having collected history must
// not suppress the flag for a different issue that has none — each proposed
// transition is judged on its OWN issue's history.
func TestFindUnguardedTransitions_ScansEveryNarrativeIndependently(t *testing.T) {
	s := unguardedStore(t)

	_, err := s.InsertEvent(jiraStatusEvent("PAAS-1:status:1", "PAAS-1", "Ready for Dev", "In Progress",
		time.Date(2026, 8, 25, 8, 0, 0, 0, time.UTC)))
	require.NoError(t, err)

	results := []ReconcileResult{
		{
			NarrativeID: 1,
			Proposed: []ProposedAction{
				{Type: ActionTransition, IssueKey: "PAAS-1", TargetStatus: "In Review", Confidence: 0.9},
			},
		},
		{
			NarrativeID: 2,
			Proposed: []ProposedAction{
				{Type: ActionTransition, IssueKey: "PAAS-2", TargetStatus: "In Progress", Confidence: 0.9},
			},
		},
	}

	got, err := FindUnguardedTransitions(s, results)
	require.NoError(t, err)

	require.Len(t, got, 1, "only PAAS-2 (no collected history) should be flagged")
	assert.Equal(t, int64(2), got[0].NarrativeID)
	assert.Equal(t, "PAAS-2", got[0].IssueKey)
}
