package claudecode

// location.go records where a segment's work happened, for the reconciler to match
// against tracker scopes (docs/superpowers/specs/2026-10-06-tracker-model-design.md,
// "Work location"). Two kinds of evidence, in strict order, and the collector only
// extracts them — which tracker a repository belongs to is decided at reconcile time,
// from config, so a config change applies without re-collecting.
//
//  1. The transcript's own SCM actions (workRepos): where the work WENT.
//  2. The working directory's repository's remotes (remoteReader): the fallback.

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/go-git/go-git/v5"

	"github.com/jcogilvie/unjira/internal/events"
)

// pushTargetRE is the line `git push` prints naming the remote it pushed to:
// "To github.com:owner/repo.git", "To https://github.com/owner/repo".
var pushTargetRE = regexp.MustCompile(`(?m)^To\s+(\S+)\s*$`)

// workRepos returns the repositories this run's own SCM actions sent work to, sorted,
// host-qualified ("<host>/<owner>/<repo>", case-folded):
//
//   - the remote a `git push` result names;
//   - a pull request a call created (created, the resolved PR-creation anchors);
//   - the -R/--repo of an AUTHORING gh command (isAuthoring) — a `gh pr view -R` is
//     investigation, and says nothing about where this work went;
//   - the owner/repo of a GitHub MCP write call (scmToolNames).
//
// Scoped to the run by line, as keys and facts are.
func workRepos(seg segment, created map[string]prAnchor) []string {
	var repos []string

	add := func(repo string) {
		if repo != "" && !slices.Contains(repos, repo) {
			repos = append(repos, repo)
		}
	}

	pushes := map[string]bool{}

	for _, line := range seg.factLines {
		for _, call := range prCreateCalls(line, "") {
			if a, ok := created[call.toolUseID]; ok {
				repo, _, _ := strings.Cut(a.prKey, "#")
				add(repo)
			}
		}

		for _, b := range blocksOf(line) {
			for _, repo := range blockRepos(b, pushes) {
				add(repo)
			}
		}
	}

	slices.Sort(repos)

	return repos
}

// blockRepos returns the repositories one content block names as where work went. A
// tool_use that runs `git push` is remembered in pushes, so its tool_result — which may
// arrive on a later line — is read for the remote the push named.
func blockRepos(b map[string]any, pushes map[string]bool) []string {
	switch b["type"] {
	case blockTypeToolUse:
		if isPush(b) {
			id, _ := b["id"].(string)
			pushes[id] = true
		}

		return toolUseRepos(b)
	case "tool_result":
		id, _ := b["tool_use_id"].(string)
		if !pushes[id] {
			return nil
		}

		var out []string
		for _, m := range pushTargetRE.FindAllStringSubmatch(resultText(b["content"]), -1) {
			if repo, ok := events.NormalizeRepo(m[1]); ok {
				out = append(out, repo)
			}
		}

		return out
	}

	return nil
}

// blocksOf returns a line's message content blocks.
func blocksOf(line map[string]any) []map[string]any {
	message, _ := line["message"].(map[string]any)
	if message == nil {
		return nil
	}

	raw, _ := message["content"].([]any)

	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if b, ok := item.(map[string]any); ok {
			out = append(out, b)
		}
	}

	return out
}

// isPush reports whether a tool_use block is a shell call running `git push`.
func isPush(block map[string]any) bool {
	input, _ := block["input"].(map[string]any)
	command, _ := input["command"].(string)

	return slices.ContainsFunc(simpleCommands(command), func(c simpleCommand) bool {
		sub, _, ok := gitSubcommand(c.words)

		return ok && sub == "push"
	})
}

