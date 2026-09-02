package reconciler

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
	"github.com/jcogilvie/unjira/internal/workflow"
)

// paasGraph is the real PAAS workflow, mined 2026-09-01 via
// `dev workflow --project PAAS`, reduced to the statuses this file exercises.
//
// Real rather than invented because the whole question multi-hop answers is
// "does the observed graph supply a legal ordering for a journey the evidence
// already covers," and an invented graph would let a test agree with a route
// no real project has.
func paasGraph() *workflow.Graph {
	g := workflow.NewGraph()
	for name, category := range map[string]string{
		"Discovery":     "new",
		"Ready for Dev": "new",
		"In Progress":   "indeterminate",
		"In Review":     "indeterminate",
		"Blocked":       "indeterminate",
		"Done":          "done",
	} {
		g.AddStatus(name, category)
	}

	// Observed edges, with the real frequencies where the spec records them.
	for _, e := range []struct {
		from, to string
		times    int
	}{
		{"Discovery", "Ready for Dev", 40},
		{"Discovery", "In Progress", 80},
		{"Ready for Dev", "In Progress", 36},
		{"In Progress", "In Review", 28},
		{"In Progress", "Blocked", 9},
		{"In Review", "Done", 22},
		{"In Review", "In Progress", 6},
	} {
		for range e.times {
			g.Observe(e.from, e.to)
		}
	}

	return g
}

// routeLink builds a verifiedLink whose live state offers only the transitions
// named, which is what a real per-issue GET /transitions returns.
func routeLink(key, liveStatus string, offered ...string) verifiedLink {
	transitions := make([]tasktracker.Transition, 0, len(offered))
	for _, name := range offered {
		transitions = append(transitions, tasktracker.Transition{ToStatus: name})
	}

	return verifiedLink{
		Issue:       tasktracker.Issue{Key: key, StatusName: liveStatus},
		Transitions: transitions,
	}
}

// TestReachableTargets_IncludesGraphRoutesBeyondTheLiveSet is the headline.
//
// unjira has no guaranteed run cadence, so between two collects a ticket can
// legitimately traverse several statuses: sprint planning moves it to Ready for
// Dev outside unjira, work starts (evidence: In Progress), and a small change is
// submitted the same day (evidence: In Review). Interrupt-driven work skips the
// planning transition entirely. One delta therefore covers a journey the live
// transition set cannot express, because that set only ever names the NEXT hop.
//
// Without this, the model is offered only In Progress, proposes it, and the
// In Review evidence in the same delta is silently unusable — and the next pass
// re-derives the same single hop, so unjira permanently trails reality by however
// many statuses the work crossed between runs.
func TestReachableTargets_IncludesGraphRoutesBeyondTheLiveSet(t *testing.T) {
	v := routeLink("PAAS-1", "Ready for Dev", "In Progress", "Blocked")

	got := reachableTargets(v, paasGraph())

	assert.Contains(t, got, "In Progress", "the directly legal hop is still offered")
	assert.Contains(t, got, "In Review",
		"and so is a status the graph reaches via In Progress, which is where a "+
			"same-day submit lands")
}

// TestReachableTargets_LiveSetAloneWhenThereIsNoGraph: the graph is optional.
// Absent it, behaviour must be exactly what it was before multi-hop existed —
// only live-legal targets — rather than an empty set that would suppress every
// transition.
func TestReachableTargets_LiveSetAloneWhenThereIsNoGraph(t *testing.T) {
	v := routeLink("PAAS-1", "Ready for Dev", "In Progress", "Blocked")

	got := reachableTargets(v, nil)

	assert.ElementsMatch(t, []string{"In Progress", "Blocked"}, got)
}

// TestReachableTargets_ExcludesTheCurrentStatus: proposing a move to where the
// issue already is would be a no-op write, and Graph.Path returns a
// single-element path for from == to, so this is a real thing to get wrong.
func TestReachableTargets_ExcludesTheCurrentStatus(t *testing.T) {
	v := routeLink("PAAS-1", "In Progress", "In Review", "Blocked")

	got := reachableTargets(v, paasGraph())

	assert.NotContains(t, got, "In Progress", "the issue is already there")
}

// TestReachableTargets_ExcludesAnUnreachableStatus: the graph must bound what is
// offered, not merely extend it. A status with no observed route from here is not
// a candidate, or the model could name anything in the project.
func TestReachableTargets_ExcludesAnUnreachableStatus(t *testing.T) {
	g := paasGraph()
	g.AddStatus("Not-A-Risk", "done")

	// Reachable from In Review, but nothing routes back from Done.
	v := routeLink("PAAS-1", "Done", "In Review")

	got := reachableTargets(v, g)

	assert.NotContains(t, got, "Not-A-Risk",
		"a status with no observed route from here must not be offered")
}

