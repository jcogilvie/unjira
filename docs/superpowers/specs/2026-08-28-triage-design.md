# `unjira triage` — design

Phase-1 slice 6's second half: the interactive review surface over the actions queue, built on the
`unjira actions list|decide` primitives from PR #20.

## Status: landed 2026-08-28

648 assertions (502 top-level + 146 subtests), 0 skipped; `earthly +reviewable` SUCCESS, lint
`0 issues.` Verified end to end against a real store, not only by unit test:

```
reviewed 2 actions with full bodies, approved one, skipped one
== 1 approve, 0 reject, 0 edit, 1 skip ==
apply? [y/N] y  ->  applied comment on DEVSBX-9
store: action status=applied; local_issue_comments holds the body
```

Both refusal paths write nothing: declining the confirmation leaves the action `proposed` with zero
comments written, and quitting after an approve does the same.

### Corrections this slice made to its own design

Recorded rather than smoothed over, because each was found by running code and two were caught by
the user:

1. **`--auto-approve` bypasses more than the prompt.** The spec claimed graduated, writable-project,
   and confidence-floor all still applied. In fact `gate.Applier` enforces only the writable-project
   check; `Graduated` and `ConfidenceFloor` live in `gate.Decide`, which the approve path never
   calls. The testing section had asked for a test asserting all three — which would have encoded a
   safety property that does not exist.
2. **Merge needed a store seam the spec said it did not.** "No new store mutations" was false:
   `Persist`'s extend path uses `AddNarrativeEvents` (`INSERT OR IGNORE`), which adds a link and
   never removes one, so a merge left events double-linked. Probed directly.
3. **The watermark is per-narrative, so moving an event could launder it.** Relinking a frozen event
   onto a never-committed narrative made it eligible again. Fixed by direction-by-commitment, which
   makes the case structurally unreachable rather than merely forbidden.
4. **"Watch is unaffected" was wrong, and then wrong in the other direction.** First draft wired
   eligibility into `hydrateContextNarratives` claiming prior narratives normally have committed
   actions — they do not, since `Graduated` ships false. A failing compaction test led me to
   restrict `watch` entirely; the user pointed out that watch runs are discrete and floating work is
   legitimately reshufflable until something commits. The failing test was reporting a broken
   *mechanism* (`collectCompactions` counted only `n.Events` for V0 after hydration began splitting
   the slices), not a broken policy.
5. **`applied` vs `failed`.** `EligibleEventIDs` filtered on `executed_at IS NOT NULL`, which
   matches failed writes, while `hasCommittedAction` filtered on `applied`. Resolved toward
   `applied`: a failed write mutated nothing, so it has nothing to protect.

### Shipped narrower than designed

**Superseded 2026-08-29 — see "Wiring the stubbed verbs" below.** `[e]dit` and `[t]arget` are now
wired; only `[s]plit` remains unavailable. The original note read:

> `[e]dit`, `[s]plit`, and `[t]arget` return explicit "not wired yet" errors. `Redraft` exists in
> `internal/reconciler` (with tests) but `StoreHandler` holds no LLM client; split and retarget need
> the same. Merge is complete. The verbs report themselves unavailable rather than silently no-oping,
> so a reviewer cannot believe a restructure happened when it did not.

That "StoreHandler holds no LLM client" diagnosis was incomplete in a way that mattered: `Redraft`
was not merely unwired, it was **uncallable**, and the shape that did compile failed silently. Below.

## Wiring the stubbed verbs (2026-08-29)

680 assertions (534 top-level + 146 subtests), 0 skipped; `golangci-lint` `0 issues.`

### A correction to this spec: reject/edit feedback was NEVER persisted

The Testing and "Deferred to slice 7" sections both asserted this, and it was false:

> `actions.feedback` is already persisted by `[r]eject`/`[e]dit`, so slice 7 will have its input
> waiting.

