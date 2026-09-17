# Artifact re-derivation — design

**Status: design**

Closes nothing yet. Resolves finding **F21** (`docs/architecture-findings.md`) by naming the shape a
fix would take, and recommends **not building it now**. This is a "record the shape, defer the work"
spec, in the same register as F1, F7, and F8's own entries in that file — a deliberate, argued
deferral, not an unfinished design.

## The problem, restated precisely

`internal/store`'s `events` table is keyed `UNIQUE (source, external_id)` and every insert is
`INSERT OR IGNORE` (`internal/store/events.go:27`). That is correct and load-bearing: it is what
makes `collect` safe to run on a cron without duplicating rows, and every collector's "re-running one
is always safe" contract depends on it.

The consequence F21 names: when a collector *improves* — learns to extract a new artifact key, or
extracts an existing one more accurately — that improvement reaches only events collected **after**
the fix ships. Every row already in the store keeps whatever artifacts its original collection pass
wrote, forever, because nothing ever runs an `UPDATE` against `events.artifacts`.

Two real instances exist in this tree's own history, both explicitly accepting the gap rather than
fixing it:

- F20 (`ArtifactSCMKeys`, commit `b21deb2`): "No backfill... the 386 existing `claude_code` events
  will never gain `scm_keys`."
- F25 (session facts, commit `dc8fa86`): ships a fixed-size "Did: …" clause appended to
  `segmentSummary`, with no mention of backfill at all — silently the same gap, just not named a
  second time.

## What #176 already decided, and why this is not a reversal

Finding #176 (referenced by F21's own text, and by F6's cross-reference table) rejected backfilling
once already, on the grounds that **a re-collect that mutates rows can rewrite history a narrative
was already built from.** That reasoning is sound and this spec does not challenge it. What #176
rejected was *re-collection* — re-running a collector's `Collect` end-to-end against old source
material, which can change an event's `occurred_at`, its segmentation (see §4), or even its identity,
none of which unjira may do to a row a narrative has already consumed.

What F21 asks for, and what this spec designs, is narrower: **recompute one named artifact key,
in place, on the existing row, touching nothing else.** `source`, `external_id`, `occurred_at`,
`summary`, `actor`, and `raw_ref` are never written by re-derivation — only a named key inside the
`artifacts` JSON blob. F21's own text already anticipates this distinction ("a re-derive command
that recomputes artifacts from `raw_ref` without touching identity"); this spec is that command's
design, and confirms the distinction holds up under scrutiny (§4 finds one real complication in it,
not a refutation).

## Measured: how much this bites today

The number cited in F21's prose (**386** `claude_code` events lacking `scm_keys`) is not a durable
metric — it was measured against whatever dev store existed at the moment F20's PR landed
(`2026-09-16 14:15:55 -0400`), and that store no longer exists in that form. I confirmed this by
finding two temp-directory snapshots (`f16-drained.db`, `f16-probe.db`, both `mtime` before F20
shipped) that carry exactly 386 `claude_code` rows missing `scm_keys` — the snapshot F20's commit
measured against, now nine collect/nuke cycles in the past.

