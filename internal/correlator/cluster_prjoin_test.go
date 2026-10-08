package correlator_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/events"
)

// prJoinBase is the fixed clock the pull-request identity join tests build events on.
var prJoinBase = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// prEvent is a github-shaped event carrying pr as its exact identity ("" for none).
func prEvent(id, pr string, offset time.Duration) correlator.Event {
	e := events.NewEvent("github", id, prJoinBase.Add(offset), "event "+id)
	if pr != "" {
		e.Artifacts[events.ArtifactPullRequest] = pr
	}

	return e
}

// resultBuilder builds a ClusterResult for the join tests, defaulting to a NEW
// cluster at confidence 0.9 with no events.
type resultBuilder struct{ r correlator.ClusterResult }

func newResult(title string) *resultBuilder {
	return &resultBuilder{r: correlator.ClusterResult{
		Kind: correlator.ClusterNew, Title: title, Summary: "summary of " + title, Confidence: 0.9,
	}}
}

func (b *resultBuilder) extends(id int64) *resultBuilder {
	b.r.Kind = correlator.ClusterExtends
	b.r.NarrativeID = id

	return b
}

func (b *resultBuilder) confidence(c float64) *resultBuilder {
	b.r.Confidence = c

	return b
}

func (b *resultBuilder) members(evts ...correlator.Event) *resultBuilder {
	b.r.Events = append(b.r.Events, evts...)

	return b
}

func (b *resultBuilder) context(evts ...correlator.Event) *resultBuilder {
	b.r.ContextEvents = append(b.r.ContextEvents, evts...)

	return b
}

func (b *resultBuilder) build() correlator.ClusterResult { return b.r }

// everyEventKept asserts the join's no-loss guarantee: every member and context
// event of the input appears in the output, as a member if it was a member anywhere.
func everyEventKept(t *testing.T, in, out []correlator.ClusterResult) {
	t.Helper()

	members := make(map[string]bool)
	all := make(map[string]bool)
	for _, r := range out {
		for _, e := range r.Events {
			members[correlator.EventKey(e)] = true
			all[correlator.EventKey(e)] = true
		}
		for _, e := range r.ContextEvents {
			all[correlator.EventKey(e)] = true
		}
	}

	for _, r := range in {
		for _, e := range r.Events {
			assert.True(t, members[correlator.EventKey(e)], "member %s/%s survives the join as a member", e.Source, e.ExternalID)
		}
		for _, e := range r.ContextEvents {
			assert.True(t, all[correlator.EventKey(e)], "context event %s/%s survives the join", e.Source, e.ExternalID)
		}
	}
}

const (
	pr1 = "github.com/o/r#1"
	pr2 = "github.com/o/r#2"
)

func TestJoinByPullRequest_TwoNewClustersSharingAPullRequestBecomeOne(t *testing.T) {
	opened := prEvent("pr-1-opened", pr1, 0)
	segment := prEvent("segment", "", time.Minute)
	merged := prEvent("pr-1-merged", pr1, 3*time.Hour)
	unrelated := prEvent("unrelated", "", 4*time.Hour)
	in := []correlator.ClusterResult{
		newResult("Open PR 1").members(opened, segment).build(),
		newResult("Other work").members(unrelated).build(),
		newResult("Merge PR 1").confidence(0.6).members(merged).build(),
	}

	out, stats := correlator.JoinByPullRequestForTest(in)

	require.Len(t, out, 2)
	joined := out[0]
	assert.Equal(t, correlator.ClusterNew, joined.Kind)
	assert.Equal(t, "Open PR 1", joined.Title, "the earliest cluster's title and summary are kept")
	assert.Equal(t, "summary of Open PR 1", joined.Summary)
	assert.Equal(t, []string{"github/pr-1-opened", "github/segment", "github/pr-1-merged"}, idsOf(joined.Events))
	assert.InDelta(t, 0.9, joined.ConfidenceOf(opened), 1e-9)
	assert.InDelta(t, 0.6, joined.ConfidenceOf(merged), 1e-9, "the model's confidence for the absorbed member is kept")
	assert.Equal(t, "Other work", out[1].Title, "an unrelated cluster is untouched")
	assert.Equal(t, 1, stats.PRIdentityJoins)
	assert.Empty(t, stats.PRIdentityConflicts)
	everyEventKept(t, in, out)
}

