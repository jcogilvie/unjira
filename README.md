# unjira

A reconciliation agent that keeps Jira in sync with what you actually did, so you never have to.

unjira is not an event forwarder. It is a reconciler, in the Kubernetes-controller sense: it
continuously compares two states — reality (what you actually worked on, observed from your
event streams) and Jira (what the org believes you did) — and proposes the minimal set of
patches to close the drift. A comment only exists if it fixes a gap between reality and the
record; "pushed a branch" never qualifies.

## Pipeline

```
Claude Code    Jira    (GitHub)   (Slack)       (collectors: deterministic, pluggable;
     \           |        |          /            parenthesized = not built yet)
      +----------+--------+---------+
                 |
             event log                          (SQLite, append-only, normalized)
                 |
             correlator                         (LLM: Cluster = events -> narratives,
                 |                               Match = narratives -> issues)
             reconciler                         (LLM: diffs narrative vs LIVE ticket state,
                 |                               proposes typed actions with confidence.
                 |                               Holds a TaskReader — cannot write)
            review queue                        (the actions table: unjira actions list|decide;
                 |                               corrections become learned rules)
           auto-commit gate                     (three deny-by-default checks; the only code
                 |                               holding a TaskWriter)
                 v
               Jira
```

Writes land in Jira, which is itself an observed stream — the loop closes and the next pass sees
its own changes as part of reality. There is no separate "executor" component: applying is
`gate.Applier`, deliberately the single narrow choke point every write route converges on.

## Status: phase 1, mostly landed — unjira can write, and does not by default

The whole loop runs: collectors → event log → correlator (cluster + match) → reconciler →
review queue → apply. `unjira watch` executes it on an interval; `unjira actions` is the
machine-facing surface over the queue.

**unjira has written to a real Jira instance** (`internal/live/autocommit_test.go`, 2026-08-27).
That makes the next paragraph the important one.

### Three independent gates, all deny-by-default

A write happens only if **all three** allow it. Any one of them refuses and the action stays
queued or lands `failed` with a persisted reason:

1. **`auto_commit.<type>.graduated`** — false for every action type, and nothing in unjira can
   write this field. Autonomy is granted by a human editing config, never earned by the system.
2. **`jira[].writable_project_keys`** — which projects may be written to, declared separately from
   `project_keys` (what may be *read*). Absent means nothing on that connection is writable. A
   project no connection lists in `project_keys` at all is refused with a *different* message,
   because it needs a different fix: unjira does not track it, so the likely remedy is retargeting
   the action rather than widening write scope.
3. **`auto_commit.<type>.confidence_floor`** — the action's own confidence must clear it.

A fresh clone has none of these configured, so a fresh clone applies nothing. `config/unjira.example.json`
deliberately omits `auto_commit` and `writable_project_keys` for exactly this reason: an example
that grants write authority is one copy-paste from arming a real deployment.

**These three cover opening new issues too.** A `create` is proposed for untracked work like any
other action and lands in the queue, where gate 1 refuses to auto-apply it unless a human graduated
`create` specifically. There is no separate flag for proposing one — a proposal is a queue entry,
not a mutation, so gate 1 is already the thing standing between untracked work and a new ticket.

### What's done, per phase-1 slice

| | |
|---|---|
| ✅ 1–4 | LLM client, `correlator.Cluster`, persistence + compaction, `internal/reconciler` |
| ✅ 5 | auto-commit gate + `watch` |
| ✅ 6 | `unjira actions list\|decide` **and `triage`** — the machine-facing and human-facing halves of the review surface |
| ⬜ 7 | `rules.Distill` — rule *reading* works in both prompts; distilling new rules from reviewer feedback does not |

See `docs/superpowers/specs/2026-08-11-phase1-correlator-design.md` for the slice list with the
non-obvious corrections each one produced.

- **Phase 2 — creation and estimation**: spin-off tickets with discovery links, the estimation
  ensemble, the `emergent` tag, the ancillary-work ledger.
- **Phase 3 — productize**: autonomy graduation, Slack review mode, shared team memory,
  config-driven setup.

## Quickstart

```sh
go build -o unjira ./cmd/unjira      # or: earthly +build
cp config/unjira.example.json unjira.config.json
cp .env.example .env                 # Jira + LLM credentials (gitignored)

./unjira collect        # ingest new events from enabled collectors
./unjira status         # event counts and collector cursor freshness
./unjira digest         # print a day's drift digest

./unjira watch --once --dry-run      # one full pass, persisting nothing
./unjira watch --interval 1h         # the real loop; Ctrl-C finishes the pass in flight

./unjira actions list                # the review queue
./unjira actions list --status failed --json
./unjira actions decide 42 --approve # applies via the same gate watch uses

./unjira triage                      # review the queue one action at a time
./unjira triage --dry-run            # walk it, decide, write nothing
```

