# Go module bootstrap + internal/events port

First implementation slice of the Python-to-Go port (see
`docs/superpowers/specs/2026-08-07-go-port-design.md` and `docs/go-conventions.md`). Scope:
initialize the Go module/toolchain and port the one dependency-free Python module every other
piece in the mapping table depends on: `events.py`.

## As Is

- The repo has no Go artifacts: no `go.mod`, no `cmd/`, no `internal/`. `go version` in this repo
  now resolves to 1.26.0 via a project-local `.tool-versions` (asdf), confirmed installed and
  reshimmed.
- `go-port` branch exists, forked cleanly from `main` at `6ed334a` (verified via
  `git log --oneline main..go-port`), with one commit so far (`.serena/project.yml` +
  `.serena/.gitignore`, adding Go as the primary Serena language server alongside Python).
- The Python implementation (`src/unjira/events.py`, tested by `tests/test_events.py`) defines:
  - `TICKET_KEY_RE = re.compile(r"\b[A-Z][A-Z0-9]{1,9}-\d+\b")` — matches PROJ-123-style keys.
  - `SENTINEL_ISSUE_NUMBERS = {"0", "1"}`.
  - `class Event(BaseModel)`: `source: str`, `external_id: str`, `occurred_at: datetime`,
    `actor: str | None = None`, `summary: str`, `artifacts: dict[str, Any] = {}`,
    `raw_ref: str | None = None`. `store.py` serializes `occurred_at` via `.isoformat()` and
    `artifacts` via `json.dumps(..., default=str)` — confirms the Go struct needs JSON
    marshaling compatible with those shapes (ISO-8601 string, JSON object), though the actual
    store port is a later slice.
  - `extract_ticket_keys(text) -> list[str]`: all regex matches, order-preserving, deduplicated.
  - `is_sentinel_key(key) -> bool`: true if the numeric suffix after the last `-` is in
    `{"0", "1"}`.
- Verified via Kindly web search: Go's `regexp` (RE2) supports `\b` as an ASCII word-boundary
  assertion, matching Python's `re` behavior for this pattern (which is itself ASCII-only, so the
  known RE2 Unicode-boundary gap does not apply here). No behavioral gap for this port.

## To Be

- `go.mod` exists at the repo root, module path `github.com/jcogilvie/unjira`, `go 1.26.0`.
- `github.com/stretchr/testify` and `github.com/google/go-cmp` are added as dependencies
  (`go.sum` populated), per `docs/go-conventions.md`'s testing conventions — needed starting with
  this first test file, not deferred.
- `internal/events` package exists with:
  - An `Event` struct with fields equivalent to the Python model (see Requirements below for
    exact typing/tags).
  - `TicketKeyRegexp` (exported, `*regexp.Regexp`) equivalent to `TICKET_KEY_RE`.
  - `ExtractTicketKeys(text string) []string` equivalent to `extract_ticket_keys`.
  - `IsSentinelKey(key string) bool` equivalent to `is_sentinel_key`.
- `internal/testutil` package exists with the start of the fluent fixture-builder/assertion
  convention from `docs/go-conventions.md` — specifically, whatever minimal builder this test
  file actually needs (an `Event` builder), not the full convention scaffolded speculatively.
- `earthly +go-test` (or, until the Earthfile exists, plain `go test ./...`) passes.

## Requirements

### R1 — Go module bootstrap
Initialize `go.mod` with module path `github.com/jcogilvie/unjira` and `go 1.26.0`. Add
`github.com/stretchr/testify` and `github.com/google/go-cmp` as direct dependencies.

**Acceptance criteria:**
- AC1.1 `go.mod` exists, `module github.com/jcogilvie/unjira`, `go 1.26.0`.
- AC1.2 `go build ./...` succeeds (trivially, with an empty tree) before any package is added.
- AC1.3 `go.sum` is generated and committed once testify/go-cmp are imported by a test file.

### R2 — `Event` struct
Port the Python `Event` model to a Go struct in `internal/events`, field-for-field:

