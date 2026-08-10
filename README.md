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
go build -o unjira ./cmd/unjira      # or: earthly +build
cp config/unjira.example.json unjira.config.json
cp .env.example .env                 # Jira credentials (gitignored; not needed in phase 0)
./unjira collect        # ingest new events from enabled collectors
./unjira digest         # print today's drift digest
./unjira status         # event counts and collector cursors
```

Schedule the batch pass on macOS with the launchd template in `ops/` (see comments in the
plist for install steps).

Dev-instance tools (need credentials in `.env`): `unjira dev seed` creates labeled test
issues and walks them through transitions to generate changelog history; `unjira dev reset`
deletes exactly what seed created; `unjira dev workflow` prints the mined workflow graph.

Testing: `go test ./...` runs the offline tiers and is what CI runs per-push (the `live`
build tag excludes `internal/live` entirely — it won't even compile without it).
`UNJIRA_LIVE=1 go test -tags=live ./internal/live/...` runs the live suite, which writes to
the dev Jira instance and cleans up after itself; in CI that's the `integration` job, gated
behind the `live-jira` environment.

## Layout

```
cmd/unjira/             CLI entrypoint (Kong): collect | digest | status | dev
internal/
  events/               normalized Event model — the contract every collector emits
  store/                SQLite schema and access: events, cursors, narratives, actions,
                        estimates, ledger
  config/               config loading (unjira.config.json)
  clients/
    jira/               Jira facade over go-jira/v2/cloud (reads + gated writes)
  collector/
    claudecode/         Claude Code session transcripts (~/.claude/projects/**/*.jsonl)
  correlator/
    refs/               fully-qualified, range-aware PR/issue reference extraction
    fanout/             env-mirror fan-out clustering
  workflow/             observed workflow graphs mined from changelogs; BFS path planning
  pipeline/             run enabled collectors, persist events; render the phase-0 digest
  devtools/             seed/reset labeled test data on the dev instance
  live/                 live-Jira integration tests (build tag "live")
rules/                  learned rules as human-auditable markdown (see rules/README.md)
config/                 example configuration
ops/                    launchd template for the scheduled batch pass
data/                   SQLite database lives here (gitignored)
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
  whatever stream it came from. Some workflows substitute a placeholder ticket key to satisfy a
  commit-message linter when no real ticket applies; `exclude_from_linking` (a list of regex
  patterns in config, empty by default) tells unjira which ticket-shaped matches are placeholders
  rather than real links, without discarding the fact that one was seen — an event whose only
  candidate keys are all excluded still shows up as untracked, annotated with which key was
  excluded, so it stays visible for later triage instead of vanishing.
- **Comments pass a narrative-worthiness test.** Draft must fit a category: decision made,
  problem discovered, scope changed, blocking, or resolved-with-substance. Otherwise it
  doesn't post.
- **Estimation by ensemble.** N independent passes with different evidence framings
  (spec + similar completed tickets, observed effort, narrative); median is the estimate,
  spread is the confidence. Discovered work is tagged `emergent` so the team can plan with
  `velocity - avg_emergent_points`.
- **Buy over build, behind our seam.** Every remote-system client lives under
  `internal/clients/<system>` as a thin facade over its upstream SDK — `clients/jira` over
  go-jira/v2/cloud today, `clients/litellm`/`clients/github`/`clients/slack` as later
  integrations land — so the community absorbs that API's churn and divergence stays
  trivial to reconcile. Hand-rolled only where no wheel exists (the Claude transcript
  parser).
- **Corrections become rules.** Review-queue edits and rejections are distilled into markdown
  rules under `rules/`, fed forward into correlator and reconciler prompts. Approval history
  drives per-action-type autonomy graduation.

## Writing a collector

Implement the `Collector` interface in `internal/pipeline/collect.go` (`Name() string`,
`Collect(s *store.Store, options map[string]any, visit func(events.Event) error) error`):
read your source since the last cursor, emit normalized `Event`s, update the cursor. Register
it in `cmd/unjira/main.go`'s `registry` map and enable it in config.
`internal/collector/claudecode` is the reference implementation.