`triage` is the human-facing surface: it shows one action at a time with its full
body, and **applies nothing until you confirm at the end**. Beyond approve/reject
it can reword an action (`e`), retarget it to a different issue (`t`), merge two
narratives that turned out to be one story (`m`), or split one that was two (`s`).
Uncommitted work stays reshufflable; anything a tracker mutation already describes
does not move — so splitting a narrative whose comment already posted keeps the
described events where they are and moves only the rest.

A reject records your reasoning, which is what slice 7's rule distillation will
learn from.

Start with `watch --once --dry-run`: it runs every stage and prints what it *would* do, skipping
`Persist` and the gate, and says which stages it skipped rather than going quiet.

Reads need Jira credentials (`UNJIRA_JIRA_CREDENTIALS`, a JSON object keyed by connection name)
and an LLM — `UNJIRA_LLM_API_KEY`, or better, `llm.api_key_helper`, a command unjira runs to fetch
a fresh token. A `watch` loop against a gateway issuing short-lived credentials needs the helper;
a captured token expires mid-pass, after earlier stages have already cost money.

Schedule the batch pass on macOS with the launchd template in `ops/` (see comments in the
plist for install steps).

Dev-instance tools (need credentials in `.env`): `unjira dev seed` creates labeled test
issues and walks them through transitions to generate changelog history; `unjira dev reset`
deletes exactly what seed created; `unjira dev workflow` prints the workflow graph, mining it
only when there's no usable per-project cache (`--refresh` forces a re-mine regardless;
`workflow.cache_ttl` in config controls how long a mined graph is trusted otherwise);
`unjira dev narrate` runs one collect+narrate+match+reconcile pass and prints what it found
and proposed. It lives under `dev` rather than as a top-level verb because narration is a
stage inside `watch`, so a top-level `narrate` would be scaffolding awaiting deletion.

