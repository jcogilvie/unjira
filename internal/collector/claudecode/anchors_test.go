package claudecode_test

// anchors_test.go covers the pull-request-creation anchor: one event per tool call that
// creates a PR, so the transcript's side of the fact GitHub records as `:opened` is a
// discrete event rather than a phrase inside a segment summary.
//
// Fixture shapes are the measured ones: across 244 real PR-creating calls (177
// `gh pr create`, 67 mcp__github__create_pull_request), every call had a paired
// tool_result; 216 named exactly one PR URL; 12 were errors (mostly the user rejecting
// the call); 14 "succeeded" with no URL because `2>&1 | tail` hid a GraphQL failure;
// and two named several PRs — a `for` loop over 8 repos and a shell function called 4
// times.

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/events"
)

func anchors(evts []events.Event) []events.Event {
	return eventsWithArtifact(evts, "anchor_kind", "pr_create")
}

func onlyAnchor(t *testing.T, evts []events.Event) events.Event {
	t.Helper()

	got := anchors(evts)
	require.Len(t, got, 1)

	return got[0]
}

func prSession(t *testing.T) *transcriptBuilder {
	t.Helper()

	return newTranscript(t).InCwd("/w/unjira").User("2026-09-20T10:00:00Z", "open the PR")
}

func TestAnchor_ResolvesOwnerRepoNumberFromToolResult(t *testing.T) {
	root := t.TempDir()
	prSession(t).
		Bash("2026-09-20T10:01:00Z", "toolu_01Create",
			"cd /w/unjira && rtk gh pr create --title \"collector: subagents\" --body \"$(cat <<'EOF'\n## Summary\nEOF\n)\"").
		ToolResult("2026-09-20T10:01:05Z", "toolu_01Create", "https://github.com/jcogilvie/unjira/pull/76\n", false).
		WriteRoot(root, slug, parentSession)

	_, got := collect(t, root, nil)

	a := onlyAnchor(t, got)
	assert.Equal(t, "pr_create:toolu_01Create", a.ExternalID)
	assert.Equal(t, "github.com/jcogilvie/unjira#76", a.Artifacts[events.ArtifactPullRequest],
		"host-qualified, so a GHES PR with the same owner/repo#N is a different key (F43)")
	assert.Equal(t, "created", a.Artifacts["pr_create_outcome"])
	assert.Equal(t, "tool_result", a.Artifacts["pr_resolution"])
	assert.Equal(t, "https://github.com/jcogilvie/unjira/pull/76", a.Artifacts["pr_url"])
	assert.Contains(t, a.Summary, "jcogilvie/unjira#76",
		"the canonical identifier must be in the summary, which is all clustering renders")
	assert.Contains(t, a.Summary, "collector: subagents")
	assert.Equal(t, "2026-09-20T10:01:05Z", a.OccurredAt.UTC().Format("2006-01-02T15:04:05Z"),
		"dated to the result, which is when the PR existed")
	assert.Equal(t, parentSession, a.Artifacts["session_id"])
	assert.NotContains(t, a.Artifacts, events.ArtifactGitBranch,
		"an anchor feeds no provenance tier: matching inputs are unchanged by this task")
	assert.False(t, events.IsTrackerRecord(a), "opening a PR is work evidence")
}

func TestAnchor_MCPCreatePullRequestIsRecognized(t *testing.T) {
	root := t.TempDir()
	prSession(t).
		ToolUse("2026-09-20T10:01:00Z", "toolu_01MCP", "mcp__github__create_pull_request", map[string]any{
			"owner": "Sanyaku", "repo": "helm-charts", "head": "PAAS-1-x", "base": "main", "title": "PAAS-1: x",
		}).
		ToolResult("2026-09-20T10:01:02Z", "toolu_01MCP", []any{
			map[string]any{"type": "text", "text": `{"id":"4146990902","url":"https://github.com/Sanyaku/helm-charts/pull/445"}`},
		}, false).
		WriteRoot(root, slug, parentSession)

	_, got := collect(t, root, nil)

	a := onlyAnchor(t, got)
	assert.Equal(t, "github.com/sanyaku/helm-charts#445", a.Artifacts[events.ArtifactPullRequest],
		"the number comes from the URL, not the result's id, which is GitHub's node id; case is folded, "+
			"since GitHub resolves owner and repo case-insensitively and the github collector reads them from config")
	assert.Contains(t, a.Summary, "Sanyaku/helm-charts#445", "the summary keeps the URL's own spelling")
	assert.Equal(t, "mcp__github__create_pull_request", a.Artifacts["tool"])
}

