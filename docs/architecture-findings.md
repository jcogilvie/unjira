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

### F21 — an event's artifacts are frozen at first collection, so a collector fix never reaches old rows

Events are keyed `(source, external_id)` with `INSERT OR IGNORE`, so re-collecting never updates an
existing row's artifacts. Every collector improvement therefore applies only to sessions collected
after it landed.

The **386** figure this finding originally cited was measured against a dev snapshot at the moment
F20's PR landed and no longer exists in that form. Re-measured on the one real store currently on
disk (`data/unjira.db`, 795 events): **419/419** `claude_code` events postdate F20 (0 missing
`scm_keys`) but predate F25 by about two hours (419/419 missing its "Did: …" clause) — a current,
concrete instance of the same mechanism, not a projection, and confirmation the gap recurs with every
collector improvement rather than being specific to F20.

**Consequence.** A fix's measured benefit is not the benefit an existing store receives, and the gap
is silent — nothing reports "this event was collected under older extraction rules." It also makes
before/after comparisons on a mature store misleading in the optimistic direction, since the new
behaviour only ever appears on new rows.

The tension is real and `INSERT OR IGNORE` is not simply wrong: idempotent re-collection is what makes
`collect` safe to run repeatedly, and finding #176 records why backfilling was rejected once already
(a re-collect that mutates rows can rewrite history a narrative was already built from).

**Designed, not implemented:**
`docs/superpowers/specs/2026-09-17-artifact-rederivation-design.md` resolves the shape #176 left
open — recompute one named artifact key in place, never touching identity — and the crux question of
what happens to narratives/actions already built from the changed event (answer: nothing needs to,
for an unmatched narrative or an applied action; a linked-but-not-yet-applied narrative is the one
genuinely open case, left for a human in triage). It also surfaces a new, unresolved hazard for
`claude_code` specifically: segment boundaries are not proven stable if a transcript grows after
collection, so re-deriving that collector's artifacts is not yet safe to build. Recommends deferring
implementation on the same grounds this entry already gave.

Not urgent while the store is disposable (`DEVSBX`, pre-release). It becomes load-bearing the moment a
real store exists, or a second collector improvement makes an operator ask for the backfill by name —
which is why it is recorded now rather than rediscovered then.

---

### F16 — the prompt is unbounded in the window, and the response ceiling binds first

A 90-day `dev narrate` ran ~18 minutes and died on a 504. The window is the only lever an operator has
and the cost is unbounded in it.

**RE-MEASURED after F18 landed, and the finding's original diagnosis no longer holds.** Attribution by
zeroing each payload site, on the current store:

| zeroed | tokens | costs |
|---|---|---|
| `Narrative.Events` | 100,600 | 0.0% |
| `Narrative.EligibleEvents` | 90,144 | 10.4% |
| `Narrative.Summary` | 99,404 | 1.2% |
| **all context narratives** | 86,927 | **13.6%** |

Context narratives were **99.8%** of the cost when this finding was written. They are now **13.6%**,
because F18 stopped tracker records from becoming narratives and every one of the 15 whale events —
the 15,037-char Jira descriptions that held 52% of all characters — is a tracker record, now excluded
before the prompt is built.

**Measured consequence: a per-event summary cap is inert on this store.** The longest summary that can
reach a prompt is **299 characters**, and **zero** work-evidence events exceed 2000. The cap was
implemented, tested, and wired (`correlator.WithMaxEventSummaryChars`, reporting truncation counts and
real lengths on `Stats`) — and it truncates nothing, because the payload it was designed to bound is
no longer in the payload.

Kept rather than reverted, for two reasons. It bounds any *single* event whatever its source, which a
future GitHub or Slack collector may well need — PR bodies and thread transcripts are whale-shaped. And
the reporting half is the part that matters: a cap that fires silently would read as "nothing was left
out", so it reports counts and pre-truncation lengths so an operator can tune it against evidence.

**What actually dominates now is VOLUME, not size:** 260 work-evidence events in a 30-day window at
52,667 characters total — many small events rather than a few large ones. A per-event cap cannot help
with that by construction; bounding it means bounding the *count*, which is a different and riskier
change (which events do you drop, and what does clustering lose?).

