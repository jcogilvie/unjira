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

### F49 — transcript handling has only been tested against macOS-written transcripts

Every transcript the collector has been measured or tested against was written on macOS: 0 of 46,343
commands contain a carriage return, and every `cwd` is a POSIX path. Nothing has exercised transcripts
written on Windows or Linux. The parts that touch a platform's text conventions are the shell parser
(CRLF), `jsonlLines` (line framing), `projectName` and `exclude_cwds` matching (path separators, drive
letters), and the summary flattening in `truncate`. **Action item:** add fixture transcripts written on
those platforms and run the collector tests against them. No defect is known.

### F48 — a script handed to a shell is read as data, so the commands it runs are invisible

`simpleCommands` (`internal/collector/claudecode/shell.go`) parses a tool call's command with
mvdan.cc/sh and counts only the simple commands it runs directly. A script passed to another shell is a
string or a heredoc body to the parser, so its commands never reach `isAuthoring`, the fact rules or
the `gh pr create` anchor recognizer. That covers `sh -c '…'`, `bash <<'EOF'`, and `docker run … sh -c
'…'`. A Python heredoc that shells out (`subprocess.run(["git", "commit", …])`) is invisible the same
way. **Consequence:** a commit or PR made that way loses its SCM keys and facts, and a PR created that
way gets no anchor.

Measured over every local transcript, by ISO week. Heredocs stepped from 1–9% of distinct commands
(W18–W33) to 11–16% (W34–W40), and Python heredocs from about 10 a week to 130–580. Through both
periods, none of the scripts handed to a shell (95) and none of the Python heredocs (2,082) that shell
out named a commit or PR verb. Since W34, with 0 of ~1,890 Python heredocs and 0 of ~47 shell scripts,
the rule-of-three upper bound is under one hidden commit or PR a week. Visible authoring runs at 70–190
commands a week. The one real case seen is a test run, `helm unittest` inside `docker run … sh -c`,
which costs a "ran tests" fact.

The measurement extrapolates a volume, not a behaviour: if commits start being made from inside
scripts, it moves within a week. `TestHiddenAuthoring_Tripwire`
(`internal/collector/claudecode/hidden_authoring_probe_test.go`) is the guard: it re-measures by ISO
week over local transcripts and fails when one appears. It judges shell scripts with the collector's
own recognizers. Python uses a heuristic, a statement-starting process call with a commit or PR verb on
its line. That heuristic's first two runs both flagged scripts that only quoted such a call inside a
string, which is why the call must start the statement. Recovery for the shell case is cheap on the parser: when a shell
program's script is a literal `-c` argument or a literal heredoc, parse it recursively. That is about
20 lines and stays deterministic. The Python case is not recoverable deterministically, since recognizing
a subprocess call means reading another language.

### F47 — a PR a subagent opened is named only in the subagent's own segment

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

### F45 — the model can still reshuffle a PR's placed members apart

The PR identity join (`internal/pipeline/preassign.go`, `planPRIdentity` at `:76`) places only
UNPLACED events. A narrative's eligible members, the PR's `:opened` among them, are still numbered in
the clustering prompt whenever that narrative is context, and `Persist` moves any the model puts in
another cluster (`moveMembers`, `internal/correlator/correlator.go:1620`). The join makes this more
likely to be offered, though not more likely to be taken: placing a merge extends its narrative's
`window_end` into the current window, so the narrative becomes context in the very pass that placed
it (`hidePreAssigned`, `preassign.go:235`, hides only the placed event). A reshuffle that moves
`:opened` away splits the PR again, with the merge left where identity put it, and a later event for
that PR then finds two holders and falls back to the model (`PRSeveralHolders`).

Not introduced by the join. Every eligible member has always been open to reshuffling. Measured since
the join landed, in six two-pass acceptance reps (`SHARED_CUT=2026-09-21`, window 2026-09-16 to
2026-10-02): 24 identity placements in total, M3 PR integrity 26/26 in every rep, and 0 identity-placed
members moved by the model. So the model has not been observed doing this. It stays open because the
mechanism is still there, and six reps of one window bound how often it happens, not whether it can.
If it occurs, it shows as an M3 split whose identity half was placed by `identity`
(`narrative_events.member_placement`), and the remedy is to number no eligible member whose PR an open
narrative already holds by identity. That is a prompt-shape decision, and is not taken here.

