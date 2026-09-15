package claudecode

// segments_test.go covers finding F15: a session was collapsed to one event dated to
// its LAST message, so a long session's work was undatable and its branches — the
// second-strongest attribution signal unjira has — were discarded except the final one.
//
// The measured case: session e951ef78 ran 2026-05-06 → 2026-07-16 (71 days, 139 user
// messages) and produced ONE event dated 2026-07-16. Inside it, `main` →
// `vp-feedback-refactor` → `mesh-routing-learnings`, and the middle segment ended
// 2026-07-09 — the same second as the merge that PAAS-3905 retro-credits. The collector
// kept only `mesh-routing-learnings`.
//
// Two mechanisms, deliberately combined, because measurement showed either alone is
// wrong:
//
//   - SLICING on branch change gives honest dates. But raw slicing over-splits: across
//     46 multi-branch sessions, 30 had more contiguous segments than distinct branches,
//     one with 3 branches producing 51 segments. Our own session showed a 3-minute,
//     16-line `rebase-probe` detour between two halves of one 435-line body of work. So
//     adjacent same-branch runs merge, and runs below a floor fold into their neighbour
//     rather than becoming peers of real work.
//   - THE BRANCH SET on every event gives clustering something to judge. 42 of 79
//     multi-day sessions never change branch at all, so slicing cannot help them;
//     carrying every branch seen means the correlator can still see what a session
//     touched.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// line builds one transcript line the way a real JSONL entry looks, reduced to the
// fields scanLines reads.
func line(ts, branch, cwd, kind, text string) map[string]any {
	l := map[string]any{"timestamp": ts, "type": kind}
	if branch != "" {
		l["gitBranch"] = branch
	}
	if cwd != "" {
		l["cwd"] = cwd
	}
	if text != "" {
		l["message"] = map[string]any{"content": text}
	}

	return l
}

// TestSegments_SplitsOnBranchChange is the finding's core: the middle of a long session
// must become its own event, dated to when THAT work ended rather than when the session
// went idle.
func TestSegments_SplitsOnBranchChange(t *testing.T) {
	lines := []map[string]any{
		line("2026-05-06T18:06:00Z", "main", "/w/vision", "user", "start the vision doc"),
		line("2026-06-09T22:14:00Z", "vp-feedback-refactor", "/w/vision", "user", "incorporate VP feedback"),
		line("2026-07-09T21:38:00Z", "vp-feedback-refactor", "/w/vision", "user", "merged to main"),
		line("2026-07-09T21:42:00Z", "mesh-routing-learnings", "/w/vision", "user", "now the routing pages"),
		line("2026-07-16T18:36:00Z", "mesh-routing-learnings", "/w/vision", "user", "split the doc up?"),
	}

	got := segments(lines, 0)

	require.Len(t, got, 3, "one segment per contiguous branch run")

	assert.Equal(t, "main", got[0].gitBranch)
	assert.Equal(t, "vp-feedback-refactor", got[1].gitBranch)
	assert.Equal(t, "2026-07-09T21:38:00Z", got[1].lastTS,
		"the middle segment must be dated to when THAT branch's work ended — 2026-07-09 is the "+
			"merge PAAS-3905 retro-credits, and the whole-session event was dated a week later")
	assert.Equal(t, "mesh-routing-learnings", got[2].gitBranch)
	assert.Equal(t, "2026-07-16T18:36:00Z", got[2].lastTS)
}

// TestSegments_MergesAdjacentRunsOfTheSameBranch is the over-splitting guard, and the
// case that made raw slicing untenable. Measured on our own session: a 3-minute
// `rebase-probe` detour sat between two runs of `live-retry-and-backlog-visibility`,
// which are one body of work interrupted, not two.
func TestSegments_MergesAdjacentRunsOfTheSameBranch(t *testing.T) {
	lines := []map[string]any{
		line("2026-09-14T18:56:00Z", "feature", "/w/u", "user", "the real work"),
		line("2026-09-14T21:31:00Z", "rebase-probe", "/w/u", "user", "quick probe"),
		line("2026-09-14T21:34:00Z", "feature", "/w/u", "user", "back to the work"),
		line("2026-09-14T21:42:00Z", "feature", "/w/u", "user", "still the work"),
	}

	// A floor of 2 messages drops the single-message probe, after which the two
	// `feature` runs are adjacent and merge.
	got := segments(lines, 2)

	require.Len(t, got, 1,
		"a short detour must not sever one body of work into two peers; 30 of 46 multi-branch "+
			"sessions had more segments than branches, so this is the common case, not the exception")
	assert.Equal(t, "feature", got[0].gitBranch)
	assert.Equal(t, "2026-09-14T21:42:00Z", got[0].lastTS, "and the merged span reaches the end")
}

// TestSegments_FoldsARunBelowTheFloor pins what happens to the dropped detour: its
// messages are not lost, they join the neighbouring segment. Discarding them would be
// silent data loss, which this codebase prefers to error over (see CLAUDE.md).
func TestSegments_FoldsARunBelowTheFloor(t *testing.T) {
	lines := []map[string]any{
		line("2026-09-14T10:00:00Z", "feature", "/w/u", "user", "one"),
		line("2026-09-14T10:01:00Z", "feature", "/w/u", "user", "two"),
		line("2026-09-14T10:02:00Z", "probe", "/w/u", "user", "tiny"),
	}

	got := segments(lines, 2)

	require.Len(t, got, 1)
	assert.Len(t, got[0].userTexts, 3,
		"the below-floor run's messages fold into the neighbour rather than vanishing")
	assert.Equal(t, "feature", got[0].gitBranch,
		"and the surviving segment keeps the branch that earned its size")
}

