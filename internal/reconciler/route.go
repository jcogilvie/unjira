package reconciler

import (
	"fmt"
	"strings"

	"github.com/jcogilvie/unjira/internal/workflow"
)

// reachableTargets lists every status this issue could be moved to, for the
// drafting prompt: the live-legal set plus anything the observed workflow graph
// reaches from the issue's current status.
//
// The graph half exists because unjira has no guaranteed run cadence. Between two
// collects a ticket legitimately traverses several statuses — sprint planning
// moves it to Ready for Dev outside unjira, work starts, and a small change is
// submitted the same day; interrupt-driven work skips planning entirely. One delta
// therefore holds evidence for a journey the live transition set cannot express,
// because that set only ever names the NEXT hop. Offering only that hop would make
// the In Review evidence in the same delta permanently unusable, and every later
// pass would re-derive the same single step, so unjira would trail reality by
// however many statuses the work crossed between runs.
//
// A nil graph yields the live set alone — exactly the pre-multi-hop behaviour, so
// an unavailable or unmined graph degrades to single-hop rather than to silence.
//
// Bounded by the graph, not merely extended by it: a status with no observed route
// from here is not offered, or the model could name anything in the project.
func reachableTargets(v verifiedLink, graph *workflow.Graph) []string {
	seen := make(map[string]bool)
	out := make([]string, 0, len(v.Transitions))

	current := normalizeName(v.Issue.StatusName)

	add := func(name string) {
		key := normalizeName(name)
		if key == "" || key == current || seen[key] {
			return
		}
		seen[key] = true
		out = append(out, name)
	}

	// Live-legal first: these are ground truth, and their names are the ones the
	// tracker will match on.
	for _, name := range v.targetNames() {
		add(name)
	}

	if graph == nil || v.Issue.StatusName == "" {
		return out
	}

	// The graph's spelling of the current status, for the same reason resolveRoute
	// needs it: Path matches exactly, and the live status comes from a different
	// endpoint than the mined graph.
	from, ok := canonicalStatus(graph, current)
	if !ok {
		// The graph has never observed this status, so it can offer no route from
		// here. The live set stands alone.
		return out
	}

	// Then anything the graph routes to. Path is consulted per status rather than
	// trusting Neighbors, because a two-hop destination is exactly the case this
	// exists for and Neighbors only sees one hop.
	for name := range graph.StatusCategories() {
		if graph.Path(from, name) == nil {
			continue
		}
		add(name)
	}

	return out
}

// resolveRoute returns the ordered hops that move the issue to target, excluding
// the status it is already at, and whether such a route exists.
//
// A single-hop transition is a one-element route: the common case is deliberately
// not a special case, so the applier has exactly one code path to walk.
//
// Two rules, in this order:
//
//  1. **The live set wins.** A target the tracker offers right now is legal
//     whether or not the mined graph has ever recorded that edge. The graph is
//     statistically observed history — a proxy for legality — and the live read is
//     ground truth, so refusing here on the strength of incomplete history would
//     suppress a legal move. This is also the spec's resolution of the
//     "what if Path returns nil" question.
//
//  2. **A graph route's first hop must be live-legal.** The graph says a route has
//     been walked before, not that it is walkable now. If its first hop is not
//     currently offered, the route cannot even start, and proposing it would queue
//     an action guaranteed to fail on its first write.
//
// Returned names are the graph's (or the live set's) canonical spelling, not the
// caller's: the applier passes each hop straight to a backend that matches on the
// name, so casing has to come from whoever the backend agrees with.
func resolveRoute(v verifiedLink, graph *workflow.Graph, target string) ([]string, bool) {
	want := normalizeName(target)
	if want == "" || want == normalizeName(v.Issue.StatusName) {
		return nil, false
	}

	// Rule 1.
	for _, name := range v.targetNames() {
		if normalizeName(name) == want {
			return []string{name}, true
		}
	}

	if graph == nil || v.Issue.StatusName == "" {
		return nil, false
	}

	// Both endpoints need the graph's own spelling: Path matches exactly, and
	// neither the caller's target nor the tracker's reported current status is
	// guaranteed to agree with the graph's casing (the graph is mined from
	// changelog history, the live status comes from a different endpoint).
	//
	// Canonicalizing only the target is a bug that passes every same-casing test —
	// the origin silently fails to match, Path returns nil, and a legal route reads
	// as unreachable.
	canonicalTarget, ok := canonicalStatus(graph, want)
	if !ok {
		return nil, false
	}

	canonicalFrom, ok := canonicalStatus(graph, normalizeName(v.Issue.StatusName))
	if !ok {
		// The graph has never seen this status. It cannot route from here, and
		// rule 1 above already covered anything the tracker offers directly.
		return nil, false
	}

	path := graph.Path(canonicalFrom, canonicalTarget)
	if len(path) < 2 {
		// nil (no observed route) or a single element (already there, which the
		// guard above should have caught).
		return nil, false
	}

	// Path is inclusive of both endpoints; the hops to execute are everything
	// after the current status.
	hops := path[1:]

	// Rule 2.
	if !offersName(v, hops[0]) {
		return nil, false
	}

	return hops, true
}

