package reconciler

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/store"
)

func TestPersistWritesEveryProposedActionAsProposed(t *testing.T) {
	s := reconcileStore(t)
	nid := seedLinkedNarrative(t, s, "PROJ-1", store.Role("primary"), codeEvent("e1", "did work"))

	persisted, err := Persist(s, []ReconcileResult{{
		NarrativeID: nid,
		Proposed: []ProposedAction{
			{
				Type: ActionComment, IssueKey: "PROJ-1", Body: "the work landed",
				Confidence: 0.8, Rationale: "delta shows it",
			},
			{
				Type: ActionTransition, IssueKey: "PROJ-1",
				TargetStatus: "In Review", Confidence: 0.6, Rationale: "finished",
			},
		},
	}})
	require.NoError(t, err)

	got, err := s.ActionsForNarrative(nid)
	require.NoError(t, err)
	require.Len(t, got, 2)

	for _, a := range got {
		assert.Equal(t, "proposed", a.Status,
			"this slice proposes only; nothing is approved or applied here")
		assert.Nil(t, a.DecidedAt)
		assert.Nil(t, a.ExecutedAt)
	}

	assert.Equal(t, "comment", got[0].Type)
	assert.JSONEq(t, `{"body":"the work landed"}`, got[0].Payload)
	assert.Equal(t, "transition", got[1].Type)
	assert.JSONEq(t, `{"target_status":"In Review"}`, got[1].Payload,
		"the persisted payload carries the status NAME: this is the exact string "+
			"gate.Applier hands to SetStatus, which matches on it")

	// Persist's return value is watch's auto-commit seam: the exact rows THIS
	// call wrote, with real ids, so a caller need not re-derive "which actions
	// are freshly proposed" via a status query that would also catch actions
	// from earlier passes a human has not yet triaged.
	require.Len(t, persisted, 2, "Persist must return exactly the rows it wrote")
	assert.ElementsMatch(t, []int64{got[0].ID, got[1].ID},
		[]int64{persisted[0].ID, persisted[1].ID})
	assert.Equal(t, "proposed", persisted[0].Status)
	assert.Equal(t, "proposed", persisted[1].Status)
}

// TestPersistWritesNothingWhenOneActionFails proves the whole-pass
// transaction rolls back: a first, individually-valid action must not survive
// a later action's failure.
//
// The plan's original version of this test forced the mid-transaction
// failure via the actions.narrative_id foreign key. That did not work at the
// time: internal/store never issued `PRAGMA foreign_keys = ON`, and
// modernc.org/sqlite defaults FK enforcement OFF, so an insert naming a
// nonexistent narrative_id silently succeeded instead of erroring.
//
// internal/store now enables foreign-key enforcement (store.sqliteDSN sets
// `_pragma=foreign_keys(1)` in the DSN, since the pragma is per-connection
// and a naive one-shot Exec only reaches whichever pooled connection happens
// to run it — see store.sqliteDSN's doc comment), so a bogus narrative_id
// would fail the same way now. This test still forces the failure via an
// unrecognized ActionType rather than switching to that, though, so it keeps
// exercising Persist's own error path (actionPayload's default case)
// directly instead of depending on a schema-level constraint to fail in the
// right place inside the loop.
func TestPersistWritesNothingWhenOneActionFails(t *testing.T) {
	s := reconcileStore(t)
	nid := seedLinkedNarrative(t, s, "PROJ-1", store.Role("primary"), codeEvent("e1", "did work"))

	persisted, err := Persist(s, []ReconcileResult{
		{NarrativeID: nid, Proposed: []ProposedAction{
			{Type: ActionComment, IssueKey: "PROJ-1", Body: "would be fine", Confidence: 0.8},
		}},
		{NarrativeID: nid, Proposed: []ProposedAction{
			{Type: ActionType("bogus"), IssueKey: "PROJ-1", Body: "unknown type", Confidence: 0.8},
		}},
	})
	require.Error(t, err)
	assert.Empty(t, persisted, "a failed Persist must return no rows: nothing to auto-commit")

	got, err := s.ActionsForNarrative(nid)
	require.NoError(t, err)
	assert.Empty(t, got,
		"Persist is all-or-nothing per pass: the good action must roll back with the bad "+
			"one, or slice 5's auto-commit gate could apply against a half-written set")
}

func TestPersistIsANoOpForResultsWithNoProposals(t *testing.T) {
	s := reconcileStore(t)
	nid := seedLinkedNarrative(t, s, "PROJ-1", store.Role("primary"), codeEvent("e1", "did work"))

	persisted, err := Persist(s, []ReconcileResult{
		{NarrativeID: nid, SkippedNoDelta: true},
	})
	require.NoError(t, err)
	assert.Empty(t, persisted)

	got, err := s.ActionsForNarrative(nid)
	require.NoError(t, err)
	assert.Empty(t, got)
}
