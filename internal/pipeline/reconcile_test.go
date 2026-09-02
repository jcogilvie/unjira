package pipeline_test

// reconcile_test.go exercises RunReconcile against a real temp-file store and
// the same fake llm.Client/tasktracker.TaskReader used by match_test.go — the
// orchestration (validating config, calling reconciler.Reconcile, and
// Persist-ing unless DryRun) is what's under test here, not Reconcile's own
// drafting logic, which internal/reconciler covers.
//
// RenderReconcileResult's tests are pure and need neither a store nor an LLM.

import (
	"os"
	"path/filepath"
	"strings"
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

// TestRunReconcile_LoadsReconcilerRulesAndAppendsThemToTheDraftingSystemPrompt
// is the fix's end-to-end proof: a scope:reconciler rule file on disk must
// reach the actual system prompt RunReconcile's LLM call sends — not merely
// survive rules.Load/ForScope, which internal/rules already tested before
// this fix existed.
// TestRunReconcile_FlagsATransitionProposedWithNoCollectedStatusHistory is
// task #174's per-narrative visibility: suppressStaleTransitions degrades
// to proposing when it has no collected status history for an issue
// (internal/reconciler.recency.go's HaveLastStatus false branch), and that
// degradation used to be silent — a reviewer could not tell "the guard
// checked and cleared this" from "the guard never ran" just by looking at
// Results[0].Proposed. If this regresses to empty, that distinction is gone
// again and every unguarded transition looks identical to a checked one in
// the CLI's own output.
func TestRunReconcile_FlagsATransitionProposedWithNoCollectedStatusHistory(t *testing.T) {
	s := matchPipelineStore(t)
	seedReconcilableNarrative(t, s, "PROJ-1")

	tracker := &pipelineFakeTracker{
		issues: map[string]tasktracker.Issue{
			"PROJ-1": {Key: "PROJ-1", Summary: "the ticket", StatusName: "In Progress"},
		},
		transitions: map[string][]tasktracker.Transition{
			"PROJ-1": {{ToStatus: "In Review", ToCategory: tasktracker.StatusInProgress}},
		},
	}
	llmFake := &pipelineFakeLLM{responses: []string{
		`[{"issue_key":"PROJ-1","type":"transition","target_status":"In Review",` +
			`"confidence":0.9,"rationale":"a PR is open"}]`,
	}}

	cfg := config.Config{Reconciler: config.ReconcilerConfig{
		MaxNarrativesPerPass: 10, MinConfidenceToPropose: 0.5,
	}}

	result, err := pipeline.RunReconcile(t.Context(), s, tracker, llmFake, cfg, pipeline.ReconcileOptions{})
	require.NoError(t, err)

	require.Len(t, result.Results, 1)
	require.Len(t, result.Results[0].Proposed, 1,
		"no collected history means the guard degrades to proposing, not to silence")

	require.Len(t, result.Unguarded, 1,
		"and that degradation must be reported, or it is indistinguishable from a checked proposal")
	assert.Equal(t, "PROJ-1", result.Unguarded[0].IssueKey)
	assert.Contains(t, result.Unguarded[0].Reason(), "In Review")
}

func TestRunReconcile_LoadsReconcilerRulesAndAppendsThemToTheDraftingSystemPrompt(t *testing.T) {
	s := matchPipelineStore(t)
	seedReconcilableNarrative(t, s, "PROJ-1")

	tracker := &pipelineFakeTracker{issues: map[string]tasktracker.Issue{
		"PROJ-1": {Key: "PROJ-1", Summary: "the ticket", StatusName: "In Progress"},
	}}
	llmFake := &pipelineFakeLLM{responses: []string{
		`[{"issue_key":"PROJ-1","type":"comment","body":"the work landed","confidence":0.9,"rationale":"delta shows it"}]`,
	}}

	rulesDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(rulesDir, "sentinel.md"), []byte(`---
scope: reconciler
confidence: high
learned: 2026-07-16
source: test
---

Sentinel reconcile-time rule body.
`), 0o600))

	cfg := config.Config{
		Reconciler: config.ReconcilerConfig{MaxNarrativesPerPass: 10, MinConfidenceToPropose: 0.5},
		Rules:      config.RulesConfig{Dir: rulesDir},
	}

	_, err := pipeline.RunReconcile(t.Context(), s, tracker, llmFake, cfg, pipeline.ReconcileOptions{})
	require.NoError(t, err)

	require.NotEmpty(t, llmFake.systemPrompts)
	assert.Contains(t, llmFake.systemPrompts[0], "Sentinel reconcile-time rule body.")
	assert.Contains(t, llmFake.systemPrompts[0], "sentinel")
}