// TestSegments_KeepsASingleBranchSessionWhole is the 42-of-79 case: a long session that
// never changes branch. Slicing cannot help it, and must not invent boundaries.
func TestSegments_KeepsASingleBranchSessionWhole(t *testing.T) {
	lines := []map[string]any{
		line("2026-05-01T09:00:00Z", "long-lived", "/w/u", "user", "day one"),
		line("2026-05-20T09:00:00Z", "long-lived", "/w/u", "user", "day twenty"),
		line("2026-06-01T09:00:00Z", "long-lived", "/w/u", "user", "day thirty"),
	}

	got := segments(lines, 0)

	require.Len(t, got, 1, "no branch boundary means no split")
	assert.Equal(t, "2026-05-01T09:00:00Z", got[0].firstTS)
	assert.Equal(t, "2026-06-01T09:00:00Z", got[0].lastTS,
		"but the interval must be recorded, which is what lets a reader see it spans a month")
}

// TestSegments_IgnoresACwdChangeWithinOneBranch is the worktree question, settled by
// measurement rather than assumption. cwd changes mid-session in 5 of 164 transcripts,
// and 4 of those are a parent repo delegating to its OWN worktree — orchestration of one
// task, not a change of focus. The one genuine focus change was two sibling worktrees,
// and its branch changed too, so branch-splitting already catches it.
//
// Splitting on cwd would therefore fragment a single task and buy nothing.
func TestSegments_IgnoresACwdChangeWithinOneBranch(t *testing.T) {
	lines := []map[string]any{
		line("2026-09-14T10:00:00Z", "feature", "/w/unjira", "user", "main repo"),
		line("2026-09-14T10:05:00Z", "feature", "/w/unjira/.claude/worktrees/agent-x", "user", "delegated"),
		line("2026-09-14T10:10:00Z", "feature", "/w/unjira", "user", "back in the parent"),
	}

	got := segments(lines, 0)

	require.Len(t, got, 1,
		"entering a worktree from a session is orchestration of the same task; splitting on cwd "+
			"would shred it, and the only real focus change in the data also changed branch")

	// This test does NOT discriminate against a cwd-splitting implementation, and a drill
	// proved it: adding a cwd boundary produces byte-identical output here, because the
	// branch never changes and coalesceAdjacent merges the pieces straight back.
	//
	// That is a stronger result than the test guarding it. Same-branch runs are merged
	// unconditionally, so a worktree excursion cannot fragment a task no matter what other
	// boundary conditions are added — the property is structural rather than asserted.
	// These assertions pin the observable consequence instead: nothing is lost and the
	// interval spans the whole run.
	assert.Len(t, got[0].userTexts, 3, "every message survives")
	assert.Equal(t, "2026-09-14T10:00:00Z", got[0].firstTS)
	assert.Equal(t, "2026-09-14T10:10:00Z", got[0].lastTS,
		"and the span covers the whole run, worktree excursion included")
	assert.Equal(t, "/w/unjira", got[0].cwd,
		"the cwd settles on where the session returned, not where it detoured")
}

// TestSegments_CarriesEveryBranchSeen is the clustering-input half. 42 of 79 multi-day
// sessions never change branch, so slicing alone leaves them collapsed; and even a split
// session's segments are more judgeable when each knows what else the session touched.
//
// Deliberately the FULL set, not just the segment's own branch: the correlator's job is
// to decide whether three branches are one story, and it cannot weigh what it is not shown.
func TestSegments_CarriesEveryBranchSeen(t *testing.T) {
	lines := []map[string]any{
		line("2026-09-14T10:00:00Z", "alpha", "/w/u", "user", "one"),
		line("2026-09-14T11:00:00Z", "beta", "/w/u", "user", "two"),
		line("2026-09-14T12:00:00Z", "gamma", "/w/u", "user", "three"),
	}

	got := segments(lines, 0)
	require.Len(t, got, 3)

	for i, seg := range got {
		assert.Equal(t, []string{"alpha", "beta", "gamma"}, seg.allBranches,
			"segment %d must see every branch the session touched, in order", i)
	}
}

// TestSegments_TicketKeysAreScopedToTheSegment stops attribution leaking sideways. A key
// mentioned only while on branch `beta` must not appear as a candidate on `alpha`'s
// event — that would manufacture a link from work that never referenced the ticket, and
// gatherCandidates ranks prose mentions as real candidates.
func TestSegments_TicketKeysAreScopedToTheSegment(t *testing.T) {
	lines := []map[string]any{
		line("2026-09-14T10:00:00Z", "alpha", "/w/u", "user", "working on PROJ-1"),
		line("2026-09-14T11:00:00Z", "beta", "/w/u", "user", "now PROJ-2"),
	}

	got := segments(lines, 0)
	require.Len(t, got, 2)

	assert.Equal(t, []string{"PROJ-1"}, got[0].orderedKeys)
	assert.Equal(t, []string{"PROJ-2"}, got[1].orderedKeys,
		"a key mentioned on one branch must not become a candidate for another segment's work")
}

// TestSegments_EmptyTranscriptYieldsNothing pins the degenerate case, so callers need no
// special case for a transcript with no usable lines.
func TestSegments_EmptyTranscriptYieldsNothing(t *testing.T) {
	assert.Empty(t, segments(nil, 0))
	assert.Empty(t, segments([]map[string]any{{"type": "system"}}, 0),
		"lines with no timestamp and no branch carry nothing to segment on")
}
