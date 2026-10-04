package claudecode_test

// subagent_branch_test.go covers F32's recoverable half: a subagent's transcript carries
// its PARENT's gitBranch, so its own branch comes only from its own tool calls.
//
// Measured over 1,044 subagent transcripts: 991 name no branch of their own (research,
// review and measurement agents), 37 name exactly one, and 16 name several (one agent
// opening two related PRs). Two of the single names were tool-generated
// `worktree-agent-<id>` names, which name no work.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/events"
)

func onlySubagentSegment(t *testing.T, b *transcriptBuilder) events.Event {
	t.Helper()

	root := t.TempDir()
	b.WriteSubagent(root, slug, parentSession, "a1")
	_, got := collect(t, root, nil)
	segs := segmentEvents(got)
	require.Len(t, segs, 1)

	return segs[0]
}

func TestCollect_SubagentBranchComesFromItsOwnCalls(t *testing.T) {
	for _, tc := range []struct{ name, command string }{
		{"checkout -b", `git checkout -b fix/514-comp-overlay`},
		{"switch -c", `cd /w/u && git switch -c fix/514-comp-overlay`},
		{"worktree add -b", `git worktree add -b fix/514-comp-overlay ../wt origin/main`},
		{"gh pr create --head, fork prefix stripped", `gh pr create --head jcogilvie:fix/514-comp-overlay --fill`},
		{"push of a named branch", `git push -u origin fix/514-comp-overlay`},
		{"push of a refspec", `git push origin HEAD:fix/514-comp-overlay`},
		{"rename", `git branch -m fix/514-comp-overlay`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seg := onlySubagentSegment(t, subagentTranscript(t, "a1").
				Bash("2026-09-20T10:01:00Z", "toolu_01Br", tc.command))

			assert.Equal(t, "fix/514-comp-overlay", seg.Artifacts[events.ArtifactGitBranch])
			assert.Equal(t, "subagent_tool_calls", seg.Artifacts["git_branch_source"],
				"a recovered branch says where it came from")
			assert.NotContains(t, seg.Artifacts, "git_branch_omitted")
			assert.Contains(t, seg.Summary, "on branch fix/514-comp-overlay")
		})
	}
}

// TestCollect_SubagentNamingSeveralBranchesGetsNone: one agent that opened two PRs worked
// on two branches, and either as THE branch would misattribute half its work. The set is
// recorded for clustering to weigh; the provenance tier gets nothing.
func TestCollect_SubagentNamingSeveralBranchesGetsNone(t *testing.T) {
	seg := onlySubagentSegment(t, subagentTranscript(t, "a1").
		Bash("2026-09-20T10:01:00Z", "toolu_01A", `git checkout -b paas-3691-long-pg-selector`).
		Bash("2026-09-20T10:02:00Z", "toolu_01B", `git checkout -b paas-3691-prods-pg-selector`))

	assert.NotContains(t, seg.Artifacts, events.ArtifactGitBranch)
	assert.Contains(t, seg.Artifacts["git_branch_omitted"], "several")
	assert.Equal(t, []string{"paas-3691-long-pg-selector", "paas-3691-prods-pg-selector"},
		seg.Artifacts["session_branches"])
}

// TestCollect_SubagentBranchIgnoresWhatNamesNoWork: a mention is not a call, HEAD is not a
// name, an expansion is not a literal, and a tool-generated worktree name was chosen by
// the harness, not by anyone naming the work.
func TestCollect_SubagentBranchIgnoresWhatNamesNoWork(t *testing.T) {
	for _, tc := range []struct{ name, command string }{
		{"mention", `echo "git checkout -b fix/x"`},
		{"push HEAD", `git push -u origin HEAD`},
		{"expansion", `git checkout -b "$BRANCH"`},
		{"tool-generated worktree name", `git push origin worktree-agent-aa5f983e79c74a9bc`},
		{"reading", `git log fix/514-comp-overlay`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seg := onlySubagentSegment(t, subagentTranscript(t, "a1").
				Bash("2026-09-20T10:01:00Z", "toolu_01X", tc.command))

			assert.NotContains(t, seg.Artifacts, events.ArtifactGitBranch)
			assert.NotEmpty(t, seg.Artifacts["git_branch_omitted"])
		})
	}
}

// TestCollect_RootSessionsKeepTheirOwnBranch: recovery is for subagents only. A root
// session's gitBranch is its own and stays the branch, whatever its calls name.
func TestCollect_RootSessionsKeepTheirOwnBranch(t *testing.T) {
	root := t.TempDir()
	newTranscript(t).OnBranch("feature/PROJ-42").
		User("2026-09-20T10:00:00Z", "go").
		Bash("2026-09-20T10:01:00Z", "toolu_01R", `git push origin other-branch`).
		WriteRoot(root, slug, parentSession)

	_, got := collect(t, root, nil)

	segs := segmentEvents(got)
	require.Len(t, segs, 1)
	assert.Equal(t, "feature/PROJ-42", segs[0].Artifacts[events.ArtifactGitBranch])
	assert.NotContains(t, segs[0].Artifacts, "git_branch_source")
}
