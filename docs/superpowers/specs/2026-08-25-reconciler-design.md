# `internal/reconciler` — design

Turns each touched narrative into a **proposed action** — a comment, a transition, or a new issue —
written to `actions` with `status=proposed`. This is the stage that makes unjira propose anything at
all; every slice before it only observed and correlated.

Phase-1 slice 4. Unblocked by the narrative→issue matching slice, which made
`narratives.issue_key` a column something writes.

## Status: landed 2026-08-25

`internal/reconciler` (`Reconcile`, `draft`, `Persist`), `pipeline.RunReconcile` +
`RenderReconcileResult`, and `dev narrate` wiring. `earthly +reviewable` green (lint 0 issues, full
offline suite passing).

Five deviations from this spec as written, each forced by something the spec assumed:

1. **`narrative_events.linked_at` and `actions.created_at` use sub-second (`%f`) timestamps.** The
   delta was not implementable at the whole-second granularity every other table uses: an event
   linked in the same second as the bounding action is invisible *forever* under `>`, because that
   action's `created_at` never advances. Both columns must share the identical format — they are
   `TEXT` compared lexically, and mixing `%f` with `%S` inverts the comparison (`.` 0x2E sorts before
   `Z` 0x5A), so a genuinely-later event reads as earlier.
2. **`tasktracker.AvailableStatusCategories` was added.** This spec said legality is checked against
   the live issue, citing `SetStatus` as precedent — but `GetTransitions` existed only on
   `*jira.Client`, not on the interface the reconciler talks to, and the local backend had no
   equivalent. Real added scope, not a detail.
3. **The interface was split into `TaskReader` + `TaskWriter`** (`TaskTracker` is now their
   composite), and the reconciler takes a `TaskReader`. This spec stated propose-never-apply as a
   rule; the split makes it a compile error. Verified by drill: calling `AddComment` from
   `verifyCandidates` fails with *"type tasktracker.TaskReader has no field or method AddComment"*.
4. **The backlog accessor is `store.NarrativesWithActionableLinks(limit, roles)`, not
   `NarrativesWithIssueKey`.** Selecting on the denormalized `narratives.issue_key` would have made
   the reconciler permanently blind to exactly the narratives it most needs: `MatchConfig.
   ConfidenceFloor` only *promotes* `issue_key` above the floor, while `narrative_issues` rows are
   written regardless — so a real but low-confidence primary has links and a NULL `issue_key`. This
   is the same class of gap as the original discovery that `issue_key` was never written at all.
5. **`correlator.Stats.addUsage` became exported `AddUsage`.** Its doc comment justified being
   unexported with "only this package makes completions" — no longer true once the reconciler makes
   them.

Not done, and not attempted: **manual end-to-end verification.** It needs a real
`unjira.config.json` and an LLM key, neither present in the environment this landed in. So the
duplicate-suppression guarantee is proven by unit test and by drill 3, *not* against real data —
which is the more valuable check and remains outstanding. Likewise the live-tier transition-gating
test compiles and is gated to skip, but has never run.

## What the spec already fixed, and what it left open

`docs/superpowers/specs/2026-08-11-phase1-correlator-design.md` specifies the shape:

> `Reconcile(narratives, lastActions, tracker)` — for each narrative, computes the **delta**:
> `narrative_events` rows added since the last action whose `decided_at`/`executed_at` covers this
> narrative (first pass, no prior action → the whole narrative is the delta). This delta is what gets
> reported/proposed — never the full cumulative narrative, so a reviewer sees "what's new since you
> last saw this," not a repeated history.

That intent stands. Four things it does not settle, settled here.

## The delta was not computable (schema gap)

The spec's delta is "`narrative_events` rows added since the last action." But the table is:

```sql
CREATE TABLE IF NOT EXISTS narrative_events (
    narrative_id INTEGER NOT NULL REFERENCES narratives (id),
    event_id     INTEGER NOT NULL REFERENCES events (id),
    PRIMARY KEY (narrative_id, event_id)
);
```

**No link timestamp.** And there is no "narratives touched since X" query either — only
`NarrativesWithoutIssueKey` and `NarrativesOverlapping`, neither of which means "changed." So "which
events were added since the last action" exists nowhere in the store; it lives only in `Persist`'s
in-memory return, within a single process. A reconciler run cold, after a separate narrate pass,
could not reconstruct it.

`events.occurred_at` is not a substitute: an event can be linked long after it occurred (a backfill),
so occurrence time is not link time.

**Fix: add `narrative_events.linked_at TEXT NOT NULL DEFAULT (strftime(...))`.** Greenfield, so no
migration machinery. The delta then becomes literal SQL, and the spec's definition becomes
implementable rather than aspirational.