// TestAnchor_FlagFallback: the result names no URL — here the measured shape where
// `2>&1 | tail` hid gh's GraphQL failure from the error flag — so the literal flags are
// what is resolvable. No number is claimed: gh pr create never takes one.
func TestAnchor_FlagFallback(t *testing.T) {
	root := t.TempDir()
	prSession(t).
		Bash("2026-09-20T10:01:00Z", "toolu_01Flags",
			"gh pr create -R crossplane/cli --head jcogilvie:jco/render-engine --title t 2>&1 | tail -3").
		ToolResult("2026-09-20T10:01:05Z", "toolu_01Flags",
			"pull request create failed: GraphQL: No commits between main and jco/render-engine (createPullRequest)", false).
		WriteRoot(root, slug, parentSession)

	_, got := collect(t, root, nil)

	a := onlyAnchor(t, got)
	assert.Equal(t, "unconfirmed", a.Artifacts["pr_create_outcome"])
	assert.Equal(t, "arguments", a.Artifacts["pr_resolution"])
	assert.Equal(t, "crossplane/cli", a.Artifacts["pr_repo"])
	assert.Equal(t, "jco/render-engine", a.Artifacts["pr_head_branch"])
	assert.NotContains(t, a.Artifacts, events.ArtifactPullRequest, "never a PR identity without a number from the result")
	assert.Contains(t, a.Artifacts["pr_unresolved_reason"], "no pull request URL")
	assert.NotContains(t, a.Summary, "opened pull request")
}

func TestAnchor_MCPFallbackUsesStructuredInput(t *testing.T) {
	root := t.TempDir()
	prSession(t).
		ToolUse("2026-09-20T10:01:00Z", "toolu_01MCPNoURL", "mcp__github__create_pull_request", map[string]any{
			"owner": "o", "repo": "r", "head": "feat",
		}).
		ToolResult("2026-09-20T10:01:02Z", "toolu_01MCPNoURL", []any{map[string]any{"type": "text", "text": "{}"}}, false).
		WriteRoot(root, slug, parentSession)

	_, got := collect(t, root, nil)

	a := onlyAnchor(t, got)
	assert.Equal(t, "arguments", a.Artifacts["pr_resolution"])
	assert.Equal(t, "o/r", a.Artifacts["pr_repo"])
	assert.Equal(t, "feat", a.Artifacts["pr_head_branch"])
}

func TestAnchor_UnresolvableRecordsItsReasonInsteadOfGuessing(t *testing.T) {
	root := t.TempDir()
	prSession(t).
		Bash("2026-09-20T10:01:00Z", "toolu_01Var", `gh pr create --repo "$REPO" --head "$1" --fill`).
		ToolResult("2026-09-20T10:01:05Z", "toolu_01Var", "Opening github.com/o/r/compare in your browser.", false).
		WriteRoot(root, slug, parentSession)

	_, got := collect(t, root, nil)

	a := onlyAnchor(t, got)
	assert.Equal(t, "unresolved", a.Artifacts["pr_resolution"])
	assert.NotContains(t, a.Artifacts, "pr_repo")
	assert.NotContains(t, a.Artifacts, "pr_head_branch")
	assert.NotContains(t, a.Artifacts, events.ArtifactPullRequest)
	reason, _ := a.Artifacts["pr_unresolved_reason"].(string)
	assert.Contains(t, reason, "no pull request URL")
	assert.Contains(t, reason, "shell expansion", "the flags were present but not literal, and that must be said")
}