It was not. `Session` recorded reject text in memory and `cmd/unjira/triage.go` only ever called
`Approved()`. So a rejected action stayed at `status=proposed`, came back in the next session, and
`actions.feedback` stayed NULL — meaning `rules.Distill` would have found **nothing** from any
triage session and silently learned nothing. Grep confirmed the sole writer of that column was
`actions decide --edit`; triage was not a caller.

Worth recording as a spec defect rather than quietly fixing, because of *how* it read as true:
`actions.feedback` did exist, `actions decide --edit` did write it, and `[r]eject` did capture
free text. Every clause was individually verifiable. Nothing connected them, and the claim that
they were connected was never checked.

Fixed by `Session.Rulings()` plus `store.RecordRuling`. Rulings persist BEFORE the apply
confirmation and independently of it: a reject writes to no tracker, so gating it on `apply?` would
mean a reviewer who rejects several actions then answers `N` loses every ruling. Verified against a
real store:

```
recorded rejected on action 1
1|rejected|this belongs on a different ticket|2026-08-29T02:16:39.040Z
proposed queue: 0
second run -> "nothing to review: no actions at status=proposed"
```

### `reconciler.Redraft` was uncallable, and the compiling call was a silent no-op

Probed rather than assumed. Naming its parameter type from another package fails to compile:

```
name verifiedLink not exported by package reconciler
```

and the one shape that DOES compile, passing `nil`, is silently useless:

```
reconciler: narrative 1 redraft named unrecognized issue_key "DEVSBX-9", ignoring
err=<nil> actions=0
```

`actionsFromVerdicts` drops every verdict whose `issue_key` is absent from the verified set, so a
nil `verified` discards the model's whole response and returns no error. A caller reaching for
`Redraft` directly gets a successful no-op: an LLM call spent, an action unchanged, a reviewer told
nothing went wrong.

Keeping `verifiedLink` unexported is still right — it pairs a store row with live tracker state read
under a specific pass, so letting a caller construct one would let it assert verification that never
happened, exactly what `rules/verify-correlations.md` forbids. The fix is `reconciler.ReworkOne`,
which **performs** the verification rather than accepting a claim of it.

That also corrects a reused assumption. `Redraft`'s doc comment justifies reusing already-verified
links because live state "was confirmed earlier in this same pass" — sound inside one `Reconcile`
pass, and false for triage, which is a separate process from the watch pass that drafted these
actions, possibly days later. There is no verification from "this same pass" to reuse.

### The redraft delta is the commit watermark, not `DeltaEvents`

`DeltaEvents` is bounded by `max(actions.created_at)`, so once the action being edited exists it
returns nothing at all:

```
delta BEFORE any action:      1 events
delta AFTER the action exists: 0 events
```