| Python field | Go field | Go type | Notes |
|---|---|---|---|
| `source` | `Source` | `string` | required |
| `external_id` | `ExternalID` | `string` | required; dedup key half |
| `occurred_at` | `OccurredAt` | `time.Time` | required |
| `actor` | `Actor` | `string` | empty string = absent (Go idiom; no `*string` needed unless a real nil/absent distinction is required downstream — none is, per store.py's SQL `TEXT` column allowing NULL either way) |
| `summary` | `Summary` | `string` | required |
| `artifacts` | `Artifacts` | `map[string]any` | defaults to non-nil empty map when constructed via a constructor, matching Pydantic's `default_factory=dict` |
| `raw_ref` | `RawRef` | `string` | empty string = absent, same reasoning as `Actor` |

JSON tags on every field (lowerCamelCase, matching the Go convention doc's `tagliatelle`
guidance referenced for the crossplane-diff audit — actually: no JSON-consumer exists yet for
this struct outside Go itself, so tags should just be present and consistent;
`snake_case` vs `camelCase` is a free choice at this point since nothing external parses it yet).
**Open question for this task**: use plain Go zero values (empty string) for optional fields, or
pointers, to allow JSON `null` round-tripping? Resolved as: plain zero values — see As-Is note
that `store.py` already treats missing `actor`/`raw_ref` as SQL NULL vs empty string
interchangeably, and no test exercises the distinction.

**Acceptance criteria:**
- AC2.1 `Event` struct compiles with the fields/types above.
- AC2.2 A constructor or literal can produce an `Event` with only `Source`/`ExternalID`/
  `OccurredAt`/`Summary` set and `Artifacts` still usable as an empty, non-nil map (i.e.
  `len(e.Artifacts) == 0` and appending to it doesn't panic) — mirrors Pydantic's
  `default_factory=dict` behavior, not Go's zero-value nil map.

### R3 — `ExtractTicketKeys`
Port `extract_ticket_keys`: find all `\b[A-Z][A-Z0-9]{1,9}-\d+\b` matches in input text,
order-preserving, deduplicated.

**Acceptance criteria:**
- AC3.1 `ExtractTicketKeys("PROJ-123 fixes AUTH-9; see PROJ-123 and proj-99 (lowercase ignored)")`
  returns `[]string{"PROJ-123", "AUTH-9"}` — direct port of the one existing Python test case.

### R4 — `IsSentinelKey`
Port `is_sentinel_key`: true iff the numeric suffix after the last `-` is `"0"` or `"1"`.

**Acceptance criteria:** (direct port of the existing Python test cases)
- AC4.1 `IsSentinelKey("XYZ-0")` → `true`.
- AC4.2 `IsSentinelKey("PROJ-0")` → `true`.
- AC4.3 `IsSentinelKey("LONGERKEY-1")` → `true`.
- AC4.4 `IsSentinelKey("XYZ-10")` → `false`.
- AC4.5 `IsSentinelKey("PROJ-123")` → `false`.

## Testing Plan

One Go test file per Python test file being ported, same TDD order as every other slice in this
port: write the test (red), then the implementation (green).

- `internal/events/events_test.go` ports `tests/test_events.py`:
  - `TestExtractTicketKeys_DedupesInOrder` ← `test_extracts_and_dedupes_in_order` (AC3.1).
  - `TestIsSentinelKey_PrefixAgnostic` ← `test_sentinel_detection_is_prefix_agnostic`
    (AC4.1–AC4.5), as a table-driven test (multiple scenarios of the same behavior — per
    go-conventions.md, this is exactly the case that calls for a table).
- No test file exists yet for `Event` itself in Python (it's exercised indirectly via
  `store.py`'s tests) — AC2.2 gets a minimal direct test anyway
  (`TestEvent_ArtifactsDefaultsToEmptyMap` or similar), since Go's zero-value nil-map gotcha is a
  real behavioral risk the Pydantic version didn't have, and this is the first place it can bite.
- Use `testify/assert` for all assertions (per go-conventions.md); use `require` only where a nil
  check gates a later dereference (not expected to be needed for this simple slice, but keep the
  convention in mind for the table-driven test if it grows an error-case row later).
- Introduce a minimal `internal/testutil` `Event` builder only if a raw struct literal in the
  test would otherwise obscure which field is under test (per go-conventions.md's fixture-builder
  rule) — for AC2.2 specifically, since that test is about `Artifacts` defaulting, not about the
  other fields.

## Implementation Plan

1. **Go module bootstrap (R1).** Run `go mod init github.com/jcogilvie/unjira` at repo root; set
   `go 1.26.0` in `go.mod` (matches the asdf-managed toolchain, confirmed installed/reshimmed).
   Test: `go build ./...` succeeds (no packages yet, so this just confirms `go.mod` is
   well-formed).
2. **Write `internal/events/events_test.go` for `ExtractTicketKeys` (R3, AC3.1) — red.** Import
   `github.com/jcogilvie/unjira/internal/events` (package doesn't exist yet) and
   `github.com/stretchr/testify/assert`. Confirm it fails to compile (package/function don't
   exist).
3. **Implement `internal/events/events.go`: package doc comment, `TicketKeyRegexp`,
   `ExtractTicketKeys` (R3) — green.** Run `go test ./internal/events/...`.
4. **Write the table-driven `IsSentinelKey` test (R4, AC4.1–AC4.5) — red.** Add to
   `events_test.go`.
5. **Implement `IsSentinelKey` (R4) — green.** Run `go test ./internal/events/...`.
6. **Write `Event` struct fields + the `Artifacts`-defaulting test (R2, AC2.1–AC2.2) — red.**
   Decide during this step whether a constructor function (`NewEvent(...)`) is needed to satisfy
   AC2.2, or whether call sites are expected to initialize `Artifacts` themselves — resolve by
   checking how `store.py`'s `insert_event` constructs/consumes an `Event` today (Python
   construction always goes through Pydantic's `default_factory`, so there is no direct
   equivalent call site to mirror; decide based on what makes `internal/store`'s later Go port
   simplest, most likely a small constructor).
7. **Implement `Event` struct (+ constructor if AC2.2 needs one) (R2) — green.** Run
   `go test ./internal/events/...`.
8. **Full-suite check.** `go vet ./...` and `go test ./...` from repo root. `go.sum` committed
   alongside `go.mod` once testify/go-cmp are pulled in.
9. **Record to Serena memory**: Go toolchain/version decision (asdf, project-local
   `.tool-versions`, 1.26.0) and the `Actor`/`RawRef` zero-value-vs-pointer decision, so later
   slices in the port don't re-litigate either.
