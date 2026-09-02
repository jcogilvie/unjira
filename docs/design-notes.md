# Design notes

Why unjira is shaped the way it is. These are the failure modes a reconciliation agent hits in
practice — each one drove a specific architectural choice. They come from operating a hand-run
predecessor that did, by hand and on a cron, what unjira automates: reconciling "what someone
actually did" (Claude Code transcripts + GitHub + Jira) against a tracked TODO list and a
review-credit ledger. Every item below is a real incident, generalized.

The behavioral conclusions are also encoded as machine-loadable constraints under `rules/` (fed
into the correlator/reconciler prompts). This document is the human-facing "why"; `rules/` is the
enforced "what". Where a lesson maps to a rule, it's cross-referenced.

## 1. Intent ≠ outcome (→ `rules/intent-not-outcome.md`)

A transcript shows what someone was **doing or drafting**, not the final state. The predecessor
once saw a *draft* of a "request changes" review comment and recorded "review not yet posted /
owed by me" — but that review had already been submitted days earlier. The item sat
mis-classified and the credit was missing from the ledger.

**Consequence:** before emitting any state-bearing action (comment that a review is pending,
transition to In Review, file a spin-off ticket), confirm the *current* state on the system of
record — never infer it from the narrative. "I'll open a PR" is not evidence a PR exists. This is
the line between a **correlator** (reads event streams) and a **reconciler** (must diff against
live Jira/GitHub *now*). It is why the reconciler is a distinct, verification-heavy stage.

## 2. The correlator will hallucinate ref↔issue links (→ `rules/verify-correlations.md`)

A scan subagent once confidently reported a `repo#573 = PROJ-3852` binding where both halves were
wrong: the PR belonged to a different ticket, and the issue key was unrelated work. Also seen: a
"create ticket" event with a **future-dated timestamp** for a ticket that did not exist — a
planned/simulated action in a transcript, read as real.

**Consequence:** every proposed `(narrative, issue-key)` binding must be verified against the real
issue (does `getIssue(key)` resolve, and does its summary/component match?) before it drives an
action. Confidence scores don't save you — a hallucinated match can be high-confidence. Treat any
ref whose timestamp is in the future, or that doesn't resolve, as a transcript artifact. Cheap API
verification *before* the review queue keeps the queue signal-rich. This is why `internal/correlator/refs`
produces *candidates* and the reconciler owns resolution.

## 3. Logical-effort clustering is load-bearing (→ `rules/env-mirror-fanout.md`, `internal/correlator/fanout`)

Infra work fans out: one logical change ("switch the shared module to managed mode") becomes ~12
near-identical PRs, one per region. Reviewing all 12 is **one** review effort, not 12. Tracking
them as 12 rows is noise; a ledger that says "reviewed 12 PRs" over-counts the same way listing
them individually under-represents the single decision — and it inflates velocity.

**Consequence:** detect fan-out families and collapse them into one narrative with a PR/issue
range. The heuristic that worked: normalize titles by folding known region/environment tokens
(after `for`/`in`/`switch` connectors, as a trailing `(region)`, or a `-region` suffix), then
group by same-author + same-normalized-title + adjacent numbers. Implemented deterministically in
`internal/correlator/fanout`; the region set is configurable.

## 4. Dedup keys must be fully qualified and range-aware (→ `internal/correlator/refs`)

Two real bugs:
- **Bare-number ambiguity.** Once the scan went multi-org, `#382` was ambiguous
  (`repo-a#382` ≠ `org/repo-b#382`). A dedup key on the bare number silently merged distinct PRs.
  Fix: key on the full `owner/repo#N`.
- **Range expansion.** Ledgers collapse fan-out families into ranges (`#655-665`). A dedup set
  built from the literal text saw only the endpoints and re-added the interior members as "new".
  Fix: expand ranges into members *before* deduping.

