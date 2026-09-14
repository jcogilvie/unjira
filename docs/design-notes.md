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

## 24. A prompt rule cannot express a structural precondition

The first triage pass over real data (2026-09-02, 173 events, 62 narratives) produced 21 proposed
comments. **18 of them were unjira paraphrasing Jira back onto Jira.** The worst was a long,
genuinely well-argued RCA comment proposed on PAAS-4000 — whose narrative contained exactly [status
`Discovery → In Progress`, that issue's own description ×4, status `In Progress → Done`]. Every
sentence in the proposed comment came from the description of the issue it would be posted on.

The tempting diagnosis was "the rules aren't wired up." That was wrong twice over.
`rules/no-self-narration.md` existed, had been written the day before *from this exact class of
observation*, and **was** loaded into the drafting prompt (`rules.Render` in `draft.go` and
`create.go`). It was also being obeyed. Its content is a constraint on how to *lead* a comment:
"never make a status change you did not perform the subject of what you write." It says nothing
about whether a narrative with no work evidence should produce a comment **at all** — and no amount
of rewording gets it there, because the model cannot see the provenance of its own inputs. Asked to
judge whether all of its context is tracker bookkeeping, it has only the text, which reads like
perfectly good source material.

An earlier related claim of mine was also wrong, and checking it mattered: I asserted that fixing
the correlator's clustering (#179) would later make "some of the suppressed comments legitimate."
Measured, 17 of the 18 issues had **zero** non-tracker evidence anywhere in the store; the 18th had
three incidental key mentions scraped from a session about unjira itself. There was no rescue
population. The mechanism was plausible and the data said no.

The fix is a precondition in code — `events.AnyWorkEvidence` over the delta, consumed by
`reconciler.suppressTrackerEcho` — keyed on a marker the *producing collector* declares
(`events.SetTrackerRecord`), because only the producer knows whether it read a tracker's own
bookkeeping or observed work.

**Generalizations worth carrying:**

- **A rule about phrasing and a rule about eligibility are different artifacts.** When a norm in
  `rules/` is being followed and the bad output persists, ask whether the norm can even express the
  constraint. Prompt rules shape *how* the model says something; only code can decide *whether* it
  gets to.
- **Don't infer provenance from a source name or an incidental field.** "Is this the jira collector?"
  is #21 again. "Does it have an `issue_key`?" is worse — a CI run or a commit can name an issue
  truthfully while being real work evidence. Declared capability, set by the producer, in the one
  function every emitted event passes through.
- **A plausible mechanism is not a finding.** "Some of these become legitimate later" survived
  because it sounded like systems thinking. One query killed it. The same defect class as a stale
  status line, just wearing better clothes.
- **Mark it where every event passes, not at each call site.** `annotate` is the single choke point
  in the Jira collector; a future event type is a tracker record too, and needing to remember is how
  a marker silently stops covering the collector it was written for. The test that pins this
  (`TestEveryEmittedEventIsMarkedATrackerRecord`) is in the *collector* package, because every
  reconciler test constructs its own events and would stay green while the marker vanished.

## 25. A process rule that compensates for a structural problem is a finding about the structure

Incident 22 produced a rule: *write the end-to-end test first when a change spans more than one
function.* Sound advice, and it has caught real bugs since. But it is worth asking what the rule was
compensating for.

The reconciler's four suppression filters shared an identical **return** contract —
`([]ProposedAction, []string)` — and four different **input** shapes. `dropUnroutable` took
`(verified, drafted)`, `suppressTrackerEcho` took `(delta, drafted)`, `suppressStaleTransitions` took
`(delta, verified, drafted)`, `suppressDuplicates` took `(store, narrativeID, drafted)`. Because
nothing unified them, `reconcileOne` hand-wired four calls in sequence and re-appended to
`result.Suppressed` after each.

So the *order* of suppression — which is load-bearing, and which each filter's doc comment argues for
— existed only as the order of statements in one function. **No test could assert it, because there
was nothing to assert it against.** That is precisely the gap incident 22 fell through: two correct
functions, composed wrongly, invisible to every unit test.

The fix was a uniform filter contract and the chain as data (`internal/reconciler/filters.go`), after
which the order is a slice that a test reads directly, and a dropped filter fails three tests. The
behaviour did not change; every pre-existing test passed untouched.

**Generalizations worth carrying:**

- **When a rule exists to compensate for a shape, fix the shape.** "Remember to test end-to-end
  because composition is invisible here" is a standing tax on every future change. Making composition
  visible pays it once. The rule stays useful — it is just no longer the only line of defense.
- **A shared return type with unshared parameter types is a near-miss abstraction.** Four functions
  agreeing on what they produce and disagreeing on what they consume is a strong signal that a context
  object is missing, not that the functions are unrelated.
- **Uniformity is worth an unused parameter.** Three of the four filters do not need the store. Giving
  them one they ignore is cheaper than the uncomposable chain that caused an incident — and the cost
  is documented on the type rather than left for a reader to wonder about.
- **Order-as-data lets order be reviewed.** A reordering is now a test failure with a reason attached,
  where before it was a diff that read as harmless.

## 26. Eventual consistency is non-monotonic, so a readiness probe cannot fix a race

An integration test failed intermittently — roughly one run in three — with
`a successful first pass must store a watermark, or the second pass proves nothing`.
The assertion is about cursor logic, so the symptom pointed at the reconciler's watermark handling.
The cause was Jira's search index.

**The shape.** The test creates an issue, then runs the collector, which finds issues by JQL. Jira's
search index lags issue creation, so the test already had a readiness probe: poll until the issue is
findable, *then* collect. **The lag's median is ~3s and its tail reaches 24s** — measured on this
instance, seven create-then-poll samples: 1, 2, 3, 4, 9, 12, 24 seconds. That probe was not enough, and the
reason generalizes past this test:

> **Eventual consistency is not monotonic.** A document can become findable, then transiently stop
> being findable, while replicas converge. A probe can only ever establish that the index *was*
> ready — never that it will still be ready one HTTP call later.

So no amount of pre-checking fixes it. The retry has to wrap the operation that can lose the race,
not run before it.

**A wrong turn worth recording.** The first diagnosis was that the probe polled `key = X` while the
collector's query goes through `EffectiveJQL` as `project IN ("SCRUM") AND (key = X)` — two different
index paths, one satisfied before the other. That is *true* and it is *not the cause*: with the probe
fixed to poll the collector's own effective JQL, the failure reproduced on the second run. A plausible
mechanism confirmed by reading code is still a hypothesis until it is run.

**This is a test-framework hazard, not a production one**, and the distinction is worth stating
because the fix belongs in exactly one place:

- `watch`'s pass collects FIRST and applies LAST (`cmd/unjira/main.go`'s `runWatchPass`), so a write
  and the next search are a full interval apart — 5 minutes by default against a ~3 second lag.
