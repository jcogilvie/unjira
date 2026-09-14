# Open architectural findings

Problems in the current tree that nobody has fixed yet. Companion to
`docs/architecture.md`, which describes what unjira *is* — this file lists where what it is falls
short of what it should be.

**Kept apart from `architecture.md` on purpose: the two have different lifecycles.** A description is
true until the code changes; a finding is open until somebody closes it. Mixing them means the
architecture doc is never in a settled state, and it makes "is this still true?" ambiguous — a stale
description misleads, while a stale finding sends someone to fix something already fixed.

## How to maintain this file

- **Delete a finding when it is fixed.** Not "resolved 2026-09-03" — delete it. `git log` holds the
  history, and a struck-through entry is just a stale finding with extra steps. If the fix taught a
  general lesson, that lesson belongs in `docs/design-notes.md` as a numbered incident.
- **Renumber nothing.** The `F<n>` labels are stable handles for cross-references, including from task
  descriptions and commit messages. A gap where F3 used to be is correct and informative.
- **Every finding carries `file:line`** so a reader can check whether it still holds. A finding that
  cannot be checked is an opinion.
- **State the consequence, not just the observation.** "This is inconsistent" is not a finding;
  "a future GitHub collector would be silently invisible to this check" is.
- None of these are prescriptions. The linked tasks are where shape decisions belong.

---

## Open findings

Ordered by consequence, not by number.

### F1 — refs and fanout await a collector that does not exist yet

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
  `confidence: high`, drawn from predecessor operational experience, and the README's pipeline
  diagram lists `(GitHub)` among the planned collectors. Deleting working implementations of a rule
  the repo still holds would leave the rule describing nothing.

Two commits ever, both from the Python→Go port (`git log -- internal/correlator/refs
internal/correlator/fanout`), recovered from a never-merged predecessor PR — so they were never
wired in any version, rather than lost in the port.

**What remains open:** nothing in the code. CLAUDE.md's invariant used to claim these two were
load-bearing today, which was false; it now names `gatherCandidates` and `runSuppression` as the
pre-filters that actually run, and records these two as awaiting the GitHub collector. This entry
stays only so a future reader who greps for uncalled packages finds the reasoning instead of
re-deriving it.

### F3 — A backend-agnostic correlator has a hardcoded Jira dependency

`internal/correlator` imports `internal/clients/jira` for one function: `IsTransportError`
(`correlator/match.go:12`, used at `:129`), which type-asserts `*jira.Error` to distinguish a
transport failure from a real 404.

This was reasoned, not accidental — `match.go:105-123` explains it at length: `tasktracker` is the
natural home, but `clients/jira` already imports `tasktracker`, so putting it there is an immediate
cycle; `correlator` was the cycle-free option.

The reasoning is sound and the consequence is still real: the classifier a *future GitHub tracker*
would need cannot recognize its errors, and `correlator` — which otherwise talks only to
`tasktracker` interfaces — has a concrete backend in its import list. This is incident 21's shape at
the package level. The comment names the cycle as the blocker, which is the actionable part.

### F5 — Three tables are created and never used

`ledger` (`store.go:174`) and `estimates` (`store.go:164`) are in the schema. **No Go code reads or
writes either** — the only references are the schema DDL and the package doc comment listing them.
Both are phase-2 placeholders; `tasktracker.go:121` confirms the estimate path is deliberately
unbuilt.

Not a defect in itself. Worth naming because schema is the most-read description of what a system
stores, and a newcomer counting tables will over-count what unjira does by three.

### F6 — Six artifacts are written and never read

`cwd`, `session_id`, `started_at`, `user_message_count` (claudecode), `field`, `project_key` (jira)
have zero production readers (verified). `field` has a doc comment claiming it *"distinguishes a
description edit from a summary edit, which nothing else records"* — true, and nothing reads it.