### F53 — stored timestamps keep their source's UTC offset, and window queries compare them as strings

`store.InsertEvent` writes `event.OccurredAt.Format(time.RFC3339)` (`internal/store/events.go:29`)
without converting to UTC, so a timestamp keeps the offset its source gave it. Jira's carry the
account's zone (`2026-08-21T12:00:57.812-0700`, the layout the Jira collector parses), and are stored as
`…-07:00`. Claude Code and GitHub timestamps are `Z`. The window queries then compare these as text:
event-range reads bound `occurred_at` with `start.Format(time.RFC3339)`, and `NarrativesOverlapping`
tests narrative windows the same way. Lexical order equals chronological order only within a single
offset. **Consequence:** a Jira event can fall on the wrong side of a pass's window, or be missed by an
overlap test, by up to its offset (7 hours for that account). The F46 fix compares instants
(`strftime('%s')`) where it decides "forward", and leaves the existing comparisons as they were.
Unmeasured: every event in the snapshots measured in this period is `Z`, since those windows held no
Jira events. The likely fix is to store UTC at every writer, which needs a fresh store under the
no-migrations rule, or to compare instants in the queries.

### F51 — a dry run reports an extend's title and window from the cluster result, not as Persist writes them

`describeUnpersisted` (`internal/pipeline/narrate.go:413`) builds each dry-run narrative from the
cluster result. For an extend that adds members, it reports `r.Title` (line 430) and a window of
`eventWindow(r.Events)` (line 421), meaning the earliest and latest of this pass's new members. Persist
reports something else for the same extend (`internal/correlator/correlator.go:1565`, `:1586`). It keeps
the stored title, because the prompt asks for a title only on `new` (F27), so `r.Title` is empty. It keeps
the stored `window_start`, and moves `window_end` only forward. Checked with a throwaway test: a
narrative `Old` over `[09:00, 14:00]` extended by one event at 12:30 reports `title="Old"
window=[09:00, 14:00]` from a real pass and `title="" window=[12:30, 12:30]` from a dry run.
**Consequence:** `NarrateOptions.DryRun` promises the reported narratives are exactly what would have
been persisted, but every extend-with-members row in a dry run's output has a blank title and a window
that can be narrower than the narrative's. Only the report is affected. Clustering, placements and the
prompt are the same as the real pass's. The fix is to report an extend from the existing narrative's
row, the way the context-only branch already does. Not fixed here because it is a separate defect from
the clustering context.

### F44 — a malformed clustering response that is not a lossless slip still kills the whole pass

Two lossless classes are absorbed: a trailing comma (`llm.JSONArrayPayload` and `llm.JSONObjectPayload`
drop it via `hujson.Standardize`), and a NEW cluster holding no event (`pipeline.dropEmptyClusters`
discards it and reports it in the pass summary). Matching is covered too: `classifyCandidates`
re-asks once on an unparseable match response, quoting the parser's reason, then fails that
narrative loudly. A real 30-day matching drain hit two, in consecutive passes: prose where the array
belonged and a duplicated malformed key (`"confidence=0.2"`).

What remains is clustering. An out-of-range index, an unknown `kind`, a missing `confidence` or prose
instead of JSON still fails `parseClusterResponse`, which is right, because a best-effort reading is
how events get silently misattributed. But nothing retries, so one such response costs the whole
multi-minute pass, and under `watch` the window may move on. The same re-ask shape would fit: once,
quoting the reason, then fail loudly. Not built, because no such clustering failure has been observed
(0 in the 32 passes measured on 2026-10-02 and 2026-10-04, and none in the 30-day drain), and a
clustering re-ask costs a full-prompt call (~70–110k tokens), where a matching re-ask costs one
narrative's prompt.

### F50 — a split narrative is still read by the create path and the review queue

`StatusSplit` is honored by one reader, `NarrativesOverlapping` (`internal/store/narratives.go:291`), so
a narrative emptied of its work (`Tx.MarkSplitIfEmptied`, `internal/store/narrativestatus.go:57`, from a
clustering pass, a triage split or a triage merge) stops being clustering context and nothing else
changes. Two other readers still see it:

- **The create path.** `NarrativesWithNoIssueLink` (`internal/store/narrativeissues.go:523`) has no
  status predicate, so a split narrative with no issue link is selected on every pass and takes one of
  `reconciler.max_narratives_per_pass`' slots. `proposeCreateOne` finds no member events and suppresses it
  with a false reason, "every event is unjira's own output" (`internal/reconciler/create.go:185`). Nothing
  records the examination, so it is selected again next pass. Probed with a cap of 1, one split narrative
  ahead of one real untracked narrative by `window_start`: the split one was examined in all three
  passes and the real one in none.
- **The review queue.** A proposed action drafted for the narrative before its work moved stays at
  `proposed` (`ActionsByStatus`, `internal/store/actions.go:102`, filters on action status alone; probed),
  so triage presents text describing work now attributed elsewhere. By reading, not probed: while it is
  open, `suppressDuplicates` (`internal/reconciler/reconciler.go:429`) drops the receiving narrative's own
  proposal on the same issue, because neither `NarrativesForIssue` nor `openProposalFromAnother` reads
  narrative status.

**Consequence:** each emptied narrative costs a create slot forever, so enough of them starve real
untracked work behind them (F12's and F26's shape), and a reviewer can approve a comment on behalf of a
narrative that holds nothing. Pre-existing for triage split's sources, which have been marked `split`
since that verb landed. Not fixed here: what a split narrative's pending actions should become (superseded,
rejected, left for the reviewer) is an action-lifecycle decision, and the create-path predicate should be
decided with it. Frequency on real data is unmeasured.

### F52 — a single dispute too large for the context window fails the pass

The dispute re-ask describes each claimant of a disputed event by ALL of its other member events, in
full (`writeClaimant`, `internal/correlator/cluster_dispute.go:326`). A dispute set that does not fit
one call is split into batches that each fit (`batchDisputes`, `:266`), so the set's size no longer
matters. One dispute's size still does: if a single disputed event's claimants alone exceed the
context window, nothing smaller can be asked, and the pass refuses before spending a dispute call
(`requireDisputeFits`, `:298`). It never truncates and never sends a prompt over budget. The case
that makes this likely is a bisected pass, which bisected because the window did not fit: each
claimant can hold a whole half, so one dispute can list more member text than either half's prompt
did (`TestCluster_ADisputeTooLargeAloneFailsLoudly`). **Consequence:** a wide window whose halves
each fit can still die at the dispute step, without having clustered anything wrong. Unmeasured on
real data: claimant sizes on a real disputed pass are what M4 and M5 will show. The remaining bounds,
capping each listed member summary or listing at most N members per claimant, trade the model's view
of a claimant for the call fitting. That is the trade F16 measured for context narratives, and
deciding it needs those numbers first.

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

**Shared context adds to completion, and does not change this finding's status.**
`docs/superpowers/specs/2026-10-02-shared-context-design.md` §7 adds `context_indices` to every cluster.
That costs completion tokens, unmeasured. It adds no event to the prompt, since a context link can only
reference an event that is already numbered. Its acceptance measurement (§9) holds it to this finding's
34% noise floor.

Measurements live in `internal/pipeline/f16_probe_test.go` (env-gated, skipped by default). Note the
probe truncates `Events` and so shows a per-event cap having no effect — which is the same
wrong-field trap design-notes #32 records, and is now a true result rather than an instrument error.

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

### F6 — Six artifacts are written and never read

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

### F57 — work linked to an upstream issue is never mirrored into another tracker

The spec's destination table gives work linked to issue *I* in tracker *T* two kinds of destination:
comments and transitions on *I*, and **creates in *T*.mirror_to**. Only the first is built. The create
path (`reconciler.ProposeCreates`, `internal/reconciler/create.go`) selects narratives with no link
at all (`store.NarrativesWithNoIssueLink`), so a narrative matched to `crossplane/crossplane#6812`
never reaches it, whatever `upstream.mirror_to` says.