- The reconciler deliberately *discards* unjira's own writes (`dropSelfAuthored`), so a self-authored
  event arriving late is the desired outcome either way.
- A pass that matches nothing leaves the watermark alone and retries next pass
  (`if highest.IsZero() { return nil }`). Correct behaviour; the test turned it into a failure only by
  asserting within one pass.

**Generalizations worth carrying:**

- **create-then-SEARCH is racy; create-then-FETCH-BY-KEY is not.** `GET /issue/{key}` reads the
  document store and is immediately consistent. Only 2 of 17 live tests were exposed, and both were
  collector tests — but that ratio is an artifact of having two collectors, not of the problem being
  rare. Every collector's live test has to create data and then find it by query, so this is the
  shape *every future collector* will hit.
- **Retry the operation, never the assertion.** `collectUntilMatched` retries the collector; the
  assertions after it still fail on the first wrong answer. A blanket retry around assertions would
  mask the collector regressions this tier exists to catch.
- **A retry's failure message must name both possibilities.** "Either Jira's index is far behind, or
  the collector genuinely does not match this issue (a real regression)" is what keeps the retry from
  converting a permanent bug into a slow timeout with no explanation.
- **Bound tries and elapsed time independently.** Tries bound cost; elapsed time bounds how long a
  human waits. A slow dependency should fail on the clock, not after N slow attempts.
