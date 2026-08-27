# `watch` + auto-commit gate — design

Phase-1 slice 5. Turns unjira from a thing you run into a thing that runs: one long-lived command
looping collect → narrate → match → reconcile, plus the gate that decides whether a freshly-proposed
action is applied immediately or left for `triage`.

**This is the first slice in which unjira writes to a tracker.** Every slice before it observed,
correlated, and proposed. That makes the review bar different in kind, not degree, and most of the
decisions below exist to keep the blast radius small and legible.

## Status: landed 2026-08-26

`config.AutoCommit`, `internal/gate` (pure `Decide` + `TaskWriter` `Applier`), `pipeline.RunAutoCommit`,
and the `watch` command. `earthly +reviewable` green, 416 tests, 0 skipped.

**The safety default is verified against real data, not just asserted.** `watch --once --since 48h`
against the live PAAS/DEVSBX setup with no `auto_commit` block in config:

```
auto-commit
  applied: 0  queued: 2  failed: 0
```

Jira `updated` timestamps captured before and after via a separate API path — `PAAS-4036` 12:34:15,
`PAAS-4037` 12:42:42, `DEVSBX-1` Aug 21 — all **unchanged**. 26 actions in the table, every one
`status=proposed`, `executed_at` NULL on all of them. Nothing was written.

Two guards drilled rather than trusted:

- Removing the `Graduated` veto from `Decide` fails three tests, including the nil-config case.
- Weakening the all-or-nothing precondition to `len(results) > 0` — the documented trap — fails
  `TestRunWatchPass_PartialReconcileAutoCommitsNothing`.

### Deviations worth knowing

1. **`reconciler.Persist` now returns `([]store.ActionRow, error)`.** Identifying "freshly-proposed"
   actions was the slice's sharpest correctness question, and the obvious accessor is *wrong*:
   `store.ActionsByStatus("proposed")` returns every action still queued, including ones from earlier
   passes a human has not triaged. Auto-committing those would apply decisions the operator was still
   considering. So Persist returns exactly the rows it wrote this call, in-transaction, and `watch`
   passes that slice — making "fresh this pass" structural rather than something a caller must
   remember to narrow.
2. **The all-or-nothing precondition lives in `watch`, not the gate.** `Decide`/`Apply` are pure and
   per-action with no notion of which pass produced an action, so there is no lower layer that could
   enforce it. Correctly argued by the implementer against this design's original placement.
3. **`--since` is a fixed lookback, not a cursor watermark.** The phase-1 spec envisaged a `cursors`
   entry for "since last run". A fixed window is safe here because narrative-membership dedup means
   widening it never double-narrates, but it is simpler than specified and left for later.
4. **The lease helpers were renamed** `narrateLease*` → `pipelineLease*`, since `watch` and
   `dev narrate` now share them. Rename only; `dev narrate`'s behaviour is unchanged.
5. **`config/unjira.example.json` deliberately has NO `auto_commit` block.** Every other block is
   populated, so this breaks convention on purpose: any example containing `graduated: true` risks
   being copy-pasted into a real deployment and arming the gate by accident. Omitting it means a fresh
   clone gets the safe default (nil map ⇒ queue everything). The implementer made this call
   independently and it is the right one.

### Known consequence, stated plainly

**`triage` does not exist yet**, so a `failed` auto-commit is invisible unless someone reads the
`actions` table directly. That is acceptable only while `Graduated` is false everywhere — which is the
shipped default. Anyone graduating an action type before slice 6 lands is accepting that failures will
be silent.

Also unproven: **no action has ever actually been applied.** The apply path is covered by unit tests
with a recording fake, never by a real write to a real tracker. Proving it means graduating an action
type against DEVSBX, which is a real mutation and deliberately left for an explicit decision.

