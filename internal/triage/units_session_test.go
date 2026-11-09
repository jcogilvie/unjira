package triage_test

// units_session_test.go covers the Session's approval units (D-18): a worklog
// and the comment or transition of the narrative it is joined to are presented
// once, and the reviewer's one decision is recorded on every member.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/triage"
)

// unitComment is the comment of narrative 7, the one worklog 3 is joined to.
func unitComment() store.ActionRow {
	return store.ActionRow{
		ID: 1, NarrativeID: 7, Type: "comment", IssueKey: "PROJ-1",
		Payload: `{"body":"b"}`, Status: store.StatusProposed,
	}
}

// unrelatedComment is narrative 8's comment, no unit's member.
func unrelatedComment() store.ActionRow {
	return store.ActionRow{
		ID: 2, NarrativeID: 8, Type: "comment", IssueKey: "PROJ-1",
		Payload: `{"body":"b"}`, Status: store.StatusProposed,
	}
}

func unitWorklog() store.ActionRow {
	return store.ActionRow{
		ID: 3, NarrativeID: 0, Type: store.ActionTypeWorklog, IssueKey: "PROJ-1",
		Payload: `{"op":"add","comment":"c"}`, Status: store.StatusProposed,
	}
}

// unitBatch is comment 1 (narrative 7), unrelated comment 2, worklog 3.
func unitBatch() []store.ActionRow {
	return []store.ActionRow{unitComment(), unrelatedComment(), unitWorklog()}
}

// unitJoins joins worklog 3 to narrative 7.
func unitJoins() map[int64]int64 { return map[int64]int64{3: 7} }

func memberIDs(item triage.Item) []int64 {
	out := make([]int64, 0, len(item.Members))
	for _, m := range item.Members {
		out = append(out, m.Action.ID)
	}

	return out
}

func memberPositions(item triage.Item) []int {
	out := make([]int, 0, len(item.Members))
	for _, m := range item.Members {
		out = append(out, m.Position)
	}

	return out
}

func rowIDs(rows []store.ActionRow) []int64 {
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}

	return out
}

func runUnitSession(
	t *testing.T, batch []store.ActionRow, answers ...triage.Decision,
) (*triage.Session, *scriptedPrompter) {
	t.Helper()

	p := &scriptedPrompter{answers: answers}
	s := triage.NewSession(t.Context(), batch, p, nil)
	s.SetUnits(unitJoins())
	require.NoError(t, s.Run())

	return s, p
}

func TestSession_PresentsAUnitOnce(t *testing.T) {
	_, p := runUnitSession(t, unitBatch())

	require.Len(t, p.shown, 2, "3 actions, one of them a 2-member unit: 2 asks")

	unit := p.shown[0]
	assert.Equal(t, []int64{1, 3}, memberIDs(unit), "members in batch order")
	assert.Equal(t, []int{1, 3}, memberPositions(unit), "each member keeps its flat position")
	assert.Equal(t, int64(1), unit.Action.ID, "the unit's Action is its comment")
	assert.Equal(t, 1, unit.Position)

	assert.Equal(t, int64(2), p.shown[1].Action.ID)
	assert.Empty(t, p.shown[1].Members, "an action outside every unit is asked alone")
}

func TestSession_UnitApproveApprovesEveryMember(t *testing.T) {
	s, _ := runUnitSession(t, unitBatch(),
		triage.Decision{Verb: triage.VerbApprove}, triage.Decision{Verb: triage.VerbSkip})

	assert.Equal(t, []int64{1, 3}, rowIDs(s.Approved()))
}

func TestSession_UnitRejectRulesOnEveryMember(t *testing.T) {
	s, _ := runUnitSession(t, unitBatch(),
		triage.Decision{Verb: triage.VerbReject, Text: "wrong work"}, triage.Decision{Verb: triage.VerbSkip})

	want := []triage.Ruling{
		{ActionID: 1, Status: store.StatusRejected, Feedback: "wrong work"},
		{ActionID: 3, Status: store.StatusRejected, Feedback: "wrong work"},
	}
	if diff := cmp.Diff(want, s.Rulings()); diff != "" {
		t.Errorf("Rulings() mismatch (-want +got):\n%s", diff)
	}
}

func TestSession_UnitSkipSkipsEveryMember(t *testing.T) {
	s, p := runUnitSession(t, unitBatch(),
		triage.Decision{Verb: triage.VerbSkip}, triage.Decision{Verb: triage.VerbApprove})

	assert.Equal(t, []int64{2}, rowIDs(s.Approved()))
	require.GreaterOrEqual(t, len(p.shown), 2)
	for _, item := range p.shown {
		assert.NotEqual(t, int64(3), item.Action.ID, "the skipped unit's worklog is never asked alone")
	}
}

func TestSession_UnitQuitDiscardsEverything(t *testing.T) {
	p := &scriptedPrompter{answers: []triage.Decision{{Verb: triage.VerbQuit}}}
	s := triage.NewSession(t.Context(), unitBatch(), p, nil)
	s.SetUnits(unitJoins())

	require.ErrorIs(t, s.Run(), triage.ErrAbandoned)
	assert.Empty(t, s.Approved())
	assert.Empty(t, s.Rulings())
}