- **Size a timeout for the TAIL, not the median — and know which bound actually binds.** The lag's
  median (~3s) was measured correctly and written down correctly; every timeout in this tier was then
  sized against it. The tail is 24s. The original 20x1s poll gave exactly 20s and failed ~1 run in 3;
  an 8-try exponential budget exhausted at ~24.6s, landing *on* the worst observed value, which
  improved the rate enough to look fixed without being fixed. Separately: `WithMaxTries(8)` and
  `WithMaxElapsedTime(45s)` were set as independent bounds, but tries always tripped first, so the
  elapsed-time ceiling was dead code that read like protection.
- **Re-measure before enlarging a budget.** A steadily-growing tail is a fact about the dependency
  worth knowing; raising the number without re-measuring converts a diagnosable trend into a
  permanently-oversized timeout.
- **A check that could not RUN must not produce a verdict.** The failure diagnostic classified a
  failure as infrastructure-or-real-bug by re-searching without the watermark — and swallowed the
  re-search's own error, so a check that never ran resolved to a confident "infrastructure, re-run".
  A deliberately-broken `watermarkClause` reported exactly that. Confidently wrong is worse than
  ambiguous, especially for a contributor deciding whether their PR is at fault; the default is now
  UNDETERMINED, with distinct wording for "could not build the query" and "the re-search itself
  failed".
- **Don't reach for long-lived test fixtures to dodge a timing problem.** It trades a timing bug for a
  state bug: fixtures accumulate transitions and comments from every prior run, so tests start
  depending on where the last run left off, and a freshly-created fixture still races anyway.

## 27. A proposed fix is a hypothesis; check it against the example that motivated it

Finding F9 recorded a real defect precisely: `gatherCandidates` ranked issue-key candidates and
truncated to a cap, and within the prose tiers ties broke **alphabetically**. It named a measured
case — a session mentioning 67 keys where `PAAS-4001`, a ticket with genuine recent Jira activity,
ranked 41 of 65 and was cut. It then proposed a fix: a corroboration tier ranked above prose, gated
on a bounded recency window.

The finding was right. **Its proposed fix was wrong, and the finding contained the evidence.**

Re-measured on current data (73 keys after a clean re-collect), **30 of those keys corroborate** —
still three times the default cap of 10. So adding the tier moves `PAAS-4001` from rank 45/73 to
26/30 and it is *still truncated*: an alphabetical sort inside a pool larger than the cap re-decides
exactly as before. The tier relocates the defect instead of removing it.

The proposed recency *window* fared worse, because its correct value is a narrow band:

| window | corroborated pool | outcome |
|---|---|---|
| 7d | 0 | the answer is outside it |
| 14d | 4 | the answer is outside it |
| 21d | 8 | works |
| 30d | 10 | works |
| 60d | 22 | over cap — alphabetical decides again |

> **A config knob whose correct value is a narrow band nobody can calibrate is not a knob, it is a
> latent bug with a default.**

What actually fixes it is ordering *within* the tier by most-recent activity. That needs no knob and
holds at every cap ≥ 8 and every window ≥ 30d — the window stops being load-bearing, so it was not
added. Measured after: `PAAS-4001` moves 45/73 → **6/73**.

**Why this is worth a numbered incident.** The failure mode is not "F9 was wrong". It is that a
finding's *diagnosis* and its *proposed fix* have completely different evidentiary standing, and
prose puts them side by side in the same confident voice. The diagnosis here was measured; the fix
was reasoned. Only the diagnosis had been checked against the data, and nothing in the document
marked the difference.

So: **before implementing a documented fix, run the finding's own cited example through it.** Not the
abstraction the fix is described in terms of — the concrete case. Two of the three tests that now
cover this exist only because that check was run first, and the drill that proves the point is
deleting the recency ordering: the resulting code *is* the fix F9 proposed, and it fails the test
suite for F9.