The action being edited is itself what suppresses its own source events. A redraft built on it would
prompt the model with an empty delta and produce text about nothing — no error, no warning. So
`store.EligibleEvents` bounds on the commit watermark instead, which is both correct ("describe
every piece of work no tracker mutation has claimed") and the same bound every restructure uses, so
an edit and a merge agree about which events are in play.

### A replacement must be persisted to be approvable

Probed: `approved 1 action(s); first ID=0`. `gate.Applier` calls
`UpdateActionStatusAndError(action.ID, ...)`, which matches zero rows for id 0 — so the failure
lands at APPLY time, after the reviewer already approved the new text, while the original row sits
at `proposed` forever. `store.SupersedeAction` therefore rules on the old row and inserts the
replacement in ONE transaction: two live proposals for the same work would let unjira post both.

### Retarget

Verifies the new issue before linking it (the old link was verified by the drafting pass; the new
one never was). Replaces rather than adds, since `one_primary_per_narrative` rejects a second
primary — in one transaction, because a crash between remove and add would leave the narrative with
no primary at all, reading as untracked work and re-entering matching's backlog. Records
`ProvenanceReviewer`, ranked ahead of every inferred tier, so a later pass can distinguish "someone
decided this" from "we guessed this". Rules the old row `rejected`, not `edited`: it named the wrong
issue, so it was not reworded.

### Split, still unwired

Unlike merge (moves existing links) and retarget (replaces one), split needs `Cluster` re-run with
an instruction and new narratives persisted — a correlator operation rather than a store one. It
reports itself unavailable.

### Also: heeded a linter rather than suppressing it

The first draft stored a `context.Context` on `StoreHandler`, which `containedctx` flags. It is
right to: a stored context outlives the call it was made for and cannot be cancelled
per-operation, and a redraft is a cancellable LLM round-trip. `Handler`'s methods now take one per
call, and `Session` holds the pass-scoped one.

The live-tier test (`internal/live/triage_test.go`) **compiles but was not run** — it writes to real
Jira and wants an explicit decision, the same handling PR #24's live test got.

## What this is for, and why clustering is the point

The obvious reading of "review queue" is *check the wording before it posts*. That is the wrong
emphasis, and the design follows from getting it right.

A clumsy comment on the **correct** ticket still closes the drift, and a human can fix the prose in
Jira in ten seconds. A well-written comment on the **wrong** ticket — or work split across two
narratives so each gets half a story — is unjira failing at its stated purpose. Models are good at
summarizing events they have been handed; the judgment call is deciding *which events belong
together*, so that is where the errors concentrate.

There is a sharper version. **Text errors are one-time; structure errors compound.** Narratives
persist, and the next `watch` tick extends whatever story already exists. A bad cluster is inherited
by every future pass; a bad sentence is not.

So merge/split is not the advanced feature to add later. It is the reason this command exists.

## The invariant

> **The unit of freezing is the event link, not the narrative. An event linked to a narrative before
> that narrative's last committed action is frozen. Everything else is eligible for reshuffling at
> commit time.**

Committed work cannot be altered: unjira cannot unpost a comment, un-transition a status, or
un-create an issue. But the watermark for
"the past" is the last commit, not the narrative's existence.

Choosing the event link over the narrative is load-bearing. Consider a narrative holding one applied
action and one proposed action — reachable today: `watch` applies A, the next tick proposes B for the
same narrative. Freezing the whole narrative would refuse a merge, but the reviewer's objection is
*precisely* that the events behind B do not belong with the events behind A. A narrative-level freeze
would reject the correction because the correction is right.

| Narrative state | Eligibility |
|---|---|
| never committed | fully eligible |
| fully committed | fully frozen |
| mixed | committed events stay; events linked since are free to move |

A merge in mixed state therefore *succeeds*: uncommitted events move out, committed ones stay, and
the tracker mutation remains true about exactly what it described. No refusal, no warning, no
inconsistency.

### Merge direction is determined, not chosen

The committed narrative is the merge target. That follows from what "committed" means: unjira has
already mutated the tracker on that narrative's behalf, making it the **workstream of record**, so
any story it absorbs joins it rather than the reverse.

**"Committed" covers three kinds of mutation, and a comment is the mildest of them.** This design
originally said "a posted comment" throughout, which understated the stakes:

| action type | what a commit did | reversibility |
|---|---|---|
| `comment` | added prose to an issue | additive; a stale one is confusing but deletable |
| `transition` | moved the issue to a new status | **destroyed the prior state** — Discovery → Done loses "it was in Discovery" outside the changelog |
| `create` | opened a new issue | **a new object others now reference**; deleting breaks links, keeping it orphans a ticket |

So the watermark argument is stronger than the comment framing implied, not weaker. A transition or a
create is *less* recoverable than a comment, and both make the target narrative the record of work in
a way that is visible to everyone else on the team.

| merge(X, Y) | target |
|---|---|
| both uncommitted | either — the reviewer's `m 1 3` order, or the model's judgment |
| exactly one committed | **the committed one**, always |
| both committed | **refuse** |

The common case is the first row. In the real 19-narrative backlog, **15 had proposed actions and
zero had committed ones** — uncommitted-to-uncommitted is what actually happens, because clustering
errors are visible before anything is posted.

The last row is refused rather than supported, and the reason is that it barely exists: two
*committed* workstreams means unjira has already mutated two issues on behalf of this work —
commented on both, or transitioned both, or created both. Merging
them post-hoc would leave one issue's mutation describing work now attributed to another, and unjira
cannot retract a comment, un-transition a status, or un-create an issue. That is an org-level decision about which ticket is real — not
something a review loop should decide silently. Refuse, name both narratives and their applied
actions, and let the human resolve it in Jira first.

#### Why this also closes a laundering hazard

Worth recording, because the naive implementation has a hole that nothing would surface.

The watermark is **per narrative**: `EligibleEventIDs` compares an event's `linked_at` against
*that narrative's* `max(executed_at)`. So relinking a frozen event onto a narrative that never
committed makes it eligible again — proven directly while planning:

```
BEFORE:                on A eligible=[]  (empty = frozen)
AFTER relink onto B:   on B eligible=[1]
CONFIRMED: frozen on A, eligible on B — the watermark is PER-NARRATIVE
```

A merge that moved events *away from* the committed narrative would therefore launder frozen events
into unfrozen ones, defeating the rule protecting committed work with nothing looking wrong.

Direction-by-commitment makes that **structurally unreachable** rather than merely forbidden: frozen
events live on the committed narrative, the committed narrative is always the target, so frozen
events never move at all. There is no case left in which a frozen event is relinked.

Note what is *not* destroyed by any of this. A `narrative_events` row is
`(narrative_id, event_id, linked_at)` — a join row. `events` is append-only and untouched, so the
record of what happened survives every restructure and any link is re-creatable. The hazard was
never data loss; it was the watermark being reset by the operation it was meant to constrain.

**No schema change.** The watermark is
`narrative_events.linked_at > max(actions.executed_at) for that narrative`. Both columns are written
with `strftime('%Y-%m-%dT%H:%M:%fZ')` — verified identical, and deliberately so; `DeltaEvents`
already depends on that same property, and its schema comment records why mixing `%f` with `%S`
would silently invert the comparison.

### This replaces a coarser rule rather than adding an exception

`Cluster` today prints existing narratives' events **without indices**, so the model structurally
cannot reassign them (see the hydrated-context rework spec). That is a blunt version of this
invariant: *all* existing narratives frozen.

Under the invariant it becomes: frozen **up to each narrative's commit watermark**. The prompt's two
sections are unchanged — indexed/assignable and context-only — only the partition between them moves.

Consequences worth stating:

- `watch`'s behaviour is unchanged in practice, since by the time it runs, prior narratives normally
  do have committed actions. But that is now a *consequence* of the invariant rather than a separate
  rule.
- **`triage` needs no special re-clustering mode.** No `WithReclustering` option, no
  `correlator.Recluster` entry point. Both were considered; both become unnecessary.
- The model may propose a *better* restructure than the reviewer asked for, as long as it stays
  inside the eligible region. The reviewer's `m 1 3` is a hint in the prompt, not a scope
  declaration.

## Shape

```
cmd/unjira/triage.go        thin I/O shell: reads keys, prints, holds no logic
        │  implements triage.Prompter
        ▼
internal/triage             the review session as a state machine
        │
        ├──▶ correlator.Cluster + Persist   (existing — merge/split ride on these)
        ├──▶ reconciler.Redraft             (new: draft + reviewer feedback)
        ├──▶ gate.Applier                   (existing: the only writer)
        └──▶ store.WithTx                   (existing, + one new seam)
```

`triage.Session` never touches a terminal. `cmd/` implements a small `Prompter`, so the session is
drivable from a table-driven test with a scripted prompter — which is what makes restructure
correctness testable at all. Rejected: `pipeline.RunTriage` (every existing `Run*` is single-shot
with no interactive state) and putting it in `cmd/` (the tricky mutations would only be testable by
driving the command).

**Nothing applies until the end.** Dispositions accumulate in memory; one final confirmation hands
the approved set to `gate.Applier`. This deviates from the phase-1 spec's "commit inline on approve,"
deliberately: with batch apply, every restructure stays free while the reviewer is still deciding,
and the "a merge would invalidate an action I already applied" conflict *stops existing* rather than
needing a refusal path. The spec predates merge/split being in scope.

## The four dispositions

- **`[a]pprove` / `[r]eject`** — record in memory. Reject captures free-text into `feedback` for
  slice 7's `rules.Distill`.
- **`[e]dit <text>`** — reword. `reconciler.Redraft` reuses the verified links already computed this
  pass (live state was confirmed, so the verification invariant holds without a second Jira
  round-trip) and re-runs only the drafting call with the feedback in the prompt. One LLM call,
  scoped to one action.
- **`[m]erge` / `[s]plit`** — the point of the command. Gather the eligible events of the named
  narratives, call `Cluster` with the reviewer's instruction, `Persist` the result (`ClusterExtends`
  for merge, `ClusterNew`s for split), invalidate the stale actions, re-reconcile. **No new
  correlator operations** — `Cluster` is already a re-clustering primitive and `Persist` already
  handles both kinds.

  **Correction, found by experiment while planning:** this section originally also claimed "no new
  store mutations." That is false. `Persist`'s `ClusterExtends` path calls `AddNarrativeEvents`,
  which is `INSERT OR IGNORE` — it adds the link to the target narrative but never removes the
  source one. Probed directly: after moving an event from narrative A to B, **both** report one
  event. So a merge without an explicit unlink leaves events double-linked, which would make the
  source narrative look alive, keep feeding it to future `Cluster` calls as context, and double-count
  the work. Merge therefore needs an `UnlinkNarrativeEvents` seam alongside retarget's
  `RemoveNarrativeIssue`. Both are additions to `internal/store`, and neither existed.

  The reviewer names **batch positions**, not narrative IDs: `m 1 3` means "the stories behind items
  1 and 3 are one story." Narrative IDs are an implementation detail a reviewer should not have to
  carry, and the batch display already numbers items `[n/N]`. The session resolves positions to
  narrative IDs itself. Split takes a single position (`s 4`) and no event list — asking a reviewer to
  partition events at a prompt is unreasonable, and the model is being handed the events plus the
  instruction anyway, which is the same shape as the bisection path `Cluster` already runs
  internally.

  **Re-presentation, not silent replacement.** After a restructure the affected actions are replaced
  by freshly derived ones and re-shown for disposition, since the reviewer has not seen the new text.
  Dispositions already recorded for *unaffected* actions survive untouched — that is what batch apply
  buys.
