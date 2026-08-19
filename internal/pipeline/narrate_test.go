// Package pipeline_test exercises RunNarrate against a real temp-file store
// and a fake llm.Client — the orchestration (input assembly, hydration,
// dry-run gating, stats merging) is what's under test, not Cluster's own
// clustering logic, which internal/correlator covers.
package pipeline_test

import (
	"context"
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
	"github.com/jcogilvie/unjira/internal/llm"
	"github.com/jcogilvie/unjira/internal/pipeline"
	"github.com/jcogilvie/unjira/internal/store"
)

// narrateLLM is a fake llm.Client returning canned responses in call order.
type narrateLLM struct {
	responses    []string
	prompts      []string
	usagePerCall llm.Usage
}

func (f *narrateLLM) Complete(_ context.Context, _ string, userPrompt string) (string, llm.Usage, error) {
	f.prompts = append(f.prompts, userPrompt)
	idx := len(f.prompts) - 1
	if idx >= len(f.responses) {
		idx = len(f.responses) - 1
	}

	return f.responses[idx], f.usagePerCall, nil
}

func narrateStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	return s
}

func seedNarrateEvent(t *testing.T, s *store.Store, extID, summary string, at time.Time) {
	t.Helper()
	_, err := s.InsertEvent(events.NewEvent("claude_code", extID, at, summary))
	require.NoError(t, err)
}

func narrateConfig() config.Config {
	return config.Config{
		LLM:        config.LLMConfig{Model: "test-model", ContextWindowTokens: 128000},
		Correlator: config.CorrelatorConfig{TailSummarizeThresholdTokens: 1_000_000, RecentEventsKept: 20},
	}
}

func TestRunNarrate_PersistsNewNarrative(t *testing.T) {
	s := narrateStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	seedNarrateEvent(t, s, "e1", "started the work", base)
	seedNarrateEvent(t, s, "e2", "finished the work", base.Add(time.Minute))

	client := &narrateLLM{
		responses:    []string{`[{"kind":"new","title":"Did work","summary":"start to finish","event_indices":[0,1]}]`},
		usagePerCall: llm.Usage{PromptTokens: 120, CompletionTokens: 30},
	}
	window := correlator.TimeRange{Start: base, End: base.Add(time.Hour)}

	got, err := pipeline.RunNarrate(t.Context(), s, client, narrateConfig(), window, pipeline.NarrateOptions{})

	require.NoError(t, err)
	assert.Equal(t, 2, got.UnlinkedEvents)
	assert.Zero(t, got.ContextNarratives)
	require.Len(t, got.Narratives, 1)
	assert.Equal(t, correlator.ClusterNew, got.Narratives[0].Kind)
	assert.Equal(t, "Did work", got.Narratives[0].Title)
	require.NotZero(t, got.Narratives[0].ID, "a persisted narrative has a real id")
	assert.Len(t, got.Narratives[0].Events, 2, "member events justify the grouping")
	assert.Equal(t, int64(120), got.Stats.PromptTokens)

	// Persisted for real: the events are now linked, so a second pass sees none.
	linked, err := s.NarrativeEventCount(got.Narratives[0].ID)
	require.NoError(t, err)
	assert.Equal(t, 2, linked)
}

func TestRunNarrate_DryRunPersistsNothingButReportsWhatItWould(t *testing.T) {
	s := narrateStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	seedNarrateEvent(t, s, "e1", "some work", base)

	client := &narrateLLM{
		responses: []string{`[{"kind":"new","title":"Work","summary":"s","event_indices":[0]}]`},
	}
	window := correlator.TimeRange{Start: base, End: base.Add(time.Hour)}

	got, err := pipeline.RunNarrate(t.Context(), s, client, narrateConfig(), window,
		pipeline.NarrateOptions{DryRun: true})

	require.NoError(t, err)
	require.Len(t, client.prompts, 1, "dry run still makes the real LLM call")
	require.Len(t, got.Narratives, 1, "and still reports what it would have written")
	assert.Zero(t, got.Narratives[0].ID, "but nothing was persisted, so there is no id")

	// The events must still be unlinked afterward.
	remaining, err := s.UnlinkedEventsInRange(base, base.Add(time.Hour))
	require.NoError(t, err)
	assert.Len(t, remaining, 1, "dry run must not consume candidates")
}

