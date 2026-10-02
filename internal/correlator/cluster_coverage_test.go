package correlator_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/llm"
)

// These tests pin the coverage contract: every numbered event must land in at
// least one cluster. Measured on real runs, a model can leave a whole PR's
// events in no cluster with no error, and `unjira watch`'s rolling window then
// moves past them, so no later pass ever clusters them. Cluster re-asks once
// for exactly the omitted events and errors loudly if any are still missing.

// coverageFixture is four in-window events and one context narrative (id 9),
// shaped so each test can omit different indices from the first response.
type coverageFixture struct {
	window   correlator.TimeRange
	evts     []correlator.Event
	existing []correlator.Narrative
}

func newCoverageFixture(t *testing.T) coverageFixture {
	t.Helper()
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	return coverageFixture{
		window: correlator.TimeRange{Start: base, End: base.Add(time.Hour)},
		evts: []correlator.Event{
			mustEvent(t, "claude_code", "e0", "cache work part one", base),
			mustEvent(t, "claude_code", "e1", "cache work part two", base.Add(time.Minute)),
			mustEvent(t, "github", "pr-7-opened", "PR #7 opened", base.Add(2*time.Minute)),
			mustEvent(t, "github", "pr-7-merged", "PR #7 merged", base.Add(3*time.Minute)),
		},
		existing: []correlator.Narrative{{
			ID: 9, Title: "Cache rework", Summary: "Reworking the shared cache",
			WindowStart: base.Add(-time.Hour), WindowEnd: base,
		}},
	}
}

func (f coverageFixture) cluster(t *testing.T, client *fakeLLM) ([]correlator.ClusterResult, correlator.Stats, error) {
	t.Helper()

	return correlator.Cluster(t.Context(), f.evts, f.existing, client, f.window, 128000)
}

// eventIDs flattens results into "source/external_id" strings, in order.
func eventIDs(results []correlator.ClusterResult) []string {
	var out []string
	for _, r := range results {
		for _, e := range r.Events {
			out = append(out, e.Source+"/"+e.ExternalID)
		}
	}

	return out
}

func TestCluster_CompleteResponseMakesExactlyOneCall(t *testing.T) {
	f := newCoverageFixture(t)
	client := &fakeLLM{responses: []string{
		`[{"kind":"extends","narrative_id":9,"summary":"s","event_indices":[0,1]},` +
			`{"kind":"new","title":"PR 7","summary":"s","event_indices":[2,3]}]`,
	}}

	results, stats, err := f.cluster(t, client)

	require.NoError(t, err)
	require.Len(t, client.prompts, 1, "a response that assigns every event must not pay for a re-ask")
	assert.Len(t, results, 2)
	assert.Zero(t, stats.OmittedEvents)
	assert.Zero(t, stats.RecoveredEvents)
}

