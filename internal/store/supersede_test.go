package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/store"
)

// seedProposedAction inserts a narrative plus one proposed action on it.
func seedProposedAction(t *testing.T, s *store.Store) (narrativeID, actionID int64) {
	t.Helper()

	base := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)
	nid, err := s.InsertNarrative(base, base.Add(time.Hour), "t", "s")
	require.NoError(t, err)

	aid, err := s.InsertAction(store.ActionRow{
		NarrativeID: nid, Type: "comment", IssueKey: "DEVSBX-9",
		Payload: `{"body":"first draft"}`, Confidence: 0.9, Status: "proposed",
	})
	require.NoError(t, err)

	return nid, aid
}

// TestSupersedeAction_ClosesTheOldRowAndPersistsTheNew is the core contract. A
// replacement that is never inserted has no id, and gate.Applier's
// UpdateActionStatusAndError would then match zero rows — failing at APPLY time,
// after the reviewer already approved the text.
func TestSupersedeAction_ClosesTheOldRowAndPersistsTheNew(t *testing.T) {
	s := openStore(t)
	nid, oldID := seedProposedAction(t, s)

	newID, err := s.SupersedeAction(oldID, "edited", "make it shorter", store.ActionRow{
		NarrativeID: nid, Type: "comment", IssueKey: "DEVSBX-9",
		Payload: `{"body":"shorter"}`, Confidence: 0.9, Status: "proposed",
	})

	require.NoError(t, err)
	assert.NotZero(t, newID, "the replacement must be persisted, or it cannot be applied")

	old, err := s.GetAction(oldID)
	require.NoError(t, err)
	assert.Equal(t, "edited", old.Status, "the superseded row must not stay at proposed")

	fresh, err := s.GetAction(newID)
	require.NoError(t, err)
	assert.Equal(t, "proposed", fresh.Status)
	assert.JSONEq(t, `{"body":"shorter"}`, fresh.Payload)
}

// TestSupersedeAction_PersistsTheReviewersWords is the defect that shipped in PR
// #26: triage recorded reject/edit text in memory and never wrote it, so
// actions.feedback stayed NULL and slice 7's rules.Distill would have found
// nothing to learn from. The spec claimed otherwise.
func TestSupersedeAction_PersistsTheReviewersWords(t *testing.T) {
	s := openStore(t)
	nid, oldID := seedProposedAction(t, s)

	_, err := s.SupersedeAction(oldID, "edited", "mention the rollback, not the deploy",
		store.ActionRow{
			NarrativeID: nid, Type: "comment", IssueKey: "DEVSBX-9",
			Payload: `{"body":"rollback"}`, Status: "proposed",
		})
	require.NoError(t, err)

	old, err := s.GetAction(oldID)
	require.NoError(t, err)
	assert.Equal(t, "mention the rollback, not the deploy", old.Feedback,
		"slice 7's rules.Distill reads actions.feedback; without this it learns nothing")
}

// TestSupersedeAction_LeavesExactlyOneLiveProposal: two live proposals for the
// same work would let unjira post BOTH. That is the failure this atomicity
// prevents, so assert the count rather than only the statuses.
func TestSupersedeAction_LeavesExactlyOneLiveProposal(t *testing.T) {
	s := openStore(t)
	nid, oldID := seedProposedAction(t, s)

	_, err := s.SupersedeAction(oldID, "edited", "reword", store.ActionRow{
		NarrativeID: nid, Type: "comment", IssueKey: "DEVSBX-9",
		Payload: `{"body":"reworded"}`, Status: "proposed",
	})
	require.NoError(t, err)

	proposed, err := s.ActionsByStatus("proposed")
	require.NoError(t, err)
	assert.Len(t, proposed, 1,
		"exactly one live proposal, or unjira could post both the old text and the new")
}

// TestSupersedeAction_RollsBackWhenTheInsertFails: the old row's ruling and the
// new row's existence are one fact. A partial commit either loses the reviewer's
// words or strands the original at proposed with no replacement.
func TestSupersedeAction_RollsBackWhenTheInsertFails(t *testing.T) {
	s := openStore(t)
	_, oldID := seedProposedAction(t, s)

	// narrative_id 999999 violates the FK, which store.Open enforces via the
	// DSN's _pragma=foreign_keys(1).
	_, err := s.SupersedeAction(oldID, "edited", "reword", store.ActionRow{
		NarrativeID: 999999, Type: "comment", IssueKey: "DEVSBX-9",
		Payload: `{"body":"orphan"}`, Status: "proposed",
	})

	require.Error(t, err)

	old, err := s.GetAction(oldID)
	require.NoError(t, err)
	assert.Equal(t, "proposed", old.Status,
		"a rolled-back supersede must leave the original untouched, not half-ruled")
	assert.Empty(t, old.Feedback, "the reviewer's words must not persist without the replacement")
}

// TestSupersedeAction_RecordsTheCallersRulingNotAFixedOne: oldStatus drives
// updateActionStatusImpl's decided_at stamping, and a retarget that meant
// "rejected" must not be recorded as "edited".
func TestSupersedeAction_RecordsTheCallersRulingNotAFixedOne(t *testing.T) {
	s := openStore(t)
	nid, oldID := seedProposedAction(t, s)

	_, err := s.SupersedeAction(oldID, "rejected", "wrong ticket entirely", store.ActionRow{
		NarrativeID: nid, Type: "comment", IssueKey: "DEVSBX-10",
		Payload: `{"body":"on the right ticket now"}`, Status: "proposed",
	})
	require.NoError(t, err)

	old, err := s.GetAction(oldID)
	require.NoError(t, err)
	assert.Equal(t, "rejected", old.Status)
}

func TestRecordRuling_PersistsStatusAndFeedback(t *testing.T) {
	s := openStore(t)
	_, id := seedProposedAction(t, s)

	require.NoError(t, s.RecordRuling(id, "rejected", "this is not worth a comment"))

	got, err := s.GetAction(id)
	require.NoError(t, err)
	assert.Equal(t, "rejected", got.Status)
	assert.Equal(t, "this is not worth a comment", got.Feedback)
}

// TestRecordRuling_AcceptsEmptyFeedback: a reviewer may simply not want an
// action, with nothing to teach. Unlike reconciler.Redraft, blank text is
// legitimate here.
func TestRecordRuling_AcceptsEmptyFeedback(t *testing.T) {
	s := openStore(t)
	_, id := seedProposedAction(t, s)

	require.NoError(t, s.RecordRuling(id, "rejected", ""))

	got, err := s.GetAction(id)
	require.NoError(t, err)
	assert.Equal(t, "rejected", got.Status, "a bare reject is still a ruling")
}