func TestJoinByPullRequest_NewClusterJoinsTheExtendsHoldingItsPullRequest(t *testing.T) {
	opened := prEvent("pr-1-opened", pr1, 0)
	merged := prEvent("pr-1-merged", pr1, 3*time.Hour)
	in := []correlator.ClusterResult{
		newResult("Merge PR 1").members(merged).build(),
		newResult("Cache rework").extends(9).confidence(0.8).members(opened).build(),
	}

	out, stats := correlator.JoinByPullRequestForTest(in)

	require.Len(t, out, 1)
	assert.Equal(t, correlator.ClusterExtends, out[0].Kind, "an existing narrative is the join's home, never a new one")
	assert.Equal(t, int64(9), out[0].NarrativeID)
	assert.Equal(t, "Cache rework", out[0].Title)
	assert.ElementsMatch(t, []string{"github/pr-1-opened", "github/pr-1-merged"}, idsOf(out[0].Events))
	assert.InDelta(t, 0.9, out[0].ConfidenceOf(merged), 1e-9)
	assert.InDelta(t, 0.8, out[0].ConfidenceOf(opened), 1e-9)
	assert.Equal(t, 1, stats.PRIdentityJoins)
	everyEventKept(t, in, out)
}

func TestJoinByPullRequest_IsTransitive(t *testing.T) {
	// A and C share nothing directly; B shares one PR with each. Both orders of the
	// links matter: identities are visited in sorted order, so in the second case the
	// B-C link is made before the A-B link that has to carry C along with it.
	tests := []struct {
		name         string
		aPR, cPR     string
		wantEventIDs []string
	}{
		{"A-B link made first", pr1, pr2, []string{"github/a", "github/b1", "github/b2", "github/c"}},
		{"B-C link made first", pr2, pr1, []string{"github/a", "github/b1", "github/b2", "github/c"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := []correlator.ClusterResult{
				newResult("A").members(prEvent("a", tt.aPR, 0)).build(),
				newResult("B").members(prEvent("b1", tt.aPR, time.Hour), prEvent("b2", tt.cPR, time.Hour)).build(),
				newResult("C").members(prEvent("c", tt.cPR, 2*time.Hour)).build(),
			}

			out, stats := correlator.JoinByPullRequestForTest(in)

			require.Len(t, out, 1)
			assert.Equal(t, "A", out[0].Title)
			assert.Equal(t, tt.wantEventIDs, idsOf(out[0].Events))
			assert.Equal(t, 2, stats.PRIdentityJoins)
			everyEventKept(t, in, out)
		})
	}
}

func TestJoinByPullRequest_TwoExistingNarrativesSharingAPullRequestAreLeftAndReported(t *testing.T) {
	in := []correlator.ClusterResult{
		newResult("nine").extends(9).members(prEvent("pr-1-opened", pr1, 0)).build(),
		newResult("New bit").members(prEvent("pr-1-review", pr1, time.Hour)).build(),
		newResult("twelve").extends(12).members(prEvent("pr-1-merged", pr1, 2*time.Hour)).build(),
	}

	out, stats := correlator.JoinByPullRequestForTest(in)

	assert.Equal(t, in, out, "merging two stored narratives is not the join's call; nothing in the component moves")
	assert.Zero(t, stats.PRIdentityJoins)
	require.Len(t, stats.PRIdentityConflicts, 1)
	assert.Equal(t, correlator.PRIdentityConflict{
		PullRequests: []string{pr1},
		Clusters:     []string{"narrative 9", `new "New bit"`, "narrative 12"},
	}, stats.PRIdentityConflicts[0])
	everyEventKept(t, in, out)
}

