package triage_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/triage"
)

// TestSessionDispatchesTargetToRetarget: Session recorded VerbTarget and routed
// it to Restructure before this wiring, where it hit the "not wired yet" branch.
// This asserts the dispatch, since a mis-wired verb would silently fall through
// to the merge path.
func TestSessionDispatchesTargetToRetarget(t *testing.T) {
	batch := []store.ActionRow{
		{ID: 1, Type: "comment", IssueKey: "DEVSBX-9", Payload: `{"body":"x"}`},
	}
	h := &stubHandler{replacement: store.ActionRow{
		ID: 2, Type: "comment", IssueKey: "DEVSBX-42", Payload: `{"body":"y"}`,
	}}
	p := &scriptedPrompter{answers: []triage.Decision{
		{Verb: triage.VerbTarget, Text: "DEVSBX-42"},
		{Verb: triage.VerbApprove},
	}}

	s := triage.NewSession(context.Background(), batch, p, h)
	require.NoError(t, s.Run())

	assert.Equal(t, []string{"DEVSBX-42"}, h.retargetCalls,
		"target must reach Retarget, not Restructure")
	assert.Empty(t, h.restructureCalls)

	approved := s.Approved()
	require.Len(t, approved, 1)
	assert.Equal(t, "DEVSBX-42", approved[0].IssueKey,
		"the retargeted action is what gets approved, after being re-presented")
}

// TestSessionRulings_ExposesRejectsSoTheyCanBePersisted is the shipped-defect
// regression. PR #26 recorded rejects in memory and cmd/unjira never read them,
// so a rejected action stayed at proposed and actions.feedback stayed NULL —
// while the spec claimed the opposite.
func TestSessionRulings_ExposesRejectsSoTheyCanBePersisted(t *testing.T) {
	batch := []store.ActionRow{
		{ID: 1, Type: "comment", IssueKey: "DEVSBX-9", Payload: `{"body":"x"}`},
		{ID: 2, Type: "comment", IssueKey: "DEVSBX-10", Payload: `{"body":"y"}`},
		{ID: 3, Type: "comment", IssueKey: "DEVSBX-11", Payload: `{"body":"z"}`},
	}
	p := &scriptedPrompter{answers: []triage.Decision{
		{Verb: triage.VerbReject, Text: "not worth a comment"},
		{Verb: triage.VerbApprove},
		{Verb: triage.VerbSkip},
	}}

	s := triage.NewSession(context.Background(), batch, p, nil)
	require.NoError(t, s.Run())

	rulings := s.Rulings()

	require.Len(t, rulings, 1, "only the reject needs persisting")
	assert.Equal(t, int64(1), rulings[0].ActionID)
	assert.Equal(t, "rejected", rulings[0].Status)
	assert.Equal(t, "not worth a comment", rulings[0].Feedback,
		"the reviewer's words are slice 7's only input")
}

// TestSessionRulings_ExcludesApproveAndSkip: approve goes through gate.Applier,
// which sets its own status; skip leaving the action at proposed IS the outcome.
// Recording either here would double-write or destroy the skip.
func TestSessionRulings_ExcludesApproveAndSkip(t *testing.T) {
	batch := []store.ActionRow{
		{ID: 1, Type: "comment", IssueKey: "DEVSBX-9", Payload: `{"body":"x"}`},
		{ID: 2, Type: "comment", IssueKey: "DEVSBX-10", Payload: `{"body":"y"}`},
	}
	p := &scriptedPrompter{answers: []triage.Decision{
		{Verb: triage.VerbApprove},
		{Verb: triage.VerbSkip},
	}}

	s := triage.NewSession(context.Background(), batch, p, nil)
	require.NoError(t, s.Run())

	assert.Empty(t, s.Rulings(),
		"approve is gate.Applier's business and skip is a deliberate non-outcome")
}

// TestSessionRulings_QuitDiscardsRulingsToo: quit abandons the session, and a
// reviewer who quits expects nothing to have happened — including no ruling
// written for something they rejected earlier in the same pass.
func TestSessionRulings_QuitDiscardsRulingsToo(t *testing.T) {
	batch := []store.ActionRow{
		{ID: 1, Type: "comment", IssueKey: "DEVSBX-9", Payload: `{"body":"x"}`},
		{ID: 2, Type: "comment", IssueKey: "DEVSBX-10", Payload: `{"body":"y"}`},
	}
	p := &scriptedPrompter{answers: []triage.Decision{
		{Verb: triage.VerbReject, Text: "no"},
		{Verb: triage.VerbQuit},
	}}

	s := triage.NewSession(context.Background(), batch, p, nil)
	require.ErrorIs(t, s.Run(), triage.ErrAbandoned)

	assert.Empty(t, s.Rulings(), "quit must not leave a partial ruling behind")
}
