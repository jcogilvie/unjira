// Package correlator_test exercises Cluster against a hand-written fake
// llmClient, not a real HTTP server — Cluster's own logic (window/adjacency
// filtering, prompt construction, response parsing, split/merge) is what's
// under test here, not any wire protocol. Compare
// internal/workflow/workflow_test.go's fakeMiner, which decouples
// MineProject from a concrete *jira.Client the same way.
package correlator_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/events"
)

// fakeLLM satisfies correlator's llmClient interface without making any
// real call. responses is consumed in call order; a test that only cares
// about one canned response can set a single-element slice.
type fakeLLM struct {
	responses []string
	prompts   []string // captured user prompts, in call order, for assertions
	err       error
}

func (f *fakeLLM) Complete(_ context.Context, _ string, userPrompt string) (string, error) {
	f.prompts = append(f.prompts, userPrompt)
	if f.err != nil {
		return "", f.err
	}

	idx := len(f.prompts) - 1
	if idx >= len(f.responses) {
		idx = len(f.responses) - 1
	}

	return f.responses[idx], nil
}

func TestCluster_EmptyEventsReturnsEmptyResult(t *testing.T) {
	llm := &fakeLLM{responses: []string{"[]"}}

	results, err := correlator.Cluster(t.Context(), nil, nil, llm, correlator.TimeRange{}, 128000)

	require.NoError(t, err)
	require.Empty(t, results)
}

func mustEvent(t *testing.T, source, externalID, summary string, occurredAt time.Time) correlator.Event {
	t.Helper()
	e := events.NewEvent(source, externalID, occurredAt, summary)
	return e
}

func TestCluster_SingleNewCluster(t *testing.T) {
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	evts := []correlator.Event{
		mustEvent(t, "claude_code", "e1", "Started investigating flaky test", base),
		mustEvent(t, "claude_code", "e2", "Found root cause: race condition", base.Add(time.Minute)),
	}
	llm := &fakeLLM{responses: []string{
		`[{"kind":"new","title":"Fix flaky test","summary":"Investigated and found a race condition.","event_indices":[0,1]}]`,
	}}

	results, err := correlator.Cluster(t.Context(), evts, nil, llm, correlator.TimeRange{
		Start: base.Add(-time.Hour), End: base.Add(time.Hour),
	}, 128000)

	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, correlator.ClusterNew, results[0].Kind)
	assert.Equal(t, "Fix flaky test", results[0].Title)
	assert.Equal(t, "Investigated and found a race condition.", results[0].Summary)
	require.Len(t, results[0].Events, 2)
	assert.Equal(t, "e1", results[0].Events[0].ExternalID)
	assert.Equal(t, "e2", results[0].Events[1].ExternalID)
}

func TestCluster_SingleExtendsCluster(t *testing.T) {
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	evts := []correlator.Event{
		mustEvent(t, "claude_code", "e1", "Continued the fix", base),
	}
	existing := []correlator.Narrative{
		{ID: 42, WindowStart: base.Add(-time.Hour), WindowEnd: base, Title: "Fix flaky test", Summary: "prior work"},
	}
	llm := &fakeLLM{responses: []string{
		`[{"kind":"extends","narrative_id":42,"title":"Fix flaky test","summary":"Continued and finished the fix.","event_indices":[0]}]`,
	}}

	results, err := correlator.Cluster(t.Context(), evts, existing, llm, correlator.TimeRange{
		Start: base.Add(-time.Minute), End: base.Add(time.Hour),
	}, 128000)

	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, correlator.ClusterExtends, results[0].Kind)
	assert.Equal(t, int64(42), results[0].NarrativeID)
	require.Len(t, results[0].Events, 1)
	assert.Equal(t, "e1", results[0].Events[0].ExternalID)
}

func TestCluster_MixedBatchOfNewAndExtends(t *testing.T) {
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	evts := []correlator.Event{
		mustEvent(t, "claude_code", "e1", "unrelated new work", base),
		mustEvent(t, "claude_code", "e2", "continuing prior work", base.Add(time.Minute)),
	}
	existing := []correlator.Narrative{
		{ID: 7, WindowStart: base.Add(-time.Hour), WindowEnd: base, Title: "Prior", Summary: "s"},
	}
	llm := &fakeLLM{responses: []string{
		`[{"kind":"new","title":"New thing","summary":"s1","event_indices":[0]},` +
			`{"kind":"extends","narrative_id":7,"title":"Prior","summary":"s2","event_indices":[1]}]`,
	}}

	results, err := correlator.Cluster(t.Context(), evts, existing, llm, correlator.TimeRange{
		Start: base.Add(-time.Minute), End: base.Add(time.Hour),
	}, 128000)

	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, correlator.ClusterNew, results[0].Kind)
	assert.Equal(t, correlator.ClusterExtends, results[1].Kind)
	assert.Equal(t, int64(7), results[1].NarrativeID)
}

