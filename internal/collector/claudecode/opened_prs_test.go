package claudecode_test

// opened_prs_test.go covers a segment naming the pull requests its own run opened.
//
// The motivating measurement (shared-context spec, slice 1): the root segment that did
// the work behind #69–#74 summarized itself as "Did: committed, created a branch, opened
// a PR, ..." and nothing more, so the clustering model, which reads only summaries,
// could not see which narratives that segment's work touched and attached it as context
// to none of them. The anchors already know exactly which PRs the run opened; this puts
// that fact where the model can read it.

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func onlySegment(t *testing.T, root string, options map[string]any) string {
	t.Helper()

	_, got := collect(t, root, options)
	segs := segmentEvents(got)
	require.Len(t, segs, 1)

	return segs[0].Summary
}

func TestSegmentSummary_NamesThePullRequestsItsRunOpened(t *testing.T) {
	root := t.TempDir()
	prSession(t).
		Bash("2026-09-20T10:01:00Z", "toolu_01A", `gh pr create --title "a"`).
		ToolResult("2026-09-20T10:01:05Z", "toolu_01A", "https://github.com/jcogilvie/unjira/pull/76\n", false).
		ToolUse("2026-09-20T10:02:00Z", "toolu_01B", "mcp__github__create_pull_request", map[string]any{
			"owner": "Sanyaku", "repo": "helm-charts", "head": "x", "title": "b",
		}).
		ToolResult("2026-09-20T10:02:02Z", "toolu_01B", []any{
			map[string]any{"type": "text", "text": `{"url":"https://github.com/Sanyaku/helm-charts/pull/445"}`},
		}, false).
		WriteRoot(root, slug, parentSession)

	got := onlySegment(t, root, nil)

	assert.Contains(t, got, "Opened pull requests: jcogilvie/unjira#76, Sanyaku/helm-charts#445.",
		"in call order, spelled as the github collector's summaries spell them, so the model can match the two")
}

// TestSegmentSummary_NamesOnlyItsOwnRunsPullRequests: a PR opened on one branch is not
// evidence about another segment's work — the scoping rule keys and facts already follow.
func TestSegmentSummary_NamesOnlyItsOwnRunsPullRequests(t *testing.T) {
	root := t.TempDir()
	b := newTranscript(t).InCwd("/w/unjira").OnBranch("first").
		User("2026-09-20T10:00:00Z", "open the first").
		Bash("2026-09-20T10:01:00Z", "toolu_01First", `gh pr create --title "first"`).
		ToolResult("2026-09-20T10:01:05Z", "toolu_01First", "https://github.com/o/r/pull/1\n", false)
	b.OnBranch("second").
		User("2026-09-20T11:00:00Z", "open the second").
		Bash("2026-09-20T11:01:00Z", "toolu_01Second", `gh pr create --title "second"`).
		ToolResult("2026-09-20T11:01:05Z", "toolu_01Second", "https://github.com/o/r/pull/2\n", false).
		WriteRoot(root, slug, parentSession)

	_, got := collect(t, root, map[string]any{"min_segment_messages": 1})
	segs := segmentEvents(got)
	require.Len(t, segs, 2)

	assert.Contains(t, segs[0].Summary, "Opened pull requests: o/r#1.")
	assert.NotContains(t, segs[0].Summary, "o/r#2")
	assert.Contains(t, segs[1].Summary, "Opened pull requests: o/r#2.")
	assert.NotContains(t, segs[1].Summary, "o/r#1")
}

// TestSegmentSummary_NamesNoPullRequestItCannotConfirm: only an anchor whose result
// named exactly one PR claims an opened PR, and the segment claims no more than the
// anchor does — a failed call, a result with no URL, and a call still awaiting its
// result all leave the clause out.
func TestSegmentSummary_NamesNoPullRequestItCannotConfirm(t *testing.T) {
	root := t.TempDir()
	prSession(t).
		Bash("2026-09-20T10:01:00Z", "toolu_01Err", `gh pr create --title "x"`).
		ToolResult("2026-09-20T10:01:05Z", "toolu_01Err", "https://github.com/o/r/pull/9 rejected", true).
		Bash("2026-09-20T10:02:00Z", "toolu_01NoURL", `gh pr create --title "y" 2>&1 | tail -1`).
		ToolResult("2026-09-20T10:02:05Z", "toolu_01NoURL", "GraphQL: No commits between main and x", false).
		Bash("2026-09-20T10:03:00Z", "toolu_01Pending", `gh pr create --title "z"`).
		WriteRoot(root, slug, parentSession)

	got := onlySegment(t, root, nil)

	assert.NotContains(t, got, "Opened pull requests")
	assert.NotContains(t, got, "o/r#9")
}

