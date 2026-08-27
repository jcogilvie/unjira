# Failure-reason capture — design

A `failed` action currently records *that* it failed and not *why*. This closes that.

Status: design. Stacked on slice 6's first half (`unjira actions`, PR #20), whose own body flags
this as its known gap. Prompted by the user directly: *"i do not like unattended failures with no
reasons; we should figure out how to capture/report the failure reason."*

## The gap, traced

The reason exists at every layer and is destroyed at each boundary. This is worth writing out,
because "add a column" understates the work — three of the four losses are in Go, not SQL.

1. **`gate.Applier.Apply`** builds a genuinely useful error — `"posting comment to PAAS-1: 403
   Forbidden"` — sets `status="failed"` via `UpdateActionStatus`, and **returns the error while
   persisting none of it.** The one place holding both the row id and the reason writes only the
   former.
2. **`pipeline.RunAutoCommit`** accumulates errors with `errors.Join` but reports
   `Failed []store.ActionRow` — *rows, not errors.* Per-action attribution is gone: the joined
   blob cannot be split back apart and matched to the row that produced each part.
3. **`RenderAutoCommitResult`** prints `failed comment action 12 on PAAS-1`. No reason, because
   the struct it renders doesn't carry one.
4. **`watch`'s interval loop** logs the returned error and proceeds to the next tick. So the
   reason appears once, in a terminal nobody is watching — which is the entire point of `watch` —
   and is then unrecoverable.

Net effect: `unjira actions list --status failed`, the surface built specifically to make failures
visible, can only ever report "it failed."

## Decisions (settled with the user)

**A new `actions.error TEXT` column.** Considered and rejected: overloading `feedback`. The
phase-1 spec scopes `feedback` as "the reviewer's free-text correction," read by `triage`'s rework
loop and `rules.Distill` (slice 7). A machine-written tracker error in that column would be fed to
`Distill` as if a human had written it — corrupting rule distillation with 403s. Also rejected:
log-only, which leaves the `--status failed` surface exactly as blind as it is today.

**`data/unjira.db` gets deleted and rebuilt, and the greenfield premise holds.** This is the
decision worth recording, because the premise is now five weeks old and had never been re-examined
against real data:

- Commit `9b54490` removed all column-migration machinery as "greenfield, no DBs to migrate."
  Three later schema-touching sessions invoked that premise as a scoping boundary without
  re-deriving it.
- The DB now holds 26 real actions, which is what put it in tension. But **every one is
  `status=proposed` with `executed_at` NULL** — nothing has ever been applied, so the rows record
  no real-world mutation. Deleting costs re-spent LLM tokens, not history.
- So: add the column to the schema string, delete the DB, keep the premise. The migration story
  gets reintroduced when there is a released version whose on-disk schema we're obliged to
  preserve — `9b54490`'s own stated condition, which still hasn't been met.

Stated plainly because it will come up again: **the next schema change may not get this answer.**
Once an action has actually been applied, that row is a record of a Jira mutation and deleting it
destroys the only local evidence. The graduation decision and the migration decision are therefore
coupled, and this note is the last time "just delete it" is free.

## Shape

Four changes, one per loss above. The column alone fixes nothing a reviewer can see.

**`actions.error TEXT`**, adjacent to `feedback` with a comment distinguishing them, since the
whole risk is a future reader conflating the two.

**`store.UpdateActionStatusWithError(id, status, errText)`** — routed through the *existing*
`updateActionStatusImpl`, which already owns the `decided_at`/`executed_at` stamping rules. A
parallel path would let those rules drift, which is why `UpdateActionStatusAndFeedback` was built
this way one PR ago. The signature keeps `feedback` and `error` in separate parameters so no caller
can pass a reviewer's prose where a machine error belongs.

**Clear `error` on a subsequent success.** A `failed` action that a human later retries and
applies must not keep a stale reason attached — `actions list` would report a lie. This is the
easiest thing to get wrong, so it gets its own test.

**`RunAutoCommit` reports reasons per action.** `Failed []store.ActionRow` becomes something that
carries the error alongside the row. `errors.Join` stays for the returned error (per-item
isolation is deliberate — see its doc comment), but the *result* gains attribution so the renderer
can print a reason next to each id.

**`RenderAutoCommitResult` and `actions list` both print it.** Two surfaces, one for the live pass
and one for after the fact. The second matters more: it's the one that still works tomorrow.

## What this does NOT do

- **Retry.** Still deliberately absent, for `Applier`'s stated reason: a write that failed because
  the issue was deleted fails identically forever, and a retry loop against a tracker is how you
  spend an API quota on nothing. Recording *why* makes a human's retry informed; it does not make
  an automatic one wise.
- **A general log sink.** `watch` printing to stdout is a separate (real) gap. Persisting the
  reason is what makes it survivable without one.
- **Reasons for suppressed / no-delta narratives.** `ReconcileResult.Suppressed` and
  `SkippedNoDelta` are rendered once and lost, so "why is there nothing for narrative 7" is still
  unanswerable after the fact. Same class of bug, different table, out of scope here. Flagged in
  the actions-primitives design too; still open.

## Testing

- Apply fails ⇒ `status=failed` **and** `error` contains the tracker's message. Asserted on the
  persisted row, not on the returned error, since the return path already worked.
- Apply succeeds ⇒ `error` is empty.
- **A previously-failed action that later applies has `error` cleared.** The stale-reason test.
- `error` and `feedback` are independently writable: setting one never clobbers the other. This is
  what protects `rules.Distill`'s input, so it's asserted rather than assumed.
- `RunAutoCommit` with two failing actions attributes the right reason to each — a single joined
  error would pass a weaker test, so the assertion is per-id.
- `actions list --status failed` shows the reason in both text and `--json`.
- The decided_at/executed_at stamping is unchanged when an error is also written, proving the
  shared-impl routing actually shares.
