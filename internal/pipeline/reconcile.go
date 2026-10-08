package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/llm"
	"github.com/jcogilvie/unjira/internal/logging"
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
	// UnmatchedNarratives is how many narratives matching left unexamined, from
	// MatchRunResult.Remaining. Nonzero DEFERS the create path entirely.
	//
	// The precondition finding F13 was missing: "this narrative has no link" only
	// means "untracked" once matching has examined everything. While matching is
	// behind it means "not looked at yet", and those are different facts.
	// ProposeCreates conflated them, and because its population (no link at all) is
	// a SUBSET of matching's (no primary link), the narratives it reached first were
	// exactly the ones matching had just skipped by cap. It proposed opening a
	// ticket for work PAAS-3898 already tracked and had closed.
	//
	// Resolved by the caller rather than queried here, matching Graph and rules
	// above: this layer takes resolved inputs. The caller already has the number —
	// RunMatch returns it and it is in scope immediately before RunReconcile — so
	// the check costs nothing new.
	//
	// Zero when the caller has no matching stage to report on (a reconcile-only
	// invocation), which reads as "matching is caught up" and preserves the
	// pre-F13 behaviour. That is the safe default only because a caller with no
	// matching stage cannot be racing one.
	UnmatchedNarratives int
	// Log is where the stage reports degradation. Nil is silent.
	Log *slog.Logger
}

// ReconcileRunResult is one reconcile pass, shaped for rendering.
type ReconcileRunResult struct {
	Results []reconciler.ReconcileResult
	Stats   correlator.Stats
	DryRun  bool
	// CreatesDeferred is how many narratives matching had left unexamined when this
	// pass chose NOT to run the create path, or 0 when creates ran normally.
	//
	// Reported rather than silent, because a silent deferral is F10's failure mode
	// wearing a different hat: a matching stage permanently behind its cap would make
	// unjira quietly stop proposing creates forever, and the pass summary would look
	// like a pass with nothing to create. The number is also the actionable part — it
	// says how far matching has to get before creates resume.
	CreatesDeferred int
	// Remaining is how many actionable-linked narratives are STILL eligible after
	// this pass — 0 when the backlog drained. See MatchRunResult.Remaining for why
	// this is data rather than only a log line.
	//
	// Covers the ACTIONABLE-LINK cap only, not ProposeCreates' separate untracked
	// cap. Two numbers in one field would be a lie, and the create backlog drains
	// on its own schedule; CreatesRemaining reports that one.
	//
	// Counted after Persist, as CreatesRemaining is, so a narrative this pass proposed
	// for is no longer counted. Under DryRun nothing is persisted, so it is: in the
	// store its delta is still unexamined.
	Remaining int
	// CreatesRemaining is how many untracked narratives STILL await a create decision
	// after this pass (store.CountNarrativesAwaitingCreate, the create selector's own
	// predicate) — 0 when that backlog drained, and 0 when creates were deferred, since
	// a deferred pass examined none of them and CreatesDeferred already says so.
	//
	// Counted after Persist, so the creates this pass proposed are no longer counted
	// as awaiting. Under DryRun nothing is persisted, so it counts them: in the store
	// they do still await a decision.
	//
	// Exists because a pass starved by its cap rendered like a finished one: on a real
	// 30-day store the create path reached 20 of 129 untracked narratives on its first
	// pass and none on any later one, and nothing on stdout said so.
	CreatesRemaining int
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

	createsDeferred := 0

	// The allowed-destination set comes from the tracker config: a link on a read-only
	// tracker is not drafted, and untracked work is ticketed where its location's
	// tracker (or default_ticket_in) says, or nowhere.
	destinations := reconciler.WithDestinations(cfg)

	// The floor is match.confidence_floor, shared by both halves: a primary below it is
	// no confident home, so Reconcile drafts onto none of that narrative's links and the
	// create path proposes a ticket for it instead, naming those links. One option for
	// both, so the two cannot disagree about which narrative is whose.
	floor := reconciler.WithConfidenceFloor(cfg.Match.ConfidenceFloor)

	reconcileOpts := []reconciler.ReconcileOption{
		reconciler.WithRules(reconcilerRules), reconciler.WithReconcileLogger(opts.Log), destinations, floor,
	}
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
	// DEFERRED while matching is behind — see ReconcileOptions.UnmatchedNarratives.
	// All-or-nothing rather than per-narrative: deferring only the narratives
	// matching has not reached would need a record of which those are, and matching
	// writes nothing when it finds no candidates. Since a later matching pass is the
	// very thing that produces the link, deferring costs latency while proposing
	// costs a duplicate ticket.
	if opts.UnmatchedNarratives > 0 {
		createsDeferred = opts.UnmatchedNarratives
	} else {
		createResults, createStats, createErr := reconciler.ProposeCreates(
			ctx, s, client, cfg.Reconciler, reconcilerRules, opts.Log, destinations, floor,
			reconciler.WithCandidateReader(tracker))
		results = append(results, createResults...)
		stats.Add(createStats)
		reconcileErr = errors.Join(reconcileErr, createErr)
	}

	result := ReconcileRunResult{
		Results: results, Stats: stats, DryRun: opts.DryRun,
		CreatesDeferred: createsDeferred,
	}

	createsRan := createsDeferred == 0

	// Both backlogs are counted where the pass ends, never before Persist: once Persist
	// commits, a narrative this pass proposed for has an action whose link sequence
	// bounds its delta, so it no longer carries unexamined work. Counted before Persist,
	// Remaining reported every narrative the pass had just handled as still outstanding.
	//
	// A dry run reports both too — that is the mode an operator uses to ask "how far
	// behind am I", and answering only on the writing path would withhold it exactly
	// when it is most wanted. It persists nothing, so its counts are the store as the
	// pass found it, which is the true answer: nothing it drafted was recorded.
	if opts.DryRun {
		result.Remaining = countDeltaBacklog(s, opts.Log)
		if createsRan {
			result.CreatesRemaining = countCreateBacklog(s, cfg.Match.ConfidenceFloor, opts.Log)
		}

		return result, reconcileErr
	}

	persisted, err := reconciler.Persist(s, results)

	// Counted whether or not Persist succeeded. Persist is all-or-nothing, so after a
	// failure the store is as the pass found it and the count is still true; the CLI
	// renders the summary alongside the error, and a 0 there would read as caught up.
	result.Remaining = countDeltaBacklog(s, opts.Log)

	if err != nil {
		return result, fmt.Errorf("persisting proposed actions: %w", err)
	}
	result.Persisted = persisted

	if createsRan {
		result.CreatesRemaining = countCreateBacklog(s, cfg.Match.ConfidenceFloor, opts.Log)
	}

	return result, reconcileErr
}

