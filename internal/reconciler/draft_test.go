package reconciler

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/llm"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

func TestDraftProducesOneActionPerActionableLinkWithDistinctBodies(t *testing.T) {
	// The motivating workflow: an engineering ticket plus a paired
	// change-management ticket for the same deploy. Two audiences, so two
	// separately-drafted bodies.
	client := &fakeLLM{responses: []string{`[
		{"issue_key":"PAAS-1","type":"comment","body":"Shipped the retry logic; see the PR.","confidence":0.9,"rationale":"delta is code work"},
		{"issue_key":"SUMO-9","type":"comment","body":"Change deployed to production, no customer impact.","confidence":0.8,"rationale":"change-management audience"}
	]`}}

	narrative := store.NarrativeRow{ID: 1, Title: "retry logic", Summary: "added retries"}
	verified := []verifiedLink{
		{
			Link:  store.NarrativeIssue{IssueKey: "PAAS-1", Role: store.Role("primary")},
			Issue: tasktracker.Issue{Key: "PAAS-1", Summary: "add retries"},
		},
		{
			Link:  store.NarrativeIssue{IssueKey: "SUMO-9", Role: store.Role("same_work")},
			Issue: tasktracker.Issue{Key: "SUMO-9", Summary: "prod change"},
		},
	}

	got, stats, err := draft(t.Context(), client, narrative,
		[]events.Event{codeEvent("e1", "added retry logic")}, verified)
	require.NoError(t, err)

	require.Len(t, got, 2)
	assert.Equal(t, 1, stats.Calls, "one LLM call per narrative, not per link")
	assert.NotEqual(t, got[0].Body, got[1].Body,
		"identical text on both tickets would defeat the point of the audiences differing")
}

func TestDraftFloorsConfidenceForAnIllegalTransition(t *testing.T) {
	// The model claims high confidence in a Done transition. The live issue
	// offers only In Progress.
	client := &fakeLLM{responses: []string{
		`[{"issue_key":"PROJ-1","type":"transition","target_status":"done","confidence":0.95,"rationale":"work is finished"}]`,
	}}

	narrative := store.NarrativeRow{ID: 1, Title: "t", Summary: "s"}
	verified := []verifiedLink{{
		Link:            store.NarrativeIssue{IssueKey: "PROJ-1", Role: store.Role("primary")},
		Issue:           tasktracker.Issue{Key: "PROJ-1", StatusCategory: tasktracker.StatusTodo},
		AvailableStatus: []tasktracker.StatusCategory{tasktracker.StatusInProgress},
	}}

	got, _, err := draft(t.Context(), client, narrative,
		[]events.Event{codeEvent("e1", "finished it")}, verified)
	require.NoError(t, err)

	require.Len(t, got, 1)
	assert.Zero(t, got[0].Confidence,
		"a transition the live issue does not offer is floored to zero regardless of the "+
			"model's self-report — rules/verify-correlations.md: a hallucinated match can be "+
			"high-confidence")
}

func TestDraftIgnoresAnUnrecognizedIssueKey(t *testing.T) {
	// The model names a key that was never presented as a verified link.
	client := &fakeLLM{responses: []string{
		`[{"issue_key":"GHOST-1","type":"comment","body":"x","confidence":0.9,"rationale":"invented"}]`,
	}}

	narrative := store.NarrativeRow{ID: 1, Title: "t", Summary: "s"}
	verified := []verifiedLink{{
		Link:  store.NarrativeIssue{IssueKey: "PROJ-1", Role: store.Role("primary")},
		Issue: tasktracker.Issue{Key: "PROJ-1"},
	}}

	got, _, err := draft(t.Context(), client, narrative,
		[]events.Event{codeEvent("e1", "did work")}, verified)
	require.NoError(t, err)

	assert.Empty(t, got,
		"a key this pass never verified must not receive an action, whatever confidence "+
			"the model stated")
}

