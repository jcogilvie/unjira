package reconciler

// filters_test.go covers the suppression chain: the four filters that stand
// between what the model drafted and what reaches a reviewer.
//
// Before this existed the chain was hand-wired in reconcileOne — four calls,
// four different parameter shapes, and `result.Suppressed = append(...)`
// repeated after each. docs/design-notes.md incident 22 was an ORDERING bug in
// exactly that code (floorConfidence ran before the route was attached, so every
// multi-hop action floored to 0), and the rule it produced — "write the
// end-to-end test first when a change spans more than one function" — was a
// process workaround for a structural problem: order was implicit in the
// sequence of statements, so nothing could assert it.
//
// These tests assert the chain as DATA. That is the point of the refactor.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// TestSuppressionChain_OrderIsExplicitAndLoadBearing pins the sequence.
//
// Order is not arbitrary — each filter's doc comment argues for its position, and
// the arguments compose:
//
//  1. dropUnroutable — an action with no route is not a proposal at all, so
//     nothing downstream should weigh in on something that cannot happen.
//  2. suppressTrackerEcho — asks whether the narrative had anything to say AT
//     ALL, before anything asks whether this particular thing is stale.
//  3. suppressStaleTransitions — a fact about the world (the tracker overtook us).
//  4. suppressDuplicates — a fact about unjira's own queue, which is only worth
//     reporting for an action that should exist in the first place.
//
// Asserting the order here means a future reordering is a test failure with a
// reason attached, rather than a silent behaviour change. If this fails, read the
// four doc comments before changing the expectation.
func TestSuppressionChain_OrderIsExplicitAndLoadBearing(t *testing.T) {
	names := make([]string, 0, len(suppressionChain))
	for _, f := range suppressionChain {
		names = append(names, f.name)
	}

	assert.Equal(t, []string{
		"unroutable",
		"tracker-echo",
		"stale-transition",
		"duplicate",
	}, names)
}

// TestRunSuppression_AppliesEveryFilterInTheChain is the guard incident 22
// needed and did not have.
//
// A filter can be perfectly correct and never run — that is the entire failure
// mode. Here each filter is given input it MUST suppress, and the test asserts
// every one of them contributed a reason. A filter dropped from the chain, or
// short-circuited by an early return, fails this.
func TestRunSuppression_AppliesEveryFilterInTheChain(t *testing.T) {
	s := reconcileStore(t)
	at := time.Date(2026, 8, 25, 9, 30, 0, 0, time.UTC)

	// A delta of only tracker records, so tracker-echo has something to bite on.
	fctx := filterContext{
		NarrativeID: 1,
		Store:       s,
		Delta: []events.Event{
			trackerRecord("PAAS-1:status:1", "PAAS-1", "PAAS-1 status: Discovery → Done", at),
		},
		Verified: []verifiedLink{{
			Link:  store.NarrativeIssue{IssueKey: "PAAS-1", Role: store.Role("primary")},
			Issue: tasktracker.Issue{Key: "PAAS-1", StatusName: "Done"},
		}},
	}

	drafted := []ProposedAction{
		// No Route and not directly offered -> unroutable.
		{Type: ActionTransition, IssueKey: "PAAS-1", TargetStatus: "In Review", Confidence: 0.9},
		// Tracker-only delta -> tracker echo.
		{Type: ActionComment, IssueKey: "PAAS-1", Body: "restating the ticket", Confidence: 0.7},
	}

	kept, suppressed := runSuppression(fctx, drafted)

	assert.Empty(t, kept)
	assert.Len(t, suppressed, 2,
		"both filters must have run; a silently skipped filter is incident 22's shape")
}

// TestRunSuppression_APassingActionSurvivesTheWholeChain is the other half.
// A chain that suppresses everything would satisfy the test above while being
// useless, so this pins that a legitimate action reaches the end untouched.
func TestRunSuppression_APassingActionSurvivesTheWholeChain(t *testing.T) {
	s := reconcileStore(t)
	at := time.Date(2026, 8, 25, 9, 30, 0, 0, time.UTC)

	fctx := filterContext{
		NarrativeID: 1,
		Store:       s,
		Delta: []events.Event{
			// Real work evidence, so tracker-echo has no grounds.
			codeEvent("cc:1", "implemented the fix"),
			trackerRecord("PAAS-1:status:1", "PAAS-1", "PAAS-1 status: Discovery → In Progress", at),
		},
		Verified: []verifiedLink{{
			Link:  store.NarrativeIssue{IssueKey: "PAAS-1", Role: store.Role("primary")},
			Issue: tasktracker.Issue{Key: "PAAS-1", StatusName: "In Progress"},
		}},
	}

	drafted := []ProposedAction{
		{Type: ActionComment, IssueKey: "PAAS-1", Body: "what we actually did", Confidence: 0.7},
	}

	kept, suppressed := runSuppression(fctx, drafted)

	require.Len(t, kept, 1)
	assert.Equal(t, "what we actually did", kept[0].Body)
	assert.Empty(t, suppressed)
}

// TestRunSuppression_StopsFeedingASuppressedActionDownstream: once a filter
// suppresses an action, later filters must not see it.
//
// Not merely wasted work — a second filter reporting on an already-dead action
// would put two reasons in Suppressed for one proposal, and a reviewer counting
// reasons would over-count what unjira considered.
func TestRunSuppression_StopsFeedingASuppressedActionDownstream(t *testing.T) {
	s := reconcileStore(t)
	at := time.Date(2026, 8, 25, 9, 30, 0, 0, time.UTC)

	// This action is BOTH unroutable and a tracker echo. Only the first filter
	// to reach it should report it.
	fctx := filterContext{
		NarrativeID: 1,
		Store:       s,
		Delta: []events.Event{
			trackerRecord("PAAS-1:status:1", "PAAS-1", "PAAS-1 status: Discovery → Done", at),
		},
		Verified: []verifiedLink{{
			Link:  store.NarrativeIssue{IssueKey: "PAAS-1", Role: store.Role("primary")},
			Issue: tasktracker.Issue{Key: "PAAS-1", StatusName: "Done"},
		}},
	}

	drafted := []ProposedAction{
		{Type: ActionTransition, IssueKey: "PAAS-1", TargetStatus: "In Review", Confidence: 0.9},
	}

	_, suppressed := runSuppression(fctx, drafted)

	assert.Len(t, suppressed, 1,
		"one dead action yields one reason, not one per filter that would have caught it")
	assert.Contains(t, suppressed[0], "no route",
		"and it is the FIRST filter's reason, since that is the one that ran")
}

// TestRunSuppression_EmptyDraftIsNotAnError: the model proposing nothing is an
// ordinary outcome, and the chain must handle it without a nil-slice panic in any
// of the four filters.
func TestRunSuppression_EmptyDraftIsNotAnError(t *testing.T) {
	kept, suppressed := runSuppression(filterContext{NarrativeID: 1, Store: reconcileStore(t)}, nil)

	assert.Empty(t, kept)
	assert.Empty(t, suppressed)
}
