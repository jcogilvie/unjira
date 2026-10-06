package correlator_test

// cluster_reask_budget_test.go pins the configurable re-ask budgets of the two
// clustering follow-ups: the omission re-ask (cluster_reask.go) and the dispute
// re-ask (cluster_dispute.go). A re-ask is any call after the original one for the
// same problem, so a budget of 0 fails loudly on the first problem and a budget of N
// makes at most N follow-up calls. Budget 1, the default, is covered by the existing
// coverage and dispute tests, unchanged.

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/correlator"
)

// omitsTwoAndThree extends narrative 9 with events 0 and 1, leaving 2 and 3 in no
// cluster.
const omitsTwoAndThree = `[{"kind":"extends","narrative_id":9,"summary":"cache so far","confidence":0.9,"event_indices":[0,1]}]`

func (f coverageFixture) clusterWith(
	t *testing.T, client *fakeLLM, opts ...correlator.ClusterOption,
) ([]correlator.ClusterResult, correlator.Stats, error) {
	t.Helper()

	return correlator.Cluster(t.Context(), f.evts, f.existing, client, f.window, 128000, opts...)
}

func TestCluster_OmissionBudgetZeroFailsWithNoReask(t *testing.T) {
	f := newCoverageFixture(t)
	client := &fakeLLM{responses: []string{
		omitsTwoAndThree,
		`[{"kind":"new","title":"PR 7","summary":"s","confidence":0.9,"event_indices":[2,3]}]`,
	}}

	results, stats, err := f.clusterWith(t, client, correlator.WithOmissionReasks(0))

	require.Error(t, err)
	assert.Nil(t, results)
	require.ErrorContains(t, err, "2 (github/pr-7-opened)")
	require.ErrorContains(t, err, "3 (github/pr-7-merged)")
	require.ErrorContains(t, err, "omission re-ask budget (llm.max_omission_reasks) is 0",
		"the error says why nothing was re-asked, naming the key that would allow it")
	assert.Len(t, client.prompts, 1, "a budget of 0 makes no follow-up call")
	assert.Equal(t, 2, stats.OmittedEvents)
	assert.Zero(t, stats.RecoveredEvents)
	assert.Zero(t, stats.OmissionReasks)
}

// A round that places only some of the omitted events leaves the rest for the next
// round, which shows the clusters AS MERGED SO FAR, so a cluster the previous round
// created has a cluster_position the model can join.
func TestCluster_OmissionBudgetTwoRecoversWhatTheFirstRoundLeft(t *testing.T) {
	f := newCoverageFixture(t)
	client := &fakeLLM{responses: []string{
		omitsTwoAndThree,
		`[{"kind":"new","title":"PR 7","summary":"opened","confidence":0.9,"event_indices":[2]}]`,
		`[{"kind":"earlier","cluster_position":1,"summary":"opened and merged","confidence":0.8,"event_indices":[3]}]`,
		`[]`,
	}}

	results, stats, err := f.clusterWith(t, client, correlator.WithOmissionReasks(2))

	require.NoError(t, err)
	require.Len(t, client.prompts, 3, "the original call and two rounds")
	require.Len(t, results, 2)
	assert.Equal(t, "PR 7", results[1].Title)
	assert.Equal(t, "opened and merged", results[1].Summary)
	assert.Equal(t, []string{"github/pr-7-opened", "github/pr-7-merged"}, eventIDs(results[1:]))
	assert.InDelta(t, 0.8, results[1].ConfidenceOf(results[1].Events[1]), 1e-9,
		"the second round's confidence is kept for the member it placed")

	assert.Equal(t, 2, stats.OmittedEvents)
	assert.Equal(t, 2, stats.RecoveredEvents)
	assert.Equal(t, 2, stats.OmissionReasks)
	assert.Equal(t, 3, stats.Calls)

	second := client.prompts[2]
	assert.True(t, strings.HasPrefix(second, client.prompts[0]),
		"every round repeats the original prompt verbatim, so indices keep their meaning")
	assert.Contains(t, second, `cluster_position=1 kind=new title="PR 7" summary="opened" event_indices=[2]`,
		"the cluster the first round created is listed, by its position in the merged list")
	assert.Contains(t, second, "Assign each of them: [3]", "only the event still unplaced is asked about")
	assert.NotContains(t, second, "could not be used", "the first round was accepted, not refused")
}

