# Collector Seam (`CollectContext`) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give collectors access to config and credentials, so a remote collector (Jira now, GitHub
later) can be written at all.

**Architecture:** Replace `Collect(store, options, visit)` with `Collect(CollectContext, visit)`,
where `CollectContext` carries the store, the loaded config, resolved per-connection credentials, and
the collector's own options. Move the credential type out of `cmd/unjira` into
`internal/credentials` so collector packages can name it. Kong's env-var binding stays in `cmd`.

**Tech Stack:** Go 1.26, `github.com/alecthomas/kong`, `github.com/stretchr/testify`.

**This is a pure refactor. Zero behavior change.** Every existing test must pass **unmodified** except
where a signature literally forces an edit. If you find yourself changing what a test asserts, stop —
that means the refactor altered behavior, which it must not.

**Why it exists:** `Collect` currently receives only `cfg.Collectors[<name>]` — the collector's own
options block. It cannot reach `cfg.Jira` (sites, project keys) or `UNJIRA_JIRA_CREDENTIALS`. That was
fine for `claude_code`, which reads local files and needs no credentials, but it blocks every remote
collector. The next slice (`docs/superpowers/specs/2026-08-21-jira-collector-design.md`) needs this
seam; a GitHub collector would need exactly the same one.

---

## Critical environment notes (every task)

**Stale `GOROOT`/`GOPATH`.** Prefix EVERY `go`/`gofmt` invocation with `env -u GOROOT -u GOPATH`,
e.g. `env -u GOROOT -u GOPATH go test ./...`. Without it you get spurious
`compile: version "X" does not match go tool version "Y"` errors unrelated to your change.

**`golangci-lint`:**

```bash
export PATH="$HOME/.asdf/installs/golang/1.25.7/bin:$PATH"
env -u GOROOT -u GOPATH golangci-lint run ./...
```

Keep local golangci-lint current with what CI installs (`@latest`); the repo's `.golangci.yml`
disables both `exhaustruct` and `exhaustruct_v5` because 2.13 renamed it, and an older binary rejects
the new name. **The branch is at `0 issues` and must stay there.** Ignore `gomodguard is deprecated`
warnings.

**The authoritative gate is `earthly +reviewable`** — lint plus tests in a clean container with its own
Go 1.26 toolchain. A local pass with a container failure means something is environment-dependent.

**Work in the worktree** `/Users/jonathan.ogilvie/workspace/unjira/.claude/worktrees/slice4-reconciler`
(branch `worktree-slice4-reconciler`). Run all commands from there. Do not create or switch branches.
Do not touch the session task list — the coordinator owns it.

**Repo conventions** (`CLAUDE.md`, `docs/go-conventions.md`): TDD; never silently drop data; doc
comments on every exported identifier explaining *why*; testify `require` (fatal) / `assert`
(non-fatal).

---

## File structure

- **Create:** `internal/credentials/credentials.go` — the `Credential` type and a `Set` keyed by
  connection name. No JSON-env-var parsing (that stays in `cmd`, where Kong needs it); this package is
  just the shape collectors and `cmd` agree on.
- **Create:** `internal/credentials/credentials_test.go` — `Set` lookup behavior.
- **Modify:** `internal/pipeline/collect.go` — add `CollectContext`, change the `Collector` interface,
  thread config and credentials through `RunCollect`.
- **Modify:** `internal/pipeline/collect_test.go` — `fakeCollector`'s signature; add a test proving the
  context reaches the collector.
- **Modify:** `internal/collector/claudecode/claudecode.go` — new signature; read `cc.Options` and
  `cc.Store`.
- **Modify:** `cmd/unjira/main.go` — `JiraCredentials` wraps `credentials.Set`; pass it to
  `RunCollect`.
- **Modify:** `cmd/unjira/main_test.go` — construct credentials via the new type.

Rationale for a separate `internal/credentials` package: `cmd/unjira` cannot be imported (it's
`package main`), and putting the type in `internal/config` would be wrong — credentials come from the
environment, never from config files, and that separation is a deliberate repo rule worth keeping
visible in the package layout.