func TestSession_WorklogFirstStillPresentsTheUnitAtItsFirstPosition(t *testing.T) {
	batch := []store.ActionRow{unitWorklog(), unrelatedComment(), unitComment()}

	_, p := runUnitSession(t, batch)

	require.NotEmpty(t, p.shown)
	unit := p.shown[0]
	assert.Equal(t, []int64{3, 1}, memberIDs(unit), "the unit is asked first, at the worklog's position")
	assert.Equal(t, 3, unit.Position, "the unit's Position is its comment's")
	assert.Equal(t, []int{1, 3}, memberPositions(unit))
}

func TestSession_NoJoinMeansNoUnit(t *testing.T) {
	p := &scriptedPrompter{}
	s := triage.NewSession(t.Context(), unitBatch(), p, nil)

	require.NoError(t, s.Run())

	require.Len(t, p.shown, 3, "without SetUnits every action is asked alone")
	for _, item := range p.shown {
		assert.Empty(t, item.Members)
	}
}

func TestSession_UnappliableMemberRefusesUnitApprove(t *testing.T) {
	p := &scriptedPrompter{answers: []triage.Decision{{Verb: triage.VerbApprove}, {Verb: triage.VerbSkip}}}
	s := triage.NewSession(t.Context(), unitBatch(), p, nil)
	s.SetUnits(unitJoins())
	s.SetWritability(config.Config{Trackers: []config.Tracker{{
		Name: "dev", Scopes: []string{"PROJ"}, WritableScopes: []string{"OTHER"},
	}}})

	require.NoError(t, s.Run())

	require.Len(t, p.notices, 1)
	assert.True(t, strings.HasPrefix(p.notices[0], "cannot apply: "), p.notices[0])
	assert.Contains(t, p.notices[0], "position 1")
	assert.Contains(t, p.notices[0], "d <n>")

	require.GreaterOrEqual(t, len(p.shown), 2)
	assert.Equal(t, []int64{1, 3}, memberIDs(p.shown[1]), "the unit is re-asked")
	assert.Empty(t, s.Approved())
}

// redraftRecorder records which action each Redraft was asked about.
type redraftRecorder struct {
	*stubHandler

	actions []int64
}

func (h *redraftRecorder) Redraft(
	ctx context.Context, action store.ActionRow, feedback string,
) (store.ActionRow, error) {
	h.actions = append(h.actions, action.ID)

	return h.stubHandler.Redraft(ctx, action, feedback)
}

func TestSession_WorkVerbOnAUnitActsOnItsCommentOrTransition(t *testing.T) {
	replacement := unitComment()
	replacement.ID = 99
	replacement.Payload = `{"body":"shorter"}`
	h := &redraftRecorder{stubHandler: &stubHandler{replacement: replacement}}
	p := &scriptedPrompter{answers: []triage.Decision{
		{Verb: triage.VerbEdit, Text: "shorter"},
		{Verb: triage.VerbSkip},
		{Verb: triage.VerbSkip},
	}}
	s := triage.NewSession(t.Context(), []store.ActionRow{unitWorklog(), unitComment(), unrelatedComment()}, p, h)
	s.SetUnits(unitJoins())

	require.NoError(t, s.Run())

	assert.Equal(t, []int64{1}, h.actions, "edit redrafts the comment at position 2, not the worklog")
	assert.Equal(t, []string{"shorter"}, h.redraftCalls)
	assert.NotContains(t, p.notices, triage.WorklogRefusal)

	require.Len(t, p.shown, 3)
	assert.Equal(t, int64(99), p.shown[1].Action.ID)
	assert.Equal(t, []int64{3, 99}, memberIDs(p.shown[1]), "the replacement is re-presented inside the unit")
}

func TestSession_DropThenApproveAppliesTheRest(t *testing.T) {
	s, p := runUnitSession(t, unitBatch(),
		triage.Decision{Verb: triage.VerbDrop, Positions: []int{1}, Text: "wrong ticket"},
		triage.Decision{Verb: triage.VerbSkip},
		triage.Decision{Verb: triage.VerbApprove})

	// Comment 2 sits between the unit's members, so it is asked before the worklog.
	require.Len(t, p.shown, 3)
	assert.Equal(t, int64(2), p.shown[1].Action.ID)
	assert.Equal(t, int64(3), p.shown[2].Action.ID, "the unit dissolves to its worklog alone")
	assert.Empty(t, p.shown[2].Members)
	assert.Equal(t, []int64{3}, rowIDs(s.Approved()))

	want := []triage.Ruling{{ActionID: 1, Status: store.StatusRejected, Feedback: "wrong ticket"}}
	if diff := cmp.Diff(want, s.Rulings()); diff != "" {
		t.Errorf("Rulings() mismatch (-want +got):\n%s", diff)
	}
}