// A refused round merges nothing and consumes the round; the next one quotes the
// refused response and the parser's reason, and asks about the same events again.
func TestCluster_OmissionBudgetTwoRecoversAfterARefusedRound(t *testing.T) {
	f := newCoverageFixture(t)
	const refused = `[{"kind":"new","title":"t","summary":"s","confidence":0.9,"event_indices":[0,2,3]}]`
	client := &fakeLLM{responses: []string{
		omitsTwoAndThree,
		refused,
		`[{"kind":"new","title":"PR 7","summary":"s","confidence":0.9,"event_indices":[2,3]}]`,
		`[]`,
	}}

	results, stats, err := f.clusterWith(t, client, correlator.WithOmissionReasks(2))

	require.NoError(t, err)
	require.Len(t, client.prompts, 3)
	require.Len(t, results, 2)
	assert.Equal(t, []string{"claude_code/e0", "claude_code/e1"}, eventIDs(results[:1]),
		"nothing from the refused round was merged")
	assert.Equal(t, []string{"github/pr-7-opened", "github/pr-7-merged"}, eventIDs(results[1:]))
	assert.Equal(t, 2, stats.RecoveredEvents)
	assert.Equal(t, 2, stats.OmissionReasks)

	second := client.prompts[2]
	assert.True(t, strings.HasPrefix(second, client.prompts[0]))
	assert.Contains(t, second, refused, "the refused response is quoted")
	assert.Contains(t, second, "was not one of the omitted indices", "with the parser's reason")
	assert.Contains(t, second, "Assign each of them: [2, 3]", "the same events are asked about again")
	assert.NotContains(t, second, "cluster_position=1", "the refused round added no cluster")
}

func TestCluster_OmissionBudgetTwoExhaustedFailsLoudly(t *testing.T) {
	f := newCoverageFixture(t)
	client := &fakeLLM{responses: []string{
		`[{"kind":"extends","narrative_id":9,"summary":"s","confidence":0.9,"event_indices":[0]}]`,
		`[{"kind":"new","title":"PR 7","summary":"s","confidence":0.9,"event_indices":[2]}]`,
		`[]`,
		// Would place everything, if a fourth call were ever made.
		`[{"kind":"earlier","cluster_position":0,"confidence":0.9,"event_indices":[1,3]}]`,
	}}

	results, stats, err := f.clusterWith(t, client, correlator.WithOmissionReasks(2))

	require.Error(t, err)
	assert.Nil(t, results, "never a best-effort partial result")
	require.ErrorContains(t, err, "1 (claude_code/e1)")
	require.ErrorContains(t, err, "3 (github/pr-7-merged)")
	assert.NotContains(t, err.Error(), "pr-7-opened", "the recovered event is not named as missing")
	require.ErrorContains(t, err, "after 2 re-ask round(s)")
	assert.Len(t, client.prompts, 3, "one original call and two rounds, never more")
	assert.Equal(t, 3, stats.OmittedEvents)
	assert.Equal(t, 1, stats.RecoveredEvents)
	assert.Equal(t, 2, stats.OmissionReasks)
}

// When the last round is refused, the error carries the refusal too: it is why the
// events are still unplaced.
func TestCluster_OmissionBudgetExhaustedByARefusalNamesTheReason(t *testing.T) {
	f := newCoverageFixture(t)
	client := &fakeLLM{responses: []string{omitsTwoAndThree, `not json`, `[{"kind":"bogus","confidence":0.9,"event_indices":[2]}]`}}

	_, stats, err := f.clusterWith(t, client, correlator.WithOmissionReasks(2))

	require.Error(t, err)
	require.ErrorContains(t, err, "2 (github/pr-7-opened)")
	require.ErrorContains(t, err, `unknown kind "bogus"`)
	assert.Len(t, client.prompts, 3)
	assert.Equal(t, 2, stats.OmissionReasks)
	assert.Zero(t, stats.RecoveredEvents)
}

// Every round is checked against the context window before it is sent. A round
// quoting a huge refused response is larger than the one before it.
func TestCluster_OmissionRoundOverTheContextWindowIsNotSent(t *testing.T) {
	f := newCoverageFixture(t)
	sys, user := correlator.BuildClusterPromptForTest(f.evts, f.existing)
	budget := correlator.EstimateTokensForTest(sys+user) * 2
	client := &fakeLLM{responses: []string{omitsTwoAndThree, "not json " + strings.Repeat("x", budget*4)}}

	results, stats, err := correlator.Cluster(t.Context(), f.evts, f.existing, client, f.window, budget,
		correlator.WithOmissionReasks(2))

	require.Error(t, err)
	assert.Nil(t, results)
	require.ErrorContains(t, err, "context window")
	require.ErrorContains(t, err, "2 (github/pr-7-opened)")
	assert.Len(t, client.prompts, 2, "the over-budget second round is not sent")
	assert.Equal(t, 1, stats.OmissionReasks)
}

func TestCluster_DisputeBudgetZeroFailsWithNoDisputeCall(t *testing.T) {
	f := newCoverageFixture(t)
	client := &fakeLLM{responses: []string{
		doubleAssignment,
		`[{"rationale":"r","event_index":0,"member":{"cluster_position":1},"confidence":0.8}]`,
	}}

	results, stats, err := f.clusterWith(t, client, correlator.WithDisputeReasks(0))

	require.Error(t, err)
	assert.Nil(t, results)
	require.ErrorContains(t, err, `github/pr-7-opened claimed by narrative 9 and new "PR 7"`)
	require.ErrorContains(t, err, "llm.max_dispute_reasks", "the error names the key that would allow a dispute call")
	assert.Len(t, client.prompts, 1, "a budget of 0 makes no dispute call")
	assert.Equal(t, 1, stats.DisputedEvents)
	assert.Zero(t, stats.DisputeCalls)
}

