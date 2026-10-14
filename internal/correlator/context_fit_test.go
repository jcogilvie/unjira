package correlator_test

// context_fit_test.go pins how a clustering call fits its context narratives to the
// prompt budget (finding F16): whole narratives, in the order the caller ranked them,
// each charged its rendered prompt plus the follow-up headroom a re-ask needs to quote
// the cluster it is expected to produce. A window over budget bisects while it can,
// and only a window that cannot be split leaves narratives out.

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/correlator"
)

// contextFitBase is the instant every fixture here is built around.
var contextFitBase = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

// fitNarrative is a context narrative whose size is set by summaryChars, overlapping
// the fixture window. Its summary carries a marker so a prompt can be checked for it.
func fitNarrative(id int64, marker string, summaryChars int) correlator.Narrative {
	return correlator.Narrative{
		ID:          id,
		Title:       "story " + marker,
		Summary:     marker + " " + strings.Repeat("x", summaryChars),
		WindowStart: contextFitBase.Add(-time.Hour),
		WindowEnd:   contextFitBase,
	}
}

// fitWindow covers every fixture event.
var fitWindow = correlator.TimeRange{Start: contextFitBase, End: contextFitBase.Add(2 * time.Hour)}

// fitEvents share one timestamp, so the window cannot be bisected (bisectable): the
// case where fitting, not splitting, is what keeps the call within budget.
func fitEvents(t *testing.T) []correlator.Event {
	t.Helper()

	return []correlator.Event{
		mustEvent(t, "claude_code", "e0", "cache work part one", contextFitBase),
		mustEvent(t, "claude_code", "e1", "cache work part two", contextFitBase),
	}
}

// splittableEvents are an hour and a half apart, one in each half of fitWindow.
func splittableEvents(t *testing.T) []correlator.Event {
	t.Helper()

	return []correlator.Event{
		mustEvent(t, "claude_code", "e0", "cache work part one", contextFitBase),
		mustEvent(t, "claude_code", "e1", "cache work part two", contextFitBase.Add(90*time.Minute)),
	}
}

const (
	newClusterOfFirst = `[{"kind":"new","title":"a","summary":"s","confidence":0.9,"event_indices":[0]}]`
	newClusterOfOther = `[{"kind":"new","title":"b","summary":"s","confidence":0.9,"event_indices":[0]}]`
	notSameStory      = `{"same_story":false}`
)

// promptBudgetFor is the budget at which exactly these context narratives fit
// alongside evts: the prompt Cluster would build for them, plus the follow-up
// headroom each one is charged.
func promptBudgetFor(t *testing.T, evts []correlator.Event, existing []correlator.Narrative) int {
	t.Helper()
	sys, user := correlator.BuildClusterPromptForTest(evts, existing)

	return correlator.EstimateTokensForTest(sys+user) + len(existing)*correlator.FollowupTokensPerContextNarrativeForTest
}

const newClusterOfBoth = `[{"kind":"new","title":"t","summary":"s","confidence":0.9,"event_indices":[0,1]}]`

// A window that cannot be split, carrying a context set that does not fit, is cut to
// the narratives that do, in one call, and the excluded narratives are reported by
// id. Before the fit this was an irreducible-window failure.
func TestCluster_UnsplittableWindowFitsItsContextToTheBudget(t *testing.T) {
	evts := fitEvents(t)
	n1, n2, n3 := fitNarrative(1, "ALPHA", 2000), fitNarrative(2, "BRAVO", 2000), fitNarrative(3, "CHARLIE", 2000)
	budget := promptBudgetFor(t, evts, []correlator.Narrative{n1})
	client := &fakeLLM{responses: []string{newClusterOfBoth}}

	_, stats, err := correlator.Cluster(t.Context(), evts, []correlator.Narrative{n1, n2, n3}, client, fitWindow, budget)

	require.NoError(t, err)
	require.Len(t, client.prompts, 1)
	assert.Zero(t, stats.Splits)
	assert.Contains(t, client.prompts[0], "ALPHA", "the first-ranked narrative that fits is kept")
	assert.NotContains(t, client.prompts[0], "BRAVO")
	assert.NotContains(t, client.prompts[0], "CHARLIE")
	assert.Equal(t, []int64{2, 3}, stats.UnfittedContextNarratives, "every exclusion is reported, never silent")
	assert.LessOrEqual(t, correlator.EstimateTokensForTest(client.systemPrompts[0]+client.prompts[0]), budget)
}

