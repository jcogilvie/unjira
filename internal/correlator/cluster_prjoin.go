package correlator

// cluster_prjoin.go is the clustering pass's pull-request identity join over its own
// results (finding F61).
//
// "These events are the same pull request" is a fact about event structure, not a
// judgment. internal/pipeline/preassign.go acts on it BEFORE the model is called, but
// only for an event whose PR a stored narrative already holds: events of one PR that
// arrive in the same pass are left to the model, on the premise that it sees them
// together. A bisected window breaks that premise. clusterWithSplit clusters each half
// from that half's events alone, so a PR opened in one half and merged in the other is
// shown to two calls that cannot see each other, and each makes a NEW cluster for its
// part. mergeSplitResults joins only same-narrative extends and one NEW pair per seam,
// chosen by response order. On a real 30-day store's first pass (eight clustering
// calls), 19 of the 99 PRs with two or more member events ended in two narratives, and
// every one of them had events on both sides of a seam.
//
// So once the halves are merged, clusters whose MEMBER events share an exact
// events.ArtifactPullRequest are joined here, deterministically, before the dispute
// pass and before Persist. The same accessor as preassign.go (events.PullRequestOf), so
// the two cannot disagree on what counts as one pull request, and the same line: exact
// identity, never issue keys.
//
// Applied to every Cluster call's results, not only a bisected one's. A single call can
// also return two clusters for one PR, and nothing in a result says whether that was a
// considered split: the model was never asked about the identity, it only wrote its
// clusters. Left in place, such a split is permanent, because a PR two narratives hold
// is PRSeveralHolders to the pre-assignment join, whose later events then go back to
// the model. On a window that fits one call and keeps each PR whole, as the 80 PRs that
// stayed whole in the measurement did, the join changes nothing. That includes triage's
// split, the one caller whose split is deliberate: the reviewer judges that a narrative
// is several stories, but the model picks the seam, and a seam through one PR is the
// model's choice, so the join applies there too and the split reports it
// (triage.SplitNarrative).

import (
	"maps"
	"slices"

	"github.com/jcogilvie/unjira/internal/events"
)

// PRIdentityConflict is one group of clusters that share pull-request identities
// across two or more DIFFERENT stored narratives, which the join left apart.
//
// Joining them would merge stored narratives, which is a reviewer's call (a triage
// merge), not an identity rule's: it is the store's PRSeveralHolders situation, arising
// within one pass. Reported so the split is visible in the pass summary rather than
// discovered later as an ambiguous identity.
type PRIdentityConflict struct {
	// PullRequests is every identity the group's clusters share, ascending.
	PullRequests []string
	// Clusters labels every cluster in the group, in result order: "narrative <id>" for
	// an extend, "new <title>" for a new one.
	Clusters []string
}

// joinByPullRequest joins results whose member events share an exact pull-request
// identity, and reports what it joined and what it left apart. Pure: no model call, no
// store.
//
// Which members count: an event that is a member of exactly ONE result. A member two
// results claim is a dispute, and its PR drives no join, so that the dispute pass,
// which shows the model the PR as evidence and lets its answer stand, decides it rather
// than this join deciding it first by merging the claimants. A CONTEXT event's PR drives
// no join either: context is some other narrative's work, and sharing background is not
// sharing work.
//
// Grouping is transitive (union-find over results): A sharing PR 1 with B, and B
// sharing PR 2 with C, is one group of three. Each group of two or more joins into one
// result, by the group's extends:
//   - none: one NEW. Title, summary and confidence are the earliest result's. Response
//     order across a bisection is chronological, so that is the cluster holding the PR's
//     earliest events, its :opened and the `gh pr create` anchor, which name the work.
//     No model call is made to rewrite them, so the summary describes the part the
//     earliest cluster saw; it catches up the next time the model extends the narrative,
//     the same accepted cost as preassign.go's.
//   - one stored narrative (one or more results extending it): its earliest extend.
//     A stored narrative is the PR's existing home, so the NEW members join it, never
//     the other way round.
//   - two or more different stored narratives: left exactly as they are, NEW members
//     included, since which narrative those would join is the same guess. Reported as a
//     PRIdentityConflict.
//
// The joined result takes the position of the group's earliest result; the others are
// removed. Every member and context event is kept: members through absorb, which keeps
// each absorbed member's stated confidence as an override and drops a context event the
// joined result now holds as a member. An event a joined result and some other result
// both hold as a member stays in both, for the dispute pass.
func joinByPullRequest(results []ClusterResult) ([]ClusterResult, Stats) {
	var stats Stats

	groups := groupByPullRequest(results)
	if len(groups) == 0 {
		return results, stats
	}

	// Each group's result, keyed by the index of its earliest member, plus every
	// index absorbed into one.
	joined := make(map[int]ClusterResult)
	absorbed := make(map[int]bool)
	for _, g := range groups {
		target := g.indices[0]
		narratives := make(map[int64]bool)
		for _, i := range g.indices {
			if results[i].Kind != ClusterExtends {
				continue
			}
			if len(narratives) == 0 {
				target = i
			}
			narratives[results[i].NarrativeID] = true
		}

		if len(narratives) > 1 {
			labels := make([]string, 0, len(g.indices))
			for _, i := range g.indices {
				labels = append(labels, clusterLabel(results[i]))
			}
			stats.PRIdentityConflicts = append(stats.PRIdentityConflicts,
				PRIdentityConflict{PullRequests: g.pullRequests, Clusters: labels})

			continue
		}

		into := results[target]
		into.Events = slices.Clone(into.Events)
		into.ContextEvents = slices.Clone(into.ContextEvents)
		into.MemberConfidence = maps.Clone(into.MemberConfidence)
		for _, i := range g.indices {
			if i != target {
				absorb(&into, results[i].Events, results[i].ContextEvents, results[i].ConfidenceOf)
				stats.PRIdentityJoins++
			}
		}
		joined[g.indices[0]] = into
		for _, i := range g.indices[1:] {
			absorbed[i] = true
		}
	}

	out := make([]ClusterResult, 0, len(results)-stats.PRIdentityJoins)
	for i, r := range results {
		if absorbed[i] {
			continue
		}
		if j, ok := joined[i]; ok {
			r = j
		}
		out = append(out, r)
	}

	return out, stats
}

