package reconciler

import (
	"log/slog"

	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/store"
)

// filterContext is everything any suppression filter needs to judge a drafted
// action. One struct rather than four different parameter lists, which is what
// lets the chain be a slice instead of four hand-wired calls.
//
// Every filter receives the whole context and reads only its own part. That is a
// deliberate trade: a filter can technically reach for a field it does not need,
// in exchange for the chain being data. The alternative — a per-filter parameter
// shape — is what the four filters had before, and it made the chain
// uncomposable, so the ORDER of suppression lived only in the sequence of
// statements in reconcileOne and nothing could assert it.
//
// docs/design-notes.md incident 22 was an ordering bug in exactly that code. The
// rule it produced ("write the end-to-end test first when a change spans more
// than one function") is a process workaround for this structural problem; this
// type is the structural fix, and filters_test.go asserts the order directly.
type filterContext struct {
	// NarrativeID identifies the narrative being reconciled, for suppression
	// reasons and for the duplicate check's "a DIFFERENT narrative" test.
	NarrativeID int64

	// Store is needed by the duplicate filter alone, which asks a question no
	// pure function can: does another narrative already hold an open proposal
	// on this issue?
	//
	// Its presence here is why filterContext is a struct and not a set of pure
	// values. Three of the four filters are pure; making all four take a store
	// they mostly ignore is the cost of a uniform chain. See suppressDuplicates'
	// own doc comment for why that impurity is inherent rather than incidental.
	Store *store.Store

	// Delta is what is new since a reviewer last saw this work — the evidence
	// the tracker-echo and staleness filters weigh.
	Delta []events.Event

	// Verified are the narrative's links whose issues were confirmed against
	// the live tracker, carrying current status and legal transitions.
	Verified []verifiedLink

	// Log is where the duplicate filter reports a degraded lookup. Nil is silent.
	Log *slog.Logger
}

// suppressionFilter is one reason a drafted action might not reach a reviewer.
//
// It returns the survivors and a reason per suppression — never a bare count and
// never a silent drop. That contract is repo-wide: "nothing proposed" must stay
// distinguishable from "nothing considered", or a reviewer reading an empty queue
// cannot tell whether unjira thought about the work at all.
type suppressionFilter struct {
	// name identifies the filter in the chain, for the ordering test and for
	// debugging which stage removed an action.
	name string

	// apply judges every action it is given. Actions of a type this filter has
	// no opinion about must be passed through untouched, not dropped — each
	// filter answers one question and stays silent on the rest.
	apply func(fctx filterContext, drafted []ProposedAction) (kept []ProposedAction, suppressed []string)
}

// suppressionChain is the ordered sequence every drafted action runs through.
//
// The order is load-bearing, and each filter's own doc comment argues for its
// position. Stated once here, because a reader should not have to reconstruct it
// from four files:
//
//  1. unroutable — an action whose target cannot be reached is not a proposal at
//     all. Nothing downstream should weigh in on something that cannot happen.
//  2. tracker-echo — asks whether the narrative had anything to say AT ALL,
//     before anything asks whether this particular thing is stale.
//  3. stale-transition — a fact about the world: the tracker moved past us.
//  4. duplicate — a fact about unjira's own queue, worth reporting only for an
//     action that should exist in the first place.
//
// Order flows from cheapest-and-most-fundamental to most-contingent, and from
// pure to impure: the only filter that touches the store runs last, so a store
// error cannot cost work the earlier filters would have done anyway.
//
// Appending here is how a new suppression reason is added. Reordering is a
// behaviour change, and TestSuppressionChain_OrderIsExplicitAndLoadBearing will
// say so.
var suppressionChain = []suppressionFilter{
	{
		name: "unroutable",
		apply: func(fctx filterContext, drafted []ProposedAction) ([]ProposedAction, []string) {
			return dropUnroutable(fctx.Verified, drafted)
		},
	},
	{
		name: "tracker-echo",
		apply: func(fctx filterContext, drafted []ProposedAction) ([]ProposedAction, []string) {
			return suppressTrackerEcho(fctx.Delta, drafted)
		},
	},
	{
		name: "stale-transition",
		apply: func(fctx filterContext, drafted []ProposedAction) ([]ProposedAction, []string) {
			return suppressStaleTransitions(fctx.Delta, fctx.Verified, drafted)
		},
	},
	{
		name: "duplicate",
		apply: func(fctx filterContext, drafted []ProposedAction) ([]ProposedAction, []string) {
			return suppressDuplicates(fctx.Store, fctx.NarrativeID, drafted, fctx.Log)
		},
	},
}

// runSuppression walks the chain, feeding each filter only what survived the
// last, and returns the survivors plus every reason collected.
//
// Feeding forward rather than judging independently is what stops one dead action
// from collecting a reason per filter that would have caught it — a reviewer
// counting reasons would otherwise over-count what unjira considered.
//
// No filter can error: a filter that cannot judge (a failed store lookup, absent
// status history) keeps the action and says so, because a proposal reaches a human
// while a suppression does not. That asymmetry is the same one the staleness
// guard's degrade-to-proposing stance encodes, and it is why this returns no
// error rather than aborting a narrative mid-chain.
func runSuppression(
	fctx filterContext, drafted []ProposedAction,
) (kept []ProposedAction, suppressed []string) {
	kept = drafted

	for _, filter := range suppressionChain {
		var reasons []string

		kept, reasons = filter.apply(fctx, kept)
		suppressed = append(suppressed, reasons...)
	}

	return kept, suppressed
}
