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
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/store"
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

func TestCluster_HydratedNarrativeEventsAppearAsContextNotAssignable(t *testing.T) {
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	window := correlator.TimeRange{Start: base, End: base.Add(time.Hour)}

	existing := []correlator.Narrative{
		{
			ID: 9, Title: "Cache rework", Summary: "Reworking the shared cache",
			WindowStart: base.Add(-2 * time.Hour), WindowEnd: base,
			Events: []correlator.Event{
				mustEvent(t, "github", "pr-412", "PR #412 add cache layer", base.Add(-2*time.Hour)),
			},
		},
		{
			ID: 3, Title: "far-away", Summary: "unrelated",
			WindowStart: base.Add(-48 * time.Hour), WindowEnd: base.Add(-24 * time.Hour),
			Events: []correlator.Event{
				mustEvent(t, "github", "pr-1", "PR #1 ancient", base.Add(-48*time.Hour)),
			},
		},
	}
	inWindow := []correlator.Event{
		mustEvent(t, "claude_code", "e1", "debugging cache eviction", base.Add(5*time.Minute)),
	}
	llm := &fakeLLM{responses: []string{"[]"}}

	_, err := correlator.Cluster(t.Context(), inWindow, existing, llm, window, 128000)

	require.NoError(t, err)
	require.Len(t, llm.prompts, 1)
	prompt := llm.prompts[0]
	// Adjacent narrative 9 and its event are present as context.
	assert.Contains(t, prompt, "Cache rework")
	assert.Contains(t, prompt, "PR #412 add cache layer")
	// Non-adjacent narrative 3 is excluded entirely.
	assert.NotContains(t, prompt, "far-away")
	assert.NotContains(t, prompt, "PR #1 ancient")
	// The in-window event is present and numbered/assignable.
	assert.Contains(t, prompt, "debugging cache eviction")
	// Structural: two labeled sections exist, and the context section warns
	// against reassigning its events.
	assert.Contains(t, prompt, "Events to cluster")
	assert.Contains(t, prompt, "CONTEXT ONLY")
}

func TestCluster_UsesNarrativeContextEventsForExtendsDecision(t *testing.T) {
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	window := correlator.TimeRange{Start: base, End: base.Add(time.Hour)}
	existing := []correlator.Narrative{{
		ID: 9, Title: "Cache rework", Summary: "Reworking the shared cache",
		WindowStart: base.Add(-2 * time.Hour), WindowEnd: base,
		Events: []correlator.Event{
			mustEvent(t, "github", "pr-412", "PR #412 add cache layer", base.Add(-2*time.Hour)),
		},
	}}
	inWindow := []correlator.Event{
		mustEvent(t, "claude_code", "e1", "fixed the cache eviction bug from PR #412", base.Add(5*time.Minute)),
	}
	// Model, having seen narrative 9's events, extends it.
	llm := &fakeLLM{responses: []string{
		`[{"kind":"extends","narrative_id":9,"title":"Cache rework","summary":"Reworking the shared cache; fixed eviction","event_indices":[0]}]`,
	}}

	results, err := correlator.Cluster(t.Context(), inWindow, existing, llm, window, 128000)

	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, correlator.ClusterExtends, results[0].Kind)
	assert.Equal(t, int64(9), results[0].NarrativeID)
	require.Len(t, results[0].Events, 1)
	assert.Equal(t, "e1", results[0].Events[0].ExternalID)
}

