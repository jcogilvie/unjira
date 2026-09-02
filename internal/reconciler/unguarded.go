package reconciler

import "fmt"

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

// noteUnguarded records every proposed transition whose issue has no collected
// status history, so a reviewer can tell "proposed, but the staleness guard could
// not run" from "proposed and cleared."
//
// Reads verifiedLink.HaveLastStatus — the same field suppressStaleTransitions
// consults — rather than re-querying the store. That is not merely cheaper: it
// makes the two structurally unable to disagree about whether a check happened,
// where two independent lookups could drift if anything ever wrote between them.
//
// Called on the actions that SURVIVED filtering, since an action that was
// suppressed or dropped as unroutable is not a proposal a reviewer will see.
func noteUnguarded(result *ReconcileResult, verified []verifiedLink) {
	byKey := make(map[string]verifiedLink, len(verified))
	for _, v := range verified {
		byKey[v.Link.IssueKey] = v
	}

	for _, action := range result.Proposed {
		if action.Type != ActionTransition {
			continue
		}

		v, ok := byKey[action.IssueKey]
		if !ok || v.HaveLastStatus {
			// Not a key this pass verified (actionsFromVerdicts already drops
			// those), or the guard had what it needed and did run.
			continue
		}

		result.Unguarded = append(result.Unguarded, UnguardedTransition{
			NarrativeID:  result.NarrativeID,
			IssueKey:     action.IssueKey,
			TargetStatus: action.TargetStatus,
		})
	}
}
