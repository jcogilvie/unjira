# Shared context — one event, several narratives — design

## Status: slice 1 landed 2026-10-02 — gate passed, but the feature is not yet USED

Slice 1 (§"First slice", items 1–6) is implemented. Slices 2 and 3 are not.

**§9 measured on review, 2026-10-02.** Both arms used one frozen copy of this repo's transcripts and
GitHub repos (exclusions off), collected once per build into identical event sets (116 events in the
window `[2026-09-16, 2026-10-02)`; the event lists were diffed). Six reps per arm: three single-pass,
three two-pass with `SHARED_CUT=2026-09-21T00:00:00Z`, each on a fresh copy of its arm's snapshot.

| | treatment (slice 1) | baseline (`main`) | verdict |
|---|---|---|---|
| M2 anchor↔PR coalescing | 21/21 in all 6 | 21/21 in all 5 | no regression |
| M3 PR integrity | 26/26 single, 24/26 two-pass | identical | no regression (see F43) |
| M4 context links written | **0, 0, 0** single; 18, 0, 4 two-pass | — | **the model rarely uses `context_indices`** |
| M4 disputes | 0 in every rep | — | never triggered |
| M5 completion (single) | 12,595 / 12,812 / 13,911 | 10,681 / 27,725 / 32,000† | steady; inside baseline's range |
| M6a delta exclusivity | 0 violations (18 and 4 multiply-linked events checked) | — | holds |
| M7 attraction | **0** into a narrative holding the other's work as background, all 3 | 0, all 3 | **gate passes**, non-vacuously in the 2 reps with links |
| member confidence | median 0.85–0.90, min 0.30–0.55 | — | recorded; floor stays 0 until calibrated |

† The 32,000-completion baseline rep died: the model emitted malformed JSON (`invalid character '}'`)
and the pass failed loudly, as designed — see F44.

**Verdict: safe, and inert.** Every invariant held and nothing regressed. But M1 — the whole point —
did not move: member-or-context coverage equalled member-only in every rep (e.g. 13/22 = 13/22),
because the model almost never attaches context. **The cause is the evidence, not the prompt.** The
root segment that did the work behind #69–#74 summarizes itself as *"Opened with: 'i think the
failsafe impl is kind of orthogonal…' Did: committed, created a branch, opened a PR, updated a PR, ran
tests."* — nothing in it says which work it touched. The store knows deterministically that the same
session opened #67–#75 during that segment's span (the PR anchors carry `session_id`), but that never
reaches the prompt. No model can judge relevance it cannot see. That is slice 2's job (lineage), and a
cheaper first step is to name, in a root segment's own summary, the PRs its session opened during it.

Two pre-existing problems surfaced, identical on both arms, and are filed rather than fixed: **F43**
(a PR whose lifecycle events land in different passes splits into two narratives — #72 and #73 in all
six two-pass reps) and **F44** (one malformed model response kills the whole pass). The harness is `internal/pipeline/shared_probe_test.go`, whose header gives the exact
commands for a treatment rep, a two-pass M7 rep (`SHARED_CUT`), and the baseline arm. The baseline
arm runs the same metric code on `main`, which was verified by compiling both probe files against
`main`'s tree.

**F43 fixed after slice 1, by pre-assignment rather than a hint.** Before clustering,
`internal/pipeline/preassign.go` joins an unplaced event to a narrative when its
`events.ArtifactPullRequest` matches a member of exactly one open narrative. The join writes a member
link at confidence 1.0 with `member_placement = 'identity'` and moves `window_end`, and the event never
reaches the model. Zero holders, two or more, or a holder that is not open all fall back to the model
and are reported. The artifact became host-qualified (`<host>/<owner>/<repo>#<N>`) in both writers, so
the baseline arm's stored values are host-less and the treatment arm's are not. M3 keys on the raw
value, so each arm is self-consistent. The probe now prints, per pass, what identity placed and every
fallback (`printPRIdentity`), and M3 counts identity placements. M4's confidence distribution counts
only the model's placements. The two-pass M3 re-measurement (expected 26/26) needs a credentialed run
and has not been made. See `docs/architecture-findings.md` F45 and F46 for what the join leaves open.

Verified offline, in the worktree:

```
go build ./...                                  exit 0
go vet ./...                                    exit 0
go vet -tags=live ./internal/live/              exit 0
go test ./... -count=1                          26 packages ok, 0 FAIL
                                                (-v: 1319 PASS incl. subtests, 4 SKIP:
                                                 the three env-gated F16 probes and
                                                 TestSharedContext_AcceptanceRep)
golangci-lint run --build-tags=live ./...       0 issues.
```

The three drills §"First slice" names, each run against the committed implementation and reverted:

| Drill | Failed |
|---|---|
| drop `kind = 'member'` from `linkedSinceLastAction` | `TestReconcile_AContextLinkIsNotNewWork` ("a context link is never delta", then "drafting for narrative 1: parsing draft response"); also `TestContextLink_ChangesNeitherBacklogCount`, `TestMemberReaders_NeverSeeContext` |
| restore last-writer-wins (no dispute re-ask, and `Persist`'s duplicate-member backstop off, so `MoveMember` applies in result order) | `TestRunNarrate_DoubleAssignmentPersistsOneHomeAndNoEmptyNarrative`: `"0" is not positive — narrative 2 must not be left empty (F37)` |
| replace the dispute re-ask with first-in-response-order | `TestCluster_DisputeWinnerIsTheModelsChoiceNotResponseOrder/the_later_claimant`: `[]string{"github/pr-7-merged"} does not contain "github/pr-7-opened"` (the `the_earlier_claimant` subtest passed, as it should) |

F36 and F37 are closed by this slice (see `docs/architecture-findings.md`). F39 and F40 are new findings
recorded there.

**Where the implementation deviates from, or had to decide what, this design left open:**

- **The dispute re-ask has its own index space.** §4's example answer is `"event_index":7`, a
  clustering-prompt number. But the re-ask runs once per pass AFTER a bisection merges its halves, which
  is what lets it resolve F36. At that point there is no single clustering prompt, and one event has a
  different number in each half. So each disputed event is numbered in the dispute prompt itself
  (`event_index=0..K-1`), and that one slice is both rendered and parsed, which is the property
  `assignableEvents` keeps. `cluster_position` names a position in the pass's merged result list.
- **"At most one per pass" holds; "reusing #79's machinery" is partial.** The dispute call reuses
  `withRulesAndInstruction`, the grouping criterion, `estimateTokens` and the loud over-budget refusal.
  It cannot reuse #79's verbatim-first-prompt trick, which is per call, for the reason above. Its
  prompt is unbounded (F39).
- **Evidence: PR and branch only, no lineage.** §4 lists lineage artifacts as possible evidence, but
  §"What follows" and F6 reserve those artifacts' reader for slice 2. Slice 1 presents only "this event
  carries a claimant's pull request" and "this event's git branch is the head branch of a claimant's
  pull request". The first gives `events.ArtifactPullRequest` its reader, and F6 is narrowed.
- **`confidence` is required only on a cluster with `event_indices`.** A context-only `extends` places no
  member, so its confidence would attribute nothing. The omission re-ask's items also carry a confidence,
  and events it joins to an existing cluster keep the re-ask's confidence as a per-event override. Every
  merge of two results (bisected halves, the same-story check, the omission re-ask) keeps each judgment's
  own confidence the same way, through one primitive.
- **The omission re-ask accepts `context_indices`**, with no restriction to the omitted events, since a
  context link moves nothing. Its system prompt carries the same context and summary rules as the first
  call.
- **The CHECK is stricter than §1's.** §1 wrote `kind = 'context' OR member_confidence IS NOT NULL`. The
  schema also requires NULL on context, `0..1` on members, and an explicit `IS NOT NULL`, without which a
  CHECK passes on NULL (design-notes #44).
- **Persist applies every member placement before any context link.** §4 defines the two operations,
  not their order. Response order would lose a context link whenever an `extends` adding background to
  narrative A came before the result moving A's member away (`AddContext` is a no-op while A still holds
  the event as a member). `Persist` also refuses an event two results claim as a member before any write,
  so last-writer-wins is unreachable even for a caller that bypasses `Cluster`.
- **Merge's moved members get `store.ReviewerMemberConfidence` (1.0).** §6 does not say. The reviewer has
  ruled the source's work is the target's, which is a human attribution, and a reviewer-placed link must
  not resurface as one to confirm.
- **Triage surfaces below-floor members read-only.** §1 says they are "surfaced in triage as an
  attribution to confirm" and names no verb. The review header lists them, with confidence, under the
  action's narrative. A reviewer corrects one with the existing merge or split. There is no confirm verb.
- **An emptied split source loses ALL its context links, frozen ones included.** §6 says they are deleted
  when the split empties the source. §4's table says a frozen context link "stays". The two meet only
  when every member of a committed narrative was linked after its last commit, and §6's rule was applied.
- **`NarratedNarrative.Events` is read back too**, not only `ContextEvents` (§"First slice" item 6),
  restricted to this pass's placements, because F37's instrument trap was about members.
- **`correlator.member_confidence_floor` is validated to `[0, 1]`**, not just non-negative, on
  `match.confidence_floor`'s precedent: above 1 would flag every member.
- **§9's M7 baseline is underspecified.** The baseline arm has no context links, so it has no `B ← e` pairs
  to count attraction for. The probe therefore reports an arm-independent screen (pass-2 member
  placements whose own PR/branch evidence points at a different pass-1 narrative) on both arms, plus
  the context-pair subset on treatment. Comparing those is the reviewer's call.

Step 4 of the subagent-collection work: let one event — typically a root session's investigation,
reused across several fixes — attach to more than one narrative, as **context**, without becoming
any of them's **work**. This is the reader `docs/architecture-findings.md` F6 names for the subagent
artifacts written on 2026-10-01. §"First slice" says what to build first (now landed, see Status)
and §"What follows" what comes after, with the condition that gates each.

## The problem, restated precisely

The user develops subagent-first. One problem cascades into several issues and PRs, issues are filed
as they are discovered, and the context assembled for one fix is reused for the next. A transcript is
therefore not a linear, one-issue-at-a-time timeline: the investigation in a root session is
legitimate evidence for every issue it produced.

That is **relevance, not ownership**, and the distinction is the design. "This investigation is why
PR #73 exists" is true of #70 and #73 at once. "This investigation is #73's work" can be true of at
most one of them, because the reconciler turns a narrative's work into prose on that narrative's
ticket — and the highest-stakes failure available here is one shared paragraph drafted onto three
tickets. So the relation needs two kinds of link, and only one of them may ever reach a tracker.

Relevance is a judgment, so it is the correlator's, made by the model in `Cluster`. Collectors stay
dumb (CLAUDE.md's first invariant): the claudecode collector records who dispatched whom
(`parent_session_id`, `dispatch_tool_use_id`, …) and asserts nothing about what that means.

## What was measured, and what checking it against the code found

### The 2026-10-01 measurement (as reported; the store it ran against no longer exists)

A real store, three clustering runs, with subagent transcripts and PR anchors present:

- Anchors coalesced with their GitHub PR every time (20/20, 21/21, 21/21); no PR was split.
- PR narratives with any transcript evidence: **12–15 of 20–21**.
- The remaining gap is **root-session context**: one root segment covers work for several PRs and
  can join only one narrative.
- Unprompted, the model put one subagent transcript (the F16 investigation) in **two** narratives,
  #70's and #73's. Its work did feed both.

I could not re-run it. The store was a temp copy that is gone, `data/unjira.db` is currently empty
(0 events), and a worktree has no credentials (design-notes #37). The PR range is recoverable: PRs
#58–#78 were created between 2026-09-16 and 2026-10-01, which is exactly 21 PRs (20 merged, #63
closed) — consistent with "20–21". §9 names that window.

### Five places the measured picture is incomplete or wrong in a way that changes the design

**1. The one-narrative rule does not live only in the prompt, the parser and `relinkEvents`.**
The prompt does say "assign each to exactly one cluster" (`correlator.go:437`). The parser does
**not** enforce it — `parseClusterResponse` (`:465`) resolves every index it is given and never checks
for repeats, which is why the double assignment reached `Persist` at all. And four more places depend
on the rule, each of which breaks silently — no error, wrong rows — once an event can hold two links:

- **Triage split depends on last-writer-wins.** `markSourceIfEmptied` (`triage/split.go:121`) says
  so: *"Verified by probe that Persist's relinkEvents empties the source."* Remove the unlink and a
  split leaves every event on the source as well as on the new narratives, and the source is never
  marked `split`.
- **Triage merge adds before it unlinks, through `INSERT OR IGNORE`** (`restructure.go:170`,
  `narratives.go:201`). With two kinds of link, a target that already holds an event as context keeps
  that row, the source's work link is then deleted, and the event has lost its home — no error.
- **`UnlinkedEventsInRange`** (`narratives.go:285`) selects "no link at all", and its doc comment
  states the rule outright: *"an event belongs to exactly one narrative, so once linked it is never a
  candidate again."*
- **Four link-sequence watermarks treat every new link as new work**: the reconciler's delta
  (`linkedSinceLastAction`, `narratives.go:421`, shared by `DeltaEvents` and `hasUnexaminedDelta`),
  the match watermark (`matchExaminationPredicate`, `matchwatermark.go:51`), and the reconcile
  watermark (`reconcileExaminationPredicate`, `reconcilewatermark.go:51`). Each would re-admit a
  narrative — at model cost — the moment it gained a context link.

**2. The persisted effect of a double assignment is worse than last-writer-wins.** Read from
`prepareOneResult` (`correlator.go:826`) and `applyPrepared` (`:953`); not executed, since this is a
spec-only change. For a response `[NEW{E}, EXTENDS 5{E, F}]`: the NEW narrative's window and summary
are computed from `E` and written; then `relinkEvents(5, [E, F])` deletes the NEW narrative's only
link. The result is an **empty open narrative** whose title and summary describe an event it does not
hold — the dead weight `NarrativesOverlapping` already excludes `split` narratives to avoid, but with
status `open`, so nothing excludes it. Recorded as finding **F37**.

**3. The run's printed output disagrees with its store.** `describePersisted`
(`pipeline/narrate.go`) renders each narrative's member events from the *cluster result*, not from
the store — so even a persisted `dev narrate` prints the double-assigned event under both narratives
while the store holds it under one. Any measurement read off the printout counts sharing that never
persisted. §9 measures from the store for this reason.

**4. `suppressDuplicates` cannot catch the failure this design must prevent.** It drops an action
when *another narrative already has an open proposal on the same issue* (`reconciler.go:429`). One
shared paragraph drafted onto three *different* tickets shares no issue key, so it passes. The
defense has to be structural and upstream (§2), not a filter.

**5. "Transcript evidence" must exclude anchors, or the metric is trivially perfect.** Anchors are
`claude_code` events and coalesced with their PR 100% of the time, so a PR narrative containing its
anchor "has transcript evidence" by construction. The 12–15 figure must have meant segment events
(branch runs), not anchors; §9 defines it that way and lists confirming the baseline's definition as
an open question. **Confirmed on review:** the scorer that produced 12–15 matched anchor lines (`opened
pull request`) in a branch evaluated *before* the generic `[claude_code]` branch, so anchors were never
counted as transcript segments. The baseline already uses §9's definition.

### Two numbers checked against a real store

Measured read-only against a copy of `data/unjira.db.pre-f30` (2026-10-01 15:15, 795 events, before
subagent collection):

- **The link table was one-to-one in practice**: 298 links over 298 distinct events, no event with
  two links, no narrative with zero links. So the schema has always *permitted* many-to-many
  (`UNIQUE (narrative_id, event_id)`), and nothing has ever *written* it.
- **Shared transcript events carry many keys**: 29 of 419 `claude_code` events carry ≥17 prose keys
  (maximum 77), and the maximum `scm_keys` on one event is **37** — the `scm_command` tier, which ranks
  above `jira_event`. §3 rests on this.

---

## Design

Vocabulary used below: a **member** link says the event is part of this narrative's work; a
**context** link says the event is relevant background for this narrative and belongs to some other
narrative's work. §1 explains why these names and not `primary`/`supporting`.

### 1. Link kinds

**Recommendation.** A `kind` column on `narrative_events`, `'member' | 'context'`, `NOT NULL` with
**no default** — the precedent `actions.created_link_seq` set: *"a default would be a guess … and an
INSERT that forgets it should fail loudly."* Plus a partial unique index mirroring
`one_primary_per_narrative` (`store.go:117`):

```sql
CREATE UNIQUE INDEX IF NOT EXISTS one_member_link_per_event
    ON narrative_events (event_id) WHERE kind = 'member';
```

**Every linked event has exactly one member home; context-only events are forbidden.** At most one is
the index above, enforced by the database. At least one is checked at `Persist`'s commit: every event
that received a context link in the transaction must hold a member link when it closes, or the pass
fails loudly. Three reasons a context-only event cannot be allowed:

- it would never be in any narrative's delta, so the work it records would never be reconciled — a
  silent drop, which CLAUDE.md forbids;
- the re-ask for omitted events (branch `correlator/reask-omitted-events`, assumed landed) guarantees
  every event is *assigned*; "assigned" must mean "has a home", or the guarantee is satisfied by an
  event that has none;
- the create path's untracked-work detection reads member events; an event nobody owns could never be
  proposed as untracked work.

The rejected alternative is supporting-only for "pure investigation that produced no work of its
own". But an investigation *is* work evidence, and unjira already has a home for it: a narrative of
its own, or membership of the narrative it most belongs to. The cost of forcing a choice is that one
ticket hears about the investigation as work and the others see it as context — which is today's
behaviour for the one ticket and strictly better for the others.

**Who assigns kinds: the model, in `Cluster`'s output** (decided with the user, 2026-10-02). Each
cluster gains an optional `context_indices` beside `event_indices`, resolved against the same numbered
slice, and a required `confidence`:

```json
[{"kind":"new","title":"…","summary":"…","confidence":0.85,"event_indices":[3,4],"context_indices":[0]}]
```

The prompt's contract becomes: every numbered event appears in **exactly one** cluster's
`event_indices` — that placement IS its member home — and may additionally appear in any number of
other clusters' `context_indices`, when it is genuinely the background for that cluster's work.
Membership is therefore an explicit answer, never inferred from response order. An event in two
clusters' `event_indices` is a contract violation, resolved by the dispute re-ask in §4.

**Why membership has to be right, not just consistent: token attribution.** Member links are the unit
of attribution — a session's tokens are charged to the workstream the event is a member of, and
context links carry **no** attribution. Phase 2's estimation ensemble takes "observed effort" as an
evidence framing, so a wrong member link is wrong estimation data, not merely a mis-filed event.

**`confidence` on every cluster, applying to its member placements.** One number per cluster (~5
tokens × ~25 clusters a pass), stored on each member link it produced as
`narrative_events.member_confidence` (`REAL`, `CHECK (kind = 'context' OR member_confidence IS NOT
NULL)`; context links carry none). Per-cluster rather than per-event deliberately: per-event would turn
`event_indices` from `[3,4]` into objects and cost ~5 tokens per event (~116 a pass) in the response F16
already measured against the 32k ceiling. Whether one doubtful member inside a confident cluster
actually occurs is measured (§9, M4) before paying for per-event; the dispute re-ask already yields
per-event confidence for the contested cases.

A **threshold** acts on it: `correlator.member_confidence_floor`, a plain number, default `0` (off), in
the `max_output_tokens` mould. Below it, the member link is surfaced in triage as an attribution to
confirm. The floor starts off because a model's stated confidence is not calibrated: it is set from
reviewer rulings — the same labeled data `learn` reads — not guessed up front. This mirrors the repo's
existing pattern (matching emits a confidence; `auto_commit.<type>.confidence_floor` gates on it).

**No `rationale` field in the main clustering response.** F16's sixth falsified candidate was exactly
that: a rationale ahead of every cluster produced *more* clusters (38/38/36 vs 36) and ~20% more
completion. Rationale belongs in the dispute re-ask (§4), which is small, conditional, and exists for
the reasoning.

Rejected alternatives:

- **Deterministic, from lineage.** The subagent artifacts say which root session dispatched which
  subagent. Auto-linking a dispatching root segment as context to every narrative its subagents
  joined is mechanical and plausible — and wrong often enough to matter: 42 of 79 multi-day sessions
  never change branch (F15), so one root segment can span weeks and topics, and dispatching a subagent
  says nothing about which of that segment's content the subagent's narrative needs. Lineage is a
  strong *hint*, and §"What follows" renders it as one; relevance stays the model's call.
- **Declared by a collector.** Collectors are dumb by invariant, and relevance is judgment.
- **A second, separate call after clustering** ("given these clusters, which events are background
  for which?"). This genuinely protects clustering from the prompt change — F16 measured that
  clustering is sensitive to small schema changes (reasoning-first key order made it *worse*, 38/38/36
  clusters) — and it is separable and skippable. It also re-sends the whole prompt (~100k prompt
  tokens in F16's attribution table) for a judgment the model already made inline, unprompted. **Kept as the
  fallback**, not the default, **by decision (user, 2026-10-02): inline first.** The switch is
  pre-committed rather than left to judgment: if §9 shows the inline change damages clustering — anchor
  coalescing below 100% (M2), PR integrity below 100% (M3), or cluster/`NEW` counts outside the noise
  band (M5) — switch to the separate call. Storage, persist and every reader are identical either way;
  only the producer of `context_indices` changes, so the switch is contained.

**Names: `member`/`context`, not `primary`/`supporting`.** `narrative_issues.role` already has
`primary`, guarded by `one_primary_per_narrative`, and code reads "the primary link" constantly. Two
tables one join apart, each with a "primary" meaning something different, is the vocabulary collision
incident 21 warns about at the artifact level. `member` matches what `NarratedNarrative` already calls
a narrative's events ("member events", `pipeline/narrate.go`). `work` was considered and rejected
because "work evidence" already names a different axis (`events.AnyWorkEvidence`, versus tracker
record): an old narrative's member event can be a tracker record.

**An old store is refused before any DDL.** Generalize `checkLinkSeqSchema` (`linkseq.go:64`) into a
required-columns check that also lists `narrative_events.kind`, run where it runs now — before the
schema `Exec` (`store.go:301`), whose own comment records why after is wrong: a refused open must not
have mutated the store. The message names this spec and the fix, delete the database and re-collect,
in the form the README already documents. The README's "no migrations" paragraph, which names F30 as
the most recent such change, is updated in the implementing PR.

Rejected: `ALTER TABLE narrative_events ADD COLUMN kind TEXT NOT NULL DEFAULT 'member'`. Unlike F30,
this backfill would be **exact** — every pre-feature link is a member link and there is one per event
(298/298 above). It is rejected anyway, because it would be unjira's first migration, and that is a
precedent to set deliberately, for a store someone cannot afford to lose, not inside a feature PR
against a disposable one. The same trigger F21 names applies: the first non-disposable store.

### 2. The reconciler: context is invisible, not merely de-emphasized

**Recommendation.** In the first slice, the reconciler sees **no context links at all**. Not in the
delta, not in the drafting prompt, not in the create prompt, not in redraft, not in any watermark.
Concretely:

- `linkedSinceLastAction` gains `ne.kind = 'member'`. Because it is one const interpolated into both
  `DeltaEvents` and `hasUnexaminedDelta`, the selector, the count and the delta cannot drift — the
  property F10 and F12 were about.
- `reconcileExaminationPredicate` and `matchExaminationPredicate` count member links only, so adding a
  context link re-admits nothing.
- `EligibleEvents` (redraft's delta, `rework.go:93`) and the create path's events (`create.go:173`)
  read member links only.
- `AllNarrativeEvents` is **renamed** (to `AllMemberEvents`) rather than quietly re-scoped, so each of
  its two callers fails to compile and gets revisited. Context events get their own accessor,
  `ContextEvents(narrativeID)`, which no reconciler path calls. Incident 13's lesson: make the unsafe
  read inexpressible rather than relying on each caller to filter.

Why every one of those, rather than "show context, tell the model not to describe it":

- **`suppressDuplicates` is keyed on the issue** (finding 4 above), so nothing downstream catches the
  same content drafted onto different tickets.
- **`suppressTrackerEcho` would be satisfied by somebody else's work.** It asks whether the delta
  holds any work evidence (`tracker_echo.go:54`). A shared root segment *is* work evidence, so a
  narrative whose only new event is a context link would pass it and draft a comment from another
  ticket's investigation.
- **`suppressStaleTransitions` would date this ticket's work by another ticket's evidence.**
  `newestWorkEvidence` (`recency.go:116`) takes the newest work event in the delta as the date the
  work happened. A context event dated after the last status change would license a transition the
  narrative's own work does not.
- **A prompt rule cannot enforce a structural precondition** — incident 24, and incident 30 again.
  "Use this only as background; never describe it as new work" is a phrasing rule standing in for an
  eligibility rule, and it is exactly the shape that produced 18 of 21 tracker-echo comments while
  being obeyed.

Self-authored filtering (`dropSelfAuthored`) is unchanged; it runs over a member-only delta. The gate
is untouched: this design adds no write path and no action type.

**The narrative summary may mention context, and that is desirable — but only as a reference.**
Workstreams genuinely touch: "found while debugging the cache-eviction work" is accurate, and is
exactly what Jira's issue links express (§8: it is the discovery link in prose). The design does not
try to keep context out of summaries. It guards the two places where context in a summary does harm:

- **Context never justifies a state-bearing action.** A comment on B may say B was found while working
  on A. A transition or create on B justified by A's evidence ("A's PR merged, so move B to Done") may
  not — CLAUDE.md already forbids state-bearing actions from transcript *intent*, and context is weaker
  than intent: it is somebody else's work. In slice 1 the reconciler reads no context links at all; the
  rule binds whatever reads the summary, and slice 3.
- **A summary is the retrieval key for future clustering, so it must not attract the other stream's
  work.** Each pass shows the model existing narratives' summaries and asks whether new events extend
  one. If B's summary *re-tells* A's investigation, next week's cache-eviction events have two
  plausible homes, and some land as **members of B** — misattribution of A's future work, and a token
  attribution error. Hence the summary rule below, and §9's M7, which measures exactly this.

`Cluster` writes each cluster's `summary` after seeing its `context_indices`, and the drafting prompt
opens with that summary (`draft.go:251`). Three mitigations, two of them structural:

1. Compaction folds **member** events only, so a recap never absorbs context.
2. An `EXTENDS` that carries only `context_indices` **does not overwrite the summary** (nor
   `window_end`) — there is no new work to summarize.
3. The cluster prompt says a summary describes the cluster's **member** work and refers to context
   **by reference, not by re-telling** — e.g. *"Fixed log flooding in the logger (discovered while
   debugging the cache-eviction narrative)."* The link survives; the other stream's substance does not.
   This is a phrasing rule and is expected to leak; M7 measures whether the leak attracts work, and M6b
   whether it carries content onto another ticket.

### 3. Matching: context links do not feed it at all

**Recommendation.** `gatherCandidates` (`match_candidates.go:83`) reads member events only, at every
tier. Context events do not reach matching's classifier prompt either, and do not re-admit a narrative
to matching's backlog.

The numbers decide it. A shared root segment routinely carries ≥17 prose keys (29 of 419 events,
maximum 77) and up to 37 `scm_command` keys, a tier that outranks `jira_event`. Fed at full
provenance, every narrative it supports inherits all of them.

The obvious compromise — a new lowest tier, `context`, below `prose_later` — is rejected on one line
of `resolveVerified` (`match.go:434`): **a lone verified candidate becomes primary deterministically,
at confidence 1.0, with no model call.** A PR narrative whose own events name no ticket, holding one
context event that names one real ticket, would be attributed to that ticket with certainty. The tier
would be weakest in the ranking and strongest in effect. It would also spend a `GetIssue` per key per
narrative.

Showing context events to the classifier *without* making them candidates is plausible — they might
help it tell the ticket a narrative implements from one it only mentions — and unmeasured. Left open.

One consequence worth stating so nobody "fixes" it: F32 records that subagent work has no branch-tier
provenance, because a subagent's `gitBranch` is its parent's. A context link to the parent's root
segment must not become the route that restores it — the branch on that segment names the
*parent's* work, which is exactly why the collector stopped emitting it.

### 4. Persist: "move this event" and "add another home" are different operations

**Recommendation.** `relinkEvents` (`correlator.go:943`) is replaced by two operations with different
names, so no caller can express one while meaning the other:

- **`moveMember(narrative, event)`** — `event_indices`. Delete the event's member link wherever it is,
  then insert a member link here. Delete first, because the partial unique index rejects a second
  member row. The link being deleted is eligible by construction (§5: only an eligible member link is
  numbered); finding a frozen one is a bug and an error, never a silent move. If this narrative already
  holds the event as a member, keep that row and its `link_seq` — the existing `INSERT OR IGNORE`
  behaviour `TestPersist_ExtendRelinkingAFrozenEventKeepsItFrozen` pins. If it holds the event as
  context, replace that row with a member row, with a **new** `link_seq`.
- **`addContext(narrative, event)`** — `context_indices`. If this narrative holds the event under
  either kind, do nothing (member wins; a repeated context link keeps its position). Otherwise insert a
  context link. **Never deletes anything.** Context links held by other narratives are untouched by
  both operations.

So last-writer-wins does not survive for intentional sharing. The unintentional case — one event in
two or more clusters' `event_indices` — is resolved by **asking the model which workstream it is
primarily the work of**, not by a positional rule. An earlier draft kept the first cluster in response
order as the member; that was rejected because response order has no logical bearing on which
workstream an event belongs to, and membership drives token attribution.

**The dispute re-ask.** After parsing, every event with more than one member placement is collected.
If any exist, **one** follow-up call (reusing #79's re-ask machinery; at most one per pass) presents, per
disputed event: the event, the clusters claiming it (title, summary, member events), and any
**deterministic evidence** the store holds — e.g. the event carries the PR anchor or pushed the branch
of one claimant's PR, or the subagent lineage artifacts (F6) connect it to one claimant's dispatch.
Evidence is presented to the model, never applied by code: relevance stays the model's call, per
CLAUDE.md. The model answers per event, **rationale first** so the decision follows the reasoning:

```json
[{"rationale":"…","event_index":7,"member":{"cluster_position":2},"confidence":0.9}]
```

The chosen cluster keeps the member link, with the dispute's per-event `confidence` as its
`member_confidence`; every other claimant gets a **context** link. Loud errors, never best-effort: an
event left unresolved, a `member` naming a non-claimant, malformed JSON. If the resolution would leave a
`NEW` cluster with no member events, the response is malformed and the pass fails loudly, naming both
clusters (F37). Disputes and their outcomes are counted on `Stats` and printed when non-zero.

**Why the upgrade issues a new sequence number.** A link's `link_seq` must mean *when it acquired its
current kind*. The delta asks "is this new **work** for this narrative", and an event that has been
background since last week but became this narrative's work today is new work today. Keeping the old
position would put it below the last action's `created_link_seq`, and the reconciler would never
draft about it — silent loss, the kind incident 40's sequence exists to prevent.

**The freeze rule is per link and kind-agnostic, unchanged.** `linkedSinceLastCommit`
(`eligibility.go:32`) still compares one link's position against its own narrative's last applied
action. What that means for each combination:

| Situation | Meaning |
|---|---|
| Frozen member on A, new context link on B | Fine, and the common case. A's posted comment described `E`; B's drafts never see `E` (§2), so nothing claims it twice. |
| Frozen member on A, model wants `E` as B's member | Unreachable: a frozen member is never numbered (§5), so `E` has no index to place. |
| Eligible member on A, frozen context on B, model makes it B's member | Allowed: delete A's member (eligible), replace B's context row with a member row. Nothing committed is reattributed — B's posted comment drew on `E` as background, and unlinking background un-says nothing. |
| Frozen context link on A, A is restructured | Stays, like any frozen link. |

Incident 13's laundering shape needs a frozen *member* link to move. Every path above that moves a
member link requires it to be eligible, checked where the move happens.

**Windows come from member events only.** A context event dated weeks earlier would otherwise widen
every narrative it supports, `NarrativesOverlapping` would return them for more windows, and context
hydration — F16's cost — would grow with sharing.

`requireNonEmptyClusters` (`pipeline/narrate.go:217`) changes accordingly: a `NEW` cluster needs at
least one member event; an `EXTENDS` needs at least one member or one context event.

**Coupling with the re-ask branch.** Its notion of an omitted event must be "appears in no cluster's
`event_indices`". Counting a `context_indices` mention as assignment would let an event with no home
satisfy the guarantee — the context-only case §1 forbids. Whichever lands second must check this.

### 5. Context hydration and the index space

**Recommendation for the first slice: one index space, collision-free by a database constraint, and
nothing new numbered.**

The numbered slice stays exactly what it is — `assignableEvents` (`correlator.go:383`), shared by
`buildClusterPrompt` and `parseClusterResponse` so they cannot drift — with one tightening of what goes
in it:

1. in-window events with **no member link** (`UnlinkedEventsInRange` asks that question, not "no link
   at all" — the question it means, per incident 28), then
2. each context narrative's **eligible member** links, in `existing` order.

Both `event_indices` and `context_indices` resolve against that one slice. No event can appear twice
in it: an in-window event has no member link, each event has at most one member link
(`one_member_link_per_event`), and context links are not numbered. `assignableEvents` still **checks**
for a repeated `(Source, ExternalID)` and returns an error rather than deduplicating. A duplicate would
mean the invariant had broken, and deduplicating would hide that, which is the shape of every
`assignableEvents` incident so far — prompt and parser numbering different slices, silently.

`correlator.Narrative` gains `ContextEvents` beside `Events` (frozen members) and `EligibleEvents`
(eligible members). `hydrateContextNarratives` partitions on kind as well as eligibility. The prompt's
context section renders them under their own heading. A context event that is *also* in the numbered
slice — `E` is eligible member of A and context of B, both in context — renders under B as a
back-reference, `-> #7`, from a map built off the same assignable slice. That ties the two renderings
deterministically and costs a few tokens instead of a repeated summary.

**What this deliberately cannot do: share a frozen event.** Once a root segment's member home has an
applied action, the segment is frozen, so it has no number and can be shown but not linked. Frozen
events become shareable in the second slice, through a **second** index space: `context_refs`, labels
`c0..cK` assigned once per distinct event in first-appearance order across the whole prompt, so every
rendering of one event carries one label. Both slices still come from one function returning both,
which is the property `assignableEvents` exists to keep. Deferred because the measured gap is a
fresh-store single pass, where nothing is frozen, and because a second numbering is exactly where
prompt/parser drift lives.

### 6. Triage merge and split

**Merge** (`MergeNarratives`, `restructure.go:157`) moves the source's eligible links with the §4
operations, by kind: an eligible member uses `moveMember` (and upgrades a target context row), an
eligible context link uses `addContext` and is then deleted from the source. Direction-by-commitment is
unchanged; frozen links stay on the source, as now. The ordering bug in finding 1 — add through
`INSERT OR IGNORE`, then unlink — is the first test the implementation writes: today's order fails the
new unique index on the ordinary case and silently drops the event's home in the context case.

**Split** (`SplitNarrative`) re-clusters the source's eligible **member** links only. The resulting
`Persist` uses `moveMember`, which deletes only the source's member link (the split's `Cluster` call
gets no context narratives, so everything numbered is the source's). `context_indices` between the
halves are allowed — the investigation in one half may genuinely be background for the other.

The source's own context links: kept if the source retains any member link, deleted if the split
empties it of members, and counted in the split's report either way. No data is lost by deleting them,
since by §1's invariant each such event keeps its member home elsewhere. `markSourceIfEmptied`
(`split.go:121`) counts member links, or a source holding only background would never be marked
`split` and would sit in every later prompt as a narrative with no work.

Rejected: letting the split prompt redistribute the source's context links to the halves (more model
judgment inside a reviewer-attended command, for background nobody asked about), and copying them to
every half (it over-shares, inflating exactly what §7 bounds). Also rejected: refusing to merge or
split any narrative involved in sharing. That is the cheapest implementation and it makes the
reviewer's correction impossible precisely on the narratives this workflow produces most —
`EligibleEventIDs`' own doc comment rejects a narrative-level freeze on the same grounds.

Retarget and edit are unaffected beyond §2's member-only redraft delta.

### 7. Cost, and the F16 ceiling

**What the first slice adds.** Prompt: one line per context link in each context narrative's context
section, most of them back-references. Completion: the `context_indices` arrays, a few tokens per
index. **What bounds it:** a context link can only reference an event already in the prompt, so no
event enters a clustering prompt because of this design — the payload F16 attributes grows by
references, not by events. Matching and the reconciler add **zero** calls, because §2 and §3 make
every watermark ignore context links. That is a testable property, not an estimate: adding a context
link must not change `CountNarrativesWithDelta` or `CountNarrativesWithoutPrimaryLink`.

**What it does to the ceiling.** F16 measured completion tracking cluster count, with a 34%
run-to-run spread, and a 365-day window already dying at 32,000 completion tokens. Context indices
make that marginally worse and change nothing about F16's status. The magnitude is unmeasured; §9
measures completion per cluster with and without it.

**Three growth paths, and why the first slice adds no cap for them:**

- **Accumulation between compactions.** Compaction folds member history; context links on a
  long-lived narrative accumulate. The compaction boundary does bound them — `NarrativeEventsForContext`
  filters every link by event position, whatever its kind — but only once member history trips the
  threshold.
- **Resumed and growing sessions (F33).** A growing root transcript re-emits every segment under a new
  `ExternalID` (size is part of it), and each snapshot can collect its own context links, so links grow
  as snapshots × narratives. Not created by this design; multiplied by it.
- **Bisection.** A pass that splits its window numbers each half separately, so sharing cannot cross
  the seam. That under-shares and never mis-shares. The measured pass should not bisect.

No `max_context_links_per_event` knob. Incident 27: a knob whose correct value is a band nobody can
calibrate is a latent bug with a default, and there is no measurement to calibrate it against yet. The
first slice reports the distribution instead: context links per pass, distinct events shared, and the
largest fan-out per event. The cap waits for a number that says it is needed.

### 8. Discovery links

Phase 2's "spin-off tickets with discovery links" — issue B discovered while working on A — relates
**narratives** (and, when applied, issues), with a direction and a time order. This design relates
**events** to narratives with no direction. They are different relations and should stay different
records.

But this design produces most of what a discovery-link pass would need, by derivation: B holds a
context link to `E`, `E`'s member home is A, and A's window opens before B's. That is a candidate
"B was discovered while working on A", computable from rows this design writes — and §2's summary rule
already states it in prose ("discovered while debugging …"). Deriving it structurally later is what
lets it become a real tracker link (Jira's "relates to" / "found while") instead of a sentence. So
**record nothing new now**:

- Rejected: a `narrative_relations` table now. It would be written with no reader (F6's shape), and
  incident 28 records what a denormalization with no reader costs. Derive, don't store.
- Rejected: a third link kind such as `discovered_from`. Direction is a separate judgment from
  relevance, and the matching vocabulary already decided the analogous question:
  `discovered_while` was considered and folded into `mentioned` (`match_types.go`) because it had no
  behavioural difference. A discovery link will have one — a tracker write — so it deserves its own
  design, not a value squeezed into this column.

One thing the design must do to keep that derivation possible: **context links are not deleted in
routine operation**. They are only removed by a split that empties their narrative, or moved by a
merge. Not by compaction, not when the member home commits.

### 9. Acceptance measurement

**Where and what.** Run where the credentials live (the main checkout's `.env`), never from a
worktree (#37). A temp store, `db_path` pointing at it, never `data/unjira.db`. This repo's own
transcripts with `collectors.claude_code.exclude_cwds` set to `[]` ("exclusions off"), the GitHub
collector reading `jcogilvie/unjira` (reading the real repo is fine; only mutating it is not), the Jira
collector off. **Window: `[2026-09-16T00:00:00Z, 2026-10-02T00:00:00Z)`**, which holds PRs #58–#78.

**Procedure.**

1. Collect **once**, with every collector's `backfill_days` reaching 2026-09-16 (the example config's
   14 does not, and a short reach silently shrinks the corpus — #41's shape). Record, before
   narrating, the root and subagent transcript files on disk with activity in the window against the
   distinct transcripts collected. Snapshot this un-narrated store.
2. Arms: **baseline** = `main` with the re-ask branch landed; **treatment** = the first slice. ≥3 reps
   per arm, each on a **fresh copy of the snapshot** — never re-collected, because live transcripts keep
   growing, and never chained, because a rep must not start from another rep's output (#35).
3. Each rep narrates the window in **one persisted pass**, through an env-gated probe in the
   `f16_probe_test.go` mould, taking an **absolute** window. `dev narrate --since` is relative to now,
   so the window would drift between reps. Not `--dry-run`: #38's placeholders, and finding 3 above —
   a dry run renders cluster output, not what persisted.
4. Every metric is read from the store with SQL, never from printed output.
5. The harness refuses to report a number from a failed run (#37): it prints `DIED` and the error
   instead of zero.
6. Optionally, a second scenario of one pass per day, which exercises sharing across passes through
   eligible members. Reported, not gating.

**Metrics.** Ranges across reps, not just means.

| | Metric | Baseline | Acceptance |
|---|---|---|---|
| M1 | PR narratives with transcript evidence: narratives holding a GitHub PR event in the window **and** at least one `claude_code` segment event that is not an anchor (no `anchor_kind`). Reported twice: by member links only, and by member or context. | 12–15 of 20–21 (re-measured, per #36) | member-or-context rises, with ranges not overlapping baseline's; **member-only does not fall**, or the gain is reshuffling rather than sharing |
| M2 | Anchor↔PR coalescing: every anchor with `pr_create_outcome=created` has the same member home as the `:opened` event carrying its `ArtifactPullRequest` | 100% (20/20, 21/21, 21/21) | 100% in every rep |
| M3 | PR integrity: each PR's GitHub events share one member home | 100% | 100% in every rep |
| M4 | Sharing structure: context links, distinct events shared, maximum fan-out, disputed member placements and how the re-ask resolved them, the `confidence` distribution, and how often a confident cluster holds a member the dispute re-ask or a reviewer later moves (the per-event-confidence question in §1), invariant violations | — | violations = 0; the rest reported |
| M5 | Cost: prompt tokens, completion tokens, completion per cluster, cluster count, `NEW` count | measured | completion range overlaps baseline's or the excess is attributed (#32: zero the context rendering and the context indices one at a time); cluster count inside noise — #39's damage column, not only the cost column |
| M6a | Delta exclusivity, deterministic, no model: for every event with ≥2 links, how many narratives' `DeltaEvents` / `AllMemberEvents` / redraft delta contain it | — | exactly 1, for every such event |
| M6b | Cross-ticket content, model in the loop: below | — | 0 confirmed duplications |
| M7 | **Attraction across passes** — does a context link pull the other stream's future work in? Below | — | **0** of A's later events become members of B; **gates slice 1** |

**M6b in detail.** Reconcile drafts only against linked issues, and this repo's work has no Jira keys.
So on each treatment rep's store, with `tracker.backend: local`: seed one local issue per PR narrative
(`store.InsertLocalIssue`, titled from the PR), link it as that narrative's primary with
`ProvenanceReviewer`, run `Reconcile` with the real model, and collect every proposed comment. Nothing
reaches a real tracker — the local backend and the review queue are the whole write surface, and no
apply runs. For every pair of comments on *different* issues whose narratives share a context event,
compute a word-shingle overlap. Calibrate the threshold on the **baseline** arm (same seeding), whose
pairs share no context links, to learn the background similarity of this repo's comments to each
other. Then a human reads every pair above threshold, and a sample below it, asking one question:
*does the comment on the context-holding ticket describe the shared event's work as that ticket's
work?* The automated overlap is a screen; the reading is the verdict. A confirmed duplication — most
likely via the summary leak named in §2 — fails acceptance and goes to slice 3's question, not into a
prompt patch.

**M7 in detail — the merge-the-workstreams failure.** §2's second hazard: B's summary mentions A, and
because summaries are what clustering matches new events against, A's *later* work lands as members of
B. M6b cannot see this; it looks within one pass. M7 needs two. Split the acceptance window at a cut
point inside it: run pass 1 over the events before the cut, persisting (not `--dry-run` — pass 2 must
see pass 1's narratives as context), then pass 2 over the events after it. For every pass-1 context
link `B ← e` (where `e`'s member home is A), take pass-2 events that a reviewer — or, as a screen, the
deterministic evidence of §4 (anchor, branch, lineage) — attributes to A's stream, and count how many
pass 2 placed as **members of B**. The baseline arm (no `context_indices`) gives the background rate
of such misplacements, since misclustering exists without this feature; acceptance is no rise over
baseline, with ranges across ≥3 reps. A rise fails slice 1 and is fixed in the summary rule (§2), not
tuned around.

**Traps, collected.**

- #37 — a pass that died reports zero. The harness checks errors and prints `DIED`.
- #38 — dry-run placeholders, plus finding 3: printed output disagrees with the store.
- #35 — the "before" must precede the work: every rep starts from the same snapshot; check
  `link_seq` and `created_at` on the rows, not totals.
- #36 — the 12–15 baseline predates the re-ask branch, which changes it. Re-measure baseline in the
  same session as treatment.
- #32 — attribute before claiming: a cost delta is assigned to context rendering or context indices
  only after zeroing each.
- F16's 34% spread — one rep per arm is uninterpretable.
- F33 — resumed sessions duplicate root segments. Report M1 counting distinct transcripts as well as
  events, so a duplicate snapshot cannot inflate "evidence".
- The anchor-definition trap (finding 5) — an M1 near 100% means anchors were counted.

---

## First slice

The smallest change that closes the measured gap safely. It records the relationship correctly,
fixes F37 on the way, and lets nothing downstream consume context yet:

1. **Schema**: `narrative_events.kind` (no default), `narrative_events.member_confidence` with its
   CHECK, and `one_member_link_per_event`, with the generalized pre-DDL refusal and an updated README
   paragraph. Config: `correlator.member_confidence_floor`, default 0 (off).
2. **Store**: member-only predicates (`linkedSinceLastAction`, both examination predicates,
   `UnlinkedEventsInRange`), `AllNarrativeEvents` renamed, `ContextEvents` added, kind-aware
   eligibility for merge, and `moveMember`/`addContext` as `Tx` methods.
3. **Cluster**: `context_indices` and per-cluster `confidence` in the prompt and parser; the summary
   rule (member work, context by reference); the dispute re-ask for multiply-placed members (rationale
   first, per-event confidence); `assignableEvents` erroring on a repeated event; `ContextEvents`
   rendered with back-references.
4. **Persist**: the two operations, member-only windows, context-only extends leaving summary and
   window alone, compaction over member events, and the commit-time invariant check.
5. **Triage**: merge and split per §6, with the merge-ordering test first.
6. **Reporting**: context links, disputes and their resolutions, and members below the confidence
   floor on `Stats`, in the pass summary when non-zero; `NarratedNarrative.ContextEvents` read back from
   the store; below-floor member links surfaced in triage as attributions to confirm.

Tests to write first, because each pins a hazard named above: a double assignment is resolved by the
dispute re-ask into one member home and context links elsewhere, and never leaves an empty narrative
(F37); the dispute re-ask's member choice is not response order (script the model to pick the *later*
claimant and assert it wins); a context link does not change either backlog count (§7); merge
onto a target holding the event as context keeps it a member (§6); split with shared events empties
and marks the source (§6); an upgraded link is in the delta (§4); a frozen member is never numbered
(§5); a context-only `NEW` is rejected (§4). Then the drills: drop `kind = 'member'` from
`linkedSinceLastAction` and confirm the reconcile-level test fails; restore last-writer-wins and
confirm the F37 test fails; replace the dispute re-ask with first-in-response-order and confirm the
"not response order" test fails.

**Slice 1 is gated by M7** (no rise in cross-stream attraction over baseline) as well as M2/M3 (no
regression in coalescing) — not only on its unit tests, because the attraction failure is invisible
below a two-pass measurement on real data.

Docs in the same PR: `docs/architecture.md` §1 (the cluster stage now produces two link kinds), this
spec's status, F37 deleted, F6 narrowed.

## What follows, and what gates it

- **Slice 2 — let the model see lineage, and share frozen events.** Render each subagent event's parent
  and dispatching root segment in the cluster prompt. The subagent's `session_id` matches its parent's
  root segments, and its `started_at` falls inside the dispatching segment's
  `[started_at, ended_at]` — a pure join. This is where F6's subagent artifacts (`session_id`,
  `parent_session_id`, `agent_id`, `parent_agent_id`, `dispatch_tool_use_id`,
  `subagent_description`, `spawn_depth`, and the root segments' `started_at`/`ended_at`) get their
  reader. Add the `context_refs` index space (§5). **Gate:** the first slice's M1 still shows PR
  narratives missing root context that the lineage would supply, or the daily-pass scenario shows
  sharing lost to freezing.
- **Slice 3 — drafting reads context as labeled background.** **Gate:** M6b passes on the first slice,
  and passes again with context rendered. This is the slice where the user sees richer comments, and
  the one with the highest stakes; it does not land on a prompt rule alone.
- **Not to build:** context links feeding `gatherCandidates` at any tier (§3 — recommended never, not
  deferred); deterministic auto-linking from lineage (§1); a fan-out cap knob until M4 shows a fan-out
  that needs one.

## Open questions

1. **Duplicate member assignment: deterministic downgrade, or a re-ask?** §4 recommends the downgrade
   because it is strictly better than today and costs nothing. The re-ask branch may make a targeted
   re-ask cheap enough to prefer — "event N is the work of clusters X and Y; which?" Decide once its
   shape is visible; I could not inspect it, because it has not been pushed.
   **Answered (2026-10-02, with the user): the re-ask**, with rationale first and a per-event
   confidence. Response order has no logical bearing on which workstream an event belongs to, and
   membership drives token attribution, so the choice must be the correct one rather than a consistent
   one. §4 now specifies it.
2. **Inline `context_indices`, or a separate relevance call?** §1 recommends inline and names the
   separate call as the fallback. M2, M3 and M5's cluster and `NEW` counts decide.
   **Answered (2026-10-02, by the user): inline first.** Not both: the separate call is built only if
   the pre-committed trigger in §1 fires.
3. **Does the summary leak (§2) defeat slice 3 outright?** If M6b finds duplication carried by
   summaries, drafting may need the summary rewritten without context, or context withheld from
   `Cluster`'s summary-writing entirely. Unknown until measured.
   **Reframed (2026-10-02, with the user):** a summary that *mentions* context is accurate and
   desirable — workstreams touch, and Jira links tickets the same way. The harms are narrower: context
   justifying a state-bearing action, and a summary that re-tells another stream's work attracting that
   stream's future events (§2). The second is measured by M7, which now gates slice 1; M6b stays the
   gate for slice 3.
4. **Should matching's classifier see context events?** Possibly useful for telling implements from
   mentions; unmeasured (§3).
5. **Is sharing frozen events needed in steady state?** That depends on how often a root segment's
   member home commits before the PRs it fed arrive. Unmeasured, and slice 2's gate.
6. **Does context accumulation between compactions need its own bound?** §7 relies on the compaction
   boundary. A narrative with little member history and much context may never trip it.
7. **Did the 12–15 baseline exclude anchors?** Finding 5 says it must have. Confirm with whoever ran
   it, or re-derive it under §9's definition and say so.
   **Answered (2026-10-02):** yes. The scoring script classified anchor lines (`opened pull request …`)
   before the branch that counted `[claude_code]` segment lines, so anchors were excluded. Left here
   rather than deleted, so the record shows it was open.
8. **Should triage show a reviewer the context links** behind an action? They explain a draft without
   being in it — and in slice 1 they are not in it. Probably yes once slice 3 lands; harmless before.
9. **When does unjira need a migration mechanism?** §1 refuses old stores on F30's precedent and
   shares F21's trigger: the first non-disposable store.
   **Answered (2026-10-02, by the user):** not until unjira is productionized — after every slice and
   phase ships. Until then the store is disposable: refuse an old one before any DDL, rename it to a
   backup, and re-collect. No ALTER-based upgrade, and no migration framework.

## Non-goals

- No new action type, write path or gate. Context links never reach `gate.Applier`.
- No change to collectors. Every artifact this design (or slice 2) reads is already written.
- No discovery links (§8), no estimation, no `emergent` tag.
- No change to `refs` or `fanout` (F1). Env-mirror fan-out is many PRs being one piece of work; this is
  one event being background to many pieces of work. Opposite shapes.