Sequencing: the credential type (Task 1) has no dependencies. `CollectContext` (Task 2) uses it.
`claudecode` (Task 3) and `cmd` (Task 4) adapt to the new interface. Task 5 verifies.

---

## Task 1: `internal/credentials`

**Files:**
- Create: `internal/credentials/credentials.go`
- Create: `internal/credentials/credentials_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/credentials/credentials_test.go`:

```go
// Package credentials_test covers the lookup contract collectors rely on.
package credentials_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/credentials"
)

func TestSet_ForReturnsCredentialByConnectionName(t *testing.T) {
	set := credentials.NewSet(map[string]credentials.Credential{
		"corp": {Email: "me@corp.example", Token: "t1"},
		"paas": {Email: "me@paas.example", Token: "t2"},
	})

	got, ok := set.For("corp")

	require.True(t, ok)
	assert.Equal(t, "me@corp.example", got.Email)
	assert.Equal(t, "t1", got.Token)
}

func TestSet_ForMissingConnectionReportsNotFound(t *testing.T) {
	set := credentials.NewSet(map[string]credentials.Credential{})

	_, ok := set.For("nope")

	assert.False(t, ok, "a missing connection must be reported, not returned as a zero credential")
}

func TestSet_ZeroValueIsUsable(t *testing.T) {
	// A Set nobody populated must not panic on lookup: a collector that needs
	// no credentials is given the zero value.
	var set credentials.Set

	_, ok := set.For("corp")

	assert.False(t, ok)
}
```

- [ ] **Step 2: Run it to confirm it fails**

Run: `env -u GOROOT -u GOPATH go test ./internal/credentials/ -v`

Expected: FAIL — the package doesn't exist
(`no required module provides package .../internal/credentials`).

- [ ] **Step 3: Implement the package**

Create `internal/credentials/credentials.go`:

```go
// Package credentials holds the shape of a remote system's credentials, keyed
// by the config connection they belong to.
//
// It exists as its own package because collectors need to name the type and
// cmd/unjira is package main (unimportable), while internal/config would be
// the wrong home: credentials come from the environment, never from config
// files, and keeping them out of the config package keeps that rule visible in
// the layout rather than only in a doc comment.
//
// Parsing lives in cmd/unjira, where Kong decodes UNJIRA_JIRA_CREDENTIALS from
// a single JSON-object env var. This package is only the agreed shape.
package credentials

// Credential is one connection's email/token pair.
type Credential struct {
	Email string `json:"email"`
	Token string `json:"token"`
}

// Set maps a config connection name (config.JiraConnection.Name) to its
// credential. The zero value is usable and reports every lookup as missing,
// so a collector needing no credentials can be handed one safely.
type Set struct {
	byName map[string]Credential
}

// NewSet returns a Set over byName. The map is used as given, not copied:
// callers build it once at startup and do not mutate it afterward.
func NewSet(byName map[string]Credential) Set {
	return Set{byName: byName}
}

// For returns the credential for a connection name, reporting whether one was
// configured. Missing is a distinct outcome from an empty credential: a caller
// must be able to say "no credentials for connection X" rather than failing
// later with an unauthenticated request.
func (s Set) For(connectionName string) (Credential, bool) {
	credential, ok := s.byName[connectionName]

	return credential, ok
}
```

- [ ] **Step 4: Run it to confirm it passes**

Run: `env -u GOROOT -u GOPATH go test ./internal/credentials/ -v`

Expected: PASS, all three tests.

- [ ] **Step 5: Commit**

```bash
env -u GOROOT -u GOPATH gofmt -w internal/credentials/
env -u GOROOT -u GOPATH go vet ./internal/credentials/...
git add internal/credentials/
git commit -m "Add internal/credentials: connection-keyed credential shape"
```

---

## Task 2: `CollectContext` and the `Collector` interface

**Files:**
- Modify: `internal/pipeline/collect.go`
- Modify: `internal/pipeline/collect_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/pipeline/collect_test.go`:

