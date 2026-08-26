# `watch` + auto-commit gate — design

Phase-1 slice 5. Turns unjira from a thing you run into a thing that runs: one long-lived command
looping collect → narrate → match → reconcile, plus the gate that decides whether a freshly-proposed
action is applied immediately or left for `triage`.

**This is the first slice in which unjira writes to a tracker.** Every slice before it observed,
correlated, and proposed. That makes the review bar different in kind, not degree, and most of the
decisions below exist to keep the blast radius small and legible.

Status: design. Unblocked by slice 4 (`internal/reconciler`, PR #15) and by `llm.api_key_helper`
(PR #16) — a loop cannot run unattended against a gateway issuing short-lived tokens, so the
credential work was a hard prerequisite rather than a nicety.

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