// TestRunReconcile_CorrelatorScopedRuleNeverReachesTheDraftingPrompt proves
// scope filtering is real rather than "load everything and hope": a
// scope:correlator rule living in the same rules/ directory as a
// scope:reconciler rule must never reach RunReconcile's drafting prompt.
func TestRunReconcile_CorrelatorScopedRuleNeverReachesTheDraftingPrompt(t *testing.T) {
	s := matchPipelineStore(t)
	seedReconcilableNarrative(t, s, "PROJ-1")

	tracker := &pipelineFakeTracker{issues: map[string]tasktracker.Issue{
		"PROJ-1": {Key: "PROJ-1", Summary: "the ticket", StatusName: "In Progress"},
	}}
	llmFake := &pipelineFakeLLM{responses: []string{
		`[{"issue_key":"PROJ-1","type":"comment","body":"the work landed","confidence":0.9,"rationale":"delta shows it"}]`,
	}}

	rulesDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(rulesDir, "correlator-only.md"), []byte(`---
scope: correlator
confidence: high
learned: 2026-07-16
source: test
---

Sentinel correlator-only rule body.
`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(rulesDir, "reconciler-only.md"), []byte(`---
scope: reconciler
confidence: high
learned: 2026-07-16
source: test
---

Sentinel reconciler-only rule body.
`), 0o600))

	cfg := config.Config{
		Reconciler: config.ReconcilerConfig{MaxNarrativesPerPass: 10, MinConfidenceToPropose: 0.5},
		Rules:      config.RulesConfig{Dir: rulesDir},
	}

	_, err := pipeline.RunReconcile(t.Context(), s, tracker, llmFake, cfg, pipeline.ReconcileOptions{})
	require.NoError(t, err)

	require.NotEmpty(t, llmFake.systemPrompts)
	assert.NotContains(t, llmFake.systemPrompts[0], "Sentinel correlator-only rule body.",
		"a scope:correlator rule must never reach the reconciler's drafting prompt")
	assert.Contains(t, llmFake.systemPrompts[0], "Sentinel reconciler-only rule body.",
		"the scope:reconciler rule in the same directory must still reach it")
}