```go
func TestRunCollect_PassesConfigAndCredentialsToCollector(t *testing.T) {
	s := openTestStore(t)

	var gotContext pipeline.CollectContext
	capturing := &contextCapturingCollector{seen: &gotContext}

	cfg := config.Config{
		Collectors: map[string]map[string]any{
			"capture": {"enabled": true, "some_option": "value"},
		},
		Jira: []config.JiraConnection{
			{Name: "corp", Site: "https://corp.atlassian.net", ProjectKeys: []string{"PROJ"}},
		},
	}
	creds := credentials.NewSet(map[string]credentials.Credential{
		"corp": {Email: "me@corp.example", Token: "t1"},
	})

	_, err := pipeline.RunCollect(cfg, s, map[string]func() pipeline.Collector{
		"capture": func() pipeline.Collector { return capturing },
	}, nil, creds)

	require.NoError(t, err)
	assert.Same(t, s, gotContext.Store, "the collector gets the same store RunCollect was given")
	assert.Equal(t, "value", gotContext.Options["some_option"],
		"the collector's own options block still reaches it")
	require.Len(t, gotContext.Config.Jira, 1)
	assert.Equal(t, "corp", gotContext.Config.Jira[0].Name,
		"a remote collector needs connection config, which is why this seam exists")
	gotCred, ok := gotContext.Credentials.For("corp")
	require.True(t, ok, "credentials must reach the collector or no remote source can authenticate")
	assert.Equal(t, "t1", gotCred.Token)
}
```

And the collector it uses, beside the existing `fakeCollector`:

```go
// contextCapturingCollector records the CollectContext it was handed, so a
// test can assert what RunCollect actually threads through.
type contextCapturingCollector struct {
	seen *pipeline.CollectContext
}

func (c *contextCapturingCollector) Name() string { return "capture" }

func (c *contextCapturingCollector) Collect(cc pipeline.CollectContext, _ func(events.Event)) error {
	*c.seen = cc

	return nil
}
```

Add `"github.com/jcogilvie/unjira/internal/credentials"` to the test file's imports.

- [ ] **Step 2: Run it to confirm it fails**

Run: `env -u GOROOT -u GOPATH go test ./internal/pipeline/ -run TestRunCollect_PassesConfigAndCredentials -v`

Expected: FAIL to compile — `undefined: pipeline.CollectContext`, and `RunCollect` takes 4 arguments,
not 5.

- [ ] **Step 3: Add `CollectContext` and change the interface**

In `internal/pipeline/collect.go`, add `"github.com/jcogilvie/unjira/internal/credentials"` to the
imports, then replace the `Collector` interface and its doc comment:

```go
// CollectContext is everything a collector may need from its caller: the store
// for cursors, the loaded config, resolved credentials, and its own options
// block from config.Collectors[<name>].
//
// It exists as a struct rather than a parameter list because collectors differ
// in what they need — claude_code reads local files and uses only Store and
// Options, while a Jira or GitHub collector needs connection config and
// credentials — and because a future need can be added here without changing
// every implementation's signature again.
//
// Credentials are passed in rather than read from the environment here, so the
// "credentials come from the environment, never config files" rule stays
// enforced at one place (cmd/unjira) instead of in every collector.
type CollectContext struct {
	Store       *store.Store
	Config      config.Config
	Credentials credentials.Set
	// Options is this collector's own block from config.Collectors[<name>],
	// including the "enabled" key that got it selected.
	Options map[string]any
}

// Collector reads its source since the last cursor, emits normalized
// events via visit, and advances its cursor via the store. Collectors must
// be deterministic and idempotent: re-running one is always safe because
// (source, external_id) dedupes at insert.
type Collector interface {
	Name() string
	Collect(cc CollectContext, visit func(events.Event)) error
}
```

- [ ] **Step 4: Thread it through `RunCollect`**

Change `RunCollect`'s signature and the call inside it. Add the `creds` parameter last, so existing
positional arguments keep their meaning:

```go
func RunCollect(
	cfg config.Config,
	s *store.Store,
	registry map[string]func() Collector,
	linkExclusions []*regexp.Regexp,
	creds credentials.Set,
) (map[string]int, error) {
```

and inside the loop, replace the `collector.Collect(s, options, func(...))` call with:

```go
		cc := CollectContext{
			Store:       s,
			Config:      cfg,
			Credentials: creds,
			Options:     options,
		}

		err := collector.Collect(cc, func(event events.Event) {
```

