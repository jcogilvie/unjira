package reconciler

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// TestReworkOne_ProducesAnActionWhereBareRedraftCannot is the whole point of
// this function. Redraft is exported but uncallable from another package —
// `verifiedLink` is unexported, so the only compiling call shape passes nil for
// `verified`, and actionsFromVerdicts then drops every verdict as
// "unrecognized issue_key". The result is a silent no-op: no error, no actions,
// one LLM call spent.
//
// This test asserts the fixed path returns a real action. Its sibling below
// asserts the broken path still behaves as observed, so the two together
// document WHY this function exists rather than leaving it looking redundant.
func TestReworkOne_ProducesAnActionWhereBareRedraftCannot(t *testing.T) {
	s := reconcileStore(t)
	nid := seedLinkedNarrative(t, s, "DEVSBX-9", store.Role("primary"),
		codeEvent("rw:1", "shipped the retry logic"))

	tracker := &fakeTracker{issues: map[string]tasktracker.Issue{
		"DEVSBX-9": {Key: "DEVSBX-9", StatusName: "In Progress", Summary: "retry logic"},
	}}
	client := &fakeLLM{responses: []string{
		`[{"issue_key":"DEVSBX-9","type":"comment","body":"Shorter, per review.","confidence":0.9,"rationale":"reviewer asked"}]`,
	}}

	got, _, err := ReworkOne(context.Background(), s, tracker, client, nid, "make it shorter", nil)

	require.NoError(t, err)
	require.Len(t, got, 1, "the redraft must yield a replacement action")
	assert.Equal(t, "Shorter, per review.", got[0].Body)
	assert.Equal(t, "DEVSBX-9", got[0].IssueKey)
}

// TestReworkOne_VerifiesLiveStateRatherThanTrustingTheStoredLink: triage runs in
// a separate process from the watch pass that drafted these actions, possibly
// days later. Redraft's own doc comment justifies reusing already-verified links
// because live state "was confirmed earlier in this same pass" — which is simply
// not true here. So ReworkOne must call the tracker itself, or nothing enforces
// rules/intent-not-outcome.md on this path at all.
func TestReworkOne_VerifiesLiveStateRatherThanTrustingTheStoredLink(t *testing.T) {
	s := reconcileStore(t)
	nid := seedLinkedNarrative(t, s, "DEVSBX-9", store.Role("primary"),
		codeEvent("rw:1", "did work"))

	tracker := &fakeTracker{issues: map[string]tasktracker.Issue{
		"DEVSBX-9": {Key: "DEVSBX-9", StatusName: "In Progress"},
	}}
	client := &fakeLLM{responses: []string{
		`[{"issue_key":"DEVSBX-9","type":"comment","body":"b","confidence":0.9,"rationale":"r"}]`,
	}}

	_, _, err := ReworkOne(context.Background(), s, tracker, client, nid, "reword", nil)

	require.NoError(t, err)
	assert.Contains(t, tracker.getCalls, "DEVSBX-9",
		"a redraft in a separate process must confirm the issue still exists")
	assert.Empty(t, tracker.writeCalls,
		"redrafting proposes; it must never write to the tracker")
}

// TestReworkOne_PutsTheReviewersWordsInThePrompt — without this the feature is
// cosmetic: an LLM call that ignores the correction returns the same text the
// reviewer just objected to.
func TestReworkOne_PutsTheReviewersWordsInThePrompt(t *testing.T) {
	s := reconcileStore(t)
	nid := seedLinkedNarrative(t, s, "DEVSBX-9", store.Role("primary"),
		codeEvent("rw:1", "did work"))

	tracker := &fakeTracker{issues: map[string]tasktracker.Issue{
		"DEVSBX-9": {Key: "DEVSBX-9", StatusName: "In Progress"},
	}}
	client := &fakeLLM{responses: []string{
		`[{"issue_key":"DEVSBX-9","type":"comment","body":"b","confidence":0.9,"rationale":"r"}]`,
	}}

	_, _, err := ReworkOne(context.Background(), s, tracker, client, nid,
		"mention the rollback, not the deploy", nil)

	require.NoError(t, err)
	require.Len(t, client.prompts, 1)
	assert.Contains(t, client.prompts[0], "mention the rollback, not the deploy",
		"the reviewer's correction must reach the model")
}

