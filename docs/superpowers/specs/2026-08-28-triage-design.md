# `unjira triage` — design

Phase-1 slice 6's second half: the interactive review surface over the actions queue, built on the
`unjira actions list|decide` primitives from PR #20.

Status: design, approved by the user 2026-08-28.

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

Committed work cannot be altered, because a posted comment cannot be unposted. But the watermark for
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
the posted comment remains true about exactly what it described. No refusal, no warning, no
inconsistency.

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
  correlator operations and no new store mutations** — `Cluster` is already a re-clustering
  primitive and `Persist` already handles both kinds.

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

  [a]pprove [r]eject [e]dit [m]erge [s]plit [t]arget [s]kip [q]uit >
```

`[s]kip` leaves an action at `proposed` for a later session — distinct from `[r]eject`, which is a
ruling. Note `skip` and `split` collide on `s`; the implementation picks single letters that do not,
and the final key bindings are the implementer's call as long as every disposition is reachable and
none is ambiguous. `[q]uit` abandons the session without applying anything.

## Flags

- **`--dry-run`** — walk the batch, show dispositions, never write. Says which stages it skipped
  rather than going quiet, matching `watch --dry-run`.
- **`--refresh`** — `store.Acquire` (blocking; already exists) so triage reviews a completed pass
  rather than data a `watch` tick is mid-way through changing.
- **`--auto-approve`** — runs the full flow and prints the batch, but approves without prompting.
  **It bypasses the prompt, never the gates**: graduated, writable project, and confidence floor all
  still apply at commit, so on today's config it would still write nothing outside DEVSBX. Worth
  saying explicitly in `--help`, because the flag's name suggests more power than it has.

## Deferred to slice 7

The phase-1 spec has `triage` also reviewing distilled rules on a learn-interval. That half needs
`rules.Distill`, `unjira rules list|decide`, and a `learn_interval` config key — **none of which
exist**. `triage` grows a second phase when they do. `actions.feedback` is already persisted by
`[r]eject`/`[e]dit`, so slice 7 will have its input waiting.

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
- **`--auto-approve` still respects all three gates** — asserted against a recording writer, since
  the whole risk of the flag is someone assuming otherwise.
- **`[t]arget` rejects an unwritable project**, inheriting PR #24's check rather than routing around
  it.
