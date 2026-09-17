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

### F26 — a narrative whose delta is entirely unjira's own output is selected, emptied, and writes nothing

The live residual of F12, kept when that finding was deleted because the finding is fixed and this is
not.

Draining converges to **7 narratives**, and those 7 are fully explained: every event they carry is
`authored_by_unjira` — status transitions unjira recorded itself. `dropSelfAuthored` runs AFTER the
selector, and must, because "unjira wrote this" is an artifact test SQL could do but the guard also
protects a narrative whose delta is only *partly* self-authored. So those narratives are selected,
emptied, and hit `SkippedNoDelta` writing nothing.

**Third instance of the same shape** — an outcome that leaves no trace — after F12's suppression and
F22's matching livelock. This is the cheap version: no `GetIssue`, no model call, just a wasted slot
per pass and a remainder that overstates by 7.

Open because the fix is a judgement call:

- **Push an `authored_by_unjira` test into the selector.** Free, since the artifact is queryable — but
  it duplicates `dropSelfAuthored`'s logic in SQL, and partial self-authorship still needs the Go check.
- **Record the skip as a watermark row**, the way suppression and `match_examinations` now do. More
  consistent with the two fixes that preceded it, and it makes the remainder honest rather than
  merely smaller.

The second is probably right, on the grounds that F22 established the pattern and a third ad-hoc
variant is worse than a third instance of one mechanism.

---

### F21 — an event's artifacts are frozen at first collection, so a collector fix never reaches old rows

Events are keyed `(source, external_id)` with `INSERT OR IGNORE`, so re-collecting never updates an
existing row's artifacts. Every collector improvement therefore applies only to sessions collected
after it landed.

Measured after F20 shipped: **386 `claude_code` events** predate `ArtifactSCMKeys` and will never
carry one, so the provenance tier that fix introduced is invisible for all of them. The same is true
of F15's segment slicing (a session collected as one event stays one event) and of anything a future
collector learns to extract.

**Consequence.** A fix's measured benefit is not the benefit an existing store receives, and the gap
is silent — nothing reports "this event was collected under older extraction rules." It also makes
before/after comparisons on a mature store misleading in the optimistic direction, since the new
behaviour only ever appears on new rows.

The tension is real and `INSERT OR IGNORE` is not simply wrong: idempotent re-collection is what makes
`collect` safe to run repeatedly, and finding #176 records why backfilling was rejected once already
(a re-collect that mutates rows can rewrite history a narrative was already built from). What is
missing is a *deliberate* path — a re-derive command that recomputes artifacts from `raw_ref` without
touching identity, or a recorded extraction version per event so a reader can tell which rules
produced it.

Not urgent while the store is disposable (`DEVSBX`, pre-release). It becomes load-bearing the moment a
real store exists, which is why it is recorded now rather than rediscovered then.

---

### F16 — nothing bounds how much event text enters a prompt, and the response ceiling is hit first

A 90-day `dev narrate` ran ~18 minutes and died on a 504, having printed nothing and persisted
nothing. The window is the only lever an operator has, and the cost is unbounded in it.

**Where the tokens are.** Measured by zeroing each payload site and diffing, against the real store
(68 context narratives, 30-day window, 140,119 estimated tokens):

| zeroed | tokens | that field costs |
|---|---|---|
| `Narrative.Events` | 140,119 | 0.0% |
| `Narrative.EligibleEvents` | 8,794 | **93.7%** |
| `Narrative.Summary` | 136,708 | 2.4% |
| all narratives | 308 | 99.8% |

So the cost is neither the window's own events nor the narrative summaries — it is the **325 events
hydrated underneath the context narratives**, at ~430 tokens each. They render at two sites:
`EligibleEvents` as numbered candidates (`internal/correlator/correlator.go:380`) and `Events` as
context detail (`:393`). `hydrateContextNarratives` partitions between them by the freeze watermark
(`internal/pipeline/narrate.go:249-250`), so an event's *field* changes with the watermark but its
presence in the prompt does not.

