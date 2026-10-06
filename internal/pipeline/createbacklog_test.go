package pipeline_test

// createbacklog_test.go covers the create path's backlog through the pipeline: a pass
// bounded by reconciler.max_narratives_per_pass must reach the narratives behind the
// ones it handled, and must SAY how many untracked narratives still await a decision.
//
// Both halves were missing. On a real 30-day store (129 untracked narratives, a cap of
// 20) pass 1 proposed 11 and declined 9, and every later pass proposed nothing: the
// selector kept returning the 20 it had handled, and the summary rendered the starved
// pass exactly like a finished one.

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
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/pipeline"
	"github.com/jcogilvie/unjira/internal/store"
)

const pipelineWorthTracking = `{"worth_tracking":true,"summary":"Track this work",` +
	`"description":"A substantial change.","confidence":0.8,"rationale":"lasting outcome"}`

// seedUntrackedBacklog inserts n untracked narratives, oldest first, each with one
// member event.
func seedUntrackedBacklog(t *testing.T, s *store.Store, n int) []int64 {
	t.Helper()

	base := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	ids := make([]int64, 0, n)
	for i := range n {
		at := base.Add(time.Duration(i) * time.Hour)
		nid, err := s.InsertNarrative(at, at.Add(time.Hour), fmt.Sprintf("work %d", i), "summary")
		require.NoError(t, err)

		extID := fmt.Sprintf("cb:%d", i)
		_, err = s.InsertEvent(events.NewEvent("claude_code", extID, at, "did the work"))
		require.NoError(t, err)
		eid, err := s.EventIDByExternalID("claude_code", extID)
		require.NoError(t, err)
		require.NoError(t, s.LinkMembers(nid, []int64{eid}, 1))

		ids = append(ids, nid)
	}

	return ids
}

func createBacklogStore(t *testing.T) *store.Store {
	t.Helper()

	s, err := store.Open(filepath.Join(t.TempDir(), "backlog.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	return s
}

// cappedConfig has one writable tracker that untracked work goes to, so every seeded
// narrative has a destination and the cap is the only thing that limits a pass.
func cappedConfig(n int) config.Config {
	return config.Config{
		Connections: []config.Connection{{Name: "j", Kind: config.KindJira, Endpoint: "https://org.atlassian.net"}},
		Trackers: []config.Tracker{
			{Name: "work", Connection: "j", Scopes: []string{"DEVSBX"}, WritableScopes: []string{"DEVSBX"}, DefaultScope: "DEVSBX"},
		},
		DefaultTicketIn: []string{"work"},
		Reconciler:      config.ReconcilerConfig{MaxNarrativesPerPass: n, MinConfidenceToPropose: 0.5},
	}
}

func persistedNarratives(rows []store.ActionRow) []int64 {
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.NarrativeID)
	}

	return ids
}

// TestRunReconcile_CreatePassesDrainPastHandledNarratives is the starvation
// reproduction through the real entry point: a cap of 2 over 3 untracked narratives.
// Pass 1 proposes for the oldest 2 and reports 1 still waiting. Pass 2 must reach the
// third — before the fix it re-selected the first two, skipped them as already
// proposed, and the third was never examined on any pass.
//
// The backlog is counted AFTER Persist: counted before, the two creates this pass
// proposed would still read as awaiting, and pass 1 would report 3.
func TestRunReconcile_CreatePassesDrainPastHandledNarratives(t *testing.T) {
	s := createBacklogStore(t)
	ids := seedUntrackedBacklog(t, s, 3)
	client := &pipelineFakeLLM{responses: []string{pipelineWorthTracking}}

	pass1, err := pipeline.RunReconcile(context.Background(), s, nil, client, cappedConfig(2),
		pipeline.ReconcileOptions{})
	require.NoError(t, err)
	require.Equal(t, ids[:2], persistedNarratives(pass1.Persisted))
	assert.Equal(t, 1, pass1.CreatesRemaining,
		"one untracked narrative was kept from the model by the cap, and the summary must say so")

	pass2, err := pipeline.RunReconcile(context.Background(), s, nil, client, cappedConfig(2),
		pipeline.ReconcileOptions{})
	require.NoError(t, err)
	assert.Equal(t, ids[2:], persistedNarratives(pass2.Persisted),
		"the narratives pass 1 handled must yield their slots to the one behind them")
	assert.Equal(t, 0, pass2.CreatesRemaining, "the create backlog drained")
	assert.Len(t, client.prompts, 3, "one model call per narrative, none spent re-skipping")
}

// TestRunReconcile_NoCreateBacklogWhileCreatesAreDeferred: a deferred create path
// examined nothing, and has its own line saying why (writeDeferredCreates). A backlog
// count beside it would describe a population this pass deliberately did not touch.
func TestRunReconcile_NoCreateBacklogWhileCreatesAreDeferred(t *testing.T) {
	s := createBacklogStore(t)
	seedUntrackedBacklog(t, s, 3)
	client := &pipelineFakeLLM{responses: []string{pipelineWorthTracking}}

	got, err := pipeline.RunReconcile(context.Background(), s, nil, client, cappedConfig(2),
		pipeline.ReconcileOptions{UnmatchedNarratives: 4})
	require.NoError(t, err)
	assert.Equal(t, 4, got.CreatesDeferred)
	assert.Equal(t, 0, got.CreatesRemaining)
}

// TestRunReconcile_DryRunReportsTheCreateBacklogAsStored: a dry run persists no
// proposal, so the narratives it would have proposed for still await a decision in the
// store, and the count says so rather than pretending the dry run drained them.
func TestRunReconcile_DryRunReportsTheCreateBacklogAsStored(t *testing.T) {
	s := createBacklogStore(t)
	seedUntrackedBacklog(t, s, 3)
	client := &pipelineFakeLLM{responses: []string{pipelineWorthTracking}}

	got, err := pipeline.RunReconcile(context.Background(), s, nil, client, cappedConfig(2),
		pipeline.ReconcileOptions{DryRun: true})
	require.NoError(t, err)
	assert.Empty(t, got.Persisted)
	assert.Equal(t, 3, got.CreatesRemaining)
}

// TestRenderReconcileResult_SurfacesTheCreateBacklog: the operator reads stdout, so a
// create pass bounded by its cap must say so there, with what to do about it.
func TestRenderReconcileResult_SurfacesTheCreateBacklog(t *testing.T) {
	out := pipeline.RenderReconcileResult(pipeline.ReconcileRunResult{CreatesRemaining: 109})

	assert.Contains(t, out, "109 narrative(s) still awaiting a create decision")
	assert.Contains(t, strings.ToLower(out), "re-run")
}

// TestRenderReconcileResult_SilentWhenNoCreateBacklog: silent at zero, like every
// remainder line, or it trains an operator to skip it.
func TestRenderReconcileResult_SilentWhenNoCreateBacklog(t *testing.T) {
	out := pipeline.RenderReconcileResult(pipeline.ReconcileRunResult{})

	assert.NotContains(t, out, "create decision")
}