func TestCluster_ExcludesNarrativesOutsideWindowAndNotAdjacent(t *testing.T) {
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	window := correlator.TimeRange{Start: base, End: base.Add(time.Hour)}

	existing := []correlator.Narrative{
		{ID: 1, Title: "adjacent-before", WindowStart: base.Add(-2 * time.Hour), WindowEnd: base},
		{ID: 2, Title: "overlapping", WindowStart: base.Add(30 * time.Minute), WindowEnd: base.Add(2 * time.Hour)},
		{ID: 3, Title: "far-away", WindowStart: base.Add(-24 * time.Hour), WindowEnd: base.Add(-12 * time.Hour)},
	}
	llm := &fakeLLM{responses: []string{"[]"}}

	_, err := correlator.Cluster(t.Context(), nil, existing, llm, window, 128000)

	require.NoError(t, err)
	require.Len(t, llm.prompts, 1)
	assert.Contains(t, llm.prompts[0], "adjacent-before")
	assert.Contains(t, llm.prompts[0], "overlapping")
	assert.NotContains(t, llm.prompts[0], "far-away")
}

func TestCluster_ResponseAndCallFailuresErrorLoudly(t *testing.T) {
	tests := []struct {
		name        string
		responses   []string
		llmErr      error
		wantErrText string
	}{
		{
			name:        "malformed JSON response",
			responses:   []string{"not json"},
			wantErrText: "not json",
		},
		{
			name:        "out of range event index",
			responses:   []string{`[{"kind":"new","title":"t","summary":"s","event_indices":[5]}]`},
			wantErrText: "out of range",
		},
		{
			name:        "unknown kind",
			responses:   []string{`[{"kind":"maybe","title":"t","summary":"s","event_indices":[0]}]`},
			wantErrText: `unknown kind "maybe"`,
		},
		{
			name:        "LLM call failure wrapped",
			llmErr:      errors.New("rate limited"),
			wantErrText: "rate limited",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := time.Now()
			llm := &fakeLLM{responses: tt.responses, err: tt.llmErr}
			evts := []correlator.Event{mustEvent(t, "claude_code", "e1", "x", base)}

			_, err := correlator.Cluster(t.Context(), evts, nil, llm, correlator.TimeRange{
				Start: base.Add(-time.Hour), End: base.Add(time.Hour),
			}, 128000)

			require.Error(t, err)
			assert.ErrorContains(t, err, tt.wantErrText)
		})
	}
}

func TestCluster_OversizedWindowSplitsAndCallsLLMPerHalf(t *testing.T) {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	// Two events spaced an hour apart so a time-midpoint split cleanly
	// separates them into two non-empty halves.
	evts := []correlator.Event{
		mustEvent(t, "claude_code", "e1", strings.Repeat("a", 4000), base),
		mustEvent(t, "claude_code", "e2", strings.Repeat("b", 4000), base.Add(time.Hour)),
	}
	llm := &fakeLLM{responses: []string{
		`[{"kind":"new","title":"first","summary":"s1","event_indices":[0]}]`,
		`[{"kind":"new","title":"second","summary":"s2","event_indices":[0]}]`,
	}}

	// contextWindowTokens sized so one event alone fits comfortably but
	// both together (in one prompt) don't.
	results, err := correlator.Cluster(t.Context(), evts, nil, llm, correlator.TimeRange{
		Start: base, End: base.Add(2 * time.Hour),
	}, 1200)

	require.NoError(t, err)
	require.Len(t, llm.prompts, 2, "expected one Complete call per split half, not one for the whole window")
	require.Len(t, results, 2)
	titles := []string{results[0].Title, results[1].Title}
	assert.ElementsMatch(t, []string{"first", "second"}, titles)
}

func TestCluster_SingleEventOverBudgetErrorsLoudlyWithoutCallingLLM(t *testing.T) {
	base := time.Now()
	evts := []correlator.Event{
		mustEvent(t, "claude_code", "e1", strings.Repeat("a", 10000), base),
	}
	llm := &fakeLLM{responses: []string{"[]"}}

	_, err := correlator.Cluster(t.Context(), evts, nil, llm, correlator.TimeRange{
		Start: base.Add(-time.Minute), End: base.Add(time.Minute),
	}, 100)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "e1")
	assert.Empty(t, llm.prompts, "no Complete call should be made for a doomed-to-fail oversized request")
}

func TestCluster_DegenerateBisectionOfIdenticalTimestampsErrorsLoudly(t *testing.T) {
	base := time.Now()
	// All events share the exact same timestamp, so a time-midpoint
	// bisection can never separate them into two non-empty halves.
	evts := []correlator.Event{
		mustEvent(t, "claude_code", "e1", strings.Repeat("a", 4000), base),
		mustEvent(t, "claude_code", "e2", strings.Repeat("b", 4000), base),
		mustEvent(t, "claude_code", "e3", strings.Repeat("c", 4000), base),
	}
	llm := &fakeLLM{responses: []string{"[]"}}

	_, err := correlator.Cluster(t.Context(), evts, nil, llm, correlator.TimeRange{
		Start: base.Add(-time.Minute), End: base.Add(time.Minute),
	}, 1000)

	require.Error(t, err)
}