This is the third slice in a row where the design pass caught a missing store seam. Slice 3 shipped
unable to assemble `Cluster`'s inputs; the matching slice caught five absent accessors by naming them
up front. Naming them here is the same discipline.

## "Last action covering this narrative" means most recent `created_at`

Three columns could define it, and the choice changes behaviour materially:

| Column | Delta means | Failure mode |
| --- | --- | --- |
| **`created_at`** | since we last **proposed** | none material — always set |
| `decided_at` | since a human last ruled | NULL while unreviewed, so an unreviewed proposal suppresses nothing and every pass re-proposes the same delta |
| `executed_at` | since we last wrote to Jira | rejected actions never execute, so a rejected proposal is re-proposed identically forever — the reviewer's "no" carries no weight |

`created_at` it is. A proposal sitting unreviewed in the queue still suppresses re-proposing its
delta, which is what keeps the queue free of duplicates while a human takes a week to look.

## `actions` is a blank slate

The table exists. **Nothing in Go touches it** — no type mirroring a row, and none of insert, query
by narrative, query most-recent, update status, or query by status. Verified: zero
`INSERT INTO actions` / `FROM actions` / `UPDATE actions` anywhere.

`actions.feedback` is likewise **absent from the schema** despite being specified under the phase-1
spec's "schema additions" — the same gap class as `narratives.issue_key`. It is added here even
though nothing writes it until slice 6, so that slice does not need a second schema change.

**No new column is needed for the multi-link decision below.** `actions(narrative_id, issue_key)` is
exactly `narrative_issues`'s primary key, so "one action per link" is already expressible. The
original schema anticipated it.

## One action per link, drafted separately

The matching slice introduced roles `primary` / `same_work` / `mentioned` and deliberately deferred
the action policy to this slice.

The motivating workflow: feature work in a `PAAS` ticket, production deploy gated by a System Change
task in `SUMO`. Not a dependency — **one body of work recorded twice for two audiences.**

So the `primary` link and each `same_work` link each get their **own** action row, drafted with
awareness of the others: the engineering ticket gets engineering framing, the change-management
ticket gets content suited to that audience. Identical text on both would defeat the point of the
audiences being different, and a single action naming both tickets would fan out at execution anyway,
since comment and transition are per-issue operations.

`mentioned` links get nothing. That is what the role means.

`store.NarrativesForIssue` guards the shared-ticket case — if another narrative already has a
`proposed` action on this key, note it rather than stacking a second. That accessor was built for
this slice (its doc comment says so) and has had no caller until now.

## Drafting: one LLM call per narrative, confidence floored by facts

Per narrative with at least one link:

1. **Fetch links** via `NarrativeIssues`. No links → skip; unlinked narratives are matching's concern.
2. **Compute the delta.** Empty → skip. This is what stops a re-run proposing the same thing twice.
3. **Verify every target link** via `tracker.GetIssue`, before drafting anything.
   `rules/intent-not-outcome.md`: *"A transcript shows what someone was doing or drafting, not the
   final state... Drafting ≠ done."* A not-found link is recorded and not acted on; a transport error
   fails this narrative for retry, reusing `correlator.IsTransportError`.
4. **Drop self-authored events from the delta.** The Jira collector sets
   `artifacts["authored_by_unjira"]`, and its doc comment states the reason: *"without it the
   reconciler proposes the same comment every pass."* That artifact has been written since the
   collector landed and **read by nothing** — this slice is its consumer. Without this step the loop
   is unstable: unjira comments, collects its own comment, and proposes commenting about it.
5. **Draft** — one call, seeing the delta plus every link with its role and live state, returning one
   draft per actionable link.
6. **Floor the confidence.** The model self-reports, and deterministic facts then **cap** it: an
   unverified link, or a transition the live issue does not offer, forces it down regardless of what
   the model claimed. `rules/verify-correlations.md`: *"Confidence scores are not enough; a
   hallucinated match can be high-confidence."* The model's number is evidence, not a verdict.
7. **Check for duplicates** via `NarrativesForIssue`.

### Two constraints on what may be proposed

**Transitions are validated against the live issue's available transitions, not a mined
`workflow.Graph`.** The graph is statistically observed changelog history — `HasEdge` answers "has
this ever been seen," which is a proxy for legality, not ground truth. `internal/workflow`'s own
package doc names the three tiers and puts per-issue `GET /transitions` at execution time as the
authority, and `tasktracker.SetStatus` already works that way. Using the graph would let the
reconciler propose a transition Jira will refuse.

**`create` is proposed only when a narrative has no verified link at all** — never alongside an
existing one, or unjira would manufacture duplicate tickets for work already tracked.

## Errors

**Per narrative**, accumulated with `errors.Join`. One narrative's tracker outage leaves it
unreconciled and moves on; "no proposal" is a valid resting state, so a failed pass costs a retry and
nothing else. Same posture as the collector's per-query isolation and matching's per-narrative
isolation.