// TestAnchor_FailedCallIsNotAnOpenedPR covers the 12 measured errors, most of them the
// user rejecting the call — and gh's "already exists", whose output carries a URL for
// a PR this call did not create.
func TestAnchor_FailedCallIsNotAnOpenedPR(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		isError bool
	}{
		{"rejected", "The user doesn't want to proceed with this tool use. The tool use was rejected.", true},
		{
			"already exists, error hidden by a pipe",
			"a pull request for branch \"feat\" into branch \"main\" already exists:\nhttps://github.com/o/r/pull/12",
			false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			prSession(t).
				Bash("2026-09-20T10:01:00Z", "toolu_01Fail", "gh pr create -R o/r -H feat --fill 2>&1 | tail -2").
				ToolResult("2026-09-20T10:01:05Z", "toolu_01Fail", tc.content, tc.isError).
				WriteRoot(root, slug, parentSession)

			_, got := collect(t, root, nil)

			a := onlyAnchor(t, got)
			assert.Equal(t, "failed", a.Artifacts["pr_create_outcome"])
			assert.NotContains(t, a.Artifacts, events.ArtifactPullRequest)
			assert.NotEmpty(t, a.Artifacts["pr_unresolved_reason"])
			assert.NotContains(t, a.Summary, "opened pull request")
			assert.Contains(t, a.Summary, "failed")
		})
	}
}

// TestAnchor_AmbiguousResultNamesEveryCandidate covers the measured loop: one call, one
// static invocation, eight PRs. Which URL "is" the call's is not answerable, so none is
// claimed — but every one is recorded and shown, so clustering can still see them.
func TestAnchor_AmbiguousResultNamesEveryCandidate(t *testing.T) {
	root := t.TempDir()
	prSession(t).
		Bash("2026-09-20T10:01:00Z", "toolu_01Loop",
			"for repo in a b; do\n  cd /w/$repo\n  gh pr create --draft --title \"PAAS-3691: x\" --body-file /tmp/b.md\ndone").
		ToolResult("2026-09-20T10:01:30Z", "toolu_01Loop",
			"=== a ===\nhttps://github.com/Sanyaku/a/pull/1907\n=== b ===\nhttps://github.com/Sanyaku/b/pull/556\n", false).
		WriteRoot(root, slug, parentSession)

	_, got := collect(t, root, nil)

	a := onlyAnchor(t, got)
	assert.Equal(t, "ambiguous", a.Artifacts["pr_create_outcome"])
	assert.NotContains(t, a.Artifacts, events.ArtifactPullRequest)
	assert.Equal(t, []any{"Sanyaku/a#1907", "Sanyaku/b#556"}, a.Artifacts["pr_candidates"])
	assert.Contains(t, a.Summary, "Sanyaku/a#1907")
	assert.Contains(t, a.Summary, "Sanyaku/b#556")
}

// TestAnchor_GHESHostIsPartOfTheIdentity: a GHES result yields its own host, and the
// same owner/repo#N on two hosts is two pull requests — so a result naming both is
// ambiguous, never one creation. Before host qualification the two URLs collapsed to one
// "o/r#7" and the call read as an exact creation of a PR it may not have made.
func TestAnchor_GHESHostIsPartOfTheIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, result, outcome, pr string
	}{
		{"one GHES URL", "https://ghes.corp.example/o/r/pull/7\n", "created", "ghes.corp.example/o/r#7"},
		{
			"one owner/repo#N on two hosts",
			"https://github.com/o/r/pull/7\nhttps://ghes.corp.example/o/r/pull/7\n", "ambiguous", "",
		},
		{"one PR in two spellings", "https://github.com/O/R/pull/7\nhttps://github.com/o/r/pull/7\n", "created", "github.com/o/r#7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			prSession(t).
				Bash("2026-09-20T10:01:00Z", "toolu_01Host", "gh pr create --fill").
				ToolResult("2026-09-20T10:01:05Z", "toolu_01Host", tc.result, false).
				WriteRoot(root, slug, parentSession)

			_, got := collect(t, root, nil)

			a := onlyAnchor(t, got)
			assert.Equal(t, tc.outcome, a.Artifacts["pr_create_outcome"])
			if tc.pr == "" {
				assert.NotContains(t, a.Artifacts, events.ArtifactPullRequest)

				return
			}
			assert.Equal(t, tc.pr, a.Artifacts[events.ArtifactPullRequest])
		})
	}
}