func TestRunNarrate_NoCandidatesMakesNoLLMCall(t *testing.T) {
	s := narrateStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	client := &narrateLLM{} // would panic on an empty responses slice if called
	window := correlator.TimeRange{Start: base, End: base.Add(time.Hour)}

	got, err := pipeline.RunNarrate(t.Context(), s, client, narrateConfig(), window, pipeline.NarrateOptions{})

	require.NoError(t, err, "an empty window is a normal outcome, not a failure")
	assert.Empty(t, client.prompts, "no candidates means no spend")
	assert.Empty(t, got.Narratives)
	assert.Zero(t, got.Stats.Calls)
}

func TestRunNarrate_IrreducibleUnitErrorPropagates(t *testing.T) {
	// A single event too large for the context budget cannot be split further.
	// Cluster errors naming the window and the event; RunNarrate must surface
	// that rather than swallowing it — silently narrating nothing would drop
	// real work.
	s := narrateStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	seedNarrateEvent(t, s, "huge", strings.Repeat("x", 8000), base)

	client := &narrateLLM{responses: []string{"[]"}}
	cfg := narrateConfig()
	cfg.LLM.ContextWindowTokens = 50 // smaller than one event's prompt
	window := correlator.TimeRange{Start: base, End: base.Add(time.Hour)}

	_, err := pipeline.RunNarrate(t.Context(), s, client, cfg, window, pipeline.NarrateOptions{})

	require.Error(t, err)
	assert.Empty(t, client.prompts, "an unsplittable window must not spend a call")
}

func TestRunNarrate_HydratesOverlappingNarrativeEventsAsContext(t *testing.T) {
	s := narrateStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	// An existing narrative whose window ends where ours begins, with a linked event.
	seedNarrateEvent(t, s, "old", "PR #412 add cache layer", base.Add(-time.Hour))
	oldID, err := s.EventIDByExternalID("claude_code", "old")
	require.NoError(t, err)
	nid, err := s.InsertNarrative(base.Add(-2*time.Hour), base, "Cache rework", "reworking the cache")
	require.NoError(t, err)
	require.NoError(t, s.AddNarrativeEvents(nid, []int64{oldID}))

	seedNarrateEvent(t, s, "new", "fixed the cache eviction bug", base.Add(time.Minute))

	client := &narrateLLM{
		responses: []string{`[{"kind":"new","title":"T","summary":"s","event_indices":[0]}]`},
	}
	window := correlator.TimeRange{Start: base, End: base.Add(time.Hour)}

	got, err := pipeline.RunNarrate(t.Context(), s, client, narrateConfig(), window, pipeline.NarrateOptions{})

	require.NoError(t, err)
	assert.Equal(t, 1, got.ContextNarratives)
	require.Len(t, client.prompts, 1)
	// The Path-B round trip: the existing narrative's raw event, hydrated from
	// the store, reached the prompt as context.
	assert.Contains(t, client.prompts[0], "PR #412 add cache layer",
		"hydrated context events must reach the prompt, not just the summary")
	assert.Contains(t, client.prompts[0], "reworking the cache")
}

