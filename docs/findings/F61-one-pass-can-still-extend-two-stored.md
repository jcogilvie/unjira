# F61 — one pass can still extend two stored narratives with one pull request's work

A bisected window clusters each half from its own events, so a PR opened in one half and merged in the
other used to become two NEW clusters. On a real 30-day store's first pass (eight clustering calls), 19
of the 99 PRs with two or more member events ended in two narratives, every one of them straddling a
seam. `joinByPullRequest` (`internal/correlator/cluster_prjoin.go:90`) now joins, after the halves are
merged and before the dispute re-ask, every group of results whose undisputed member events share an
exact `events.PullRequestOf` value: NEW with NEW, and NEW into the one stored narrative a group extends.

What it leaves is a group that extends two or more DIFFERENT stored narratives (`:115`). Joining those
would merge stored narratives, which is a triage merge's call, so the group is left exactly as it is,
NEW members included, and reported (`Stats.PRIdentityConflicts`, a warning log, and a pass-summary line).
A disputed member is also left to the model by design: a member two results claim drives no join, so
if the dispute re-ask gives it to a result holding none of that PR's other work, the PR is in two
clusters after the pass, as the model decided with the PR shown to it as evidence.

**Consequence:** the PR is held by two narratives from that pass on, which is `PRSeveralHolders`
(`internal/pipeline/preassign.go:102`) for every later event, so those go to the model. Arises only where
two stored narratives already hold the PR's work or the model places its events under two of them; the
pre-assignment join places a later event into a single open holder before the model is called.
Unmeasured: the pass summary's conflict line is what would count it.

## Status

open, narrowed: `joinByPullRequest` joins results sharing an exact PR identity after clustering (on a real 30-day first pass, 19 of 99 multi-event PRs had split across bisection seams). Left apart and reported: a group extending two different stored narratives, and a disputed member the re-ask gives to a result holding none of its PR's other work. Unmeasured