func TestCluster_OmittedEventsAreRecoveredByOneReask(t *testing.T) {
	// First response for every case: indices 0 and 1 extend narrative 9 as
	// cluster position 0, and indices 2 and 3 are omitted.
	const firstResponse = `[{"kind":"extends","narrative_id":9,"summary":"cache so far","event_indices":[0,1]}]`

	tests := []struct {
		name  string
		reask string
		check func(t *testing.T, results []correlator.ClusterResult)
	}{
		{
			name:  "into a brand-new cluster",
			reask: `[{"kind":"new","title":"PR 7","summary":"opened and merged","event_indices":[2,3]}]`,
			check: func(t *testing.T, results []correlator.ClusterResult) {
				t.Helper()
				require.Len(t, results, 2)
				assert.Equal(t, correlator.ClusterNew, results[1].Kind)
				assert.Equal(t, "PR 7", results[1].Title)
				assert.Equal(t, []string{"github/pr-7-opened", "github/pr-7-merged"}, eventIDs(results[1:]))
			},
		},
		{
			// The omitted events extend narrative 9, which the first response
			// already extended: they must MERGE into that cluster rather than
			// produce a second result for the same narrative_id. (A context
			// narrative the first response left untouched is its own test below.)
			name:  "into a context narrative the first response already extended",
			reask: `[{"kind":"extends","narrative_id":9,"summary":"cache incl. PR 7","event_indices":[2,3]}]`,
			check: func(t *testing.T, results []correlator.ClusterResult) {
				t.Helper()
				require.Len(t, results, 1, "a second extends-9 must merge, not duplicate the narrative")
				assert.Equal(t, int64(9), results[0].NarrativeID)
				assert.Equal(t, "cache incl. PR 7", results[0].Summary,
					"the re-ask saw the cluster and the added events, so its summary is the cumulative one")
				assert.Len(t, results[0].Events, 4)
			},
		},
		{
			name:  "into a cluster from the first response, by position",
			reask: `[{"kind":"earlier","cluster_position":0,"summary":"cache and PR 7","event_indices":[2,3]}]`,
			check: func(t *testing.T, results []correlator.ClusterResult) {
				t.Helper()
				require.Len(t, results, 1)
				assert.Equal(t, correlator.ClusterExtends, results[0].Kind, "joining keeps the target's kind")
				assert.Equal(t, int64(9), results[0].NarrativeID)
				assert.Equal(t, "cache and PR 7", results[0].Summary)
				assert.Equal(t, []string{
					"claude_code/e0", "claude_code/e1", "github/pr-7-opened", "github/pr-7-merged",
				}, eventIDs(results))
			},
		},
		{
			name:  "into a cluster from the first response, keeping its summary when none is given",
			reask: `[{"kind":"earlier","cluster_position":0,"event_indices":[2,3]}]`,
			check: func(t *testing.T, results []correlator.ClusterResult) {
				t.Helper()
				require.Len(t, results, 1)
				assert.Equal(t, "cache so far", results[0].Summary)
				assert.Len(t, results[0].Events, 4)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newCoverageFixture(t)
			client := &fakeLLM{responses: []string{firstResponse, tt.reask}}

			results, stats, err := f.cluster(t, client)

			require.NoError(t, err)
			require.Len(t, client.prompts, 2, "exactly one re-ask")
			tt.check(t, results)
			assert.Equal(t, 2, stats.OmittedEvents)
			assert.Equal(t, 2, stats.RecoveredEvents)
			assert.Equal(t, 2, stats.Calls)
		})
	}
}

// A context narrative that the first response did not touch at all is a
// distinct target from the merge case above: the recovered events become a
// new ClusterExtends result.
func TestCluster_OmittedEventsCanExtendAnUntouchedContextNarrative(t *testing.T) {
	f := newCoverageFixture(t)
	client := &fakeLLM{responses: []string{
		`[{"kind":"new","title":"PR 7","summary":"s","event_indices":[2,3]}]`,
		`[{"kind":"extends","narrative_id":9,"summary":"cache continued","event_indices":[0,1]}]`,
	}}

	results, _, err := f.cluster(t, client)

	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, correlator.ClusterExtends, results[1].Kind)
	assert.Equal(t, int64(9), results[1].NarrativeID)
	assert.Equal(t, "cache continued", results[1].Summary)
	assert.Equal(t, []string{"claude_code/e0", "claude_code/e1"}, eventIDs(results[1:]))
}

func TestCluster_ReaskPromptCarriesWhatTheModelNeeds(t *testing.T) {
	f := newCoverageFixture(t)
	client := &fakeLLM{responses: []string{
		`[{"kind":"extends","narrative_id":9,"summary":"cache so far","event_indices":[0,1]}]`,
		`[{"kind":"new","title":"PR 7","summary":"s","event_indices":[2,3]}]`,
	}}

	_, _, err := f.cluster(t, client)
	require.NoError(t, err)
	require.Len(t, client.prompts, 2)

	reask := client.prompts[1]
	assert.True(t, strings.HasPrefix(reask, client.prompts[0]),
		"the re-ask must show the model the same numbered events and context it first saw, verbatim, "+
			"so its indices mean what they meant the first time")
	assert.Contains(t, reask, "CONTEXT ONLY")
	assert.Contains(t, reask, `cluster_position=0 kind=extends narrative_id=9`,
		"first-response clusters are listed by position, which is how a new one is referred to")
	assert.Contains(t, reask, `"cache so far"`)
	assert.Contains(t, reask, "[2, 3]", "the omitted indices are named explicitly")

	sys := client.systemPrompts[1]
	assert.Contains(t, sys, "CONTEXT ONLY", "the context-only rule is kept on the re-ask")
	assert.Contains(t, sys, `"earlier"`, "the re-ask's schema offers joining a first-response cluster")
}