**Corollary for how findings are written.** Recording "candidate fix, **not yet chosen**" was what
made this recoverable — it framed the fix as a hypothesis rather than a decision, so measuring it was
an obvious step rather than a challenge to a settled question. Keep writing them that way, and keep
the rejected alternative in the record with its numbers: `IssueActivity`'s doc comment explains why
there is no window knob, which is the question the next reader will otherwise ask.

## 28. A denormalization with no reader is a bug waiting for a writer to forget it

`narratives.issue_key` duplicated a fact `narrative_issues` already held: which key is a narrative's
`primary`. It had **zero readers** — it was hydrated into `correlator.Narrative.IssueKey`, which
nothing ever consulted, and `buildClusterPrompt` never rendered it. Same for the `confidence` column
beside it. Both were write-only.

That did not make them harmless. Three of the four writers of the primary role forgot to update the
column, and the omissions were invisible precisely *because* nothing read it — until one query did.

**The mechanism.** `persistLinks` wrote the primary link row, then checked `match.confidence_floor`
and returned early before setting the column. Both statements were in one transaction, so it
committed **atomically into a state where the two disagreed**: primary link present, column NULL.
Matching's backlog selected on the column, so the narrative never left the pool; every pass
re-matched it; and when a re-match assigned a *different* primary, the partial unique index
`one_primary_per_narrative` rejected the insert and **aborted the entire pass**. A drain crashed on
narrative 15 — `primary=PAAS-4002` at 0.55 against a floor of 0.70.

> A denormalized column that nothing reads still has to be maintained by every writer, and nothing
> tells you when one stops. The read that eventually arrives is the one that fails.

**The fix was already in this file.** When the *create* path hit the same trap, incident notes here
recorded the answer verbatim: "The correct question is `NOT EXISTS (SELECT 1 FROM
narrative_issues ...)`". Two of the three accessors were built or corrected to ask the link table.
Matching was never revisited, so it kept asking the column for months.

| accessor | asked | correct? |
|---|---|---|
| `NarrativesWithNoIssueLink` (create) | `NOT EXISTS` any link | yes |
| `NarrativesWithActionableLinks` (reconciler) | `EXISTS` link + role | yes |
| `NarrativesWithoutIssueKey` (**matching**) | the denormalized column | **no** |

**Two lessons, and the second is the one that generalizes.**

*A fix applied to the instance is not applied to the class.* The create path's fix was correct,
documented, and complete — for the create path. Nothing swept the sibling accessors, so the same
defect sat two functions away in the same file. When a bug is worth a design note, the note should
end with "where else does this shape exist", and that sweep should happen in the same PR.

*Deleting beats disciplining.* The tempting fix was to make each of the three writers also set the
column. That is the same discipline that already failed three times out of four, so it would fail a
fourth. The column, its sibling, their setter, and both struct fields were deleted instead — the
duplicated fact cannot drift because it no longer exists. Cost: nothing. Every reader was already
asking the wrong question or not asking at all.

**No migration was needed, which is worth noticing.** `NOT EXISTS(primary link)` re-derives the truth
from data that was always correct, so narrative 15 self-repaired: it *has* a primary link, so it is
attributed, so it left the backlog. When a denormalization drifts, the normalized side is usually
still right — repair by deriving, not by patching rows.

**The class was then swept, per that lesson.** The only other column that looks like this is
`actions.issue_key`, and it is *not* a denormalization: `reconciler.SelectionRoles` includes
`same_work`, and `route.go` drafts against those links too, so an action legitimately targets an
issue that is not its narrative's primary. It records a per-action decision rather than duplicating
one. (It happens to equal the primary on every row in the current store — which is exactly the kind
of coincidence that would make a `COUNT(*)` check say "duplicate"; the schema and the drafting code
are what settle it, not the data.) No other instance of the shape exists.

**Verified by draining.** The pass that crashed now completes; matching's backlog went 38 → 26 and
recorded primaries 33 → 45 in one pass. The floor keeps its real job: it withholds the *assertion*
(`MatchResult.Primary`, which the renderer prints), never the *record*, which was always the
intent — `config.MatchConfig.ConfidenceFloor`'s own doc comment says "the floor governs what unjira
asserts, not what it records", because a dropped row would make a low-confidence match
indistinguishable from finding nothing at all.

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
