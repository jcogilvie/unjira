package correlator_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

const loneMentionedResponse = `[{"issue_key":"PAAS-2710","role":"mentioned","confidence":0.9,` +
	`"rationale":"a closed CVE ticket the investigation cites, not where it is tracked"}]`

// TestMatch_ALoneMentionedCandidateIsJudgedNotAssumed is class 2 of a real 30-day
// store's sweep. A new ArgoCD performance investigation mentioned PAAS-2710, a CVE
// ticket closed the year before, in passing. It was the only candidate, so it became
// the primary at confidence 1.0 with no model call, and a comment was proposed on it.
// Being the only key a session happened to mention is not a fact about where the work
// is tracked. The model judges it, and may say none.
func TestMatch_ALoneMentionedCandidateIsJudgedNotAssumed(t *testing.T) {
	cases := []struct {
		name       string
		provenance correlator.Provenance
		seed       func(t *testing.T, s *store.Store)
	}{
		{name: "first mentioned in prose", provenance: correlator.ProvenanceProseFirst},
		{
			name:       "corroborated by collected Jira activity",
			provenance: correlator.ProvenanceCorroborated,
			seed: func(t *testing.T, s *store.Store) {
				t.Helper()

				// Collected, never linked to the narrative: activity is what
				// corroborates a prose key, membership is not needed.
				e := jiraEvent(t, "PAAS-2710")
				_, err := s.InsertEvent(e)
				require.NoError(t, err)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := matchStore(t)
			if tc.seed != nil {
				tc.seed(t, s)
			}

			id := seedNarrative(t, s, "ArgoCD performance investigation", "profiled the app controller",
				claudeEvent(t, "s1", "main", "PAAS-2710"))
			tracker := &fakeTracker{issues: map[string]tasktracker.Issue{
				"PAAS-2710": {
					Key: "PAAS-2710", Summary: "CVEs: ArgoCD", StatusName: "Done",
					StatusCategory: tasktracker.StatusDone,
				},
			}}
			llmFake := &fakeLLM{responses: []string{loneMentionedResponse}}

			results, stats, err := correlator.Match(t.Context(), s, tracker, llmFake, matchCfg())
			require.NoError(t, err)
			require.Len(t, results, 1)

			assert.Equal(t, 1, stats.Calls, "a lone %s candidate is a judgment, so the model is asked", tc.provenance)
			assert.Empty(t, results[0].Primary)
			assert.Empty(t, primaryOf(t, s, id))

			links, err := s.NarrativeIssues(id)
			require.NoError(t, err)
			require.Len(t, links, 1)
			assert.Equal(t, store.Role("mentioned"), links[0].Role)
			assert.Equal(t, string(tc.provenance), links[0].Provenance)

			backlog, err := s.NarrativesWithoutPrimaryLink(10)
			require.NoError(t, err)
			assert.Empty(t, backlog, "judged with no primary is examined, not re-asked every pass")
		})
	}
}

// TestMatch_ALoneCandidateTheWorkNamedIsStillDeterministic: a branch name, a commit
// or PR title, or a Jira event about the issue is someone naming the ticket for this
// work, a recorded fact rather than an inference. One such survivor stays primary at
// confidence 1.0 with no model spend.
func TestMatch_ALoneCandidateTheWorkNamedIsStillDeterministic(t *testing.T) {
	scm := events.NewEvent("claude_code", "s1", time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC), "committed")
	events.SetSCMKeys(&scm, []string{"PROJ-42"})

	cases := []struct {
		name       string
		event      events.Event
		provenance correlator.Provenance
	}{
		{name: "branch", event: claudeEvent(t, "s1", "feature/PROJ-42"), provenance: correlator.ProvenanceBranch},
		{name: "scm authoring command", event: scm, provenance: correlator.ProvenanceSCMCommand},
		{name: "jira event", event: jiraEvent(t, "PROJ-42"), provenance: correlator.ProvenanceJiraEvent},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := matchStore(t)
			id := seedNarrative(t, s, "Implement feature", "did the feature work", tc.event)
			tracker := &fakeTracker{issues: map[string]tasktracker.Issue{
				"PROJ-42": {Key: "PROJ-42", Summary: "Feature work", StatusName: "In Progress"},
			}}
			llmFake := &fakeLLM{}

			results, stats, err := correlator.Match(t.Context(), s, tracker, llmFake, matchCfg())
			require.NoError(t, err)
			require.Len(t, results, 1)

			assert.Zero(t, stats.Calls, "a lone %s candidate needs no model judgment", tc.provenance)
			assert.Equal(t, "PROJ-42", results[0].Primary)
			assert.Equal(t, "PROJ-42", primaryOf(t, s, id))
		})
	}
}

// TestMatch_ClassifierMayGiveNoCandidatePrimary pins the system prompt's permission to
// assign no primary. It used to require exactly one, which a model asked about a lone
// cited ticket can satisfy only by calling the citation the work, while the parser
// already accepted none (F58).
func TestMatch_ClassifierMayGiveNoCandidatePrimary(t *testing.T) {
	s := matchStore(t)
	seedNarrative(t, s, "ArgoCD performance investigation", "profiled the app controller",
		claudeEvent(t, "s1", "main", "PAAS-2710"))
	tracker := &fakeTracker{issues: map[string]tasktracker.Issue{
		"PAAS-2710": {Key: "PAAS-2710", Summary: "CVEs: ArgoCD", StatusName: "Done"},
	}}
	llmFake := &fakeLLM{responses: []string{loneMentionedResponse}}

	_, _, err := correlator.Match(t.Context(), s, tracker, llmFake, matchCfg())
	require.NoError(t, err)
	require.Len(t, llmFake.systemPrompts, 1)

	system := llmFake.systemPrompts[0]
	assert.NotContains(t, system, "Exactly one candidate must receive this role")
	assert.Contains(t, system, "At most one candidate may receive this role")
}
