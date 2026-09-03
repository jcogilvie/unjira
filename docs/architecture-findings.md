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

### F1 — The invariant CLAUDE.md calls load-bearing describes dead code

`CLAUDE.md` states: *"The correlator's deterministic primitives run before any model.
`internal/correlator/refs` and `internal/correlator/fanout` are pure functions with no I/O and no
Jira dependency — keep them that way; they're what keeps the LLM's review queue signal-rich."*

They are pure, and they are thoroughly tested. **They have zero production callers.**
`refs.ParsePRRefs`, `fanout.ClusterFanout`, and `fanout.NormalizeTitle` are referenced nowhere
outside their own packages except in two doc comments citing them as exemplars
(`clients/openai/openai.go:136`, `docs/go-conventions.md:48`).

So the invariant is true of code that does not run, and whatever protection it describes, the pipeline
does not have. The deterministic pre-filter that *does* run is `correlator/match_candidates.go`'s
`gatherCandidates`, which the invariant does not mention.

This interacts directly with **#179**: `fanout` groups mirrored work (the 12-region-change case) and
`refs` parses PR references. Both are plausibly relevant to why the two event streams cluster into
disjoint narratives, so the two are entangled and #181 blocks #179.

### F2 — Two vocabularies are half-declared, which is incident 21 unresolved

Incident 21 established: an undeclared map key or string vocabulary is a contract nobody signed.
Both instances below are the same defect at different scales.

**Artifact keys.** Four are declared constants in `internal/events` and read through them —
`issue_key`, `status_from`, `status_to`, `tracker_record`. Four more are **bare literals written in
one package and read in another**:

| key | written | read | packages |
|---|---|---|---|
| `connection` | `collector/jira/events.go:190` | `correlator/match_candidates.go:84` | jira → correlator |
| `authored_by_unjira` | `collector/jira/events.go:191` | `reconciler/reconciler.go:324` | jira → reconciler |
| `git_branch` | `collector/claudecode/claudecode.go:238` | `correlator/match_candidates.go:88`, `:171` | claudecode → correlator |
| `ticket_keys` | `collector/claudecode/claudecode.go:239` | `correlator/match_candidates.go:200`, `pipeline/collect.go:119`, `pipeline/digest.go:34` | claudecode → correlator **and** pipeline |

`authored_by_unjira` is the sharpest case: `reconciler/reconciler.go:315-320` documents that the
artifact *"has been written since the collector landed and read by nothing — this is its first
consumer."* The repo already noticed the pattern and did not close it.

**Action statuses.** Half constants, half literals across five packages — see §3.

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

---

### F10 — a truncated pass ends with a clean-looking summary

Both `match.max_narratives_per_pass` and `reconciler.max_narratives_per_pass` (default 20) log to
**stderr** when they truncate, naming the config key. The rendered pass summary goes to **stdout** and
says nothing about the remainder.

So a pass that examined 20 of 62 narratives ends looking complete. This is not hypothetical: it is how
a 42-narrative backlog was misread as a clustering defect, and the misreading survived three drain
passes because the diagnosing session piped output through `tail`, discarding the very warning that
would have explained it.

The cap itself is right — it bounds LLM spend and blast radius per pass, and it is configurable. The
gap is that draining requires re-running, and nothing on the happy path tells an operator that.

**Candidate fix, not yet chosen:** return the remainder as data (`MatchRunResult.Remaining`, and the
reconciler's equivalent) and render it in the pass summary, so `watch` can also act on it — a loop
that knows it is behind can drain rather than sleep. Threading it touches `correlator.Match`'s return
signature, `RunMatch`, and both renderers; the cheaper alternative of having the renderer query the
store would break the renderers' no-I/O property, which is probably the wrong trade.

---

## Task cross-references

| Finding | Task |
|---|---|
| F1 — dead primitives | **#181**, which blocks **#179** |
| F2 — undeclared vocabularies | **#182** |
| F3 — concrete backend in the correlator | **#183** |
| F5, F6 — dead schema, unread artifacts | **#176** |
| F7 — connection/identity model | **#178** |
| F8 — resolver's home | **#177** |
| F9 — alphabetical candidate tiebreak | new; the real residue of #179 |
| F10 — truncated pass looks complete | new |