// The caller's order is the ranking: the earlier-listed narrative wins a contested
// slot whatever its id, and one that does not fit does not stop a smaller,
// lower-ranked one that does.
func TestCluster_ContextFitKeepsTheCallersRankingAndSkipsWhatDoesNotFit(t *testing.T) {
	evts := fitEvents(t)
	big, small, alsoBig := fitNarrative(7, "BIG", 4000), fitNarrative(5, "SMALL", 100), fitNarrative(3, "ALSOBIG", 4000)
	budget := promptBudgetFor(t, evts, []correlator.Narrative{small})
	client := &fakeLLM{responses: []string{newClusterOfBoth}}

	_, stats, err := correlator.Cluster(t.Context(), evts, []correlator.Narrative{big, small, alsoBig}, client, fitWindow, budget)

	require.NoError(t, err)
	assert.Contains(t, client.prompts[0], "SMALL", "a lower-ranked narrative that fits is kept")
	assert.NotContains(t, client.prompts[0], "BIG")
	assert.Equal(t, []int64{7, 3}, stats.UnfittedContextNarratives, "reported in ranked order")

	first, second := fitNarrative(9, "FIRST", 2000), fitNarrative(1, "SECOND", 2000)
	client = &fakeLLM{responses: []string{newClusterOfBoth}}

	_, stats, err = correlator.Cluster(t.Context(), evts, []correlator.Narrative{first, second}, client, fitWindow,
		promptBudgetFor(t, evts, []correlator.Narrative{first}))

	require.NoError(t, err)
	assert.Contains(t, client.prompts[0], "FIRST", "list order decides, not id")
	assert.Equal(t, []int64{1}, stats.UnfittedContextNarratives)
}

// The reply's reserve comes off the context window before anything is fitted: a
// server sizes the prompt and max_tokens together.
func TestCluster_ResponseReserveComesOffThePromptBudget(t *testing.T) {
	evts := fitEvents(t)
	n1, n2 := fitNarrative(1, "ALPHA", 2000), fitNarrative(2, "BRAVO", 2000)
	window := promptBudgetFor(t, evts, []correlator.Narrative{n1, n2})
	reserve := window - promptBudgetFor(t, evts, []correlator.Narrative{n1})

	client := &fakeLLM{responses: []string{newClusterOfBoth}}
	_, stats, err := correlator.Cluster(t.Context(), evts, []correlator.Narrative{n1, n2}, client, fitWindow, window)
	require.NoError(t, err)
	assert.Empty(t, stats.UnfittedContextNarratives, "with no reserve, both fit the window")

	client = &fakeLLM{responses: []string{newClusterOfBoth}}
	_, stats, err = correlator.Cluster(t.Context(), evts, []correlator.Narrative{n1, n2}, client, fitWindow, window,
		correlator.WithResponseReserve(reserve))
	require.NoError(t, err)
	assert.Equal(t, []int64{2}, stats.UnfittedContextNarratives, "the reserve is room the prompt may not use")
	assert.LessOrEqual(t, correlator.EstimateTokensForTest(client.systemPrompts[0]+client.prompts[0]), window-reserve)
}

// A reserve that leaves no room for any prompt is a configuration error, reported
// before any call rather than discovered as a bisection that cannot end.
func TestCluster_ResponseReserveAtLeastTheWindowFailsLoudly(t *testing.T) {
	client := &fakeLLM{responses: []string{newClusterOfBoth}}

	_, _, err := correlator.Cluster(t.Context(), fitEvents(t), nil, client, fitWindow, 1000,
		correlator.WithResponseReserve(1000))

	require.ErrorContains(t, err, "llm.max_output_tokens")
	require.ErrorContains(t, err, "llm.context_window_tokens")
	assert.Empty(t, client.prompts, "nothing is sent")
}