func TestCluster_IncludesMidWindowOverlappingNarrativeAsContext(t *testing.T) {
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	window := correlator.TimeRange{Start: base, End: base.Add(time.Hour)}
	existing := []correlator.Narrative{{
		ID: 5, Title: "mid-overlap", Summary: "overlaps the middle of the window",
		WindowStart: base.Add(30 * time.Minute), WindowEnd: base.Add(90 * time.Minute),
		Events: []correlator.Event{
			mustEvent(t, "github", "pr-77", "PR #77 mid overlap work", base.Add(35*time.Minute)),
		},
	}}
	llm := &fakeLLM{responses: []string{"[]"}}

	_, err := correlator.Cluster(t.Context(), nil, existing, llm, window, 128000)

	require.NoError(t, err)
	require.Len(t, llm.prompts, 1)
	assert.Contains(t, llm.prompts[0], "mid-overlap")
	assert.Contains(t, llm.prompts[0], "PR #77 mid overlap work")
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
		// Third response: the two split halves each yield an adjacent
		// ClusterNew result, so mergeSplitResults makes one merge-boundary
		// same-story check. These are unrelated events, so the model says no.
		`{"same_story":false}`,
	}}

	// contextWindowTokens sized so one event alone fits comfortably but
	// both together (in one prompt) don't.
	results, err := correlator.Cluster(t.Context(), evts, nil, llm, correlator.TimeRange{
		Start: base, End: base.Add(2 * time.Hour),
	}, 1200)

	require.NoError(t, err)
	require.Len(t, llm.prompts, 3, "one Complete call per split half, plus one merge-boundary same-story check")
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

func TestCluster_ExtendsResultsSharingNarrativeIDMergeAcrossSplit(t *testing.T) {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	evts := []correlator.Event{
		mustEvent(t, "claude_code", "e1", strings.Repeat("a", 4000), base),
		mustEvent(t, "claude_code", "e2", strings.Repeat("b", 4000), base.Add(time.Hour)),
	}
	existing := []correlator.Narrative{
		{ID: 9, Title: "Ongoing", Summary: "s", WindowStart: base.Add(-time.Hour), WindowEnd: base},
	}
	llm := &fakeLLM{responses: []string{
		`[{"kind":"extends","narrative_id":9,"title":"Ongoing","summary":"s1","event_indices":[0]}]`,
		`[{"kind":"extends","narrative_id":9,"title":"Ongoing","summary":"s2","event_indices":[0]}]`,
	}}

	results, err := correlator.Cluster(t.Context(), evts, existing, llm, correlator.TimeRange{
		Start: base, End: base.Add(2 * time.Hour),
	}, 1200)

	require.NoError(t, err)
	require.Len(t, llm.prompts, 2, "extends-merge is deterministic; it must not trigger a merge-boundary same-story LLM call")
	require.Len(t, results, 1, "both halves' extends-9 results should merge into one")
	assert.Equal(t, correlator.ClusterExtends, results[0].Kind)
	assert.Equal(t, int64(9), results[0].NarrativeID)
	require.Len(t, results[0].Events, 2, "merged result should union both halves' events")
}

func TestCluster_AdjacentNewClustersMergeViaLLMWhenSameStory(t *testing.T) {
	tests := []struct {
		name            string
		sameStoryReply  string
		wantResultCount int
		wantTitle       string
		wantSummary     string
	}{
		{
			name:            "same story merges into one result",
			sameStoryReply:  `{"same_story":true,"title":"Merged story","summary":"unified summary"}`,
			wantResultCount: 1,
			wantTitle:       "Merged story",
			wantSummary:     "unified summary",
		},
		{
			name:            "distinct stories stay separate",
			sameStoryReply:  `{"same_story":false}`,
			wantResultCount: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
			evts := []correlator.Event{
				mustEvent(t, "claude_code", "e1", strings.Repeat("a", 4000), base),
				mustEvent(t, "claude_code", "e2", strings.Repeat("b", 4000), base.Add(time.Hour)),
			}
			llm := &fakeLLM{responses: []string{
				`[{"kind":"new","title":"Part 1","summary":"s1","event_indices":[0]}]`,
				`[{"kind":"new","title":"Part 2","summary":"s2","event_indices":[0]}]`,
				tt.sameStoryReply,
			}}

			results, err := correlator.Cluster(t.Context(), evts, nil, llm, correlator.TimeRange{
				Start: base, End: base.Add(2 * time.Hour),
			}, 1200)

			require.NoError(t, err)
			require.Len(t, llm.prompts, 3, "expected exactly one merge-boundary call after the two split-half calls")
			require.Len(t, results, tt.wantResultCount)

			if tt.wantResultCount == 1 {
				assert.Equal(t, tt.wantTitle, results[0].Title)
				assert.Equal(t, tt.wantSummary, results[0].Summary)
				require.Len(t, results[0].Events, 2)
			}
		})
	}
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

