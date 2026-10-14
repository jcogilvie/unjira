package pipeline_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/llm"
	"github.com/jcogilvie/unjira/internal/pipeline"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// stageSpy records the stage of every call before delegating to a fake.
type stageSpy struct {
	inner  llm.Client
	stages []string
}

func (c *stageSpy) Complete(ctx context.Context, systemPrompt, userPrompt string) (string, llm.Usage, error) {
	c.stages = append(c.stages, llm.StageOf(ctx))

	return c.inner.Complete(ctx, systemPrompt, userPrompt)
}

// TestRunStages_LabelContext: each Run* entry labels its calls with its own stage,
// so the llm_calls ledger can say which part of a pass spent the tokens.
func TestRunStages_LabelContext(t *testing.T) {
	stagesOf := func(t *testing.T, spy *stageSpy) map[string]bool {
		t.Helper()
		require.NotEmpty(t, spy.stages, "the run must make at least one call")
		got := map[string]bool{}
		for _, s := range spy.stages {
			got[s] = true
		}

		return got
	}

	t.Run("narrate", func(t *testing.T) {
		s := narrateStore(t)
		base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
		seedNarrateEvent(t, s, "e1", "started the work", base)
		spy := &stageSpy{inner: &narrateLLM{responses: []string{
			`[{"kind":"new","title":"W","summary":"s","confidence":0.9,"event_indices":[0]}]`,
		}}}

		_, err := pipeline.RunNarrate(t.Context(), s, spy, narrateConfig(),
			correlator.TimeRange{Start: base, End: base.Add(time.Hour)}, pipeline.NarrateOptions{})

		require.NoError(t, err)
		assert.Equal(t, map[string]bool{"cluster": true}, stagesOf(t, spy))
	})

	t.Run("match", func(t *testing.T) {
		s := matchPipelineStore(t)
		e := events.NewEvent("claude_code", "s1", time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC), "summary")
		e.Artifacts["git_branch"] = "feature/PROJ-1"
		e.Artifacts["ticket_keys"] = []any{"SUMO-2"}
		seedMatchNarrative(t, s, "t", "s", e)
		tracker := &pipelineFakeTracker{issues: map[string]tasktracker.Issue{
			"PROJ-1": {Key: "PROJ-1", Summary: "a", StatusName: "To Do"},
			"SUMO-2": {Key: "SUMO-2", Summary: "b", StatusName: "To Do"},
		}}
		spy := &stageSpy{inner: &pipelineFakeLLM{responses: []string{
			`[{"issue_key":"PROJ-1","role":"primary","confidence":0.9,"rationale":"r"}]`,
		}}}
		cfg := config.Config{Match: config.MatchConfig{MaxCandidatesPerNarrative: 10, ConfidenceFloor: 0.5}}

		_, err := pipeline.RunMatch(t.Context(), s, tracker, spy, cfg, pipeline.MatchOptions{})

		require.NoError(t, err)
		assert.Equal(t, map[string]bool{"match": true}, stagesOf(t, spy))
	})

	t.Run("reconcile", func(t *testing.T) {
		s, err := store.Open(filepath.Join(t.TempDir(), "r.db"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })
		seedUntrackedForPipeline(t, s)
		spy := &stageSpy{inner: &pipelineFakeLLM{responses: []string{`{"worth_tracking":true,` +
			`"summary":"x","description":"y","confidence":0.8,"rationale":"r"}`}}}
		cfg := withDefaultTicketIn(config.Config{Reconciler: config.ReconcilerConfig{
			MaxNarrativesPerPass: 10, MinConfidenceToPropose: 0.5,
		}})

		_, err = pipeline.RunReconcile(t.Context(), s, nil, spy, cfg, pipeline.ReconcileOptions{})

		require.NoError(t, err)
		assert.Equal(t, map[string]bool{"draft": true}, stagesOf(t, spy))
	})

	t.Run("learn", func(t *testing.T) {
		s, cfg, _ := learnFixture(t)
		spy := &stageSpy{inner: &learnLLM{response: oneCandidate}}

		_, err := pipeline.RunLearn(t.Context(), s, spy, cfg, pipeline.LearnOptions{})

		require.NoError(t, err)
		assert.Equal(t, map[string]bool{"distil": true}, stagesOf(t, spy))
	})
}