func TestSession_DropInAThreeMemberUnitRepresentsTheRest(t *testing.T) {
	second := unitWorklog()
	second.ID = 4
	batch := []store.ActionRow{unitComment(), unitWorklog(), second}

	p := &scriptedPrompter{answers: []triage.Decision{
		{Verb: triage.VerbDrop, Positions: []int{3}, Text: "not this"},
		{Verb: triage.VerbSkip},
	}}
	s := triage.NewSession(t.Context(), batch, p, nil)
	s.SetUnits(map[int64]int64{3: 7, 4: 7})
	require.NoError(t, s.Run())

	require.GreaterOrEqual(t, len(p.shown), 2)
	assert.Equal(t, []int64{1, 3, 4}, memberIDs(p.shown[0]))
	assert.Equal(t, []int64{1, 3}, memberIDs(p.shown[1]), "the rest is re-presented as a unit")
}

func TestSession_DropOutsideAUnitIsRefused(t *testing.T) {
	p := &scriptedPrompter{answers: []triage.Decision{
		{Verb: triage.VerbDrop, Positions: []int{1}},
		{Verb: triage.VerbSkip},
	}}
	s := triage.NewSession(t.Context(), []store.ActionRow{unrelatedComment()}, p, nil)
	require.NoError(t, s.Run())

	assert.Equal(t, []string{triage.DropRefusal}, p.notices)
	require.Len(t, p.shown, 2, "the item is re-asked")
	assert.Equal(t, int64(2), p.shown[1].Action.ID)
	assert.Empty(t, s.Rulings())
}

func TestSession_DropNamesAMember(t *testing.T) {
	for name, positions := range map[string][]int{
		"non-member": {2},
		"none":       {},
		"two":        {1, 3},
	} {
		t.Run(name, func(t *testing.T) {
			s, p := runUnitSession(t, unitBatch(),
				triage.Decision{Verb: triage.VerbDrop, Positions: positions},
				triage.Decision{Verb: triage.VerbSkip},
				triage.Decision{Verb: triage.VerbSkip})

			assert.Equal(t, []string{"drop names one member of this unit: positions 1, 3"}, p.notices)
			require.GreaterOrEqual(t, len(p.shown), 2)
			assert.Equal(t, []int64{1, 3}, memberIDs(p.shown[1]), "the unit is re-asked")
			assert.Empty(t, s.Rulings())
		})
	}
}

func TestSession_DropThenQuitRecordsNothing(t *testing.T) {
	p := &scriptedPrompter{answers: []triage.Decision{
		{Verb: triage.VerbDrop, Positions: []int{3}, Text: "x"},
		{Verb: triage.VerbQuit},
	}}
	s := triage.NewSession(t.Context(), unitBatch(), p, nil)
	s.SetUnits(unitJoins())

	require.ErrorIs(t, s.Run(), triage.ErrAbandoned)
	assert.Empty(t, s.Rulings())
}

func TestSession_RestructureDissolvesTheUnit(t *testing.T) {
	tests := []struct {
		name string
		d    triage.Decision
	}{
		{"target", triage.Decision{Verb: triage.VerbTarget, Text: "PROJ-2"}},
		{"merge", triage.Decision{Verb: triage.VerbMerge, Positions: []int{1, 2}}},
		{"split", triage.Decision{Verb: triage.VerbSplit, Positions: []int{1}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			replacement := unitComment()
			replacement.ID = 99
			h := &stubHandler{replacement: replacement, restructured: []store.ActionRow{replacement}}
			p := &scriptedPrompter{answers: []triage.Decision{tt.d}}
			s := triage.NewSession(t.Context(), unitBatch(), p, h)
			s.SetUnits(unitJoins())

			require.NoError(t, s.Run())

			require.Equal(t, []int64{1, 3}, memberIDs(p.shown[0]), "the unit is presented before the verb")
			asked := map[int64][]int64{}
			for _, item := range p.shown[1:] {
				asked[item.Action.ID] = memberIDs(item)
			}
			assert.Contains(t, asked, int64(99))
			assert.Contains(t, asked, int64(3))
			assert.Empty(t, asked[99], "the replacement comment is asked alone")
			assert.Empty(t, asked[3], "the worklog is asked alone once its unit dissolved")
		})
	}
}

// TestSession_AFailedRestructureDissolvesNothing: a handler that refuses the
// verb leaves the batch as it was, so the unit is re-asked whole.
func TestSession_AFailedRestructureDissolvesNothing(t *testing.T) {
	h := &stubHandler{err: errors.New("refused")}
	p := &scriptedPrompter{answers: []triage.Decision{
		{Verb: triage.VerbTarget, Text: "PROJ-2"},
		{Verb: triage.VerbSkip},
	}}
	s := triage.NewSession(t.Context(), unitBatch(), p, h)
	s.SetUnits(unitJoins())

	require.NoError(t, s.Run())

	assert.Equal(t, []string{"refused"}, p.notices)
	require.GreaterOrEqual(t, len(p.shown), 2)
	assert.Equal(t, []int64{1, 3}, memberIDs(p.shown[1]), "the unit is re-asked whole")
}