**Why it was not built with the rest.** A mirror create for linked work needs a selection of its own,
and two invariants that assume one tracker per narrative to give way first: matching's
`one_primary_per_narrative` index, and `gate.Applier`'s duplicate backstop, which refuses a create for
a narrative with a primary (now narrowed to a primary in the destination's own tracker). It also wants
a memory of "already mirrored", or every pass re-proposes. Each is a design decision, not a line.

**Consequence:** an operator who sets `mirror_to` on an upstream tracker gets mirrored tickets for
untracked upstream work only; upstream work that names its issue gets none. Silent in the sense that
nothing reports the gap per narrative.

### F55 — a key no tracker lists is still read, through a fallback

The tracker model's routing is a pure function of key and config, and the spec reads that as "find the
one tracker whose scopes contain it". The resolver keeps one exception, deliberately: a project-syntax
key no tracker's scopes cover is **read** through the tracker covering the first configured project
scope (`appContext.resolver`, `cmd/unjira/main.go`; `tasktracker.Resolver.WithReadFallback`). That is
the tracker every candidate used to be verified against, so behaviour is unchanged by the refactor.
A fallback resolution never carries a writer, and the write gate refuses the key as untracked.

**Why it stays for now.** Without it, work on another team's ticket (a key in an unlisted project) no
longer verifies, the narrative stays unmatched, and the create path proposes a new ticket for work that
is already tracked: a duplicate, which is worse than today's "linked, refused as untracked, retarget
in triage".

**The cost.** Such a key reads from a site chosen by config order rather than by ownership, so on a
multi-site setup it can be checked against the wrong site, the bug per-key routing otherwise removes.
Every stored key that resolves only this way is reported at startup (`pipeline.UnroutedStoredKeys`),
so the population is visible. Deciding whether to keep the fallback needs that population measured
on a real store: how many links depend on it, and how many of them a scope added to config would
cover instead.

### F7 — a connection's name still does four jobs

**Narrowed by the tracker model's slice 1** (`docs/superpowers/specs/2026-10-06-tracker-model-design.md`).
The one struct that held endpoint, identity, read scope, write scope and collection config is now two:
`config.Connection` (`internal/config/trackers.go`) holds kind and endpoint, and `config.Tracker` holds
scopes, `writable_scopes`, `default_scope`, `mirror_to` and the Jira `queries`. Write-scope separation
survives unchanged — `writable_scopes` does not default to `scopes`.

**What remains:** identity still has no field of its own. `Connection.Name` is the config key, the
`UNJIRA_JIRA_CREDENTIALS` lookup key, the Jira collector's cursor prefix
(`collector/jira/cursor.go:24`), and the connection recorded on every stored link
(`narrative_issues.connection`). Renaming a connection therefore resets its cursors, and leaves its
old links naming a connection that no longer exists. Verification no longer reads that column — a key
routes by its scope — so the stale name is provenance only, but it is still the one place a link
records where it came from. **#178**'s kubeconfig-like question (a user
by name, separate from the server) is still open on its own terms.

### F32 — a subagent that names no branch of its own has no branch at all

Claude Code writes the PARENT session's branch into every line of a subagent transcript. Of 31 subagent
metas naming a `worktreeBranch`, 27 never show it in `gitBranch`, which instead holds the parent's
(`pr-27998`, `worktree-reconciler`, `main`). The meta's `worktreeBranch` is no substitute: all 31 values
are tool-generated `worktree-agent-<id>` names, recorded at creation and routinely renamed afterwards.
So the collector clears `gitBranch` before segmenting a subagent, and never emits it.

What a subagent names itself is recovered (`subagentBranches`, `internal/collector/claudecode/
subagent_branch.go`): the branch its own calls create (`checkout -b`, `switch -c`, `worktree add -b`),
rename to, push, or open a PR from (`gh pr create --head`). Exactly one such name becomes
`events.ArtifactGitBranch`, with `git_branch_source: subagent_tool_calls`. Several become
`session_branches`, with no branch chosen. Measured over 1,041 subagent segments: 39 recover a branch,
19 name several, and the rest name none.

**What remains:** those that name none, about 94%, still have no branch. Most did no branch work
(research, review, measurement), so nothing is lost. But a subagent that committed in a harness-made
worktree and never pushed or renamed the branch did work on a branch it never named, and that branch is
invisible. Its only record is the meta's tool-generated `worktree-agent-<id>` name, which is
deliberately not used. Frequency unmeasured. The provenance effect is small either way: 1 of 31
recovered branch names carries a ticket key. The value is in clustering and the dispute re-ask, which
match a branch against a PR's head.