// TestReworkOne_UsesTheCommitWatermarkNotTheActionDelta is the bug that would
// otherwise be invisible. The action being edited is itself what suppresses its
// own source events under DeltaEvents, so a redraft built on it would prompt the
// model with an empty delta and describe nothing.
//
// Asserted on prompt CONTENT rather than on the returned action: an empty delta
// still produces whatever the fake was told to return, so only the prompt can
// show whether the events actually made it in.
func TestReworkOne_UsesTheCommitWatermarkNotTheActionDelta(t *testing.T) {
	s := reconcileStore(t)
	nid := seedLinkedNarrative(t, s, "DEVSBX-9", store.Role("primary"),
		codeEvent("rw:1", "the load-bearing detail"))

	// The action a reviewer is about to edit. Its mere existence is what makes
	// DeltaEvents return nothing.
	_, err := s.InsertAction(store.ActionRow{
		NarrativeID: nid, Type: "comment", IssueKey: "DEVSBX-9",
		Payload: `{"body":"first draft"}`, Confidence: 0.9, Status: "proposed",
	})
	require.NoError(t, err)

	empty, err := s.DeltaEvents(nid)
	require.NoError(t, err)
	require.Empty(t, empty, "precondition: DeltaEvents is empty once the action exists")

	tracker := &fakeTracker{issues: map[string]tasktracker.Issue{
		"DEVSBX-9": {Key: "DEVSBX-9", StatusName: "In Progress"},
	}}
	client := &fakeLLM{responses: []string{
		`[{"issue_key":"DEVSBX-9","type":"comment","body":"b","confidence":0.9,"rationale":"r"}]`,
	}}

	_, _, err = ReworkOne(context.Background(), s, tracker, client, nid, "reword", nil)

	require.NoError(t, err)
	require.Len(t, client.prompts, 1)
	assert.Contains(t, client.prompts[0], "the load-bearing detail",
		"the redraft prompt must contain the work being described, not an empty delta")
}

// TestReworkOne_RefusesWhenEveryEventIsAlreadyCommitted: a narrative whose every
// event a posted comment already describes has nothing left to redraft. Failing
// loudly beats spending an LLM call to regenerate prose about nothing, and beats
// silently returning zero actions — which is exactly the failure mode the bare
// Redraft path had.
func TestReworkOne_RefusesWhenEveryEventIsAlreadyCommitted(t *testing.T) {
	s := reconcileStore(t)
	nid := seedLinkedNarrative(t, s, "DEVSBX-9", store.Role("primary"),
		codeEvent("rw:1", "already described"))

	id, err := s.InsertAction(store.ActionRow{
		NarrativeID: nid, Type: "comment", IssueKey: "DEVSBX-9",
		Payload: `{"body":"posted"}`, Confidence: 0.9, Status: "proposed",
	})
	require.NoError(t, err)
	require.NoError(t, s.UpdateActionStatus(id, "applied"))

	tracker := &fakeTracker{issues: map[string]tasktracker.Issue{
		"DEVSBX-9": {Key: "DEVSBX-9", StatusName: "Done"},
	}}
	client := &fakeLLM{}

	_, _, err = ReworkOne(context.Background(), s, tracker, client, nid, "reword", nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no uncommitted events")
	assert.Empty(t, client.prompts, "no LLM call should be spent on an empty delta")
}

// TestReworkOne_RefusesAnUnlinkedNarrative names the fix rather than just
// failing: a narrative with no actionable link cannot be redrafted, but it CAN
// be retargeted, so the error says so.
func TestReworkOne_RefusesAnUnlinkedNarrative(t *testing.T) {
	s := reconcileStore(t)
	nid := seedLinkedNarrative(t, s, "", store.Role("primary"), codeEvent("rw:1", "work"))

	_, _, err := ReworkOne(context.Background(), s, &fakeTracker{}, &fakeLLM{}, nid, "reword", nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no actionable link")
	assert.Contains(t, err.Error(), "retarget", "the error should name the remedy")
}

// TestReworkOne_RefusesWhenTheLinkNoLongerResolves: a deleted issue must not be
// redrafted against. verifyLinks records it as unverified and ReworkOne then has
// nothing to draft for, which must be an error rather than an empty success.
func TestReworkOne_RefusesWhenTheLinkNoLongerResolves(t *testing.T) {
	s := reconcileStore(t)
	nid := seedLinkedNarrative(t, s, "DEVSBX-404", store.Role("primary"),
		codeEvent("rw:1", "work"))

	// fakeTracker returns a 404 for any key not in issues.
	tracker := &fakeTracker{issues: map[string]tasktracker.Issue{}}
	client := &fakeLLM{}

	_, _, err := ReworkOne(context.Background(), s, tracker, client, nid, "reword", nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no linked issue verified")
	assert.Empty(t, client.prompts, "a redraft against a deleted issue must not reach the model")
}

// TestReworkOne_EmptyFeedbackIsRefusedBeforeSpendingACall: Redraft already
// rejects blank feedback, and this asserts ReworkOne does not paper over it. A
// blank correction means the reviewer's words were lost upstream, so
// regenerating the draft they just rejected is the wrong recovery.
func TestReworkOne_EmptyFeedbackIsRefusedBeforeSpendingACall(t *testing.T) {
	s := reconcileStore(t)
	nid := seedLinkedNarrative(t, s, "DEVSBX-9", store.Role("primary"),
		codeEvent("rw:1", "work"))

	tracker := &fakeTracker{issues: map[string]tasktracker.Issue{
		"DEVSBX-9": {Key: "DEVSBX-9", StatusName: "In Progress"},
	}}
	client := &fakeLLM{}

	_, _, err := ReworkOne(context.Background(), s, tracker, client, nid, "   ", nil)

	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "feedback is empty")
	assert.Empty(t, client.prompts)
}
