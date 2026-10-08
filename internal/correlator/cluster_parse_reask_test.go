package correlator_test

// cluster_parse_reask_test.go pins the re-ask of a refused FIRST clustering response
// (finding F44): a response parseClusterResponse refuses is asked about again,
// quoting it and the parser's reason, up to llm.max_cluster_reasks follow-up calls,
// and a spent budget is a loud error naming every refusal.

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/correlator"
)

const (
	// placesAll is a complete, well-formed answer for coverageFixture: every
	// in-window event has a home.
	placesAll = `[{"kind":"extends","narrative_id":9,"summary":"cache so far","confidence":0.9,"event_indices":[0,1]},` +
		`{"kind":"new","title":"PR 7","summary":"s","confidence":0.9,"event_indices":[2,3]}]`
	// outOfRange names index 99 against a four-event prompt.
	outOfRange = `[{"kind":"new","title":"t","summary":"s","confidence":0.9,"event_indices":[0,1,2,3,99]}]`
	// unknownKind uses a kind the schema does not have.
	unknownKind = `[{"kind":"continues","summary":"s","confidence":0.9,"event_indices":[0,1,2,3]}]`
)

func TestCluster_ClusterBudgetZeroFailsOnTheFirstRefusalWithNoReask(t *testing.T) {
	f := newCoverageFixture(t)
	client := &fakeLLM{responses: []string{"Here are the clusters you asked for.", placesAll}}

	results, stats, err := f.clusterWith(t, client, correlator.WithClusterReasks(0))

	require.Error(t, err)
	assert.Nil(t, results)
	require.ErrorContains(t, err, "refused 1 time(s), exhausting the re-ask budget of 0 (llm.max_cluster_reasks)",
		"the error names the key that would allow a re-ask")
	require.ErrorContains(t, err, "Here are the clusters", "and quotes the refused response")
	assert.Len(t, client.prompts, 1, "a budget of 0 makes no follow-up call")
	assert.Zero(t, stats.ClusterReasks)
	assert.Equal(t, 1, stats.Calls)
}

// Budget 1 is the default, so no option is passed.
func TestCluster_RefusedClusterResponseIsReaskedOnceByDefault(t *testing.T) {
	f := newCoverageFixture(t)
	client := &fakeLLM{responses: []string{outOfRange, placesAll}}

	results, stats, err := f.clusterWith(t, client)

	require.NoError(t, err)
	require.Len(t, client.prompts, 2, "the original call and one re-ask")
	assert.Equal(t, []string{"claude_code/e0", "claude_code/e1", "github/pr-7-opened", "github/pr-7-merged"},
		eventIDs(results))
	assert.Equal(t, 1, stats.ClusterReasks)
	assert.Equal(t, 2, stats.Calls)

	reask := client.prompts[1]
	assert.True(t, strings.HasPrefix(reask, client.prompts[0]),
		"the re-ask repeats the original prompt verbatim, so every index means what it meant")
	assert.Contains(t, reask, outOfRange, "the refused response is quoted")
	assert.Contains(t, reask, "event_indices value 99 out of range [0,4)", "with the parser's reason")
	assert.Equal(t, client.systemPrompts[0], client.systemPrompts[1],
		"the same system prompt: the re-ask is the same question, answered again")
}

func TestCluster_ClusterBudgetTwoRecoversOnTheSecondReask(t *testing.T) {
	f := newCoverageFixture(t)
	client := &fakeLLM{responses: []string{outOfRange, unknownKind, placesAll}}

	results, stats, err := f.clusterWith(t, client, correlator.WithClusterReasks(2))

	require.NoError(t, err)
	require.Len(t, client.prompts, 3)
	assert.Len(t, eventIDs(results), 4)
	assert.Equal(t, 2, stats.ClusterReasks)
	assert.Equal(t, 3, stats.Calls)

	second := client.prompts[2]
	assert.True(t, strings.HasPrefix(second, client.prompts[0]))
	assert.Contains(t, second, unknownKind, "the latest refusal is quoted")
	assert.Contains(t, second, `unknown kind "continues"`)
	assert.NotContains(t, second, outOfRange, "an older refusal is not: its reason may no longer apply")
}