- **`[t]arget <KEY>`** — wrong ticket. The only one needing genuinely new plumbing: a store seam to
  *remove* a link, since `AddNarrativeIssues` only upserts and the partial unique index
  `one_primary_per_narrative` would reject a second `primary`. Then verify the new issue (it was
  never verified this pass) and redraft against it. `provenance` is recorded as reviewer-asserted,
  distinguishing a human's claim from a model's inference.

## Presentation

One action at a time, full body — never approve text you did not read. Real drafted bodies run 200+
characters, so a truncated batch list would invite exactly the mistake this surface exists to
prevent.

```
12 proposed actions across 8 issues.

[1/12] comment  PAAS-3939  confidence 0.40
  Follow-up investigation session (branch paas-xelasticache-autoscaling)
  triggered by a Slack report of errors when merging cse-gitops PR #3673...

  why: The delta is an investigation-only Claude Code session tied to a
       Slack-reported error on the related cse-gitops PR

  [a]pprove [r]eject [e]dit [m]erge [s]plit s[k]ip [t]arget [q]uit >
```

The eight keys are `a r e m s k t q` — all distinct, which is a constraint on the binding rather
than a suggestion. `skip` takes `k` precisely because `s` belongs to `split`.

`k` (skip) leaves an action at `proposed` for a later session — distinct from `r` (reject), which is
a ruling that gets recorded and read later by `rules.Distill`. `q` (quit) abandons the session
without applying anything.