func TestAnchor_MentionIsNotAnAnchor(t *testing.T) {
	root := t.TempDir()
	prSession(t).
		Bash("2026-09-20T10:01:00Z", "toolu_01Plan",
			"cat >> docs/plan.md <<'PLANEOF'\n```sh\ngh pr create --title x\n```\nPLANEOF").
		ToolResult("2026-09-20T10:01:01Z", "toolu_01Plan", "", false).
		Bash("2026-09-20T10:02:00Z", "toolu_01Grep", `grep -rn "gh pr create" .`).
		ToolResult("2026-09-20T10:02:01Z", "toolu_01Grep", "scm.go: gh pr create https://github.com/o/r/pull/3", false).
		WriteRoot(root, slug, parentSession)

	_, got := collect(t, root, nil)

	assert.Empty(t, anchors(got))
}

// TestAnchor_RereadOfGrownTranscriptIsNotDuplicated: the anchor is keyed on the
// tool_use id, which is immutable per call, so a transcript that grows (and so re-emits
// its size-keyed segments) re-emits the anchor under the SAME id, which dedupes.
func TestAnchor_RereadOfGrownTranscriptIsNotDuplicated(t *testing.T) {
	root := t.TempDir()
	b := prSession(t).
		Bash("2026-09-20T10:01:00Z", "toolu_01Grow", "gh pr create --fill").
		ToolResult("2026-09-20T10:01:05Z", "toolu_01Grow", "https://github.com/o/r/pull/9", false)
	b.WriteRoot(root, slug, parentSession)

	s, first := collect(t, root, nil)
	insertAll(t, s, first)

	b.User("2026-09-20T11:00:00Z", "more work").WriteRoot(root, slug, parentSession)
	second := collectWith(t, s, root, nil)

	require.Len(t, anchors(second), 1, "the grown transcript is re-read and the anchor re-emitted")
	assert.Equal(t, anchors(first)[0].ExternalID, anchors(second)[0].ExternalID)
	assert.Equal(t, 0, countInserted(t, s, anchors(second)), "and the store must ignore it as already held")
}

// TestAnchor_SameCallInTwoTranscriptsInsertsOnce is the resume-copy shape: measured,
// 1,841 tool_use ids appear in more than one root transcript, because a resumed session
// copies earlier history into a new file. A repeated id IS the same call, so the
// anchor carries no transcript identity in its key.
func TestAnchor_SameCallInTwoTranscriptsInsertsOnce(t *testing.T) {
	root := t.TempDir()
	b := prSession(t).
		Bash("2026-09-20T10:01:00Z", "toolu_01Shared", "gh pr create --fill").
		ToolResult("2026-09-20T10:01:05Z", "toolu_01Shared", "https://github.com/o/r/pull/9", false)
	b.WriteRoot(root, slug, parentSession)
	b.WriteRoot(root, slug, "d8159df7-1dda-4b1f-9055-4b0486f1d5f0")

	s, got := collect(t, root, nil)

	require.Len(t, anchors(got), 2, "both copies are read")
	assert.Equal(t, 1, countInserted(t, s, anchors(got)))
}