**The response ceiling is now the binding constraint, and it REPRODUCES post-F18.** A 365-day window
on the current store:

```
Error: clustering: ... completing chat prompt: response truncated after 32000
completion tokens (finish_reason=length): raise llm.max_output_tokens, or reduce
the prompt — a truncated reply is not safe to parse
```

So this is not a stale concern that F18 incidentally fixed. F18 cut a 60-day window's candidates from
108 to 36, but **121 work-evidence candidates remain across a year** and still yield enough clusters to
exhaust the ceiling. Completion scales with **cluster count**, which nothing above touches — a capped
prompt fails identically.

`max_output_tokens` is already configurable, exposed and validated (`config.go:209`), so this is a
tuning question rather than a feature. Two things make the tuning awkward, and both are worth knowing
before someone attempts it:

- **A 365-day pass takes 25+ minutes and prints nothing until it finishes.** Raising the ceiling and
  re-running is a ~30-minute experiment per value, which is why no verified value is recorded here yet.
  The pass now reports **completion tokens per cluster** (`274/cluster across 110`) so the ceiling can
  be predicted arithmetically instead of discovered by overflowing.
- **The environment's own `CLAUDE_CODE_MAX_OUTPUT_TOKENS` is also 32000**, so the gateway may impose
  its own ceiling regardless of what unjira asks for. The config comment already documents a case where
  a litellm-fronted sonnet silently capped at 4096 while advertising 128000. Confirm the gateway
  accepts a higher value before concluding unjira's config is the constraint.

The cheaper fix may not be a bigger ceiling at all: **fewer clusters.** But the near-1:1 ratio is
**not the model failing to group, and no prompt wording collapses it.** Three prompt variants × 3
reps on `post-f18.db` over a 7-day window carrying 40+ genuinely-new (unlinked) events — the case a
grouping instruction is supposed to help:

| arm | clusters | NEW | EXTENDS | completion (mean) | range |
|---|---|---|---|---|---|
| baseline | 36, 36, 36 | 4 | 32 | 10,599 | 8,365–12,009 |
| + explicit grouping criterion | 36, 36, 36 | 4 | 32 | 9,927 | 8,284–10,851 |
| + criterion, reasoning-first key order | 38, 38, 36 | 4 | 32/34 | 12,656 | 9,825–14,931 |

**`EXTENDS` equals the context-narrative count (32) in all nine runs.** `assignableEvents` appends
every context narrative's `EligibleEvents` into the numbered index space, so the model receives ~190
events of which ~150 already belong to one of 32 narratives, and it returns them where they belong.
Completion scales with the number of *existing overlapping narratives*, not with how coarsely the
model groups new events. Collapsing two of those clusters would mean merging two already-persisted
narratives, which is a different operation from clustering and one the pass should not perform
implicitly.

**The 40 new events collapse to 4 `NEW` clusters — the same 4 in every arm, differing only in title
wording.** The model was already grouping new work ~10:1 unprompted. The premise that it was not
grouping was wrong.

**Note the noise floor: baseline completion alone spans 8,365–12,009 across identical runs, a 34%
spread.** Any single-run completion comparison on this pass is uninterpretable. An earlier
measurement here reported a "25% drop" from one run per arm; it was entirely inside this spread.
Replicate before attributing a completion delta to a change.

Attacking the ceiling means reducing the count of *hydrated context narratives*
(`pipeline.hydrateContextNarratives` → `store.NarrativesOverlapping`), and that lever now exists:
`correlator.max_context_narratives` (`config.go`), zero (unlimited) by default so the knob ships
inert. `pipeline.boundContextNarratives` applies it BEFORE the per-narrative event/eligibility
queries, so an excluded narrative's hydration cost is excluded too, not merely its prompt bytes.