## Flags

- **`--dry-run`** — walk the batch, show dispositions, never write. Says which stages it skipped
  rather than going quiet, matching `watch --dry-run`.
- **`--refresh`** — `store.Acquire` (blocking; already exists) so triage reviews a completed pass
  rather than data a `watch` tick is mid-way through changing.
- **`--auto-approve`** — runs the full flow and prints the batch, but approves without prompting.
  **What it does and does not bypass — corrected after reading the code, because the first draft of
  this spec overstated it.** `gate.Applier.Apply` enforces exactly one gate: the writable-project
  check. `Graduated` and `ConfidenceFloor` live in `gate.Decide`, which the human-approval path
  (`actions decide --approve`) **never calls** — deliberately, per its doc comment: the auto-commit
  gate governs *unattended* writes, and a human explicitly approving an action is not unattended.

  So `--auto-approve` is stronger than "skip the prompt." It approves everything a human would have
  been asked about, and only the writable-project scope stands between it and a write. On today's
  config that still means nothing outside DEVSBX — that gate is real — but a reviewer must not
  believe `Graduated: false` protects them here. **`--help` has to say this plainly**, and the flag
  should say so plainly in `--help`.

  **Decided: ship it.** `--auto-approve` is a flag a human passes consciously — an available choice,
  not a default or an inference. The gate exists to stop *unattended* writes; a person typing this
  flag is attending. What it must not do is mislead, hence the `--help` wording above.

