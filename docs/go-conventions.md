# Go conventions

unjira is switching implementation language from Python to Go (see `docs/design-notes.md` for
why the Python phase-0 code existed, and the port design spec for why we switched). This
document is the single source of truth for unjira's own Go style — followed consistently rather
than reinvented per file. Each convention below states its rationale so it can be revisited on
its own merits later, not treated as inherited dogma.

## CLI: Kong

`github.com/alecthomas/kong`. Chosen because it gives dependency injection that flag-parsing
libraries like `cobra` don't have built in: `kong.BindTo`/`kong.BindToProvider` register
values/providers on the parse context, and a field's `BeforeApply(ctx *kong.Context) error`
method can bind itself (`ctx.BindTo(c, (*SomeProvider)(nil))`) so later-resolved dependencies can
reach it. Struct tags define flags/help/defaults/enums directly on the command struct — no
separate flag-registration code.

Pattern: a top-level `cli` struct with one field per subcommand (`cmd:""` tag), shared flags
factored into an embedded `CommonCmdFields` struct, `kong.Parse(&cli{}, ...)` in `main()`,
`ctx.Run()` to dispatch. Provider functions (`func provideX(deps...) (*X, error)`) are resolved
lazily, once per unique dependency chain — cache manually (package-level var) if a provider must
be a singleton across the command lifecycle.

## Interfaces and constructors

- Interface name is the capability (`Correlator`, `JiraClient`); the concrete type is
  `Default<Interface>` or a purpose-named type (`FakeJiraClient` for test doubles); constructor
  is `New<Interface>(deps...) <Interface>` — returns the interface type, not the concrete struct.
- Assert satisfaction at compile time next to the fake/mock: `var _ Interface = (*FakeImpl)(nil)`.
- Dependencies are injected via constructor parameters (`NewCorrelator(llmClient, store, logger)`).
  Reach for a functional-options constructor (`type Option func(*Config)`) only once a
  constructor accumulates enough optional parameters that positional args get unclear — don't
  default to options for a two-or-three-dependency constructor.

## Error handling

