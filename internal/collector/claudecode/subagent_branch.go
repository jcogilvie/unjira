package claudecode

// subagent_branch.go recovers a subagent's own branch from its own tool calls (F32).
//
// A subagent transcript's gitBranch is its PARENT's (subagentBranchOmittedReason), so it
// is never the subagent's branch. What the subagent itself names is: the branch it
// creates, renames to, pushes, or opens a PR from. The subagents that do any of that are
// the ones implementing a change, which is the work whose branch matches a GitHub PR's
// head, so the recovered branch reaches clustering and the dispute re-ask's
// head-branch evidence.

import (
	"regexp"
	"slices"
	"strings"
)

// gitBranchSource records where a subagent's events.ArtifactGitBranch came from, since
// for a root session it is the transcript's own gitBranch and here it is not.
const (
	artifactGitBranchSource = "git_branch_source"
	branchFromToolCalls     = "subagent_tool_calls"

	subagentSeveralBranchesReason = "subagent transcript: its own tool calls name several branches, " +
		"so no one of them is the subagent's branch; see session_branches"
)

// toolGeneratedBranch matches the names the harness gives a subagent's worktree. Nobody
// chose one to name the work, and they are routinely renamed (artifactWorktreeBranch).
var toolGeneratedBranch = regexp.MustCompile(`^worktree-agent-[0-9a-f]+$`)

// subagentBranches returns the branches a transcript's own tool calls name, in order of
// first appearance and without repeats. Only literal names count: an expansion's value
// is not in the transcript.
func subagentBranches(lines []map[string]any) []string {
	var out []string

	for _, line := range lines {
		for _, command := range toolCommands(line) {
			for _, c := range simpleCommands(command) {
				for _, name := range branchesNamed(c.words) {
					if name == "" || name == "HEAD" || toolGeneratedBranch.MatchString(name) {
						continue
					}
					if !slices.Contains(out, name) {
						out = append(out, name)
					}
				}
			}
		}
	}

	return out
}

// branchesNamed returns the branches one simple command creates, renames to, pushes, or
// opens a PR from.
func branchesNamed(words []shellWord) []string {
	if isProgram(words, "gh") && hasWords(words, "pr", "create") {
		if head := parsePRCreateArgs(words[3:]).head; head != "" {
			return []string{head}
		}

		return nil
	}

	sub, args, ok := gitSubcommand(words)
	if !ok {
		return nil
	}

	if flags, ok := branchCreatingFlags[sub]; ok {
		return literalAfter(args, flags...)
	}

	switch sub {
	case "branch":
		if hasAnyWord(args, "-m", "-M", "--move") {
			if p := positionals(args); len(p) > 0 {
				return lastLiteral(p)
			}
		}
	case "push":
		return pushedBranches(args)
	}

	return nil
}

// literalAfter is the literal word after the first of flags, as a one-element list.
func literalAfter(args []shellWord, flags ...string) []string {
	for i, w := range args {
		if hasAnyWord([]shellWord{w}, flags...) && i+1 < len(args) && args[i+1].literal {
			return []string{args[i+1].text}
		}
	}

	return nil
}

// pushedBranches reads `git push [options] <remote> <refspec>...`: each refspec's
// destination (`src:dst` pushes to dst), without a force `+` or a refs/heads/ prefix.
func pushedBranches(args []shellWord) []string {
	p := positionals(args)
	if len(p) < 2 {
		return nil
	}

	var out []string
	for _, ref := range p[1:] {
		if !ref.literal {
			continue
		}
		name := strings.TrimPrefix(ref.text, "+")
		if _, dst, ok := strings.Cut(name, ":"); ok {
			name = dst
		}
		out = append(out, strings.TrimPrefix(name, "refs/heads/"))
	}

	return out
}

// positionals are the words that are not options, skipping the value of git push's
// options that take one as the next word.
func positionals(args []shellWord) []shellWord {
	var out []shellWord
	for i := 0; i < len(args); i++ {
		w := args[i].text
		if strings.HasPrefix(w, "-") {
			if (w == "-o" || w == "--push-option" || w == "--repo" || w == "--receive-pack") && i+1 < len(args) {
				i++
			}

			continue
		}
		out = append(out, args[i])
	}

	return out
}

func lastLiteral(words []shellWord) []string {
	if last := words[len(words)-1]; last.literal {
		return []string{last.text}
	}

	return nil
}