// prGroup is one set of results connected by shared pull-request identities.
type prGroup struct {
	// indices are the group's results, ascending.
	indices []int
	// pullRequests are the identities two or more of them share, ascending.
	pullRequests []string
}

// groupByPullRequest is every group of two or more results connected by a shared
// pull-request identity of an undisputed member (see joinByPullRequest), ascending by
// earliest index.
func groupByPullRequest(results []ClusterResult) []prGroup {
	holders := undisputedPRHolders(results)

	sets := newUnionFind(len(results))
	var shared []string
	for _, pr := range slices.Sorted(maps.Keys(holders)) {
		hs := holders[pr]
		if len(hs) < 2 {
			continue
		}
		shared = append(shared, pr)
		for _, i := range hs[1:] {
			sets.union(hs[0], i)
		}
	}
	if len(shared) == 0 {
		return nil
	}

	byRoot := make(map[int]*prGroup)
	var roots []int
	for i := range results {
		root := sets.find(i)
		g, ok := byRoot[root]
		if !ok {
			g = &prGroup{}
			byRoot[root] = g
			roots = append(roots, root)
		}
		g.indices = append(g.indices, i)
	}
	for _, pr := range shared {
		g := byRoot[sets.find(holders[pr][0])]
		g.pullRequests = append(g.pullRequests, pr)
	}

	var out []prGroup
	for _, root := range roots {
		if g := byRoot[root]; len(g.indices) > 1 {
			out = append(out, *g)
		}
	}

	return out
}

// undisputedPRHolders maps each pull-request identity to every result holding it
// through a member no other result also claims, ascending by index.
func undisputedPRHolders(results []ClusterResult) map[string][]int {
	claims := make(map[string]int)
	for _, r := range results {
		seen := make(map[string]bool, len(r.Events))
		for _, e := range r.Events {
			if k := EventKey(e); !seen[k] {
				seen[k] = true
				claims[k]++
			}
		}
	}

	holders := make(map[string][]int)
	for i, r := range results {
		for _, e := range r.Events {
			pr := events.PullRequestOf(e)
			if pr == "" || claims[EventKey(e)] != 1 {
				continue
			}
			if hs := holders[pr]; len(hs) == 0 || hs[len(hs)-1] != i {
				holders[pr] = append(hs, i)
			}
		}
	}

	return holders
}

// unionFind is a disjoint-set forest over result indices, whose root is always the
// set's smallest index, so a group's root is its earliest result.
type unionFind []int

func newUnionFind(n int) unionFind {
	u := make(unionFind, n)
	for i := range u {
		u[i] = i
	}

	return u
}

func (u unionFind) find(i int) int {
	if u[i] != i {
		u[i] = u.find(u[i])
	}

	return u[i]
}

func (u unionFind) union(a, b int) {
	ra, rb := u.find(a), u.find(b)
	u[max(ra, rb)] = min(ra, rb)
}