Unblocked by slice 4 (`internal/reconciler`, PR #15) and by `llm.api_key_helper` (PR #16) — a loop
cannot run unattended against a gateway issuing short-lived tokens, so the credential work was a hard
prerequisite rather than a nicety.

## Scope, decided with the user

- **Both `watch` and the gate**, in one slice, matching the spec as written.
- **`Graduated` defaults to false for every action type**, so nothing auto-applies until a human edits
  config. The write path exists and is one config edit from live; that is the point of the gate, and it
  is why the default matters more than the code.
- **Write targets are config-scoped** (`jira[].project_keys` + `tracker.default_project`), with **no
  hardcoded project guard**. Considered and rejected: a temporary "DEVSBX only" check in code. It would
  have to be removed later, and a guard that must be deleted before production is a guard nobody trusts
  — worse, it would make the config path untested precisely where it matters.

## The gate

Per the phase-1 spec: for each freshly-proposed action, read `config.AutoCommit[actionType]`
(`{ConfidenceFloor float64, Graduated bool}`); apply immediately if
`Confidence >= ConfidenceFloor && Graduated`, else leave `status=proposed`.

Two properties the spec calls out, both load-bearing:

- **`Graduated` is set only by explicit human action.** Never flipped by the system, "even once approval
  history looks clean." So there is no code path that writes it — it is read-only to unjira. That is
  worth asserting in a test, because the obvious future feature ("auto-graduate after N clean
  approvals") is exactly the thing this forbids.
- **All-or-nothing per invocation.** If reconciliation fails partway, nothing from that pass
  auto-commits until a clean pass succeeds. Note this is a *different* atomicity than
  `reconciler.Persist`'s: Persist is atomic over the rows it writes; the gate must additionally refuse
  to act on a *partial set*. `reconciler.Reconcile` isolates failures per narrative and returns both
  results and an error, so the gate's precondition is "err == nil", not "results non-empty".

### Shape

`gate.Decide(action, cfg) Decision` as a pure function — `(action, config) -> commit-now | queue`,
table-driven, no I/O, per the spec's testing note. The applying half is separate and takes a
`tasktracker.TaskWriter`, so the decision logic stays trivially testable and the write authority is
visible in a signature.

This is the **first consumer of `TaskWriter`** since the interface was split in slice 4. That split was
built for exactly this moment: the reconciler holds a `TaskReader` and *cannot* write, while the
applier holds a `TaskWriter` and does nothing else. Two different types, two different
authorities — enforced by the compiler rather than by convention.

### Failure handling

A write error sets `status=failed`, never silently dropped (spec). `failed` is terminal for that
action; `triage` surfaces it later. Deliberately NOT retried inside the gate: a comment that failed
because the issue was deleted will fail identically forever, and a retry loop against a tracker is how
you rate-limit yourself out of an API quota.

## `watch`

One command, an interval loop over the four existing `pipeline.Run*` functions. It adds no pipeline
logic of its own — that is the design constraint, since every stage is already tested in isolation.

- **Holds the lease for the whole pass.** `RunNarrate`/`RunMatch`/`RunReconcile` each deliberately take
  no lease (their doc comments say so) precisely so a caller can span them under one. `watch` is that
  caller — the reason the lease seam exists.
- **`--interval`**, via `config.Span` (`go-str2duration`), matching `--since`'s existing treatment.
- **`--dry-run`** skips `Persist` and the gate both, and *says* which stages it skipped rather than
  going quiet — the same discipline `dev narrate` already follows.
- **`--once`** runs a single pass and exits, so the loop is testable and cron-friendly without a
  supervisor.
- **Graceful shutdown** on SIGINT/SIGTERM: finish the in-flight pass, release the lease, exit. Killing
  mid-pass is safe (the lease is TTL-bounded and Persist is transactional) but leaves a lock to expire,
  which delays the next run for no reason.

### What a pass failure must NOT do

Exit the loop. A transient Jira 503 or an expired credential is precisely the condition `watch` exists
to survive; a single bad pass logs loudly and the next interval retries. But an error that will recur
forever — invalid config, a missing credential helper — should fail fast at startup rather than looping
on it, so config validation happens once before the first pass.

## Open question, deliberately not resolved here

**Interval-driven vs. event-driven.** The spec says "interval-driven (cron/launchd/cloud scheduler)".
An interval is simple and predictable; the cost is latency between doing work and seeing it narrated,
plus an LLM bill on every tick whether or not anything changed. Worth noting `RunNarrate` already
short-circuits when there are no unlinked events, so an idle tick is cheap — but not free. Left as-is
because changing it is a bigger design conversation than this slice.

## Testing

- **Gate decisions: table-driven, no I/O.** Confidence exactly at the floor (inclusive per the spec's
  `>=`), below it, `Graduated` false at high confidence, unknown action type.
- **`Graduated` is never written by unjira** — a test that greps its own package, or better, the
  absence of any setter. Stated because the temptation to add one is real.
- **Partial-failure refusal**: `Reconcile` returns results *and* an error ⇒ zero writes. This is the
  test most likely to be got wrong, because the results look usable.
- **The applier takes a `TaskWriter`**, verified by a recording fake asserting exactly which calls were
  made — and a drill that calling `GetIssue` on it fails to compile.
- **`watch` with `--once`** against fakes: one pass, lease acquired and released, stages in order.
- **Dry-run writes nothing**, asserted against the store and the tracker fake both.

## What this slice does NOT do

- **`triage`** — slice 6. Until it exists there is still no way to *view* the queue except reading the
  `actions` table, and `failed` actions accumulate unseen. Worth stating plainly: shipping the gate
  before `triage` means an auto-commit failure is invisible unless someone looks.
- **Rule distillation** (`rules.Distill`) — slice 7.
- **Auto-graduation.** Forbidden by the spec, not merely unimplemented.
- **`estimate` actions** — `tasktracker` has no method for them.