## Deferred to slice 7

The phase-1 spec has `triage` also reviewing distilled rules on a learn-interval. That half needs
`rules.Distill`, `unjira rules list|decide`, and a `learn_interval` config key — **none of which
exist**. `triage` grows a second phase when they do.

~~`actions.feedback` is already persisted by `[r]eject`/`[e]dit`, so slice 7 will have its input
waiting.~~ **False when written, fixed 2026-08-29.** Triage persisted nothing, so this input did not
exist. See "A correction to this spec" above.

## Testing

- **The invariant, table-driven, no LLM:** never-committed ⇒ all eligible; fully committed ⇒ none;
  mixed ⇒ exactly the events linked after the watermark. This is the highest-value test in the
  slice; every restructure depends on it.
- **Drill it:** flip the comparison to `<` and the mixed case must fail. An inverted watermark would
  freeze the wrong half — worse than no check, since it looks like it protects committed work while
  actually exposing it.
- **`Cluster`'s prompt partitions correctly** — frozen events appear in the context-only section
  without indices, eligible ones in the indexed section. Asserted on prompt content captured from a
  fake client, since that partition *is* the safety property.
- **`watch` is unaffected** — its existing tests must pass unchanged, proving the invariant subsumed
  the old rule rather than altering routine behaviour.
- **Merge in mixed state** keeps committed events on the original narrative and moves the rest.
- **Session state machine** with a scripted prompter: dispositions accumulate, nothing applies before
  the final confirm, `q` abandons without writing.
- **`--auto-approve` respects the writable-project gate** — asserted against a recording writer.
  Deliberately NOT asserted for `Graduated`/`ConfidenceFloor`: the approve path does not consult
  them, and a test claiming otherwise would encode a false safety property. Instead, assert that a
  PAAS-targeted action is refused while a DEVSBX one applies, which is the protection that actually
  exists.


- **`[t]arget` rejects an unwritable project**, inheriting PR #24's check rather than routing around
  it.
