package triage_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/triage"
)

// scriptedPrompter answers with a fixed sequence, recording what it was shown.
type scriptedPrompter struct {
	answers []triage.Decision
	shown   []triage.Item
	confirm bool
	notices []string
}

func (p *scriptedPrompter) Ask(item triage.Item) (triage.Decision, error) {
	p.shown = append(p.shown, item)
	if len(p.answers) == 0 {
		return triage.Decision{Verb: triage.VerbSkip}, nil
	}
	d := p.answers[0]
	p.answers = p.answers[1:]

	return d, nil
}

func (p *scriptedPrompter) Confirm(string) (bool, error) { return p.confirm, nil }

func (p *scriptedPrompter) Notify(message string) error {
	p.notices = append(p.notices, message)

	return nil
}

func batchOf(ids ...int64) []store.ActionRow {
	out := make([]store.ActionRow, 0, len(ids))
	for _, id := range ids {
		out = append(out, store.ActionRow{
			ID: id, NarrativeID: id, Type: "comment",
			IssueKey: "PROJ-1", Payload: `{"body":"b"}`, Status: "proposed",
		})
	}

	return out
}

func TestSession_CollectsOneDecisionPerActionAndAppliesNothing(t *testing.T) {
	p := &scriptedPrompter{answers: []triage.Decision{
		{Verb: triage.VerbApprove},
		{Verb: triage.VerbReject, Text: "not worth posting"},
		{Verb: triage.VerbApprove},
	}}
	s := triage.NewSession(batchOf(1, 2, 3), p, nil)

	require.NoError(t, s.Run())

	require.Len(t, p.shown, 3)
	assert.Equal(t, 1, p.shown[0].Position)
	assert.Equal(t, 3, p.shown[0].Total)

	approved := s.Approved()
	require.Len(t, approved, 2, "only the two approvals")
	assert.Equal(t, int64(1), approved[0].ID)
	assert.Equal(t, int64(3), approved[1].ID)
}

func TestSession_QuitAbandonsWithoutApplying(t *testing.T) {
	p := &scriptedPrompter{answers: []triage.Decision{
		{Verb: triage.VerbApprove},
		{Verb: triage.VerbQuit},
	}}
	s := triage.NewSession(batchOf(1, 2, 3), p, nil)

	err := s.Run()

	require.ErrorIs(t, err, triage.ErrAbandoned)
	assert.Empty(t, s.Approved(),
		"quit must discard even decisions already recorded: nothing was applied, so nothing is owed")
}

// stubHandler records what it was asked and returns canned replacements.
type stubHandler struct {
	redraftCalls     []string
	restructureCalls []triage.Decision
	replacement      store.ActionRow
	restructured     []store.ActionRow
	err              error
}

func (h *stubHandler) Redraft(_ store.ActionRow, feedback string) (store.ActionRow, error) {
	h.redraftCalls = append(h.redraftCalls, feedback)

	return h.replacement, h.err
}

func (h *stubHandler) Restructure(d triage.Decision, _ []store.ActionRow) ([]store.ActionRow, error) {
	h.restructureCalls = append(h.restructureCalls, d)

	return h.restructured, h.err
}

// TestSession_EditRepresentsTheReplacement: an edit must not silently accept
// the redraft. The reviewer has not read the new text, so it goes back into the
// batch for disposition — approving unread text is the mistake this surface
// exists to prevent.
func TestSession_EditRepresentsTheReplacement(t *testing.T) {
	replacement := store.ActionRow{
		ID: 99, NarrativeID: 1, Type: "comment", IssueKey: "PROJ-1",
		Payload: `{"body":"revised"}`, Status: "proposed",
	}
	h := &stubHandler{replacement: replacement}
	p := &scriptedPrompter{answers: []triage.Decision{
		{Verb: triage.VerbEdit, Text: "too vague"},
		{Verb: triage.VerbApprove}, // disposition for the REPLACEMENT
	}}

	s := triage.NewSession(batchOf(1), p, h)
	require.NoError(t, s.Run())

	assert.Equal(t, []string{"too vague"}, h.redraftCalls)
	require.Len(t, p.shown, 2, "the replacement must be presented, not silently accepted")
	assert.Equal(t, int64(99), p.shown[1].Action.ID)

	approved := s.Approved()
	require.Len(t, approved, 1)
	assert.Equal(t, int64(99), approved[0].ID, "the approved action is the REPLACEMENT, not the original")
}

// TestSession_NilHandlerNotifiesAndReprompts: --dry-run runs with no handler,
// and asking for a restructure there must cost one keystroke rather than
// ending the session.
func TestSession_NilHandlerNotifiesAndReprompts(t *testing.T) {
	p := &scriptedPrompter{answers: []triage.Decision{
		{Verb: triage.VerbMerge, Positions: []int{1, 2}},
		{Verb: triage.VerbApprove},
	}}

	s := triage.NewSession(batchOf(1), p, nil)
	require.NoError(t, s.Run())

	require.Len(t, p.notices, 1)
	assert.Contains(t, p.notices[0], "not available")
	assert.Len(t, s.Approved(), 1, "the reviewer's follow-up approve still lands")
}

// TestSession_RestructureKeepsUnaffectedDispositions is what batch apply buys:
// a restructure late in the batch must not discard earlier judgments, because
// none of them have been applied.
func TestSession_RestructureKeepsUnaffectedDispositions(t *testing.T) {
	merged := store.ActionRow{
		ID: 77, NarrativeID: 2, Type: "comment", IssueKey: "PROJ-2",
		Payload: `{"body":"merged"}`, Status: "proposed",
	}
	h := &stubHandler{restructured: []store.ActionRow{merged}}
	p := &scriptedPrompter{answers: []triage.Decision{
		{Verb: triage.VerbApprove},                       // action 1
		{Verb: triage.VerbMerge, Positions: []int{1, 2}}, // action 2 -> merged
		{Verb: triage.VerbApprove},                       // the merged replacement
	}}

	s := triage.NewSession(batchOf(1, 2), p, h)
	require.NoError(t, s.Run())

	approved := s.Approved()
	require.Len(t, approved, 2)
	assert.Equal(t, int64(1), approved[0].ID, "action 1's earlier approval survived the restructure")
	assert.Equal(t, int64(77), approved[1].ID)
}
