# F6 — Six artifacts are written and never read

`cwd`, `session_id`, `started_at`, `ended_at`, `session_branches`, `user_message_count` (claudecode),
`field`, `project_key` (jira) have zero production readers.

**Added deliberately ahead of their reader, 2026-10-01.** Subagent collection writes `parent_session_id`,
`agent_id`, `parent_agent_id`, `dispatch_tool_use_id`, `subagent_description`, `spawn_depth`,
`worktree_branch`, `subagent_meta` and `git_branch_omitted`; PR anchors write `anchor_kind`,
`tool_use_id`, `tool`, `pr_create_outcome`, `pr_resolution`, `pr_unresolved_reason`, `pr_url`,
`pr_repo`, `pr_head_branch`, `pr_candidates`, and segments `pr_creates_awaiting_result`. None is read in
production yet, and that is the plan, not an oversight: their intended reader is the next step,
attaching a root session's shared context to the narratives its subagents and PRs produced — which
needs exactly the relationship these record.

**That reader is now designed:** `docs/superpowers/specs/2026-10-02-shared-context-design.md`. Its first
slice, landed, reads none of the artifacts listed above: context links are assigned by the model, and
the measured gap is closable without lineage. Its second slice is the reader: it renders each subagent's
parent and dispatching root segment into the clustering prompt, from `session_id`, `parent_session_id`,
`agent_id`, `parent_agent_id`, `dispatch_tool_use_id`, `subagent_description`, `spawn_depth`, and the
root segments' `started_at`/`ended_at`. That design reads none of the anchor artifacts, so it leaves
their readers undecided. (The slice's acceptance probe, `internal/pipeline/shared_probe_*_test.go`,
reads `anchor_kind`, `pr_create_outcome`, `session_id` and `agent_id` to compute its metrics. Test-only,
so none of them has a production reader.)

`events.ArtifactPullRequest` is no longer on this list. Its two writers (claudecode anchors, every
github PR event) gained a reader in the first slice: the dispute re-ask
(`correlator/cluster_dispute.go`, `writeEvidence`) tells the model when a disputed event carries the
same pull request as one claimant's PR event. That is evidence the model weighs. Code never applies it.
The deterministic anchor↔`:opened` join is still a later measurement's decision.

**Re-verified 2026-09-16**, because F15/F20/F25 all added artifacts and it was worth checking whether
any of these had since gained a reader. None had. One near-miss worth naming: `segmentSummary` renders
`len(seg.userTexts)`, not the `user_message_count` artifact — the number reaches a reader, the artifact
does not. `scm_keys` (F20) is the counter-example that shows the difference: it was written *and*
wired to `gatherCandidates` in the same change, so it never belonged on this list.

`ended_at` and `session_branches` arrived with F15 and are listed here in the same breath as they were
added, deliberately: they exist so a *future* reader — clustering weighing whether three branches are one
story — can use them, and writing them without a reader is the honest first half of that. The distinction
worth keeping is between an artifact nothing reads YET and one nothing will ever read. `field` has a doc comment claiming it *"distinguishes a
description edit from a summary edit, which nothing else records"* — true, and nothing reads it.

Cheap to keep and genuinely useful when re-enriching (**#176**). Listed for completeness, not as
something to remove.

## Status

**#176**, re-verified 2026-09-16 after F15/F20/F25 each added artifacts: still zero readers. Near-miss worth naming — `segmentSummary` renders `len(seg.userTexts)`, not the `user_message_count` artifact. `scm_keys` is the counter-example: written AND wired in one change, so it never belonged here. Narrowed 2026-10-02: `events.ArtifactPullRequest` gained readers, the dispute re-ask's evidence and then the PR identity join (F43)
