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

### F23 — the Jira client has no retry, so one transient timeout aborts a whole pass

A drain pass died on this:

```
Error: matching narratives: verifying candidate PAAS-3042 for narrative 55:
getting jira issue PAAS-3042: Get "https://…/rest/api/2/issue/PAAS-3042":
read tcp …: read: operation timed out
```

One request out of hundreds. Nothing retried it, and the pass ended.

**Measured:** zero matches for `retry`/`backoff` anywhere under `internal/clients/jira/`. The only
retry in the tree is `openai.go`'s single 401-credential-refresh, which is auth-specific. No
`http.Client` timeout is configured either — the facade uses whatever go-jira's default is
(`internal/clients/jira/jira.go`).

**Consequence.** `verifyLinks` performs one `GetIssue` per candidate, so the more candidates a pass
has, the likelier it dies partway. Store-mediation bounds the damage — the pass committed what it
finished and a re-run resumes — so this is a robustness gap rather than a correctness one. But the
intended deployment is a cron running `collect` plus a human running `triage`, and a pipeline that
fails on any single flaky request will fail often enough to erode trust in the cron.

**What a fix must get right**, per the library review in
`docs/superpowers/specs/2026-09-16-http-retry-design.md`:

- **Only idempotent methods.** `AddComment`, `TransitionIssue` and `CreateIssue` are POST/PUT and Jira
  offers no idempotency key, so a blind retry risks a duplicate comment or a double transition. This
  rules out `hashicorp/go-retryablehttp`'s default `CheckRetry` (retries 5xx regardless of method), and
  it is also why `failsafe-go` offers no advantage here: its `HandleIf` predicate receives only
  `(*http.Response, error)`, so on a transport error — F23's actual case, where `resp` is nil — it
  cannot see the method at all. An outer method-gate is structurally required either way.
- **`Retry-After` on 429.** Jira Cloud rate-limits and sends the header; ignoring it turns a retry
  into an amplifier.
- **Draining and closing the response body between attempts**, or the connection is not returned to
  the pool.
- **Context cancellation**, so a retry loop honors the pass's deadline rather than outliving it.

Not fixed yet because it touches the client seam and wants its own tests for the attempt bounds.

---

### F19 — a link derived from co-clustering is recorded at 0.98–1.0 confidence

Three narratives hold a primary link at `ProvenanceJiraEvent`:

```
narrative  issue_key   role     provenance   confidence
17         PAAS-3939   primary  jira_event   0.98
18         PAAS-4017   primary  jira_event   1.0
25         PAAS-3805   primary  jira_event   1.0
```

`ProvenanceJiraEvent` means "a key found in a Jira-sourced event" — and that event is in the cluster
only because clustering put it there. So for narratives 17 and 25 the link rests on *nothing but
co-occurrence in a window*: their session branches are `paas-xelasticache-autoscaling` and
`vpc-output-application`, and neither contains its ticket key, in prose or anywhere else.

The links are probably **correct** — `paas-xelasticache-autoscaling` really is PAAS-3939's *"Add
Application Auto Scaling support to XElastiCache"*. But the relationship is **semantic**, and the
pipeline is recording it as deterministic. `ProvenanceJiraEvent`'s doc comment justifies its rank
because "the event IS about that issue" — true of the *event*, and silently inherited by the
*narrative* the event was grouped into.

**Consequence.** A 1.0-confidence link nothing downstream can falsify. `confidence_floor` is one of
the three write gates, so an inflated confidence is a safety property, not a cosmetic one: it is the
number that decides whether a proposal needs review. It also hides the semantic-matching gap that
motivates the vector index — the case looks solved.

Not a rank bug: `ProvenanceJiraEvent` is correctly ranked for what it describes. The defect is that a
narrative inherits an event-level provenance without recording that the inheritance happened.
Addressed by `docs/superpowers/specs/2026-09-16-work-evidence-vs-tracker-state-design.md`, which
removes the co-clustering that produces it.

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

### F5 — Three tables are created and never used

`ledger` (`store.go:155`) and `estimates` (`store.go:145`) are in the schema. **No Go code reads or
writes either** — the only references are the schema DDL and the package doc comment listing them.
Both are phase-2 placeholders; `tasktracker.go:121` confirms the estimate path is deliberately
unbuilt.

Not a defect in itself. Worth naming because schema is the most-read description of what a system
stores, and a newcomer counting tables will over-count what unjira does by three.

### F6 — Six artifacts are written and never read

`cwd`, `session_id`, `started_at`, `ended_at`, `session_branches`, `user_message_count` (claudecode),
`field`, `project_key` (jira) have zero production readers (verified).

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

### F12 — resolved, except a residual: the reconcile remainder over-counted, and suppressed narratives starved the queue