Leave the rest of the loop body — the `collectErr` capture, `annotateExcludedTicketKeys`,
`InsertEvent`, and the counting — exactly as it is. Also add a line to `RunCollect`'s doc comment
noting that `creds` is passed to every collector via `CollectContext`.

- [ ] **Step 5: Update `fakeCollector`**

In `internal/pipeline/collect_test.go`, change `fakeCollector.Collect` to the new signature. This is
the one forced test edit; do not change what any existing test asserts:

```go
func (f *fakeCollector) Collect(_ pipeline.CollectContext, visit func(events.Event)) error {
```

Every existing `RunCollect` call site in this file gains a final argument. Use `credentials.Set{}`
(the zero value, which reports every lookup missing) where the test does not care about credentials:

```go
	results, err := pipeline.RunCollect(cfg, s, registry, nil, credentials.Set{})
```

- [ ] **Step 6: Run the package**

Run: `env -u GOROOT -u GOPATH go test ./internal/pipeline/ -v`

Expected: the new test PASSES, and **every pre-existing collect/digest/narrate test still passes with
its assertions unchanged**. If a pre-existing assertion now fails, the refactor changed behavior —
investigate rather than adjusting the test.

- [ ] **Step 7: Commit**

Note `internal/collector/claudecode` and `cmd/unjira` do not compile yet; Tasks 3 and 4 fix them.
Commit anyway so the interface change is its own reviewable step:

```bash
env -u GOROOT -u GOPATH gofmt -w internal/pipeline/
git add internal/pipeline/
git commit -m "Add pipeline.CollectContext so collectors can reach config and credentials"
```

---

## Task 3: `claudecode` adopts the new signature

**Files:**
- Modify: `internal/collector/claudecode/claudecode.go`

`claudecode` needs nothing new — it reads local files. This task is purely mechanical, and that is the
point: the seam must not burden a collector that doesn't use it.

- [ ] **Step 1: Change the signature and use the context**

In `internal/collector/claudecode/claudecode.go`, change `Collect`'s signature and the two things it
reads. The current first lines are:

```go
func (c *Collector) Collect(s *store.Store, options map[string]any, visit func(events.Event)) error {
	root := stringOption(options, "transcript_root", "")
```

Replace with:

```go
func (c *Collector) Collect(cc pipeline.CollectContext, visit func(events.Event)) error {
	root := stringOption(cc.Options, "transcript_root", "")
```

Then replace every remaining use of the old `s` parameter inside the function body with `cc.Store` —
`s.GetCursor(...)` becomes `cc.Store.GetCursor(...)`, `s.SetCursor(...)` becomes
`cc.Store.SetCursor(...)`. Grep for `\bs\.` within the function to catch them all.

Update the doc comment's first line to say `cc.Options["transcript_root"]` rather than
`options["transcript_root"]`.

**Import cycle check:** `internal/collector/claudecode` importing `internal/pipeline` is a new edge.
Verify `internal/pipeline` does not import any `internal/collector/...` package — it should not, since
the registry lives in `cmd/unjira`:

```bash
env -u GOROOT -u GOPATH go list -deps ./internal/pipeline | grep "internal/collector" && echo "CYCLE" || echo "no cycle"
```

Expected: `no cycle`. If it prints `CYCLE`, stop and report — `CollectContext` would need to move to
its own package (as `internal/credentials` did) rather than living in `pipeline`.

Also remove the now-unused `"github.com/jcogilvie/unjira/internal/store"` import if nothing else in the
file references it, and add `"github.com/jcogilvie/unjira/internal/pipeline"`.

- [ ] **Step 2: Run the package**

Run: `env -u GOROOT -u GOPATH go test ./internal/collector/claudecode/ -v`

Expected: PASS, every test unchanged. This collector's behavior is identical; only how it receives its
inputs changed.

Note: if `claudecode`'s tests call `Collect` directly, they need the new argument shape —
`Collect(pipeline.CollectContext{Store: s, Options: opts}, visit)`. That is a forced signature edit,
not a behavior change.

- [ ] **Step 3: Commit**

