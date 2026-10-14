# F16 — the prompt is unbounded in the window, and the response ceiling binds first

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

## Status

**open**: completion tokens track EXTENDS-cluster count, which equalled the context-narrative count in all nine measured runs. `correlator.max_context_narratives` bounds that count, ranking a narrative sharing an issue key with the window above the rest by recency (not `NarrativesOverlapping`'s own oldest-first order) — available and unit-tested, but UNMEASURED end to end: no worktree has Jira credentials, so no before/after token number is claimed (design-notes #37/#38). Six candidates falsified by measurement, including window-splitting at 1.37× and a coarser-grouping instruction saving zero clusters