**Ranking, not a bare `LIMIT`, is the load-bearing half.** `store.NarrativesOverlapping` orders
`(window_start, id)`, so a bare `LIMIT` keeps the OLDEST rows — close to the worst choice, since the
newest overlapping narrative is the likeliest to be extended by a new event. Worse than the token
cost, dropping the WRONG narrative corrupts the data the bound exists to protect: the model cannot
see the story an event belongs to, opens a spurious `NEW` cluster, and fragments a narrative that
already exists. `pipeline.selectContextNarratives` keeps two tiers: first, any narrative already
linked (`store.NarrativeIssueKeysByNarrative`) to an issue key the window's own candidate events also
name (`pipeline.candidateIssueKeys`, reading the same artifact tiers `match_candidates.go`'s
`gatherCandidates` ranks by provenance, flattened to a set); the remainder by most-recent
`window_end` first. Exclusions are reported (`NarrateResult.ExcludedContextNarratives`, rendered on
stdout only when non-zero, mirroring `ExcludedTrackerRecords`), so an operator can tune the bound
against evidence rather than guessing blind.

**MEASURED, AND THE LEVER IS HARMFUL — it is an escape hatch, not a tuning knob.** `post-f18.db`,
7-day window, 2 reps per arm:

| bound | clusters | NEW | ctx narratives | completion |
|---|---|---|---|---|
| off (0) | 37, 37 | **6** | 31 | 6,968 / 10,465 |
| 12 | 25, 26 | **13, 14** | 12 | 7,826 / 5,324 |

Cluster count fell 37 → 25, which is exactly the win the bound was built for. It is not a win.
**`NEW` clusters more than doubled**, and every extra one duplicates a narrative that already exists
but was dropped from context — checked against the store, `'triage-shows-context'`,
`'corroborated-candidate-tier'`, `'finding-issue-key-drift'` and `'finding-reconcile-remainder'` each
already had a row in `narratives`. The model could not see the story, so it opened a new one. That is
data corruption, not overspend, and it is the precise failure the ranking design was written to avoid
— arriving anyway, at a bound of 12 on real data.

**Completion did not even improve.** 5,324–7,826 bounded against 6,968–10,465 unbounded: overlapping
ranges, well inside the 34% noise floor above. So the trade is narrative integrity for nothing
measurable.

Therefore the knob is documented as settable **only where the alternative is a pass that fails
outright** — the 365-day window that dies on the response ceiling, where a fragmented narrative beats
no narrative. Not for trimming a pass that already completes. It ships inert (zero = unlimited) and
nothing enables it.

**Why the damage lands where it does, which is the useful part.** The shared-issue-key tier cannot
rescue an *unmatched* narrative, and the dropped ones mostly had no issue key yet — so they fell
through to the recency tier and off the end. **A branch- or repo-overlap tier is the untried idea**: a
collector records branch and repo on every event, so two narratives on one branch are plausibly one
story even when neither is matched. That is the next thing to try, and it is untested.