Nor by the back door. `docs/superpowers/specs/2026-10-02-shared-context-design.md` §3 keeps context links
out of matching entirely, so linking a subagent's narrative to its parent's root segment as context does
not hand it the parent's branch as provenance.

### F54 — the watch launchd template cannot start before the operator logs in, or at all headless

`ops/com.unjira.watch.plist` is a per-user `LaunchAgent` (installed under
`~/Library/LaunchAgents`), which launchd only runs inside a logged-in user session. `RunAtLoad` +
`KeepAlive` keeps `watch` running once that session exists and restarts it after a crash, but
neither key makes launchd start it at boot before login, or on a headless/shared Mac where nobody
ever logs in to the GUI session that owns LaunchAgents. A `LaunchDaemon`
(`/Library/LaunchDaemons`, root-owned, runs system-wide independent of any login) would cover
that, but changes the credential story: a daemon runs as a different user by default and the
`internal/envfile` repo-root `.env` convention this template relies on (see the plist's own
comments) assumes `WorkingDirectory` and the operator's checkout are the same user's. **Consequence:**
on a machine that reboots unattended, or where the operator is not reliably logged in (a headless
build box, a shared Mac), `watch` silently stops running until the next login, with nothing short
of checking `launchctl list com.unjira.watch` to notice. Unmeasured — no deployment has hit this
yet; noted while writing the template, not found as a live incident.

---

### F60 — upstream work done for an internal ticket is routed by repository, not by purpose

Destinations route untracked work by the repository it happened in, which is right for the common case.
But some upstream work exists to serve an internal ticket: a design review that unblocks an internal
story, an upstream fix for an internal blocker, or a regression analysis behind an internal rollout. If
that upstream repository's tracker mirrors nowhere, the work is ticketed nowhere, and it never reaches
the internal ticket it serves.

On a real 30-day store, under a policy that mirrored the upstream repositories the operator does ticket,
three of the five narratives sent nowhere were exactly this. Each had an internal ticket it plausibly
belonged to, and matching linked none of them.

The fix is not a routing rule, since "serves ticket X" is semantic. It is matching: an upstream
narrative that matches an internal ticket is linked work, so it is commented on that ticket rather than
proposed as a create anywhere. Two things are not measured yet: how often an internally motivated
upstream narrative names its internal ticket at all, and whether the candidate ranking reaches the
ticket when it does.

## Task cross-references