// TestRunNarrate_EventsFoldedCountsOnlyThisPassNotLifetimeTotal proves
// Compaction.EventsFolded reports what THIS pass's compaction folded, not
// the narrative's cumulative fold count across every compaction it has ever
// had. That distinction only shows up on a *second* compaction of the same
// narrative: narrative_events rows are never deleted (see
// store.NarrativeEventsForContext), so a naive
// "total linked minus currently visible" subtraction would double-count the
// events an earlier pass already folded.
//
// Three passes force this: pass 1 creates a narrative from 2 events (no
// compaction — Persist never compacts a brand-new ClusterNew). Pass 2
// extends it with 3 more events and a low threshold forces a compaction,
// folding 3 of the 5 total (keeping the newest 2). Pass 3 extends again with
// 2 more events and forces a second compaction, folding 2 of the 4
// then-visible events (the 2 kept from pass 2, plus the 2 new ones).
//
// If EventsFolded were computed as lifetime-linked-minus-visible, pass 3's
// report would wrongly include the 3 events pass 2 already folded (5 instead
// of the correct 2), since narrative_events still carries all 7 rows ever
// linked to this narrative by that point.
func TestRunNarrate_EventsFoldedCountsOnlyThisPassNotLifetimeTotal(t *testing.T) {
	s := narrateStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	cfg := narrateConfig()
	cfg.Correlator.TailSummarizeThresholdTokens = 10 // trivially low: any tail over RecentEventsKept trips it
	cfg.Correlator.RecentEventsKept = 2

	// Pass 1: create the narrative from e1, e2. No compaction — Persist never
	// compacts a ClusterNew.
	seedNarrateEvent(t, s, "e1", "started the work", base)
	seedNarrateEvent(t, s, "e2", "kept going", base.Add(time.Minute))
	client := &narrateLLM{
		responses: []string{`[{"kind":"new","title":"T","summary":"s0","event_indices":[0,1]}]`},
	}
	window1 := correlator.TimeRange{Start: base, End: base.Add(2 * time.Minute)}

	got1, err := pipeline.RunNarrate(t.Context(), s, client, cfg, window1, pipeline.NarrateOptions{})
	require.NoError(t, err)
	require.Len(t, got1.Narratives, 1)
	narrativeID := got1.Narratives[0].ID
	require.NotZero(t, narrativeID)
	assert.Empty(t, got1.Compactions, "a brand-new narrative is never compacted the pass it's created")

	// Pass 2: extend with e3, e4, e5. Post-boundary history is now
	// e1..e5 (5 events) against RecentEventsKept=2, so this must compact,
	// folding e1, e2, e3 (3 events) and leaving e4, e5 visible.
	seedNarrateEvent(t, s, "e3", "third thing", base.Add(2*time.Minute))
	seedNarrateEvent(t, s, "e4", "fourth thing", base.Add(3*time.Minute))
	seedNarrateEvent(t, s, "e5", "fifth thing", base.Add(4*time.Minute))
	client.responses = append(client.responses,
		fmt.Sprintf(`[{"kind":"extends","narrative_id":%d,"title":"T","summary":"s1","event_indices":[0,1,2]}]`, narrativeID),
		"recap of e1-e3",
	)
	window2 := correlator.TimeRange{Start: base.Add(time.Minute), End: base.Add(5 * time.Minute)}

	got2, err := pipeline.RunNarrate(t.Context(), s, client, cfg, window2, pipeline.NarrateOptions{})
	require.NoError(t, err)
	require.Len(t, got2.Compactions, 1, "the post-boundary history (5 events) exceeds RecentEventsKept (2)")
	assert.Equal(t, narrativeID, got2.Compactions[0].NarrativeID)
	assert.Equal(t, 3, got2.Compactions[0].EventsFolded, "folds e1, e2, e3; keeps e4, e5")

	visibleAfterPass2, err := s.NarrativeEventsForContext(narrativeID)
	require.NoError(t, err)
	assert.Len(t, visibleAfterPass2, 2, "e4 and e5 remain visible after the first compaction")

	// Pass 3: extend with e6, e7. Post-boundary history is now e4..e7 (4
	// events, since e1-e3 are already folded and not part of the "visible"
	// tail) against RecentEventsKept=2, so this compacts again, folding e4
	// and e5 (2 events) — NOT the 3 events pass 2 already folded plus these 2.
	seedNarrateEvent(t, s, "e6", "sixth thing", base.Add(5*time.Minute))
	seedNarrateEvent(t, s, "e7", "seventh thing", base.Add(6*time.Minute))
	client.responses = append(client.responses,
		fmt.Sprintf(`[{"kind":"extends","narrative_id":%d,"title":"T","summary":"s2","event_indices":[0,1]}]`, narrativeID),
		"recap of e4-e5",
	)
	window3 := correlator.TimeRange{Start: base.Add(4 * time.Minute), End: base.Add(7 * time.Minute)}

	got3, err := pipeline.RunNarrate(t.Context(), s, client, cfg, window3, pipeline.NarrateOptions{})
	require.NoError(t, err)
	require.Len(t, got3.Compactions, 1)
	assert.Equal(t, narrativeID, got3.Compactions[0].NarrativeID)
	assert.Equal(t, 2, got3.Compactions[0].EventsFolded,
		"this pass folded only e4 and e5 (2 events), not the lifetime total of 5 "+
			"(the 3 pass 2 already folded plus these 2) — narrative_events keeps every row ever linked, "+
			"so a lifetime linked-minus-visible subtraction would wrongly report 5")

	// The total linked count keeps growing even though "visible" shrinks —
	// this is what would make the naive subtraction overcount.
	totalLinked, err := s.NarrativeEventCount(narrativeID)
	require.NoError(t, err)
	assert.Equal(t, 7, totalLinked, "e1 through e7 are all still linked; compaction never deletes rows")
}