// countDeltaBacklog is ReconcileRunResult.Remaining. A count failure does not fail the
// pass: reconciling happened, and 0 — which reads as caught up — is wrong but quieter
// than discarding a completed pass over a COUNT(*).
//
// CountNarrativesWithDelta, not CountNarrativesWithActionableLinks: the latter is what a
// pass SELECTS, and reconcileOne skips a selected narrative whose delta is empty.
// Counting the selection reported 55 where 20 had nothing new (finding F12), telling an
// operator to re-run for work that did not exist — and each re-run bills for the sweep.
// The count mirrors the skip.
func countDeltaBacklog(s *store.Store, log *slog.Logger) int {
	n, err := s.CountNarrativesWithDelta(reconciler.SelectionRoles)
	if err != nil {
		logging.For(log, "pipeline").Warn(
			"could not count the narratives with unexamined work",
			"err", err, "consequence", "this pass's summary will not report a backlog")

		return 0
	}

	return n
}

// countCreateBacklog is ReconcileRunResult.CreatesRemaining. A count failure does not
// fail the pass, for the reason countDeltaBacklog gives: the work happened, and 0 is the
// quieter wrong answer.
//
// floor is the one the create pass selected under (match.confidence_floor), or the count
// describes a different population than the pass examined.
func countCreateBacklog(s *store.Store, floor float64, log *slog.Logger) int {
	n, err := s.CountNarrativesAwaitingCreate(floor)
	if err != nil {
		logging.For(log, "pipeline").Warn(
			"could not count the narratives awaiting a create decision",
			"err", err, "consequence", "this pass's summary will not report a create backlog")

		return 0
	}

	return n
}