func TestCluster_ReaskCarriesRulesAndInstruction(t *testing.T) {
	f := newCoverageFixture(t)
	client := &fakeLLM{responses: []string{
		`[{"kind":"extends","narrative_id":9,"summary":"s","event_indices":[0,1]}]`,
		`[{"kind":"new","title":"PR 7","summary":"s","event_indices":[2,3]}]`,
	}}

	_, _, err := correlator.Cluster(t.Context(), f.evts, f.existing, client, f.window, 128000,
		correlator.WithInstruction("SENTINEL INSTRUCTION"))

	require.NoError(t, err)
	require.Len(t, client.systemPrompts, 2)
	assert.Contains(t, client.systemPrompts[1], "SENTINEL INSTRUCTION",
		"a reviewer's split directive must still govern where the omitted events go")
}

func TestCluster_EventsStillOmittedAfterReaskErrorLoudly(t *testing.T) {
	f := newCoverageFixture(t)
	client := &fakeLLM{responses: []string{
		`[{"kind":"extends","narrative_id":9,"summary":"s","event_indices":[0]}]`,
		`[{"kind":"new","title":"PR 7","summary":"s","event_indices":[2]}]`,
	}}

	results, stats, err := f.cluster(t, client)

	require.Error(t, err)
	assert.Nil(t, results, "never a best-effort partial result")
	require.ErrorContains(t, err, "1 (claude_code/e1)")
	require.ErrorContains(t, err, "3 (github/pr-7-merged)")
	assert.NotContains(t, err.Error(), "pr-7-opened", "the recovered event is not named as missing")
	require.Len(t, client.prompts, 2, "one re-ask, not a retry loop")
	assert.Equal(t, 3, stats.OmittedEvents)
	assert.Equal(t, 1, stats.RecoveredEvents)
}