The event store already dedupes on `(source, external_id)` at insert — the right layer for raw
events. But the *narrative/correlation* layer has its own dedup, and that's where these bit us.
`internal/correlator/refs` handles both: fully-qualified keys and range expansion, with a `max_span`
guard that errors loudly rather than expanding an absurd range (see #9).

## 5. Query scope silently creates permanent blind spots

A review-capture query was scoped to one org. The reviewer also reviewed upstream repos. Because
the query was org-scoped **and** advanced a forward-only cursor, real reviews were never captured —
and never would be, because the cursor had moved past them. Widening the scope only fixed it going
forward; the pre-cursor gap needed a separate backfill.

**Consequence:** a collector's scope/filter is part of its cursor's meaning. If you widen scope,
the old cursor is no longer valid for the new scope — you owe a backfill over the widened scope
back to some horizon, not just forward. Prefer the widest defensible collector scope from day one;
narrowing later is safe, widening later leaves a hole. Treat any scope change as a backfill trigger.

## 6. PR state ≠ review state

The cheap PR-state call (open/merged/mergeable) does not tell you whether a review was left or what
verdict. A CHANGES_REQUESTED review was invisible to the periodic refresh — it showed only as an
`updated_at` bump the refresh read as noise. Detecting "I reviewed this" vs "waiting on the author"
required the reviews endpoint specifically.

**Consequence:** when a tracked PR's `updated_at` moves, fetch the reviews to see *what* changed.
Relevant to both credit (a review is work) and state (a review changes who's blocked).

## 7. Review staleness via head-SHA comparison (→ `rules/review-staleness.md`)

A review is "done" only until the author pushes commits past the reviewed SHA. Comparing the PR's
current head SHA to the `commit_id` of the reviewer's latest review discriminates correctly: head
advanced ⇒ re-review owed; head unchanged ⇒ still waiting on the author. Zero false positives in
practice. Store the reviewed `commit_id` with the review event and diff against head each pass.

## 8. Bot noise dilutes a credit ledger (→ `rules/bot-pr-noise.md`)

Dependency-bump approvals from `renovate[bot]`/`dependabot` dominated the raw review count (18 of
23 in one window) and buried the substantive reviews. Dropping them entirely lost legitimate
volume; itemizing them buried the signal.

**Consequence:** exclude `[bot]`-authored PRs from *itemized* narratives, keep them in a rollup
count. More broadly: a review or commit is not automatically creditable work — apply a substance
filter to what counts as trackable effort at all.

## 9. Window sizing: page by time, never silently drop

A transcript scan over a busy multi-day window blew the token budget. The safe design: subdivide
the time window, recurse if still too large, union the results — and **error loudly rather than
silently truncate** when a single window can't fit. A subtle trap: when over budget, a subagent
"helpfully" narrowed *what it extracted* (dropping a whole signal class) instead of narrowing the
*window* — losing data while appearing to succeed.

**Consequence:** the batch correlator splits by time and merges narratives across sub-batches; it
must never cope by quietly extracting less. This is also why `internal/correlator/refs` raises on an
over-`max_span` range instead of truncating: silent data loss is the enemy.

## 10. Ownership is a first-class dimension

Items move between people. Someone diagnoses a problem, writes a runbook, files a ticket, and
**assigns it to a teammate** — at which point it leaves their active list and becomes "waiting on
X". Same with reviews: once you request changes, the ball is in the author's court. A tracker with
only "done / not-done" mis-represents all of this.

**Consequence:** model ownership/assignee transitions explicitly. "Work happened" and "work is now
mine to act on" are different axes; assignee + review-state + who-pushed-last together determine
which. This also decides what belongs in a given person's digest vs. merely tracked.

## 11. Exclude the tool's own workspace (→ `internal/collector/claudecode` `exclude_cwds`)

The predecessor's scanner had to skip its own workspace, or it would ingest its own bookkeeping
sessions as "work" and spiral. unjira's executor writes to Jira, which is itself an observed
stream — the loop closes intentionally — but the collectors need the same hygiene: the
`claude_code` collector skips configured self-directories (unjira's own repo), and the Jira
collector should distinguish the bot account's own writes from human activity so it doesn't
re-narrate its own comments as new work.

## 12. Verify before outward/destructive actions; surface ambiguity, don't auto-apply

Every high-confidence-but-wrong case above was caught because the workflow verified external refs
before writing and surfaced ambiguous reclassifications for human confirmation instead of
auto-applying. The review queue is unjira's version of this. What belongs there is not just
low-confidence items, but any action that (a) hinges on an unverified correlation, (b) transitions
state on inferred rather than confirmed outcome, or (c) would be hard to reverse. A confident
hallucination is more dangerous than a flagged unknown.

---

## 13. A rule keyed on an entity's current parent is defeated by reparenting

Discovered 2026-08-28 while building `triage`'s re-clustering (`internal/triage`,
`store.EligibleEventIDs`).

Committed work must not be reattributed: unjira cannot unpost a comment, un-transition a status, or
un-create an issue. The rule was a watermark — an event linked to a narrative *before* that
narrative's last applied action is frozen, everything since is reshufflable. Sound in isolation, and
it needs no schema change, since `narrative_events.linked_at` and `actions.executed_at` already share
a timestamp format.

But the watermark is evaluated **per narrative**. So relinking a frozen event onto a narrative that
never committed makes it eligible again — proven directly:

```
BEFORE:                on A eligible=[]  (empty = frozen)
AFTER relink onto B:   on B eligible=[1]
```

A merge could therefore *launder* a frozen event into an unfrozen one. Nothing looks wrong
afterward: both narratives exist, the event exists, no error is raised. Every later pass then treats
committed work as reshufflable.

The instructive part is the fix. Guarding the move — "refuse to relink a frozen event" — would work
and would depend on every future caller remembering. Instead the *direction* became determined: the
committed narrative is always the merge target, because a tracker mutation already made it the
workstream of record. Frozen events therefore live on the target and are never relinked at all. The
unsafe case stops being forbidden and becomes **unreachable**.

The same reasoning shaped the seam: `UnlinkNarrativeEvents` takes explicit event ids rather than "all
of narrative N", so a caller cannot express the laundering case in one call. Two drills confirm it —
skipping the unlink fails immediately, and asking for "all events" does not compile, because no such
accessor exists.

**Generalization worth carrying:** when an invariant is keyed on an entity's *current* parent, any
operation that reparents can launder it. Prefer making the unsafe reparenting inexpressible over
checking for it, because a check lives in one place and callers multiply.

## 14. An unexported parameter type can make an exported function silently useless

Discovered 2026-08-29 while wiring `triage`'s `[e]dit` (`internal/reconciler/rework.go`).

`reconciler.Redraft` was exported, tested, and documented — and uncallable from any other package.
Its `verified []verifiedLink` parameter names an unexported type. Naming that type from outside
fails to compile, which is fine; a caller learns immediately. The problem is what happens next:
`nil` satisfies the slice, so the *workaround* compiles. And `actionsFromVerdicts` drops every
verdict whose `issue_key` is absent from the verified set, so a nil `verified` discards the model's
entire response:

```
reconciler: narrative 1 redraft named unrecognized issue_key "DEVSBX-9", ignoring
err=<nil> actions=0
```

No error. No actions. One LLM call spent. A caller doing the only thing that compiles gets a
successful no-op.

Two failure modes compounded. The type system blocked the correct call while permitting an incorrect
one, and a defensive "drop what we cannot verify" policy — right on its own terms, from
`rules/verify-correlations.md` — converted the incorrect call into a silent success rather than a
loud failure. Neither is a bug alone.

The fix was not to export `verifiedLink`. It pairs a store row with live tracker state read under a
specific pass, so a caller able to construct one could assert verification that never happened,
which is the thing that rule exists to prevent. Instead a new entry point *performs* the
verification: `ReworkOne` takes a narrative id and does the `GetIssue` itself. The unexported type
stays unexported; the capability becomes reachable through a function that cannot lie about it.

That also surfaced a stale assumption in `Redraft`'s own doc comment — it justifies reusing
already-verified links because live state "was confirmed earlier in this same pass," which is true
inside one `Reconcile` pass and false for `triage`, a separate process running possibly days later.
A comment can be accurate when written and become wrong when a second caller appears.

**Generalization worth carrying:** an exported function whose signature mentions an unexported type
is not part of the package's API, whatever it looks like. Before treating one as a seam, try calling
it from where it will actually be called — and check what the *compiling* call does, not only whether
it compiles. "It returned no error" and "it did the thing" are different claims.
## 15. A missing selection path reads as a missing prompt option

Discovered 2026-08-28 while investigating "the drafting prompt never offers `create`, so untracked
work produces no action" (`internal/reconciler/create.go`).

The report was accurate about the symptom and wrong about the cause, and the wrong fix looked
obvious — add `create` to `draftSystemPrompt` and a create becomes proposable. Every layer
downstream already supported it: the parser accepted the type, `suppressDuplicates` special-cased
it, `Persist` persisted it, `gate.Applier` applied it via `CreateIssue`, and
`tracker.default_project` existed solely to route it. Only the prompt appeared to be missing.

Probing first:

```
NarrativesWithActionableLinks (reconciler's backlog) -> 0 rows
NarrativesWithoutIssueKey (matching's backlog)       -> 1 rows
```

`Reconcile` selects narratives that HAVE a link, by construction. A narrative with none is never
passed to `reconcileOne` at all, so the drafting prompt is never consulted for it. Adding an option
to that prompt would have changed **nothing** — the code would look fixed, the tests would pass,
and untracked work would still produce no action.

The real gap was a selection path. And building one surfaced a second trap: the obvious accessor,
`NarrativesWithoutIssueKey`, selects on the denormalized `narratives.issue_key`, which
`MatchConfig.ConfidenceFloor` only promotes above the floor. A narrative with a REAL but
low-confidence primary therefore has `narrative_issues` rows AND a NULL `issue_key`, and appears in
its results — so a create path built on it would open duplicate tickets for work that IS tracked,
just not confidently. The correct question is `NOT EXISTS (SELECT 1 FROM narrative_issues ...)`:
does any link exist at all.

Both halves of the same mistake: reasoning about a pipeline from the stage where the symptom appears
rather than from the stage that decides what reaches it.

**Generalization worth carrying:** when a capability is fully plumbed but never observed, check
whether anything *selects* the input for it before concluding the last visible layer is at fault.
"Nothing happens" is evidence about reachability, not about the code you can see. And when two
accessors sound interchangeable, read what each one actually filters on — a denormalized column and
the table it denormalizes are not the same question.

## 16. A gate is only a gate if it prevents an action no other gate prevents

Caught in review 2026-08-29, designing the create path above
(`internal/reconciler/create.go`). Recorded because it was nearly shipped, and because the reasoning
that nearly shipped it is reusable in the wrong direction.

`create` is the least recoverable mutation unjira makes — a stale comment is deletable, a wrong
transition is in the changelog, but a spurious issue is a new object other people reference. That
makes "require an explicit opt-in before proposing one" sound obviously right, in the same register as
`auto_commit.graduated`.

It is not right, and one probe is enough to see why:

```
Decide(create @ confidence 1.0, nil)          -> DecisionQueue
Decide(create, {comment: graduated=true})     -> DecisionQueue
Decide(create, {create: graduated=false})     -> DecisionQueue
```

`gate.Decide` refuses to auto-apply a create unless a human graduated creates specifically. **The
review queue is the gate.** A flag guarding *proposing* would prevent nothing, because proposing
mutates nothing — and it would conflate "unjira suggests something" with "unjira acts", the
distinction this whole codebase is organized around. It would also make the capability unusable in
practice: nobody enables a flag whose own documentation implies danger.

What makes such a gate *feel* justified is usually a real defect standing next to it. Here it was
cost. Without a memory of refusals, a declined narrative is re-judged every pass:

```
pass 1: proposed=0  cumulative LLM calls=1
pass 2: proposed=0  cumulative LLM calls=2
pass 3: proposed=0  cumulative LLM calls=3
```

A recurring bill, not a hazard — and a gate "fixes" it only by disabling the feature. The actual fix
is to persist the decline and re-ask when new events arrive, reusing `DeltaEvents`, which is already
bounded by `max(actions.created_at)` and so answers "has anything changed since we last judged this"
with no new column.

**Generalization worth carrying:** before adding a gate, name the specific action it prevents and
check whether an existing gate already prevents it. A gate that reads as safety but only suppresses a
*proposal* is a feature flag wearing borrowed authority. And when one feels necessary anyway, look for
the real defect nearby — it is usually a cost, a missing memory, or an unbounded loop, and fixing that
is both cheaper and reversible where a gate tends to be permanent.

## 17. One symptom can be two bugs, and a passing unit test is the tell

Discovered 2026-09-01, running the pipeline over 300 real events for the first
time since several changes landed (`internal/correlator/match.go`).

A narrative whose events named `PAAS-4038` three times ended with zero
`narrative_issues` rows, so the create path proposed a brand-new ticket for work
that ticket already tracked. One symptom, and it took two fixes.

**First bug: the classifier could not see the events.** `buildMatchPrompt` sent the
narrative's title and summary plus each candidate's Jira metadata, while
`classifySystemPrompt` instructed the model to "judge from each candidate's
summary, description, and status, not from provenance strength alone" — asking for
evidence-based judgment and withholding the evidence. The two Jira events that
settled which of four candidates owned the work reached the model only as the word
`jira_event` on a candidate line. It returned an empty array; `resolveVerified`
discarded all four candidates with no error and no log.

**Second bug, found because the first fix appeared not to work.** After the prompt
fix, a real pass still left the work unlinked. The instinct was to keep re-running
and reading output. What actually resolved it was a unit test driving `matchOne` on
that narrative's exact events — which **passed**, linking the candidate
deterministically at 1.0 with no model call. A unit test that passes while the
pipeline disagrees is not a failed diagnosis; it is the diagnosis. The difference
had to be the pass, not the matching logic.

It was: `correlator.Match` used the candidate cap as the narrative cap.

```go
limit := cfg.CandidateLimit()                          // candidates per narrative
narratives, err := s.NarrativesWithoutIssueKey(limit)   // narratives per pass
```

`max_candidates_per_narrative=10` therefore examined ten narratives per pass, and
the corroborating evidence was in the data: linked narrative ids formed a
**contiguous block** (2–18) rather than a scatter. Genuine per-narrative
classification failures do not arrive in id order.

Compounding it, matching truncated **silently**, while `Reconcile` had logged its
own cap since it shipped. Twelve unexamined narratives therefore presented as
twelve matching failures.

**Generalizations worth carrying:**

- When a targeted unit test passes but the real pipeline disagrees, stop re-running
  the pipeline. The gap between them *is* the finding, and it is usually
  environmental: a limit, a config value, a selection query.
- Distribution is evidence. Contiguous ids mean a batch boundary; scattered ids
  mean per-item judgment. Read the shape of a failure before theorizing a cause.
- Two config values that share a variable will eventually be confused, whatever the
  variable is called. `limit` serving both "candidates per narrative" and
  "narratives per pass" is the whole bug.
- Every stage that caps a batch must log reaching the cap, and diagnostic parity
  across stages is a real property: one stage logging and its neighbour not is how
  a batch limit masquerades as a correctness failure.

## 18. A normalization built for reading becomes a lie when reused for writing

`tasktracker.StatusCategory` (`todo | in_progress | done`) was a **read-side** projection: it let
`Issue` report state uniformly whether the backend was Jira or GitHub Issues. Correct, and it still
does that.

Then `TaskWriter.SetStatus` took it as the transition *target*, because the type was already there
and looked like the right shape. Nothing revisited whether a projection adequate for reading is
adequate for acting. The whole justification lived in three lines of code comment, arguing GitHub's
open/closed as the common denominator across backends — and no spec ever recorded a decision.

Measurement inverted the argument. Mining the real PAAS workflow (19 statuses):

```
Ready for Dev -> In Progress   (x36)
In Progress   -> In Review     (x28)
```

Both are `indeterminate -> indeterminate`. So are `-> Blocked`, `-> In Test`,
`-> Waiting for customer`. The two edges unjira exists to propose were **inexpressible**, and
"move to In Review" was byte-identical to "move to Blocked" — so the Jira backend, matching on
category, would execute whichever transition the API happened to list first. Not an edge case: every
transition request in a real project would have hit the wrong status.

The direction of degradation is the lesson. A named target degrades gracefully to a two-name backend
(`open` and `closed` *are* names). A category target cannot upgrade to a nineteen-status one. **A
common denominator belongs as the fallback, not the representation** — and the argument for the
narrow type came from the least-capable backend, which is exactly the one whose needs generalize
worst.

**Generalizations worth carrying:**

- A type that normalizes for *reporting* is not automatically fit for *acting*. Reading tolerates
  lossiness because a human interprets the result; a write executes.
- The check is cheap: name a distinction the real system makes and ask whether the type can state
  it. Two statuses in one bucket answered this in one query.
- Before arguing with an implementation choice, grep the specs for the decision. "We decided this"
  is a claim, and it was false here — there was no decision to reverse, only an accident to correct.
- A correct decision can be defeated by the type it is routed through. The reconciler spec's
  "validate against the live issue's available transitions" was right and unchanged; implementing it
  via `AvailableStatusCategories` quietly turned it into "validate against a lossy projection of
  ground truth."
- The strictness that protects a lossy type stops making sense when the type stops being lossy. The
  old code *dropped* transitions whose category it did not recognize — right when a category
  licensed the write, wrong once a verified name did, and it was silently hiding three real statuses.

## 19. Monotonicity does not prevent fighting a handoff; it guarantees it

The obvious guard on work-derived transitions is to never propose a status earlier in the workflow
than where the ticket already sits. It fails on the case that matters most.

Security scans a ticket unjira called done, finds the vulnerability still present, and moves it
`In Review -> In Progress`. That is simultaneously evidence that work happened *and* that more work
remains — exactly what a reconciler should represent. Now apply monotonicity: unjira still holds
PR-created evidence, still concludes `In Review`, and proposes moving it forward again. Every pass,
until the evidence ages out.

Direction is not the discriminator. **Recency is:** propose only when the work evidence postdates the
issue's last status change. One rule covers both cases, needs no notion of forward or backward, and
generalizes to handoffs from systems unjira does not even collect from.

**Generalizations worth carrying:**

- When a guard is meant to prevent a conflict, check it against the conflict's real shape. This one
  was derived from "don't walk tickets backwards" and never tested against "somebody else moved it."
- "Never do X" guards are suspicious when X is something a human legitimately does. The question is
  not whether the action is allowed but whether *this* actor has current information.
- Prefer a freshness test to a direction test when reconciling against a system other agents also
  write to. Direction encodes an assumption about who is right; freshness measures who knew last.

## 20. A drill that does not compile proves nothing, and a filtered test run hides that

Verifying a fix by breaking it and confirming the guard test fails is the habit that has caught every
real defect in this work. It has a failure mode.

Two of three drills on the staleness guard were patched in a way that left an unused import, so the
package did not build. The test command was filtered through `grep -E "^--- FAIL"` — and a build
failure prints no `--- FAIL` lines at all. Both drills read as "no failures," which looks identical to
"the code is fine" and is the opposite of what happened.

The third drill then genuinely produced no failures while compiling, which was a real finding: the
status-change exclusion it disabled was not load-bearing for any test written. `LatestStatusEvent`
returns the *newest* status event, so a status event in the delta can only equal its timestamp, and
ties suppress either way. The exclusion only bites for an event `LatestStatusEvent` *skipped* — one
with `field=status` but no readable destination — which can be arbitrarily newer. That case got its own
test, and the drill then failed as it should.

**Generalizations worth carrying:**

- Check the drill compiled before reading its result. `go vet` before `go test`, or grep for `FAIL`
  rather than `--- FAIL`, which catches build failures too.
- Filtering test output can invert its meaning. A filter tuned for one failure shape reports every
  other shape as success.
- A drill that produces no failure is information, not a formality to skip past. Either the guard is
  not load-bearing, or the test does not discriminate — both worth knowing, and the second is a test
  that would have passed against a broken implementation.

## 21. An undeclared map key is a contract nobody signed

`events.Artifacts` is `map[string]any` with no declared keys. Every key in it — `issue_key`,
`git_branch`, `ticket_keys`, `connection` — was therefore a private convention between the collector
that wrote it and whichever consumer hardcoded the same string literal and hoped.

That held up while consumers were few. It broke on the staleness guard, where the reconciler needed to
know an event *was* a status change. The first implementation tested
`Artifacts["field"] == "status"` — which is Jira's **changelog** vocabulary — redeclared as constants
in the reconciler, under a comment claiming "any collector that supplies status history uses this
contract."

Two things wrong with that, and the second is worse:

1. No other collector existed to honor the claim. The comment *asserted* a cross-collector contract
   into being, which is exactly how `tasktracker.StatusCategory` became load-bearing (incident 18).
2. The shape could not generalize. GitHub Issues has no `field` concept; open/closed arrives as a
   timeline event. A GitHub collector would have had to emit a fake Jira-shaped artifact to be
   noticed at all — or, more likely, the guard would silently never fire for it, which is the
   invisible failure the guard exists to prevent.

Fixed by moving the contract into `internal/events` as named keys plus `SetStatusChange` /
`StatusChangeOf`, and by changing the *test*: an event is a status change if a **destination** was
recorded, not if it carries a particular backend's field name.

**Generalizations worth carrying:**

- A shared contract belongs in the shared package, not restated in each consumer. Two packages
  spelling the same string literal are not sharing a contract; they are independently guessing.
- Detect a fact by the fact, not by one producer's vocabulary for it. "A destination was recorded"
  survives a new backend; "field == status" does not.
- A doc comment cannot create a contract. If it says "any X does this" and only one X exists, that is
  a plan, and writing it as present tense is how a plan gets relied upon.
- Unifying two definitions of the same predicate can delete a test case. When
  `StatusChangeOf` and the store query agreed on "requires a destination," the divergence one test
  exercised stopped existing — that test had to be replaced with the case that still discriminates,
  not deleted or forced.
- A reader whose only protection is its writer is unprotected. `SetStatusChange` refuses an empty
  destination, which made the reader's own check undrillable until a test set the artifacts directly —
  and a store round trip or a future collector can do exactly that.

## 22. Two correct functions in the wrong order are a bug no unit test can see

Multi-hop needed two things: resolve a route to the target, and stop
`floorConfidence` from zeroing an action whose target is not directly offered. Both were implemented
correctly. Both had passing unit tests. The feature was still completely broken.

`floorConfidence` runs inside `toProposedAction`, during drafting. The route was attached afterwards,
in a separate filtering pass. So at the moment flooring ran, `action.Route` was always empty, the
route-aware branch never fired, and every multi-hop action was floored to zero — then dutifully
carried through the rest of the pipeline at confidence 0.

No unit test could catch it. `resolveRoute` was tested directly and passed. `floorConfidence`'s
route-aware branch was tested directly and passed. The bug lived entirely in the *sequence*, which is
visible only from a caller that runs both.

The fix was to move routing into `toProposedAction`, so the decision that depends on the route is made
where the route is computed rather than one pass later.

**Generalizations worth carrying:**

- When B's correctness depends on A having run, put them in one function or make the dependency a
  parameter. "A then B" enforced only by call-site ordering is an invariant with no enforcement.
- A feature needs at least one test at the layer where its pieces compose. Unit tests prove each piece
  works; they cannot prove the pieces are wired in the right order. This is the third time that gap
  has cost real time here (see incidents 17 and 20) — the pattern is now: write the end-to-end test
  FIRST when a change spans more than one function.
- The tell was a suspiciously specific failure: not "no action proposed" but "action proposed at
  confidence 0." A value that is exactly the floor, when a floor exists, means the floor fired — so
  ask what the floor saw, not whether the feature ran.

## 23. Inserting above a function orphans its doc comment, and only the linter notices

Twice in one change, adding a new declaration just above an existing function put the new code
*between* that function and its doc comment. Go then treats the comment as a floating comment and the
function as undocumented — `revive` flagged both as "exported function should have comment," which read
like a nit about my new code and was actually a report that I had detached documentation from two
pre-existing functions.

Nothing else catches it. It compiles, every test passes, `gofmt` is happy, and reading the diff shows a
sensible new function above a sensible old one. The damage is only visible in the rendered godoc, or in
a lint message that names the *victim* rather than the cause.

**Generalizations worth carrying:**

- When inserting a declaration near an existing one, anchor on the existing **doc comment**, not on its
  `func` line. Text-anchored edits land wherever the anchor is, and the anchor most people reach for is
  the signature.
- An "exported X should have comment" finding on code you did not write is usually not a missing
  comment — it is a comment you separated from its owner.
- The lint message names the wrong file position for this class of defect. Read what moved, not what
  was flagged.

## What these validate about the architecture

- **The correlator/reconciler split is the core defense.** The pain came from conflating "extract
  what the transcript says" (deterministic, cheap) with "judge what it means and whether it's done"
  (must verify against live state). Keeping collectors dumb and putting all verification in the
  reconciler encodes that separation.
- **Deterministic pre-filters keep the LLM's queue clean.** `internal/correlator/refs` and
  `internal/correlator/fanout` are pure, testable functions that run *before* any model, so the review
  queue stays signal-rich and cheap API checks catch hallucinations early.
- **Batch beats real-time** for the fan-out reason (#3): whole narratives need the whole batch.
  Real-time would fragment a 12-region change into 12 unrelated events.
- **`rules/` as human-auditable markdown is the right shape.** Norms accumulate as reviewable text,
  version-controlled, and lifting team-level norms into a shared repo later is a `git remote`, not
  a redesign.
