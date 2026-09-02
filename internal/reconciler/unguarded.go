package reconciler

import (
	"fmt"

	"github.com/jcogilvie/unjira/internal/store"
)

// UnguardedTransition names one proposed transition that reached
// ReconcileResult.Proposed without ever passing through
// suppressStaleTransitions' staleness check, because unjira has no collected
// status history for that issue (verifiedLink.HaveLastStatus false — see
// recency.go's own doc comment on that branch). It is a distinct fact from
// "suppressed": a suppression is a transition that WAS judged and rejected; an
// unguarded transition was never judged at all. Reporting only the first
// would make an unchecked proposal look exactly like a cleared one.
type UnguardedTransition struct {
	NarrativeID  int64
	IssueKey     string
	TargetStatus string
}

// Reason renders a human-facing explanation, matching the style of the
// strings suppressStaleTransitions already appends to ReconcileResult.Suppressed
// (see recency.go) so the two read as one family of "why" text in triage
// output.
func (u UnguardedTransition) Reason() string {
	return fmt.Sprintf(
		"transition %s -> %q was proposed with no staleness check: unjira has no collected "+
			"status history for this issue, so it cannot tell whether the tracker moved it since "+
			"the newest work evidence (see docs/design-notes.md incident 19); review with extra care",
		u.IssueKey, u.TargetStatus,
	)
}

// FindUnguardedTransitions scans results for every proposed ActionTransition
// whose issue has no status change collected in s, so a caller can surface
// "proposed, but the staleness guard could not run" distinctly from "not
// proposed" (ReconcileResult.Suppressed) — making task #174's known gap
// visible per narrative rather than merely absent.
//
// It exists as a separate pass over an already-computed []ReconcileResult,
// rather than as a field reconciler.Reconcile populates directly on
// ReconcileResult, because internal/reconciler/{types,reconciler,draft,
// persist}.go and recency.go are owned by a parallel in-flight change this
// session must not conflict with — see this package's unguarded_test.go doc
// comment. Recomputing via s.LatestStatusEvent(key), the exact call
// verifyLinks already makes while building HaveLastStatus, reproduces that
// same fact with no intervening collection between Reconcile returning and
// this running in the same pass (RunReconcile calls this immediately after,
// before Persist or any collector runs again) — so this sees precisely what
// suppressStaleTransitions saw, at a small, one-time query-per-transition
// cost, not a live tracker call.
//
// A store error aborts the whole call rather than silently treating the
// failed lookup as "guarded" (which would hide the exact case this function
// exists to surface) or "unguarded" (which would fabricate a finding from a
// failure that says nothing about the issue itself). The caller decides how
// to degrade — see RunReconcile, which logs and keeps the real proposals
// rather than discarding a clean pass over a failure in this purely
// informational annotation step.
func FindUnguardedTransitions(s *store.Store, results []ReconcileResult) ([]UnguardedTransition, error) {
	var out []UnguardedTransition

	for _, result := range results {
		for _, action := range result.Proposed {
			if action.Type != ActionTransition {
				continue
			}

			_, haveHistory, err := s.LatestStatusEvent(action.IssueKey)
			if err != nil {
				return nil, fmt.Errorf(
					"checking collected status history for %s on narrative %d: %w",
					action.IssueKey, result.NarrativeID, err,
				)
			}

			if haveHistory {
				continue
			}

			out = append(out, UnguardedTransition{
				NarrativeID:  result.NarrativeID,
				IssueKey:     action.IssueKey,
				TargetStatus: action.TargetStatus,
			})
		}
	}

	return out, nil
}
