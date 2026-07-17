# Findings cash-out — deterministic, zero-write progress from docs/design-notes.md

Scope confirmed by user: seed `rules/`, add collector self-exclusion, and build the
deterministic clustering + dedup primitives. All zero-write / read-only. TDD throughout.

Source of requirements: `docs/design-notes.md`, the failure-mode catalog from a predecessor that
hand-ran what unjira automates. Each requirement traces to a numbered lesson there (a real
incident, not speculation).

## As Is

- Phase 0 is built and green (17 offline tests pass, 3 live deselected).
- `src/unjira/events.py`: normalized `Event`, `extract_ticket_keys`, `is_sentinel_key`.
- `src/unjira/collectors/claude_code.py`: scans `~/.claude/projects/*/*.jsonl`, one snapshot
  event per changed session file. It does **not** exclude unjira's own repo sessions.
- `src/unjira/pipeline/digest.py`: deterministic phase-0 digest (linked vs untracked).
- `rules/` contains only `README.md` documenting the frontmatter format; **no rule files seeded**.
- There is **no correlator code** — clustering/dedup primitives the findings specify do not exist.
- No `unjira.config.json`, no `data/` db, no Jira credentials configured. Jira MCP is read-only
  and for the operator's exploration only; **production code must not depend on live Jira.**

## To Be

1. `rules/` holds 5 seeded markdown rules encoding the predecessor's hard-won norms, in the
   documented frontmatter format, so the phase-1 correlator/reconciler prompt loader can consume
   them and so the norms are institutional memory now.