func TestJoinByPullRequest_TwoResultsExtendingOneNarrativeJoin(t *testing.T) {
	in := []correlator.ClusterResult{
		newResult("nine").extends(9).members(prEvent("pr-1-opened", pr1, 0)).build(),
		newResult("nine again").extends(9).members(prEvent("pr-1-merged", pr1, 2*time.Hour)).build(),
	}

	out, stats := correlator.JoinByPullRequestForTest(in)

	require.Len(t, out, 1, "one narrative, so there is nothing to choose between")
	assert.Equal(t, "nine", out[0].Title)
	assert.Equal(t, 1, stats.PRIdentityJoins)
	assert.Empty(t, stats.PRIdentityConflicts)
	everyEventKept(t, in, out)
}

func TestJoinByPullRequest_ContextDoesNotDriveAJoin(t *testing.T) {
	opened := prEvent("pr-1-opened", pr1, 0)
	in := []correlator.ClusterResult{
		newResult("Open PR 1").members(opened).build(),
		// B holds PR 1 only as background: someone else's work.
		newResult("B").members(prEvent("b", "", time.Hour)).context(opened).build(),
	}

	out, stats := correlator.JoinByPullRequestForTest(in)

	assert.Equal(t, in, out)
	assert.Zero(t, stats.PRIdentityJoins)
	everyEventKept(t, in, out)
}

func TestJoinByPullRequest_AJoinedMemberIsNoLongerContext(t *testing.T) {
	opened := prEvent("pr-1-opened", pr1, 0)
	merged := prEvent("pr-1-merged", pr1, 3*time.Hour)
	background := prEvent("background", "", time.Hour)
	in := []correlator.ClusterResult{
		newResult("Open PR 1").members(opened).context(merged, background).build(),
		newResult("Merge PR 1").members(merged).context(background).build(),
	}

	out, _ := correlator.JoinByPullRequestForTest(in)

	require.Len(t, out, 1)
	assert.Equal(t, []string{"github/pr-1-opened", "github/pr-1-merged"}, idsOf(out[0].Events))
	assert.Equal(t, []string{"github/background"}, idsOf(out[0].ContextEvents),
		"a member is never also context of its own cluster, and context is not doubled")
	everyEventKept(t, in, out)
}

// A disputed event — a member of two clusters — is not settled membership, so its
// pull request drives no join; the dispute pass decides it, with the PR shown as
// evidence. Here B holds PR 1 only through the disputed event.
func TestJoinByPullRequest_ADisputedMemberDoesNotDriveAJoin(t *testing.T) {
	opened := prEvent("pr-1-opened", pr1, 0)
	disputed := prEvent("pr-1-review", pr1, time.Hour)
	in := []correlator.ClusterResult{
		newResult("Open PR 1").members(opened, disputed).build(),
		newResult("nine").extends(9).members(disputed, prEvent("cache", "", 2*time.Hour)).build(),
	}

	out, stats := correlator.JoinByPullRequestForTest(in)

	assert.Equal(t, in, out, "the double placement reaches the dispute pass unchanged")
	assert.Zero(t, stats.PRIdentityJoins)
}

// After a join, an event that two clusters still both claim is left for the dispute
// pass: the join unions, it never picks a claimant.
func TestJoinByPullRequest_LeavesADoublePlacementForTheDisputePass(t *testing.T) {
	shared := prEvent("investigation", "", 30*time.Minute)
	in := []correlator.ClusterResult{
		newResult("Open PR 1").members(prEvent("pr-1-opened", pr1, 0)).build(),
		newResult("Merge PR 1").members(prEvent("pr-1-merged", pr1, 3*time.Hour), shared).build(),
		newResult("nine").extends(9).members(shared).build(),
	}

	out, stats := correlator.JoinByPullRequestForTest(in)

	require.Len(t, out, 2)
	assert.Contains(t, idsOf(out[0].Events), "github/investigation")
	assert.Contains(t, idsOf(out[1].Events), "github/investigation")
	assert.Equal(t, 1, stats.PRIdentityJoins)
	everyEventKept(t, in, out)
}