Testing: `go test ./...` runs the offline tiers and is what CI runs per-push (the `live`
build tag excludes `internal/live` entirely — it won't even compile without it).
`UNJIRA_LIVE=1 go test -tags=live ./internal/live/...` runs the live suite, which writes to
the dev Jira instance and cleans up after itself; in CI that's the `integration` job, gated
behind the `live-jira` environment.

## Layout

```
cmd/unjira/             CLI entrypoint (Kong): collect | digest | status | watch |
                        actions | triage | dev
internal/
  events/               normalized Event model — the contract every collector emits
  store/                SQLite schema and access: events, cursors, narratives,
                        narrative_events, narrative_issues, actions, estimates, ledger,
                        pipeline_lock
  config/               config loading (unjira.config.json) and validation
  credentials/          the JSON-blob credential set from UNJIRA_JIRA_CREDENTIALS
  envfile/              .env loader with repo-root walk-up
  clients/
    jira/               Jira facade over go-jira/v2/cloud (reads + gated writes)
    local/              local mimicked tracker — no real tracker reachable
    openai/             OpenAI-shaped LLM facade (litellm, etc.)
  llm/                  backend-agnostic LLM contract: Client, Usage, CredentialSource,
                        and the model-response tolerances every parser shares
  tasktracker/          TaskReader / TaskWriter / TaskTracker — the split that makes
                        write authority visible in a signature
  collector/
    claudecode/         Claude Code session transcripts (~/.claude/projects/**/*.jsonl)
    jira/               Jira issues and changelogs as an observed stream
  correlator/
    refs/               fully-qualified, range-aware PR/issue reference extraction
    fanout/             env-mirror fan-out clustering
                        (correlator itself: Cluster = events -> narratives,
                         Match = narratives -> issues)
  reconciler/           delta vs live state, drafting, confidence flooring, Persist.
                        Holds a TaskReader and therefore cannot write
  gate/                 the auto-commit gate: pure Decide + an Applier holding a
                        TaskWriter. The only code in unjira that writes to a tracker
  triage/               the interactive review session as a state machine, with
                        Prompter and Handler seams so it never touches a terminal
  rules/                load, scope-filter, and render rules/*.md into prompts
  workflow/             observed workflow graphs mined from changelogs; BFS path planning;
                        a per-project on-disk cache (TTL + explicit + dirty-flag invalidation)
  pipeline/             stage orchestration: RunCollect / RunNarrate / RunMatch /
                        RunReconcile / RunAutoCommit, plus the digest renderer
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
  whatever stream it came from. unjira then asks whether the work warrants a ticket nobody filed —
  and most of the time the answer is no, so a refusal is a first-class outcome, recorded as a
  `declined` action rather than silence. A decline is remembered: the same narrative is only
  re-judged once new events arrive, so a "no" costs one model call rather than one per `watch`
  tick. Some workflows substitute a placeholder ticket key to satisfy a
  commit-message linter when no real ticket applies; `exclude_from_linking` (a list of regex
  patterns in config, empty by default) tells unjira which ticket-shaped matches are placeholders
  rather than real links, without discarding the fact that one was seen — an event whose only
  candidate keys are all excluded still shows up as untracked, annotated with which key was
  excluded, so it stays visible for later triage instead of vanishing. Those patterns are
  unanchored, so anchor them: `-0$` is right; a bare `-0` would also swallow `PROJ-10` and
  `PROJ-100` as "placeholders".
- **Every stage caps its own batch, and says when it hits the cap.** `match.max_narratives_per_pass`
  and `reconciler.max_narratives_per_pass` are separate numbers because the per-narrative costs
  differ — matching resolves a lone candidate for free and only calls a model when two or more
  survive, while the reconciler drafts for everything it examines. Both log when the backlog
  exceeds the cap: a stage that truncates silently makes unexamined work look like failed work,
  which is exactly how a matching batch limit once read as a matching bug.
- **Comments pass a narrative-worthiness test.** Draft must fit a category: decision made,
  problem discovered, scope changed, blocking, or resolved-with-substance. Otherwise it
  doesn't post.
- **Estimation by ensemble.** N independent passes with different evidence framings
  (spec + similar completed tickets, observed effort, narrative); median is the estimate,
  spread is the confidence. Discovered work is tagged `emergent` so the team can plan with
  `velocity - avg_emergent_points`.
- **SQLite for the event log; no stored procedures.** unjira is a single Go binary against a
  local, single-writer SQLite file — never a networked multi-client RDBMS. The two usual reasons
  to reach for stored procedures (blocking SQL injection from ad-hoc queries; giving many
  consumers one enforced, network-round-trip-saving interface) don't transfer here:
  parameterized queries already close the injection angle, and `internal/store` is already the
  single mandatory door every write goes through — enforced in Go, not SQL, which keeps it
  testable and doesn't require SQLite's procedural-language support (there isn't much). The
  schema is genuinely relational (narratives reference many events; actions reference a
  narrative) — a document store would mean denormalizing the exact join-shaped queries phase 1
  needs most. A dedicated graph DB isn't warranted either: `internal/workflow`'s status-transition
  graph is small (dozens of nodes) and needs only BFS shortest-path, not complex multi-hop
  traversal at scale. A vector store *is* a real future need — for the phase-1 correlator
  matching narratives to open issues by meaning, not exact key — but that's an index alongside
  the event log, not a replacement for it.
- **Buy over build, behind our seam.** Every remote-system client lives under
  `internal/clients/<system>` as a thin facade over its upstream SDK — `clients/jira` over
  go-jira/v2/cloud today, `clients/litellm`/`clients/github`/`clients/slack` as later
  integrations land — so the community absorbs that API's churn and divergence stays
  trivial to reconcile. Hand-rolled only where no wheel exists (the Claude transcript
  parser).
- **Corrections become rules.** Review-queue edits and rejections are distilled into markdown
  rules under `rules/`, fed forward into correlator and reconciler prompts. Approval history
  drives per-action-type autonomy graduation.
- **Write authority is visible in a type signature.** `internal/tasktracker` splits into
  `TaskReader` (`GetIssue`, `SearchIssues`, `AvailableStatusCategories`) and `TaskWriter`
  (`AddComment`, `SetStatus`, `CreateIssue`), with `TaskTracker` the composite. The reconciler
  holds a `TaskReader` and therefore *cannot* write — calling `AddComment` from it is a build
  error, not a code-review catch. `gate.Applier` holds a `TaskWriter` and does nothing else. Note
  `AvailableStatusCategories` sits on the **reader** deliberately: it is a read that describes a
  write, reporting what a write *could* do without performing one.
- **Read scope and write scope are separate config.** `jira[].project_keys` is what unjira may
  *read*; `jira[].writable_project_keys` (a subset) is what it may *write*. Absent means nothing
  is writable. Learned the hard way: reusing the read list as the write surface meant a config
  spanning a sandbox and a production project would, on graduation, have written to production.
  The check lives in `gate.Applier` rather than the gate's `Decide`, because `actions decide
  --approve` never calls `Decide` — a check there would have covered only the automatic path.
- **Pluggable apply-target backend, decided before phase 1 needs it.** `clients/jira.Tracker` and
  `clients/local.Tracker` both implement the interface, config-selected via `tracker.backend`. The local backend lets unjira run with no real
  tracker reachable (e.g. a hosted control plane with no Jira auth) while still deriving value
  from event clustering, persisting its own issue state locally. `SetStatus` takes the tracker's own
  **status name** ("In Review"), not a normalized category. It was categorical until 2026-09-01, on
  the reasoning that GitHub Issues has only open/closed; measurement inverted that argument. In the
  PAAS project both edges unjira exists to propose — `Ready for Dev -> In Progress` and
  `In Progress -> In Review` — are `indeterminate -> indeterminate`, so a category could express
  neither, and "move to In Review" was the same request as "move to Blocked". A named target
  degrades gracefully to a two-name backend (open and closed *are* names) while a category target
  cannot upgrade to a nineteen-status one, so the common denominator belongs as the fallback rather
  than the representation. `StatusCategory` survives for reporting and coarse direction checks.
  Legality is `AvailableTransitions`, a live per-issue read — there is no closed set of names to
  validate against, so `gate.Applier` does not try. Workflow-graph mining is a separate
  `workflow.GraphProvider` capability (type-asserted, not part of `TaskTracker`), since only
  backends with an admin-configurable workflow to mine (Jira) need it. `Config.Jira` is a list of
  named connections, not a single global site, so one project set can span more than one Jira
  instance (a migration, an acquisition). Matching resolves a tracker **per candidate** from the
  connection its provenance recorded (`correlator.TrackerResolver`), so a cross-site candidate is
  verified against the site that actually holds it rather than whichever one the default project
  selects; a candidate naming an unconfigured connection is reported with the reason rather than
  silently falling back, since falling back is how it used to check the wrong site. The reconciler
  still reads through a single tracker — same gap, tracked separately. Credentials come from one
  JSON-blob env var
  (`UNJIRA_JIRA_CREDENTIALS`, keyed by connection name) rather than scaling env-var count with
  connection count.
- **A transition may cross several statuses in one action, because unjira has no guaranteed run
  cadence.** Between two collects a ticket legitimately traverses `Ready for Dev -> In Progress ->
  In Review` — sprint planning moves it outside unjira, work starts, and a small change is submitted
  the same day; interrupt-driven work skips planning entirely. The evidence for the whole journey
  arrives in **one delta**, so the observed workflow graph supplies the legal ordering for a journey
  the evidence already covers rather than inventing a route. One row, one approval, N tracker writes;
  the applier walks the hops and `actions.error` records how far it got, with retry being simply the
  next reconcile pass. The graph only ever **plans** — every hop is validated against the live
  per-issue transition set immediately before it executes, so a stale graph costs a refused hop and
  never a wrong write. A backend with no graph to give degrades to single-hop.
- **A work-derived transition is suppressed when the tracker knows something newer, judged by
  recency rather than direction.** The obvious guard — never move a ticket to an earlier status —
  inverts: backward moves are legitimate (a security scan bouncing a ticket back after finding the
  vulnerability still present, a failed test cycle), and applying monotonicity to that case makes
  unjira re-propose its *forward* move every pass, undoing the handoff. So the rule is: propose only
  when the work evidence postdates the issue's last status change. Two checks implement it, both from
  data already in hand — the newest **collected** status event (the Jira collector already ingests
  status changes, so no API call) compared against the **live** `StatusName` the reconciler already
  reads. Disagreement means somebody moved it since the last collect; a collected change newer than
  the work means that work was superseded. Status changes on the subject issue are excluded from
  "work evidence," or a collected handoff would become its own justification. With no collected status
  history the guard cannot fire and transitions are unguarded — the pre-guard behavior, not something
  worse, but never silent about it. Per narrative, `ReconcileResult.Unguarded` names every such
  proposal — read from the same `HaveLastStatus` the guard branches on, so the two cannot disagree
  about whether a check ran — rendered alongside `Suppressed`/`LowConfidence` in `dev narrate`/`watch`
  output. Once, at startup, `cmd/unjira` warns when NOTHING enabled can ever
  supply this history at all (a configuration gap, distinct from "no history for this one ticket yet")
  — decided via a `pipeline.StatusHistorySource` marker interface the jira collector implements, not a
  name check, so a future GitHub tracker's own status collector is recognized the same way. Silent on
  the `local` tracker backend, which has no real external tracker for anyone to move a ticket behind
  unjira's back. See `docs/design-notes.md` incident 19.

## Writing a collector

Implement the `Collector` interface in `internal/pipeline/collect.go` (`Name() string`,
`Collect(s *store.Store, options map[string]any, visit func(events.Event) error) error`):
read your source since the last cursor, emit normalized `Event`s, update the cursor. Register
it in `cmd/unjira/main.go`'s `registry` map and enable it in config.
`internal/collector/claudecode` is the reference implementation.
