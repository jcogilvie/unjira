package pipeline_test

// reconcile_test.go exercises RunReconcile against a real temp-file store and
// the same fake llm.Client/tasktracker.TaskReader used by match_test.go — the
// orchestration (validating config, calling reconciler.Reconcile, and
// Persist-ing unless DryRun) is what's under test here, not Reconcile's own
// drafting logic, which internal/reconciler covers.
//
// RenderReconcileResult's tests are pure and need neither a store nor an LLM.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/pipeline"
	"github.com/jcogilvie/unjira/internal/reconciler"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// seedReconcilableNarrative inserts a narrative with one non-self-authored
// event and a `primary` narrative_issues link to issueKey — the minimum
// shape reconciler.Reconcile will actually draft against (an actionable
// link plus a non-empty delta). seedMatchNarrative (match_test.go) does not
// attach any narrative_issues row, so it is not reusable here as-is.
func seedReconcilableNarrative(t *testing.T, s *store.Store, issueKey string) int64 {
	t.Helper()

	e := events.NewEvent("claude_code", "e1",
		time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC), "did some work")

	id := seedMatchNarrative(t, s, "did the work", "summary of the work", e)

	require.NoError(t, s.WithTx(func(tx *store.Tx) error {
		return tx.AddNarrativeIssues(id, []store.NarrativeIssue{{
			IssueKey:   issueKey,
			Role:       correlator.RolePrimary,
			Provenance: "branch",
			Confidence: 0.9,
			Connection: "dev",
		}})
	}))

	return id
}

func TestRunReconcileValidatesConfigBeforeAnyWork(t *testing.T) {
	s := matchPipelineStore(t)
	tracker := &pipelineFakeTracker{}
	llmFake := &pipelineFakeLLM{}

	cfg := config.Config{Reconciler: config.ReconcilerConfig{MinConfidenceToPropose: 1.5}}

	_, err := pipeline.RunReconcile(t.Context(), s, tracker, llmFake, cfg, pipeline.ReconcileOptions{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "min_confidence_to_propose",
		"a bad config must fail before any tracker or LLM call")
	assert.Empty(t, tracker.getCalls)
	assert.Empty(t, llmFake.prompts)
}

func TestRunReconcileDryRunPersistsNothing(t *testing.T) {
	s := matchPipelineStore(t)
	nid := seedReconcilableNarrative(t, s, "PROJ-1")

	tracker := &pipelineFakeTracker{issues: map[string]tasktracker.Issue{
		"PROJ-1": {Key: "PROJ-1", Summary: "the ticket", StatusName: "In Progress"},
	}}
	llmFake := &pipelineFakeLLM{responses: []string{
		`[{"issue_key":"PROJ-1","type":"comment","body":"the work landed","confidence":0.9,"rationale":"delta shows it"}]`,
	}}

	cfg := config.Config{Reconciler: config.ReconcilerConfig{
		MaxNarrativesPerPass: 10, MinConfidenceToPropose: 0.5,
	}}

	result, err := pipeline.RunReconcile(t.Context(), s, tracker, llmFake, cfg,
		pipeline.ReconcileOptions{DryRun: true})
	require.NoError(t, err)

	assert.True(t, result.DryRun)
	assert.NotEmpty(t, result.Results, "the pass still ran, including the real LLM call")
	assert.NotEmpty(t, llmFake.prompts, "dry run still makes the real LLM call")

	got, err := s.ActionsForNarrative(nid)
	require.NoError(t, err)
	assert.Empty(t, got, "--dry-run skips Persist")
}

func TestRunReconcilePersistsWhenNotDryRun(t *testing.T) {
	s := matchPipelineStore(t)
	nid := seedReconcilableNarrative(t, s, "PROJ-1")

	tracker := &pipelineFakeTracker{issues: map[string]tasktracker.Issue{
		"PROJ-1": {Key: "PROJ-1", Summary: "the ticket", StatusName: "In Progress"},
	}}
	llmFake := &pipelineFakeLLM{responses: []string{
		`[{"issue_key":"PROJ-1","type":"comment","body":"the work landed","confidence":0.9,"rationale":"delta shows it"}]`,
	}}

	cfg := config.Config{Reconciler: config.ReconcilerConfig{
		MaxNarrativesPerPass: 10, MinConfidenceToPropose: 0.5,
	}}

	result, err := pipeline.RunReconcile(t.Context(), s, tracker, llmFake, cfg, pipeline.ReconcileOptions{})
	require.NoError(t, err)
	assert.False(t, result.DryRun)
	require.Len(t, result.Results, 1)
	require.Len(t, result.Results[0].Proposed, 1)

	got, err := s.ActionsForNarrative(nid)
	require.NoError(t, err)
	require.Len(t, got, 1, "a non-dry-run pass persists what it drafted")
	assert.Equal(t, "proposed", got[0].Status)
}

func TestRenderReconcileResultNamesWhatWasSuppressedAndWhy(t *testing.T) {
	out := pipeline.RenderReconcileResult(pipeline.ReconcileRunResult{
		Results: []reconciler.ReconcileResult{
			{NarrativeID: 1, Proposed: []reconciler.ProposedAction{
				{
					Type: reconciler.ActionComment, IssueKey: "PROJ-1",
					Body: "the work landed", Confidence: 0.8,
				},
			}},
			{NarrativeID: 2, SkippedNoDelta: true},
			{NarrativeID: 3, Unverified: []string{"PROJ-404"}},
			{NarrativeID: 4, Suppressed: []string{"PROJ-9: another narrative already has an open proposal"}},
		},
	})

	assert.Contains(t, out, "PROJ-1")
	assert.Contains(t, out, "0.8")
	assert.Contains(t, out, "no new events", "an unchanged narrative must be explained, not omitted")
	assert.Contains(t, out, "PROJ-404")
	assert.Contains(t, out, "already has an open proposal",
		"'nothing proposed' is otherwise indistinguishable from 'nothing considered'")
}

func TestRenderReconcileResultShowsEveryOutcomeKind(t *testing.T) {
	out := pipeline.RenderReconcileResult(pipeline.ReconcileRunResult{
		Results: []reconciler.ReconcileResult{
			{NarrativeID: 5, Proposed: []reconciler.ProposedAction{
				{
					Type: reconciler.ActionTransition, IssueKey: "PROJ-2",
					TargetStatus: tasktracker.StatusDone, Confidence: 0.95,
				},
			}},
			{NarrativeID: 6, Proposed: []reconciler.ProposedAction{
				{
					Type: reconciler.ActionComment, IssueKey: "PROJ-3",
					Body: "weak signal", Confidence: 0.1,
				},
			}, LowConfidence: []string{"comment PROJ-3 at confidence 0.10 is below reconciler.min_confidence_to_propose 0.50"}},
		},
	})

	assert.Contains(t, out, "PROJ-2")
	assert.Contains(t, out, "done", "a transition target must render")
	assert.Contains(t, out, "PROJ-3")
	assert.Contains(t, out, "min_confidence_to_propose")
}

func TestRenderReconcileResult_EmptyPassSaysSoInsteadOfPrintingNothing(t *testing.T) {
	out := pipeline.RenderReconcileResult(pipeline.ReconcileRunResult{})

	assert.Contains(t, out, "no linked narratives to reconcile")
}