// A window whose OWN events do not fit still bisects, and each half fits its own
// context. A narrative left out of both halves is reported once.
func TestCluster_WindowWhoseOwnEventsDoNotFitStillBisects(t *testing.T) {
	evts := splittableEvents(t)
	huge := fitNarrative(4, "HUGE", 20000)
	huge.WindowEnd = fitWindow.End
	budget := promptBudgetFor(t, evts[:1], nil) + 5
	require.Greater(t, promptBudgetFor(t, evts, nil), budget, "both events together must not fit")
	client := &fakeLLM{responses: []string{newClusterOfFirst, newClusterOfOther, notSameStory}}

	_, stats, err := correlator.Cluster(t.Context(), evts, []correlator.Narrative{huge}, client, fitWindow, budget)

	require.NoError(t, err)
	assert.Equal(t, 1, stats.Splits)
	for i, p := range client.prompts[:2] {
		assert.NotContains(t, p, "HUGE", "half %d fits its own context", i)
	}
	assert.Equal(t, []int64{4}, stats.UnfittedContextNarratives, "one narrative, reported once across the halves")
}

// Context over budget bisects before it leaves anything out, while the window can be
// split: each half sees the narratives near its own events, so every story stays
// visible to some call. Leaving a narrative out is what fragments it (F16), and a
// split only costs a call.
func TestCluster_ContextOverBudgetBisectsBeforeLeavingAnyNarrativeOut(t *testing.T) {
	evts := splittableEvents(t)
	early := fitNarrative(1, "EARLY", 2000)
	late := fitNarrative(2, "LATE", 2000)
	late.WindowStart, late.WindowEnd = evts[1].OccurredAt, evts[1].OccurredAt.Add(time.Hour)
	budget := max(promptBudgetFor(t, evts[:1], []correlator.Narrative{early}),
		promptBudgetFor(t, evts[1:], []correlator.Narrative{late}))
	require.Greater(t, promptBudgetFor(t, evts, []correlator.Narrative{early, late}), budget,
		"precondition: the whole window's context does not fit")
	client := &fakeLLM{responses: []string{newClusterOfFirst, newClusterOfOther, notSameStory}}

	_, stats, err := correlator.Cluster(t.Context(), evts, []correlator.Narrative{early, late}, client, fitWindow, budget)

	require.NoError(t, err)
	assert.Equal(t, 1, stats.Splits)
	assert.Empty(t, stats.UnfittedContextNarratives, "nothing is left out when bisecting can fit it")
	assert.Contains(t, client.prompts[0], "EARLY")
	assert.NotContains(t, client.prompts[0], "LATE")
	assert.Contains(t, client.prompts[1], "LATE")
}

// Each kept narrative is charged the headroom an omission re-ask needs to quote the
// cluster it produces, so a prompt filled to the budget can still be re-asked. Without
// that charge, BRAVO would fill the room and the re-ask below would be over budget.
func TestCluster_FittedContextLeavesRoomForAnOmissionReask(t *testing.T) {
	evts := fitEvents(t)
	n1 := fitNarrative(1, "ALPHA", 1000)
	n2 := fitNarrative(2, "BRAVO", 2*correlator.FollowupTokensPerContextNarrativeForTest-160)
	budget := promptBudgetFor(t, evts, []correlator.Narrative{n1})
	client := &fakeLLM{responses: []string{
		`[{"kind":"extends","narrative_id":1,"summary":"s","confidence":0.9,"event_indices":[0]}]`,
		`[{"kind":"new","title":"t","summary":"s","confidence":0.9,"event_indices":[1]}]`,
	}}

	results, stats, err := correlator.Cluster(t.Context(), evts, []correlator.Narrative{n1, n2}, client, fitWindow, budget)

	require.NoError(t, err)
	assert.Len(t, results, 2)
	assert.Equal(t, 1, stats.OmissionReasks)
	assert.Equal(t, []int64{2}, stats.UnfittedContextNarratives)
}
