# Named status transitions — design

Replacing the three-category transition target with the tracker's own status names, and making
`workflow.Graph` load-bearing for the first time.

## Status: partially landed 2026-09-01, task #174 landed 2026-09-02

Sections 1, 2, the prompt half of 3, 3a (recency), and 4 (the graph cache) have landed. Section 5
(multi-hop) has **not** — it is a separate change, and this section will be updated when it lands
rather than describing it as done.

Section 3a's own "known gap" — a work-derived transition proposed with no staleness check, and no
signal that it happened — is closed as of 2026-09-02 (task #174). See the "known gap, and its fix"
subsection under 3a below for what landed.

**Landed:**

- `SetStatus(key, targetStatus string)`, `AvailableTransitions` replacing
  `AvailableStatusCategories`, the new `Transition` type. Both backends.
- `reconciler.ProposedAction.TargetStatus` is a `string`; the persisted payload carries a name.
- `gate.Applier` no longer validates the target against a closed enum. It could not: there is no
  closed set of names. It checks only that the payload is non-empty and lets `SetStatus` do the live
  check it already does. Deviation from the spec, which said to re-check against
  `AvailableTransitions` here — that would issue a second identical request and open a window
  between check and write, preventing nothing `SetStatus` does not already prevent (incident 16).
- The drafting prompt names work-derived transition evidence, requires exact status names, and
  forbids restating a status change somebody else made.
- `rules/no-self-narration.md`.
- `local_issues.status_category` renamed to `status`, holding a name. **No migration.** Nothing is
  live, so an existing dev database is deleted and recreated rather than migrated. I built an
  idempotent `ALTER TABLE` path first and removed it: a migration target in the schema before launch
  is permanent clutter bought to avoid `rm data/unjira.db`, and the repo's own "greenfield schema, no
  migration" note in the Scope section below already said so.

**Not landed, and why:**

- **Section 3a (recency)** needs `Issue.StatusChangedAt`, which needs the issue's last status change
  from a changelog read. The field was written and then removed from this change rather than shipped
  always-zero: a field nothing populates reads as "no status change on record," which under the
  recency rule means "propose," i.e. exactly the handoff-fighting behavior the rule exists to stop.
  The obvious cheap source (`statuscategorychangedate`) is wrong here — it moves only when the
  *category* changes, so `In Progress -> In Review` would not update it. `expand=changelog` looks
  free but Jira truncates embedded histories, and a truncated changelog yields a too-old timestamp,
  which fails in the same unsafe direction.
- **Sections 4 and 5** are in flight separately.

**Verification:**

```
go test ./...                          # 23 packages ok, 746 tests
go vet -tags=live ./internal/live/     # clean — the live tier is excluded from `go test ./...`
                                       # and silently broke CI on #27 for exactly this reason
golangci-lint run ./...                # 0 issues.
```

The fix was drilled by breaking it and confirming the guard fails: name matching disabled in
`SetStatus` → `TestTracker_SetStatus_PicksTheTransitionMatchingTheName` fails with
`expected: "41" / actual: "31"`. 41 is In Review; 31 is In Progress and shares its category. That is
the wrong write, reproduced.

The regression guard is `TestTransition_DistinguishesTargetsSharingACategory` plus
`TestTracker_SetStatus_PicksTheTransitionMatchingTheName`, both built on the real mined PAAS
transition set. The second one fails by construction under the old category-matching code: all four
destinations share `indeterminate`, so asking for In Review posted transition 31 (In Progress).

Live-tier tests for the named write path are written and compile but have **not been run** against
the dev instance in this change.

## Original design follows

## The decision being revisited, and why it is not in any spec

`tasktracker.StatusCategory` — `todo | in_progress | done` — appears in **no spec**. Not the
reconciler design, not the phase-1 design, not `design-notes`. Its whole justification is three
lines of code comment written during the `TaskTracker` extraction:

> Deliberately coarse: GitHub Issues has no named-status concept at all, only open/closed, so a
> category-based target is the common denominator across backends.

That was a *read-side* normalization, so `Issue` could report state uniformly across backends. Then
`TaskWriter.SetStatus` reused the same type for the **write** side, and nothing revisited whether a
projection adequate for reading is adequate for acting.

The reconciler spec's actual, well-argued decision is different and stays:

> **Transitions are validated against the live issue's available transitions, not a mined
> `workflow.Graph`.** The graph is statistically observed changelog history — `HasEdge` answers "has
> this ever been seen," which is a proxy for legality, not ground truth.

That decision is about *authority*, not about *naming*. Implementation routed it through
`AvailableStatusCategories`, which collapses `GetTransitions`' named results into the triad — so
"validate against ground truth" silently became "validate against a lossy projection of ground
truth."

## Why the triad cannot express the target case

Mined from the real PAAS project (`dev workflow --project PAAS`, 200 issues):

```
Statuses:  Backlog, Blocked, Closed, Discovery, Done, In Progress, In Review,
           In Rollout (Limited Availability), In Test, In Triage, New, Not-A-Risk,
           Open, Paused, Ready for Dev, Resolved, To Do, Waiting for customer,
           Waiting for support
```

| Category | Statuses |
|---|---|
| `new` | Backlog, Discovery, In Triage, New, Ready for Dev |
| `indeterminate` | Blocked, In Progress, In Review, In Rollout, In Test, Waiting for customer, Waiting for support |
| `done` | Closed, Done, Not-A-Risk, Resolved |
| `unknown` | Open, Paused, To Do |

The two transitions this work exists to propose are the graph's busiest edges:

```
Ready for Dev -> In Progress   (x36)
In Progress   -> In Review     (x28)
```

**Both are `indeterminate -> indeterminate`.** So are `-> Blocked`, `-> In Test`, and
`-> Waiting for customer`. Under the current type, "move this to In Review" and "move this to
Blocked" are the same request. And `Open`/`Paused`/`To Do` normalize to `unknown`, which
`AvailableStatusCategories` **drops entirely** — three real statuses are invisible to the reconciler.

The GitHub argument also inverts the right direction. A named target degrades gracefully to a
two-name backend (`open`, `closed` are names). A category target cannot upgrade to a nineteen-status
one. The common denominator belongs as the *fallback*, not the representation.

## What changes

### 1. `SetStatus` takes a name

The interface method changes rather than gaining a sibling. Two ways to do one thing is what
produced this problem — a read-side type reused for writes because it was there.

```go
// before
SetStatus(key string, target StatusCategory) error
// after
SetStatus(key, targetStatus string) error
```

`AvailableStatusCategories` is replaced by `AvailableTransitions(key string) ([]Transition, error)`,
where `Transition` carries both the name and its category:

```go
type Transition struct {
    // ToStatus is the tracker's own name for the destination — "In Review",
    // not "in_progress". This is what a ProposedAction targets.
    ToStatus string
    // ToCategory is the normalized bucket, retained for the cheap
    // direction check floorConfidence wants (a done -> new move is a
    // suspicious reopen regardless of names). Never the target itself.
    ToCategory StatusCategory
}
```

`StatusCategory` survives, narrowed to what it was originally for: reporting and coarse direction
checks. `Issue.StatusName` already exists and already carries the real name — the write path was the
only place the projection was load-bearing.

### 2. `ProposedAction.TargetStatus` becomes a name

`reconciler.ProposedAction.TargetStatus` changes from `tasktracker.StatusCategory` to `string`.
`actionPayload` writes `{"target_status": "In Review"}`; `gate.Applier`'s `transitionPayload` decodes
the same. `normalizeStatusCategory`'s syntactic check in the applier is replaced by a check that the
name appears in the live `AvailableTransitions` result at apply time — a stronger check than the
current one, since it validates against the tracker rather than against a closed enum.

### 3. The drafting prompt gains transition evidence, and a rule about self-narration

Two problems, both observed on real data.

**Transitions are never proposed.** Across two real passes over 300 events: 32 comments, 3 creates,
**zero transitions**. `draftSystemPrompt` says "move the issue to a different status" and lists legal
targets, with nothing about what constitutes *evidence* for a move. The prompt will name the
work-derived signals:

| Evidence | Proposed transition |
|---|---|
| A session's transcript shows work on the ticket's code | → In Progress |
| A PR opened for the ticket | → In Review |

Deliberately asymmetric: a transition is proposable from observed **work**, never from observed
**tracker state**. Tracker state is what tells unjira a move is unnecessary.

### 3a. Recency, not direction, decides whether to propose

The obvious guard is monotonicity — never propose a status earlier in the workflow than where the
ticket already is. It is wrong, and inverts into the exact failure it is meant to prevent.

Backward moves are ordinary and load-bearing. Security scans a ticket we marked done, finds the vuln
still present, and moves it `In Review → In Progress`. A test cycle produces findings and bounces a
ticket back for rework. Each is simultaneously evidence that work happened *and* that more work
remains — precisely the state a reconciler should be able to represent.

Now apply monotonicity to that case. unjira still holds PR-created evidence, still concludes
`In Review`, and proposes moving the ticket forward again — undoing the handoff. Monotonicity does not
stop unjira from fighting a human's deliberate move; it *guarantees* it, every pass, until the
evidence ages out.

Direction is not the discriminator. **Recency is:**

> Propose a transition only when the work evidence postdates the issue's last status change.

| Situation | Outcome |
|---|---|
| Security moved it at T2; our newest linked event is T1 < T2 | Propose nothing. The tracker knows something we don't. |
| Our newest linked event postdates the last status change | Propose, in **either** direction. |

One rule covers both cases, needs no notion of forward or backward, and generalizes to handoffs unjira
has never seen — including ones by systems it does not collect from. A move by anyone, for any reason,
supersedes evidence older than itself.

This sharpens the asymmetry above rather than weakening it. Tracker state still never *motivates* a
transition; it only ever *vetoes* one. What changes is the vocabulary of the veto: not just "already
there, unnecessary," but "changed after we looked, superseded."

The timestamp is available. `workflow`'s miner already reads changelogs via `StatusChanges`; this needs
one status change — the most recent — which is `Issue.StatusChangedAt`, populated on the live read path
beside `StatusName`. Absent it (a backend with no changelog, or a ticket never transitioned), the rule
cannot fire and the transition is proposable — degrading toward proposing, since a proposal is reviewed
and a suppression is invisible.

#### Status: landed 2026-09-01, and `StatusChangedAt` was never needed

The paragraph above is wrong about the mechanism, in a way worth keeping rather than editing out: it
assumed the timestamp had to come from a live changelog read, which is what made this section look
expensive enough to defer twice.

It does not. **The Jira collector already ingests status changes as events** — `trackedFields`
includes `status`, and every change becomes an event carrying the change's own `occurred_at`. So the
last status change is already in unjira's own store, and the guard needs no API call, no
`Issue.StatusChangedAt`, and no exposure to Jira truncating an embedded changelog.

Better still, the live read the reconciler *already* performs (`Issue.StatusName`, from `verifyLinks`)
supplies the other half. Comparing it against what the newest collected status event says the status
*became* answers a question a timestamp cannot:

| Live vs collected | Meaning |
|---|---|
| Agree | unjira's history is current for this issue, so its timestamp can be trusted |
| Disagree | somebody moved it since the last collect; unjira cannot know when or why |

That closes the freshness hole a local-only design otherwise has. A timestamp comparison alone is
blind for exactly the window that matters most: right after a handoff and before the next collect, the
collected history still shows *our* last known move, so the timestamps look fine and the proposal goes
out. So the guard is two checks:

1. **Superseded** — the collected status change postdates the newest work evidence.
2. **Stale collection** — the live status disagrees with the newest collected status change.

Check 2 runs first. If the collected history is stale its timestamp cannot support check 1, so
reporting "superseded" would name the wrong reason.

**One subtlety makes the two compose.** Once somebody else's move *is* collected it enters the delta as
a status event. Counting that as work evidence would make the status change its own justification, and
the suppression would silently stop working the moment collection caught up. So `newestWorkEvidence`
excludes status changes **on the subject issue** — not status events generally (another issue's move is
real activity in this narrative) and not the whole `jira` source (a comment or description edit is
work).

Implemented as `store.LatestStatusEvent` plus `reconciler.suppressStaleTransitions`, with the status
change declared through a new contract in `internal/events`.

**The contract had to move.** The first cut had the reconciler testing
`Artifacts["field"] == "status"` — Jira's *changelog* vocabulary, redeclared in the reconciler as
local constants with a comment asserting "any collector that supplies status history uses this
contract." That claim was invented: no other collector existed to honor it, and the shape does not
generalize. GitHub Issues has no `field` concept at all (open/closed arrives as a timeline event), so a
GitHub collector would have had to emit a fake Jira-shaped artifact to be noticed, or the guard would
silently never fire for it.

Root cause, which predates this work: `events.Artifacts` is a bare `map[string]any` with **no declared
keys**, so every key is a private convention between one collector and whichever consumer hardcoded the
same string. The correlator already did this twice (`issue_key`, `git_branch`).

So `internal/events` now declares the cross-package keys and two accessors — `SetStatusChange` /
`StatusChangeOf` — and the test for "is this a status change" is whether a **destination** was
recorded, not which backend vocabulary the event uses. Source-agnostic by construction: a consumer that
switched on `Source` would need editing for every new collector. The store query interpolates the same
named constants, so it and `StatusChangeOf` cannot drift.

The endpoints still have to be structured data either way: recovering the destination from the summary
means parsing a human-facing string on the transition arrow, which is not reserved in a Jira status
name, so a status genuinely containing it parses to the wrong destination.

**Deliberately not applied to `ReworkOne`.** That is triage's `[e]dit`: a human asking for a redraft.
The initiative is theirs and current by construction, so filtering the result would answer an explicit
request with silence.

**The known gap, and its fix (task #174, landed 2026-09-02).** With no collected status history the
guard cannot fire and transitions are unguarded — the pre-guard behavior, not something worse, but it
used to be invisible. Two seams now make it visible, at the two altitudes the gap actually has:

1. **Per-narrative.** `reconciler.FindUnguardedTransitions` scans a completed pass's
   `[]ReconcileResult` and names every proposed transition whose issue has no collected status change
   — i.e. every case where `suppressStaleTransitions`'s `HaveLastStatus` was false. `RunReconcile`
   (`internal/pipeline/reconcile.go`) calls it and attaches the result to
   `ReconcileRunResult.Unguarded`, and `RenderReconcileResult` prints each entry under its own
   narrative, alongside `Suppressed`/`LowConfidence`. This is deliberately NOT a field on
   `reconciler.ReconcileResult` itself, which is where it reads most naturally: `internal/reconciler`'s
   `types.go`/`reconciler.go`/`draft.go`/`persist.go` and `recency.go` were owned by a parallel
   in-flight change this session could not touch, so the annotation is a second pass over the already-
   computed result, re-deriving "was there collected history" via the same `store.LatestStatusEvent`
   call `verifyLinks` already makes (no store writes happen between `Reconcile` returning and this
   running, so it sees exactly what the guard saw).
2. **Once, at startup, when the cause is configuration.** `pipeline.HasEnabledStatusHistorySource`
   answers "can anything enabled ever supply this" from config and the collector registry alone, with
   no store or narrative involved. It works via a new marker interface, `pipeline.StatusHistorySource`
   (`SuppliesStatusHistory() bool`), which the jira collector implements and `claude_code` does not —
   deliberately not a name check for `"jira"`, per incident 21's lesson about hardcoding one
   collector's vocabulary into a general question. `cmd/unjira`'s `appContext.warnIfNoStatusHistorySource`
   calls it once per `watch`/`dev narrate` invocation (not per pass, not per narrative) and logs when
   nothing qualifies — silent on the `local` tracker backend, which has no real external tracker for
   anyone to move behind unjira's back (`internal/clients/local`'s own package doc), so the guard's
   premise does not apply and there is nothing to warn about. This is also what keeps `local`-backed
   offline tests quiet.

The two are deliberately not merged into one signal: "no history for THIS issue yet" is the ordinary
case (nobody has moved the ticket), and "nothing in this configuration can EVER supply it" is a real
gap. Conflating them would make the ordinary case read as alarming on every pass, or make the real gap
sound as unremarkable as an ordinary unmoved ticket.

Verified: `go test ./...` — 23 packages, 656 tests. `go vet -tags=live ./internal/live/` clean.
`golangci-lint run ./...` — 0 issues. Both new seams drilled by breaking them (inverting the
`haveHistory` check in `FindUnguardedTransitions`, and the local-backend/registry checks in
`warnIfNoStatusHistorySource`) and confirming the specific test fails, then restoring.

Verification: 23 packages, 795 tests, `go vet -tags=live` clean, `golangci-lint` 0 issues. Each half
drilled by breaking it and confirming the specific guard test fails.

**unjira narrates transitions it did not make.** The comment proposed for PAAS-4038 opened:

> "Status moved from Discovery to Ready for Dev. Implementation plan finalized: ..."

The human made that move. Reporting it back to them on the ticket where they made it is circular. The
rest of that comment had real content, so this is a constraint on the lead, not a suppression of the
action: a `rules/` entry (scope `reconciler`) saying a status change unjira did not perform is context
for judging what to say, never the subject of what is said.

### 4. `workflow.Graph` becomes load-bearing, with a cache

#### Status: landed 2026-09-01 (cache only — sections 1-3 and 5 are still design)

Built `workflow.Cached`/`workflow.CacheOptions`/`workflow.CacheStatus`/`workflow.MarkDirty` in
`internal/workflow/cache.go`, the `workflow.cache_ttl` config key
(`internal/config.WorkflowConfig.CacheTTL`, a `config.Span`), and wired `dev workflow` through
`Cached` with a `--refresh` flag, in `github.com/jcogilvie/unjira` PR (workflow-cache branch).

All three invalidation triggers below are implemented and tested. `MarkDirty` has **no caller
yet** — per this section's own scope, that lands with the `SetStatus`/`AvailableTransitions`
work in sections 1-3, which a separate, parallel change is implementing. `MarkDirty` is exercised
directly in `internal/workflow/cache_test.go` so the mechanism has coverage before it has a
caller.

Verification: `go test ./...` — `ok` for every package, including
`github.com/jcogilvie/unjira/internal/workflow` and `github.com/jcogilvie/unjira/cmd/unjira`.
`go vet -tags=live ./internal/live/` — clean. `golangci-lint run ./...` — `0 issues.`

One deviation from the paragraph below: the cache does **not** call `workflow.MineProject`
directly. It takes a `workflow.GraphProvider` (already existed) and calls `WorkflowGraph`, so the
mining call count and its `maxIssues` cap stay entirely provider-owned (`jira.Tracker.WorkflowGraph`
already hardcodes 200) rather than becoming a second parameter `Cached` would need to plumb through.
This also means `internal/clients/jira` and `internal/clients/local` needed no changes — both
already implement `GraphProvider`.

`Graph.Save`/`workflow.Load` were **deleted** rather than used: a cache entry needs `mined_at` and
`dirty` beside the graph, which their format cannot carry, so `cache.go` writes its own entry shape
around `Graph.ToMap`. That left them at zero callers and zero tests after the change that was meant
to give them a caller — uncalled, untested code that looks like the supported serialization path.

The cache file lives at `data/workflow-cache/<sanitized-key>-<hash12>.json` (see
`workflow.CachePath`, `workflow.DefaultCacheDir`): a `"data/"`-relative path matching
`config.DBPath`'s own default, and a hashed filename suffix so two project/repo keys that
sanitize to the same safe string (`"owner/repo"` vs `"owner-repo"`, or a deliberately
path-traversal-shaped key) never collide or escape the cache directory — see
`TestCachePath_DistinctProjectKeysNeverCollideEvenWhenSanitizedTheSame` and
`TestCachePath_TraversalShapedKeyStaysInsideTheCacheDir`.

`workflow.DefaultCacheTTL` is 24h, defended in its own doc comment: Jira workflows change on the
order of months, but 24h means at most one "wasted" ~40s re-mine per calendar day of `watch`
ticks even with no dirty-flag activity, since the dirty flag (once wired) is the real staleness
detector.

---

Today `internal/workflow` mines real graphs, computes BFS paths, and has **exactly one caller** —
`dev workflow`, a debug printer. `Save`/`Load` exist with zero callers.

Its package doc already describes the intended design, and it matches what this needs:

> Three-tier workflow knowledge: admin API when available, this observed graph for planning, and
> per-issue `GET /transitions` as ground truth at execution time. [...] Staleness: cache the graph per
> project; when the live transitions endpoint returns an edge the graph doesn't predict, mark it dirty
> and re-mine.

So: **one traversal mechanism, two roles.** The graph plans; the live endpoint authorizes each hop.
That preserves the reconciler spec's decision exactly — the graph never licenses a write.

**Cache invalidation, three triggers:**

1. **TTL** — a `workflow.cache_ttl` config key. Mining takes ~40s for 200 issues, far too slow
   per-narrative, and a workflow changes on the order of months.
2. **Explicit flag** — `dev workflow --refresh` re-mines and overwrites.
3. **Rejection-triggered** — the interesting one, and the package doc's own suggestion: when a live
   transition attempt is refused for a target the graph predicted was reachable, the graph is wrong.
   Mark dirty, re-mine on the next pass. A cache that only expires on a timer stays wrong for its
   whole TTL after a workflow edit; one that notices being contradicted self-heals.

`Save`/`Load` already handle serialization. What is missing is a cache path convention, the TTL
check, and the dirty flag.

**This turned out to be wrong, and `Save`/`Load` were deleted.** They serialize a bare graph, but a
cache entry needs `mined_at` (for the TTL) and `dirty` (for the rejection trigger) alongside it —
metadata `Save`'s format cannot carry. So `cache.go` writes its own entry shape wrapping
`Graph.ToMap`, and `Save`/`Load` remained at zero callers and zero tests after the work that was
supposed to give them one. Untested, uncalled code that looks like the supported path is worse than
its absence: the next reader would reasonably use it and get a cache the TTL cannot read.

### 5. Multi-hop: batch the edges, coalesce only at presentation

When the target is several hops away — `Discovery -> In Review` has no direct edge in the real graph,
but `Discovery -> In Progress (x80)` then `In Progress -> In Review (x28)` does — `Graph.Path`
computes the route.

The action carries the **whole path**. One approval authorizes the sequence, and the hops execute in
order, each validated against live `GetTransitions` immediately before it runs.

```json
{"target_status": "In Review", "path": ["Discovery", "In Progress", "In Review"]}
```

`actions.error` records how far it got: `"reached In Progress; In Progress -> In Review refused"`. A
partial application is a real state and must be legible as one — the alternative, leaving the reader
to infer position from the ticket, is the "usable-looking partial result" trap `Persist` was built to
avoid.

**Retry is the next reconcile pass, not a loop here.** A half-moved ticket is simply a ticket whose
current status differs from where the work says it should be — which is the drift unjira exists to
detect. The next pass sees `In Progress`, computes a shorter path, and proposes the remainder. No
retry machinery, and the failure mode degrades into the normal case.

So coalescing is presentation-only, as suspected: **one row, one approval, one apply call, N tracker
writes.** The reviewer sees "move to In Review (via In Progress)" rather than three actions to approve
separately — two of which are mechanical consequences of the third.

## Scope

- `internal/tasktracker` — `SetStatus` signature, `AvailableTransitions` replacing
  `AvailableStatusCategories`, new `Transition` type, `Issue.StatusChangedAt`
- `internal/clients/jira` — both methods; `SetStatus` matches on name rather than resolving a category
- `internal/clients/local` — same; `local_issues.status_category` becomes `status` holding a name
  (greenfield schema, no migration)
- `internal/reconciler` — `TargetStatus` type, `floorConfidence` against names, drafting prompt,
  path resolution
- `internal/gate` — `transitionPayload`, per-hop validation, partial-failure reporting
- `internal/workflow` — cache path, TTL, dirty flag
- `internal/config` — `workflow.cache_ttl`
- `rules/` — one new reconciler-scope rule on self-narration
- `cmd/unjira` — `dev workflow --refresh`; graph wiring into the reconcile path

## Testing

- **The real graph is the fixture.** `Ready for Dev -> In Progress` and `In Progress -> In Review`
  are both `indeterminate -> indeterminate`, so a test asserting they are distinguishable fails
  under the old type by construction. That is the regression guard for the whole change.
- **`unknown`-category statuses are reachable.** `Open`, `Paused`, `To Do` are currently dropped;
  assert a transition to one is proposable.
- **Multi-hop path from the real graph:** `Discovery -> In Review` resolves to the two-hop route,
  and a mid-path failure reports which hop failed and leaves the ticket at the reached status.
- **Per-hop live validation:** a fake tracker that permits hop 1 and refuses hop 2 must not attempt
  hop 3.
- **Rejection marks the cache dirty**, and the next pass re-mines. Asserted on the dirty flag, not
  on a re-mine call count, so the test does not encode when re-mining happens.
- **Evidence asymmetry:** a narrative containing only a human's status change proposes no
  transition; one containing a PR-opened event does.
- **Self-narration:** the drafted comment for a narrative whose events include a human transition
  does not open by restating it. Asserted on prompt content plus the rule reaching the prompt, since
  the model's output is not deterministic.

## Open questions

These were the questions this spec could not answer from the code. Each is answered below, in
`## Resolutions`; the questions are left as written because the record of what was uncertain is worth
more than a doc that looks prescient.

- **Does the admin workflow API tier get built now or later?** The package doc names it as tier one.
  Mining works and is what this spec uses; the admin API would make the graph authoritative rather
  than statistical, but needs permissions a normal user may not have. Deferring means the graph stays
  a proxy, which the per-hop live check already covers.
- **Should rare edges gate?** `RareEdges` exists and is uncalled. A `Done -> In Progress (x1)` move
  is legal but unusual — plausibly worth a lower confidence rather than a refusal. Unclear whether
  this is real signal or noise until transitions have run against real data for a while.
- **What happens when `Path` returns nil?** No observed route from current to target. Either the
  graph is stale (re-mine and retry) or the move is genuinely impossible (refuse and say so). These
  are indistinguishable from inside `Path`, and guessing wrong in either direction is bad: refusing a
  legal move, or looping on an impossible one.

## Resolutions

**Admin API: deferred.** The mined graph stays the planner. Its being statistical costs a refused hop
at worst, never a bad write, because every hop is validated live before it runs. Building the admin
tier would also require writing the mining fallback anyway, for users without the permission.

**Rare edges: record the frequency, do not gate.** The edge's observed count travels with the action so
a reviewer sees `seen 1x` and the data accumulates. Choosing a threshold today would be inventing
rigor — nothing yet establishes that rarity predicts wrongness, and a wrong threshold silently
suppresses legitimate moves, which is the failure mode hardest to notice.

**Nil `Path`: ask the live endpoint, then decide.** Call `AvailableTransitions`. If the target is
directly offered, the graph was merely stale — take the hop and mark the cache dirty, which is the same
self-healing trigger a rejection uses. If it is not offered, refuse and say so, naming both facts: no
observed route *and* not currently offered. This resolves the ambiguity with the authority the design
already grants rather than by guessing, and costs an API call the apply path makes regardless. Inline
re-mining is rejected: ~40s for 200 issues is far too slow inside a reconcile pass.
