# unjira

A reconciliation agent that keeps Jira in sync with what you actually did, so you never have to.

unjira is not an event forwarder. It is a reconciler, in the Kubernetes-controller sense: it
continuously compares two states — reality (what you actually worked on, observed from your
event streams) and Jira (what the org believes you did) — and proposes the minimal set of
patches to close the drift. A comment only exists if it fixes a gap between reality and the
record; "pushed a branch" never qualifies.

## Pipeline

```
Claude Code    GitHub    Slack    Jira          (collectors: deterministic, pluggable)
     \            |        |       /
      +-----------+--------+------+
                  |
              event log                         (SQLite, append-only, normalized)
                  |
              correlator                        (LLM: clusters events into work narratives,
                  |                              matches narratives to issues)
              reconciler                        (LLM: diffs narrative vs ticket state,
                  |                              proposes typed actions with confidence)
             review queue                       (human gate: end-of-day approval;
                  |                              corrections become learned rules)
               executor                         (deterministic: Jira Cloud REST, bot account)
```

The executor's writes land in Jira, which is itself an observed stream — the loop closes and
the next batch pass sees its own changes as part of reality.

## Status: phase 0 (observe only)

Collectors, the event log, and a daily drift digest. Zero write risk. Every digest you
mentally correct is labeled training data for phase 1, and this phase measures ticket-match
accuracy before the agent is granted any write access.

- **Phase 1 — propose and approve**: reconciler + review queue + executor for comments and
  forward transitions.
- **Phase 2 — creation and estimation**: spin-off tickets with discovery links, the estimation
  ensemble, the `emergent` tag, the ancillary-work ledger.
- **Phase 3 — productize**: autonomy graduation, Slack review mode, shared team memory,
  config-driven setup.

## Quickstart

```sh
uv venv && uv pip install -e ".[dev]"     # or: pip install -e ".[dev]"
cp config/unjira.example.json unjira.config.json
unjira collect        # ingest new events from enabled collectors
unjira digest         # print today's drift digest
unjira status         # event counts and collector cursors
```

Schedule the batch pass on macOS with the launchd template in `ops/` (see comments in the
plist for install steps).

## Layout

```
src/unjira/
  events.py            normalized Event model — the contract every collector emits
  store.py             SQLite schema and access: events, cursors, narratives, actions,
                       estimates, ledger
  config.py            config loading (unjira.config.json)
  cli.py               unjira collect | digest | status
  collectors/
    __init__.py        Collector protocol and registry — the plugin surface
    claude_code.py     Claude Code session transcripts (~/.claude/projects/**/*.jsonl)
  pipeline/
    collect.py         run enabled collectors, persist events
    digest.py          phase-0 daily digest (deterministic; LLM narrative pass is phase 1)
rules/                 learned rules as human-auditable markdown (see rules/README.md)
config/                example configuration
ops/                   launchd template for the scheduled batch pass
data/                  SQLite database lives here (gitignored)
```

## Design decisions (locked)

- **Batch, not real-time.** Hourly-ish collect passes plus an end-of-day review. Batch gives
  the correlator whole stories ("branch + 3 commits + PR" as one narrative) instead of
  fragments.
- **Bot account for Jira.** Dodges human SSO, keeps the audit history honest, and is the right
  shape for a shareable product.
- **Arbitrary workflows, no hardcoded states.** Three tiers: full workflow graph via admin API
  when available; observed graph mined from issue changelogs (with edge frequencies — rare
  edges get gated); per-issue `GET /issue/{key}/transitions` as ground truth at execution
  time. The cache plans the route; the live endpoint validates every hop, so staleness can
  never cause an illegal call.
- **Untracked-work detection is the default path, not a special case.** Any narrative that
  fails to match an open issue with sufficient confidence lands in the unlinked bucket,
  whatever stream it came from. Sentinel keys ($PROJECT-0/-1, any prefix) are one weak
  signal among several.
- **Comments pass a narrative-worthiness test.** Draft must fit a category: decision made,
  problem discovered, scope changed, blocking, or resolved-with-substance. Otherwise it
  doesn't post.
- **Estimation by ensemble.** N independent passes with different evidence framings
  (spec + similar completed tickets, observed effort, narrative); median is the estimate,
  spread is the confidence. Discovered work is tagged `emergent` so the team can plan with
  `velocity - avg_emergent_points`.
- **Corrections become rules.** Review-queue edits and rejections are distilled into markdown
  rules under `rules/`, fed forward into correlator and reconciler prompts. Approval history
  drives per-action-type autonomy graduation.

## Writing a collector

Implement the `Collector` protocol in `src/unjira/collectors/__init__.py`: read your source
since the last cursor, emit normalized `Event`s, update the cursor. Register it in
`REGISTRY` and enable it in config. `claude_code.py` is the reference implementation.
