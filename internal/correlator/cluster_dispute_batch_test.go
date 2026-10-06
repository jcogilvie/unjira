package correlator_test

// cluster_dispute_batch_test.go pins how the dispute re-ask fits the context window:
// a dispute set too large for one call is split into calls that each fit, and a single
// dispute too large on its own is a loud refusal. Never a truncated claimant, never a
// dropped dispute, never a prompt sent over budget.

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/correlator"
)

// wideDispute is a pass whose dispute set is larger than the context window while
// each dispute alone is not: two new clusters both claim events 0, 1 and 2, and each
// dispute lists BOTH claimants' other members, so every big summary is repeated once
// per dispute. The clustering prompt numbers each event once, which is how a pass
// that clustered within the window reaches a dispute step that does not fit.
type wideDispute struct {
	window   correlator.TimeRange
	evts     []correlator.Event
	response string
	// results is the clustering response as Cluster parses it, for sizing the
	// dispute prompt from the real renderer.
	results []correlator.ClusterResult
}

func newWideDispute(t *testing.T) wideDispute {
	t.Helper()
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	evts := make([]correlator.Event, 0, 7)
	for i, c := range "abcdef" {
		evts = append(evts, mustEvent(t, "claude_code", "w"+string(c), strings.Repeat(string(c), 800),
			base.Add(time.Duration(i)*time.Minute)))
	}
	evts = append(evts, mustEvent(t, "github", "pr-1-opened", "PR #1 opened", base.Add(10*time.Minute)))

	return wideDispute{
		window: correlator.TimeRange{Start: base, End: base.Add(time.Hour)},
		evts:   evts,
		response: `[{"kind":"new","title":"A","summary":"a","confidence":0.9,"event_indices":[0,1,2,3,4,5]},` +
			`{"kind":"new","title":"B","summary":"b","confidence":0.9,"event_indices":[0,1,2,6]}]`,
		results: []correlator.ClusterResult{
			{Kind: correlator.ClusterNew, Title: "A", Summary: "a", Confidence: 0.9, Events: evts[:6]},
			{
				Kind: correlator.ClusterNew, Title: "B", Summary: "b", Confidence: 0.9,
				Events: []correlator.Event{evts[0], evts[1], evts[2], evts[6]},
			},
		},
	}
}

// budget is a context window the clustering call and any ONE dispute fit, and no
// two disputes together do — checked, not assumed, so a change to either prompt's
// size cannot quietly turn these into tests of nothing.
func (w wideDispute) budget(t *testing.T) int {
	t.Helper()
	est := func(sys, user string) int { return correlator.EstimateTokensForTest(sys + user) }

	clustering := est(correlator.BuildClusterPromptForTest(w.evts, nil))
	single := max(est(correlator.BuildDisputePromptForTest(w.results, 0)),
		est(correlator.BuildDisputePromptForTest(w.results, 1)),
		est(correlator.BuildDisputePromptForTest(w.results, 2)))
	budget := max(clustering, single)

	require.Greater(t, est(correlator.BuildDisputePromptForTest(w.results, 0, 1)), budget,
		"precondition: two disputes together exceed the window")
	require.Greater(t, est(correlator.BuildDisputePromptForTest(w.results, 1, 2)), budget,
		"precondition: two disputes together exceed the window")

	return budget
}