// TestRunReconcile_MissingRulesDirIsANoOpNotAnError mirrors RunMatch's and
// RunNarrate's identical guarantee (see match_test.go's
// TestRunMatch_MissingRulesDirIsANoOpNotAnError): a fresh clone or a
// deployment with no seeded rules/ must still run, cleanly, all the way
// through drafting. The byte-for-byte "no stray appended section" assertion
// for an empty rules set is pinned once, precisely, at the unit level —
// internal/reconciler's TestDraftWithNoRulesLeavesSystemPromptUnchanged —
// since draftSystemPrompt is unexported and this package intentionally has
// no export_test.go shim reaching into internal/reconciler's internals
// (unlike internal/correlator's, which internal/pipeline's own tests never
// reach into either).
func TestRunReconcile_MissingRulesDirIsANoOpNotAnError(t *testing.T) {
	s := matchPipelineStore(t)
	seedReconcilableNarrative(t, s, "PROJ-1")

	tracker := &pipelineFakeTracker{issues: map[string]tasktracker.Issue{
		"PROJ-1": {Key: "PROJ-1", Summary: "the ticket", StatusName: "In Progress"},
	}}
	llmFake := &pipelineFakeLLM{responses: []string{
		`[{"issue_key":"PROJ-1","type":"comment","body":"the work landed","confidence":0.9,"rationale":"delta shows it"}]`,
	}}

	cfg := config.Config{
		Reconciler: config.ReconcilerConfig{MaxNarrativesPerPass: 10, MinConfidenceToPropose: 0.5},
		Rules:      config.RulesConfig{Dir: filepath.Join(t.TempDir(), "does-not-exist")},
	}

	result, err := pipeline.RunReconcile(t.Context(), s, tracker, llmFake, cfg, pipeline.ReconcileOptions{})
	require.NoError(t, err)
	require.Len(t, result.Results, 1)
	require.Len(t, result.Results[0].Proposed, 1)
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

// TestRenderReconcileResult_ShowsAnUnguardedTransitionUnderItsOwnNarrative
// proves the CLI's own output — what an operator running `dev narrate` or
// `watch` actually reads — carries task #174's visibility, not just the
// data structure. Without this, ReconcileRunResult.Unguarded could be
// populated correctly and a human would still never see it: the rendered
// text is what triage/CLAUDE.md's "keep the docs true" bar and an actual
// reviewer both depend on.
func TestRenderReconcileResult_ShowsAnUnguardedTransitionUnderItsOwnNarrative(t *testing.T) {
	out := pipeline.RenderReconcileResult(pipeline.ReconcileRunResult{
		Results: []reconciler.ReconcileResult{
			{NarrativeID: 7, Proposed: []reconciler.ProposedAction{
				{
					Type: reconciler.ActionTransition, IssueKey: "PROJ-7",
					TargetStatus: "In Review", Confidence: 0.9,
				},
			}},
			{NarrativeID: 8, Proposed: []reconciler.ProposedAction{
				{
					Type: reconciler.ActionComment, IssueKey: "PROJ-8",
					Body: "unrelated narrative", Confidence: 0.9,
				},
			}},
		},
		Unguarded: []reconciler.UnguardedTransition{
			{NarrativeID: 7, IssueKey: "PROJ-7", TargetStatus: "In Review"},
		},
	})

	assert.Contains(t, out, "narrative 7")
	assert.Contains(t, out, "PROJ-7")
	assert.Contains(t, out, "no staleness check",
		"a reviewer must be told this transition bypassed the guard entirely, not merely see a proposal")

	beforeNarrative8 := strings.Index(out, "narrative 8")
	require.Positive(t, beforeNarrative8, "narrative 8 must render")
	assert.Less(t, strings.Index(out, "no staleness check"), beforeNarrative8,
		"the unguarded note must render under narrative 7, not narrative 8, which has no transition")
}

func TestRenderReconcileResultShowsEveryOutcomeKind(t *testing.T) {
	out := pipeline.RenderReconcileResult(pipeline.ReconcileRunResult{
		Results: []reconciler.ReconcileResult{
			{NarrativeID: 5, Proposed: []reconciler.ProposedAction{
				{
					Type: reconciler.ActionTransition, IssueKey: "PROJ-2",
					TargetStatus: "In Review", Confidence: 0.95,
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
	assert.Contains(t, out, "In Review",
		"a transition target must render as the status NAME a reviewer recognizes, "+
			"not a category that could mean four different destinations")
	assert.Contains(t, out, "PROJ-3")
	assert.Contains(t, out, "min_confidence_to_propose")
}

func TestRenderReconcileResult_EmptyPassSaysSoInsteadOfPrintingNothing(t *testing.T) {
	out := pipeline.RenderReconcileResult(pipeline.ReconcileRunResult{})

	assert.Contains(t, out, "no linked narratives to reconcile")
}
