package correlator_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// TestMatch_ThePromptSetsEachCandidatesDatesAgainstTheNarrativesWindow is class 2 of a
// real 30-day store's sweep: long-closed tickets accepted as where new work is tracked.
// The model saw "status: Done" and nothing that said when. A ticket closed the day
// before a follow-up and one closed a year before new work on the same topic read
// identically. The facts are rendered, with the arithmetic done, and the judgment stays
// the model's: a closed ticket can legitimately be where a follow-up belongs.
//
// seedNarrative's window is 2026-08-24 09:00 to 10:00 UTC.
func TestMatch_ThePromptSetsEachCandidatesDatesAgainstTheNarrativesWindow(t *testing.T) {
	s := matchStore(t)
	seedNarrative(t, s, "ArgoCD performance investigation", "profiled the app controller",
		claudeEvent(t, "s1", "main", "PAAS-2710", "PAAS-4100"))

	tracker := &fakeTracker{issues: map[string]tasktracker.Issue{
		"PAAS-2710": {
			Key: "PAAS-2710", Summary: "CVEs: ArgoCD", StatusName: "Done",
			StatusCategory: tasktracker.StatusDone,
			Resolved:       time.Date(2025, 8, 24, 9, 0, 0, 0, time.UTC),
			Updated:        time.Date(2025, 8, 23, 12, 0, 0, 0, time.UTC),
		},
		"PAAS-4100": {
			Key: "PAAS-4100", Summary: "ArgoCD sharding", StatusName: "In Progress",
			StatusCategory: tasktracker.StatusInProgress,
			Updated:        time.Date(2026, 8, 24, 9, 30, 0, 0, time.UTC),
		},
	}}
	llmFake := &fakeLLM{responses: []string{
		`[{"issue_key":"PAAS-4100","role":"primary","confidence":0.9,"rationale":"the work"},` +
			`{"issue_key":"PAAS-2710","role":"mentioned","confidence":0.9,"rationale":"cited"}]`,
	}}

	_, _, err := correlator.Match(t.Context(), s, tracker, llmFake, matchCfg())
	require.NoError(t, err)
	require.Len(t, llmFake.prompts, 1)

	prompt := llmFake.prompts[0]
	assert.Contains(t, prompt, "window: 2026-08-24T09:00:00Z to 2026-08-24T10:00:00Z")
	assert.Contains(t, prompt, "status: Done (category: done)")
	assert.Contains(t, prompt, "resolved: 2025-08-24 (365 days before this narrative's window began)")
	assert.Contains(t, prompt, "last updated: 2025-08-23 (366 days before this narrative's window began)")
	assert.Contains(t, prompt, "status: In Progress (category: in_progress)")
	assert.Contains(t, prompt, "last updated: 2026-08-24 (during this narrative's window)")
	assert.NotContains(t, prompt, "resolved: 0001", "an unresolved issue's zero date is unknown, never rendered")

	assert.Contains(t, llmFake.systemPrompts[0], "resolved well before",
		"the system prompt says what the dates are for")
}

// TestMatch_ADateAfterTheWindowSaysSo: a ticket can be resolved or touched after the
// work it tracks, which is the ordinary case for a follow-up's own ticket.
func TestMatch_ADateAfterTheWindowSaysSo(t *testing.T) {
	s := matchStore(t)
	seedNarrative(t, s, "Fix", "fixed it", claudeEvent(t, "s1", "main", "PAAS-1", "PAAS-2"))

	tracker := &fakeTracker{issues: map[string]tasktracker.Issue{
		"PAAS-1": {Key: "PAAS-1", Summary: "one", Resolved: time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)},
		"PAAS-2": {Key: "PAAS-2", Summary: "two", Resolved: time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)},
	}}
	llmFake := &fakeLLM{responses: []string{`[]`}}

	_, _, err := correlator.Match(t.Context(), s, tracker, llmFake, matchCfg())
	require.NoError(t, err)
	require.Len(t, llmFake.prompts, 1)

	assert.Contains(t, llmFake.prompts[0], "resolved: 2026-08-25 (1 day after this narrative's window ended)")
	assert.Contains(t, llmFake.prompts[0], "resolved: 2026-08-31 (7 days after this narrative's window ended)")
}
