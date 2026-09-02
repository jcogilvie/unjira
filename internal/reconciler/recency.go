package reconciler

import (
	"fmt"
	"strings"
	"time"

	"github.com/jcogilvie/unjira/internal/events"
)

// statusFieldArtifact is the artifacts key a collector sets to name which issue
// field a change touched, and statusField is its value for a status change.
// Matching internal/collector/jira's own constants; duplicated rather than
// imported because internal/reconciler must not depend on a specific collector
// — any collector that supplies status history uses this contract.
const (
	statusFieldArtifact = "field"
	statusField         = "status"
	issueKeyArtifact    = "issue_key"
)

// suppressStaleTransitions drops proposed transitions the tracker has already
// overtaken, returning the survivors and a reason per suppression.
//
// A transition is proposed from evidence that WORK reached a new stage. That
// evidence goes stale when somebody else moves the issue afterwards: they knew
// something unjira's events do not record. The canonical case is a security scan
// bouncing a ticket In Review -> In Progress after finding the vulnerability
// still present — real, deliberate, and unjira must not undo it.
//
// Two checks, because the tracker can get ahead of unjira in two different ways:
//
//  1. SUPERSEDED. The collected status change postdates the newest work
//     evidence. Whoever moved it moved it knowing more than we do.
//  2. STALE COLLECTION. The live status disagrees with the newest collected
//     status change. Somebody moved the issue since the last collect, and unjira
//     cannot know when or why — so it cannot establish (1) either way.
//
// Check 2 exists because check 1 alone is blind in exactly the window that
// matters most: right after a handoff and before the next collect, the collected
// history still shows OUR last known move, so the timestamps look fine and the
// proposal goes out.
//
// Deliberately NOT a direction test. "Never move a ticket backwards" fails on
// this case twice over: backward moves are legitimate (review findings, failed
// testing), and the proposal that undoes the security handoff is FORWARDS, so a
// direction test would let it through. See docs/design-notes.md incident 19.
//
// A link with no collected history (HaveLastStatus false) is left alone: the
// guard cannot run, so it degrades to proposing rather than to silence. That is
// the weaker direction on purpose — a proposal reaches a human, a suppression
// does not — and making an unrunnable guard visible rather than merely absent is
// task #174.
func suppressStaleTransitions(
	delta []events.Event, verified []verifiedLink, drafted []ProposedAction,
) (kept []ProposedAction, suppressed []string) {
	byKey := make(map[string]verifiedLink, len(verified))
	for _, v := range verified {
		byKey[v.Link.IssueKey] = v
	}

	kept = make([]ProposedAction, 0, len(drafted))

	for _, action := range drafted {
		if action.Type != ActionTransition {
			kept = append(kept, action)

			continue
		}

		v, ok := byKey[action.IssueKey]
		if !ok || !v.HaveLastStatus {
			// Not ours to judge, or nothing to judge against.
			kept = append(kept, action)

			continue
		}

		// Check 2 first: if the collected history is stale, its timestamp cannot
		// support check 1, so reporting "superseded" would name the wrong reason.
		if !strings.EqualFold(strings.TrimSpace(v.Issue.StatusName), strings.TrimSpace(v.LastStatus.To)) {
			suppressed = append(suppressed, fmt.Sprintf(
				"transition %s -> %q: the issue is at %q but unjira's newest collected "+
					"status change says %q, so it moved since unjira last collected; "+
					"whoever moved it knows something these events do not",
				action.IssueKey, action.TargetStatus, v.Issue.StatusName, v.LastStatus.To,
			))

			continue
		}

		newest, haveWork := newestWorkEvidence(delta, action.IssueKey)
		if haveWork && newest.After(v.LastStatus.OccurredAt) {
			kept = append(kept, action)

			continue
		}

		// Equal timestamps land here deliberately. Jira's changelog is
		// second-granular, so work and a status change in the same second have no
		// knowable order, and assuming we came second is the unsafe guess.
		suppressed = append(suppressed, fmt.Sprintf(
			"transition %s -> %q: the issue moved to %q at %s, after the newest work "+
				"evidence (%s), so that evidence has been superseded",
			action.IssueKey, action.TargetStatus, v.LastStatus.To,
			v.LastStatus.OccurredAt.Format(time.RFC3339), workEvidenceLabel(newest, haveWork),
		))
	}

	return kept, suppressed
}

// newestWorkEvidence returns the newest event in delta that counts as evidence
// work advanced, and whether any did.
//
// Status changes ON issueKey are excluded, and that exclusion is what makes the
// two checks compose. Once somebody else's move is collected it enters the delta
// as a status event with that move's own timestamp — so counting it as work would
// make the status change its own justification, and the suppression would
// silently stop working the moment collection caught up.
//
// Scoped to issueKey, not to status events generally: another issue's transition
// is ordinary activity in this narrative and remains real evidence. And scoped to
// status events, not to the whole jira source: a comment or a description edit is
// work, so dropping every jira event would make the Jira collector's output
// invisible as evidence.
func newestWorkEvidence(delta []events.Event, issueKey string) (newest time.Time, found bool) {
	for _, e := range delta {
		if isStatusChangeOn(e, issueKey) {
			continue
		}
		if !found || e.OccurredAt.After(newest) {
			newest = e.OccurredAt
			found = true
		}
	}

	return newest, found
}

// isStatusChangeOn reports whether e is a collected status change on issueKey.
func isStatusChangeOn(e events.Event, issueKey string) bool {
	if field, _ := e.Artifacts[statusFieldArtifact].(string); field != statusField {
		return false
	}

	key, _ := e.Artifacts[issueKeyArtifact].(string)

	return key == issueKey
}

// workEvidenceLabel renders the newest work timestamp for a suppression reason,
// distinguishing "no work evidence at all" from a real time — a reason a reviewer
// cannot act on is barely better than silence.
func workEvidenceLabel(newest time.Time, found bool) string {
	if !found {
		return "none: the delta holds only status changes on this issue"
	}

	return newest.Format(time.RFC3339)
}
