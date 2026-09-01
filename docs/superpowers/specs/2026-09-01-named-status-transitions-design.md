# Named status transitions — design

Replacing the three-category transition target with the tracker's own status names, and making
`workflow.Graph` load-bearing for the first time.

## Status: design

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

**unjira narrates transitions it did not make.** The comment proposed for PAAS-4038 opened:

> "Status moved from Discovery to Ready for Dev. Implementation plan finalized: ..."

The human made that move. Reporting it back to them on the ticket where they made it is circular. The
rest of that comment had real content, so this is a constraint on the lead, not a suppression of the
action: a `rules/` entry (scope `reconciler`) saying a status change unjira did not perform is context
for judging what to say, never the subject of what is said.

### 4. `workflow.Graph` becomes load-bearing, with a cache

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
  `AvailableStatusCategories`, new `Transition` type
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