func TestCluster_MalformedReaskErrorsLoudly(t *testing.T) {
	const firstResponse = `[{"kind":"extends","narrative_id":9,"summary":"s","event_indices":[0,1]}]`

	tests := []struct {
		name    string
		reask   string
		wantErr string
	}{
		{
			name:    "out-of-range index",
			reask:   `[{"kind":"new","title":"t","summary":"s","event_indices":[2,3,4]}]`,
			wantErr: "event_indices value 4 out of range [0,4)",
		},
		{
			name:    "index that was already assigned",
			reask:   `[{"kind":"new","title":"t","summary":"s","event_indices":[0,2,3]}]`,
			wantErr: "event_indices value 0 was not one of the omitted indices [2 3]",
		},
		{
			name:    "earlier cluster position out of range",
			reask:   `[{"kind":"earlier","cluster_position":1,"event_indices":[2,3]}]`,
			wantErr: "cluster_position 1 out of range [0,1)",
		},
		{
			name:    "earlier without a cluster position",
			reask:   `[{"kind":"earlier","event_indices":[2,3]}]`,
			wantErr: `kind "earlier" without cluster_position`,
		},
		{
			name:    "unknown kind",
			reask:   `[{"kind":"bogus","event_indices":[2,3]}]`,
			wantErr: `unknown kind "bogus"`,
		},
		{
			name:    "invalid JSON",
			reask:   `not json`,
			wantErr: "parsing cluster re-ask response",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newCoverageFixture(t)
			client := &fakeLLM{responses: []string{firstResponse, tt.reask}}

			results, _, err := f.cluster(t, client)

			require.Error(t, err)
			assert.Nil(t, results)
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

// "earlier" is a re-ask-only kind: a first response has no earlier clusters to
// refer to, so it must stay an unknown kind there, exactly as before.
func TestCluster_EarlierKindIsRejectedInAFirstResponse(t *testing.T) {
	f := newCoverageFixture(t)
	client := &fakeLLM{responses: []string{
		`[{"kind":"earlier","cluster_position":0,"event_indices":[0,1,2,3]}]`,
	}}

	_, _, err := f.cluster(t, client)

	require.Error(t, err)
	require.ErrorContains(t, err, `unknown kind "earlier"`)
}

// Double assignment is deliberately left alone: a separate design (shared
// context attached to several narratives) will change that contract. An event
// in two clusters must parse exactly as it did before coverage was checked —
// accepted, present in both, no error and no re-ask.
func TestCluster_DoubleAssignmentIsUnchanged(t *testing.T) {
	f := newCoverageFixture(t)
	client := &fakeLLM{responses: []string{
		`[{"kind":"extends","narrative_id":9,"summary":"s","event_indices":[0,1,2]},` +
			`{"kind":"new","title":"PR 7","summary":"s","event_indices":[2,3]}]`,
	}}

	results, stats, err := f.cluster(t, client)

	require.NoError(t, err)
	require.Len(t, client.prompts, 1, "double assignment is not omission, so no re-ask")
	require.Len(t, results, 2)
	assert.Equal(t, []string{
		"claude_code/e0", "claude_code/e1", "github/pr-7-opened",
		"github/pr-7-opened", "github/pr-7-merged",
	}, eventIDs(results))
	assert.Zero(t, stats.OmittedEvents)
}

// An eligible event (a context narrative's uncommitted link) that the model
// leaves in no cluster is NOT re-asked for, and that is deliberate. Persist only
// touches events a cluster names, so an omitted eligible event simply keeps the
// link it already has — nothing is lost, and its narrative is unchanged. The
// re-ask exists for IN-WINDOW events, which have no link: left out, they stay
// unlinked, and `watch`'s rolling window can move past them for good.
//
// Re-asking for eligible events too would spend a model call on omissions that
// lose nothing, and they dominate the index space — 242 of 246 assignable events
// in the F16 measurements were eligible context events.
func TestCluster_OmittedEligibleEventIsNotReasked(t *testing.T) {
	f := newCoverageFixture(t)
	f.existing[0].EligibleEvents = []correlator.Event{
		mustEvent(t, "github", "pr-500", "ELIGIBLE uncommitted work", f.window.Start.Add(-30*time.Minute)),
	}
	client := &fakeLLM{responses: []string{
		`[{"kind":"new","title":"all","summary":"s","event_indices":[0,1,2,3]}]`,
	}}

	results, stats, err := f.cluster(t, client)

	require.NoError(t, err)
	assert.Len(t, client.prompts, 1, "an omitted eligible event keeps its link, so no re-ask")
	assert.Len(t, results, 1)
	assert.Zero(t, stats.OmittedEvents, "only in-window omissions are counted")
}

// Each bisected half is its own model call with its own index space, so each
// half checks and re-asks for itself; the merge that follows only unions.
func TestCluster_SplitHalvesEachRecoverTheirOwnOmissions(t *testing.T) {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	evts := []correlator.Event{
		mustEvent(t, "claude_code", "a1", strings.Repeat("a", 2000), base),
		mustEvent(t, "claude_code", "a2", strings.Repeat("b", 2000), base.Add(time.Minute)),
		mustEvent(t, "claude_code", "b1", strings.Repeat("c", 2000), base.Add(time.Hour)),
		mustEvent(t, "claude_code", "b2", strings.Repeat("d", 2000), base.Add(time.Hour+time.Minute)),
	}
	client := &fakeLLM{
		responses: []string{
			// First half: omits a2, then recovers it into its own cluster 0.
			`[{"kind":"new","title":"A","summary":"s","event_indices":[0]}]`,
			`[{"kind":"earlier","cluster_position":0,"event_indices":[1]}]`,
			// Second half: omits b1, then recovers it as a new cluster.
			`[{"kind":"new","title":"B","summary":"s","event_indices":[1]}]`,
			`[{"kind":"new","title":"B0","summary":"s","event_indices":[0]}]`,
			// Merge-boundary same-story check between A and B.
			`{"same_story":false}`,
		},
		usagePerCall: llm.Usage{PromptTokens: 10, CompletionTokens: 1},
	}

	results, stats, err := correlator.Cluster(t.Context(), evts, nil, client, correlator.TimeRange{
		Start: base, End: base.Add(2 * time.Hour),
	}, splitBudget(t, evts))

	require.NoError(t, err)
	require.Len(t, client.prompts, 5, "two halves, one re-ask each, one merge-boundary check")
	assert.ElementsMatch(t, []string{
		"claude_code/a1", "claude_code/a2", "claude_code/b1", "claude_code/b2",
	}, eventIDs(results), "nothing slips through a bisected window")
	assert.Equal(t, 1, stats.Splits)
	assert.Equal(t, 2, stats.OmittedEvents, "omissions sum across halves")
	assert.Equal(t, 2, stats.RecoveredEvents)
	assert.Equal(t, 5, stats.Calls)
}

func TestCluster_SplitHalfStillOmittingErrorsLoudly(t *testing.T) {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	evts := []correlator.Event{
		mustEvent(t, "claude_code", "a1", strings.Repeat("a", 2000), base),
		mustEvent(t, "claude_code", "a2", strings.Repeat("b", 2000), base.Add(time.Minute)),
		mustEvent(t, "claude_code", "b1", strings.Repeat("c", 2000), base.Add(time.Hour)),
		mustEvent(t, "claude_code", "b2", strings.Repeat("d", 2000), base.Add(time.Hour+time.Minute)),
	}
	client := &fakeLLM{responses: []string{
		`[{"kind":"new","title":"A","summary":"s","event_indices":[0,1]}]`,
		`[{"kind":"new","title":"B","summary":"s","event_indices":[1]}]`,
		`[]`,
	}}

	results, _, err := correlator.Cluster(t.Context(), evts, nil, client, correlator.TimeRange{
		Start: base, End: base.Add(2 * time.Hour),
	}, splitBudget(t, evts))

	require.Error(t, err)
	assert.Nil(t, results)
	require.ErrorContains(t, err, "claude_code/b1")
}

// splitBudget fits three of these four equal-sized events but not all four, so
// the window bisects once and each two-event half fits with room left for its
// re-ask, which is larger than the half's first prompt.
func splitBudget(t *testing.T, evts []correlator.Event) int {
	t.Helper()
	require.Len(t, evts, 4)
	budget := budgetFitting(t, evts[:3], nil)
	require.Less(t, budget, budgetFitting(t, evts, nil)*100/110, "the whole window must not fit")

	return budget
}

// A re-ask is a bigger prompt than the one that fitted: it adds the clusters
// already produced. If it does not fit, that is a loud error naming the omitted
// events, not a request sent over budget and not a silent partial result.
func TestCluster_ReaskOverBudgetErrorsLoudly(t *testing.T) {
	f := newCoverageFixture(t)
	sys, user := correlator.BuildClusterPromptForTest(f.evts, f.existing)
	exactFit := correlator.EstimateTokensForTest(sys + user)
	client := &fakeLLM{responses: []string{
		`[{"kind":"extends","narrative_id":9,"summary":"s","event_indices":[0,1]}]`,
	}}

	results, _, err := correlator.Cluster(t.Context(), f.evts, f.existing, client, f.window, exactFit)

	require.Error(t, err)
	assert.Nil(t, results)
	require.ErrorContains(t, err, "re-ask")
	require.ErrorContains(t, err, "2 (github/pr-7-opened)")
	assert.Len(t, client.prompts, 1, "an over-budget re-ask is not sent")
}