// -- Persist ---------------------------------------------------------------

func persistStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func seedPersistedEvent(t *testing.T, s *store.Store, extID, summary string, at time.Time) correlator.Event {
	t.Helper()
	e := events.NewEvent("claude_code", extID, at, summary)
	_, err := s.InsertEvent(e)
	require.NoError(t, err)
	return e
}

func TestPersist_NewNarrativeRoundTrips(t *testing.T) {
	s := persistStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	e1 := seedPersistedEvent(t, s, "e1", "started work", base)
	e2 := seedPersistedEvent(t, s, "e2", "more work", base.Add(time.Minute))
	llm := &fakeLLM{} // no LLM call expected for a new narrative

	cfg := config.CorrelatorConfig{TailSummarizeThresholdTokens: 1_000_000, RecentEventsKept: 20}
	got, err := correlator.Persist(t.Context(), s, llm, []correlator.ClusterResult{{
		Kind: correlator.ClusterNew, Title: "New story", Summary: "did some work",
		Events: []correlator.Event{e1, e2},
	}}, cfg)

	require.NoError(t, err)
	require.Len(t, got, 1)
	require.NotZero(t, got[0].ID)
	assert.Empty(t, llm.prompts, "new narrative needs no compaction call")

	row, err := s.GetNarrative(got[0].ID)
	require.NoError(t, err)
	assert.Equal(t, "New story", row.Title)
	assert.True(t, base.Equal(row.WindowStart))
	assert.True(t, base.Add(time.Minute).Equal(row.WindowEnd))
	ctxEvents, err := s.NarrativeEventsForContext(got[0].ID)
	require.NoError(t, err)
	assert.Len(t, ctxEvents, 2)
}

func TestPersist_ExtendUpdatesExistingNarrative(t *testing.T) {
	s := persistStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	seedPersistedEvent(t, s, "e1", "start", base)
	id, err := s.InsertNarrative(base, base.Add(time.Minute), "Story", "old summary")
	require.NoError(t, err)
	eid, err := s.EventIDByExternalID("claude_code", "e1")
	require.NoError(t, err)
	require.NoError(t, s.AddNarrativeEvents(id, []int64{eid}))

	e2 := seedPersistedEvent(t, s, "e2", "continued", base.Add(time.Hour))
	cfg := config.CorrelatorConfig{TailSummarizeThresholdTokens: 1_000_000, RecentEventsKept: 20}
	got, err := correlator.Persist(t.Context(), s, &fakeLLM{}, []correlator.ClusterResult{{
		Kind: correlator.ClusterExtends, NarrativeID: id, Title: "Story", Summary: "new cumulative summary",
		Events: []correlator.Event{e2},
	}}, cfg)

	require.NoError(t, err)
	require.Len(t, got, 1)
	row, err := s.GetNarrative(id)
	require.NoError(t, err)
	assert.True(t, base.Add(time.Hour).Equal(row.WindowEnd), "window_end advanced")
	assert.Equal(t, "new cumulative summary", row.Summary)
	ctxEvents, err := s.NarrativeEventsForContext(id)
	require.NoError(t, err)
	assert.Len(t, ctxEvents, 2, "both events now linked")
}

// TestPersist_ExtendWithOlderEventsDoesNotMoveWindowEndBackward guards spec
// step 3's "advance window_end to max(existing, this batch's latest event)
// — never backward." A batch whose events are all older than the
// narrative's current window_end (e.g. a late-arriving out-of-order event,
// or a re-clustered older window) must leave window_end untouched.
func TestPersist_ExtendWithOlderEventsDoesNotMoveWindowEndBackward(t *testing.T) {
	s := persistStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	currentEnd := base.Add(2 * time.Hour)
	id, err := s.InsertNarrative(base, currentEnd, "Story", "old summary")
	require.NoError(t, err)

	// olderE occurred well before the narrative's existing window_end.
	olderE := seedPersistedEvent(t, s, "older", "a late-arriving older event", base.Add(30*time.Minute))
	cfg := config.CorrelatorConfig{TailSummarizeThresholdTokens: 1_000_000, RecentEventsKept: 20}

	got, err := correlator.Persist(t.Context(), s, &fakeLLM{}, []correlator.ClusterResult{{
		Kind: correlator.ClusterExtends, NarrativeID: id, Title: "Story", Summary: "updated summary",
		Events: []correlator.Event{olderE},
	}}, cfg)

	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.True(t, currentEnd.Equal(got[0].WindowEnd), "returned Narrative's window_end must not move backward")

	row, err := s.GetNarrative(id)
	require.NoError(t, err)
	assert.True(t, currentEnd.Equal(row.WindowEnd), "window_end unchanged when the batch's events are all older")
}