2. The `claude_code` collector skips sessions whose working directory is under a configured
   self-exclusion path (unjira's own repo), preventing the self-reference loop (Lesson 11).
3. A new deterministic `correlator` package provides pure, LLM-free primitives that keep the
   future correlator's review queue signal-rich:
   - **refs**: fully-qualified, range-aware PR/issue reference parsing and dedup keys (Lesson 4).
   - **fanout**: env-mirror title normalization and clustering of same-author + same-title +
     adjacent-number PRs into one narrative (Lesson 3).

## Requirements

### R1 — Seed learned rules (Lessons 1, 2, 3, 8, 7)
Seed exactly these 5 files under `rules/`, each with valid frontmatter (`scope`, `confidence`,
`learned`, `source`) and a one-idea body:
- `intent-not-outcome.md` (scope reconciler) — never emit a state action from transcript intent;
  confirm current state via the live GitHub/Jira API first. Drafting ≠ done.
- `verify-correlations.md` (scope correlator) — every narrative→issue-key binding must resolve and
  match before it drives an action; a future-dated or non-resolving ref is a transcript artifact.
- `env-mirror-fanout.md` (scope correlator) — collapse env-mirror fan-out into one narrative.
- `bot-pr-noise.md` (scope reconciler) — `[bot]`-authored dep-bump PRs are not substantive work:
  exclude from itemized narratives, keep in rollup counts.
- `review-staleness.md` (scope reconciler) — a review is stale (re-review owed) when the PR head
  SHA has advanced past the reviewer's latest review commit_id.

**Acceptance criteria:**
- AC1.1 Each of the 5 files exists under `rules/`.
- AC1.2 Each file parses as YAML-ish frontmatter delimited by `---` lines and contains the keys
  `scope`, `confidence`, `learned`, `source`; `scope` ∈ {correlator, reconciler, estimator};
  `confidence` ∈ {high, provisional}.
- AC1.3 Each file has a non-empty body after the closing `---`.
- AC1.4 A test enforces AC1.1–AC1.3 over every `rules/*.md` except `README.md`.

### R2 — Collector self-exclusion (Lesson 11)
The `claude_code` collector accepts an `exclude_cwds` option: a list of absolute path prefixes.
A session whose resolved `cwd` equals, or is nested under, any prefix produces **no event**
(but its cursor still advances, so it is not rescanned).

**Acceptance criteria:**
- AC2.1 With `exclude_cwds=[<repo>]`, a session with `cwd == <repo>` yields no event.
- AC2.2 A session with `cwd == <repo>/sub/dir` (nested) yields no event.
- AC2.3 A session with `cwd == <repo>-docs` (sibling sharing a string prefix) DOES yield an event
  (path-boundary-aware, not naive `startswith`).
- AC2.4 With no `exclude_cwds` (or empty), behavior is unchanged from today.
- AC2.5 The excluded session's cursor is still advanced (verified via `store.get_cursor`).

### R3 — Fully-qualified, range-aware refs (Lesson 4)
A `correlator.refs` module with:
- A `Ref` value type carrying a `repo` qualifier (as written: `owner/repo` or bare `repo`) and an
  integer `number`, exposing `key` → `"{repo}#{number}"`.
- `parse_pr_refs(text, *, default_repo=None, max_span=500) -> list[Ref]` that extracts refs,
  expands `#A-B` / `#A–B` (hyphen or en-dash) ranges into member Refs, attaches `default_repo`
  to bare `#N` when provided (and skips bare `#N` when not), preserving first-seen order and
  de-duplicating identical member refs.

**Acceptance criteria:**
- AC3.1 `repo-a#382` and `org/repo-b#382` yield distinct keys (no bare-number
  collision).
- AC3.2 `infra-repo#678-689` expands to 12 refs, keys `infra-repo#678` … `infra-repo#689`.
- AC3.3 En-dash range `infra-repo#655–665` expands to 11 member refs (endpoints + interior).
- AC3.4 Bare `#382` with `default_repo="o/r"` → `o/r#382`; bare `#382` with no default → skipped.
- AC3.5 A range whose span exceeds `max_span` raises `ValueError` (error loudly, per Lesson 9),
  rather than silently truncating.
- AC3.6 Order-preserving and de-duplicated: repeated identical refs and range overlaps collapse to
  one Ref each, in first-seen order.
- AC3.7 `end < start` (e.g. `#665-655`) is treated as a single ref (`#665`), not an inverted range.

### R4 — Env-mirror fan-out clustering (Lesson 3)
A `correlator.fanout` module with:
- `normalize_title(title) -> str`: strips a trailing `(region)`, folds region tokens after the
  connectors `for` / `in` / `switch` and a `-region` suffix, collapses whitespace, lowercases —
  where `region` ∈ {zrh, us2, tky, syd, mon, kor, fra, fed, dub, corp, long, prod, dev, stag}.
  Word-boundary gated so `production` (contains `prod`) is untouched.
- `cluster_fanout(items, *, max_gap=1) -> list[FanoutCluster]`: groups by
  `(repo, author, normalized_title)`, then within a group splits into runs of consecutive numbers
  (gap ≤ `max_gap`). Each `FanoutCluster` exposes `numbers` (sorted tuple), `span` (min, max), and
  `is_fanout` (len > 1). Singletons are returned as size-1 clusters.

**Acceptance criteria:**
- AC4.1 `normalize_title("Switch shared-infra to managed mode (zrh)")` ==
  `normalize_title("Switch shared-infra to managed mode (us2)")` (trailing paren region folds).
- AC4.2 `normalize_title("Enable managed mode for zrh")` ==
  `normalize_title("Enable managed mode for us2")`.
- AC4.3 `normalize_title("Ship the production release")` keeps `production` intact (no `prod` fold).
- AC4.4 12 items, same repo+author, normalized title identical, numbers 678..689 → one
  `FanoutCluster` with `is_fanout` true and `span == (678, 689)`.
- AC4.5 Same title/author but numbers 678, 680 (gap 2, default `max_gap=1`) → two clusters.
- AC4.6 Different `repo` with adjacent numbers and identical title/author → two clusters (repo is
  part of the group key).
- AC4.7 A lone item → one size-1 cluster with `is_fanout` false.

## Testing Plan (TDD — tests precede implementation)

- `tests/test_rules.py` — enforces R1 (AC1.1–AC1.4). Inline frontmatter parse; no production
  loader yet (phase 1 builds the prompt loader).
- `tests/test_claude_code_collector.py` — R2. Build a tiny transcript JSONL in a `tmp_path`, run
  the collector against an in-memory/temp `Store`, assert event presence/absence and cursor
  advance across the AC2 cases. (No existing collector test — this also backfills coverage.)
- `tests/test_refs.py` — R3, one test per AC3.x.
- `tests/test_fanout.py` — R4, one test per AC4.x.

All tests are offline (no Jira, no network), consistent with the CI tier. Each new test is
written and seen to fail (or its target import to be missing) before the implementation lands.

## Implementation Plan (smallest sequential steps)

1. **R1 rules** — write `tests/test_rules.py` (fails: files absent). Seed the 5 `rules/*.md`.
   Green. Smallest, no production code risk.
2. **R3 refs** — write `tests/test_refs.py` (fails: module absent). Implement
   `src/unjira/correlator/__init__.py` + `refs.py` (`Ref`, `parse_pr_refs`). Green per AC3.x.
3. **R4 fanout** — write `tests/test_fanout.py` (fails). Implement `fanout.py`
   (`normalize_title`, `FanoutCluster`, `cluster_fanout`). Green per AC4.x.
4. **R2 self-exclusion** — write `tests/test_claude_code_collector.py` (fails on excluded-case
   assertions). Thread `exclude_cwds` through `ClaudeCodeCollector.collect` → `_session_event`
   with boundary-aware matching. Add `exclude_cwds` to the config example. Green per AC2.x.
5. **Full suite** — `pytest` all offline tiers green; then `superpowers:code-reviewer` pass.

Each step: run only the new test file first (fast feedback), then the whole offline suite before
moving on.