// TestAnchor_AwaitingResultIsDeferredVisiblyThenEmitted: a call whose result is not yet
// in the transcript is in flight. Emitting it now would freeze an unresolved row that
// INSERT OR IGNORE never updates (F21), so it is deferred — recorded on the segment
// events, so the deferral is visible — and emitted on the pass that sees its result.
func TestAnchor_AwaitingResultIsDeferredVisiblyThenEmitted(t *testing.T) {
	root := t.TempDir()
	b := prSession(t).Bash("2026-09-20T10:01:00Z", "toolu_01Flight", "gh pr create --fill")
	b.WriteRoot(root, slug, parentSession)

	s, first := collect(t, root, nil)

	assert.Empty(t, anchors(first))
	segs := segmentEvents(first)
	require.Len(t, segs, 1)
	assert.Equal(t, []any{"toolu_01Flight"}, segs[0].Artifacts["pr_creates_awaiting_result"])

	b.ToolResult("2026-09-20T10:01:09Z", "toolu_01Flight", "https://github.com/o/r/pull/10", false).
		WriteRoot(root, slug, parentSession)
	second := collectWith(t, s, root, nil)

	a := onlyAnchor(t, second)
	assert.Equal(t, "github.com/o/r#10", a.Artifacts[events.ArtifactPullRequest])
	assert.NotContains(t, segmentEvents(second)[0].Artifacts, "pr_creates_awaiting_result")
}

func TestAnchor_FromSubagentCarriesItsTranscript(t *testing.T) {
	root := t.TempDir()
	path := newTranscript(t).AsSubagent(parentSession, "a1").
		User("2026-09-20T10:00:00Z", "implement and open a PR").
		Bash("2026-09-20T10:01:00Z", "toolu_01Sub", "gh pr create --fill").
		ToolResult("2026-09-20T10:01:05Z", "toolu_01Sub", "https://github.com/o/r/pull/11", false).
		WriteSubagent(root, slug, parentSession, "a1")
	writeMeta(t, path, map[string]any{"toolUseId": "toolu_01Dispatch"})

	_, got := collect(t, root, nil)

	a := onlyAnchor(t, got)
	assert.Equal(t, "pr_create:toolu_01Sub", a.ExternalID)
	assert.Equal(t, "a1", a.Artifacts["agent_id"])
	assert.Equal(t, "toolu_01Dispatch", a.Artifacts["dispatch_tool_use_id"])
	assert.Contains(t, a.Summary, "subagent")
}

func TestAnchor_ExcludedCwdEmitsNoAnchor(t *testing.T) {
	root := t.TempDir()
	repo := "/Users/j/workspace/unjira"
	newTranscript(t).InCwd(repo+"/.claude/worktrees/agent-a1").
		User("2026-09-20T10:00:00Z", "go").
		Bash("2026-09-20T10:01:00Z", "toolu_01Excl", "gh pr create --fill").
		ToolResult("2026-09-20T10:01:05Z", "toolu_01Excl", "https://github.com/o/r/pull/12", false).
		WriteRoot(root, slug, parentSession)

	_, got := collect(t, root, map[string]any{"exclude_cwds": []string{repo}})

	assert.Empty(t, got)
}

// TestAnchor_UnparseableLinesNeverCrash: real transcripts hold malformed JSON lines and
// tool calls with no id or a non-string command. None may fail the pass.
func TestAnchor_UnparseableLinesNeverCrash(t *testing.T) {
	root := t.TempDir()
	path := prSession(t).
		ToolUse("2026-09-20T10:01:00Z", "", "Bash", map[string]any{"command": "gh pr create --fill"}).
		ToolUse("2026-09-20T10:01:01Z", "toolu_01Odd", "Bash", map[string]any{"command": 42}).
		ToolUse("2026-09-20T10:01:02Z", "toolu_01Odd2", "mcp__github__create_pull_request", map[string]any{"owner": 7}).
		ToolResult("2026-09-20T10:01:03Z", "toolu_01Odd2", 99, false).
		WriteRoot(root, slug, parentSession)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString("{not json\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	_, got := collect(t, root, nil)

	a := onlyAnchor(t, got)
	assert.Equal(t, "pr_create:toolu_01Odd2", a.ExternalID)
	assert.Equal(t, "unresolved", a.Artifacts["pr_resolution"])
}