**`Persist` is all-or-nothing per pass**, in one `store.WithTx`. The phase-1 spec requires this for
the slice-5 gate (its auto-commit paragraph, `2026-08-11-phase1-correlator-design.md:189-193`):
*"All-or-nothing per `watch` invocation: if reconciliation fails partway through a pass, nothing from
that pass auto-commits until a clean pass succeeds."* Half-written proposals would let that gate auto-apply
against an inconsistent set.

`--dry-run` skips `Persist`, following `dev narrate`'s existing treatment of matching.

## Config

`ReconcilerConfig`, validated like `CorrelatorConfig`/`MatchConfig`:

- `MaxNarrativesPerPass` — bounds the LLM and `GetIssue` fan-out. Hitting it is **logged, never
  silent**; a silent cap presents as a clean pass that quietly ignored work.
- `MinConfidenceToPropose` — below it the action is **still written**, with its low score, rather than
  dropped. Slice 6's triage needs to see weak proposals in order to judge them, and a dropped
  proposal is indistinguishable from "nothing to do." Same reasoning as `MatchConfig.ConfidenceFloor`:
  govern what unjira *asserts*, not what it *records*.

## Testing

Offline: real temp-file SQLite via `openStore`, the existing `fakeLLM` shape, and a **new**
`fakeTracker`. The one in `match_test.go` stubs `AddComment`/`SetStatus`/`CreateIssue` as silent
no-ops — matching never writes — so a reconciler test needs one that records calls and can fail them.

Table-driven: no links, empty delta, unverifiable link, transport failure, self-authored-only delta,
`same_work` producing two distinct drafts, duplicate suppression, confidence flooring.

Four break-it drills, on the behaviours where silent failure is plausible:

1. Stop filtering `authored_by_unjira` → the self-authored test must fail. This is the loop-stability
   guard, and its absence would not error, only compound.
2. Let model confidence through unfloored → the unverified-link test must fail.
3. Ignore the empty delta → the re-run test must propose twice.
4. Skip verification before drafting → the unverified-link test must fail.

Live tier: seed an issue, propose against it, confirm `GetTransitions` gates a transition the
workflow does not offer. Compiles and skips without credentials.

### Break-it drill results (run 2026-08-25)

All four drills produced a failure, so every guard is covered. Each change was reverted and the suite
confirmed green afterwards.

| Drill | Guard removed | Test that failed | Failure observed |
| --- | --- | --- | --- |
| 1 | `dropSelfAuthored` returns `evts` unchanged | `TestReconcileDropsSelfAuthoredEventsBeforeComputingTheDelta` | FAIL — a Jira comment unjira itself posted re-entered the delta |
| 2 | `floorConfidence`'s legality gate | `TestDraftFloorsConfidenceForAnIllegalTransition` | `Should be zero, but was 0.95` |
| 3 | `reconcileOne`'s `len(delta) == 0` early return | `TestReconcileSkipsWhenTheDeltaIsEmpty` | FAIL — `SkippedNoDelta` false and the LLM was called |
| 4 | `verifyLinks`, building `verified` straight from `actionable` | `TestReconcileDropsAnUnverifiableLinkWithoutFailingTheNarrative` | FAIL — nonexistent `PROJ-404` reached drafting |

Two notes for whoever repeats these. Drill 2 must remove *only* the
`slices.Contains(v.AvailableStatus, …)` check: replacing the whole function body with
`return action.Confidence` drops the `[0,1]` clamp too and leaves `slices` unused, so the package
fails to *compile* — a build error is not evidence about the guard. Drill 4 has the same hazard with
the `tracker` parameter. A drill that breaks the build proves nothing; it has to break the behaviour.

## What this slice does NOT do

- **Apply anything.** It writes `status=proposed` and stops. No Jira writes.
- **The auto-commit gate**, `config.AutoCommit{ConfidenceFloor, Graduated}` — slice 5.
- **`triage`, the rework loop, `rules.Distill`** — slices 6–7. `actions.feedback` is added but
  unwritten.
- **`estimate` actions.** `tasktracker` deliberately has no method for them; phase 2+.
- **`rules/review-staleness.md` and `rules/bot-pr-noise.md`.** Both are GitHub-PR-shaped (head SHA,
  `[bot]` authors) and there is no GitHub collector, so their data is not in the event stream. Stated
  rather than pretending the reconciler honours them.
- **Wiring `scope: reconciler` rules into the drafting prompt.** The rules loader supports the scope;
  wiring it is a follow-up once both land.

### Known consequence

Nothing can *view* the proposal queue until slice 6's `triage`. End-to-end verification for this
slice is `dev narrate` output plus reading the `actions` table directly. Recorded so it is not later
mistaken for a bug.
