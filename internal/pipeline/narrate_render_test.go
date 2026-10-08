package pipeline_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/pipeline"
)

func TestRenderNarrateResult(t *testing.T) {
	base := time.Date(2026, 8, 14, 9, 12, 0, 0, time.UTC)
	window := correlator.TimeRange{Start: base, End: base.Add(8 * time.Hour)}

	full := pipeline.NarrateResult{
		Window:            window,
		UnlinkedEvents:    5,
		ContextNarratives: 2,
		Stats: correlator.Stats{
			Calls: 4, Splits: 1, MergeChecks: 1,
			PromptTokens: 38412, CompletionTokens: 1205, EstimatedTokens: 41000,
		},
		Narratives: []pipeline.NarratedNarrative{
			{
				Kind: correlator.ClusterNew, ID: 14,
				WindowStart: base, WindowEnd: base.Add(2 * time.Hour),
				Title:   "Fix flaky correlator tests",
				Summary: "Chased an intermittent failure in the split path.",
				Events: []correlator.Event{
					events.NewEvent("claude_code", "e1", base, "unjira: 3 user messages."),
				},
			},
			{
				Kind: correlator.ClusterExtends, ID: 9,
				PriorWindowEnd: base.Add(-2 * time.Hour),
				WindowStart:    base.Add(-6 * time.Hour), WindowEnd: base.Add(time.Hour),
				Title: "Cache rework", Summary: "Continued the cache work.",
			},
		},
		Compactions: []pipeline.Compaction{
			{NarrativeID: 9, EventsFolded: 12, Boundary: base.Add(-4 * time.Hour)},
		},
	}

	t.Run("full pass shows stats, narratives, and member events", func(t *testing.T) {
		out := pipeline.RenderNarrateResult(full)

		assert.Contains(t, out, "5 unlinked candidate")
		assert.Contains(t, out, "2 narrative")
		assert.Contains(t, out, "4 call")
		assert.Contains(t, out, "1 split")
		assert.Contains(t, out, "38412")
		assert.Contains(t, out, "41000", "the estimate is shown next to the actual")
		assert.Contains(t, out, "[NEW #14]")
		assert.Contains(t, out, "Fix flaky correlator tests")
		assert.Contains(t, out, "unjira: 3 user messages.", "member events make the grouping judgeable")
		assert.Contains(t, out, "[EXTENDS #9]")
		assert.Contains(t, out, "folded 12 event")
	})

	t.Run("dry run marks ids as unpersisted and says so", func(t *testing.T) {
		dry := pipeline.NarrateResult{
			Window:            full.Window,
			UnlinkedEvents:    full.UnlinkedEvents,
			ContextNarratives: full.ContextNarratives,
			DryRun:            true,
			Stats:             full.Stats,
			Narratives: []pipeline.NarratedNarrative{{
				Kind: correlator.ClusterNew, ID: 0,
				WindowStart: base, WindowEnd: base.Add(time.Hour),
				Title: "Would be written", Summary: "s",
			}},
		}

		out := pipeline.RenderNarrateResult(dry)

		assert.Contains(t, out, "nothing persisted")
		assert.Contains(t, out, "[NEW #-]", "a dry run has no id to show")
		assert.NotContains(t, out, "#0]", "0 must never be rendered as an id")
	})

	t.Run("empty pass says so instead of printing nothing", func(t *testing.T) {
		out := pipeline.RenderNarrateResult(pipeline.NarrateResult{Window: window})

		assert.Contains(t, out, "no narratives produced")
	})

	t.Run("excluded context narratives are reported so the bound is tunable", func(t *testing.T) {
		bounded := full
		bounded.ExcludedContextNarratives = 3

		out := pipeline.RenderNarrateResult(bounded)

		assert.Contains(t, out, "3", "the exclusion count reaches stdout")
		assert.Contains(t, out, "max_context_narratives",
			"names the knob an operator would raise, matching the truncation line's own convention")
	})

	t.Run("context narratives left out to fit the prompt budget are reported", func(t *testing.T) {
		fitted := full
		fitted.UnfittedContextNarratives = 4

		out := pipeline.RenderNarrateResult(fitted)

		assert.Contains(t, out, "left 4 existing narrative(s) out of at least one clustering call",
			"the count reaches stdout")
		assert.Contains(t, out, "llm.context_window_tokens", "names what bounded it")
		assert.NotContains(t, out, "raise correlator.max_context_narratives",
			"raising the cap cannot keep what the budget left out")
	})

	t.Run("events the model omitted and a re-ask recovered are reported", func(t *testing.T) {
		reasked := full
		reasked.Stats.OmittedEvents = 5
		reasked.Stats.RecoveredEvents = 5

		out := pipeline.RenderNarrateResult(reasked)

		assert.Contains(t, out, "re-asked 5 event(s) the model left in no cluster; recovered 5",
			"a pass that needed a second call to account for everything says so")
	})

	t.Run("omissions recovered over several rounds say how many", func(t *testing.T) {
		reasked := full
		reasked.Stats.OmittedEvents = 5
		reasked.Stats.RecoveredEvents = 5
		reasked.Stats.OmissionReasks = 3

		out := pipeline.RenderNarrateResult(reasked)

		assert.Contains(t, out, "re-asked 5 event(s) the model left in no cluster; recovered 5 over 3 rounds")

		one := full
		one.Stats.OmittedEvents = 5
		one.Stats.RecoveredEvents = 5
		one.Stats.OmissionReasks = 1
		assert.NotContains(t, pipeline.RenderNarrateResult(one), "rounds", "one round is the line as it always read")
	})

	t.Run("a refused dispute answer re-asked is reported apart from batching", func(t *testing.T) {
		disputed := full
		disputed.Stats.DisputedEvents = 1
		disputed.Stats.DisputeCalls = 2
		disputed.Stats.DisputeReasks = 1
		disputed.Stats.Disputes = make([]correlator.DisputeResolution, 1)

		out := pipeline.RenderNarrateResult(disputed)

		assert.Contains(t, out, "resolved 1 by one re-ask, after re-asking 1 refused answer(s)")
		assert.NotContains(t, out, "batched", "a second call that re-asked a refusal is not a batch")
	})

	t.Run("a dispute pass batched to fit the window says how many calls it took", func(t *testing.T) {
		disputed := full
		disputed.Stats.DisputedEvents = 3
		disputed.Stats.DisputeCalls = 2
		disputed.Stats.Disputes = make([]correlator.DisputeResolution, 3)

		out := pipeline.RenderNarrateResult(disputed)

		assert.Contains(t, out, "resolved 3 by 2 re-ask calls, batched to fit the context window")
		assert.NotContains(t, out, "by one re-ask")
	})

	t.Run("clusters joined by pull-request identity are reported", func(t *testing.T) {
		joined := full
		joined.Stats.PRIdentityJoins = 2

		out := pipeline.RenderNarrateResult(joined)

		assert.Contains(t, out, "identity 2 cluster(s) joined another cluster holding the same pull request's work")
	})

	t.Run("a pull request two stored narratives hold in one pass is reported, not joined", func(t *testing.T) {
		conflict := full
		conflict.Stats.PRIdentityConflicts = []correlator.PRIdentityConflict{{
			PullRequests: []string{"github.com/o/r#1"},
			Clusters:     []string{"narrative 9", `new "New bit"`, "narrative 12"},
		}}

		out := pipeline.RenderNarrateResult(conflict)

		assert.Contains(t, out, "identity 1 pull-request group(s) span several stored narratives — left apart:")
		assert.Contains(t, out, `github.com/o/r#1: narrative 9, new "New bit", narrative 12`)
	})

	t.Run("no pull-request identity join prints nothing extra", func(t *testing.T) {
		out := pipeline.RenderNarrateResult(full)

		assert.NotContains(t, out, "joined another cluster", "silent at zero, like the other lines")
		assert.NotContains(t, out, "span several stored narratives")
	})

	t.Run("zero omitted events prints nothing extra", func(t *testing.T) {
		out := pipeline.RenderNarrateResult(full)

		assert.NotContains(t, out, "re-asked", "silent at zero, like the other exclusion lines")
	})

	t.Run("zero excluded context narratives prints nothing extra", func(t *testing.T) {
		out := pipeline.RenderNarrateResult(full)

		assert.NotContains(t, out, "max_context_narratives",
			"silent at zero: a line on every ordinary pass trains an operator to skip it")
		assert.NotContains(t, out, "clustering call", "nor the prompt budget's line")
	})
}
