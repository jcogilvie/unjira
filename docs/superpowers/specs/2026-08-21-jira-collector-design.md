# Jira collector — design

Pulls Jira changes into the event log so the correlator can see what the org believes happened,
alongside what the transcripts show actually happened. This is the input side of the reconciliation
the project is named for.

## Why this comes before the reconciler

The phase-1 spec's slice order put `internal/reconciler` next. Exploring it surfaced a blocker:
**nothing ever sets `narratives.issue_key`.** The column exists, `store.NarrativeRow` carries it,
`correlator.Cluster` reads it — and no code path writes it. Since the reconciler's verification step
is conditional on it ("if the narrative has an issue link, call `tracker.GetIssue`"), on today's code
that branch is unreachable, every narrative is unlinked forever, and every drafted action would be a
`create`.

`README.md` describes the architecture the phase-1 spec omits:

```
Claude Code    GitHub    Slack    Jira          (collectors: deterministic, pluggable)
```

> The executor's writes land in Jira, which is itself an observed stream — the loop closes and the
> next batch pass sees its own changes as part of reality.

and assigns narrative→issue matching to the **correlator**, not the reconciler. So the real
dependency order is: Jira becomes an event source → narratives get matched to issues → the
reconciler has something to verify and diff. This spec covers the first step. Matching and the
reconciler follow as their own slices.

## Scope

In scope:

- `internal/collector/jira` — a `pipeline.Collector` implementation.
- `jira.Client.GetComments` — a new read; the client can write comments but not read them.
- `config.JiraConnection.Queries` — named JQL queries, plus `max_issues_per_query`.
- Registration in `cmd/unjira/main.go`'s collector registry.

Not in scope: narrative→issue matching (next slice), the reconciler, and any write path. This slice
is read-only.

**Success criterion**, deliberately narrow: Jira changes appear in the event log, correctly shaped,
idempotently, and visible to the correlator. It does *not* prove anything about linking or actions.

## What the collector emits

Four event kinds, derived from what phase 1 can actually *do* — `README.md` scopes phase 1 to
"comments and forward transitions", so the observable changes that matter are the ones those two
actions turn on:

| `ExternalID` | `OccurredAt` | Source |
| --- | --- | --- |
| `PROJ-42:status:<changelog-id>` | when the change happened | changelog, `field == "status"` |
| `PROJ-42:comment:<comment-id>` | comment created | `GetComments` |
| `PROJ-42:description:<changelog-id>` | when edited | changelog, `field == "description"` |
| `PROJ-42:summary:<changelog-id>` | when edited | changelog, `field == "summary"` |

Built with `events.NewEvent(Name, externalID, occurredAt, summary)`, where `Name = "jira"`.
`Actor` carries the author's display name; `RawRef` the issue's browse URL.

Artifacts on every event: `issue_key`, `project_key`, `connection` (which configured connection it
came from), `field` (for changelog-derived events), and `authored_by_unjira` (see below).

### Why these four, and not the rest

`status` is the transition action's entire basis. Comments answer "has this work been narrated?",
which is the comment action's basis. `summary`/`description` are what narrative→issue matching
compares against — a ticket whose body says "investigate the flaky correlator test" is the strongest
signal that a narrative about debugging that test belongs to it.

**Skipped:** assignee, priority, labels, sprint, story points, and every other changelog field. Phase
1 never proposes changing any of them, so an event about one is something the correlator must reason
about and can never act on. A groomed backlog generates constant label/sprint/point churn; emitting
it would bury the signal. Adding a field later is additive — when an action type needs it, it gets
emitted.

`resolution` was considered and skipped: it usually moves with `status`, and
`tasktracker.Issue` has no field for it, so nothing downstream could read it.

### Full text, no truncation

Event summaries carry the complete new value. No length cap, no elision.

Measured against the configured 128k-token window at the pessimistic 2 chars/token
(`correlator.charsPerTokenEstimate`), which is 256,000 characters of prompt budget:

| Description size | Share of one prompt |
| --- | --- |
| 1,200 chars (ordinary ticket) | 0.47% |
| 5,000 chars (detailed) | 1.95% |
| 20,000 chars (RFC-style) | 7.81% |
| 32,767 chars (Jira's own field ceiling) | 12.80% |

The worst case Jira permits is an eighth of one prompt, and `Cluster` already bisects on overflow and
errors loudly on a genuinely irreducible unit — that machinery exists precisely so individual events
need not pre-emptively self-censor. Truncating the richest available statement of intent, to fit a
budget we are a fraction of a percent into, would be self-inflicted data loss against
`docs/design-notes.md`'s standing preference for erroring loudly over truncating.

Terminal *display* is a separate concern: `pipeline.RenderNarrateResult` prints event summaries
inline, and a 20,000-character description would swamp it. That is a renderer change, not a storage
one, and is deliberately left out of this slice.

### Self-authored changes: emitted and tagged, never filtered

One `Myself()` call per pass yields our own `accountId`. Any changelog entry or comment authored by
it gets `artifacts["authored_by_unjira"] = true`.

They are **not** filtered out. "unjira commented on PROJ-42" is something that happened, and the
event log is an append-only record of what happened. More practically, that event is the positive
evidence a drift was already closed — it is what stops the reconciler proposing the same comment
twice. Dropping it would both violate never-silently-drop-data and destroy the signal that makes the
loop stable.

The cost is that every downstream consumer must honour the tag; one that ignores it reintroduces the
feedback loop. That is a real risk, and it is the reason the tag exists as data rather than as an
absence.

**This is why `GetComments` is required.** Comments do not appear in the Jira changelog. Without
reading them, unjira posts a comment, the next pass sees no evidence of it, and proposes it again —
for the action type phase 1 is built around. Reading comments also lets the reconciler see that a
*human* already said it, and not duplicate them.

## Configuration

`config.JiraConnection` gains `Queries` and `MaxIssuesPerQuery`:

```json
{
  "name": "corp",
  "site": "https://corp.atlassian.net",
  "project_keys": ["PROJ", "OPS"],
  "max_issues_per_query": 200,
  "queries": [
    { "name": "mine",    "jql": "assignee = currentUser()" },
    { "name": "watched", "jql": "watcher = currentUser()" }
  ]
}
```

Named queries rather than one JQL string: a single Jira site legitimately has several views worth
collecting, and duplicating `site` plus credentials to get them is the wrong shape —
`project_keys` already establishes "one connection, several things".

### `project_keys` constrains the queries

`project_keys` exists today as a **routing table**: `JiraConnectionForProject` answers "which
connection owns PROJ" for the write path, and `appContext.projectKey` picks a default. It is not a
collection filter, and `queries[].jql` is not a routing hint — they answer different questions.

Left unrelated, though, they admit a silent failure. `assignee = currentUser()` spans an entire
site, so it can collect `ACME-7` from a project no connection covers. That work gets narrated and
matched, and then the reconciler has nowhere to send the write:
`JiraConnectionForProject("ACME")` returns false. unjira would have collected work it provably cannot
act on.

So the collector scopes every query to its connection's projects:

```
(<configured jql>) AND project IN (PROJ, OPS) AND updated >= <watermark>
```

Collection can never exceed what the connection can write to. The project list stays declared once,
with no per-query copy to drift out of sync. An operator who wants to observe another project adds
it to `project_keys` — which is the correct requirement: if you care about a project, declare it.

**Empty `project_keys` is a loud error when the Jira collector is enabled**, naming the connection.
It is not read as "collect everything", since that is exactly the unroutable case above. An empty
list stays valid for a config that never collects from Jira, so this does not retroactively break
existing setups.

## Cursors

One cursor per query, in the existing `cursors` table — no schema change, since
`PRIMARY KEY (collector, resource)` has a free-form `resource`:

```
collector: "jira"
resource:  "corp/mine"                                  -- <connection>/<query name>
position:  "<sha256(effective jql)[:12]>:<max issue.updated seen>"
```

### Why the hash

A watermark is only valid for the query that produced it. Widen `assignee = currentUser()` to
`project = PROJ` and a stored "caught up to Aug 20" makes **every PROJ issue untouched since then
permanently invisible** — the watermark answers a question that was never asked. Comparing a hash of
the JQL detects the edit: on mismatch the watermark is ignored and the query rescans from its own
horizon.

The hash covers the **effective** JQL, including the auto-added `project IN (...)` clause. Adding
`OPS` to `project_keys` widens what every query covers, and a watermark from the narrower version
would hide untouched `OPS` issues — the same bug one level up.

### The watermark selects issues, never entries

The watermark bounds *which issues are fetched*. It must **not** filter which changelog entries are
emitted.

An issue reassigned to you today has `updated = now`, so it matches — but its changelog runs back
months. Those entries are older than the watermark and **new to us**. Emitting them all is correct;
`(source, external_id)` dedup at insert makes re-emission free, which is the idempotence the
`Collector` interface already documents ("re-running one is always safe").

### Advancing

A query's watermark advances only after that query completes successfully, to the maximum
`issue.updated` observed. A partial failure leaves it untouched so the next run re-covers the gap
rather than stepping over it.

### Accepted limitation

An issue that *leaves* a query — reassigned away — stops being observed, so a narrative already
linked to it goes stale with no signal. This is stated rather than fixed: the backstop is the
reconciler's `GetIssue` verification before any action, which is exactly what
`rules/intent-not-outcome.md` requires. Recording it here so it is not later mistaken for a bug.

## `Collect` flow

```go
const Name = "jira"

func (c *Collector) Collect(s *store.Store, options map[string]any, visit func(events.Event)) error
```

Per connection, per named query:

1. Build the `jira.Client` from `UNJIRA_JIRA_CREDENTIALS` for that connection; call `Myself()` once
   per pass and cache the `accountId`.
2. `GetCursor("jira", "<conn>/<query>")`; split into hash and watermark. Mismatch or absent → ignore
   the watermark, scan from the query's horizon.
3. `SearchIssues(effectiveJQL, fields, maxIssuesPerQuery, visit)` — already paginated. `fields` is
   `["key", "project", "updated"]`: the collector needs the key to fetch changelog/comments, the
   project for the artifact and routing check, and `updated` to advance the watermark. The issue's
   current `summary`/`description` are deliberately *not* requested — those arrive as changelog
   events when they change, and requesting them here would invite emitting a snapshot, which this
   design rejected in favour of discrete events.
4. Per issue: `GetChangelog(key)` and `GetComments(key)`. Emit the four event kinds, skipping other
   changelog fields, tagging self-authored ones.
5. `SetCursor` once, after the query succeeds.

### Cost, and why the limit is required

`SearchIssues` has no `expand` parameter, so changelogs and comments are per-issue calls: **2 calls
per issue** plus the paginated search. That makes the issue set a cost decision, not just a filter.

`max_issues_per_query` is therefore required, with a default of 200 applied when the field is absent
or zero — matching the `maxIssues int` parameter `workflow.MineProject` already takes for the same
reason. A negative value is a config error rather than silently coerced. Hitting the limit is
**logged, never silent** — "query corp/mine hit its 200-issue limit; N issues not examined this
pass". A silent cap would present as a clean pass while quietly ignoring work, the failure mode
`design-notes.md` calls hardest to notice.

Because the watermark only advances on a *complete* query, hitting the limit repeatedly does not
strand progress: the next pass re-runs the same range and works through it, rather than skipping
ahead past unexamined issues.

### Failure granularity

**Per query, not per pass.** One query failing does not abort the others; its watermark simply does
not advance, so the next run retries that range. `Collect` still returns a non-nil error naming the
failed queries, so `RunCollect`'s caller sees it.

The reasoning: a 403 on one JQL (a revoked project permission) should not stop an unrelated query
from progressing, and an unadvanced watermark makes the retry automatic.

**A single issue's failure fails its query.** If `GetChangelog` returns 500 for one issue mid-search,
that query's watermark stays put rather than advancing past an issue we could not read. Skipping it
would step over data permanently.

This collector is the first to read a remote, rate-limited, permissioned source. `claudecode` reads
local files, where failure means "the file went away". Here transient 429s and partial permissions
are normal operating conditions, which is why the granularity is per-query rather than
all-or-nothing.

## Testing

Offline (what CI runs), following existing patterns:

- **Collector unit tests** — `httptest` serving canned Jira JSON, as `clients/openai` and
  `clients/jira` already do. Covers: the four event kinds with correct `ExternalID`/`OccurredAt`;
  skipped fields producing no events; `authored_by_unjira` set only on a matching author; full text
  preserved.
- **Cursor semantics** — real temp-file SQLite via `store.Open`, per the phase-1 spec's note that
  cursor logic is worth testing against the real DB. Covers: hash mismatch forcing a rescan;
  effective-JQL hashing, so a `project_keys` edit invalidates too; no advance on failure; and the
  case that matters most — **entries older than the watermark still emitted** for an issue newly
  entering a query.
- **Config** — auto-scoping produces the expected effective JQL; empty `project_keys` errors when the
  collector is enabled and not otherwise.
- **`RunCollect` integration** — collector registered in the registry, run end-to-end against
  `httptest` plus a real store, asserting dedup on a second identical pass.

Every regression test gets the break-it drill: deliberately break the guarded behaviour, confirm the
test fails, restore. This project has caught four tests this month that looked like coverage and were
not. The older-entries case is the one most likely to pass for the wrong reason — a fixture with no
old entries proves nothing.

**Live tier.** `internal/live` already exists behind the `live` build tag and `UNJIRA_LIVE=1`,
running against `unjira.atlassian.net` with `dev seed`-created issues. A live test seeds an issue,
transitions it, comments on it, then asserts the collector produces the expected events — the first
real check that these JSON-shape assumptions match Jira's actual responses, which the `httptest`
fixtures cannot establish on their own.

**Manual verification.** `unjira dev narrate` picks up Jira events automatically once they are in the
log, so a real pass shows whether Jira events cluster sensibly alongside `claude_code` ones. That
check is what found the fenced-JSON bug in the previous slice.