// TestCluster_DisputesOverTheContextWindowAreBatchedNotDropped: a dispute set too
// large for one call is split into calls that each fit, every disputed event is
// resolved exactly once, and the answers are applied together.
func TestCluster_DisputesOverTheContextWindowAreBatchedNotDropped(t *testing.T) {
	w := newWideDispute(t)
	budget := w.budget(t)
	client := &fakeLLM{responses: []string{
		w.response,
		`[{"rationale":"r0","event_index":0,"member":{"cluster_position":1},"confidence":0.6}]`,
		`[{"rationale":"r1","event_index":0,"member":{"cluster_position":0},"confidence":0.7}]`,
		`[{"rationale":"r2","event_index":0,"member":{"cluster_position":1},"confidence":0.8}]`,
	}}

	results, stats, err := correlator.Cluster(t.Context(), w.evts, nil, client, w.window, budget)

	require.NoError(t, err)
	require.Len(t, client.prompts, 4, "one clustering call, then one dispute call per batch that fits")
	for i, c := range "abc" {
		p := client.prompts[1+i]
		assert.LessOrEqual(t, correlator.EstimateTokensForTest(client.systemPrompts[1+i]+p), budget,
			"dispute call %d must fit the context window", i)
		assert.Contains(t, p, `event_index=0 [claude_code] "`+strings.Repeat(string(c), 800)+`"`,
			"each batch numbers its own disputes from 0")
		assert.NotContains(t, p, "event_index=1 ", "one dispute per call at this budget")
	}

	require.Len(t, results, 2)
	assert.Equal(t, []string{"claude_code/wb", "claude_code/wd", "claude_code/we", "claude_code/wf"},
		idsOf(results[0].Events))
	assert.Equal(t, []string{"claude_code/wa", "claude_code/wc"}, idsOf(results[0].ContextEvents))
	assert.Equal(t, []string{"claude_code/wa", "claude_code/wc", "github/pr-1-opened"}, idsOf(results[1].Events))
	assert.Equal(t, []string{"claude_code/wb"}, idsOf(results[1].ContextEvents))
	assert.InDelta(t, 0.7, results[0].ConfidenceOf(w.evts[1]), 1e-9, "a later batch's confidence is applied")

	assert.Equal(t, 3, stats.DisputedEvents)
	assert.Equal(t, 3, stats.DisputeCalls)
	assert.Equal(t, 4, stats.Calls, "every dispute call is counted")
	require.Len(t, stats.Disputes, 3)
	assert.Equal(t, []string{"claude_code/wa", "claude_code/wb", "claude_code/wc"},
		[]string{stats.Disputes[0].Event, stats.Disputes[1].Event, stats.Disputes[2].Event},
		"resolutions are reported in dispute order, across batches")
	assert.Equal(t, "r1", stats.Disputes[1].Rationale)
}

// TestCluster_DisputeBatchesHoldAsManyDisputesAsFit: a batch is filled, in order,
// until the next dispute would take it over budget, so a window that fits two of the
// three disputes asks [0, 1] then [2] — and the second call's answer, numbered from
// 0 in its own batch, resolves the third dispute.
func TestCluster_DisputeBatchesHoldAsManyDisputesAsFit(t *testing.T) {
	w := newWideDispute(t)
	est := func(sys, user string) int { return correlator.EstimateTokensForTest(sys + user) }
	budget := est(correlator.BuildDisputePromptForTest(w.results, 0, 1))
	require.GreaterOrEqual(t, budget, est(correlator.BuildClusterPromptForTest(w.evts, nil)),
		"precondition: the clustering call fits")
	require.Greater(t, est(correlator.BuildDisputePromptForTest(w.results)), budget,
		"precondition: all three disputes together do not")

	client := &fakeLLM{responses: []string{
		w.response,
		`[{"rationale":"r","event_index":0,"member":{"cluster_position":1},"confidence":0.6},` +
			`{"rationale":"r","event_index":1,"member":{"cluster_position":0},"confidence":0.6}]`,
		`[{"rationale":"r","event_index":0,"member":{"cluster_position":1},"confidence":0.6}]`,
	}}

	results, stats, err := correlator.Cluster(t.Context(), w.evts, nil, client, w.window, budget)

	require.NoError(t, err)
	require.Len(t, client.prompts, 3)
	sys, user := correlator.BuildDisputePromptForTest(w.results, 0, 1)
	assert.Equal(t, user, client.prompts[1], "the first batch is disputes 0 and 1, rendered as one call renders them")
	assert.Equal(t, sys, client.systemPrompts[1])
	sys, user = correlator.BuildDisputePromptForTest(w.results, 2)
	assert.Equal(t, user, client.prompts[2], "the second is dispute 2, renumbered from 0")
	assert.Equal(t, sys, client.systemPrompts[2])
	assert.Equal(t, 2, stats.DisputeCalls)
	assert.Equal(t, []string{"claude_code/wa", "claude_code/wc", "github/pr-1-opened"}, idsOf(results[1].Events))
}