The one real store currently on disk in this environment (`data/unjira.db`, gitignored,
`DEVSBX`/pre-release per the README's own status line) gives a fresh, current instance of the same
mechanism, measured directly with read-only `sqlite3` (no writes made):

```
events total:            795  (419 claude_code, 376 jira)
claude_code missing scm_keys (F20):     0 / 419
claude_code missing "Did: …" (F25):   419 / 419   ← 100%
```

Every `claude_code` event in the current store predates F25 (ingested `2026-09-16T18:28:41Z` through
`20:12:27Z`; F25 landed at `2026-09-16T21:59:12-04:00` = `21:59:12Z`, roughly two hours later) and
postdates F20 (which landed at `18:15:55Z`, before the earliest `ingested_at` here). So: **a
collector improvement that shipped hours ago already has zero effect on the only real store in this
repository, and will continue to have none** unless the underlying transcript files grow enough to
produce new segments (which only appends new rows) or something re-derives the old ones.

This is a real, present-tense demonstration of exactly what F21 describes — not a projection. It is
also, honestly, still a **disposable dev store**: the presence of thirteen other timestamped
snapshot copies in this session's temp directory (`before-nuke`, `nuked`, `baseline-preclear`, …)
confirms this exact database is being rebuilt and discarded repeatedly during ordinary development.
I could not find, and do not believe there currently exists, a store for which this backfill gap has
real operational consequences (a review queue someone is actually working, or a narrative history
someone is relying on). That absence is the load-bearing fact behind this spec's recommendation.

## Design

Presented so the shape exists when it is needed, per F21's own "not urgent... recorded now rather
than rediscovered then." Every open question CLAUDE.md and the task instructions asked to be
resolved is resolved below; §6 says which parts are genuinely unresolved rather than papering over
them.

### 1. Unit of re-derivation: the artifact key, not the event or the collector

Three units were considered, matching the task's own framing:

- **The collector** (re-run `Collect` over all historical source material): this is re-collection,
  not re-derivation, and reopens exactly what #176 closed — segmentation, identity, `occurred_at`.
  Rejected outright; it is a different, larger, already-decided-against feature.
- **The event** (recompute every artifact on a row): couples unrelated concerns. A fix to
  `scm_keys` extraction has no reason to also touch `git_branch` on the same row, and re-touching an
  artifact whose extraction logic never changed adds risk (see §4's edited-source hazard) for zero
  benefit.
- **The artifact key**, scoped to a named collector: recomputes exactly the thing that changed. This
  is what F21's own text names ("recomputes artifacts... without touching identity") and it keeps
  blast radius equal to the fix that motivated running it, not larger.

**Recommendation: the artifact key**, invoked per collector (`--collector claude_code --artifact
scm_keys`), operating over an operator-named event population (§2).

### 2. Identifying staleness: recompute-and-diff, not a version stamp

Four approaches were weighed:

| approach | verdict |
|---|---|
| Collector version stamp (a new `_extractor_version` artifact, bumped per change) | Rejected. Requires every future collector change to remember to bump it — the exact "discipline nothing enforces" failure this codebase has hit twice (design-notes incident 21, "a doc comment cannot create a contract"; incident 28, "deleting beats disciplining"). It also answers a different question than the one that matters: "collected before version N" does not tell you whether *this specific* artifact key changed at version N, only that *something* did. |
| Content hash of extraction logic | Rejected. Not meaningful for compiled Go source, and every whitespace-only diff would look like a semantic change. |
| Explicit operator-named population | **Recommended.** No schema commitment at all — it is a `WHERE` clause the operator writes at invocation time (`--collector X --artifact Y`, optionally narrowed by an id range or `ingested_at` bound). The trigger for naming one is exactly what F20 and F25's own commit messages already did unprompted: "N existing rows predate this." |
| Full re-derive of everything, unconditionally | Subsumed by the recommendation below — see next paragraph. |

The staleness *test itself* does not need a stored marker at all: **recompute the artifact for every
event in the named population, and diff against what is already stored.** A row whose stored value
already matches the recomputed one was never stale, regardless of when it was collected — this
is correct by construction, not by bookkeeping, which is why no new schema is needed (§5). It also
means running the command twice is safe: the second run reports "0 changed" rather than
re-writing identical bytes.

### 3. Opt-in, propose-then-commit — same family as `unjira learn`

Two existing precedents already establish this shape for exactly this reason:

- `gate.Applier`: nothing writes to the tracker without a human or a graduated auto-commit rule
  approving it first.
- `unjira learn` (`cmd/unjira/learn.go`): "a rule shapes every future prompt on every narrative, a
  wider blast radius than any single tracker write, so the same propose-then-commit separation
  `gate.Applier` enforces applies here."

Artifact re-derivation's blast radius is at least as wide as `learn`'s: it can change which
candidates every future *and past* matching pass sees for an event (§4). So:

```
unjira dev rederive --collector claude_code --artifact scm_keys [--apply]
```

Dry-run (no `--apply`) is the default and only prints a report:

```
== rederive: claude_code / scm_keys ==
population: 419 events (source=claude_code, artifacts.scm_keys IS NOT NULL OR IS NULL — no filter named)
  unchanged:  340
  changed:     71   (sample: event 88 "" -> ["PAAS-3969","PAAS-4019"])
  skipped:      8   transcript file not found at <path> (5)
                     transcript file present but shorter than recorded size (3)
```

`--apply` performs the writes (§5) and nothing else — no side effect on any tracker, no narrative
re-clustering, no action re-drafting. What happens to narratives/actions already built from a changed
event is answered in §4, and deliberately requires no code in this command at all.

### 4. What happens to narratives and actions already built from the event — the crux

Rewriting `artifacts.scm_keys` on event E changes **only** that JSON value. It does not touch E's
row identity, so `narrative_events` links are untouched — no relink, no re-cluster, no orphaning.

The real question is which *decisions already made by reading the old value* become stale, and what
to do about each:

**Clustering (which narrative E belongs to).** Unaffected by any artifact this spec's use cases
target. `buildClusterPrompt` (`internal/correlator/correlator.go:393`) renders only `Source`,
`Summary`, and `OccurredAt` — never artifacts. A changed `scm_keys` or a new fact clause cannot move
an event between narratives, because clustering never looks at either.

**Matching (which issue a narrative links to) — this is where it matters, and it splits into two
cases by exactly the boundary `EligibleEventIDs` already draws:**

- **Narrative has no primary link yet** (`store.NarrativesWithoutPrimaryLink`'s backlog). This is
  free, and needs no new mechanism at all: `matchOne` calls `gatherCandidates` fresh, over live
  event rows, on every pass (`internal/correlator/match.go:300`, `AllNarrativeEvents`, not a cached
  copy). Re-derive the artifact, and the very next ordinary `Match` pass sees the improved
  candidates — exactly the ranking win F20's own commit message describes ("18 undifferentiated
  candidates collapse to 2"). No invalidation, no watermark, no flag: the backlog already re-reads
  current data every time it runs.
- **Narrative already has a primary link** (right or wrong), because `persistLinks` writes the link
  row regardless of confidence (`internal/correlator/match.go:539`'s doc comment — "the floor
  governs what unjira asserts, not what it records"). `NarrativesWithoutPrimaryLink`'s whole
  selection predicate is "no primary role row exists" (`internal/store/narrativeissues.go:82`), so
  once *any* primary is recorded the narrative permanently leaves that backlog. Backfilling the
  artifact does **not** make Match reconsider it — nothing does, today.

  This narrative-already-linked case further splits on whether an action was **applied**:
  - **An action already applied** (a comment posted, a transition made): the link must stay exactly
    as decided. `internal/store/eligibility.go`'s doc comment and design-notes incident 13
    ("committed work must not be reattributed... the unsafe case stops being forbidden and becomes
    unreachable") already establish this boundary for a different operation (re-clustering); artifact
    re-derivation must respect the identical line rather than inventing a second way to reopen a
    commitment that invariant exists to keep closed. **This spec proposes no mechanism to revisit an
    applied narrative's link, deliberately.**
  - **No action applied yet** (proposed, or not yet drafted): the link may be *wrong*, now
    demonstrably so given the better artifact — but nothing in unjira's existing pipeline re-opens
    it automatically, and this spec does not add anything that does either. The honest state is: a
    human reviewing that narrative in `triage` before it is ever approved is the existing safety net
    (verifyLinks/reconcile/triage all run before `gate.Applier` can act), so a wrong-but-not-yet-
    applied link is not silently dangerous — it is merely not automatically corrected by
    re-derivation. **Left as an explicitly open question (§6)**, not resolved here: a future
    `unjira dev rematch --narrative N` (or a triage verb) that forces one narrative back through
    `Match` is the natural shape, should this ever matter enough to build.

  If that future extension is ever built, it needs a per-narrative watermark of the same shape as
  `match_examinations` — `examined_at` in `strftime('%Y-%m-%dT%H:%M:%fZ', 'now')` (milliseconds),
  compared against `narrative_events.linked_at`, which already uses that exact format
  (`internal/store/store.go:71-78`). **This spec's own command needs no such comparison** — see next
  paragraph — but any extension that does must not reintroduce the `%f`-vs-`%S` inversion this
  codebase has paid for twice (design-notes incidents 21/33: `'.'` (0x2E) sorts before `'Z'` (0x5A),
  so a whole-second timestamp compares as *earlier* than a millisecond one that is actually later).

**Reconciler delta (`DeltaEvents`, `EligibleEvents`).** Both compare `narrative_events.linked_at`
against `actions.created_at`/`executed_at`, neither of which this command touches. A re-derived
artifact does not change which events are "new since the last action" — it changes what a *future*
read of an unchanged set of events sees. No comparison in this command needs a timestamp at all: the
diff in §2 is a value comparison (old artifact vs. recomputed artifact), not a time comparison.

**Rules distillation (`rules.Distill`, slice 7).** Reads `actions.feedback`, never `events.artifacts`
directly. Unaffected.

### 5. Mechanics — no new schema

Because staleness detection is "recompute and diff" (§2) rather than "read a stored marker," this
design needs **no new table and no new column** — the one constraint every prior spec in this file
has had to design around (`internal/store/store.go`'s own comment: "every statement is `CREATE TABLE
IF NOT EXISTS`... a new COLUMN would need an ALTER nothing runs"). `--apply` needs exactly one new
store method:

```go
// UpdateEventArtifacts overwrites the artifacts JSON for one event, identified by its
// row id. Unlike InsertEvent, this is a plain UPDATE — it exists precisely to touch the
// one column InsertEvent's INSERT OR IGNORE can never revisit, and it must never be given
// a way to also change source/external_id/occurred_at/summary/raw_ref: doing so would
// blur this into the re-collection #176 rejected.
func (s *Store) UpdateEventArtifacts(id int64, artifacts map[string]any) error
```

Each collector owns its own re-derivation function, mirroring the existing rule that only a
producer can declare a fact about its own output (`events.SetTrackerRecord`'s doc comment: "the
producer is the only party that can know"):

```go
// claudecode package
func RederiveSCMKeys(evt events.Event) (keys []string, ok bool, err error)
```

`ok=false` is the "source unavailable, skip and say so" path (§6), never a silent zero-length
result — a session with genuinely zero SCM keys and a session whose transcript file is gone must be
distinguishable in the report, per the never-silently-drop-data invariant this spec is bound by as
much as any collector is.

### 6. Source availability — resolved partially, one gap left explicitly open

**Jira-sourced events.** `raw_ref` is a browse URL, not a stored copy of the API response
(`internal/collector/jira/events.go:139`) — unjira never persists the raw changelog/comment JSON.
Re-derivation for a Jira artifact therefore means re-fetching by issue key
(`events.ArtifactIssueKey`) via the connection's `GetChangelog`/`GetComments`, keyed by the
changelog entry id or comment id already embedded in `external_id`. Two failure shapes, both
reported by name rather than folded into "unchanged": the issue no longer exists (deleted,
permissions revoked), or the connection named in `ArtifactConnection` is no longer configured. Jira
changelog entries are themselves effectively immutable once written (an edit produces a *new*
entry), so re-fetching the *same* entry id should reproduce the same `fromString`/`toString` —
**except for a comment's body**, which Jira does let a user edit in place. Re-deriving an artifact
from a re-fetched comment risks silently picking up a post-collection edit and presenting it as
what the artifact "always should have said" — which is a real, narrower version of the identity
hazard #176 named. **This spec's recommendation: re-derivation must prefer the event's own stored
`summary` (frozen at collection time) as its input wherever the target artifact's information is
already present there, and must only re-fetch from the live API when the stored row genuinely
lacks the needed material** — flagging any row it had to re-fetch for, in its report, as
"recomputed from a live re-fetch, which may reflect a post-collection edit" rather than presenting
it with the same confidence as a stored-data recomputation.

**Claude Code-sourced events.** `raw_ref` is the transcript file path. F20 and F25's own extraction
(`scmKeys`, `sessionFacts`) reads raw JSONL tool-call blocks (`facts.go:100`'s `toolCommands`) that
are **not** captured anywhere in the stored `summary` or any existing artifact — re-derivation here
is unconditionally a live re-read of the file, with no "recompute from stored data" option at all.
Two known-file-gone cases are reportable exactly like the Jira case (moved, rotated, deleted).

**The gap this investigation surfaced that F21's own text does not mention, and that blocks safe
implementation for `claude_code` specifically:** `sessionEvents` slices a transcript into segments
by branch change with a message floor (`internal/collector/claudecode/segments.go`), and the event's
`ExternalID` records only `<session>:<file-size-at-collection>:<segment-index>` — never the line
range that produced that segment. If the transcript file has grown since collection (a live session
resumed, or simply re-read later), re-running `segments()` over the *current* file and trusting that
segment index *i* still denotes the same lines is not verified safe by anything in this codebase
today. `foldShortRuns`/`dropBelowFloor` fold a too-short run into its **neighbour**, and which run is
whose neighbour can in principle depend on what comes after it in the transcript — meaning content
appended *after* the original collection point is not obviously incapable of changing how an
*earlier* segment's boundary was drawn.

I did not resolve this gap; I am naming it because implementing this spec for `claude_code` without
resolving it first would risk exactly the kind of silent, hard-to-notice corruption CLAUDE.md warns
against — a re-derived artifact attributed to segment 0 that actually reflects a different set of
lines than the ones that originally produced event 0's `summary`. Two directions, neither designed
here: (a) verify segment-boundary stability empirically (re-run `segments()` at several later file
sizes against transcripts already collected, and measure whether early boundaries ever move), or
(b) have the collector persist enough position information (a line range, or a hash of the covered
lines) to make re-derivation provably safe rather than merely usually safe. Until one of those
lands, **Jira artifacts are the safer first candidate for this mechanism, should it ever be built** —
changelog/comment entries are addressed by immutable ids, with no analogous segmentation hazard.

## Recommendation

**Defer.** Write this design now, as F21 itself asks for, and do not implement it in this PR or
soon after, for three reasons that together outweigh the case for building it:

1. **No real store currently depends on it.** The only concrete number available (§3) comes from a
   store that is, by its own README's status line, disposable dev data undergoing repeated
   nuke/rebuild cycles during ordinary development. F21's own text already names the trigger
   condition — "load-bearing the moment a real store exists" — and nothing found during this
   investigation shows that condition has occurred.
2. **#176's caution generalizes further than its own text states.** This spec confirms the
   identity-preserving version of backfill #176 asked for is safe *for Jira*, but surfaces a
   genuinely unresolved segmentation-reproducibility question for `claude_code` (§6) that a
   full implementation would have to answer first. Shipping the command before that is resolved
   would risk a new, subtler instance of exactly the harm #176 was written to prevent.
3. **The value most likely to matter — unmatched narratives seeing better candidates (§4) — requires
   no new invalidation machinery**, which means the actual implementation cost, when it is
   eventually justified, is small: one store method, one per-collector re-derive function, one
   `dev` subcommand with a dry-run report. Nothing about deferring this loses that simplicity; the
   design does not decay by waiting.

This mirrors F1, F7, and F8's own entries in `docs/architecture-findings.md`: each records a
considered position and a condition that would change the answer, rather than either implementing
prematurely or leaving the question unexamined. The condition here is explicit: **revisit when a
non-disposable store exists, or when a second collector improvement (after F20 and F25) makes the
backfill gap costly enough that an operator asks for it by name** — at which point this spec's §§1-5
are ready to implement directly, and §6's `claude_code` gap is the one piece of remaining design
work.

## Open questions

- **The `claude_code` segment-boundary reproducibility gap (§6).** Genuinely unresolved. Needs
  either an empirical stability study or a collector change (persisting a line range or content
  hash per segment) before re-derivation can be trusted for this collector's artifacts.
- **Should a wrong-but-not-yet-applied primary link ever be automatically reopened** by a backfill,
  rather than left for a human to notice in triage? This spec says no (§4) on the grounds that
  nothing forces the codebase to reopen a decision automatically today, and triage already reviews
  before anything is applied — but a future reviewer with different risk tolerance could reasonably
  design a triage-surfaced flag using the `match_examinations`-shaped watermark named in §4, if the
  "narrative already linked, not yet applied" case turns out to matter more in practice than
  measured here.
- **Should re-derivation ever be triggered automatically by a collector's own version bump**, rather
  than always operator-invoked? This spec rejects a version stamp as the *staleness detector* (§2)
  but does not fully foreclose using one purely as an *automatic trigger* for a human to review later
  (distinct roles: "which rows are stale" vs. "should someone be told a backfill is now available").
  Left unexplored because nothing in the current evidence justifies the added bookkeeping yet.