```bash
env -u GOROOT -u GOPATH gofmt -w internal/collector/claudecode/
env -u GOROOT -u GOPATH go vet ./internal/collector/claudecode/...
git add internal/collector/claudecode/
git commit -m "claudecode: take CollectContext"
```

---

## Task 4: `cmd/unjira` supplies credentials

**Files:**
- Modify: `cmd/unjira/main.go`
- Modify: `cmd/unjira/main_test.go`

- [ ] **Step 1: Re-point the credential types at the shared package**

In `cmd/unjira/main.go`, **delete** the local `jiraCredential` struct (currently at ~line 56) and
change `JiraCredentials` to wrap `credentials.Set`. Keep Kong's `UnmarshalJSON` here — parsing an env
var is `cmd`'s job:

```go
// JiraCredentials maps a config.JiraConnection.Name to its credential,
// decoded from a single JSON-object env var (UNJIRA_JIRA_CREDENTIALS), e.g.:
//
//	UNJIRA_JIRA_CREDENTIALS='{"corp":{"email":"a@x.com","token":"..."},"paas":{"email":"b@x.com","token":"..."}}'
//
// One var per credential kind, not one pair per connection — this scales to
// any number of configured connections without a new env var per one. Must
// be a named struct wrapping the map (not a bare map[string]T): Kong checks
// for a json.Unmarshaler implementation by type before falling back to its
// own built-in map decoder (which expects key=value;key2=value2 syntax, not
// JSON), and a bare map type never satisfies json.Unmarshaler.
type JiraCredentials struct {
	set credentials.Set
}

// UnmarshalJSON implements json.Unmarshaler so Kong decodes this type from
// its env var automatically.
func (c *JiraCredentials) UnmarshalJSON(data []byte) error {
	var byName map[string]credentials.Credential
	if err := json.Unmarshal(data, &byName); err != nil {
		return err
	}
	c.set = credentials.NewSet(byName)

	return nil
}

// Set returns the parsed credentials for passing to collectors.
func (c JiraCredentials) Set() credentials.Set {
	return c.set
}
```

Add `"github.com/jcogilvie/unjira/internal/credentials"` to the imports.

- [ ] **Step 2: Fix `jiraClientForProject`**

It currently reads `a.jiraCredentials.byName[conn.Name]`. Change it to use the `For` lookup, keeping
the same error text so no test's expectation changes:

```go
	creds, ok := a.jiraCredentials.set.For(conn.Name)
	if !ok {
		return nil, fmt.Errorf(
			"no credentials for jira connection %q in UNJIRA_JIRA_CREDENTIALS", conn.Name,
		)
	}

	return jira.New(conn.Site, creds.Email, creds.Token)
```

- [ ] **Step 3: Pass credentials to both `RunCollect` call sites**

There are two (currently ~line 166 in `collectCmd.Run` and ~line 362 in `devNarrateCmd.Run`). Both
gain a final argument:

```go
	results, err := pipeline.RunCollect(app.config, app.store, registry, linkExclusions, app.jiraCredentials.Set())
```

```go
	if _, err := pipeline.RunCollect(app.config, app.store, registry, linkExclusions, app.jiraCredentials.Set()); err != nil {
```

- [ ] **Step 4: Update `main_test.go`'s constructions**

`cmd/unjira/main_test.go` builds credentials directly at ~lines 24, 43, 69, and 98 via
`JiraCredentials{byName: map[string]jiraCredential{...}}`. Change each to the new shape, preserving
the same values so every assertion stays as-is:

```go
		jiraCredentials: JiraCredentials{set: credentials.NewSet(map[string]credentials.Credential{
			"corp": {Email: "a@x.com", Token: "t1"},
		})},
```

and the empty case:

```go
		jiraCredentials: JiraCredentials{set: credentials.NewSet(map[string]credentials.Credential{})},
```

`TestJiraCredentials_UnmarshalsFromJSON` (~line 68) reads `creds.byName["corp"].Email` directly
(verified). Rewrite those three assertions to go through the accessor, asserting the same facts:

```go
	corp, ok := creds.Set().For("corp")
	require.True(t, ok)
	assert.Equal(t, "a@x.com", corp.Email)
	assert.Equal(t, "tok1", corp.Token)

	paas, ok := creds.Set().For("paas")
	require.True(t, ok)
	assert.Equal(t, "b@x.com", paas.Email)
```

Add the `credentials` import to the file.

- [ ] **Step 5: Run the package**

Run: `env -u GOROOT -u GOPATH go test ./cmd/... -v`

Expected: PASS, with every assertion unchanged in intent.

- [ ] **Step 6: Commit**

```bash
env -u GOROOT -u GOPATH gofmt -w cmd/unjira/
env -u GOROOT -u GOPATH go vet ./cmd/...
git add cmd/unjira/
git commit -m "cmd/unjira: supply credentials to collectors via CollectContext"
```

---

## Task 5: full verification

**Files:** none (verification only)

- [ ] **Step 1: whole-repo build and tests**

```bash
env -u GOROOT -u GOPATH go build ./...
env -u GOROOT -u GOPATH go test ./... -count=1
```

Expected: build clean; every package `ok`. `internal/credentials` and `internal/llm` report
`[no test files]` for `internal/llm` only — `credentials` has tests.

- [ ] **Step 2: vet and lint**

```bash
env -u GOROOT -u GOPATH go vet ./...
export PATH="$HOME/.asdf/installs/golang/1.25.7/bin:$PATH"
env -u GOROOT -u GOPATH golangci-lint run ./...
```

Expected: vet clean, lint `0 issues`. Watch for: missing doc comments on the new exported identifiers
(`credentials.Credential`, `credentials.Set`, `credentials.NewSet`, `Set.For`,
`pipeline.CollectContext`, `JiraCredentials.Set`); `gci` import grouping in the new files.

- [ ] **Step 3: confirm no import cycle and no layering violation**

```bash
env -u GOROOT -u GOPATH go list -deps ./internal/credentials | grep "jcogilvie/unjira" || echo "OK: credentials has no internal deps"
env -u GOROOT -u GOPATH go list -deps ./internal/pipeline | grep "internal/collector" && echo "CYCLE" || echo "OK: pipeline does not import collectors"
```

Expected: both `OK` lines. The first keeps `credentials` a leaf (it must not pull in `config`); the
second is what makes `claudecode` importing `pipeline` safe.

- [ ] **Step 4: prove the refactor preserved behavior**

The strongest available evidence is that no pre-existing test's assertions changed. Verify it from the
diff rather than from memory:

```bash
git diff main..HEAD -- '*_test.go' | grep -E "^[-+].*(assert\.|require\.)" | sort | uniq -c | sort -rn | head -20
```

Every `-` line should have a matching `+` line differing only in the credential-construction syntax or
the `Collect`/`RunCollect` argument shape. **If any assertion's expected value changed, that is a
behavior change and must be explained or reverted.** Report what this shows.

- [ ] **Step 5: the authoritative gate**

```bash
earthly +reviewable
```

Expected: `SUCCESS`. This runs lint and tests in a clean container with its own Go 1.26 toolchain, and
is the check that matters — a local pass with a container failure means something is
environment-dependent.

- [ ] **Step 6: commit if any step required fixes**

```bash
git add -A
git commit -m "Fix lint findings in the collector-seam refactor"
```

---

## Verification checklist (spec coverage)

This plan has no design spec of its own — it is the enabling refactor named in
`docs/superpowers/specs/2026-08-21-jira-collector-design.md`. Coverage against what that spec needs
from the seam:

- [x] A collector can read `config.Jira` connections (sites, project keys) — `CollectContext.Config`,
  Task 2.
- [x] A collector can resolve per-connection credentials — `CollectContext.Credentials`, Tasks 1-2.
- [x] Credentials still come only from the environment, parsed in one place — Kong binding stays in
  `cmd/unjira`, Task 4.
- [x] A collector needing none of it is unburdened — `claudecode` reads only `Store` and `Options`,
  Task 3.
- [x] No behavior change — Task 5 Step 4 checks this from the diff, not from assertion.

Not in scope: the Jira collector itself, `GetComments`, `config.JiraConnection.Queries`, and the
registry entry. Those are the next plan, written against this seam.
