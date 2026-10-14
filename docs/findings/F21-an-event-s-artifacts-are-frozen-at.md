# F21 — an event's artifacts are frozen at first collection, so a collector fix never reaches old rows

Events are keyed `(source, external_id)` with `INSERT OR IGNORE`, so re-collecting never updates an
existing row's artifacts. Every collector improvement therefore applies only to sessions collected
after it landed.

The **386** figure this finding originally cited was measured against a dev snapshot at the moment
F20's PR landed and no longer exists in that form. Re-measured on the one real store currently on
disk (`data/unjira.db`, 795 events): **419/419** `claude_code` events postdate F20 (0 missing
`scm_keys`) but predate F25 by about two hours (419/419 missing its "Did: …" clause) — a current,
concrete instance of the same mechanism, not a projection, and confirmation the gap recurs with every
collector improvement rather than being specific to F20.

**Consequence.** A fix's measured benefit is not the benefit an existing store receives, and the gap
is silent — nothing reports "this event was collected under older extraction rules." It also makes
before/after comparisons on a mature store misleading in the optimistic direction, since the new
behaviour only ever appears on new rows.

The tension is real and `INSERT OR IGNORE` is not simply wrong: idempotent re-collection is what makes
`collect` safe to run repeatedly, and finding #176 records why backfilling was rejected once already
(a re-collect that mutates rows can rewrite history a narrative was already built from).

**Designed, not implemented:**
`docs/superpowers/specs/2026-09-17-artifact-rederivation-design.md` resolves the shape #176 left
open — recompute one named artifact key in place, never touching identity — and the crux question of
what happens to narratives/actions already built from the changed event (answer: nothing needs to,
for an unmatched narrative or an applied action; a linked-but-not-yet-applied narrative is the one
genuinely open case, left for a human in triage). It also surfaces a new, unresolved hazard for
`claude_code` specifically: segment boundaries are not proven stable if a transcript grows after
collection, so re-deriving that collector's artifacts is not yet safe to build. Recommends deferring
implementation on the same grounds this entry already gave.

Not urgent while the store is disposable (`DEVSBX`, pre-release). It becomes load-bearing the moment a
real store exists, or a second collector improvement makes an operator ask for the backfill by name —
which is why it is recorded now rather than rediscovered then.

## Status

**open, designed**: `docs/superpowers/specs/2026-09-17-artifact-rederivation-design.md` resolves the shape (artifact-key-scoped, recompute-and-diff, no new schema) and the invalidation question, but recommends deferring — no real store currently depends on it, and `claude_code` re-derivation has an unresolved segment-boundary hazard the spec surfaces but does not close
