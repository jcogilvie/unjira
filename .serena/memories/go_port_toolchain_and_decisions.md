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
- `IsSentinelKey`/`ExtractTicketKeys` ported as direct 1:1 translations of the Python regex/logic.
  Go's RE2 `\b` is ASCII-only (unlike PCRE's Unicode-aware `\b`), but the ticket-key pattern
  (`[A-Z][A-Z0-9]{1,9}-\d+`) is itself ASCII-only, so this is not a behavioral gap for unjira.

## Deferred (do not start until the whole port is complete)
- Task #10: the `-0`/`-1` sentinel-issue-key convention (`IsSentinelKey`) is a hardcoded
  workplace-specific commit-linter convention leaking into public OSS code. Needs to become
  configurable (e.g. via config) rather than hardcoded, post-port.

See `docs/superpowers/specs/2026-08-07-go-port-design.md` and `docs/go-conventions.md` for the
full port plan and Go conventions. `.requirements/20260807T185557Z_go_module_bootstrap_and_events/`
has this slice's full requirements doc.