Cheap to keep and genuinely useful when re-enriching (**#176**). Listed for completeness, not as
something to remove.

### F7 — config.JiraConnection carries four concerns

One struct (`config/config.go:57-86`) holds: **endpoint** (`Site`), **identity** (implicitly, via
`Name` → credential lookup), **read scope** (`ProjectKeys`), **write scope**
(`WritableProjectKeys`), and **collection config** (`Queries`, `MaxIssuesPerQuery`).

One part of this is not up for reconsideration: **write-scope separation is a safety property.**
`WritableProjectKeys` deliberately does not default to `ProjectKeys`, because defaulting would arm
every readable project for writes the moment a connection is configured — the exact bug write scope
exists to prevent. Collapsing the two fields would undo that, spec and all.

The rest is a genuine open question. *Identity* is the one concern with no field of its own: it rides
on `Name`, which is simultaneously the config key, the cursor key prefix, and the credential lookup
key. One string doing four jobs is why renaming a connection has non-obvious consequences.

**#178** asks whether this model wants a kubeconfig-like shape (contexts referencing a server and a
user by name, with per-endpoint auth). No recommendation here — deliberately, so the question stays
open on its own terms.

### F8 — correlator.TrackerResolver may be in the wrong package

`TrackerResolver` (`correlator/match.go:162`) is `func(connection string) (tasktracker.TaskReader, error)`
— a type owned by `correlator` whose signature is entirely `tasktracker` vocabulary. It has one
consumer today (`correlator.Match`) and **#177** proposes two more, on the reconciler and applier
paths. When it has three consumers in three packages, the resolver living in one of them is arbitrary.

At one consumer this is not worth moving. `tasktracker` is the obvious home if it grows, and #177 is
the natural moment to decide.

---

### F9 — a low-precision candidate list truncates on an alphabetical tiebreak

`gatherCandidates` (`internal/correlator/match_candidates.go:55-122`) ranks issue-key candidates by
provenance — Jira event, then branch name, then first-mention prose, then later-mention prose — and
truncates to `match.max_candidates_per_narrative`. Within the `ProvenanceProseLater` tier, ties break
**alphabetically**, which has no relationship to relevance.

Measured on real data: narrative 60 is a Claude Code session whose `ticket_keys` holds **67
text-scraped keys** (including `UTF-8`, `Z0-9`, `CP-01`..`CP-15`). Running the real ranking logic
against it, `PAAS-4001` — a key with genuine recent Jira activity — lands at **rank 41 of 65** and is
truncated away, while alphabetically-earlier noise survives.

The collector is right to scrape broadly and defer judgment (that is the dumb-collector invariant).
The defect is that the *discriminator* is alphabetical.

**Candidate fix, not yet chosen:** a deterministic corroboration tier ranked between `JiraEvent` and
`ProseFirst` — promote a prose key whose issue has Jira activity inside a bounded recent window. It
stays deterministic and pre-model, and the window is a new config knob independent of the clustering
window. Cost when wrong: a false join costs one `GetIssue` verification plus a classifier call that
can assign `mentioned`; a missed join is exactly today's behaviour. It changes
`gatherCandidates`' signature, so `match_candidates_test.go` needs new cases — a real behaviour
change, honestly flagged rather than sold as a refactor.


## Task cross-references

| Finding | Task |
|---|---|
| F1 — refs/fanout await the GitHub collector | resolved: keep, invariant corrected. Not a blocker. |
| F3 — concrete backend in the correlator | **#183** |
| F5, F6 — dead schema, unread artifacts | **#176** |
| F7 — connection/identity model | **#178** |
| F8 — resolver's home | **#177** |
| F9 — alphabetical candidate tiebreak | new; the real residue of #179 |
| F10 — truncated pass looks complete | resolved: the remainder is data on `MatchRunResult`/`ReconcileRunResult`, counted in `internal/pipeline` and rendered on stdout. The finding framed this as a choice between threading `correlator.Match`'s signature and giving the renderers I/O; both were avoidable, because the layer that already does store I/O is the one holding the result struct. |
