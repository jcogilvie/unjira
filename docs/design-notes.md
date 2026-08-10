# Design notes

Why unjira is shaped the way it is. These are the failure modes a reconciliation agent hits in
practice — each one drove a specific architectural choice. They come from operating a hand-run
predecessor that did, by hand and on a cron, what unjira automates: reconciling "what someone
actually did" (Claude Code transcripts + GitHub + Jira) against a tracked TODO list and a
review-credit ledger. Every item below is a real incident, generalized.

The behavioral conclusions are also encoded as machine-loadable constraints under `rules/` (fed
into the correlator/reconciler prompts). This document is the human-facing "why"; `rules/` is the
enforced "what". Where a lesson maps to a rule, it's cross-referenced.

## 1. Intent ≠ outcome (→ `rules/intent-not-outcome.md`)

A transcript shows what someone was **doing or drafting**, not the final state. The predecessor
once saw a *draft* of a "request changes" review comment and recorded "review not yet posted /
owed by me" — but that review had already been submitted days earlier. The item sat
mis-classified and the credit was missing from the ledger.

**Consequence:** before emitting any state-bearing action (comment that a review is pending,
transition to In Review, file a spin-off ticket), confirm the *current* state on the system of
record — never infer it from the narrative. "I'll open a PR" is not evidence a PR exists. This is
the line between a **correlator** (reads event streams) and a **reconciler** (must diff against
live Jira/GitHub *now*). It is why the reconciler is a distinct, verification-heavy stage.

## 2. The correlator will hallucinate ref↔issue links (→ `rules/verify-correlations.md`)

A scan subagent once confidently reported a `repo#573 = PROJ-3852` binding where both halves were
wrong: the PR belonged to a different ticket, and the issue key was unrelated work. Also seen: a
"create ticket" event with a **future-dated timestamp** for a ticket that did not exist — a
planned/simulated action in a transcript, read as real.

**Consequence:** every proposed `(narrative, issue-key)` binding must be verified against the real
issue (does `getIssue(key)` resolve, and does its summary/component match?) before it drives an
action. Confidence scores don't save you — a hallucinated match can be high-confidence. Treat any
ref whose timestamp is in the future, or that doesn't resolve, as a transcript artifact. Cheap API
verification *before* the review queue keeps the queue signal-rich. This is why `correlator/refs.py`
produces *candidates* and the reconciler owns resolution.

## 3. Logical-effort clustering is load-bearing (→ `rules/env-mirror-fanout.md`, `correlator/fanout.py`)

Infra work fans out: one logical change ("switch the shared module to managed mode") becomes ~12
near-identical PRs, one per region. Reviewing all 12 is **one** review effort, not 12. Tracking
them as 12 rows is noise; a ledger that says "reviewed 12 PRs" over-counts the same way listing
them individually under-represents the single decision — and it inflates velocity.

**Consequence:** detect fan-out families and collapse them into one narrative with a PR/issue
range. The heuristic that worked: normalize titles by folding known region/environment tokens
(after `for`/`in`/`switch` connectors, as a trailing `(region)`, or a `-region` suffix), then
group by same-author + same-normalized-title + adjacent numbers. Implemented deterministically in
`correlator/fanout.py`; the region set is configurable.

## 4. Dedup keys must be fully qualified and range-aware (→ `correlator/refs.py`)

Two real bugs:
- **Bare-number ambiguity.** Once the scan went multi-org, `#382` was ambiguous
  (`repo-a#382` ≠ `org/repo-b#382`). A dedup key on the bare number silently merged distinct PRs.
  Fix: key on the full `owner/repo#N`.
- **Range expansion.** Ledgers collapse fan-out families into ranges (`#655-665`). A dedup set
  built from the literal text saw only the endpoints and re-added the interior members as "new".
  Fix: expand ranges into members *before* deduping.

