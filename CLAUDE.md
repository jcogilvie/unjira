# Working on unjira

Guidance for anyone (human or agent) editing this codebase. This is distinct from `rules/`:

- **`CLAUDE.md` (this file)** — how to work on *unjira the codebase*.
- **`rules/`** — behavioral constraints for the agent *unjira implements* (the correlator and
  reconciler load them into their prompts). Don't put codebase conventions there, or agent
  behavioral norms here.
- **`docs/design-notes.md`** — *why* unjira is shaped this way: the failure modes that drove the
  architecture. Read it before changing the pipeline's structure.
- **`docs/architecture.md`** — *what* is actually there: the pipeline, write-authority graph, action
  lifecycle and package dependencies as mermaid diagrams, plus open findings from the 2026-09-02
  scan. Kept current in the PR that invalidates it, same as the README.
- **`docs/go-conventions.md`** — how unjira's Go is written: CLI/DI (Kong), error handling,
  testing (testify + go-cmp + fluent builders), package layout, linting, build (Earthfile). Read
  it before writing or reviewing any Go in this repo.

## What unjira is

A reconciliation agent that diffs reality (what you did, from event streams) against Jira (what the
org thinks you did) and proposes the minimal patch. It is a reconciler in the Kubernetes-controller
sense, not an event forwarder. See `README.md` for the pipeline and locked design decisions.

## Commands

```sh
go test ./...                 # offline tests (what CI runs); the "live" build tag excludes
                               # internal/live entirely — it won't even compile without it
golangci-lint run ./...       # or: earthly +lint
earthly +reviewable           # lint + test; run before opening a PR
go run ./cmd/unjira collect | digest | status
UNJIRA_LIVE=1 go test -tags=live ./internal/live/...   # writes to the dev Jira instance; needs creds
```

## Architecture invariants

These are load-bearing — `docs/design-notes.md` explains the incidents behind each:

- **Collectors are dumb and deterministic.** They extract metadata + candidates and defer all
  judgment. No LLM, no network beyond their own source, no "does this ticket exist" checks. Adding
  a stream = implement the `Collector` interface in `internal/pipeline`, register it in
  `cmd/unjira/main.go`'s registry, enable it in config.
- **All verification lives in the reconciler.** Never emit a state-bearing action from transcript
  intent; confirm current state against live Jira/GitHub first (see `rules/intent-not-outcome.md`).
- **The correlator's deterministic primitives run before any model.** `internal/correlator/refs`
  and `internal/correlator/fanout` are pure functions with no I/O and no Jira dependency — keep
  them that way; they're what keeps the LLM's review queue signal-rich.
- **Never silently drop data.** Prefer erroring loudly over truncating (e.g. `refs.ParsePRRefs`
  errors on an over-`max_span` range). Silent data loss is the hardest class of bug to notice here.
- **Writes are gated by the pipeline, not the client.** `clients/jira.Client` has write methods,
  but *authorization* to call them lives in the review queue / autonomy graduation, never in the
  client.

## Keep the docs true in the PR that changes the code

Not as a follow-up task. A 2026-08-27 audit found the README claiming **"Status: phase 0 (observe
only) / Zero write risk"** while `gate.Applier` was reachable from two shipped commands and had
already written to real Jira — the most load-bearing paragraph in the repo for a newcomer, and
materially false. Two spec files said `Status: design` for work that had merged. Every instance
traced to the same cause: landing a PR without touching the docs it invalidated.

So, in the same PR:

- **`README.md`** — if you changed the CLI surface, a config key, the pipeline's shape, a package
  layout, or anything in "Design decisions (locked)". The Status section especially: it states what
  unjira *can do to someone's Jira*, so a stale one is a safety problem, not a tidiness one.
- **The design spec** — move `Status: design` to `## Status: landed <date>` with the verification
  evidence (what you ran, what it printed) and any deviation from the design. Leave the original
  "open questions" as written rather than editing them into agreement with what shipped; the record
  of what was uncertain is worth more than a doc that looks prescient.
- **`docs/design-notes.md`** — only when you hit a *new* failure mode worth a numbered incident.
  This file is why-we-are-shaped-this-way, not a changelog.
- **`docs/architecture.md`** — if you changed a pipeline stage, who holds write authority, the action
  lifecycle, or a package's dependencies. The diagrams are load-bearing for a newcomer's mental
  model, so a stale one misleads in the same way a stale Status section does. Also strike a finding
  when you fix it, rather than leaving a resolved problem described as open.
- **This file** — when an architecture invariant changes.

A doc claim that can be checked should be checked. "602 assertions pass" and "the write path is
gated" are both verifiable; write them only after running the thing, and quote what it printed. A
plausible-looking number nobody measured is the same defect as a stale status line, just harder to
notice.

## Conventions

Full Go style — CLI parsing, error handling, testing idioms, package layout, linting, build — is
in `docs/go-conventions.md`, not duplicated here. In brief:

- TDD: write the failing test first, then the implementation.
- `internal/clients/<system>` is the seam for every remote-system facade (Jira today; future
  litellm/GitHub/Slack). Thin facade, no business logic — that lives one layer up.
- Credentials come from the environment (`UNJIRA_JIRA_*`), never config files. Config is
  `unjira.config.json` (gitignored); `config/unjira.example.json` is the template.
