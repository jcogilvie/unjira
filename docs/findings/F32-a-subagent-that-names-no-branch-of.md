# F32 — a subagent that names no branch of its own has no branch at all

Claude Code writes the PARENT session's branch into every line of a subagent transcript. Of 31 subagent
metas naming a `worktreeBranch`, 27 never show it in `gitBranch`, which instead holds the parent's
(`pr-27998`, `worktree-reconciler`, `main`). The meta's `worktreeBranch` is no substitute: all 31 values
are tool-generated `worktree-agent-<id>` names, recorded at creation and routinely renamed afterwards.
So the collector clears `gitBranch` before segmenting a subagent, and never emits it.

What a subagent names itself is recovered (`subagentBranches`, `internal/collector/claudecode/
subagent_branch.go`): the branch its own calls create (`checkout -b`, `switch -c`, `worktree add -b`),
rename to, push, or open a PR from (`gh pr create --head`). Exactly one such name becomes
`events.ArtifactGitBranch`, with `git_branch_source: subagent_tool_calls`. Several become
`session_branches`, with no branch chosen. Measured over 1,041 subagent segments: 39 recover a branch,
19 name several, and the rest name none.

**What remains:** those that name none, about 94%, still have no branch. Most did no branch work
(research, review, measurement), so nothing is lost. But a subagent that committed in a harness-made
worktree and never pushed or renamed the branch did work on a branch it never named, and that branch is
invisible. Its only record is the meta's tool-generated `worktree-agent-<id>` name, which is
deliberately not used. Frequency unmeasured. The provenance effect is small either way: 1 of 31
recovered branch names carries a ticket key. The value is in clustering and the dispute re-ask, which
match a branch against a PR's head.

Nor by the back door. `docs/superpowers/specs/2026-10-02-shared-context-design.md` §3 keeps context links
out of matching entirely, so linking a subagent's narrative to its parent's root segment as context does
not hand it the parent's branch as provenance.

## Status

open, narrowed: a subagent's own calls now supply its branch when they name exactly one (39 of 1,041 segments); several are recorded as a set. The remainder named none