Both halves are fixed; the residual at the end of this entry is the part still open, and it is a
throughput bug rather than a correctness one. Kept as one entry because the second defect was only
visible once the first was fixed, and separating them would lose that.

`ReconcileRunResult.Remaining` was `CountNarrativesWithActionableLinks(SelectionRoles)`, rendered as
*"N narrative(s) still eligible to reconcile — re-run to continue draining"*.

That population is everything the reconciler *selects*, not everything it will *act on*.
`reconcileOne` computes `store.DeltaEvents` and returns early with `SkippedNoDelta` when it is empty
(`reconciler.go:214`) — the steady state for a narrative nothing new has happened to. Those
narratives cost no model call and cannot produce an action, but they are counted in the remainder,
so the number an operator reads is larger than the work that exists.

Measured on the live store: of **55** selected, **20** have an empty delta and would be skipped, and
**35** have a real one. The honest remainder is 35.

The gap does close on its own. `DeltaEvents` bounds on `MAX(created_at)` over actions **regardless of
status** (`narratives.go:421`), so once a narrative acquires any action row — including a declined one
— its links fall behind that watermark and every later pass skips it. Those 20 are narratives the
reconciler already worked through. So this is an over-count that shrinks, not a number that is stuck.

**Why the fix is a query and not a schema change.** The tempting move is to record the examination —
a `reconciled_at`, or a `narratives.status` value. That would duplicate a fact the schema already
holds: `actions.created_at` IS the watermark, and `DeltaEvents` already reads it. Adding a column
beside it recreates exactly the F11 defect, where a denormalization drifted from the table it
duplicated because writers forgot to maintain it.

So: count narratives whose delta is non-empty, using the same predicate `reconcileOne` skips on. One
store method, no schema change, and it mirrors an existing definition rather than inventing a second
one — which is the F10 lesson about a count and its selector drifting into describing different
populations.

Counting "narratives with no action yet" is close to right and still wrong: it reports 33 here, but a
narrative carrying an old action and newer events *does* have work to do, and that predicate would
exclude it.

#### The count was only half of it: a suppressed narrative starved the queue

Fixing the count exposed a second, worse defect that the wrong number had been hiding. With the
honest remainder in place, three consecutive passes reported **35** and moved nothing.

The reconciler selects `ORDER BY (window_start, id) LIMIT 20` — oldest first, a fixed order. The 20
oldest linked narratives here are all pure tracker-echo, so PR #39's `suppressTrackerEcho` correctly
declines to comment on them:

```
narrative 1
  suppressed: comment on PAAS-3802: no evidence of work in the delta — every event is a
  record the tracker produced about itself (8 of them)
```

**Suppression writes no action row.** No action row means no watermark, which means `DeltaEvents`
still returns those links, which means the next pass selects the identical 20. Meanwhile the 35
narratives at position 21+ — the ones carrying real work — are never reached.

| position in the fixed order | count | fate |
|---|---|---|
| 1–20 | 20 | re-examined every pass, always suppressed |
| 21+ | **35** | never reached |

That is a livelock, not a cap. The cap is doing its job; the combination of a *stable* selection
order with an outcome that *does not advance the watermark* is what starves the tail. Nothing
converges, and every pass pays full model cost to re-derive the same 20 suppressions.

So the remainder is now truthful and the instruction beside it is still false: re-running cannot
reach those 35. **Both halves need fixing, and the second is the one that blocks draining.**

**Fixed by recording the suppression.** `Persist` now writes one `StatusSuppressed` row per
narrative per pass that suppressed something, exactly as the create path writes `StatusDeclined` for a
refusal. That advances the watermark, so the narrative yields to the next one. Verified by draining:
the remainder went **35 → 17 → 16**, where three previous passes had all reported 35. 83 suppression
rows were written in the process.

`StatusSuppressed` is distinct from `StatusDeclined` for the same reason `StatusDeclined` is distinct
from `StatusRejected`: those record a MODEL's judgment and a HUMAN's ruling, and this records neither
— a deterministic filter fired. Collapsing them would feed slice 7's `rules.Distill` filter outcomes
as though a model had weighed them, and would make `actions list --status declined` stop meaning "the
model said no".

The row is a watermark, not a tombstone: new events land past it, so a narrative returns when there
is finally something to say. That is asserted directly
(`TestPersist_SuppressionDoesNotBlockALaterRealAction`), and it mirrors `openOrAppliedCreate`, which
deliberately ignores `declined` for the same reason.

The two rejected alternatives, for the record. **Ordering by least-recently-examined** is not
independent — there is nothing to order by until an examination is recorded, so it needs this fix
first, and it makes progress statistical rather than guaranteed. **Excluding tracker-echo in the
selector** (pushing `events.AnyWorkEvidence` into SQL, which is expressible since the marker is a
queryable artifact) fixes only one of the four filters; `unroutable`, `stale-transition` and
`duplicate` starve identically. It remains a worthwhile *optimization* — free beats one cheap row —
but it is not a substitute.

