package reconciler

import (
	"fmt"
	"strings"

	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// suppressSettledStatus drops transitions the issue's LIVE status already settles,
// returning the survivors and a reason per suppression:
//
//   - A transition to the status the issue is already at. It changes nothing, so it
//     is not a proposal; a reviewer approving it would learn nothing.
//   - Any transition out of a done-category status. Reopening a closed ticket is a
//     judgment about why it was closed, which no collected event records, so it is a
//     human's call, not one unjira proposes. This is not the direction test design
//     incident 19 rejects: moves between working statuses, backwards or forwards, are
//     untouched. Only leaving done is refused.
//
// Judged from the live status alone, on purpose. suppressStaleTransitions needs the
// issue's collected status history and lets a transition through when there is none,
// which is the right fallback for its question. That made it blind to tickets outside
// the collected projects, and on a real store those were exactly the bad ones: an
// Implementing ticket proposed -> "Implementing", and a Closed incident ticket proposed
// -> "Ongoing".
//
// Comments are untouched, including on a done issue: a note on a ticket that just
// closed is legitimate. An action whose issue this filter cannot see passes through,
// for dropUnroutable, which runs first, to have already judged.
func suppressSettledStatus(verified []verifiedLink, drafted []ProposedAction) (kept []ProposedAction, suppressed []string) {
	byKey := make(map[string]verifiedLink, len(verified))
	for _, v := range verified {
		byKey[v.Link.IssueKey] = v
	}

	kept = make([]ProposedAction, 0, len(drafted))

	for _, action := range drafted {
		v, ok := byKey[action.IssueKey]
		if action.Type != ActionTransition || !ok {
			kept = append(kept, action)

			continue
		}

		current := strings.TrimSpace(v.Issue.StatusName)
		target := strings.TrimSpace(action.TargetStatus)

		switch {
		case current != "" && strings.EqualFold(current, target):
			suppressed = append(suppressed, fmt.Sprintf(
				"transition %s -> %q: the issue is already at %q, so the move changes nothing",
				action.IssueKey, target, current))
		case v.Issue.StatusCategory == tasktracker.StatusDone:
			suppressed = append(suppressed, fmt.Sprintf(
				"transition %s -> %q: the issue is done (%q); reopening a done issue is a human's call, "+
					"since why it was closed is not in any collected event",
				action.IssueKey, target, current))
		default:
			kept = append(kept, action)
		}
	}

	return kept, suppressed
}