// TestCluster_DisputesThatFitTogetherStayOneCall: batching is for a set that does
// not fit, not a per-dispute default — each call re-pays the system prompt — and the
// one call it makes is the prompt the unbatched re-ask always sent.
func TestCluster_DisputesThatFitTogetherStayOneCall(t *testing.T) {
	w := newWideDispute(t)
	client := &fakeLLM{responses: []string{
		w.response,
		`[{"rationale":"r","event_index":0,"member":{"cluster_position":1},"confidence":0.6},` +
			`{"rationale":"r","event_index":1,"member":{"cluster_position":0},"confidence":0.6},` +
			`{"rationale":"r","event_index":2,"member":{"cluster_position":1},"confidence":0.6}]`,
	}}

	_, stats, err := correlator.Cluster(t.Context(), w.evts, nil, client, w.window, 128000)

	require.NoError(t, err)
	require.Len(t, client.prompts, 2)
	sys, user := correlator.BuildDisputePromptForTest(w.results)
	assert.Equal(t, user, client.prompts[1], "one call holds every dispute")
	assert.Equal(t, sys, client.systemPrompts[1])
	assert.Equal(t, 1, stats.DisputeCalls)
}

// TestCluster_ABadAnswerInALaterDisputeBatchFailsThePass: answers are applied only
// once every batch has answered, so a malformed later batch leaves nothing half
// resolved, and no further call is made.
func TestCluster_ABadAnswerInALaterDisputeBatchFailsThePass(t *testing.T) {
	w := newWideDispute(t)
	client := &fakeLLM{responses: []string{
		w.response,
		`[{"rationale":"r","event_index":0,"member":{"cluster_position":1},"confidence":0.6}]`,
		`[]`,
		`[{"rationale":"r","event_index":0,"member":{"cluster_position":1},"confidence":0.6}]`,
	}}

	results, _, err := correlator.Cluster(t.Context(), w.evts, nil, client, w.window, w.budget(t))

	require.Error(t, err)
	assert.Nil(t, results)
	require.ErrorContains(t, err, "dispute re-ask call 2 of 3")
	require.ErrorContains(t, err, "1 disputed event(s) left unresolved: 0 (claude_code/wb)")
	assert.Len(t, client.prompts, 3, "no call after the batch that failed")
}

// TestCluster_ADisputeTooLargeAloneFailsLoudly: one disputed event whose claimants'
// member events alone exceed the window cannot be split further without cutting a
// claimant's description, so the pass refuses — naming the event and its claimants —
// before spending a dispute call. Reached through a bisection, the case that makes it
// likely: each claimant holds a half, so the dispute lists more than either half did.
func TestCluster_ADisputeTooLargeAloneFailsLoudly(t *testing.T) {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	evts := []correlator.Event{
		mustEvent(t, "claude_code", "a1", strings.Repeat("a", 6000), base),
		mustEvent(t, "claude_code", "a2", strings.Repeat("b", 6000), base.Add(time.Minute)),
		mustEvent(t, "claude_code", "b1", strings.Repeat("c", 6000), base.Add(time.Hour)),
		mustEvent(t, "claude_code", "b2", strings.Repeat("d", 6000), base.Add(time.Hour+time.Minute)),
	}
	spanning := mustEvent(t, "claude_code", "elig", "eligible work spanning the split", base.Add(30*time.Minute))
	existing := []correlator.Narrative{{
		ID: 9, Title: "spans the split", Summary: "s",
		WindowStart: base, WindowEnd: base.Add(2 * time.Hour),
		EligibleEvents: []correlator.Event{spanning},
	}}
	client := &fakeLLM{responses: []string{
		`[{"kind":"new","title":"A","summary":"s","confidence":0.9,"event_indices":[0,1,2]}]`,
		`[{"kind":"new","title":"B","summary":"s","confidence":0.9,"event_indices":[0,1,2]}]`,
		`{"same_story":false}`,
		`[{"rationale":"r","event_index":0,"member":{"cluster_position":1},"confidence":0.6}]`,
	}}

	sys, user := correlator.BuildClusterPromptForTest(append(evts[:3:3], spanning), existing)
	budget := correlator.EstimateTokensForTest(sys + user)

	results, stats, err := correlator.Cluster(t.Context(), evts, existing, client,
		correlator.TimeRange{Start: base, End: base.Add(2 * time.Hour)}, budget)

	require.Error(t, err)
	assert.Nil(t, results)
	require.Equal(t, 1, stats.Splits, "precondition: the window bisected")
	assert.Len(t, client.prompts, 3, "two halves and the same-story check; no dispute call over budget")
	require.ErrorContains(t, err, `claude_code/elig claimed by new "A" and new "B"`)
	require.ErrorContains(t, err, "alone is estimated at")
	require.ErrorContains(t, err, "F52")
	assert.Zero(t, stats.DisputeCalls)
}