// canonicalStatus returns the graph's own spelling of the status whose normalized
// form is normalized, and whether the graph knows it at all.
func canonicalStatus(graph *workflow.Graph, normalized string) (string, bool) {
	for name := range graph.StatusCategories() {
		if normalizeName(name) == normalized {
			return name, true
		}
	}

	return "", false
}

// offersName reports whether the live transition set includes name.
func offersName(v verifiedLink, name string) bool {
	want := normalizeName(name)
	for _, offered := range v.targetNames() {
		if normalizeName(offered) == want {
			return true
		}
	}

	return false
}

// normalizeName folds a status name for comparison, matching how the Jira backend
// compares them (see jira.normalizeStatusName): trimmed and case-insensitive,
// because the name round-trips through a model response and a persisted payload,
// and refusing a legal move over casing would be a self-inflicted false negative.
func normalizeName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// dropUnroutable removes every drafted transition whose target could not be
// routed, saying why.
//
// A target with no route is not a proposal: the first write would fail, or in the
// stale-graph case there is no first write to attempt. Reporting it in Suppressed
// rather than silently discarding it is the difference between "unjira considered
// this and could not do it" and "unjira never thought of it" — and per the spec's
// nil-Path resolution, the reason names both facts the reviewer needs: what was
// wanted, and what the tracker actually offered.
//
// The routing itself happened in toProposedAction, because floorConfidence's
// verdict depends on it. This is the reporting half, kept separate so a
// suppression reason is built where the other suppression reasons are.
//
// Non-transition actions pass through untouched.
func dropUnroutable(
	verified []verifiedLink, drafted []ProposedAction,
) (routed []ProposedAction, suppressed []string) {
	byKey := make(map[string]verifiedLink, len(verified))
	for _, v := range verified {
		byKey[v.Link.IssueKey] = v
	}

	routed = make([]ProposedAction, 0, len(drafted))

	for _, action := range drafted {
		if action.Type != ActionTransition {
			routed = append(routed, action)

			continue
		}

		v, ok := byKey[action.IssueKey]
		if !ok {
			// Not a key this pass verified. actionsFromVerdicts already drops
			// those, so reaching here means a bug elsewhere — pass it through
			// rather than swallowing the evidence of it.
			routed = append(routed, action)

			continue
		}

		// The route was resolved in toProposedAction, where flooring needed it.
		// An empty one here means the target is unreachable — reported rather
		// than silently dropped, so "unjira considered this and could not do it"
		// stays distinguishable from "unjira never thought of it."
		if len(action.Route) == 0 {
			suppressed = append(suppressed, fmt.Sprintf(
				"transition %s -> %q: no route from %q — the tracker offers %s, and the "+
					"observed workflow graph has no path either",
				action.IssueKey, action.TargetStatus, v.Issue.StatusName,
				offeredLabel(v),
			))

			continue
		}

		routed = append(routed, action)
	}

	return routed, suppressed
}

// offeredLabel renders the live transition set for a suppression reason,
// distinguishing "nothing is currently legal" from a list — a reason a reviewer
// cannot act on is barely better than silence.
func offeredLabel(v verifiedLink) string {
	names := v.targetNames()
	if len(names) == 0 {
		return "no transitions at all right now"
	}

	return strings.Join(names, ", ")
}