What is verified beyond the measurement: the ordering and bound as pure functions
(`internal/pipeline/context_narratives_test.go`, `internal/store/narrativeissuekeys_test.go`) — zero
bound includes everything, a bound below the population keeps the ranked set rather than the first N
by insertion, a shared issue key outranks recency, ties break deterministically on id, and the
frozen/eligible partition (`hydrateContextNarratives`'s commit-watermark split) still holds for
whatever survives the cut.

**F16 stays open, and its remaining lever is now none of the ones tried.** Every candidate that
reduces what the model sees has been measured: window splitting costs more, the per-event cap is
inert, grouping instructions change nothing, and bounding context corrupts narratives. The honest
remaining options are a higher response ceiling (blocked on the gateway's own 32000 limit) or the
untried relevance tier above.

Per-cluster output was the other candidate and is now smaller: the prompt asked for a `title` on every
cluster while `ExtendNarrative`'s `UPDATE` has no title column, so ~32 titles per pass were generated
and discarded. Fixed — `title` is now new-only. Worth ~446 completion tokens (4.2%), which is **well
inside the 34% noise floor above** and therefore not verifiable by comparing pass totals; verified
instead by cluster composition (36/36 clusters, the same 4 `NEW` groupings) and by the model emitting
empty titles on 32/32 extends.

**Candidates falsified by measurement, recorded so they are not re-proposed:**

1. **Split the window.** Already implemented (`clusterWithSplit`). Costs **1.37×** at 90 days — both
   halves re-hydrate the same context.
2. **Cap the narrative count.** Attacks what is now a 1.2% term — of PROMPT tokens. This measurement
   predates the completion-token mechanism above and does not falsify `max_context_narratives`: that
   bound targets completion tokens via EXTENDS-cluster count, a cost this line never measured, not
   the 1.2% prompt-byte term this line did. Kept rather than deleted so a future reader does not
   re-run the same prompt-byte measurement expecting it to bear on the bound that landed instead.
3. **Truncate `Events` only.** Measured **0.0%** — that field is empty whenever nothing is applied.
4. **Drain the action queue to advance the freeze watermark.** **+131 tokens**; the watermark governs
   assignability, not visibility.
5. **Tell the model to group more coarsely.** **0 clusters saved** (36 → 36 across 3 reps, table
   above). The ratio is one cluster per pre-existing narrative, which is a property of what the
   prompt is given rather than of how the prompt asks for it.
6. **Reasoning-first key order** — emitting a `rationale` field before `kind`/`narrative_id`, so the
   judgment follows the reasoning rather than preceding it. Sound principle, and the schema's current
   order genuinely does ask for the hardest decision first; `json.Unmarshal` binds by name so
   reordering is free. But measured **worse**: 38/38/36 clusters and ~20% more completion, because
   `rationale` is pure added output and justifying each cluster made the model *more* willing to open
   new ones. It also fragmented one narrative's incoming events across three same-titled clusters
   (harmless — `mergeSplitResults` unions `ClusterExtends` sharing a `NarrativeID`, and a persisted
   run shows no duplicate titles — but not an improvement).

Measurements live in `internal/pipeline/f16_probe_test.go` (env-gated, skipped by default). Note the
probe truncates `Events` and so shows a per-event cap having no effect — which is the same
wrong-field trap design-notes #32 records, and is now a true result rather than an instrument error.

---

### F30 — the match watermark's strict inequality fails inside one millisecond

`TestNarrativesWithoutPrimaryLink_ExaminedIsAWatermarkNotATombstone` is **flaky on `main`**, and the
flake is a real defect rather than a slow test. Reproduced while reviewing the GitHub collector, which
did not cause it:

```
go test ./internal/store/ -run TestNarrativesWithoutPrimaryLink_ExaminedIsAWatermarkNotATombstone -count=30
  FAIL — "new work past the watermark must re-open the narrative for matching:
          recording 'never match this' would make an early absence permanent"
```

`-count=5` passes; `-count=30` fails. Root cause is the **granularity of the comparison**, not the
format: `matchExaminationPredicate` re-opens a narrative with `ne.linked_at > me.examined_at`, both
columns written as `strftime('%Y-%m-%dT%H:%M:%fZ','now')` — millisecond precision. Two `now` calls
inside one millisecond are byte-identical:

```
sqlite3 :memory: "SELECT strftime('%Y-%m-%dT%H:%M:%fZ','now'), strftime('%Y-%m-%dT%H:%M:%fZ','now');"
  2026-09-21T20:49:09.492Z|2026-09-21T20:49:09.492Z
```

So a narrative examined and then linked within the same millisecond is **never re-admitted**: the
strict `>` is false and the watermark behaves as the tombstone its own doc comment says it must not be.
The test surfaces it because it writes both in quick succession, but the production path has the same
hazard — a fast `collect`→`match` sequence on a small store can link an event inside the millisecond the
examination was recorded, and that narrative silently stops being a matching candidate until *another*
event arrives.

Note this is a **different** bug from the one the format was chosen to avoid: F22's implementation hit
`%f` vs `%S` inversion (`.` sorting before `Z`), and the fix was to align formats. Both columns are
correctly aligned here. The residual is that equal timestamps are indistinguishable under `>`, which no
amount of format agreement fixes.

The same shape exists in `reconcileExaminationPredicate` (F26's fix, `store/reconcilewatermark.go`),
which compares `linked_at > examined_at` identically. Not yet observed failing there — a reconcile pass
does more work between the two writes — but the hazard is structural, not incidental to matching.

Open because the fix is a judgement call, and the obvious ones each have a cost:

- **`>=` instead of `>`.** One character, and it makes a same-millisecond link re-open the narrative. But
  it also re-admits a narrative whose *examination* was the last thing to touch it, so a pass could
  re-examine the same narrative indefinitely — reintroducing the livelock the watermark exists to break.
- **Higher-resolution timestamps.** SQLite's `strftime` offers no sub-millisecond precision, so this
  means generating the value in Go and giving up the "written SQL-side only" property that keeps the two
  columns' formats from drifting — the exact discipline F22 needed.
- **A monotonic sequence rather than a timestamp.** Correct, and the biggest change: a counter column or
  `rowid` comparison sidesteps clock granularity entirely, at the cost of no longer being human-readable
  in a `sqlite3` session, which is how most of this codebase's watermark bugs have been diagnosed.

A fourth option worth stating because it is tempting and wrong: making the *test* slower (a sleep
between writes) would hide the defect rather than fix it, and the production hazard would remain.

---

### F1 — refs and fanout await a wiring decision, not a missing collector

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

### F28 — a collector cannot ask whether its own system is a tracker in this deployment

unjira must work for arbitrary tracker/collector combinations — Jira, GitHub, GitLab, Trello,
YouTrack as trackers; collectors for each of those plus Slack and other streams. **Whether an event is
a tracker record is a property of (artifact kind × which systems are trackers *here*), not of the
collector that produced it.** `events/tracker_record.go` says so explicitly, as its stated reason the
marker is declared rather than inferred: *"A GitHub-Issues collector's timeline events are tracker
records too, and must be recognized without editing any consumer."*

A GitHub Issue closing is a tracker record when GitHub Issues is the tracker, and work evidence when
Jira is. Symmetrically, a Jira comment is work evidence in a deployment tracking work in GitHub. Same
artifact, classified oppositely by configuration.

**A collector has no way to make that call.** `CollectContext` carries `Config`, so it can *look*, but
there is nothing typed to look at: `config.JiraConnection` is Jira-shaped, `tracker.backend` is a bare
string, and `events.ArtifactConnection` is documented as "the configured **Jira** connection name"
(`events/artifact_keys.go:12`). So the only available basis for the decision is the collector's own
package identity — exactly the inference `tracker_record.go` forbids, and the one incident 21 already
burned this codebase for.

**Consequence.** Any collector for a system that *could* be a tracker (GitHub, GitLab, Trello — most
of the planned ones) must either hardcode an assumption about the deployment or mark nothing, and both
are wrong in some valid configuration. Marking nothing is the direction `IsTrackerRecord`'s default
chooses deliberately (too talkative, never silent), so the symptom is unjira paraphrasing a tracker
back onto itself — design-notes #24's 18-of-21 measurement.

**A second, sharper gap: self-authorship detection is Jira-only.** When a collector's system *is* the
tracker, unjira's own writes return through that collector — post a comment, re-collect it next pass.
`ArtifactAuthoredByUnjira` handles this, computed per-connection from `SelfAccountID` via one
`Myself()` call (`collector/jira/events.go:211`). Nothing requires a collector to supply an
equivalent, and absence reads as "not self-authored", so a tracker-collector without a self-identity
narrates unjira's own output back at itself. **This is the one thing that genuinely differs when
collector and tracker coincide** — and F26 (fixed, deleted) existed because those events accumulate
even with Jira's detection working.

Not urgent while Jira is the only real tracker and `local` the only alternative. It becomes a blocker
for the second `tasktracker` implementation, and a **prerequisite for any GitHub slice collecting
Issues** rather than only PRs (see
`docs/superpowers/specs/2026-09-17-github-collector-design.md` §"Which side of the diff GitHub sits
on", which sidesteps the ambiguity by collecting no issue-shaped artifacts).

Overlaps **F7** and should probably be decided with it: F7 asks whether `JiraConnection` wants a
kubeconfig-like shape, and a system-typed connection is most of what this needs. F7 currently reads as
a tidiness question about one struct; this makes it a multi-tracker blocker.

### F29 — nothing expresses which tracker a narrative's work belongs to

F28 is about *classifying* an event. This is about *routing* one, and it has a worse failure mode.

The relationship between collectors and trackers is **many-to-many**, and all three directions occur
in one real deployment:

- **One collector, several trackers.** A single `claude_code` collector observes OSS work on an
  upstream project (tracked in that project's GitHub Issues) and employer work (tracked in Jira), in
  the same transcript corpus, often on the same day.
- **One tracker, several collectors.** Jira already receives evidence from `claude_code` and the Jira
  collector; a GitHub collector makes three.
- **One narrative, several trackers.** An upstream PR raised for an internal reason legitimately
  concerns both: the OSS issue wants "PR opened upstream", the internal ticket wants "fix submitted,
  awaiting maintainer review." Same work, two audiences, and **different prose** — which is not
  routing but per-destination content, and is the hard part.

**unjira has nowhere to express any of this.** `correlator.TrackerResolver` is already per-candidate
(`func(connection string) (tasktracker.TaskReader, error)`), and its doc comment says the design
intent plainly: *"this package only knows that different candidates can need different trackers."* So
the *verification* layer is ready. What is missing is upstream of it — `Candidate.Connection` is
populated only by jira-source events, everything else arrives empty and takes a resolver's default,
and no config surface names a routing key at all (no repo, path, org, or source field exists).

**The consequence is a disclosure risk, not a cost.** Narrative summaries are generated from
transcript content, which routinely contains employer-internal ticket keys, incident detail and
architecture. Routing one to a *public* tracker publishes that prose irreversibly — categorically
unlike a wrong comment on an internal Jira issue, which is embarrassing and deletable. Today a
GitHub-Issues tracker would arrive with no equivalent of `WritableProjectKeys`, so there is no way to
say *"read this tracker, never write to it."*

`config.JiraConnection.WritableProjectKeys` is the precedent and the right shape: write authority
declared **independently** of read scope, absent meaning nothing is writable, and deliberately NOT
defaulting to read scope because *"that would mean every project unjira reads is armed for writes the
moment a connection is configured at all."* The multi-tracker version needs the same property per
tracker, and a public tracker makes it load-bearing rather than merely prudent.

**The policy is the operator's, and unjira must not encode one.** Whether OSS work is also tracked
internally varies by org — some require it for time and compliance reasons, some explicitly forbid it.
So the deliverable is **configurability, not a default**: a routing key (repo or path is the likely
shape, since it is the one thing both collectors can name and it matches the ignore-list idea for
keeping unjira's own sessions out of its corpus), per-tracker write authority, and a deny-by-default
stance when no rule matches. Enumerating the plausible operator policies — route, mirror, or exclude —
is useful for validating that the mechanism can express each, not for choosing one.

Deferred deliberately, and recorded so the deferral is a decision rather than an oversight. It is not
reachable today: one tracker backend is real, `local` is the only alternative, and no collector emits
events for a second tracker. It becomes urgent with the **first public or second real tracker**, and
the write-authority half should land *before* any tracker that could be public — a missing gate is
discovered by publishing something.

Decide with **F7** (**#178**) and **F28**: a system-typed connection carrying its own write scope is
most of the mechanism all three need.

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
| F7 — connection/identity model | **#178** — see F28, which makes this a multi-tracker blocker rather than a tidiness question |
| F8 — resolver's home | **#177** |
| F28 — tracker-record-ness is deployment-relative; a collector cannot ask | open. Decide with F7/**#178**; prerequisite for a GitHub slice that collects Issues, and for any second `tasktracker` implementation |
| F30 — match/reconcile watermarks use a strict `>` on millisecond timestamps | open. Flaky on `main` at `-count=30`, and a real production hazard: a narrative examined and linked in the same millisecond is never re-admitted, so the watermark acts as a tombstone. Same shape in both `matchwatermark.go:50` and `reconcilewatermark.go:56`. Three candidate fixes, each with a cost — do NOT "fix" it by slowing the test |
| F29 — nothing expresses which tracker a narrative's work belongs to | open, **deferred deliberately**. Many-to-many collector↔tracker routing, plus per-tracker write authority. Not reachable with one real tracker; the write-authority half must land BEFORE any tracker that could be public, since a missing gate is discovered by publishing. Decide with F7/**#178** and F28 |
| F9 — alphabetical candidate tiebreak | resolved: `ProvenanceCorroborated` ranks between `JiraEvent` and `ProseFirst`, ordered WITHIN the tier by most-recent collected Jira activity (`store.IssueActivity`). The finding's own proposed fix was measured and does **not** fix its cited example — 30 of those 73 keys corroborate, still 3x the cap, so an alphabetical sort inside the new tier re-decides identically and PAAS-4001 lands at 26/30. Its recency *window* was rejected for the same reason: correct only in a ~21-30d band (14d excludes the answer, 60d restores the alphabetical tiebreak), so the knob would have been a latent bug. Recency ordering needs no knob and holds at every cap >= 8. Measured after: PAAS-4001 moves 45/73 -> 6/73. |
| F10 — truncated pass looks complete | resolved: the remainder is data on `MatchRunResult`/`ReconcileRunResult`, counted in `internal/pipeline` and rendered on stdout. The finding framed this as a choice between threading `correlator.Match`'s signature and giving the renderers I/O; both were avoidable, because the layer that already does store I/O is the one holding the result struct. |
| F11 — issue_key denormalization drifts | resolved: the column is **deleted**, along with `.confidence`, `SetNarrativeIssueLink` and `NarrativeRow.IssueKey`/`.Confidence` — all write-only. `NarrativesWithoutIssueKey` became `NarrativesWithoutPrimaryLink`, asking `NOT EXISTS(primary link)`. The fix was already named in `design-notes.md` when the create path hit the same trap; matching was the one accessor never revisited. No migration: narrative 15 self-repaired, since it *has* a primary link. Verified by draining — the pass that crashed now completes, backlog 38 → 26. |
| F12 — reconcile remainder over-counted; suppressed narratives starved the queue | **fixed**: the count mirrors `DeltaEvents` (55 → 35), and `StatusSuppressed` records the examination so the watermark advances. Draining converges: 35 → 17 → 16. The selector now applies the delta test too, so the cap is a spend bound again: one pass moved the remainder 16 → 7 where the old design moved 1 per pass. A small **residual** remains — 7 narratives whose delta is entirely unjira's own output are selected, emptied by `dropSelfAuthored`, and write nothing |
| F13 — create outruns matching, proposes duplicates | **resolved**: creates are deferred while matching is behind (the precondition), and `applyCreate` refuses a create whose narrative has since acquired a primary link (the backstop). Found in triage. `ProposeCreates` reaches narratives matching skipped by cap and proposes tickets for work already tracked. Nothing downstream re-checks, so approving one opens a duplicate ticket |
| F14 — an issue's own body is never ingested | **resolved**: `EventFromIssueBody` emits summary+description per issue, with ADF flattening (the live shape) and an `updated`-keyed ExternalID for idempotence. Found while reviewing a create proposal. The collector derives events only from CHANGES, so 70 of 99 collected issues have no description text. Corrects an earlier "no path exists" conclusion and reorders #29 behind it |
| F15 — a session is one event dated to its last message | **resolved**: `segments()` slices on branch change with a message floor, each event dated to its own run and carrying the full branch set plus `ended_at`. Verified on the motivating transcript (3 events, middle dated 2026-07-09). 42 of 79 multi-day sessions are single-branch and remain one event — see the README's onboarding-backfill entry. Originally: found by tracing a create proposal back to its transcript. A 71-day session across 3 branches became one event dated 7 days after the merge it describes, discarding the branch that names the ticket |
| F17 — the slowest stage is silent while it runs | **resolved**: `log/slog` adopted (stdlib, no dependency), injected via each package's existing seam, all 35 call sites migrated, `--log-level`/`--log-format` with text default and JSON first-class, and `Cluster` announces its plan before calling the model. Found rebuilding the store; the fix silently reintroduced the finding once via an uncalled `SetLogger`, hence "prove it fires" in go-conventions.md |
| F16 — nothing bounds event text entering a prompt | **open**: completion tokens track EXTENDS-cluster count, which equalled the context-narrative count in all nine measured runs. `correlator.max_context_narratives` bounds that count, ranking a narrative sharing an issue key with the window above the rest by recency (not `NarrativesOverlapping`'s own oldest-first order) — available and unit-tested, but UNMEASURED end to end: no worktree has Jira credentials, so no before/after token number is claimed (design-notes #37/#38). Six candidates falsified by measurement, including window-splitting at 1.37× and a coarser-grouping instruction saving zero clusters |
| F22 — matching livelocks on narratives that name no ticket | **resolved**: `match_examinations` records "examined, nothing to match against" and `matchExaminationPredicate` (one shared const, so the selector and its count cannot drift) skips those until an event is linked past the watermark. Verified live: a store stuck at 49 for eleven passes moved to **30 in one pass**; 29 watermarks written, both reasons firing. `examined_at` must use `%f` millisecond format — the first attempt used whole seconds and the comparison silently inverted |
| F23 — the Jira client has no retry | **resolved**: a `retryTransport` retries GET/HEAD on transport errors, 429 (honoring `Retry-After`) and 5xx, capped at 4 attempts / 30s, with an explicit 20s client timeout. Writes are never retried — a single early return, since Jira has no idempotency key and a retried POST means a duplicate comment. 404 deliberately passes through, because `verifyCandidates` prunes on it |
| F24 — write scope was invisible until approval | **resolved**: `config.ProjectWritability` is one shared predicate; `gate.Applier` defers to it and triage consults it per action. `[a]pprove` is dropped from the prompt for an unappliable action and the Session refuses the verb regardless. Verified live: the header reports "17 of them cannot be applied" and each names its remedy |
| F25 — a summary named its message count while withholding the messages | **resolved**: `sessionFacts` extracts bounded deterministic phrases from the same tool calls `scmKeys` reads, and `segmentSummary` appends a "Did: …" clause. Fixed-size by construction (40 commits -> one phrase), +3.7% prompt cost. Verified on the motivating case: the rationale went from "no PR or completion evidence yet" to "commit + PR opened + tests run", escalating a comment to an In Review transition |
| F26 — a self-authored delta is selected, emptied, writes nothing | **resolved**: `reconcile_examinations` (`store.RecordReconcileExamined`) is a watermark table, F22's mechanism applied to the reconciler's Go-side `dropSelfAuthored` filter rather than a SQL predicate duplicating it. `NarrativesWithActionableLinks`/`CountNarrativesWithDelta` share one predicate (`hasUnexaminedDelta`) so the two cannot drift. Verified: a 2-narrative repro where the older, self-authored-only narrative previously starved a real one behind it under a cap of 1 now yields the real narrative on pass 2 |
| F19 — a co-clustered link is recorded at 0.98–1.0 confidence | **resolved by F18**, verified: zero `jira_event` primary links remain (branch 2, corroborated 4, prose_first 4, scm_command 5). The provenance can no longer be manufactured, because the Jira event is not in the cluster for a key to be read out of |
| F20 — the claudecode collector discards SCM commands | **resolved**: `scmKeys` extracts from authoring commands only, carried on `ArtifactSCMKeys`, consumed as `ProvenanceSCMCommand` (below branch, above jira_event). Verified live: 13 events, 34 keys. The value is RE-RANKING not new keys — one event's 18 prose candidates collapse to 2 authoritative ones — which corrected the finding's own framing |
| F21 — artifacts are frozen at first collection | **open, designed**: `docs/superpowers/specs/2026-09-17-artifact-rederivation-design.md` resolves the shape (artifact-key-scoped, recompute-and-diff, no new schema) and the invalidation question, but recommends deferring — no real store currently depends on it, and `claude_code` re-derivation has an unresolved segment-boundary hazard the spec surfaces but does not close |
| F18 — clustering narrates the tracker's own bookkeeping | **resolved**: `events.PartitionByTrackerRecord` excludes tracker records from clustering candidates. Verified live — 73 of 143 unlinked events no longer reach the model, 1 call where the width used to bisect. Found while attributing F16's cost; the exit filters (`AnyWorkEvidence` et al.) STAY, because 96 records were already linked and `narrative_events` rows are never deleted |