The distribution is extreme: **15 events (4.6%) hold 52% of the characters**, all 15 Jira events
whose `description` is a full incident write-up (PAAS-4002's is 12,397 chars). Their first ~150
chars carry the issue key, title, and summary heading — the part clustering needs.

**The fix that measures well.** Capping each context event's summary at both render sites:

```
since | uncapped |  cap5000  cap2000  cap1000   cap500
  30d |  140,119 |  110,757   74,072   59,221   50,010
  90d |  203,625!|  174,262  137,577  122,726  113,516
 365d |  229,263!|  199,901  163,216  148,365  139,154
                                  ! = over the 200k budget, bisects
```

`cap2000` fits a **full-year** window in one call. Two live runs at `cap2000` versus uncapped agreed
on **100 of 110 clusters (91%)**; the 10 differences were regroupings at the margin (8 `NEW`, 2
`EXTENDS`), i.e. slightly coarser clusters, not a collapse. Prompt fell 143,229 → 89,193 actual
tokens (38%).

**But the response ceiling binds first.** The `cap2000` run initially *failed* where uncapped
succeeded:

```
response truncated after 32000 completion tokens (finish_reason=length)
```

A smaller prompt still yields ~104 clusters, each emitting a title and summary, so
`llm.max_output_tokens` (32,000 — `internal/config/config.go:209`) is exhausted before the prompt
budget is. Any input-side fix alone leaves a wide window failing for the opposite reason. The
completion side scales with **cluster count**, which measured ~1 per candidate (110 and 104 clusters
from 108 candidates over two runs).

**Four candidates were falsified by measurement, and are recorded so they are not re-proposed:**

1. **Split the window.** Already implemented (`clusterWithSplit`). Costs **1.37×** at 90 days — both
   halves re-hydrate the same 68 context narratives, dividing events while duplicating context.
2. **Cap the narrative count.** Attacks 2.4% of the payload. It appeared to work only because
   dropping a narrative incidentally drops its events.
3. **Truncate `Events` / omit detail for primary-linked narratives.** Measured **0.0%** — `Events`
   is empty whenever nothing has been applied. Also unsafe: dropping a narrative from `existing`
   removes its events from `assignableEvents`' index space (`:348`), so reshuffling silently loses
   reach.
4. **Drain the action queue so the freeze watermark advances.** Simulated on a copy: 16 narratives
   frozen, 102 events moved `EligibleEvents` → `Events`, and tokens went **up 131**. The watermark
   governs assignability, not visibility.

`estimateTokens` is *not* implicated: at 500× the scale of its documented calibration it implied
2.41 chars/token against the server's own count (172,351 est / 143,229 actual), squarely on the
documented 2.41–2.51. Its 1.2× pessimism is the documented design.

Measurements live in `internal/pipeline/f16_probe_test.go` (env-gated, skipped by default).

**Excluding tracker records from clustering did NOT reduce this**, and that is worth stating plainly
because an earlier draft of this finding predicted it would. Re-measured on the same store after that
exclusion landed:

```
since | uncapped |  cap5000  cap2000  cap1000   cap500
  30d |  139,059 |  110,845   77,002   63,651   55,195
  90d |  209,440!|  180,077  143,392  128,541  119,331
 365d |  235,512!|  206,150! 169,465  154,614  145,403
                                  ! = over the 200k budget, bisects
```

Slightly *higher* than before, not lower. The two touch different things: the exclusion filters
clustering **candidates**, while the 93.7% attributed above is events hydrated as **context** under
narratives that already exist. Those narratives were built before the filter and their
`narrative_events` rows are never deleted, so their tracker records still render. Excluding a category
at the entrance does not retroactively empty the containers already holding it — the same reasoning
that keeps the reconciler's exit filters load-bearing, and a consequence of F21.

So the cap is still needed, and sizing it is now unblocked: `cap2000` is the smallest tier that fits a
365-day window in one call, and even `cap5000` stops fitting at 180 days.

---

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

### F6 — Six artifacts are written and never read

`cwd`, `session_id`, `started_at`, `ended_at`, `session_branches`, `user_message_count` (claudecode),
`field`, `project_key` (jira) have zero production readers.

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

## Task cross-references

| Finding | Task |
|---|---|
| F1 — refs/fanout await the GitHub collector | resolved: keep, invariant corrected. Not a blocker. |
| F3 — concrete backend in the correlator | **#183** |
| F5 — dead schema (estimates, ledger) | **resolved**: both dropped. Only TWO tables, not the three the finding claimed — a miscount nobody had checked. Existing databases keep their orphans, since this package has no migration mechanism, which is harmless because nothing referenced them |
| F6 — unread artifacts | **#176**, re-verified 2026-09-16 after F15/F20/F25 each added artifacts: still zero readers. Near-miss worth naming — `segmentSummary` renders `len(seg.userTexts)`, not the `user_message_count` artifact. `scm_keys` is the counter-example: written AND wired in one change, so it never belonged here |
| F7 — connection/identity model | **#178** |
| F8 — resolver's home | **#177** |
| F9 — alphabetical candidate tiebreak | resolved: `ProvenanceCorroborated` ranks between `JiraEvent` and `ProseFirst`, ordered WITHIN the tier by most-recent collected Jira activity (`store.IssueActivity`). The finding's own proposed fix was measured and does **not** fix its cited example — 30 of those 73 keys corroborate, still 3x the cap, so an alphabetical sort inside the new tier re-decides identically and PAAS-4001 lands at 26/30. Its recency *window* was rejected for the same reason: correct only in a ~21-30d band (14d excludes the answer, 60d restores the alphabetical tiebreak), so the knob would have been a latent bug. Recency ordering needs no knob and holds at every cap >= 8. Measured after: PAAS-4001 moves 45/73 -> 6/73. |
| F10 — truncated pass looks complete | resolved: the remainder is data on `MatchRunResult`/`ReconcileRunResult`, counted in `internal/pipeline` and rendered on stdout. The finding framed this as a choice between threading `correlator.Match`'s signature and giving the renderers I/O; both were avoidable, because the layer that already does store I/O is the one holding the result struct. |
| F11 — issue_key denormalization drifts | resolved: the column is **deleted**, along with `.confidence`, `SetNarrativeIssueLink` and `NarrativeRow.IssueKey`/`.Confidence` — all write-only. `NarrativesWithoutIssueKey` became `NarrativesWithoutPrimaryLink`, asking `NOT EXISTS(primary link)`. The fix was already named in `design-notes.md` when the create path hit the same trap; matching was the one accessor never revisited. No migration: narrative 15 self-repaired, since it *has* a primary link. Verified by draining — the pass that crashed now completes, backlog 38 → 26. |
| F12 — reconcile remainder over-counted; suppressed narratives starved the queue | **fixed**: the count mirrors `DeltaEvents` (55 → 35), and `StatusSuppressed` records the examination so the watermark advances. Draining converges: 35 → 17 → 16. The selector now applies the delta test too, so the cap is a spend bound again: one pass moved the remainder 16 → 7 where the old design moved 1 per pass. A small **residual** remains — 7 narratives whose delta is entirely unjira's own output are selected, emptied by `dropSelfAuthored`, and write nothing |
| F13 — create outruns matching, proposes duplicates | **resolved**: creates are deferred while matching is behind (the precondition), and `applyCreate` refuses a create whose narrative has since acquired a primary link (the backstop). Found in triage. `ProposeCreates` reaches narratives matching skipped by cap and proposes tickets for work already tracked. Nothing downstream re-checks, so approving one opens a duplicate ticket |
| F14 — an issue's own body is never ingested | **resolved**: `EventFromIssueBody` emits summary+description per issue, with ADF flattening (the live shape) and an `updated`-keyed ExternalID for idempotence. Found while reviewing a create proposal. The collector derives events only from CHANGES, so 70 of 99 collected issues have no description text. Corrects an earlier "no path exists" conclusion and reorders #29 behind it |
| F15 — a session is one event dated to its last message | **resolved**: `segments()` slices on branch change with a message floor, each event dated to its own run and carrying the full branch set plus `ended_at`. Verified on the motivating transcript (3 events, middle dated 2026-07-09). 42 of 79 multi-day sessions are single-branch and remain one event — see the README's onboarding-backfill entry. Originally: found by tracing a create proposal back to its transcript. A 71-day session across 3 branches became one event dated 7 days after the merge it describes, discarding the branch that names the ticket |
| F17 — the slowest stage is silent while it runs | **resolved**: `log/slog` adopted (stdlib, no dependency), injected via each package's existing seam, all 35 call sites migrated, `--log-level`/`--log-format` with text default and JSON first-class, and `Cluster` announces its plan before calling the model. Found rebuilding the store; the fix silently reintroduced the finding once via an uncalled `SetLogger`, hence "prove it fires" in go-conventions.md |
| F16 — nothing bounds event text entering a prompt | **open**: 93.7% of a 140k-token prompt is the 325 events hydrated under context narratives; 15 Jira descriptions (4.6% of events) hold 52% of the chars. Capping per-event summaries at both render sites fits a 365-day window in one call and agrees with uncapped on 91% of clusters — but `max_output_tokens` (32,000) is exhausted first, since completion scales with cluster count. Four candidates falsified by measurement, including window-splitting at 1.37× |
| F22 — matching livelocks on narratives that name no ticket | **resolved**: `match_examinations` records "examined, nothing to match against" and `matchExaminationPredicate` (one shared const, so the selector and its count cannot drift) skips those until an event is linked past the watermark. Verified live: a store stuck at 49 for eleven passes moved to **30 in one pass**; 29 watermarks written, both reasons firing. `examined_at` must use `%f` millisecond format — the first attempt used whole seconds and the comparison silently inverted |
| F23 — the Jira client has no retry | **resolved**: a `retryTransport` retries GET/HEAD on transport errors, 429 (honoring `Retry-After`) and 5xx, capped at 4 attempts / 30s, with an explicit 20s client timeout. Writes are never retried — a single early return, since Jira has no idempotency key and a retried POST means a duplicate comment. 404 deliberately passes through, because `verifyCandidates` prunes on it |
| F24 — write scope was invisible until approval | **resolved**: `config.ProjectWritability` is one shared predicate; `gate.Applier` defers to it and triage consults it per action. `[a]pprove` is dropped from the prompt for an unappliable action and the Session refuses the verb regardless. Verified live: the header reports "17 of them cannot be applied" and each names its remedy |
| F25 — a summary named its message count while withholding the messages | **resolved**: `sessionFacts` extracts bounded deterministic phrases from the same tool calls `scmKeys` reads, and `segmentSummary` appends a "Did: …" clause. Fixed-size by construction (40 commits -> one phrase), +3.7% prompt cost. Verified on the motivating case: the rationale went from "no PR or completion evidence yet" to "commit + PR opened + tests run", escalating a comment to an In Review transition |
| F26 — a self-authored delta is selected, emptied, writes nothing | **open**, F12's residual, kept when that finding was deleted: 7 narratives whose every event is `authored_by_unjira` are selected, emptied by `dropSelfAuthored`, and hit `SkippedNoDelta`. Third instance of outcome-leaves-no-trace after F12 and F22 |
| F19 — a co-clustered link is recorded at 0.98–1.0 confidence | **resolved by F18**, verified: zero `jira_event` primary links remain (branch 2, corroborated 4, prose_first 4, scm_command 5). The provenance can no longer be manufactured, because the Jira event is not in the cluster for a key to be read out of |
| F20 — the claudecode collector discards SCM commands | **resolved**: `scmKeys` extracts from authoring commands only, carried on `ArtifactSCMKeys`, consumed as `ProvenanceSCMCommand` (below branch, above jira_event). Verified live: 13 events, 34 keys. The value is RE-RANKING not new keys — one event's 18 prose candidates collapse to 2 authoritative ones — which corrected the finding's own framing |
| F21 — artifacts are frozen at first collection | **open**: `INSERT OR IGNORE` on `(source, external_id)` means a collector fix never reaches existing rows. 386 `claude_code` events predate `ArtifactSCMKeys` and never gain one, so F20's tier is invisible for all of them. Makes a fix's measured benefit differ silently from what a mature store receives |
| F18 — clustering narrates the tracker's own bookkeeping | **resolved**: `events.PartitionByTrackerRecord` excludes tracker records from clustering candidates. Verified live — 73 of 143 unlinked events no longer reach the model, 1 call where the width used to bisect. Found while attributing F16's cost; the exit filters (`AnyWorkEvidence` et al.) STAY, because 96 records were already linked and `narrative_events` rows are never deleted |
