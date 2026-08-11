# Go port: toolchain + early decisions

## Toolchain
- Go managed via asdf. Project-local `.tool-versions` pins `golang 1.26.0` (repo root), separate
  from the user's global asdf default (was 1.25.7 at port start). Installed + reshimmed.
- **Gotcha**: long-lived shell sessions (incl. this Claude Code session) may have `GOROOT`/`GOPATH`
  exported stale from before `.tool-versions` was created, even though `go version` and the `go`
  shim itself resolve correctly. Symptom: `compile: version "X" does not match go tool version "Y"`
  spam across stdlib packages when running `go build`/`go test`. Fix: prefix Go invocations with
  `env -u GOROOT -u GOPATH` in that shell, or open a fresh shell — no reshim/reinstall needed, the
  installation is fine, only the exported env vars are stale.
- Module path: `github.com/jcogilvie/unjira`. Dependencies added so far: `github.com/stretchr/testify`
  (+ transitive go-spew/go-difflib/yaml.v3), `github.com/google/go-cmp` (not yet imported by any
  test, so `go mod tidy` currently drops it from go.mod until first use — re-add when a test
  actually calls `cmp.Diff`).

## internal/events (first ported package)
- `Event` struct: optional Python fields (`actor`, `raw_ref`) become plain Go `string` zero-values,
  NOT `*string` — nothing in the codebase distinguishes "absent" from "empty" for these today, and
  store.py already treats them interchangeably as SQL NULL vs empty. Revisit only if a real
  nil-vs-empty distinction is needed later.
- `Artifacts map[string]any` needs a constructor (`NewEvent`) to default to a non-nil empty map —
  Go's zero-value nil map is unsafe to write to, unlike Pydantic's `default_factory=dict`. This is
  a real behavioral gap Python didn't have; guarded by TestNewEvent_ArtifactsDefaultsToEmptyMap.
- `ExtractTicketKeys` ported as a direct 1:1 translation of the Python regex/logic. Go's RE2 `\b`
  is ASCII-only (unlike PCRE's Unicode-aware `\b`), but the ticket-key pattern
  (`[A-Z][A-Z0-9]{1,9}-\d+`) is itself ASCII-only, so this is not a behavioral gap for unjira.
- **Resolved (post-port, per Task #10):** the hardcoded `-0`/`-1` sentinel-issue-key convention
  (`IsSentinelKey`) was replaced by a configurable `exclude_from_linking` (regex patterns, empty
  by default) in `unjira.config.json`. `events.CompileLinkExclusionPatterns` +
  `events.PartitionExcludedKeys` do the matching; `pipeline.RunCollect` persists an
  `excluded_ticket_keys` artifact alongside the untouched `ticket_keys` (never drops data);
  `RenderDigest` annotates untracked events that had an excluded-only match instead of making them
  indistinguishable from a plain no-match event. See
  `.requirements/20260810T210148Z_configurable_link_exclusion_patterns/REQUIREMENTS.md`.

- **Resolved: Task #26 (datastore choice).** SQLite stays for the event log; no stored
  procedures. Neither of stored procs' usual rationales transfers to unjira's shape (single Go
  binary, single-writer, embedded, local file, no network hop): parameterized queries already
  block injection, and `internal/store` is already the one enforced door every write goes
  through — enforced in Go, not SQL. Document store rejected — the schema is genuinely
  relational (narratives→events, actions→narratives are join-shaped queries phase 1 needs most).
  Dedicated graph DB rejected for `internal/workflow`'s transition graph — small, BFS-only, no
  complex multi-hop traversal need. Vector store flagged as a real *future* need, but for the
  phase-1 correlator's narrative-to-issue semantic matching specifically, as an index alongside
  the event log — not a replacement for it, and not a phase-0 concern. See the "SQLite for the
  event log; no stored procedures" bullet in README.md's Design decisions, and Task #29.

- **Resolved: Task #27 (pluggable task-tracking backends).** New
  `internal/tasktracker.TaskTracker` interface (`GetIssue`, `SearchIssues`, `AddComment`,
  `SetStatus`, `CreateIssue` — no `estimate` method, since estimates are unjira's own derived
  data, never a native tracker field). Two implementations exist from day one, not just Jira: a
  Jira adapter (`internal/clients/jira.Tracker`, wrapping the existing `Client` with API-shape
  normalization) and a fully local backend (`internal/clients/local.Tracker`, backed by new
  `local_issues`/`local_issue_comments` tables in `internal/store`) for running with no real
  tracker reachable — e.g. a hosted control plane with no Jira auth. A second, non-Jira
  implementation existing before any phase-1 caller is what proves the interface abstraction
  generalizes rather than being Jira-shaped in disguise. `SetStatus` is deliberately categorical
  (`StatusTodo`/`InProgress`/`Done`), not Jira's named-transition model, since GitHub Issues (a
  real future backend, already used elsewhere) has only open/closed. Workflow-graph mining stays
  a separate `workflow.GraphProvider` capability (type-asserted, not part of `TaskTracker`) since
  GitHub/local have no changelog to mine — Jira's implementation wraps the existing
  `workflow.MineProject`; local's is a hardcoded static 3-node graph.
  `config.Jira` also became `[]JiraConnection` (was a single global site) so one project set can
  span multiple Jira instances (migration/acquisition scenario) — collector and tracker resolve a
  connection by project key via `JiraConnectionForProject`. Credentials moved from two bare env
  vars to one JSON-blob env var (`UNJIRA_JIRA_CREDENTIALS`, keyed by connection name) — verified
  empirically that this requires a named struct wrapping the map with an explicit `UnmarshalJSON`,
  not a bare `map[string]T`, since Kong's env decoding falls back to its own `key=value;...`
  map decoder for anything not satisfying `json.Unmarshaler` by type. New `TrackerConfig.
  DefaultProject` + `Config.DefaultProjectConnection()` answer "where does a brand-new issue land
  with no other routing signal" — smart routing is deferred reconciler work, this is just the
  configured floor. See the design doc this was implemented from: plan file referenced in this
  session's TDD steps (15 total, each red-green committed independently across 4 commits).

## Deferred (post-port; do not start without user confirmation)
- Task #29: evaluate a vector index (e.g. sqlite-vec) for the phase-1 correlator's narrative-to-
  issue semantic matching — surface during phase-1 design, not before.

See `docs/superpowers/specs/2026-08-07-go-port-design.md` and `docs/go-conventions.md` for the
full port plan and Go conventions. `.requirements/20260807T185557Z_go_module_bootstrap_and_events/`
has this slice's full requirements doc.
