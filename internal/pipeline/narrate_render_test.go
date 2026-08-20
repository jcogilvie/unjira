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
}