// TestResolveRoute_ReturnsTheHopsForAMultiHopTarget: the applier needs the whole
// ordered path, because it walks it. Returning only the endpoint would leave the
// applier to re-derive the route with no graph of its own.
func TestResolveRoute_ReturnsTheHopsForAMultiHopTarget(t *testing.T) {
	v := routeLink("PAAS-1", "Ready for Dev", "In Progress")

	route, ok := resolveRoute(v, paasGraph(), "In Review")

	require.True(t, ok)
	assert.Equal(t, []string{"In Progress", "In Review"}, route,
		"the hops to EXECUTE, excluding the status the issue is already at")
}

// TestResolveRoute_ASingleHopIsAOneElementRoute: the common case must not be a
// special case. A direct transition is a route of length one, so the applier has
// exactly one code path.
func TestResolveRoute_ASingleHopIsAOneElementRoute(t *testing.T) {
	v := routeLink("PAAS-1", "Ready for Dev", "In Progress")

	route, ok := resolveRoute(v, paasGraph(), "In Progress")

	require.True(t, ok)
	assert.Equal(t, []string{"In Progress"}, route)
}

// TestResolveRoute_RefusesWhenTheFirstHopIsNotLiveLegal is the authority boundary,
// and the reason the graph plans rather than authorizes.
//
// The graph is statistically observed changelog history: it says a route has been
// walked before, not that it is walkable now. The live transition set is ground
// truth for the next hop. If the graph's first hop is not currently offered, the
// route cannot even start, so proposing it would queue an action guaranteed to
// fail on its first write.
func TestResolveRoute_RefusesWhenTheFirstHopIsNotLiveLegal(t *testing.T) {
	// The graph routes Ready for Dev -> In Progress -> In Review, but live state
	// offers only Blocked right now.
	v := routeLink("PAAS-1", "Ready for Dev", "Blocked")

	_, ok := resolveRoute(v, paasGraph(), "In Review")

	assert.False(t, ok,
		"a route whose first hop the tracker will not accept must not be proposed")
}

// TestResolveRoute_RefusesAnUnreachableTarget: no observed route means no route.
// Per the spec's resolution of the nil-Path question, the live set is consulted
// first (a directly-offered target is taken even with no graph edge), and only a
// target that is neither offered nor routed is refused.
func TestResolveRoute_RefusesAnUnreachableTarget(t *testing.T) {
	v := routeLink("PAAS-1", "Done", "In Review")

	_, ok := resolveRoute(v, paasGraph(), "Discovery")

	assert.False(t, ok)
}

// TestResolveRoute_PrefersTheLiveSetOverTheGraph is the stale-graph case, and the
// direction the spec chose deliberately.
//
// A target the tracker offers RIGHT NOW is legal whether or not the mined graph
// has ever seen that edge — the graph is a proxy for legality and the live read is
// ground truth. Refusing here because the graph lacks the edge would suppress a
// legal move on the strength of incomplete history.
func TestResolveRoute_PrefersTheLiveSetOverTheGraph(t *testing.T) {
	g := workflow.NewGraph()
	g.AddStatus("Ready for Dev", "new")
	g.AddStatus("In Progress", "indeterminate")
	// Deliberately no edges at all: a freshly-mined graph on a young project.

	v := routeLink("PAAS-1", "Ready for Dev", "In Progress")

	route, ok := resolveRoute(v, g, "In Progress")

	require.True(t, ok, "the tracker offers it, so it is legal regardless of the graph")
	assert.Equal(t, []string{"In Progress"}, route)
}

// TestResolveRoute_IsCaseInsensitive: the target round-trips through a model
// response and a persisted payload, and Jira treats status names as display
// strings with no casing guarantee. Refusing a legal route over casing would be a
// self-inflicted false negative — the same reasoning as SetStatus's own matching.
func TestResolveRoute_IsCaseInsensitive(t *testing.T) {
	v := routeLink("PAAS-1", "ready for dev", "In Progress")

	route, ok := resolveRoute(v, paasGraph(), "in review")

	require.True(t, ok)
	assert.Equal(t, []string{"In Progress", "In Review"}, route,
		"the route carries the graph's canonical names, not the model's casing")
}

