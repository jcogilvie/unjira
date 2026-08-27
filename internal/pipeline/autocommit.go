package pipeline

import (
	"errors"
	"fmt"
	"strings"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/gate"
	"github.com/jcogilvie/unjira/internal/store"
)

// AutoCommitOptions bundles the gate pieces RunAutoCommit needs: the rules
// gate.Decide consults, and the Applier authorized to write. Kept separate
// from config.Config so a caller (and every test) can construct only the
// input that matters, matching gate.Decide's own rationale for taking
// map[string]config.AutoCommitRule directly rather than a whole Config.
type AutoCommitOptions struct {
	Rules   map[string]config.AutoCommitRule
	Applier *gate.Applier
}

// AutoCommitRunResult tallies one auto-commit pass, for rendering: which
// actions applied, which queued for triage, and which failed on write.
type AutoCommitRunResult struct {
	Applied []store.ActionRow
	Queued  []store.ActionRow
	Failed  []store.ActionRow
}

// RunAutoCommit is the auto-commit gate's orchestration half: for each of
// actions, consult gate.Decide and, on DecisionApply, call opts.Applier.Apply.
//
// actions is deliberately a plain slice rather than a ReconcileRunResult or
// similar pass-shaped type. The gate's all-or-nothing-per-invocation
// property — "if reconciliation fails partway through a pass, nothing from
// that pass auto-commits" — has no representation here on purpose: Decide
// and Apply are both pure/per-action with no notion of which reconcile pass
// produced an action, so there is no pass-level state for this function to
// refuse on. That decision belongs to the caller (watch), which knows
// whether RunReconcile returned a clean pass (err == nil) and passes an
// empty or nil actions slice otherwise. RunAutoCommit itself has no opinion
// on why it was handed few or many actions — it only ever decides and
// applies whatever it is given, which is what keeps it trivially testable
// without reconstructing a whole reconcile pass.
//
// One action's apply failure does not abort the rest: every action targets
// its own issue/narrative independently, Applier.Apply already records the
// failure as status=failed on that one row (never retried — see Applier's
// own doc comment), and Reconcile itself sets the precedent for per-item
// isolation via errors.Join rather than abandoning a whole batch over one
// bad write. Aborting here would leave every action AFTER the failing one
// stuck at status=proposed for no reason related to their own validity.
func RunAutoCommit(actions []store.ActionRow, opts AutoCommitOptions) (AutoCommitRunResult, error) {
	var (
		result AutoCommitRunResult
		errs   error
	)

	for _, action := range actions {
		switch gate.Decide(action, opts.Rules) {
		case gate.DecisionApply:
			if err := opts.Applier.Apply(action); err != nil {
				result.Failed = append(result.Failed, action)
				errs = errors.Join(errs, err)

				continue
			}

			result.Applied = append(result.Applied, action)
		case gate.DecisionQueue:
			result.Queued = append(result.Queued, action)
		}
	}

	return result, errs
}

// RenderAutoCommitResult formats one auto-commit pass for a terminal,
// matching RenderReconcileResult's style: every action is named, not just
// counted, so an operator watching `watch`'s output can see WHICH issue got
// a live write without cross-referencing the actions table.
func RenderAutoCommitResult(r AutoCommitRunResult) string {
	var b strings.Builder

	b.WriteString("\nauto-commit\n")
	fmt.Fprintf(&b, "  applied: %d  queued: %d  failed: %d\n",
		len(r.Applied), len(r.Queued), len(r.Failed))

	for _, a := range r.Applied {
		fmt.Fprintf(&b, "  applied  %s action %d on %s\n", a.Type, a.ID, a.IssueKey)
	}
	for _, a := range r.Queued {
		fmt.Fprintf(&b, "  queued   %s action %d on %s (confidence %.2f)\n", a.Type, a.ID, a.IssueKey, a.Confidence)
	}
	for _, a := range r.Failed {
		fmt.Fprintf(&b, "  failed   %s action %d on %s\n", a.Type, a.ID, a.IssueKey)
	}

	return b.String()
}