func TestCluster_ClusterBudgetExhaustedIsLoudWithEveryRefusal(t *testing.T) {
	f := newCoverageFixture(t)
	const missingConfidence = `[{"kind":"new","title":"t","summary":"s","event_indices":[0,1,2,3]}]`
	client := &fakeLLM{responses: []string{outOfRange, unknownKind, missingConfidence}}

	results, stats, err := f.clusterWith(t, client, correlator.WithClusterReasks(2))

	require.Error(t, err)
	assert.Nil(t, results)
	assert.Len(t, client.prompts, 3, "the original call and exactly two re-asks")
	require.ErrorContains(t, err, "refused 3 time(s), exhausting the re-ask budget of 2 (llm.max_cluster_reasks)")
	require.ErrorContains(t, err, "response 1: ")
	require.ErrorContains(t, err, "out of range")
	require.ErrorContains(t, err, `unknown kind "continues"`)
	require.ErrorContains(t, err, "has no confidence")
	require.ErrorContains(t, err, "clustering events in window", "the error says which window failed")
	assert.Equal(t, 2, stats.ClusterReasks)
	assert.Equal(t, 3, stats.Calls)
}

// A re-ask is larger than the prompt that fitted, by the refused response it quotes.
// Over the window it is not sent: there is nothing to bisect that would keep the
// refusal in view, and a rejected request part-way through the pass is the failure
// estimateTokens exists to prevent.
func TestCluster_ClusterReaskOverTheContextWindowIsNotSent(t *testing.T) {
	f := newCoverageFixture(t)
	budget := budgetFitting(t, f.evts, f.existing)
	huge := "I could not decide. " + strings.Repeat("x", 4*budget)
	client := &fakeLLM{responses: []string{huge, placesAll}}

	results, stats, err := correlator.Cluster(t.Context(), f.evts, f.existing, client, f.window, budget)

	require.Error(t, err)
	assert.Nil(t, results)
	assert.Len(t, client.prompts, 1, "the over-window re-ask was never sent")
	require.ErrorContains(t, err, "over the")
	require.ErrorContains(t, err, "context window")
	require.ErrorContains(t, err, "llm.max_cluster_reasks")
	require.ErrorContains(t, err, "I could not decide", "the refusal that prompted the re-ask is in the error")
	assert.Zero(t, stats.ClusterReasks)
	assert.Zero(t, stats.Splits, "the first prompt fitted, so nothing bisected")
}

// The accepted re-ask answer is checked for coverage like any first answer, and the
// omission round that follows repeats the ORIGINAL prompt: it is the index space,
// and the refusal it no longer needs is not carried into it.
func TestCluster_AnAcceptedClusterReaskIsThenCheckedForOmissions(t *testing.T) {
	f := newCoverageFixture(t)
	client := &fakeLLM{responses: []string{
		outOfRange,
		omitsTwoAndThree,
		`[{"kind":"new","title":"PR 7","summary":"s","confidence":0.9,"event_indices":[2,3]}]`,
	}}

	results, stats, err := f.clusterWith(t, client)

	require.NoError(t, err)
	require.Len(t, client.prompts, 3, "the original call, the parse re-ask, one omission round")
	assert.Len(t, eventIDs(results), 4)
	assert.Equal(t, 1, stats.ClusterReasks)
	assert.Equal(t, 1, stats.OmissionReasks)
	assert.Equal(t, 2, stats.RecoveredEvents)

	round := client.prompts[2]
	assert.True(t, strings.HasPrefix(round, client.prompts[0]))
	assert.NotContains(t, round, outOfRange, "the omission round does not quote the parse refusal")
}