func TestJoinByPullRequest_LeavesAHostlessPullRequestAlone(t *testing.T) {
	// The pre-host-qualification spelling cannot tell two hosts apart; it joins nothing,
	// as it joins nothing in the pre-assignment join and the dispute pass.
	in := []correlator.ClusterResult{
		newResult("A").members(prEvent("a", "o/r#1", 0)).build(),
		newResult("B").members(prEvent("b", "o/r#1", time.Hour)).build(),
	}

	out, stats := correlator.JoinByPullRequestForTest(in)

	assert.Equal(t, in, out)
	assert.Zero(t, stats.PRIdentityJoins)
}

func TestStatsAdd_SumsPullRequestIdentityJoins(t *testing.T) {
	s := correlator.Stats{PRIdentityJoins: 2, PRIdentityConflicts: []correlator.PRIdentityConflict{{PullRequests: []string{pr1}}}}
	s.Add(correlator.Stats{PRIdentityJoins: 3, PRIdentityConflicts: []correlator.PRIdentityConflict{{PullRequests: []string{pr2}}}})

	assert.Equal(t, 5, s.PRIdentityJoins)
	assert.Len(t, s.PRIdentityConflicts, 2)
}

// TestCluster_BisectedPullRequestIsOneCluster is F61 end to end: the window is too
// big for one call, so each half clusters only its own events, and each makes a NEW
// cluster for its part of PR 1. The model's same-story check at the seam says no.
// Exact identity still joins them.
func TestCluster_BisectedPullRequestIsOneCluster(t *testing.T) {
	evts := []correlator.Event{
		prEvent("pr-1-opened", pr1, 0),
		prEvent("pr-1-merged", pr1, time.Hour),
	}
	for i := range evts {
		evts[i].Summary = strings.Repeat("x", 400)
	}
	client := &fakeLLM{responses: []string{
		`[{"kind":"new","title":"Open PR 1","summary":"s1","confidence":0.9,"event_indices":[0]}]`,
		`[{"kind":"new","title":"Merge PR 1","summary":"s2","confidence":0.7,"event_indices":[0]}]`,
		`{"same_story":false}`,
	}}

	results, stats, err := correlator.Cluster(t.Context(), evts, nil, client, correlator.TimeRange{
		Start: prJoinBase, End: prJoinBase.Add(2 * time.Hour),
	}, budgetTooSmallFor(t, evts, nil))

	require.NoError(t, err)
	assert.Equal(t, 1, stats.Splits, "the window was bisected")
	require.Len(t, results, 1, "one pull request, one cluster")
	assert.Equal(t, "Open PR 1", results[0].Title)
	assert.Equal(t, []string{"github/pr-1-opened", "github/pr-1-merged"}, idsOf(results[0].Events))
	assert.InDelta(t, 0.7, results[0].ConfidenceOf(evts[1]), 1e-9)
	assert.Equal(t, 1, stats.PRIdentityJoins)
}

// A single call can return two clusters for one PR as well; the join applies there
// too, since nothing distinguishes that split from a bisection's.
func TestCluster_OneCallsTwoClustersForOnePullRequestJoin(t *testing.T) {
	evts := []correlator.Event{
		prEvent("pr-1-opened", pr1, 0),
		prEvent("pr-1-merged", pr1, time.Hour),
	}
	client := &fakeLLM{responses: []string{
		`[{"kind":"new","title":"Open PR 1","summary":"s1","confidence":0.9,"event_indices":[0]},` +
			`{"kind":"new","title":"Merge PR 1","summary":"s2","confidence":0.9,"event_indices":[1]}]`,
	}}

	results, stats, err := correlator.Cluster(t.Context(), evts, nil, client, correlator.TimeRange{
		Start: prJoinBase, End: prJoinBase.Add(2 * time.Hour),
	}, 128000)

	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, 1, stats.PRIdentityJoins)
}
