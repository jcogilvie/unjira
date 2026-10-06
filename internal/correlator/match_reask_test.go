package correlator_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// Both responses below are real shapes from a 30-day matching pass: prose where the
// array belonged, and a key the model duplicated into a malformed one
// ("confidence=0.2"). Each cost its narrative a pass before the re-ask existed.
const (
	proseMatchResponse = `Looking at this narrative, it describes recurring automated work.`
	validMatchResponse = `[{"issue_key":"PAAS-1","role":"primary","confidence":0.9,"rationale":"branch"},` +
		`{"issue_key":"SUMO-2","role":"mentioned","confidence":0.4,"rationale":"cited"}]`
)

func twoCandidateTracker() *fakeTracker {
	return &fakeTracker{issues: map[string]tasktracker.Issue{
		"PAAS-1": {Key: "PAAS-1", Summary: "Feature work", StatusName: "In Progress"},
		"SUMO-2": {Key: "SUMO-2", Summary: "Change task", StatusName: "To Do"},
	}}
}

func TestMatch_UnparseableResponseIsReaskedOnceWithTheReason(t *testing.T) {
	s := matchStore(t)
	id := seedNarrative(t, s, "Feature", "summary", claudeEvent(t, "s1", "feature/PAAS-1", "SUMO-2"))
	llmFake := &fakeLLM{responses: []string{proseMatchResponse, validMatchResponse}}

	results, stats, err := correlator.Match(t.Context(), s, twoCandidateTracker(), llmFake, matchCfg())
	require.NoError(t, err)
	require.Len(t, results, 1)

	assert.Equal(t, 2, stats.Calls, "the re-ask is a second call and must be counted")
	assert.Equal(t, 1, stats.MatchReasks)
	assert.Equal(t, "PAAS-1", primaryOf(t, s, id))

	require.Len(t, llmFake.prompts, 2)
	reask := llmFake.prompts[1]
	assert.Contains(t, reask, llmFake.prompts[0], "the re-ask must repeat the original prompt verbatim")
	assert.Contains(t, reask, proseMatchResponse, "the re-ask must quote what could not be used")
	assert.Contains(t, reask, "invalid character", "the re-ask must say why the response was refused")
	assert.Equal(t, llmFake.systemPrompts[0], llmFake.systemPrompts[1])
}

func TestMatch_ResponseUnparseableTwiceFailsLoudlyNamingBoth(t *testing.T) {
	s := matchStore(t)
	id := seedNarrative(t, s, "Feature", "summary", claudeEvent(t, "s1", "feature/PAAS-1", "SUMO-2"))
	second := `[{"issue_key":"PAAS-1","role":"owner","confidence":0.9}]`
	llmFake := &fakeLLM{responses: []string{proseMatchResponse, second}}

	_, stats, err := correlator.Match(t.Context(), s, twoCandidateTracker(), llmFake, matchCfg())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid character", "the first refusal must survive into the error")
	assert.Contains(t, err.Error(), `"owner"`, "the second refusal must be in the error")
	assert.Equal(t, 2, stats.Calls, "one re-ask is the budget, never more")
	assert.Len(t, llmFake.prompts, 2)
	assert.Empty(t, primaryOf(t, s, id), "a narrative that failed twice stays unmatched for a later pass")
}

// Distinct refusals, so a test can tell which one a prompt or an error quotes.
const (
	badRoleMatchResponse  = `[{"issue_key":"PAAS-1","role":"owner","confidence":0.9}]`
	emptyKeyMatchResponse = `[{"issue_key":"","role":"primary","confidence":0.9}]`
)

func TestMatch_ReaskBudgetZeroFailsOnTheFirstRefusalWithNoFollowUp(t *testing.T) {
	s := matchStore(t)
	id := seedNarrative(t, s, "Feature", "summary", claudeEvent(t, "s1", "feature/PAAS-1", "SUMO-2"))
	llmFake := &fakeLLM{responses: []string{proseMatchResponse, validMatchResponse}}

	_, stats, err := correlator.Match(t.Context(), s, twoCandidateTracker(), llmFake, matchCfg(),
		correlator.WithMatchReasks(0))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid character", "the refusal's reason is in the error")
	assert.Contains(t, err.Error(), "llm.max_match_reasks", "the error names the key that would allow a re-ask")
	assert.Len(t, llmFake.prompts, 1, "a budget of 0 makes no follow-up call")
	assert.Zero(t, stats.MatchReasks)
	assert.Empty(t, primaryOf(t, s, id))
}

// Each follow-up repeats the original prompt and quotes the LATEST refusal: the model
// is answering what it just said, and an older refusal's reason may no longer apply.
func TestMatch_ReaskBudgetTwoRecoversOnTheSecondFollowUp(t *testing.T) {
	s := matchStore(t)
	id := seedNarrative(t, s, "Feature", "summary", claudeEvent(t, "s1", "feature/PAAS-1", "SUMO-2"))
	llmFake := &fakeLLM{responses: []string{proseMatchResponse, badRoleMatchResponse, validMatchResponse}}

	_, stats, err := correlator.Match(t.Context(), s, twoCandidateTracker(), llmFake, matchCfg(),
		correlator.WithMatchReasks(2))

	require.NoError(t, err)
	assert.Equal(t, 3, stats.Calls)
	assert.Equal(t, 2, stats.MatchReasks)
	assert.Equal(t, "PAAS-1", primaryOf(t, s, id))

	require.Len(t, llmFake.prompts, 3)
	second := llmFake.prompts[2]
	assert.Contains(t, second, llmFake.prompts[0], "every follow-up repeats the original prompt verbatim")
	assert.Contains(t, second, badRoleMatchResponse, "the latest refused response is quoted")
	assert.Contains(t, second, `"owner"`, "with the parser's reason for refusing it")
	assert.NotContains(t, second, proseMatchResponse, "an earlier refusal is not carried forward")
}

func TestMatch_ReaskBudgetTwoExhaustedFailsNamingEveryRefusal(t *testing.T) {
	s := matchStore(t)
	id := seedNarrative(t, s, "Feature", "summary", claudeEvent(t, "s1", "feature/PAAS-1", "SUMO-2"))
	llmFake := &fakeLLM{responses: []string{
		proseMatchResponse, badRoleMatchResponse, emptyKeyMatchResponse, validMatchResponse,
	}}

	_, stats, err := correlator.Match(t.Context(), s, twoCandidateTracker(), llmFake, matchCfg(),
		correlator.WithMatchReasks(2))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid character")
	assert.Contains(t, err.Error(), `"owner"`)
	assert.Contains(t, err.Error(), "empty issue_key")
	assert.Len(t, llmFake.prompts, 3, "one original call and two re-asks, never more")
	assert.Equal(t, 3, stats.Calls)
	assert.Equal(t, 2, stats.MatchReasks)
	assert.Empty(t, primaryOf(t, s, id))
}

func TestMatch_ParseableResponseIsNeverReasked(t *testing.T) {
	s := matchStore(t)
	seedNarrative(t, s, "Feature", "summary", claudeEvent(t, "s1", "feature/PAAS-1", "SUMO-2"))
	llmFake := &fakeLLM{responses: []string{validMatchResponse}}

	_, stats, err := correlator.Match(t.Context(), s, twoCandidateTracker(), llmFake, matchCfg())
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Calls)
	assert.Zero(t, stats.MatchReasks)
}
