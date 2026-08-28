# `unjira actions` primitives — design

Phase-1 slice 6, first half. The machine-facing, scriptable surface over the `actions` table:

```
unjira actions list [--json] [--status <s>]
unjira actions decide <id> --approve | --reject | --edit <text>
```

`triage` (second half) is built **on** these, per the phase-1 spec's own layering: "the
human-facing surface, built on the primitives above." Splitting the slice that way means the queue
becomes viewable — and an auto-commit failure becomes visible — before the interactive loop exists.

## Status: landed 2026-08-27 (PR #20)

`earthly +reviewable` green, 432 tests, 0 skipped. `actions list` surfaced the real 26-action
backlog for the first time — only `list` was run against real data, since `decide` mutates the queue
and (for `--approve`) Jira. The double-post guard was drilled rather than trusted: removing it fails
`TestActionsDecide_ApproveOnAlreadyAppliedActionIsRefused`.

**The open question below — where a failed write's reason lives — was closed immediately after, in
PR #21.** A new `actions.error` column, deliberately separate from `feedback`. See
`2026-08-27-failure-reason-capture-design.md`; the "Open questions" section here is kept as written
for the record rather than edited into agreement with what shipped.

Unblocked by slice 5 (`watch` + the auto-commit gate, PR #17).

## Why this is the urgent half

Slice 5 shipped a gate that can apply actions to a real tracker, and its design note says plainly:
*"a `failed` auto-commit is invisible unless someone reads the `actions` table."* Right now the only
way to see the queue is `sqlite3`. That is the gap this closes, and it closes it for the *whole*
existing backlog — 26 proposed actions today — not just future ones.

It also removes the reason `Graduated` must stay false everywhere. Once failures are visible,
graduating an action type becomes a decision rather than a leap.

## `actions list`

Reads `store.ActionsByStatus`, which already exists and already returns every column. Default to
`proposed` — the review queue — but accept `--status` so `failed` is reachable, since **surfacing
`failed` is half the point of building this now**. The phase-1 spec says failures are "surfaced by
`triage` alongside `proposed` actions"; the primitive should not make that harder than `triage` will.

`--json` for scripting, plain text otherwise. Both render the same fields: id, narrative, type, issue
key, confidence, status, and — for a `failed` action — enough to know *why* it failed. Note the
schema has no failure-reason column; `feedback` exists but is reserved for reviewer corrections. So a
failed action currently records *that* it failed and not *why*. **That is a real gap this design
surfaces rather than fixes** — see Open questions.

## `actions decide`

Three verbs, mapping onto the documented `actions.status` enum:

- `--approve` → apply the action now via `gate.Applier`, exactly as the auto-commit gate does. Not a
  second apply path: reuse `Applier` or the slice is wrong.
- `--reject` → `status=rejected`, `decided_at` stamped. No tracker call.
- `--edit <text>` → `status=edited` plus `feedback` recorded. The spec says `feedback` is "the
  reviewer's free-text correction on reject/edit, persisted regardless of whether it later becomes a
  rule," read by both the in-`triage` rework loop and `rules.Distill`. So `--edit` must persist the
  text even though nothing consumes it yet — writing it now is what makes slice 7 possible.

`store.UpdateActionStatus` already stamps `decided_at`/`executed_at` per state and already errors on
a nonexistent id. Reuse it; do not add a parallel path.

### The decision that needs care

**`--approve` writes to a tracker.** It is the second place in the codebase that can, after
`gate.Applier`'s auto-commit caller. Two consequences:

1. It must take a `tasktracker.TaskWriter`, not a `TaskTracker` — same reasoning as slice 5, and the
   compiler should enforce that this command cannot read-and-decide on its own.
2. **Approving an already-decided action must be refused, not silently re-applied.** An action at
   `applied` has already mutated Jira; re-approving would double-post a comment. The status enum is
   the guard: only `proposed` (and arguably `failed`, deliberately, as a retry) may be approved.
   `failed` → approve is the one case worth thinking about — slice 5 deliberately does *not* retry
   failed writes automatically, but a human explicitly retrying after fixing the cause is different
   from a loop hammering the API. Allow it, and say why in the doc comment.

## What this does NOT do

- **`triage`** — the interactive batch loop, `--auto-approve`/`--refresh`/`--dry-run`, and the
  in-session rework loop. Second half of the slice.
- **`unjira rules list|decide`** — needs `internal/rules.Distill` (slice 7) to have anything to list.
  Building the CLI for an empty table would be scaffolding.
- **A failure-reason column.** See below.

## Open questions, deliberately unresolved

**Where does a failed write's reason live?** `actions` has no column for it, and `feedback` is
spoken for. Options: a new `error TEXT` column; overload `feedback` with a prefix convention (ugly,
and it would corrupt `rules.Distill`'s input); or log-only, which makes the reason invisible in
exactly the surface built to make failures visible. This wants deciding before graduating any action
type, because "it failed and we don't know why" is a bad place to be with unattended writes. Left
open rather than guessed at.

**Should `list` show suppressed/no-op outcomes?** `ReconcileResult` carries `Suppressed` and
`SkippedNoDelta` explanations that never reach the `actions` table — they are rendered once and lost.
A reviewer asking "why is there nothing for narrative 7" cannot find out after the fact. Out of scope
here, but worth a later look.

## Testing

- `list` against a seeded store: correct rows, correct default status filter, `--json` parses and
  round-trips every field.
- `decide --reject` sets `rejected` + `decided_at`, and makes **no** tracker call — asserted against
  a recording fake, not just by absence of error.
- `decide --edit` persists `feedback` verbatim, including text with quotes and newlines (free prose
  routinely has both; the payload-encoding bug class from slice 4 applies here too).
- `decide --approve` applies via `Applier` and lands `applied` + `executed_at`.
- **`decide --approve` on an already-`applied` action is refused** — the test that stops a
  double-post.
- A nonexistent id errors rather than silently no-oping.