#### Also fixed: the selector spent its cap on narratives it would skip

The cap is documented as a SPEND bound — `config.DefaultMaxNarrativesPerPass` says *"each narrative
costs at least one GetIssue per link plus one LLM drafting call, so an unbounded pass is unbounded
spend"*. Recording suppressions broke that equivalence: a `LIMIT 20` selection returned 20 rows of
which **19 were free skips and exactly 1 cost a model call.** The cap had silently become a row
limit — the same confusion `MaxNarrativesPerPass` was split out of `MaxCandidatesPerNarrative` to
prevent.

Worse, the work was not merely behind the window. It sat at positions **20 through 53 of 55**, so at
one useful slot per pass the tail needed ~30 passes, each paying to re-select and re-skip the same
head.

`NarrativesWithActionableLinks` now applies the delta test too, so every row it returns is a row that
will cost something and the cap means what it says. Measured: one pass took the remainder **16 → 7**,
where the previous design moved one narrative per pass.

The predicate is a shared `const hasUnexaminedDelta` rather than two copies, because selector/count
divergence is the specific failure this entry already recorded twice.

`CountNarrativesWithActionableLinks` was deleted in the same change — with the selector filtered, it
matched no selector and had no production caller.

#### Residual: a narrative whose delta is entirely unjira's own output

Draining now converges to **7**, and those 7 are fully explained: every event they carry is
`authored_by_unjira` — status transitions unjira recorded itself. `dropSelfAuthored` runs AFTER the
selector (it must: "unjira wrote this" is an artifact test SQL could do, but the guard also protects
against a narrative whose delta is *partly* self-authored), so those narratives are selected, emptied,
and hit `SkippedNoDelta` writing nothing.

