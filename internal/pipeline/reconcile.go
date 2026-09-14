package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/llm"
	"github.com/jcogilvie/unjira/internal/reconciler"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
	"github.com/jcogilvie/unjira/internal/workflow"
)

// ReconcileOptions configures one reconcile pass.
type ReconcileOptions struct {
	// DryRun runs the full pass — including the real LLM calls — but skips
	// Persist, matching NarrateOptions.DryRun's treatment.
	DryRun bool
	// Graph is the project's observed workflow graph, when the caller could get
	// one. It lets drafting propose a status several hops away and the action
	// carry the route there — necessary because unjira has no guaranteed run
	// cadence, so one delta routinely spans several statuses.
	//
	// Optional. Nil means single-hop, which is the pre-multi-hop behaviour, so a
	// tracker that cannot supply a graph needs no special case here. Resolved by
	// the caller (cmd/unjira) rather than here, for the same reason rules are:
	// this layer takes resolved inputs and does not reach for a tracker
	// capability itself.
	Graph *workflow.Graph
}

// ReconcileRunResult is one reconcile pass, shaped for rendering.
type ReconcileRunResult struct {
	Results []reconciler.ReconcileResult
	Stats   correlator.Stats
	DryRun  bool
	// Remaining is how many actionable-linked narratives are STILL eligible after
	// this pass — 0 when the backlog drained. See MatchRunResult.Remaining for why
	// this is data rather than only a log line.
	//
	// Covers the ACTIONABLE-LINK cap only, not ProposeCreates' separate untracked
	// cap. Two numbers in one field would be a lie, and the create backlog drains
	// on its own schedule; reporting the one an operator can act on beats reporting
	// a sum of two unrelated populations.
	Remaining int
	// Persisted is exactly what reconciler.Persist wrote THIS call — empty
	// under DryRun (Persist never ran) or after a Persist failure. This is
	// watch's auto-commit seam: it identifies "freshly proposed this pass" by
	// construction, which store.ActionsByStatus("proposed") cannot do (that
	// accessor returns every action still sitting at that status, including
	// ones from earlier passes a human has not yet triaged — see Persist's
	// own doc comment for why auto-committing those would be wrong).
	//
	// Populated even when the returned error is non-nil: Reconcile isolates
	// failures per narrative, so a partial pass still persists whatever it
	// drafted cleanly (see RunReconcile's doc comment). Callers that gate
	// auto-commit on a clean pass must check the returned error themselves —
	// Persisted being non-empty is NOT evidence the pass succeeded.
	Persisted []store.ActionRow
}

// RunReconcile runs one reconcile pass: validate cfg.Reconciler, load
// rules/'s rules.ScopeReconciler subset (see loadReconcilerRules) and hand
// it to reconciler.WithRules, draft proposals, and (unless DryRun) persist
// them. Loading rules/ here rather than in internal/reconciler follows the
// identical split RunMatch already uses for the correlator's rules — see
// match.go's doc comment: config parsing/loading is pipeline's job, and
// reconciler only ever sees the resolved rules.Rule slice as a parameter.
//
// tracker is a tasktracker.TaskReader, not a TaskTracker: reconciler.Reconcile
// already takes only a reader (the reconciler proposes and never applies), and
// widening the type here would discard that guarantee at the very layer meant
// to carry it up to the CLI. Nothing in this function's call graph can write
// to the tracker.
//
// Acquires no lease, for the same reason RunNarrate and RunMatch do not: the
// scope differs per caller, and taking one here would make `watch` contend with
// itself.
//
// On partial failure this returns both the accumulated result AND a non-nil
// error, because Reconcile isolates failures per narrative — the narratives that
// drafted cleanly are still worth showing. Note the asymmetry with Persist: a
// partial pass still persists whatever it produced, since each narrative's
// proposals are internally consistent even when a sibling narrative failed.
// That is not a contradiction of Persist's own all-or-nothing guarantee:
// Persist is atomic over the set it's handed, but Reconcile is what decides
// what makes it into that set — a narrative that failed never contributes any
// ProposedAction to results in the first place, so Persist's transaction never
// sees its half-drafted state.
func RunReconcile(
	ctx context.Context,
	s *store.Store,
	tracker tasktracker.TaskReader,
	client llm.Client,
	cfg config.Config,
	opts ReconcileOptions,
) (ReconcileRunResult, error) {
	if err := cfg.Reconciler.Validate(); err != nil {
		return ReconcileRunResult{}, fmt.Errorf("invalid reconciler config: %w", err)
	}

	reconcilerRules, err := loadReconcilerRules(cfg)
	if err != nil {
		return ReconcileRunResult{}, err
	}

	reconcileOpts := []reconciler.ReconcileOption{reconciler.WithRules(reconcilerRules)}
	if opts.Graph != nil {
		reconcileOpts = append(reconcileOpts, reconciler.WithWorkflowGraph(opts.Graph))
	}

	results, stats, reconcileErr := reconciler.Reconcile(
		ctx, s, tracker, client, cfg.Reconciler, reconcileOpts...)

	// Untracked narratives are a SEPARATE selection: Reconcile's backlog requires
	// a narrative_issues link by construction, so a narrative with none is never
	// examined by it at all. That is why untracked work produced no action even
	// though every layer below supports `create` — the gap was a missing selection
	// path, not a missing prompt option.
	createResults, createStats, createErr := reconciler.ProposeCreates(
		ctx, s, client, cfg.Reconciler, reconcilerRules)
	results = append(results, createResults...)
	stats.Add(createStats)
	reconcileErr = errors.Join(reconcileErr, createErr)

	result := ReconcileRunResult{Results: results, Stats: stats, DryRun: opts.DryRun}

	// Before the DryRun early return, so a dry run reports its backlog too — that
	// is the mode an operator uses to ask "how far behind am I", and answering only
	// on the writing path would withhold it exactly when it is most wanted.
	//
	// A count failure does not fail the pass: reconciling happened. Remaining stays
	// 0, which reads as caught-up — wrong, but quieter than discarding a completed
	// pass over a COUNT(*).
	if remaining, countErr := s.CountNarrativesWithActionableLinks(reconciler.SelectionRoles); countErr != nil {
		log.Printf("pipeline: could not count the remaining eligible narratives (%v); "+
			"this pass's summary will not report a backlog", countErr)
	} else {
		result.Remaining = remaining
	}

	if opts.DryRun {
		return result, reconcileErr
	}

	persisted, err := reconciler.Persist(s, results)
	if err != nil {
		return result, fmt.Errorf("persisting proposed actions: %w", err)
	}
	result.Persisted = persisted

	return result, reconcileErr
}