| Finding | Task |
|---|---|
| F1 — refs/fanout await the GitHub collector | resolved: keep, invariant corrected. Not a blocker. |
| F5 — dead schema (estimates, ledger) | **resolved**: both dropped. Only TWO tables, not the three the finding claimed — a miscount nobody had checked. Existing databases keep their orphans, since this package has no migration mechanism, which is harmless because nothing referenced them |
| F6 — unread artifacts | **#176**, re-verified 2026-09-16 after F15/F20/F25 each added artifacts: still zero readers. Near-miss worth naming — `segmentSummary` renders `len(seg.userTexts)`, not the `user_message_count` artifact. `scm_keys` is the counter-example: written AND wired in one change, so it never belonged here. Narrowed 2026-10-02: `events.ArtifactPullRequest` gained readers, the dispute re-ask's evidence and then the PR identity join (F43) |
| F32 — a subagent that names no branch has none | open, narrowed: a subagent's own calls now supply its branch when they name exactly one (39 of 1,041 segments); several are recorded as a set. The remainder named none |
| F36 — a bisected window numbers a spanning narrative's eligible events in both halves | **resolved** by shared-context slice 1: the dispute re-ask runs once per `Cluster` call, after `mergeSplitResults`, so an eligible event both halves placed differently is a dispute the model resolves (`TestCluster_DisputeAcrossBisectedHalvesIsResolved`), and `Persist` refuses an event two results claim as a member rather than keeping the last |
| F37 — a double-assigned event persists in one narrative, possibly leaving an empty one | **resolved** by shared-context slice 1: one member home per event (index + commit check), the dispute re-ask instead of last-writer-wins, a NEW left memberless is a loud error, and the pass summary reads members and context back from the store. Drill: restoring last-writer-wins left the new narrative with 0 members (`TestRunNarrate_DoubleAssignmentPersistsOneHomeAndNoEmptyNarrative`) |
| F43 — cross-pass PR split | **resolved**: PR-identity pre-assignment before clustering (`pipeline/preassign.go`). An unplaced event whose host-qualified `ArtifactPullRequest` matches a member of exactly one open narrative joins it, unseen by the model; ambiguity falls back to the model and is reported. Drills: removing the exactly-one, open-status and host guards each fail a named test. Two-pass M3 re-measured 2026-10-02 on real data: 24/26 -> 26/26 in all three reps |
| F45 — the model can still reshuffle a PR's placed members apart | open. Pre-existing for every eligible member; the join only places unplaced events. Measured in six two-pass reps after the join: 24 identity placements, M3 26/26 in every rep, 0 identity-placed members moved by the model |
| F51 — a dry run reports an extend's title and window from the cluster result | open. Report only: clustering and placements match the real pass. Found fixing F46 |
| F53 — stored timestamps keep their offset; window queries compare strings | open. Jira timestamps are stored with the account's offset, everything else `Z`; unmeasured |
| F47 — a subagent-opened PR is not named in its root segment | open. Needs cross-transcript lineage (shared-context slice 2). 0 of 70 here |
| F48 — a script handed to a shell is read as data | open, guarded: `TestHiddenAuthoring_Tripwire` (`HIDDEN_AUTHORING_PROBE=1`) re-measures by week and fails if one appears. 0 through W40 |
| F49 — only macOS-written transcripts have been tested | open, action item: fixture transcripts from Windows and Linux |
| F54 — the watch LaunchAgent cannot run before login or headless | open. A LaunchDaemon would, but changes the credential story; unmeasured |
| F60 — upstream work done for an internal ticket is routed by repository, not by purpose | open. Matching's job, not routing's. 3 of 5 measured cases |
| F44 — a non-lossless malformed response kills the pass | open, narrowed: the observed trailing comma is absorbed (hujson). 0 other deaths in 32 passes; re-ask-once-then-fail is the shape if one appears |
| F52 — a single dispute too large for the context window fails the pass | open. What remains of the dispute re-ask's size once a dispute set too large for one call is batched. Unmeasured |
| F50 — a split narrative is still read by the create path and the review queue | open. Pre-existing for triage split's sources; found while fixing F40, which made clustering and merge mark emptied narratives split too. Probed: a split narrative held a create slot for 3 of 3 passes at cap 1 |
| F38 — live-tier delete errors discarded | **resolved**: all seven per-test cleanups go through `deleteIssueOnCleanup`, and they and the shared fixture report a failed delete via `reportCleanupFailure` (stderr, plus a `::warning` under Actions). Never fails the test. Unit-tested without Jira |
| F7 — connection/identity model | **#178**, narrowed: connection and tracker are split (tracker model slice 1); a connection's name still carries identity, credential lookup, cursor prefix and stored-link provenance |
| F30 — match/reconcile watermarks use a strict `>` on millisecond timestamps | **resolved**: every link comparison is now sequence-vs-sequence — `narrative_events.link_seq` (AUTOINCREMENT, since restructures delete links) against a high-water mark recorded at examination, action creation and execution. The scope was **six** comparisons, not the two the finding named: both watermarks, the reconciler delta (`DeltaEvents`, `hasUnexaminedDelta`) and the freeze rule (`EligibleEventIDs`, `EligibleEvents`, against a different table's `executed_at`). Timestamps kept as display-only. Requires a fresh store; an old one is refused at `Open`. Formerly flaky test: 100/100 |
| F31 — learn's watermark can skip corrections | **resolved**, then superseded: the watermark is a `store.CorrectionsCursor` advanced by `KeepCandidates` from what the draft READ, never from a clock reading at keep, and never backwards. Its position is now `actions.corrected_seq`, a sequence stamped when a row becomes a correction (from `correction_marks`), which also closed F41 (the clock stepping back) and F42 (a lesson added by a later ruling). Drill: a clock reading at keep fails the between-draft-and-keep test 5/5 |
| F57 — linked work is never mirrored into another tracker | open. The tracker model's slice 5 built mirror_to for untracked work only |
| F55 — an unlisted project's key is still read through a fallback | open. Kept by the tracker model's slice 2 to preserve behaviour; reported at startup. Unmeasured |
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