// TestReconcile_ProposesAMultiHopTransitionEndToEnd drives the real path — store,
// verifyLinks, the graph-aware prompt, resolveRoutes, and the staleness guard —
// rather than calling the route helpers directly.
//
// This is the scenario multi-hop exists for, and it can only be tested here: the
// unit tests above can all pass while the graph never reaches drafting, because
// that requires WithWorkflowGraph to be threaded through Reconcile ->
// reconcileOne -> draft, and resolveRoutes to run before the other filters. A
// previous session lost hours to exactly this gap — targeted unit tests green
// while the pipeline disagreed.
//
// The ticket is at Ready for Dev (sprint planning moved it there, outside unjira).
// One delta holds both work-started and PR-opened evidence, because unjira has no
// guaranteed run cadence and this was a same-day change. The live tracker offers
// only In Progress. Without multi-hop the In Review evidence is unusable forever.
func TestReconcile_ProposesAMultiHopTransitionEndToEnd(t *testing.T) {
	s := reconcileStore(t)

	nid := seedLinkedNarrative(t, s, "PAAS-1", store.Role("primary"),
		codeEvent("cc:1", "implemented the fix and opened a PR"))

	// The last status change predates our work, so the staleness guard permits a
	// proposal — otherwise this test would pass for the wrong reason.
	entered := time.Date(2026, 8, 25, 8, 0, 0, 0, time.UTC)
	insertStatusEvent(t, s, "PAAS-1:status:1", "PAAS-1", "Discovery", "Ready for Dev", entered)

	tracker := &fakeTracker{
		issues: map[string]tasktracker.Issue{
			"PAAS-1": {Key: "PAAS-1", Summary: "the ticket", StatusName: "Ready for Dev"},
		},
		transitions: map[string][]tasktracker.Transition{
			// Only the next hop, which is all a real GET /transitions ever returns.
			"PAAS-1": {{ToStatus: "In Progress", ToCategory: tasktracker.StatusInProgress}},
		},
	}

	client := &fakeLLM{responses: []string{
		`[{"issue_key":"PAAS-1","type":"transition","target_status":"In Review",` +
			`"confidence":0.9,"rationale":"work is done and a PR is open"}]`,
	}}

	got, _, err := Reconcile(t.Context(), s, tracker, client, testConfig(),
		WithWorkflowGraph(paasGraph()))

	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, nid, got[0].NarrativeID)

	require.Len(t, got[0].Proposed, 1,
		"the graph routes Ready for Dev -> In Progress -> In Review, so the "+
			"evidence for both hops is usable in one action")
	action := got[0].Proposed[0]
	assert.Equal(t, ActionTransition, action.Type)
	assert.Equal(t, "In Review", action.TargetStatus)
	assert.Equal(t, []string{"In Progress", "In Review"}, action.Route,
		"one approval, and the applier walks both hops")
	assert.Positive(t, action.Confidence,
		"floorConfidence must not zero a routed action just because its target is "+
			"not directly offered — that would silently undo multi-hop")

	// The prompt has to have OFFERED In Review, or the model naming it was luck.
	require.Len(t, client.prompts, 1)
	assert.Contains(t, client.prompts[0], `"In Review"`,
		"a target the model cannot see is a target it cannot propose")
}

// TestReconcile_WithoutAGraphStaysSingleHop is the other half. Absent a graph,
// behaviour must be exactly what it was before multi-hop: the model is offered
// only the live-legal set, and a target outside it is refused with a reason.
//
// Without this, a bug that silently ignored the nil case would pass every test
// above — and every automated test uses the local backend, whose graph is static.
func TestReconcile_WithoutAGraphStaysSingleHop(t *testing.T) {
	s := reconcileStore(t)

	seedLinkedNarrative(t, s, "PAAS-1", store.Role("primary"),
		codeEvent("cc:1", "implemented the fix and opened a PR"))

	entered := time.Date(2026, 8, 25, 8, 0, 0, 0, time.UTC)
	insertStatusEvent(t, s, "PAAS-1:status:1", "PAAS-1", "Discovery", "Ready for Dev", entered)

	tracker := &fakeTracker{
		issues: map[string]tasktracker.Issue{
			"PAAS-1": {Key: "PAAS-1", Summary: "the ticket", StatusName: "Ready for Dev"},
		},
		transitions: map[string][]tasktracker.Transition{
			"PAAS-1": {{ToStatus: "In Progress", ToCategory: tasktracker.StatusInProgress}},
		},
	}

	client := &fakeLLM{responses: []string{
		`[{"issue_key":"PAAS-1","type":"transition","target_status":"In Review",` +
			`"confidence":0.9,"rationale":"work is done and a PR is open"}]`,
	}}

	// No WithWorkflowGraph.
	got, _, err := Reconcile(t.Context(), s, tracker, client, testConfig())

	require.NoError(t, err)
	require.Len(t, got, 1)

	assert.Empty(t, got[0].Proposed,
		"with no graph there is no route to In Review, so nothing is proposed")
	require.Len(t, got[0].Suppressed, 1)
	assert.Contains(t, got[0].Suppressed[0], "no route from",
		"and the reason must name what was wanted and what was offered, or "+
			"'nothing proposed' is indistinguishable from 'nothing considered'")
	assert.Contains(t, got[0].Suppressed[0], "In Progress",
		"the offered set is the actionable half of that reason")

	// The prompt must NOT have offered In Review — that is what "single-hop"
	// means, and asserting it here is what makes this test about the graph's
	// absence rather than about the model's whim.
	require.Len(t, client.prompts, 1)
	assert.NotContains(t, client.prompts[0], `"In Review"`)
}
