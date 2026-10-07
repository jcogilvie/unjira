package reconciler

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// The drafter may answer "none" for an issue: nothing in the delta is that issue's
// news. Before it could, the prompt demanded one action per issue, and on a real store
// the model complied with comments whose whole body said there was nothing to say
// ("No work was performed on this ticket", "No action proposed"). A "none" is not an
// action: it is recorded as a suppression with the model's reason, which is also the
// watermark that stops the next pass paying to draft the same delta again.
func TestReconcile_ANoneVerdictProposesNothingAndIsRecorded(t *testing.T) {
	s := reconcileStore(t)
	tracker := &fakeTracker{issues: map[string]tasktracker.Issue{
		"PROJ-1": {Key: "PROJ-1", Summary: "the ticket"},
	}}
	seedLinkedNarrative(t, s, "PROJ-1", store.Role("primary"), codeEvent("e1", "quoted PROJ-1 as test data"))

	llmClient := &fakeLLM{responses: []string{
		`[{"issue_key":"PROJ-1","type":"none","confidence":0.9,"rationale":"the delta only quotes this key as fixture data"}]`,
	}}
	cfg := config.ReconcilerConfig{MaxNarrativesPerPass: 10, MinConfidenceToPropose: 0.5}

	results, _, err := Reconcile(t.Context(), s, tracker, llmClient, cfg)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Empty(t, results[0].Proposed)
	require.Len(t, results[0].Suppressed, 1)
	assert.Contains(t, results[0].Suppressed[0], "PROJ-1")
	assert.Contains(t, results[0].Suppressed[0], "fixture data", "the model's reason is the record")

	_, err = Persist(s, results)
	require.NoError(t, err)

	again, _, err := Reconcile(t.Context(), s, tracker, llmClient, cfg)
	require.NoError(t, err)
	assert.Empty(t, again, "nothing new since the recorded decision: not drafted again")
	assert.Len(t, llmClient.prompts, 1)
}

func TestParseDraftResponse_NoneNeedsAnIssueKey(t *testing.T) {
	_, err := parseDraftResponse(`[{"type":"none","rationale":"r"}]`)
	require.Error(t, err)

	got, err := parseDraftResponse(`[{"issue_key":"PROJ-1","type":"none","rationale":"r"}]`)
	require.NoError(t, err)
	require.Len(t, got, 1)
}

// A "none" never becomes an action, on either path that maps verdicts, so nothing
// downstream (persist, the applier) can ever be handed one.
func TestActionsFromVerdicts_DropsNone(t *testing.T) {
	verified := []verifiedLink{{
		Link:  store.NarrativeIssue{IssueKey: "PROJ-1", Role: store.RolePrimary},
		Issue: tasktracker.Issue{Key: "PROJ-1"},
	}}

	got := actionsFromVerdicts(1, []draftVerdict{{IssueKey: "PROJ-1", Type: verdictNone, Rationale: "r"}},
		verified, "draft", nil, nil)

	assert.Empty(t, got)
}

// A reviewer asking for a redraft is never answered with silence (see ReworkOne): a
// "none" that leaves nothing to propose is an error carrying the model's reason, so the
// reviewer can reject the action instead.
func TestRedraft_ANoneThatLeavesNothingIsAnError(t *testing.T) {
	verified := []verifiedLink{{
		Link:  store.NarrativeIssue{IssueKey: "PROJ-1", Role: store.RolePrimary},
		Issue: tasktracker.Issue{Key: "PROJ-1"},
	}}
	client := &fakeLLM{responses: []string{
		`[{"issue_key":"PROJ-1","type":"none","rationale":"the correction says this is not the ticket"}]`,
	}}

	_, _, err := Redraft(t.Context(), client, store.NarrativeRow{ID: 7}, nil, verified, nil, "wrong ticket")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not the ticket")
	assert.Contains(t, err.Error(), "reject")
}