The event store already dedupes on `(source, external_id)` at insert — the right layer for raw
events. But the *narrative/correlation* layer has its own dedup, and that's where these bit us.
`correlator/refs.py` handles both: fully-qualified keys and range expansion, with a `max_span`
guard that errors loudly rather than expanding an absurd range (see #9).

## 5. Query scope silently creates permanent blind spots

A review-capture query was scoped to one org. The reviewer also reviewed upstream repos. Because
the query was org-scoped **and** advanced a forward-only cursor, real reviews were never captured —
and never would be, because the cursor had moved past them. Widening the scope only fixed it going
forward; the pre-cursor gap needed a separate backfill.

**Consequence:** a collector's scope/filter is part of its cursor's meaning. If you widen scope,
the old cursor is no longer valid for the new scope — you owe a backfill over the widened scope
back to some horizon, not just forward. Prefer the widest defensible collector scope from day one;
narrowing later is safe, widening later leaves a hole. Treat any scope change as a backfill trigger.

## 6. PR state ≠ review state

The cheap PR-state call (open/merged/mergeable) does not tell you whether a review was left or what
verdict. A CHANGES_REQUESTED review was invisible to the periodic refresh — it showed only as an
`updated_at` bump the refresh read as noise. Detecting "I reviewed this" vs "waiting on the author"
required the reviews endpoint specifically.

**Consequence:** when a tracked PR's `updated_at` moves, fetch the reviews to see *what* changed.
Relevant to both credit (a review is work) and state (a review changes who's blocked).

## 7. Review staleness via head-SHA comparison (→ `rules/review-staleness.md`)

A review is "done" only until the author pushes commits past the reviewed SHA. Comparing the PR's
current head SHA to the `commit_id` of the reviewer's latest review discriminates correctly: head
advanced ⇒ re-review owed; head unchanged ⇒ still waiting on the author. Zero false positives in
practice. Store the reviewed `commit_id` with the review event and diff against head each pass.

## 8. Bot noise dilutes a credit ledger (→ `rules/bot-pr-noise.md`)

Dependency-bump approvals from `renovate[bot]`/`dependabot` dominated the raw review count (18 of
23 in one window) and buried the substantive reviews. Dropping them entirely lost legitimate
volume; itemizing them buried the signal.

**Consequence:** exclude `[bot]`-authored PRs from *itemized* narratives, keep them in a rollup
count. More broadly: a review or commit is not automatically creditable work — apply a substance
filter to what counts as trackable effort at all.

## 9. Window sizing: page by time, never silently drop

A transcript scan over a busy multi-day window blew the token budget. The safe design: subdivide
the time window, recurse if still too large, union the results — and **error loudly rather than
silently truncate** when a single window can't fit. A subtle trap: when over budget, a subagent
"helpfully" narrowed *what it extracted* (dropping a whole signal class) instead of narrowing the
*window* — losing data while appearing to succeed.

**Consequence:** the batch correlator splits by time and merges narratives across sub-batches; it
must never cope by quietly extracting less. This is also why `correlator/refs.py` raises on an
over-`max_span` range instead of truncating: silent data loss is the enemy.

## 10. Ownership is a first-class dimension

Items move between people. Someone diagnoses a problem, writes a runbook, files a ticket, and
**assigns it to a teammate** — at which point it leaves their active list and becomes "waiting on
X". Same with reviews: once you request changes, the ball is in the author's court. A tracker with
only "done / not-done" mis-represents all of this.

**Consequence:** model ownership/assignee transitions explicitly. "Work happened" and "work is now
mine to act on" are different axes; assignee + review-state + who-pushed-last together determine
which. This also decides what belongs in a given person's digest vs. merely tracked.

## 11. Exclude the tool's own workspace (→ `collectors/claude_code.py` `exclude_cwds`)

The predecessor's scanner had to skip its own workspace, or it would ingest its own bookkeeping
sessions as "work" and spiral. unjira's executor writes to Jira, which is itself an observed
stream — the loop closes intentionally — but the collectors need the same hygiene: the
`claude_code` collector skips configured self-directories (unjira's own repo), and the Jira
collector should distinguish the bot account's own writes from human activity so it doesn't
re-narrate its own comments as new work.

## 12. Verify before outward/destructive actions; surface ambiguity, don't auto-apply

Every high-confidence-but-wrong case above was caught because the workflow verified external refs
before writing and surfaced ambiguous reclassifications for human confirmation instead of
auto-applying. The review queue is unjira's version of this. What belongs there is not just
low-confidence items, but any action that (a) hinges on an unverified correlation, (b) transitions
state on inferred rather than confirmed outcome, or (c) would be hard to reverse. A confident
hallucination is more dangerous than a flagged unknown.

---

## What these validate about the architecture

- **The correlator/reconciler split is the core defense.** The pain came from conflating "extract
  what the transcript says" (deterministic, cheap) with "judge what it means and whether it's done"
  (must verify against live state). Keeping collectors dumb and putting all verification in the
  reconciler encodes that separation.
- **Deterministic pre-filters keep the LLM's queue clean.** `correlator/refs.py` and
  `correlator/fanout.py` are pure, testable functions that run *before* any model, so the review
  queue stays signal-rich and cheap API checks catch hallucinations early.
- **Batch beats real-time** for the fan-out reason (#3): whole narratives need the whole batch.
  Real-time would fragment a 12-region change into 12 unrelated events.
- **`rules/` as human-auditable markdown is the right shape.** Norms accumulate as reviewable text,
  version-controlled, and lifting team-level norms into a shared repo later is a `git remote`, not
  a redesign.