Wrap with stdlib `fmt.Errorf("cannot get issue %s: %w", key, err)` at every layer boundary — never
return a bare upstream error. This mirrors a locked unjira design invariant that predates the Go
port (see `docs/design-notes.md` #9 on erroring loudly): silent or context-free failure is the
hardest class of bug to notice, so every wrap should say what we were trying to do, not just what
broke underneath.

## Testing

Use **`github.com/stretchr/testify`** (`assert`/`require`), not bare stdlib comparisons.
Rationale: a common objection to assertion libraries is that generic assertions produce generic,
unhelpful failure messages — but testify's trailing `msgAndArgs` param lets every call carry a
specific message (`assert.Equal(t, want, got, "ParsePRRefs(%q)", input)`), and
`require.ErrorContains`/`ErrorIs`/`ErrorAs` remove a repeated hand-rolled check from every
table-driven test's error-case row. The dependency is small and easy to remove later if it stops
earning its keep — this is not a one-way door.

- `require.X` for anything that should stop the test immediately on failure (a nil check before
  dereferencing, an unexpected error in the non-error-case branch); `assert.X` for checks that
  should keep collecting failures within one test.
- `google/go-cmp` (`cmp.Diff`) backs structural comparisons where seeing a `-want +got` diff in
  the failure output is worth more than a plain pass/fail from `assert.Equal`.
- **Fluent, AssertJ-style domain assertions are a natural extension, not a separate library.**
  Because testify's `TestingT` is just `{ Errorf(format string, args ...interface{}) }`, write our
  own chained assertion helpers over unjira's domain types when a call chain reads better than a
  flat `assert.X` — e.g. `tu.AssertRefs(t, got).HasLen(2).First().HasKey("repo-a#382")`. These live
  in the same test-support package as the fixture builders (below), not in a separate framework.
- Table-driven tests for multiple scenarios of the same behavior — this is the primary place
  testify's terseness compounds: every table needs its own error-shape check duplicated on every
  wantErr row if written by hand, one line each with `require.ErrorContains`.
- `t.Context()` (stdlib, Go 1.24+) instead of hand-rolling a `context.Background()` per test.

## Test fixture builders

Never construct a nontrivial test fixture as a raw struct literal or a raw helper function with a
long, easy-to-miscount positional argument list. Both bury *which fields actually matter for this
test* under boilerplate the reader has to mentally filter out. Instead, use a fluent builder per
domain type: one method per settable field/aspect, sensible defaults for everything not
explicitly set, terminated by `.Build()`:

```go
// Instead of a raw struct literal (which of these fields matter here?):
item := FanoutItem{Repo: "infra-repo", Author: "alice", Title: "...", Number: 678}

// ...a builder makes the test's actual point visible at a glance:
item := tu.NewFanoutItem().WithTitle("Switch shared-infra to managed mode (zrh)").Build()
// repo/author/number take sensible defaults; only the field under test is set explicitly
```

Put these in an internal `testutil`/`tu` package per domain area, imported only from `_test.go`
files. Combine with the fluent assertion helpers above so both halves of a test — the given and
the then — read as chains describing intent, not as blocks of positional literals.

## Formatting & linting

`golangci-lint` v2, `linters.default: all` minus an explicit, commented disable-list. Only disable
a linter with a written rationale next to it — an unexplained disable invites someone to
re-enable it without knowing why it was off. Starting set to disable, each because it fights a
style choice made deliberately above or elsewhere in this doc, not because the linter is wrong in
general:

- `wsl`, `nlreturn` — add whitespace in the name of readability; not something we've found worth
  enforcing mechanically.
- `lll` — line-length limits aren't idiomatic Go; prefer linting for the *causes* of long lines
  (too many params, redundant type names) over the symptom.
- `err113` — requires `errors.Is`/`errors.As`-only equality checks and package-level `errors.New`;
  stricter than we need for a project this size.
- `dupl`, `gocyclo`/`cyclop`/`nestif`/`funlen`/`maintidx` (duplicate what `gocognit` already
  covers), `exhaustruct`, `godox`, `mnd`, `nonamedreturns`, `ireturn` — each trades a real signal
  for a lot of false positives or friction that doesn't match how this codebase is meant to read.
- `noinlineerr`, `noctx` — inline error handling is idiomatic and readable; explicit context
  threading can be added incrementally where it matters instead of being enforced everywhere.
- `forbidigo` — bans `fmt.Print*`/`println`; fights the entire purpose of `cmd/unjira`, a CLI
  whose job is printing to stdout.
- `gochecknoglobals` — package-level const/var lookup tables (region lists, sentinel numbers,
  the collector registry, the Kong `cli` struct) are idiomatic Go at package scope, not a smell.
- `wrapcheck` — wants every returned error wrapped, even ones that already carry full context or
  come from a well-understood stdlib/interface boundary (`bufio.Scanner.Err`, `sql.Rows.Err`).
  Our own convention is to wrap at meaningful boundaries with "what were we trying to do" context
  (see Error handling above), not universally.
- `gosec` — G404 (weak RNG) fires on `internal/devtools`'s deliberately seedable
  `math/rand/v2` generator (reproducible test data, not security-sensitive); G304 (file inclusion
  via variable) fires on every function that reads a user-supplied config/transcript/graph-cache
  path, which is the entire point of those functions.
- `nilerr`, `nilnil` — `internal/collector/claudecode` deliberately returns `(nil, nil)` for "no
  event this session" (missing transcript dir, no user messages, excluded cwd) — a valid,
  non-error outcome matching the ported Python's `return` / `return None`, not a bug to paper
  over with an artificial sentinel error.
- `varnamelen` — wants short-lived loop/handler variables (`w`, `r`, `i`, `ok`, `err`) given
  longer names; fights Go's own idiom of short names for small-scope variables.
- `testpackage` — our tests already consistently use `*_test` packages; this adds no signal on
  top of a convention we already follow.
- `paralleltest` — the suite is fast (sub-second); the wall-clock win from auditing every test
  for safe `t.Parallel()` use isn't worth it at this size.
- `perfsprint` — warns `fmt.Sprintf` is less performant than concatenation/`strconv`; we value
  readability over an unmeasured performance difference.
- `tagliatelle`, `tagalign` — enforce camelCase-first JSON tag conventions. unjira's few
  JSON-tagged structs mirror the original Python/Jira field names (e.g. `status_categories`) for
  continuity with the ported schema and the live Jira API shape, not a Go-idiomatic API of our
  own.
- `wsl`/`wsl_v5` (the same rule, renamed in newer golangci-lint releases; both disabled so an
  upgrade doesn't silently re-enable it) — see above.

`dupword` stays enabled (it catches real typos); the one legitimate false positive — a test
string that repeats a token on purpose to exercise dedup logic — is suppressed inline with an
explained `//nolint:dupword`, not globally disabled.

Formatters: `gofmt` (simplify), `gofumpt`, `goimports`, `gci` with custom import ordering:
standard library, then third-party, then a blank-line-separated group for unjira's own
`github.com/jcogilvie/unjira/...` packages.

## Package layout

License header + one-line package doc comment (`// Package foo does X.`) at the top of every
file that starts a new package. Package names are short, lowercase, no underscores
(`correlator`, `jira` — not `llm_correlator`), per standard Go convention.

**Remote-system clients live under `internal/clients/<system>`** (`clients/jira`,
`clients/litellm`, `clients/github`, `clients/slack`, ...), one subpackage per external system
unjira talks to — whether as a write target (Jira) or an event source (a future GitHub/Slack
collector) or a model backend (LiteLLM/Bedrock/Anthropic/OpenAI). Grouping by "talks to a remote
system" gives every new integration an obvious, consistent home instead of each one inventing its
own top-level package name. Each client package is a thin facade over its upstream SDK/API — no
unjira business logic inside; that logic (collectors interpreting a client's data, the correlator
judging it, gated writes deciding whether to call a client at all) is layered on top, in
`internal/collector/...`, `internal/correlator/...`, etc., not inside `clients/`.

## Build: Earthfile (earthbuild)

Use earthbuild (the community-maintained Earthly fork) as the build system: small composable
targets (`+go-build`, `+go-test`, `+go-lint`, `+go-modules-tidy`) plus meta-targets that `BUILD`
them (`+build`, `+test`, `+lint`, `+generate`, `+reviewable`). `+reviewable` is the pre-PR gate —
run it before opening a PR, the same way the Python phase-0 code expected `pytest`+`ruff` first.
Earthfiles give reproducible, cacheable builds across contributors' machines and CI without a
separate Dockerfile-plus-Makefile pairing to keep in sync.

## Module path

New Go module path: `github.com/jcogilvie/unjira` (matches the existing GitHub remote). This
replaces the Python `src/unjira/` package; there is no dual-module transition — the repo becomes
Go-only once the port lands (see the port design spec for what happens to the Python tree).
