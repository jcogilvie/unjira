package claudecode_test

// location_test.go covers where untracked work happened: the transcript's own SCM
// actions (where the work WENT), and, as a fallback, the remotes of the working
// directory's repository, read with go-git. See
// docs/superpowers/specs/2026-10-06-tracker-model-design.md, "Work location".

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/events"
)

// gitRepo builds a real repository at dir with the given remotes (name -> URL).
func gitRepo(t *testing.T, dir string, remotes map[string]string) {
	t.Helper()

	repo, err := git.PlainInit(dir, false)
	require.NoError(t, err)

	for name, u := range remotes {
		_, err := repo.CreateRemote(&gitconfig.RemoteConfig{Name: name, URLs: []string{u}})
		require.NoError(t, err)
	}
}

// linkedWorktree lays out a linked worktree of the repository at main, the way `git
// worktree add` does: a .git FILE pointing into main's .git/worktrees/<name>, whose
// commondir leads back to main's .git, where the remotes live.
func linkedWorktree(t *testing.T, main, wt, name string) {
	t.Helper()

	admin := filepath.Join(main, ".git", "worktrees", name)
	require.NoError(t, os.MkdirAll(admin, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(admin, "HEAD"), []byte("ref: refs/heads/"+name+"\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(admin, "commondir"), []byte("../..\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(admin, "gitdir"), []byte(filepath.Join(wt, ".git")+"\n"), 0o644))

	require.NoError(t, os.MkdirAll(wt, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+admin+"\n"), 0o644))
}

// collectSegmentIn collects one root session whose single segment ran in cwd.
func collectSegmentIn(t *testing.T, cwd string) events.Event {
	t.Helper()

	root := t.TempDir()
	newTranscript(t).InCwd(cwd).OnBranch("main").
		User("2026-10-06T10:00:00Z", "please look at this").
		WriteRoot(root, "proj", "s1")

	_, evts := collect(t, root, nil)
	require.Len(t, evts, 1)

	return evts[0]
}

func TestCollect_RecordsEveryCwdRemoteSorted(t *testing.T) {
	dir := t.TempDir()
	gitRepo(t, dir, map[string]string{
		"origin":   "git@github.com:Me/crossplane.git",
		"upstream": "https://github.com/crossplane/crossplane.git",
		"a":        "https://github.com/a/one.git",
		"b":        "https://github.com/b/two.git",
		"z":        "https://github.com/z/three.git",
		"local":    "/srv/git/mirror.git",
	})

	evt := collectSegmentIn(t, dir)

	assert.Equal(t, []string{
		"github.com/a/one", "github.com/b/two", "github.com/crossplane/crossplane",
		"github.com/me/crossplane", "github.com/z/three",
	},
		events.ReposOf(evt, events.ArtifactCwdRemotes),
		"every hosted remote, not only origin, sorted; a local path names no hosted repository")
	assert.NotContains(t, evt.Artifacts, events.ArtifactCwdRemotesOmitted)
}

// TestCollect_LinkedWorktreeResolvesToItsMainRepositorysRemotes: a linked worktree's
// .git is a file, and its remotes live in the main repository's config.
func TestCollect_LinkedWorktreeResolvesToItsMainRepositorysRemotes(t *testing.T) {
	main := filepath.Join(t.TempDir(), "main")
	gitRepo(t, main, map[string]string{"origin": "git@github.com:org/helm-charts.git"})
	wt := filepath.Join(t.TempDir(), "wt")
	linkedWorktree(t, main, wt, "wt")

	evt := collectSegmentIn(t, wt)

	assert.Equal(t, []string{"github.com/org/helm-charts"}, events.ReposOf(evt, events.ArtifactCwdRemotes))
}

func TestCollect_ASubdirectoryResolvesToItsRepository(t *testing.T) {
	dir := t.TempDir()
	gitRepo(t, dir, map[string]string{"origin": "https://github.com/o/r"})
	sub := filepath.Join(dir, "charts", "app")
	require.NoError(t, os.MkdirAll(sub, 0o755))

	evt := collectSegmentIn(t, sub)

	assert.Equal(t, []string{"github.com/o/r"}, events.ReposOf(evt, events.ArtifactCwdRemotes))
}

// TestCollect_AGoneOrNonRepositoryCwdRecordsWhy: 8% of root and 13% of subagent cwds no
// longer exist by a later pass, mostly deleted worktrees. The absence is recorded with
// its reason, as the other *_omitted artifacts are.
func TestCollect_AGoneOrNonRepositoryCwdRecordsWhy(t *testing.T) {
	gone := collectSegmentIn(t, filepath.Join(t.TempDir(), "deleted-worktree"))
	assert.Nil(t, events.ReposOf(gone, events.ArtifactCwdRemotes))
	assert.Contains(t, gone.Artifacts[events.ArtifactCwdRemotesOmitted], "no longer exists")

	plain := collectSegmentIn(t, t.TempDir())
	assert.Nil(t, events.ReposOf(plain, events.ArtifactCwdRemotes))
	assert.Contains(t, plain.Artifacts[events.ArtifactCwdRemotesOmitted], "not a git repository")
}

// TestCollect_RecordsWhereTheTranscriptSentTheWork: the remote a push named, a pull
// request a call created, a gh -R on an authoring command, and a GitHub MCP write call.
func TestCollect_RecordsWhereTheTranscriptSentTheWork(t *testing.T) {
	root := t.TempDir()
	newTranscript(t).InCwd(t.TempDir()).OnBranch("fix").
		User("2026-10-06T10:00:00Z", "ship the fix").
		Bash("2026-10-06T10:01:00Z", "t1", "git push -u origin fix").
		ToolResult("2026-10-06T10:01:05Z", "t1",
			"remote: \nTo github.com:Org/CSE-Gitops.git\n * [new branch]      fix -> fix\n", false).
		Bash("2026-10-06T10:02:00Z", "t2", `gh pr create -R upstream/proj --title "fix"`).
		ToolResult("2026-10-06T10:02:05Z", "t2", "https://github.com/upstream/proj/pull/9\n", false).
		Bash("2026-10-06T10:03:00Z", "t3", `gh issue create --repo issues/tracker --title x`).
		ToolUse("2026-10-06T10:04:00Z", "t4", "mcp__github__create_pull_request",
			map[string]any{"owner": "Mcp", "repo": "Repo", "title": "t", "head": "fix", "base": "main"}).
		Bash("2026-10-06T10:05:00Z", "t5", `gh pr view -R reading/only 3`).
		WriteRoot(root, "proj", "s1")

	_, evts := collect(t, root, nil)

	var segment events.Event
	for _, e := range evts {
		if _, ok := e.Artifacts["user_message_count"]; ok {
			segment = e
		}
	}
	require.NotNil(t, segment.Artifacts)

	assert.Equal(t, []string{
		"github.com/issues/tracker",
		"github.com/mcp/repo",
		"github.com/org/cse-gitops",
		"github.com/upstream/proj",
	}, events.ReposOf(segment, events.ArtifactWorkRepos),
		"a reading command (gh pr view -R) is investigation, not where work went")
}

func TestCollect_NoSCMActionRecordsNoWorkRepos(t *testing.T) {
	evt := collectSegmentIn(t, t.TempDir())

	assert.NotContains(t, evt.Artifacts, events.ArtifactWorkRepos)
}
