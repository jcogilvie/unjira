package reconciler

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
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
				TargetStatus: tasktracker.StatusDone, Confidence: 0.6, Rationale: "finished",
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
	assert.JSONEq(t, `{"target_status":"done"}`, got[1].Payload)

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
// failure via the actions.narrative_id foreign key. That does not work here:
// internal/store never issues `PRAGMA foreign_keys = ON` (verified by
// grep — see persist.go's doc comment), and modernc.org/sqlite defaults FK
// enforcement OFF, so an insert naming a nonexistent narrative_id silently
// succeeds instead of erroring. Enabling the pragma would be a repo-wide
// behavior change — every INSERT across every table gets FK-checked, not
// just this one — with unknown blast radius across a schema that has never
// had it on, and connection-pool nuance (modernc.org/sqlite needs it set
// per-connection, e.g. via the DSN, since a naive one-shot Exec only touches
// whichever pooled connection happens to run it). That is out of scope for
// this task, so instead this test forces the same kind of failure — an error
// returned partway through the loop inside Persist's transaction — by giving
// the second action an ActionType actionPayload does not recognize. That
// exercises the exact error path Persist actually has, without touching
// schema-wide behavior.
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