// toolUseRepos returns the repositories a tool_use block names as the target of an
// authoring call: a gh command's -R/--repo, or a GitHub MCP write call's owner/repo.
// Both are github.com: gh's host prefix is dropped by its parser, and the GitHub MCP
// server talks to github.com.
func toolUseRepos(block map[string]any) []string {
	name, _ := block["name"].(string)
	input, _ := block["input"].(map[string]any)

	if slices.Contains(scmToolNames, name) {
		owner, _ := input["owner"].(string)
		repo, _ := input["repo"].(string)
		if owner == "" || repo == "" {
			return nil
		}

		return []string{strings.ToLower("github.com/" + owner + "/" + repo)}
	}

	command, _ := input["command"].(string)

	var out []string
	for _, c := range simpleCommands(command) {
		if !isProgram(c.words, "gh") || !isAuthoring(c.words) {
			continue
		}

		if repo := parsePRCreateArgs(c.words).repo; repo != "" {
			out = append(out, strings.ToLower("github.com/"+repo))
		}
	}

	return out
}

// Reasons recorded in events.ArtifactCwdRemotesOmitted.
const (
	cwdGoneReason    = "the working directory no longer exists, so its remotes cannot be read"
	cwdNotRepoReason = "the working directory is not a git repository"
	cwdUnsetReason   = "the segment recorded no working directory"
)

// remotes is what one working directory's repository says about where its work lives:
// its hosted remotes, sorted, or why there are none.
type remotes struct {
	repos   []string
	omitted string
}

// remoteReader reads each distinct working directory's remotes once per collection
// pass. Read at collect time because the directory often does not survive: 8% of root
// and 13% of subagent working directories no longer existed later, mostly deleted
// worktrees.
//
// The bounded exception to "collectors are dumb" (CLAUDE.md): it reads a working tree's
// local git config through go-git. Local, deterministic, no network, no git binary, no
// judgment — it records remotes, and which tracker they belong to is decided at
// reconcile time.
type remoteReader struct {
	cache map[string]remotes
}

func newRemoteReader() *remoteReader {
	return &remoteReader{cache: map[string]remotes{}}
}

// read returns cwd's remotes, from the cache when this pass already read it.
func (r *remoteReader) read(cwd string) remotes {
	if got, ok := r.cache[cwd]; ok {
		return got
	}

	got := readRemotes(cwd)
	r.cache[cwd] = got

	return got
}

// readRemotes opens the repository containing cwd and normalizes every remote's URLs.
//
// DetectDotGit finds the repository from a subdirectory; EnableDotGitCommonDir follows a
// linked worktree's .git file to its main repository, where the remotes live. go-git's
// remote order is not stable, so the result is sorted.
func readRemotes(cwd string) remotes {
	if cwd == "" {
		return remotes{omitted: cwdUnsetReason}
	}

	if _, err := os.Stat(cwd); errors.Is(err, os.ErrNotExist) {
		return remotes{omitted: cwdGoneReason}
	}

	repo, err := git.PlainOpenWithOptions(cwd, &git.PlainOpenOptions{DetectDotGit: true, EnableDotGitCommonDir: true})
	if errors.Is(err, git.ErrRepositoryNotExists) {
		return remotes{omitted: cwdNotRepoReason}
	}
	if err != nil {
		return remotes{omitted: fmt.Sprintf("opening the working directory's repository: %v", err)}
	}

	list, err := repo.Remotes()
	if err != nil {
		return remotes{omitted: fmt.Sprintf("reading the repository's remotes: %v", err)}
	}

	var repos []string

	for _, remote := range list {
		for _, u := range remote.Config().URLs {
			if normalized, ok := events.NormalizeRepo(u); ok && !slices.Contains(repos, normalized) {
				repos = append(repos, normalized)
			}
		}
	}

	slices.Sort(repos)

	return remotes{repos: repos}
}

// setLocationArtifacts records seg's work location on evt: the transcript's evidence
// when it has any, and the cwd's remotes always, since which one wins is the
// reconciler's decision, made from both.
func setLocationArtifacts(evt *events.Event, seg segment, created map[string]prAnchor, reader *remoteReader) {
	if repos := workRepos(seg, created); len(repos) > 0 {
		events.SetRepos(evt, events.ArtifactWorkRepos, repos)
	}

	got := reader.read(seg.cwd)
	if got.omitted != "" {
		evt.Artifacts[events.ArtifactCwdRemotesOmitted] = got.omitted

		return
	}

	events.SetRepos(evt, events.ArtifactCwdRemotes, got.repos)
}