Same shape as the two defects above — an outcome that leaves no trace — and this is its third
instance. It is now the cheap version: no GetIssue, no model call, just a wasted slot and a remainder
that overstates by 7. Left open because the fix is a judgement call: either push an
`authored_by_unjira` test into the selector (possible — the artifact is queryable — but it duplicates
`dropSelfAuthored`'s logic in SQL, and partial self-authorship still needs the Go check), or record
the skip as a watermark row the way suppression now does. The second is more consistent with the fix
above; the first is free.

**A correction, recorded because**A correction, recorded because the mistake is more instructive than the finding.** This finding
first claimed the population could *never* drain, on the evidence that three consecutive passes left
`proposed` at 3 and `declined` at 35. That reading was wrong: all 35 declines were timestamped inside
the preceding 90 minutes, created by those very passes (10 at 17:11, 13 at 21:48–49, 6 at 22:03–04).
The "before" figure had been captured *after* earlier passes already ran, so a post-work state was
compared against a post-work state and a real change read as zero. The reconciler was working
throughout — examining narratives, drafting, and suppressing correctly.

The lesson generalizes past this finding: **a before/after measurement is only evidence if the
"before" was captured before the work.** Two identical numbers are equally consistent with "nothing
happened" and "the measurement window missed it", and the timestamps are what discriminate. Check
`created_at` on the rows themselves rather than trusting a total.


---

### F13 — resolved: `ProposeCreates` reached narratives matching had not examined yet, and proposed duplicate tickets

Both stages select oldest-first with the same cap, but over **different populations**:

| stage | selects | cap |
|---|---|---|
| `correlator.Match` | narratives with **no primary link** | `match.max_narratives_per_pass` (20) |
| `reconciler.ProposeCreates` | narratives with **no link at all** | `reconciler.max_narratives_per_pass` (20) |

Create's population is a **subset** of matching's, and that is the defect rather than a safeguard. A
subset's members sit at *lower* positions, so a narrative beyond matching's cap can be comfortably
inside create's — and the narratives create reaches first are precisely the ones matching just skipped.

> Same ordering, same cap number, different populations. The subset is reached **more** easily, not
> less, so subsetting inverts the protection it appears to give.

**Observed, and reconstructed exactly from timestamps.** Action 16 proposes opening a ticket for work
`PAAS-3898` already tracks and closed (Done, 2026-08-21). Its rationale says both things at once —
naming `PAAS-3898` while asserting "no issue currently tracks it in the target system" — because both
were true at different layers.

| time | event |
|---|---|
| 16:43 | all 11 `PAAS-3898` events **ingested** |
| 17:05 | all 11 **linked** to narrative 24, each carrying `issue_key=PAAS-3898` |
| 17:05–17:07 | matching links narratives **1–20** — exactly its cap — then stops |
| 17:12 | `ProposeCreates` runs. Its pool excludes those 19 linked narratives, so it is `[6, 21, 22, 23, 24, 25, …]` and **narrative 24 ranks 5th**. Create proposed. |
| 20:27 | a later pass finally matches narrative 24 → `primary=PAAS-3898`, confidence **1.0** |

So this is not "the issue arrived later" and not "we lacked context". Matching had a
confidence-1.0 candidate at its strongest provenance (`ProvenanceJiraEvent`, from
`events.ArtifactIssueKey`) sitting in the narrative's own linked events, seven minutes before the
create. It simply had not looked yet.

**The proposal is worse than wrong, it is confidently wrong — and the prompt is why.**
`buildCreatePrompt` (`create.go:398`) does not merely omit the link rows; it states their absence as
fact:

```go
b.WriteString("Every event in this work (no tracker issue exists for any of it):\n")
```

So the model was handed a false premise, then shown eleven events each titled `PAAS-3898 status: ...`.
It resolved that contradiction the only way the prompt allows — trusting the premise — and produced an
accurate summary with an inverted conclusion. It even named the ticket it was duplicating. That is the
hardest kind of output for a reviewer to catch, because everything except the recommendation is right.

The line is not wrong in itself: it is true of every narrative `NarrativesWithNoIssueLink` is
*supposed* to return. It becomes a lie only because the selector now returns narratives that are
merely unreached. A prompt that asserts a precondition the selector no longer guarantees is a second
instance of design note 24's lesson — a prompt cannot enforce a structural precondition, and here it
does not even survive one being violated.

**Nothing downstream catches it either.** `openOrAppliedCreate` inspects only *other actions*, never
whether links appeared. `gate.Applier.applyCreate` checks project-writability and decodes the payload,
then calls `CreateIssue` — **there is no link re-check before the write.** So approving action 16
opens a duplicate ticket, which the create path's own doc comments name as the worst outcome it exists
to prevent.

**Reproducible without any failure.** No crash, no timeout, no unverifiable candidate — just a backlog
longer than the cap, which is the normal state. Any narrative between matching's cap and create's cap
is a candidate on every pass. It reads as zero today (both pools have drained to 16, below the cap)
and that is a property of a drained store, not of the code.

**Fixed by a precondition, plus a backstop at the write.**

The primary fix defers the create path entirely while matching is behind:
`ReconcileOptions.UnmatchedNarratives` carries `MatchRunResult.Remaining` (already computed for F10
and in scope immediately before `RunReconcile`), and a nonzero value skips `ProposeCreates`. That
addresses the cause rather than the evidence: "no link" only means "untracked" once matching has
examined everything, and while it is behind the same absence means "not looked at yet".

It also makes `buildCreatePrompt` honest again. That prompt asserts *"no tracker issue exists for any
of it"*, which is true of every narrative the selector is supposed to return — so fixing the selector
repairs the prompt rather than requiring it to hedge.

**All-or-nothing rather than per-narrative**, deliberately. Deferring only the narratives matching has
not reached needs a record of which those are, and matching writes nothing when it finds no candidates
(`match.go:296` — "untracked work is the default path"). Since a later matching pass is the very thing
that produces the link, deferring costs latency while proposing costs a duplicate ticket, and a create
is the highest-blast-radius action unjira proposes. The deferral is reported, not silent: a
permanently-behind matching stage would otherwise make unjira quietly stop proposing creates forever,
which is F10's failure mode wearing a different hat.

**The backstop:** `gate.Applier.applyCreate` now refuses when the narrative already has a primary link,
naming the issue. A create is proposed from a snapshot and the applier is the only code that writes, so
"is this still true?" belongs there rather than in a queue-expiry rule — a duplicate ticket cannot be
un-opened. Only a PRIMARY link disqualifies: a `mentioned` link is a citation, not an attribution, and
refusing on any link would make unjira unable to open a ticket for work that merely references another
issue.

**Verified against the state as it was, not as it is.** At 17:12 matching had linked narratives 1-20
and stopped, leaving `Remaining = 52` — so the guard defers and action 16 is never proposed. This is
the check the rejected candidates would have failed: they consult link rows, and narrative 24's link
row did not exist until three hours later. The backstop was verified against today's queue instead,
where action 16 is still pending and narrative 24 now links to `PAAS-3898`, so approving it is refused
by name.

Two candidates were rejected. **Refusing when unresolved tracker evidence exists** (an `issue_key`
artifact with no link row) is deterministic and measured — it would have refused action 16, with zero
false refusals across all 16 genuinely-untracked narratives — but it guards an evidence class rather
than the precondition, and is blind to prose-only candidates. **Recording that matching examined a
narrative** is the general form and closes the prose case, but needs new state for a case the
precondition already covers.

---

### F14 — resolved: the jira collector recorded issue *changes*, so an issue's own body was never ingested

The collector has exactly two event constructors: `EventsFromChangelogEntry` (`jira/events.go:73`) and
`EventFromComment` (`:151`). Both derive from things that *happened to* an issue. **Nothing derives an
event from the issue itself**, so a summary and description written at creation and never edited are
invisible to unjira — while an issue whose description was later edited has that text, because the edit
is a changelog entry.

The data is fetched and thrown away. `clients/jira` requests `fields=*all` (`jira.go:122`), and
`IssueContext` — the struct the collector threads through both constructors — carries only `Key` and
`ProjectKey` (`events.go:48`). The body never reaches the code that builds events.

**Scale, measured:** of **99** issues collected, only **29** have any description text. **70 do not.**

**How it surfaced.** Reviewing a `create` proposal for narrative 32 ("Architectural vision & 3-year
roadmap document"), I concluded there was no deterministic path from that narrative to `PAAS-3905`, the
ticket the reviewer knew tracked the work — because `PAAS-3905` appears nowhere in the narrative's
events, and its only candidate keys are doc-scraped noise (`CP-01`..`CP-15`, `SC-7`, `AC-4`).

That was checking the wrong direction. The reviewer pointed out that `PAAS-3905`'s **description**
contains `PR: Sanyaku/platform-vision#1` — and the narrative's session ran in the `vision` repo on
branch `mesh-routing-learnings`. There *is* a cross-reference. It was simply never collected:
`PAAS-3905` has two events, both status transitions, so no description text exists for it.

**This corrects two earlier conclusions in this document's history, and that is the reason it is
written down.** The narrative-32 case was called "the semantic-matching gap, with no path available",
and cited as concrete justification for the vector index (**#29**). Both were overstated. A vector
index over a corpus missing the one discriminating string would not have found this either — it would
have returned nothing and been read as evidence that semantic matching does not work.

> Before concluding that a signal does not exist, check whether it was collected. "The data does not
> support this" and "we never ingested the data" produce identical query results and lead to opposite
> decisions.

**Ordering consequence:** fix collection before building semantic matching, or the first evaluation of
semantic matching runs against a corpus with the key evidence missing.

**Fixed by emitting an issue-body event.** `EventFromIssueBody` (`jira/issuebody.go`) turns an issue's
current summary and description into one event, emitted before the changelog and comments in
`collectIssue`. No extra request: `fields` already holds the body because the client asks for
`fields=*all`.

Two design questions this finding raised, both settled and both tested:

- **Idempotence.** The `ExternalID` is `<KEY>:body:<updated-unix>`. A fixed id per issue would freeze
  the first body ever collected under `INSERT OR IGNORE` and silently ignore every later revision —
  the same class of bug as **#176**. Jira advances `updated` on any field change, so an edited
  description mints a new row while an unchanged re-collect dedupes. It over-collects (an unrelated
  field change also mints one), which is the safe direction: a duplicate body event is inert because
  it is a tracker record, whereas a missed revision is invisible forever.
- **`tracker_record`: yes.** It is the tracker describing itself, so it must not read as evidence
  that work happened — otherwise the reconciler could draft a comment restating text the issue
  already contains, which is what PR #39's tracker-echo filter exists to stop. The marker
  deliberately does not hide it from `gatherCandidates`, which walks artifacts regardless. Usable for
  **attribution**, unusable for **narration**.

**ADF flattening was the load-bearing half, and it was nearly missed.** Verified against the live
instance: `fields.description` is an ADF object (`map[string]any` with keys `{content, type, version}`),
so `clients/jira`'s existing `fields["description"].(string)` yields `""` — silently, which is how this
went unnoticed while the code looked like it handled descriptions. `adfText` recurses, because the text
that matters sits arbitrarily deep: the PR reference is two levels down and list items nest three. A
flattener reading only top-level content passes a hand-written flat fixture and loses every real
description, so the test fixture is deliberately nested and a drill confirms the shallow version fails.

**What this does and does not fix, stated precisely.** The F14 check was "would it give
`gatherCandidates` a path from narrative 32 to `PAAS-3905`". Measured against the real ADF: the event
carries `issue_key=PAAS-3905` at strongest provenance and `platform-vision` is in its searchable text.
But the body event is dated to the issue's `updated` time (2026-07-13), while narrative 32's window
starts 2026-07-16 — so clustering places it with narrative 25, the `PAAS-3905` lifecycle, not with the
session that did the work.

> So this does **not** close the narrative-32 case deterministically. What it does is make the
> discriminating string exist at all, which is the precondition for the semantic path (**#29**) rather
> than a substitute for it.

That distinction is the finding's real content: collection was the blocker, and fixing it changes #29
from "build an index and hope" to "build an index over a corpus that contains the answer".

---

### F15 — resolved: a session was collapsed to one event dated to its last message, so a long session's work was undatable and unsplittable

`collectSession` (`claudecode/claudecode.go:225-228`) sets `occurredAt` to `meta.lastTS` — the timestamp of
the session's final message — and emits **one event per transcript snapshot**. Everything between the
first and last message becomes a single point in time, summarised by its *opening* line.

For a short session that is right. For a long one it destroys the only evidence that dates the work.

**The measured case.** Session `e951ef78` in the `vision` repo ran **2026-05-06 → 2026-07-16**: 71 days,
139 user messages, one event dated `2026-07-16T18:36`. Inside it:

| branch | span |
|---|---|
| `main` | 2026-05-06 → 2026-06-09 |
| `vp-feedback-refactor` | 2026-06-09 → **2026-07-09** |
| `mesh-routing-learnings` | 2026-07-09 → 2026-07-16 |

The transcript records `gitBranch` **per line** (1906 / 369 / 165 lines respectively). The collector
keeps only the last one, so `git_branch` is `mesh-routing-learnings` and the other two are discarded.

`PAAS-3905` — a retro-credit ticket — names `PR: Sanyaku/platform-vision#1 (vp-feedback-refactor) —
MERGED 2026-07-09T21:38:18Z`. And the session's own message at `2026-07-09T21:38` reads **"merged to
main. now i need us to incorporate learnings from these pages on a new branch"**. Same second. The
session IS the work that ticket tracks, on a branch the collector threw away, at a timestamp seven days
before the event it produced.

**Three consequences, in increasing severity.**

1. **Every date comparison is against "when did you last type", not "when was the work".** The
   reconciler's delta, `NarrativesOverlapping`, and any `--since` window all compare against
   `occurred_at`. A 71-day session is invisible to a 30-day window until its final message, then
   appears entirely.
2. **The strongest attribution signal is discarded.** `ProvenanceBranch` ranks second only to a Jira
   event precisely because a branch name is an explicit human act of naming the ticket for this work
   (`gatherCandidates`' doc comment). This session named three branches and unjira kept one.
3. **Clustering cannot split what arrives as one event.** `Cluster` groups *events*; three distinct
   bodies of work (vision authoring, VP-feedback refactor, mesh-routing learnings) are one indivisible
   unit, so no clustering improvement can separate them. Triage's `[s]plit` cannot help either — it
   redistributes events between narratives, and there is only one.

**Not a lookback problem, which is what it first looked like.** Verified: the drains ran at
`--since 720h`, so both this event and `PAAS-3905`'s transitions were always in the same window, and
`NarrativesOverlapping` offered the neighbouring narrative as context. The window was never the
constraint. The evidence was already collapsed before clustering saw it.

**The mechanism is already half-built, which is what makes this tractable.** `external_id` is
`<sessionID>:<fileSize>`, so a growing session already emits multiple events — today's own session has
**19**. So "one event per session" is not an invariant anyone relies on; the events are simply sliced by
*collection time* rather than by anything in the content. Each is a full-session snapshot re-summarised
from message one, which is also why the 19 events all carry the same opening line.

**Fixed by slicing on branch change AND carrying the branch set, which are two mechanisms because
measurement showed either alone is wrong.**

`segments()` (`claudecode/segments.go`) walks a transcript once and returns contiguous branch runs;
`sessionEvents` emits one event per run, dated to **that run's** last message, carrying **that run's**
branch and its own opening line. The motivating session now produces three events:

| branch | occurred_at | messages |
|---|---|---|
| `main` | 2026-06-09 | 127 |
| `vp-feedback-refactor` | **2026-07-09** | 8 |
| `mesh-routing-learnings` | 2026-07-16 | 4 |

That middle date is the check this finding demanded, and it matches `PAAS-3905`'s merge to the second.

**Raw slicing over-splits, so runs below a floor fold into their neighbour.** Measured: the churniest
session produced **51 runs from 14 branches**, and at the default floor of 3 messages it produces **6**.
Our own session showed a 3-minute, 16-line `rebase-probe` detour sitting between two halves of one
435-line body of work; emitting that as a peer would shred a session rather than disentangle it. A
below-floor run **folds** rather than being dropped — its messages and ticket keys carry over, because
silent data loss is the one thing this codebase errors over.

The floor's default is 3 rather than tuned per operator. F9 is the standing argument against a knob
whose correct value has to be discovered: at 3 the motivating case is unaffected, at 5 it wrongly merges
the last two runs, so 3 has headroom below the point where the floor starts destroying real boundaries.
Overridable via `min_segment_messages` for anyone who needs it.

**Every event carries the full branch set** (`session_branches`), not just its own. Slicing helps only
sessions that change branch, and **42 of 79** multi-day sessions never do — so the set is what gives the
correlator something to weigh in the other half of the cases. Deciding whether three branches are one
story is judgment, and judgment cannot weigh what it is not shown. Events also carry `ended_at`
alongside the existing `started_at`, so a run spanning three weeks is distinguishable from one spanning
an hour.

**Ticket keys are scoped to the run that mentioned them.** A key named only while on one branch must not
become a candidate for another segment's work — `gatherCandidates` treats a prose mention as a real
candidate, so leaking them sideways would manufacture links from work that never referenced the ticket.

**Worktrees are deliberately NOT a boundary, and the reason is measured.** `cwd` changes mid-session in
**5 of 164** transcripts, and **4 of those** are a parent repo delegating to its own worktree —
orchestration of one task. The single genuine focus change (two sibling worktrees) also changed branch,
so branch-slicing already catches it. Stronger than a policy: because same-branch runs merge
unconditionally, adding a cwd boundary produces byte-identical output when the branch is stable, so a
worktree excursion *cannot* fragment a task. That property is structural, not asserted — noted in
`segments_test.go` so a future reader does not mistake the test for the guarantee.

**Idempotence.** The `ExternalID` becomes `<sessionID>:<fileSize>:<segmentIndex>`. Size alone made a
growing session re-emit whole-session snapshots (one live session produced 19); size plus index keeps
each segment distinct within a snapshot while an unchanged re-read still dedupes at insert. The index
rather than the branch name, because a branch can legitimately appear twice when its runs are far enough
apart not to coalesce.

**`scanLines` and `sessionMeta` are deleted**, not left beside the new path — `segments` subsumes both,
and two ways to read a transcript would drift.

**Still open, and this is the honest limit:** slicing gives 37 of 79 multi-day sessions honest dates. The
other 42 never change branch, so they remain one event — now carrying a real interval rather than a bare
point, but still a single unit that clustering cannot subdivide. Recovering work bodies inside a
single-branch session needs semantic judgment over the transcript, which is what the README's
onboarding-backfill entry is for.


---

### F17 — resolved: the slowest stage in the pipeline said nothing until it finished

Every stage renders **after** it returns. In `cmd/unjira/main.go`:

```go
result, err := pipeline.RunNarrate(ctx, app.store, client, app.config, window, ...)
// ...
fmt.Print(pipeline.RenderNarrateResult(result))
```

`RunNarrate`'s cost is one LLM call, and it is the longest-running thing unjira does. So the stage that
takes minutes is precisely the one that prints nothing while it takes them.

There was also **no verbosity control of any kind** — no `--verbose`, no `--log-level`, nothing in
config. Logging was 35 bare `log.Printf` calls across 13 files, and in the correlator and pipeline every
one of them was an error or degradation path (`could not load issue activity`, `could not count the
remaining unmatched narratives`). Nothing reported what a pass was *doing*.

The render-after-return structure above is unchanged and correct: a stage summary belongs after the
stage. What was missing was anything said *during* it.

**What that cost, concretely.** Rebuilding the store produced three separate failures of
understanding in one sitting:

1. A 90-day window built a ~147k-token prompt, ran ~18 minutes, and died on a 504 having printed
   nothing and persisted nothing (the timeout itself is F16, landing separately). The only way to know it was still alive was
   `ps`.
2. Asked "has something changed?", neither reviewer nor agent could answer without querying SQLite
   directly. The pipeline's own output could not distinguish *running* from *hung*.
3. Two durations were reported from feel and both were wrong: "22 minutes" was a 3-pass loop of ~3m24s
   each, and "8 minutes" was a single 3m24s pass. Nothing printed a stage boundary to count, so there
   was nothing to be right about.

> A pass that emits its summary only on success is unobservable exactly when observation matters: while
> it is slow, and after it has failed.

**The numbers that would have answered it already exist, together, at the moment they are needed.**
Before the call, `Cluster` holds the candidate count, the context-narrative count, and its own
`estimateTokens` result. A mature store makes the third one the surprise: one pass spent **104,219
prompt tokens to cluster 16 candidate events**, because **52 existing narratives** were hydrated as
context. Without that breakdown the cost reads as a defect rather than as the price of context.

**This is not a request for a progress bar.** A single line before the call, naming those three
numbers, would have collapsed all three failures above into a first-second observation. The rest of the
gap is the absence of a level: `log.Printf` cannot be turned up when diagnosing or down when running
`watch` on an interval.

**Fixed with `log/slog`, and with the whole sweep rather than a 36th `log.Printf`.** slog is stdlib as
of Go 1.21 and this module is on 1.26, so it costs no dependency — and there was none for logging.
`internal/logging` builds the one logger from a level and a format; `--log-level` and `--log-format` are
Kong flags with `enum:` tags, so a typo fails at parse time instead of silently defaulting. Text is the
default because unjira is a CLI; JSON is a first-class mode rather than a debug affordance, because a
structured mode retrofitted later means consumers spend the interim parsing a human format with
regexes. All 35 call sites migrated, so no mixed state remains. The library trade-offs and the
injection rules are recorded in `docs/go-conventions.md`, which said nothing about logging before.

**The announcement itself.** `correlator.Cluster` now logs before calling the model:

```
level=INFO msg=clustering component=correlator unlinked_events=1
  assignable_events=30 context_narratives=1 est_tokens=5586
```

`est_tokens` is the number that predicts the wait and the one a caller cannot compute for itself —
`buildClusterPrompt` is unexported. `clusterWithSplit` forwards its options so each half of a split
announces too; a split pass must not go quieter than an unsplit one.

`assignable_events` is deliberately separate from `unlinked_events`. The first draft logged only
`candidates`, and against a pass summary reporting 2 unlinked it printed 30 — inviting exactly the "is
that a bug?" question this line exists to prevent. The larger number is in-window events plus the
reshufflable events context narratives already hold.

**The logger is injected, never ambient**, through whatever seam each package already had — the
variadic-option types, an options-struct field, a field on `store.Store`, or an explicit parameter
threaded to unexported helpers. No `slog.Default()`, no package global: the same reasoning that makes
`gate.Applier` the only holder of write authority.

**A component attribute replaced the message prefix.** Every old message began with its component
(`"correlator: compacted narrative..."`), which is an attribute wearing a costume — unqueryable once
shipped as JSON and repeated in every format string where it could drift.

**And the fix reintroduced the finding once, which is worth recording.** `store.SetLogger` existed for a
whole commit with nothing calling it, so both of the store's warnings were built and permanently silent.
It was found by forcing a stale pipeline lease and observing that nothing was emitted — not by reading
the code, which looked correct. Hence the convention's closing rule: **a log site is not done until it
has been seen in real output.**

**The check this finding set** — can an operator tell, within seconds, roughly how long a pass will take
and whether it is progressing — is met by the announcement, and was verified against the real binary in
both output modes rather than only in tests.

---

## Task cross-references

| Finding | Task |
|---|---|
| F1 — refs/fanout await the GitHub collector | resolved: keep, invariant corrected. Not a blocker. |
| F3 — concrete backend in the correlator | **#183** |
| F5, F6 — dead schema, unread artifacts | **#176** |
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
| F23 — the Jira client has no retry | **open**: zero `retry`/`backoff` under `internal/clients/jira/` and no client timeout, so one `read: operation timed out` on one `GetIssue` aborted a whole drain pass. `verifyLinks` calls once per candidate, so failure probability grows with candidate count. Design settled in `docs/superpowers/specs/2026-09-16-http-retry-design.md`: promote the already-present `cenkalti/backoff/v5` behind a RoundTripper, idempotent methods only |
| F24 — write scope was invisible until approval | **resolved**: `config.ProjectWritability` is one shared predicate; `gate.Applier` defers to it and triage consults it per action. `[a]pprove` is dropped from the prompt for an unappliable action and the Session refuses the verb regardless. Verified live: the header reports "17 of them cannot be applied" and each names its remedy |
| F19 — a co-clustered link is recorded at 0.98–1.0 confidence | **open**: narratives 17/25 hold primary links at `jira_event` provenance resting on nothing but co-occurrence in a window — their branches contain no key. `confidence_floor` is a write gate, so inflation is a safety property. Fixed by the work-evidence/tracker-state separation, which removes the co-clustering |
| F20 — the claudecode collector discards SCM commands | **resolved**: `scmKeys` extracts from authoring commands only, carried on `ArtifactSCMKeys`, consumed as `ProvenanceSCMCommand` (below branch, above jira_event). Verified live: 13 events, 34 keys. The value is RE-RANKING not new keys — one event's 18 prose candidates collapse to 2 authoritative ones — which corrected the finding's own framing |
| F21 — artifacts are frozen at first collection | **open**: `INSERT OR IGNORE` on `(source, external_id)` means a collector fix never reaches existing rows. 386 `claude_code` events predate `ArtifactSCMKeys` and never gain one, so F20's tier is invisible for all of them. Makes a fix's measured benefit differ silently from what a mature store receives |
| F18 — clustering narrates the tracker's own bookkeeping | **resolved**: `events.PartitionByTrackerRecord` excludes tracker records from clustering candidates. Verified live — 73 of 143 unlinked events no longer reach the model, 1 call where the width used to bisect. Found while attributing F16's cost; the exit filters (`AnyWorkEvidence` et al.) STAY, because 96 records were already linked and `narrative_events` rows are never deleted |