func TestCluster_DisputeBudgetTwoReasksARefusedBatch(t *testing.T) {
	f := newCoverageFixture(t)
	client := &fakeLLM{responses: []string{
		doubleAssignment,
		`[]`,
		`[{"rationale":"PR 7's own opening","event_index":0,"member":{"cluster_position":1},"confidence":0.8}]`,
	}}

	results, stats, err := f.clusterWith(t, client, correlator.WithDisputeReasks(2))

	require.NoError(t, err)
	require.Len(t, client.prompts, 3, "one clustering call and two dispute calls")
	require.Len(t, results, 2)
	assert.Contains(t, idsOf(results[1].Events), "github/pr-7-opened")
	assert.Contains(t, idsOf(results[0].ContextEvents), "github/pr-7-opened")
	assert.Equal(t, 2, stats.DisputeCalls, "both dispute calls are counted")
	assert.Equal(t, 1, stats.DisputeReasks, "one of them re-asked a refused response")
	assert.Equal(t, 3, stats.Calls)

	reask := client.prompts[2]
	assert.True(t, strings.HasPrefix(reask, client.prompts[1]),
		"the re-ask repeats the dispute prompt verbatim, so event_index keeps its meaning")
	assert.Contains(t, reask, "left unresolved", "it quotes the parser's reason")
	assert.Contains(t, reask, "The refused response:\n\n[]")
	assert.Equal(t, client.systemPrompts[1], client.systemPrompts[2])
}

func TestCluster_DisputeBudgetTwoExhaustedFailsNamingEveryRefusal(t *testing.T) {
	f := newCoverageFixture(t)
	client := &fakeLLM{responses: []string{
		doubleAssignment,
		`[]`,
		`not json`,
		`[{"rationale":"r","event_index":0,"member":{"cluster_position":1},"confidence":0.8}]`,
	}}

	results, stats, err := f.clusterWith(t, client, correlator.WithDisputeReasks(2))

	require.Error(t, err)
	assert.Nil(t, results)
	require.ErrorContains(t, err, "left unresolved")
	require.ErrorContains(t, err, "invalid character")
	require.ErrorContains(t, err, "llm.max_dispute_reasks")
	assert.Len(t, client.prompts, 3, "two dispute calls, never more")
	assert.Equal(t, 2, stats.DisputeCalls)
}

// A dispute re-ask quoting a huge refused response is checked against the context
// window before it is sent, like every other call.
func TestCluster_DisputeReaskOverTheContextWindowIsNotSent(t *testing.T) {
	f := newCoverageFixture(t)
	client := &fakeLLM{responses: []string{doubleAssignment, "not json " + strings.Repeat("x", 128000*4)}}

	results, stats, err := f.clusterWith(t, client, correlator.WithDisputeReasks(2))

	require.Error(t, err)
	assert.Nil(t, results)
	require.ErrorContains(t, err, "context window")
	require.ErrorContains(t, err, "invalid character", "the refusal that prompted the re-ask is named")
	assert.Len(t, client.prompts, 2, "the over-budget re-ask is not sent")
	assert.Equal(t, 1, stats.DisputeCalls)
}

// Re-asks are per batch: a refused later batch is re-asked on its own, and the
// batches that answered are not asked again.
func TestCluster_DisputeReaskIsPerBatch(t *testing.T) {
	w := newWideDispute(t)
	// Room for a re-ask's quoted refusal on top of one dispute, still short of two.
	budget := w.budget(t) + 400
	require.Greater(t, correlator.EstimateTokensForTest(
		strings.Join(pair(correlator.BuildDisputePromptForTest(w.results, 0, 1)), "")), budget,
		"precondition: two disputes together still exceed the window")
	client := &fakeLLM{responses: []string{
		w.response,
		`[{"rationale":"r0","event_index":0,"member":{"cluster_position":1},"confidence":0.6}]`,
		`[]`,
		`[{"rationale":"r1","event_index":0,"member":{"cluster_position":0},"confidence":0.7}]`,
		`[{"rationale":"r2","event_index":0,"member":{"cluster_position":1},"confidence":0.8}]`,
	}}

	results, stats, err := correlator.Cluster(t.Context(), w.evts, nil, client, w.window, budget,
		correlator.WithDisputeReasks(2))

	require.NoError(t, err)
	require.Len(t, client.prompts, 5, "three batches, and one re-ask for the second")
	assert.True(t, strings.HasPrefix(client.prompts[3], client.prompts[2]), "the re-ask is of the second batch")
	assert.Contains(t, client.prompts[4], `"`+strings.Repeat("c", 800)+`"`, "then the third batch")
	require.Len(t, results, 2)
	assert.Equal(t, 3, stats.DisputedEvents)
	assert.Equal(t, 4, stats.DisputeCalls)
	assert.Equal(t, 1, stats.DisputeReasks)
	assert.Equal(t, "r1", stats.Disputes[1].Rationale)
}

func pair(a, b string) []string { return []string{a, b} }
