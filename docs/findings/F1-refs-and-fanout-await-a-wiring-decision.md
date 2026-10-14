# F1 — refs and fanout await a wiring decision, not a missing collector

**Narrowed 2026-09-21: the GitHub collector this entry was waiting for now exists**
(`internal/collector/github`, PR lifecycle slice — opened, merged, closed). That does not close
this finding, because the collector's own slice deliberately does not wire either package in — see
`docs/superpowers/specs/2026-09-17-github-collector-design.md`'s §6, unchanged by that slice
landing. `fanout` has a real integration point designed (`groupByFanoutFamily` feeding
`correlator.WithInstruction`, at the pipeline layer) but deferred to a later slice pending real
fan-out data; `refs` has no consumer even with a collector in hand, because matching resolves a
narrative to a *Jira issue key* and nothing in the pipeline represents a GitHub PR-to-PR
relationship. So the finding's shape has changed — "no collector" is no longer true — but its
substance has not: both packages remain uncalled, and the reasoning below (unchanged) still
explains why deleting them would be wrong.

`internal/correlator/refs` and `internal/correlator/fanout` are pure, thoroughly tested, and have
**zero production callers**. `refs.ParsePRRefs`, `fanout.ClusterFanout` and `fanout.NormalizeTitle`
are referenced nowhere outside their own packages except two doc comments citing them as exemplars
(`clients/openai/openai.go:136`, `docs/go-conventions.md:48`).

This is **not** dead code awaiting deletion, and it is **not** relevant to the clustering problem.
Both facts are settled:

- They solve **GitHub-PR-shaped** problems. `fanout.Item` is `{Repo, Author, Title, Number}`
  (`fanout/fanout.go:67-72`) and groups on `(repo, author, normalizedTitle)`; `refs.refRE`
  (`refs/refs.go:33`) requires a literal `#`. Neither can match a Jira issue key like `PAAS-4001`, so
  neither can join a Jira event to a Claude Code session. Anything proposing them as the fix for
  disjoint clustering is mistaken about their shape.
- The problems are real and still expected. `rules/env-mirror-fanout.md` is live at
  `confidence: high`, drawn from predecessor operational experience, and `internal/collector/github`
  is exactly the collector this finding named as missing. Deleting working implementations of a rule
  the repo still holds would leave the rule describing nothing.

Two commits ever, both from the Python→Go port (`git log -- internal/correlator/refs
internal/correlator/fanout`), recovered from a never-merged predecessor PR — so they were never
wired in any version, rather than lost in the port.

**What remains open:** nothing in the code. CLAUDE.md's invariant used to claim these two were
load-bearing today, which was false; it now names `gatherCandidates` and `runSuppression` as the
pre-filters that actually run, and records these two as awaiting a wiring decision now that the
GitHub collector exists (see the opening paragraph above for what that decision resolved to, per
`docs/superpowers/specs/2026-09-17-github-collector-design.md`'s §6). This entry stays only so a
future reader who greps for uncalled packages finds the reasoning instead of re-deriving it.

## Status

resolved: keep, invariant corrected. Not a blocker.