// TestSegmentSummary_BoundsTheNamedPullRequestsAndSaysSo: a run that loops `gh pr create`
// can open any number of PRs. The summary names a bounded set and states how many it
// left out, so the count is never silently lower than the truth; each anchor still names
// its own PR in full. The bound sits above every measured segment (max 19 across all
// projects' transcripts), so in practice it never cuts.
func TestSegmentSummary_BoundsTheNamedPullRequestsAndSaysSo(t *testing.T) {
	root := t.TempDir()
	b := prSession(t)
	for i := 1; i <= 27; i++ {
		id := fmt.Sprintf("toolu_01Fan%02d", i)
		b.Bash("2026-09-20T10:01:00Z", id, `gh pr create --title "mirror"`).
			ToolResult("2026-09-20T10:01:05Z", id, fmt.Sprintf("https://github.com/o/r/pull/%d\n", i), false)
	}
	b.WriteRoot(root, slug, parentSession)

	got := onlySegment(t, root, nil)

	assert.Contains(t, got, "o/r#19, ", "the largest measured segment must be named in full")
	assert.Contains(t, got, ", o/r#25 and 2 more.")
	assert.NotContains(t, got, "o/r#26")
}

func TestSegmentSummary_NoClauseWhenTheRunOpenedNoPullRequest(t *testing.T) {
	root := t.TempDir()
	prSession(t).WriteRoot(root, slug, parentSession)

	assert.NotContains(t, onlySegment(t, root, nil), "Opened pull requests")
}

// TestSegmentSummary_NamesAPullRequestOnceHoweverItIsSpelled: a resumed session can
// carry the same PR under two calls, and owner/repo case is not identity — the
// host-qualified key is.
func TestSegmentSummary_NamesAPullRequestOnceHoweverItIsSpelled(t *testing.T) {
	root := t.TempDir()
	prSession(t).
		Bash("2026-09-20T10:01:00Z", "toolu_01Upper", `gh pr create --title "a"`).
		ToolResult("2026-09-20T10:01:05Z", "toolu_01Upper", "https://github.com/Sanyaku/helm-charts/pull/7\n", false).
		Bash("2026-09-20T10:02:00Z", "toolu_01Lower", `gh pr view 7`).
		Bash("2026-09-20T10:03:00Z", "toolu_01Again", `gh pr create --title "a"`).
		ToolResult("2026-09-20T10:03:05Z", "toolu_01Again", "https://github.com/sanyaku/helm-charts/pull/7\n", false).
		WriteRoot(root, slug, parentSession)

	got := onlySegment(t, root, nil)

	assert.Contains(t, got, "Opened pull requests: Sanyaku/helm-charts#7.")
}

// TestSegmentSummary_TruncatesOnACharacterBoundary: the opening line was cut by bytes,
// which split a multi-byte character. Measured: 3 stored summaries were invalid UTF-8,
// each a box-drawing or punctuation character cut in half just before the "...".
func TestSegmentSummary_TruncatesOnACharacterBoundary(t *testing.T) {
	root := t.TempDir()
	newTranscript(t).InCwd("/w/unjira").
		User("2026-09-20T10:00:00Z", "ab"+strings.Repeat("│", 200)).
		WriteRoot(root, slug, parentSession)

	got := onlySegment(t, root, nil)

	assert.True(t, utf8.ValidString(got), "summary must be valid UTF-8: %q", got)
	assert.Contains(t, got, `│..."`)
}

// TestAnchorSummary_TruncatesTheTitleOnACharacterBoundary: the same cut, in the anchor's
// title.
func TestAnchorSummary_TruncatesTheTitleOnACharacterBoundary(t *testing.T) {
	root := t.TempDir()
	prSession(t).
		Bash("2026-09-20T10:01:00Z", "toolu_01Long", `gh pr create --title "ab`+strings.Repeat("—", 200)+`"`).
		ToolResult("2026-09-20T10:01:05Z", "toolu_01Long", "https://github.com/o/r/pull/1\n", false).
		WriteRoot(root, slug, parentSession)

	_, got := collect(t, root, nil)

	a := onlyAnchor(t, got)
	assert.True(t, utf8.ValidString(a.Summary), "summary must be valid UTF-8: %q", a.Summary)
}
