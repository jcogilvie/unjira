# F47 — a PR a subagent opened is named only in the subagent's own segment

A segment summary names the PRs its run opened (`openedPullRequests`,
`internal/collector/claudecode/anchors.go`), and so lets the clustering model see which work a root
orchestration run touched. It reads only the segment's own transcript. A root session that dispatches a
subagent, which then opens the PR, gets no such clause, and the dispatch is invisible in the root's
summary in the same way all of a run's PRs were before. **Consequence:** in subagent-opens-the-PR
workflows, the root segment behind several PRs reads as having touched none of them, so it can be
context to none, which is the inertness the shared-context measurement found. Connecting the two needs
cross-transcript lineage (the subagent's `session_id` is its parent's, and the dispatching `Task` call
is in the root's lines): the shared-context spec's slice 2. Frequency: 0 of the 70 created-PR anchors
in this repo's transcripts came from a subagent; unmeasured elsewhere.

## Status

open. Needs cross-transcript lineage (shared-context slice 2). 0 of 70 here
