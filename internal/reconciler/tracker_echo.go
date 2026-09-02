package reconciler

import (
	"fmt"

	"github.com/jcogilvie/unjira/internal/events"
)

// suppressTrackerEcho drops proposed comments whose narrative supplied no
// evidence of work — only records the tracker produced about itself.
//
// unjira's job is to close the gap between what you did and what the tracker
// knows. A delta of nothing but changelog entries, field edits and existing
// comments demonstrates no gap: every sentence a model can write from it is
// already on the issue. The comment is then a paraphrase of the thing it would be
// posted on.
//
// Measured, not theorized. On the 2026-09-02 triage pass 18 of 21 proposed
// comments had zero non-tracker evidence anywhere in the store. The worst was a
// long, well-argued RCA proposed on PAAS-4000 whose narrative held exactly [status
// Discovery -> In Progress, that issue's own description x4, status In Progress ->
// Done]. See docs/design-notes.md incident 24.
//
// Why this is not already covered by rules/no-self-narration.md: that rule is
// loaded into the drafting prompt and IS obeyed — it governs how to LEAD a
// comment ("never make a status change you did not perform the subject of what you
// write"). It says nothing about whether a narrative with no work evidence should
// produce a comment at all. A prompt-time constraint on phrasing cannot express a
// structural precondition, and asking a model to notice that all of its input is
// tracker bookkeeping is asking it to judge provenance it cannot see.
//
// # Scope
//
// Comments only.
//
//   - A TRANSITION asserts no prose. Its justification is
//     suppressStaleTransitions' business, which weighs work evidence on its own
//     terms and deliberately degrades to proposing when it cannot judge. Applying
//     this filter there would both duplicate that guard and override its chosen
//     failure direction.
//   - A CREATE concerns work with no issue at all, so "restating the issue" is
//     incoherent — there is nothing to restate.
//
// Judged over the whole delta, not per issue — deliberately unlike
// newestWorkEvidence, which is per-issue because a status change must not become
// its own justification. Here the question is about the narrative: a narrative is
// one story, and work evidence anywhere in it establishes that unjira saw
// something the tracker did not. Scoping per issue would suppress the second half
// of every genuine multi-issue narrative, where a same_work sibling carries the
// transcript.
//
// Suppressions are reported rather than dropped, per the repo-wide invariant that
// "nothing proposed" must stay distinguishable from "nothing considered".
func suppressTrackerEcho(
	delta []events.Event, drafted []ProposedAction,
) (kept []ProposedAction, suppressed []string) {
	// One decision for the whole narrative: the predicate does not vary per
	// action, so evaluating it per action would invite a future edit to make it
	// vary without noticing that it should not.
	if events.AnyWorkEvidence(delta) {
		return drafted, nil
	}

	kept = make([]ProposedAction, 0, len(drafted))

	for _, action := range drafted {
		if action.Type != ActionComment {
			kept = append(kept, action)

			continue
		}

		suppressed = append(suppressed, fmt.Sprintf(
			"comment on %s: no evidence of work in the delta — every event is a record the "+
				"tracker produced about itself (%d of them), so a comment could only restate "+
				"what the issue already says",
			action.IssueKey, len(delta),
		))
	}

	return kept, suppressed
}