func TestPersist_ExtendUnknownNarrativeIDErrorsLoudly(t *testing.T) {
	s := persistStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	e1 := seedPersistedEvent(t, s, "e1", "x", base)
	cfg := config.CorrelatorConfig{TailSummarizeThresholdTokens: 1_000_000, RecentEventsKept: 20}

	_, err := correlator.Persist(t.Context(), s, &fakeLLM{}, []correlator.ClusterResult{{
		Kind: correlator.ClusterExtends, NarrativeID: 999, Title: "x", Summary: "x",
		Events: []correlator.Event{e1},
	}}, cfg)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "999")
}

func TestPersist_EventNotInStoreErrorsLoudly(t *testing.T) {
	s := persistStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	// Event constructed but never inserted into the store.
	ghost := events.NewEvent("claude_code", "ghost", base, "never persisted")
	cfg := config.CorrelatorConfig{TailSummarizeThresholdTokens: 1_000_000, RecentEventsKept: 20}

	_, err := correlator.Persist(t.Context(), s, &fakeLLM{}, []correlator.ClusterResult{{
		Kind: correlator.ClusterNew, Title: "x", Summary: "x", Events: []correlator.Event{ghost},
	}}, cfg)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "ghost")
}

func TestPersist_AllOrNothingRollsBackFirstResultOnLaterFailure(t *testing.T) {
	s := persistStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	e1 := seedPersistedEvent(t, s, "e1", "ok", base)
	cfg := config.CorrelatorConfig{TailSummarizeThresholdTokens: 1_000_000, RecentEventsKept: 20}

	// First result is a valid new narrative; second references a missing id.
	_, err := correlator.Persist(t.Context(), s, &fakeLLM{}, []correlator.ClusterResult{
		{Kind: correlator.ClusterNew, Title: "First", Summary: "s", Events: []correlator.Event{e1}},
		{Kind: correlator.ClusterExtends, NarrativeID: 999, Title: "x", Summary: "x", Events: []correlator.Event{e1}},
	}, cfg)

	require.Error(t, err)
	// The first result's narrative must NOT have been persisted (rollback).
	_, gErr := s.GetNarrative(1)
	require.ErrorIs(t, gErr, store.ErrNarrativeNotFound)
}