func TestDraftPromptCarriesTheDeltaAndEachLinksLiveState(t *testing.T) {
	client := &fakeLLM{responses: []string{
		`[{"issue_key":"PROJ-1","type":"comment","body":"ok","confidence":0.7,"rationale":"r"}]`,
	}}

	narrative := store.NarrativeRow{ID: 1, Title: "the title", Summary: "the summary"}
	verified := []verifiedLink{{
		Link: store.NarrativeIssue{IssueKey: "PROJ-1", Role: store.Role("primary")},
		Issue: tasktracker.Issue{
			Key: "PROJ-1", Summary: "live ticket summary", StatusName: "In Progress",
		},
		AvailableStatus: []tasktracker.StatusCategory{tasktracker.StatusDone},
	}}

	_, _, err := draft(t.Context(), client, narrative,
		[]events.Event{codeEvent("e1", "the delta event summary")}, verified)
	require.NoError(t, err)

	require.Len(t, client.prompts, 1)
	prompt := client.prompts[0]

	assert.Contains(t, prompt, "the delta event summary", "the delta is what gets proposed on")
	assert.Contains(t, prompt, "PROJ-1")
	assert.Contains(t, prompt, "primary", "the role determines the framing")
	assert.Contains(t, prompt, "In Progress", "live status, not inferred status")
	assert.Contains(t, prompt, "done", "the legal transitions bound what may be proposed")
}

func TestDraftRejectsAnUnknownActionType(t *testing.T) {
	client := &fakeLLM{responses: []string{
		`[{"issue_key":"PROJ-1","type":"estimate","body":"x","confidence":0.5,"rationale":"r"}]`,
	}}

	narrative := store.NarrativeRow{ID: 1, Title: "t", Summary: "s"}
	verified := []verifiedLink{{
		Link:  store.NarrativeIssue{IssueKey: "PROJ-1", Role: store.Role("primary")},
		Issue: tasktracker.Issue{Key: "PROJ-1"},
	}}

	_, _, err := draft(t.Context(), client, narrative,
		[]events.Event{codeEvent("e1", "did work")}, verified)
	require.Error(t, err, "an action type outside the closed set must be a loud error, not a silent drop")
}

func TestDraftToleratesAMarkdownFenceDespiteTheSystemPromptForbiddingIt(t *testing.T) {
	client := &fakeLLM{responses: []string{
		"```json\n" +
			`[{"issue_key":"PROJ-1","type":"comment","body":"ok","confidence":0.7,"rationale":"r"}]` +
			"\n```",
	}}

	narrative := store.NarrativeRow{ID: 1, Title: "t", Summary: "s"}
	verified := []verifiedLink{{
		Link:  store.NarrativeIssue{IssueKey: "PROJ-1", Role: store.Role("primary")},
		Issue: tasktracker.Issue{Key: "PROJ-1"},
	}}

	got, _, err := draft(t.Context(), client, narrative,
		[]events.Event{codeEvent("e1", "did work")}, verified)
	require.NoError(t, err)
	require.Len(t, got, 1)
}

func TestDraftClampsAnOutOfRangeConfidence(t *testing.T) {
	client := &fakeLLM{responses: []string{
		`[{"issue_key":"PROJ-1","type":"comment","body":"ok","confidence":1.4,"rationale":"r"}]`,
	}}

	narrative := store.NarrativeRow{ID: 1, Title: "t", Summary: "s"}
	verified := []verifiedLink{{
		Link:  store.NarrativeIssue{IssueKey: "PROJ-1", Role: store.Role("primary")},
		Issue: tasktracker.Issue{Key: "PROJ-1"},
	}}

	got, _, err := draft(t.Context(), client, narrative,
		[]events.Event{codeEvent("e1", "did work")}, verified)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.LessOrEqual(t, got[0].Confidence, 1.0, "a model confidence above 1 must be clamped, never trusted verbatim")
}

func TestDraftRecordsUsageInStats(t *testing.T) {
	client := &fakeLLM{
		responses:    []string{`[{"issue_key":"PROJ-1","type":"comment","body":"ok","confidence":0.7,"rationale":"r"}]`},
		usagePerCall: llm.Usage{PromptTokens: 11, CompletionTokens: 22},
	}

	narrative := store.NarrativeRow{ID: 1, Title: "t", Summary: "s"}
	verified := []verifiedLink{{
		Link:  store.NarrativeIssue{IssueKey: "PROJ-1", Role: store.Role("primary")},
		Issue: tasktracker.Issue{Key: "PROJ-1"},
	}}

	_, stats, err := draft(t.Context(), client, narrative,
		[]events.Event{codeEvent("e1", "did work")}, verified)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Calls, "addUsage must not be double-counted alongside an explicit stats.Calls++")
	assert.EqualValues(t, 11, stats.PromptTokens)
	assert.EqualValues(t, 22, stats.CompletionTokens)
}