func TestPersist_CompactsWhenHistoryExceedsThreshold(t *testing.T) {
	s := persistStore(t)
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	// Seed a narrative with several linked events; make its summary large so
	// the post-boundary history estimate exceeds a small threshold.
	id, err := s.InsertNarrative(base, base.Add(5*time.Hour), "Long", strings.Repeat("x", 4000))
	require.NoError(t, err)
	linkIDs := make([]int64, 0, 5)
	for i := range 5 {
		seedPersistedEvent(t, s, fmt.Sprintf("old-%d", i), strings.Repeat("y", 500), base.Add(time.Duration(i)*time.Hour))
		eid, err := s.EventIDByExternalID("claude_code", fmt.Sprintf("old-%d", i))
		require.NoError(t, err)
		linkIDs = append(linkIDs, eid)
	}
	require.NoError(t, s.AddNarrativeEvents(id, linkIDs))

	newE := seedPersistedEvent(t, s, "new-1", "newest", base.Add(6*time.Hour))
	llm := &fakeLLM{responses: []string{"recap: earlier work compacted"}}
	cfg := config.CorrelatorConfig{TailSummarizeThresholdTokens: 500, RecentEventsKept: 2}

	_, err = correlator.Persist(t.Context(), s, llm, []correlator.ClusterResult{{
		Kind: correlator.ClusterExtends, NarrativeID: id, Title: "Long", Summary: strings.Repeat("z", 4000),
		Events: []correlator.Event{newE},
	}}, cfg)

	require.NoError(t, err)
	require.Len(t, llm.prompts, 1, "compaction should make exactly one LLM call")

	row, err := s.GetNarrative(id)
	require.NoError(t, err)
	require.NotNil(t, row.CompactionBoundary, "boundary set after compaction")
	assert.Contains(t, row.Summary, "recap: earlier work compacted")

	// narrative_events rows are never deleted: all 6 events (5 seeded old-*
	// plus the new one) are still linked in the store even though context
	// now returns only the recent tail. NarrativeEventCount ignores the
	// compaction boundary, unlike NarrativeEventsForContext below, so it's
	// what actually proves survival rather than just a bounded context size
	// (which a destructive compaction that deleted rows down to the kept
	// tail would satisfy just as well).
	linkCount, err := s.NarrativeEventCount(id)
	require.NoError(t, err)
	assert.Equal(t, 6, linkCount, "all narrative_events links survive compaction, none deleted")

	// Separately: the assembled context (what future Cluster calls see) is
	// bounded to the post-boundary tail — recent kept + the new one.
	ctxEvents, err := s.NarrativeEventsForContext(id)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(ctxEvents), cfg.RecentEventsKept+1) // recent kept + the new one, all post-boundary
}

func TestPersist_NoCompactionBelowThreshold(t *testing.T) {
	s := persistStore(t)
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	seedPersistedEvent(t, s, "e1", "start", base)
	id, err := s.InsertNarrative(base, base.Add(time.Minute), "Small", "tiny")
	require.NoError(t, err)
	eid, _ := s.EventIDByExternalID("claude_code", "e1")
	require.NoError(t, s.AddNarrativeEvents(id, []int64{eid}))

	e2 := seedPersistedEvent(t, s, "e2", "next", base.Add(time.Hour))
	llm := &fakeLLM{}
	cfg := config.CorrelatorConfig{TailSummarizeThresholdTokens: 1_000_000, RecentEventsKept: 20}

	_, err = correlator.Persist(t.Context(), s, llm, []correlator.ClusterResult{{
		Kind: correlator.ClusterExtends, NarrativeID: id, Title: "Small", Summary: "still tiny",
		Events: []correlator.Event{e2},
	}}, cfg)

	require.NoError(t, err)
	assert.Empty(t, llm.prompts, "below threshold: no compaction call")
	row, err := s.GetNarrative(id)
	require.NoError(t, err)
	assert.Nil(t, row.CompactionBoundary)
}

func TestPersist_EmptyResultsIsNoOp(t *testing.T) {
	s := persistStore(t)
	got, err := correlator.Persist(t.Context(), s, &fakeLLM{}, nil,
		config.CorrelatorConfig{TailSummarizeThresholdTokens: 100, RecentEventsKept: 2})
	require.NoError(t, err)
	assert.Empty(t, got)
}

// TestPersist_UnknownClusterKindErrorsLoudly guards this repo's
// "never silently drop data" invariant (see CLAUDE.md): a ClusterResult
// tagged with neither ClusterNew nor ClusterExtends must fail the whole
// pass, not vanish from the touched-narratives result silently.
func TestPersist_UnknownClusterKindErrorsLoudly(t *testing.T) {
	s := persistStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	e1 := seedPersistedEvent(t, s, "e1", "x", base)
	cfg := config.CorrelatorConfig{TailSummarizeThresholdTokens: 1_000_000, RecentEventsKept: 20}

	_, err := correlator.Persist(t.Context(), s, &fakeLLM{}, []correlator.ClusterResult{{
		Kind: correlator.ClusterKind(99), Title: "x", Summary: "x", Events: []correlator.Event{e1},
	}}, cfg)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown ClusterKind")
}
