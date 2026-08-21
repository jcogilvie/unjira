# Jira Collector Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Pull Jira status changes, comments, and ticket-text edits into the event log, so the
correlator can see what the org believes happened alongside what the transcripts show.

**Architecture:** A `pipeline.Collector` in `internal/collector/jira`, reading through the
`CollectContext` seam that just landed. Per configured connection, per named JQL query: search issues
(auto-scoped to the connection's `project_keys`), then fetch each issue's changelog and comments,
emitting four event kinds. Cursors are per query, keyed by a hash of the effective JQL.

**Tech Stack:** Go 1.26, `github.com/stretchr/testify`, `net/http/httptest`, `modernc.org/sqlite`.

**Spec:** `docs/superpowers/specs/2026-08-21-jira-collector-design.md` — authoritative where this plan
and it disagree. **This plan's code is a draft you are responsible for**: plans in this repo have
shipped real bugs when transcribed verbatim (an `err == sql.ErrNoRows` that fails on wrapped errors; a
`time.RFC3339` format that silently truncated a sub-second TTL; an assertion that compared a struct to
itself and so could not catch the bug it was written for). If something here is wrong, fix it and say
so in your report.

---

## Critical environment notes (every task)

**Stale `GOROOT`/`GOPATH`.** Prefix EVERY `go`/`gofmt` invocation with `env -u GOROOT -u GOPATH`.
Without it you get spurious `compile: version "X" does not match go tool version "Y"` errors unrelated
to your change.

**`golangci-lint`:**

```bash
export PATH="$HOME/.asdf/installs/golang/1.25.7/bin:$PATH"
env -u GOROOT -u GOPATH golangci-lint run ./...
```

Keep local golangci-lint current with what CI installs (`@latest`). **The branch is at `0 issues` and
must stay there.** Ignore `gomodguard is deprecated` warnings.

**The authoritative gate is `earthly +reviewable`** — lint plus tests in a clean container with its own
Go 1.26 toolchain. A local pass with a container failure means something is environment-dependent.

**Work in the worktree** `/Users/jonathan.ogilvie/workspace/unjira/.claude/worktrees/slice4-reconciler`
(branch `worktree-slice4-reconciler`). Run all commands from there. Do not create or switch branches.
Do not touch the session task list — the coordinator owns it.

**Repo conventions** (`CLAUDE.md`, `docs/go-conventions.md`): TDD — failing test first, confirm it
fails for the right reason, then implement. Collectors are dumb and deterministic: no LLM, no judgment,
no network beyond their own source. Never silently drop data; error loudly with
`fmt.Errorf("...: %w", err)` naming the relevant id. testify `require` (fatal) / `assert` (non-fatal).
Doc comments on every exported identifier, explaining *why*.

**Proving a regression test works.** For any test guarding a specific defect, temporarily break the
guarded behaviour, confirm the test fails, restore, confirm it passes. Paste both outputs. This has
caught four tests in this project that looked like coverage and were not.

---

## What already exists (verified — do not re-derive)

```go
// internal/pipeline — the seam, landed in PR #9
type CollectContext struct {
    Store       *store.Store
    Config      config.Config
    Credentials credentials.Set
    Options     map[string]any
}
type Collector interface {
    Name() string
    Collect(cc CollectContext, visit func(events.Event)) error
}

// internal/credentials
type Credential struct { Email, Token string }
func (s Set) For(connectionName string) (Credential, bool)

// internal/clients/jira
func New(site, email, token string) (*Client, error)
func (c *Client) Myself() (map[string]any, error)
func (c *Client) SearchIssues(jql string, fields []string, limit int, visit func(map[string]any)) error
func (c *Client) GetChangelog(key string) ([]map[string]any, error)   // paginated, oldest first
func (c *Client) AddComment(key, text string) (map[string]any, error) // write only; no read yet

// internal/config
type JiraConnection struct {
    Name        string   `json:"name"`
    Site        string   `json:"site"`
    ProjectKeys []string `json:"project_keys"`
}

// internal/store
func (s *Store) GetCursor(collector, resource string) (string, error)  // "" when absent
func (s *Store) SetCursor(collector, resource, position string) error

// internal/events
func NewEvent(source, externalID string, occurredAt time.Time, summary string) Event
// Event{Source, ExternalID, OccurredAt, Actor, Summary, Artifacts map[string]any, RawRef}
```

**A changelog entry's JSON shape** (from how `StatusChanges` already parses it): each entry has `id`,
`created`, `author`, and `items` (a `[]any` of maps with `field`, `fromString`, `toString`).

---

## File structure

- **Modify:** `internal/clients/jira/jira.go` — add `GetComments`.
- **Modify:** `internal/clients/jira/jira_test.go` — `GetComments` pagination + shape.
- **Modify:** `internal/config/config.go` — `JiraQuery`, `JiraConnection.Queries`,
  `JiraConnection.MaxIssuesPerQuery`, and an `EffectiveJQL` helper.
- **Modify:** `internal/config/config_test.go` — parsing, auto-scoping, validation.
- **Create:** `internal/collector/jira/jira.go` — the collector: `Name`, `New`, `Collect`, and the
  per-query loop.
- **Create:** `internal/collector/jira/events.go` — pure functions turning changelog entries and
  comments into `events.Event`. Separated so they are table-testable with no HTTP and no store.
- **Create:** `internal/collector/jira/cursor.go` — the `<hash>:<watermark>` position format.
- **Create:** `internal/collector/jira/jira_test.go` — collector tests (httptest + real SQLite).
- **Create:** `internal/collector/jira/events_test.go` — event-shaping table tests.
- **Create:** `internal/collector/jira/cursor_test.go` — position encode/decode, hash invalidation.
- **Modify:** `cmd/unjira/main.go` — registry entry.
- **Modify:** `config/unjira.example.json` — a `queries` block.
- **Modify:** `internal/live/jira_test.go` — a live-tier collector test.

Rationale for splitting the package into three files: event shaping and cursor encoding are pure
functions with no I/O, and keeping them separate is what lets their tests run without an HTTP server or
a database. `jira.go` holds the part that genuinely needs both.

Sequencing: `GetComments` (Task 1) and config (Task 2) are independent and could run in parallel. Event
shaping (Task 3) and cursors (Task 4) are pure and depend only on Task 2's types. The collector
(Task 5) needs all four. Wiring (Task 6), then verification (Task 7), then the live test (Task 8).

---

## Task 1: `jira.Client.GetComments`

**Files:**
- Modify: `internal/clients/jira/jira.go`
- Modify: `internal/clients/jira/jira_test.go`

The client can post comments but not read them. Without a read, the loop cannot close for phase 1's
primary action type: unjira posts a comment, the next pass sees no evidence of it, and proposes it
again.

- [ ] **Step 1: Write the failing test**

Add to `internal/clients/jira/jira_test.go`:

```go
func TestGetComments_WalksEveryPage(t *testing.T) {
	var calls int
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("startAt") == "" || r.URL.Query().Get("startAt") == "0" {
			writeJSON(t, w, http.StatusOK, map[string]any{
				"comments": []map[string]any{
					{"id": "101", "created": "2026-08-01T12:00:00.000+0000",
						"body": "first", "author": map[string]any{"accountId": "acct-1", "displayName": "Alice"}},
				},
				"startAt":    0,
				"maxResults": 1,
				"total":      2,
			})

			return
		}
		writeJSON(t, w, http.StatusOK, map[string]any{
			"comments": []map[string]any{
				{"id": "102", "created": "2026-08-01T13:00:00.000+0000",
					"body": "second", "author": map[string]any{"accountId": "acct-2", "displayName": "Bob"}},
			},
			"startAt":    1,
			"maxResults": 1,
			"total":      2,
		})
	})

	comments, err := client.GetComments("PROJ-42")

	require.NoError(t, err)
	require.Len(t, comments, 2, "pagination must not stop after the first page")
	assert.Equal(t, "101", comments[0]["id"])
	assert.Equal(t, "102", comments[1]["id"])
	assert.Equal(t, 2, calls)
}

func TestGetComments_NoCommentsReturnsEmptyNotError(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{
			"comments": []map[string]any{}, "startAt": 0, "maxResults": 50, "total": 0,
		})
	})

	comments, err := client.GetComments("PROJ-42")

	require.NoError(t, err, "an issue with no comments is normal, not an error")
	assert.Empty(t, comments)
}
```

- [ ] **Step 2: Run it to confirm it fails**

Run: `env -u GOROOT -u GOPATH go test ./internal/clients/jira/ -run TestGetComments -v`

Expected: FAIL to compile — `client.GetComments undefined`.

- [ ] **Step 3: Implement `GetComments`**

Add to `internal/clients/jira/jira.go`, beside `GetChangelog`:

```go
// commentPage is the raw shape of one comment-list response page.
type commentPage struct {
	Comments   []map[string]any `json:"comments"`
	StartAt    int              `json:"startAt"`
	MaxResults int              `json:"maxResults"`
	Total      int              `json:"total"`
}

// GetComments returns every comment on an issue, oldest first.
//
// Comments do not appear in an issue's changelog, so this is the only way to
// know whether an issue has already been narrated — which is what stops the
// reconciler proposing a comment unjira (or a human) already posted. See
// docs/superpowers/specs/2026-08-21-jira-collector-design.md.
//
// Returns the raw maps rather than a typed struct, matching GetChangelog: the
// collector picks out the fields it needs, and a thin facade should not decide
// which of a comment's fields matter.
func (c *Client) GetComments(key string) ([]map[string]any, error) {
	var comments []map[string]any
	start := 0

	for {
		path := fmt.Sprintf("rest/api/2/issue/%s/comment?startAt=%d&maxResults=100", key, start)

		var page commentPage
		if err := c.do(http.MethodGet, path, nil, &page); err != nil {
			return nil, err
		}

		comments = append(comments, page.Comments...)

		// Advance by what the server actually returned, not by maxResults: a
		// server returning fewer than requested would otherwise make this skip
		// comments. Stop when a page is empty or we have them all.
		if len(page.Comments) == 0 || len(comments) >= page.Total {
			return comments, nil
		}
		start += len(page.Comments)
	}
}
```

Note the termination condition differs from `GetChangelog`'s (`isLast`) because the comment endpoint
reports `total` instead. Confirm against the fixtures that this terminates — an off-by-one here is an
infinite loop against a live server.

- [ ] **Step 4: Run to confirm it passes**

Run: `env -u GOROOT -u GOPATH go test ./internal/clients/jira/ -v`

Expected: both new tests PASS, every pre-existing client test still passes.

- [ ] **Step 5: Prove the pagination test discriminates**

Temporarily change the loop to `return page.Comments, nil` after the first fetch (no pagination).
Confirm `TestGetComments_WalksEveryPage` FAILS on the length assertion, then restore and confirm it
passes. Paste both outputs — a pagination test whose fixture only ever serves one page proves nothing.

- [ ] **Step 6: Commit**

```bash
env -u GOROOT -u GOPATH gofmt -w internal/clients/jira/
env -u GOROOT -u GOPATH go vet ./internal/clients/jira/...
git add internal/clients/jira/
git commit -m "Add jira.Client.GetComments: comments are absent from the changelog"
```

---

## Task 2: config — named queries, auto-scoping, validation

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `config/unjira.example.json`

- [ ] **Step 1: Write the failing tests**

Add to `internal/config/config_test.go`:

```go
func TestJiraConnection_EffectiveJQLScopesToProjectKeys(t *testing.T) {
	conn := config.JiraConnection{
		Name:        "corp",
		Site:        "https://corp.atlassian.net",
		ProjectKeys: []string{"PROJ", "OPS"},
		Queries: []config.JiraQuery{
			{Name: "mine", JQL: "assignee = currentUser()"},
		},
	}

	got, err := conn.EffectiveJQL(conn.Queries[0])

	require.NoError(t, err)
	assert.Equal(t, `(assignee = currentUser()) AND project IN ("PROJ", "OPS")`, got,
		"collection must be bounded to projects the connection can also write to")
}

func TestJiraConnection_EffectiveJQLErrorsWithoutProjectKeys(t *testing.T) {
	conn := config.JiraConnection{
		Name:    "corp",
		Queries: []config.JiraQuery{{Name: "mine", JQL: "assignee = currentUser()"}},
	}

	_, err := conn.EffectiveJQL(conn.Queries[0])

	require.Error(t, err, "an unscoped query would collect issues no connection can write to")
	assert.Contains(t, err.Error(), "corp")
	assert.Contains(t, err.Error(), "project_keys")
}

func TestJiraConnection_MaxIssuesDefaultsAndRejectsNegative(t *testing.T) {
	tests := []struct {
		name       string
		configured int
		want       int
		wantErr    bool
	}{
		{name: "absent defaults", configured: 0, want: 200},
		{name: "explicit is honored", configured: 50, want: 50},
		{name: "negative is an error", configured: -1, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn := config.JiraConnection{Name: "corp", MaxIssuesPerQuery: tt.configured}

			got, err := conn.IssueLimit()

			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "max_issues_per_query")

				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestLoad_ParsesJiraQueries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unjira.config.json")
	body := `{"jira":[{"name":"corp","site":"https://corp.atlassian.net",
	  "project_keys":["PROJ"],"max_issues_per_query":50,
	  "queries":[{"name":"mine","jql":"assignee = currentUser()"}]}]}`
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	cfg, err := config.Load(path)

	require.NoError(t, err)
	require.Len(t, cfg.Jira, 1)
	require.Len(t, cfg.Jira[0].Queries, 1)
	assert.Equal(t, "mine", cfg.Jira[0].Queries[0].Name)
	assert.Equal(t, "assignee = currentUser()", cfg.Jira[0].Queries[0].JQL)
	assert.Equal(t, 50, cfg.Jira[0].MaxIssuesPerQuery)
}
```

- [ ] **Step 2: Run to confirm it fails**

Run: `env -u GOROOT -u GOPATH go test ./internal/config/ -run 'TestJiraConnection|TestLoad_ParsesJiraQueries' -v`

Expected: FAIL to compile — `undefined: config.JiraQuery`, `conn.EffectiveJQL undefined`,
`conn.IssueLimit undefined`.

- [ ] **Step 3: Implement**

In `internal/config/config.go`, add the query type and extend `JiraConnection`:

```go
// DefaultMaxIssuesPerQuery bounds how many issues one collector query examines
// per pass. A limit is required rather than optional: the Jira collector makes
// two API calls per issue (changelog and comments, since the search endpoint
// cannot expand either), so an unbounded query is unbounded API cost.
const DefaultMaxIssuesPerQuery = 200

// JiraQuery is one named JQL view of a connection's issues. Named rather than a
// single string per connection because one Jira site legitimately has several
// views worth collecting, and duplicating site plus credentials to get them
// would be the wrong shape.
//
// The name is also the cursor key (see internal/collector/jira), so renaming a
// query resets only that query's watermark.
type JiraQuery struct {
	Name string `json:"name"`
	JQL  string `json:"jql"`
}

// JiraConnection is one Jira site unjira reads from and writes to. Site is the
// base URL; ProjectKeys routes a project key to this connection (for writes)
// and bounds what its queries may collect (for reads).
type JiraConnection struct {
	Name        string   `json:"name"`
	Site        string   `json:"site"`
	ProjectKeys []string `json:"project_keys"`
	// Queries are the named JQL views the Jira collector reads. Empty means
	// this connection is write-only: it routes project keys but collects
	// nothing.
	Queries []JiraQuery `json:"queries"`
	// MaxIssuesPerQuery bounds one query's issue count per pass. Zero means
	// DefaultMaxIssuesPerQuery.
	MaxIssuesPerQuery int `json:"max_issues_per_query"`
}

// EffectiveJQL returns query's JQL scoped to this connection's ProjectKeys.
//
// The scope is added rather than left to the operator because the two lists
// answer different questions that must not disagree: ProjectKeys says which
// projects this connection can write to, and an unscoped JQL (assignee =
// currentUser(), say) spans a whole site. Collecting an issue from a project no
// connection covers would narrate work the reconciler can never act on —
// JiraConnectionForProject would return false when it came time to write.
//
// Empty ProjectKeys is an error rather than "collect everything", for the same
// reason.
func (c JiraConnection) EffectiveJQL(query JiraQuery) (string, error) {
	if len(c.ProjectKeys) == 0 {
		return "", fmt.Errorf(
			"jira connection %q has no project_keys: cannot scope collector query %q, and an "+
				"unscoped query would collect issues no connection can write to",
			c.Name, query.Name,
		)
	}

	quoted := make([]string, 0, len(c.ProjectKeys))
	for _, key := range c.ProjectKeys {
		quoted = append(quoted, fmt.Sprintf("%q", key))
	}

	return fmt.Sprintf("(%s) AND project IN (%s)", query.JQL, strings.Join(quoted, ", ")), nil
}

// IssueLimit returns the per-query issue cap, defaulting when unset. A negative
// value is a configuration error rather than silently coerced: it most likely
// means someone intended "no limit", which this collector deliberately does not
// offer.
func (c JiraConnection) IssueLimit() (int, error) {
	switch {
	case c.MaxIssuesPerQuery < 0:
		return 0, fmt.Errorf(
			"jira connection %q has max_issues_per_query %d: must be positive, or omitted for the default of %d",
			c.Name, c.MaxIssuesPerQuery, DefaultMaxIssuesPerQuery,
		)
	case c.MaxIssuesPerQuery == 0:
		return DefaultMaxIssuesPerQuery, nil
	default:
		return c.MaxIssuesPerQuery, nil
	}
}
```

Add `"strings"` to the imports if absent.

- [ ] **Step 4: Add the example config block**

In `config/unjira.example.json`, add to the first `jira` connection object, after `project_keys`:

```json
      "max_issues_per_query": 200,
      "queries": [
        { "name": "mine", "jql": "assignee = currentUser() AND updated >= -14d" }
      ],
```

- [ ] **Step 5: Run and verify the example parses**

```bash
env -u GOROOT -u GOPATH go test ./internal/config/ -v
python3 -c "import json; json.load(open('config/unjira.example.json'))" && echo VALID
```

Expected: all config tests pass (including pre-existing ones, unchanged); `VALID`.

- [ ] **Step 6: Commit**

```bash
env -u GOROOT -u GOPATH gofmt -w internal/config/
env -u GOROOT -u GOPATH go vet ./internal/config/...
git add internal/config/ config/unjira.example.json
git commit -m "Add config.JiraQuery: named queries scoped to a connection's project keys"
```

---

## Task 3: event shaping (pure functions)

**Files:**
- Create: `internal/collector/jira/events.go`
- Create: `internal/collector/jira/events_test.go`

This task builds only the pure translation from Jira JSON to `events.Event`. No HTTP, no store. The
tests are table-driven over literal maps, which is what makes the four event kinds and the
skipped-fields rule cheap to assert exhaustively.

- [ ] **Step 1: Write the failing tests**

Create `internal/collector/jira/events_test.go`:

```go
package jira_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	collectorjira "github.com/jcogilvie/unjira/internal/collector/jira"
)

// issueContext is rebuilt per test rather than shared, so one test mutating a
// map cannot affect another.
func testIssue() collectorjira.IssueContext {
	return collectorjira.IssueContext{
		Key:           "PROJ-42",
		ProjectKey:    "PROJ",
		Connection:    "corp",
		Site:          "https://corp.atlassian.net",
		SelfAccountID: "acct-unjira",
	}
}

func TestEventsFromChangelogEntry_EmitsTrackedFields(t *testing.T) {
	tests := []struct {
		name        string
		field       string
		toString    string
		wantEvents  int
		wantExtID   string
		wantSummary string
	}{
		{
			name: "status", field: "status", toString: "In Progress", wantEvents: 1,
			wantExtID: "PROJ-42:status:10001", wantSummary: "PROJ-42 status: To Do → In Progress",
		},
		{
			name: "description", field: "description", toString: "investigate the flaky test",
			wantEvents: 1, wantExtID: "PROJ-42:description:10001",
			wantSummary: "PROJ-42 description: investigate the flaky test",
		},
		{
			name: "summary", field: "summary", toString: "Fix flaky correlator test",
			wantEvents: 1, wantExtID: "PROJ-42:summary:10001",
			wantSummary: "PROJ-42 summary: Fix flaky correlator test",
		},
		{name: "assignee is not tracked", field: "assignee", toString: "Bob", wantEvents: 0},
		{name: "priority is not tracked", field: "priority", toString: "High", wantEvents: 0},
		{name: "labels are not tracked", field: "labels", toString: "backend", wantEvents: 0},
		{name: "sprint is not tracked", field: "Sprint", toString: "Sprint 9", wantEvents: 0},
		{name: "resolution is not tracked", field: "resolution", toString: "Done", wantEvents: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry := map[string]any{
				"id":      "10001",
				"created": "2026-08-20T14:30:00.000+0000",
				"author":  map[string]any{"accountId": "acct-alice", "displayName": "Alice"},
				"items": []any{
					map[string]any{"field": tt.field, "fromString": "To Do", "toString": tt.toString},
				},
			}

			got, err := collectorjira.EventsFromChangelogEntry(testIssue(), entry)

			require.NoError(t, err)
			require.Len(t, got, tt.wantEvents,
				"only status/description/summary are actionable in phase 1")
			if tt.wantEvents == 0 {
				return
			}
			assert.Equal(t, "jira", got[0].Source)
			assert.Equal(t, tt.wantExtID, got[0].ExternalID)
			assert.Equal(t, tt.wantSummary, got[0].Summary)
			assert.Equal(t, "Alice", got[0].Actor)
			assert.Equal(t, "https://corp.atlassian.net/browse/PROJ-42", got[0].RawRef)
			assert.Equal(t,
				time.Date(2026, 8, 20, 14, 30, 0, 0, time.UTC),
				got[0].OccurredAt.UTC(),
				"OccurredAt is when the change happened, not when we collected it")
			assert.Equal(t, "PROJ-42", got[0].Artifacts["issue_key"])
			assert.Equal(t, "PROJ", got[0].Artifacts["project_key"])
			assert.Equal(t, "corp", got[0].Artifacts["connection"])
			assert.Equal(t, tt.field, got[0].Artifacts["field"])
			assert.Equal(t, false, got[0].Artifacts["authored_by_unjira"])
		})
	}
}

func TestEventsFromChangelogEntry_MultipleItemsInOneEntry(t *testing.T) {
	// Jira batches simultaneous field changes into one changelog entry. Both
	// tracked fields must be emitted, and their ExternalIDs must differ or the
	// second silently dedupes away against the first.
	entry := map[string]any{
		"id":      "10007",
		"created": "2026-08-20T14:30:00.000+0000",
		"author":  map[string]any{"accountId": "acct-alice", "displayName": "Alice"},
		"items": []any{
			map[string]any{"field": "status", "fromString": "To Do", "toString": "Done"},
			map[string]any{"field": "assignee", "fromString": "", "toString": "Bob"},
			map[string]any{"field": "summary", "fromString": "old", "toString": "new"},
		},
	}

	got, err := collectorjira.EventsFromChangelogEntry(testIssue(), entry)

	require.NoError(t, err)
	require.Len(t, got, 2, "status and summary tracked, assignee skipped")
	ids := []string{got[0].ExternalID, got[1].ExternalID}
	assert.ElementsMatch(t, []string{"PROJ-42:status:10007", "PROJ-42:summary:10007"}, ids,
		"the field must be in the ExternalID or co-occurring changes collide")
}

func TestEventsFromChangelogEntry_SelfAuthoredIsTaggedNotDropped(t *testing.T) {
	entry := map[string]any{
		"id":      "10002",
		"created": "2026-08-20T14:30:00.000+0000",
		"author":  map[string]any{"accountId": "acct-unjira", "displayName": "unjira"},
		"items": []any{
			map[string]any{"field": "status", "fromString": "To Do", "toString": "Done"},
		},
	}

	got, err := collectorjira.EventsFromChangelogEntry(testIssue(), entry)

	require.NoError(t, err)
	require.Len(t, got, 1,
		"our own changes are evidence a drift was closed; dropping them makes the loop propose it again")
	assert.Equal(t, true, got[0].Artifacts["authored_by_unjira"])
}

func TestEventsFromChangelogEntry_MalformedEntryErrorsNaming(t *testing.T) {
	tests := []struct {
		name    string
		entry   map[string]any
		wantMsg string
	}{
		{
			name:    "missing id",
			entry:   map[string]any{"created": "2026-08-20T14:30:00.000+0000", "items": []any{}},
			wantMsg: "id",
		},
		{
			name:    "unparseable created",
			entry:   map[string]any{"id": "1", "created": "yesterday", "items": []any{}},
			wantMsg: "created",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := collectorjira.EventsFromChangelogEntry(testIssue(), tt.entry)

			require.Error(t, err, "a shape we cannot read must be loud, not skipped")
			assert.Contains(t, err.Error(), tt.wantMsg)
			assert.Contains(t, err.Error(), "PROJ-42", "the error must name the issue")
		})
	}
}

func TestEventFromComment_ShapeAndFullText(t *testing.T) {
	long := ""
	for range 3000 {
		long += "x"
	}
	comment := map[string]any{
		"id":      "9001",
		"created": "2026-08-20T15:00:00.000+0000",
		"body":    long,
		"author":  map[string]any{"accountId": "acct-alice", "displayName": "Alice"},
	}

	got, err := collectorjira.EventFromComment(testIssue(), comment)

	require.NoError(t, err)
	assert.Equal(t, "PROJ-42:comment:9001", got.ExternalID)
	assert.Equal(t, "Alice", got.Actor)
	assert.Contains(t, got.Summary, long,
		"comment text is carried in full; truncation would discard the richest statement of intent")
	assert.Equal(t, false, got.Artifacts["authored_by_unjira"])
	assert.NotContains(t, got.Artifacts, "field", "comments are not a changelog field")
}

func TestEventFromComment_SelfAuthoredIsTagged(t *testing.T) {
	comment := map[string]any{
		"id":      "9002",
		"created": "2026-08-20T15:00:00.000+0000",
		"body":    "Narrated by unjira.",
		"author":  map[string]any{"accountId": "acct-unjira", "displayName": "unjira"},
	}

	got, err := collectorjira.EventFromComment(testIssue(), comment)

	require.NoError(t, err)
	assert.Equal(t, true, got.Artifacts["authored_by_unjira"],
		"this tag is what stops the reconciler proposing a comment we already posted")
}
```

- [ ] **Step 2: Run to confirm it fails**

Run: `env -u GOROOT -u GOPATH go test ./internal/collector/jira/ -v`

Expected: build failure — no such package.

- [ ] **Step 3: Implement**

Create `internal/collector/jira/events.go`:

```go
// Package jira collects Jira changes into the event log, so the correlator can
// see what the org believes happened alongside what the transcripts show.
//
// It emits four event kinds — status transitions, comments, and description and
// summary edits. Every other changelog field is deliberately skipped: phase 1
// can only propose comments and forward transitions, so an event about a label
// or sprint change is something the correlator must reason about and can never
// act on. A groomed backlog generates constant churn in those fields, and
// emitting it would bury the signal. See
// docs/superpowers/specs/2026-08-21-jira-collector-design.md.
package jira

import (
	"fmt"
	"time"

	"github.com/jcogilvie/unjira/internal/events"
)

// Name is the collector's registry key and the Source on every event it emits.
const Name = "jira"

// jiraTimeFormat is Jira's changelog/comment timestamp layout: ISO 8601 with
// milliseconds and a numeric zone offset with no colon, which is why
// time.RFC3339 cannot parse it.
const jiraTimeFormat = "2006-01-02T15:04:05.000-0700"

// trackedFields are the changelog fields that become events, mapped to the
// ExternalID segment naming them. A map rather than a slice so lookup is the
// same operation as the skip decision.
//
// Jira reports custom field names with their configured display name (Sprint,
// Story Points), so an unlisted field simply falls through — which is the
// intent: this list grows when an action type needs it.
var trackedFields = map[string]string{
	"status":      "status",
	"description": "description",
	"summary":     "summary",
}

// IssueContext is the per-issue data every event from that issue carries. It is
// passed rather than re-derived so the pure event builders need no client.
type IssueContext struct {
	Key        string
	ProjectKey string
	// Connection is the configured connection name, recorded so the reconciler
	// knows which Jira site to write back to.
	Connection string
	Site       string
	// SelfAccountID is our own Jira accountId, from one Myself() call per pass.
	// Empty means "unknown", in which case nothing is tagged self-authored.
	SelfAccountID string
}

// browseURL is the human-facing link back to the issue.
func (ic IssueContext) browseURL() string {
	return fmt.Sprintf("%s/browse/%s", ic.Site, ic.Key)
}

// EventsFromChangelogEntry converts one changelog entry into zero or more
// events — zero when it touched only untracked fields, more than one when Jira
// batched several tracked changes into a single entry (it does: a transition
// that also edits the summary is one entry with two items).
//
// Returns an error rather than skipping when the entry's shape is unreadable: a
// silently dropped entry is invisible data loss, which docs/design-notes.md
// names the hardest class of bug to notice here.
func EventsFromChangelogEntry(ic IssueContext, entry map[string]any) ([]events.Event, error) {
	id, ok := entry["id"].(string)
	if !ok || id == "" {
		return nil, fmt.Errorf("changelog entry for %s has no usable id field", ic.Key)
	}

	occurredAt, err := parseJiraTime(entry["created"])
	if err != nil {
		return nil, fmt.Errorf("changelog entry %s on %s: created: %w", id, ic.Key, err)
	}

	accountID, displayName := author(entry["author"])

	items, ok := entry["items"].([]any)
	if !ok && entry["items"] != nil {
		return nil, fmt.Errorf("changelog entry %s on %s: items is %T, want a list", id, ic.Key, entry["items"])
	}

	var out []events.Event

	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("changelog entry %s on %s: item is %T, want an object", id, ic.Key, raw)
		}

		field, _ := item["field"].(string)

		segment, tracked := trackedFields[field]
		if !tracked {
			continue
		}

		from, _ := item["fromString"].(string)
		to, _ := item["toString"].(string)

		summary := fmt.Sprintf("%s %s: %s", ic.Key, field, to)
		if field == "status" {
			summary = fmt.Sprintf("%s status: %s → %s", ic.Key, from, to)
		}

		// The field is part of the ExternalID, not just the id: two tracked
		// changes in one entry share the entry id, and would dedupe against
		// each other without it.
		evt := events.NewEvent(Name, fmt.Sprintf("%s:%s:%s", ic.Key, segment, id), occurredAt, summary)
		evt.Actor = displayName
		evt.RawRef = ic.browseURL()
		evt.Artifacts["field"] = field
		ic.annotate(evt, accountID)

		out = append(out, evt)
	}

	return out, nil
}

// EventFromComment converts one comment into an event.
//
// Comments do not appear in an issue's changelog, so this is the only evidence
// that an issue has already been narrated — by unjira or by a human. Without it
// the reconciler proposes the same comment every pass.
func EventFromComment(ic IssueContext, comment map[string]any) (events.Event, error) {
	id, ok := comment["id"].(string)
	if !ok || id == "" {
		return events.Event{}, fmt.Errorf("comment on %s has no usable id field", ic.Key)
	}

	occurredAt, err := parseJiraTime(comment["created"])
	if err != nil {
		return events.Event{}, fmt.Errorf("comment %s on %s: created: %w", id, ic.Key, err)
	}

	accountID, displayName := author(comment["author"])
	body, _ := comment["body"].(string)

	// Full text, no cap. The worst case Jira permits in a comment is a small
	// fraction of one correlator prompt, and Cluster already bisects on
	// overflow — that machinery exists so events need not self-censor.
	evt := events.NewEvent(
		Name,
		fmt.Sprintf("%s:comment:%s", ic.Key, id),
		occurredAt,
		fmt.Sprintf("%s comment by %s: %s", ic.Key, displayName, body),
	)
	evt.Actor = displayName
	evt.RawRef = ic.browseURL()
	ic.annotate(evt, accountID)

	return evt, nil
}

// annotate sets the artifacts common to every event this collector emits.
//
// authored_by_unjira is data rather than an absence: our own changes are kept
// and tagged, never filtered, because "unjira commented on PROJ-42" is
// something that happened and is the positive evidence a drift was closed. The
// cost is that every consumer must honour the tag; one that ignores it
// reintroduces the feedback loop.
func (ic IssueContext) annotate(evt events.Event, authorAccountID string) {
	evt.Artifacts["issue_key"] = ic.Key
	evt.Artifacts["project_key"] = ic.ProjectKey
	evt.Artifacts["connection"] = ic.Connection
	evt.Artifacts["authored_by_unjira"] = ic.SelfAccountID != "" && authorAccountID == ic.SelfAccountID
}

// author pulls the accountId and display name out of a Jira author object.
// Both are optional in practice (deleted users, app-authored changes), so a
// missing author yields empty strings rather than an error.
func author(raw any) (accountID, displayName string) {
	obj, ok := raw.(map[string]any)
	if !ok {
		return "", ""
	}
	accountID, _ = obj["accountId"].(string)
	displayName, _ = obj["displayName"].(string)

	return accountID, displayName
}

// parseJiraTime parses Jira's timestamp format.
func parseJiraTime(raw any) (time.Time, error) {
	s, ok := raw.(string)
	if !ok || s == "" {
		return time.Time{}, fmt.Errorf("missing or non-string value %v", raw)
	}

	t, err := time.Parse(jiraTimeFormat, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing %q as %q: %w", s, jiraTimeFormat, err)
	}

	return t, nil
}
```

Note `annotate` takes `evt` by value and mutates `evt.Artifacts` — that works because the map header
is copied but points at the same map. If that reads as too subtle, change it to a pointer receiver on
the event; say so in your report if you do.

- [ ] **Step 4: Run to confirm it passes**

Run: `env -u GOROOT -u GOPATH go test ./internal/collector/jira/ -v`

Expected: every test PASSES.

- [ ] **Step 5: Prove the two riskiest tests discriminate**

Two behaviours here are easy to write a test *around* rather than *for*:

1. Delete `field` from the ExternalID format string (leaving `"%s:%s"` with key and id). Confirm
   `TestEventsFromChangelogEntry_MultipleItemsInOneEntry` FAILS on the `ElementsMatch`.
2. Change `annotate` to skip self-authored events entirely (return before appending). Confirm
   `TestEventsFromChangelogEntry_SelfAuthoredIsTaggedNotDropped` FAILS on the length assertion.

Restore after each; paste all four outputs.

- [ ] **Step 6: Commit**

```bash
env -u GOROOT -u GOPATH gofmt -w internal/collector/jira/
export PATH="$HOME/.asdf/installs/golang/1.25.7/bin:$PATH"
env -u GOROOT -u GOPATH golangci-lint run ./internal/collector/jira/...
git add internal/collector/jira/
git commit -m "Add jira event shaping: four tracked fields, full text, self-author tagging"
```

---

## Task 4: cursor position encoding

**Files:**
- Create: `internal/collector/jira/cursor.go`
- Create: `internal/collector/jira/cursor_test.go`

A watermark is only valid for the query that produced it. Widening a JQL makes a stored "caught up to
Aug 20" hide every issue untouched since then — permanently. Hashing the effective JQL into the
position detects the edit.

- [ ] **Step 1: Write the failing tests**

Create `internal/collector/jira/cursor_test.go`:

```go
package jira_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	collectorjira "github.com/jcogilvie/unjira/internal/collector/jira"
)

func TestPosition_RoundTrips(t *testing.T) {
	watermark := time.Date(2026, 8, 20, 14, 30, 0, 0, time.UTC)
	encoded := collectorjira.EncodePosition("project = PROJ", watermark)

	got, ok := collectorjira.DecodePosition(encoded, "project = PROJ")

	require.True(t, ok, "the same JQL must accept its own watermark")
	assert.True(t, got.Equal(watermark), "want %v, got %v", watermark, got)
}

func TestDecodePosition_RejectsWatermarkFromDifferentJQL(t *testing.T) {
	encoded := collectorjira.EncodePosition("assignee = currentUser()", time.Now())

	_, ok := collectorjira.DecodePosition(encoded, "project = PROJ")

	assert.False(t, ok,
		"a watermark from a narrower query would permanently hide issues untouched since it was stored")
}

func TestDecodePosition_HashCoversTheProjectScope(t *testing.T) {
	// EncodePosition is always given the *effective* JQL, so adding a project
	// key must invalidate too — the same bug one level up.
	narrow := `(assignee = currentUser()) AND project IN ("PROJ")`
	wide := `(assignee = currentUser()) AND project IN ("PROJ", "OPS")`
	encoded := collectorjira.EncodePosition(narrow, time.Now())

	_, ok := collectorjira.DecodePosition(encoded, wide)

	assert.False(t, ok, "widening project_keys must invalidate the watermark")
}

func TestDecodePosition_MalformedInputsRescanRatherThanFail(t *testing.T) {
	tests := []struct {
		name     string
		position string
	}{
		{name: "empty (no cursor yet)", position: ""},
		{name: "no separator", position: "abc123"},
		{name: "unparseable watermark", position: "abc123:not-a-time"},
		{name: "empty watermark", position: "abc123:"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ok := collectorjira.DecodePosition(tt.position, "project = PROJ")

			assert.False(t, ok,
				"an unreadable cursor must fall back to a full rescan, which is safe; "+
					"treating it as valid would skip issues")
		})
	}
}

func TestEncodePosition_PreservesSubSecondPrecisionAndZone(t *testing.T) {
	// A watermark truncated to the second can re-fetch an issue (harmless) or,
	// if rounded up, skip one (not harmless). This project has already shipped
	// one bug from a timestamp format that silently dropped sub-second digits.
	watermark := time.Date(2026, 8, 20, 14, 30, 0, 123456789, time.FixedZone("+02:00", 2*60*60))
	encoded := collectorjira.EncodePosition("project = PROJ", watermark)

	got, ok := collectorjira.DecodePosition(encoded, "project = PROJ")

	require.True(t, ok)
	assert.True(t, got.Equal(watermark), "want %v, got %v", watermark, got)
}
```

- [ ] **Step 2: Run to confirm it fails**

Run: `env -u GOROOT -u GOPATH go test ./internal/collector/jira/ -run Position -v`

Expected: FAIL to compile — `EncodePosition`/`DecodePosition` undefined.

- [ ] **Step 3: Implement**

Create `internal/collector/jira/cursor.go`:

```go
package jira

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

// positionTimeFormat is how a watermark is stored. RFC3339Nano is correct here
// and is *not* safe in contexts that compare timestamps as strings — it strips
// trailing zeros, so ".1" sorts after ".15". Nothing compares positions
// lexicographically; they are parsed. Keep it that way.
const positionTimeFormat = time.RFC3339Nano

// jqlHashLength is how much of the JQL digest goes into the position. Twelve
// hex characters is ample to detect an edit, which is all this needs to do — it
// is not a security boundary.
const jqlHashLength = 12

// CursorResource is the cursors-table resource key for one connection's named
// query. The query name is part of the key, so renaming a query resets only
// that query's watermark.
func CursorResource(connection, query string) string {
	return connection + "/" + query
}

// EncodePosition renders a cursor position as "<jql hash>:<watermark>".
//
// effectiveJQL must be the fully scoped query — the configured JQL plus the
// auto-added project clause — because widening either invalidates the
// watermark for the same reason.
func EncodePosition(effectiveJQL string, watermark time.Time) string {
	return hashJQL(effectiveJQL) + ":" + watermark.Format(positionTimeFormat)
}

// DecodePosition returns the stored watermark, and whether it is usable for
// effectiveJQL.
//
// False means "rescan from the query's own horizon", which is always safe:
// (source, external_id) dedup makes re-emission free, so the cost of a
// needless rescan is API calls. Wrongly trusting a watermark costs data, which
// is why every unreadable form returns false rather than an error a caller
// might handle by continuing.
func DecodePosition(position, effectiveJQL string) (time.Time, bool) {
	hash, raw, found := strings.Cut(position, ":")
	if !found || hash != hashJQL(effectiveJQL) {
		return time.Time{}, false
	}

	watermark, err := time.Parse(positionTimeFormat, raw)
	if err != nil {
		return time.Time{}, false
	}

	return watermark, true
}

// hashJQL digests a query for change detection.
func hashJQL(jql string) string {
	sum := sha256.Sum256([]byte(jql))

	return hex.EncodeToString(sum[:])[:jqlHashLength]
}
```

Note `strings.Cut` splits on the *first* colon, and `positionTimeFormat` output contains colons — that
is why the hash goes first. Reversing the order would break parsing.

- [ ] **Step 4: Run to confirm it passes**

Run: `env -u GOROOT -u GOPATH go test ./internal/collector/jira/ -v`

Expected: all cursor tests PASS, Task 3's still pass.

- [ ] **Step 5: Prove the invalidation test discriminates**

Change `DecodePosition` to ignore the hash (parse the watermark from whatever follows the first colon,
return true). Confirm `TestDecodePosition_RejectsWatermarkFromDifferentJQL` **and**
`TestDecodePosition_HashCoversTheProjectScope` both FAIL. Restore. Paste both outputs.

- [ ] **Step 6: Commit**

```bash
env -u GOROOT -u GOPATH gofmt -w internal/collector/jira/
env -u GOROOT -u GOPATH golangci-lint run ./internal/collector/jira/...
git add internal/collector/jira/
git commit -m "Add jira cursor positions: watermark keyed by effective-JQL hash"
```

---

## Task 5: the collector

**Files:**
- Create: `internal/collector/jira/jira.go`
- Create: `internal/collector/jira/jira_test.go`

Ties the pure pieces to the client, the store, and the config. This is the only file in the package
that does I/O.

- [ ] **Step 1: Write the failing tests**

Create `internal/collector/jira/jira_test.go`:

```go
package jira_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	collectorjira "github.com/jcogilvie/unjira/internal/collector/jira"
	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/credentials"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/pipeline"
	"github.com/jcogilvie/unjira/internal/store"
)

// fakeJira serves canned Jira responses. Requests are recorded so tests can
// assert on the JQL actually sent, which is where the watermark and the project
// scope become observable.
type fakeJira struct {
	mu sync.Mutex
	// searchJQLs records the jql query parameter of each search request.
	searchJQLs []string
	// issues is returned by the search endpoint, in order.
	issues []map[string]any
	// changelogs and comments are keyed by issue key.
	changelogs map[string][]map[string]any
	comments   map[string][]map[string]any
	// failChangelogFor makes GetChangelog return 500 for one issue key.
	failChangelogFor string
	// accountID is what Myself() reports.
	accountID string
}

func (f *fakeJira) start(t *testing.T) string {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		path := r.URL.Path
		w.Header().Set("Content-Type", "application/json")

		switch {
		case strings.HasSuffix(path, "/myself"):
			f.writeJSON(t, w, map[string]any{"accountId": f.accountID, "displayName": "unjira"})

		case strings.Contains(path, "/search/jql"):
			f.searchJQLs = append(f.searchJQLs, r.URL.Query().Get("jql"))
			f.writeJSON(t, w, map[string]any{"issues": f.issues})

		case strings.HasSuffix(path, "/changelog"):
			key := issueKeyFromPath(path)
			if key == f.failChangelogFor {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"errorMessages":["boom"]}`))

				return
			}
			f.writeJSON(t, w, map[string]any{
				"values": f.changelogs[key], "isLast": true, "maxResults": 100,
			})

		case strings.HasSuffix(path, "/comment"):
			key := issueKeyFromPath(path)
			list := f.comments[key]
			f.writeJSON(t, w, map[string]any{
				"comments": list, "startAt": 0, "maxResults": 100, "total": len(list),
			})

		default:
			t.Errorf("unexpected request path %q", path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	return srv.URL
}

func (f *fakeJira) writeJSON(t *testing.T, w http.ResponseWriter, body map[string]any) {
	t.Helper()
	// assert, not require: this runs inside an httptest handler goroutine,
	// where require's FailNow (runtime.Goexit) would not fail the test itself.
	// internal/clients/jira/jira_test.go documents the same constraint.
	assert.NoError(t, json.NewEncoder(w).Encode(body))
}

// recordedJQLs returns the searches made so far, under the mutex. Tests must
// use this rather than touching f.searchJQLs: it is written from the httptest
// handler goroutine, so an unguarded read is a data race that -race will flag.
func (f *fakeJira) recordedJQLs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.searchJQLs...)
}

// issueKeyFromPath pulls PROJ-42 out of .../issue/PROJ-42/changelog.
func issueKeyFromPath(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i, p := range parts {
		if p == "issue" && i+1 < len(parts) {
			return parts[i+1]
		}
	}

	return ""
}

func issue(key, project, updated string) map[string]any {
	return map[string]any{
		"key": key,
		"fields": map[string]any{
			"project": map[string]any{"key": project},
			"updated": updated,
		},
	}
}

func changelogEntry(id, created, field, from, to string) map[string]any {
	return map[string]any{
		"id":      id,
		"created": created,
		"author":  map[string]any{"accountId": "acct-alice", "displayName": "Alice"},
		"items": []any{
			map[string]any{"field": field, "fromString": from, "toString": to},
		},
	}
}

// testStore opens a real temp-file SQLite store. Cursor behaviour is worth
// testing against the real DB rather than a fake, per the phase-1 spec.
func testStore(t *testing.T) *store.Store {
	t.Helper()

	s, err := store.Open(filepath.Join(t.TempDir(), "unjira.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	return s
}

func testContext(t *testing.T, site string, conn config.JiraConnection) pipeline.CollectContext {
	t.Helper()

	conn.Site = site

	return pipeline.CollectContext{
		Store:  testStore(t),
		Config: config.Config{Jira: []config.JiraConnection{conn}},
		Credentials: credentials.NewSet(map[string]credentials.Credential{
			conn.Name: {Email: "dev@example.com", Token: "token"},
		}),
		Options: map[string]any{},
	}
}

func collectAll(t *testing.T, cc pipeline.CollectContext) ([]events.Event, error) {
	t.Helper()

	var got []events.Event
	err := collectorjira.New().Collect(cc, func(e events.Event) { got = append(got, e) })

	return got, err
}

func TestCollect_EmitsChangelogAndCommentEvents(t *testing.T) {
	fake := &fakeJira{
		accountID: "acct-unjira",
		issues:    []map[string]any{issue("PROJ-42", "PROJ", "2026-08-20T16:00:00.000+0000")},
		changelogs: map[string][]map[string]any{
			"PROJ-42": {
				changelogEntry("10001", "2026-08-20T14:00:00.000+0000", "status", "To Do", "In Progress"),
				changelogEntry("10002", "2026-08-20T15:00:00.000+0000", "assignee", "", "Bob"),
			},
		},
		comments: map[string][]map[string]any{
			"PROJ-42": {{
				"id": "9001", "created": "2026-08-20T15:30:00.000+0000", "body": "looking at it",
				"author": map[string]any{"accountId": "acct-alice", "displayName": "Alice"},
			}},
		},
	}
	cc := testContext(t, fake.start(t), config.JiraConnection{
		Name: "corp", ProjectKeys: []string{"PROJ"},
		Queries: []config.JiraQuery{{Name: "mine", JQL: "assignee = currentUser()"}},
	})

	got, err := collectAll(t, cc)

	require.NoError(t, err)
	ids := make([]string, 0, len(got))
	for _, e := range got {
		ids = append(ids, e.ExternalID)
	}
	assert.ElementsMatch(t, []string{"PROJ-42:status:10001", "PROJ-42:comment:9001"}, ids,
		"the assignee change is not actionable in phase 1 and must not be emitted")
}

func TestCollect_ScopesJQLToProjectKeysAndAppliesWatermark(t *testing.T) {
	fake := &fakeJira{accountID: "acct-unjira"}
	cc := testContext(t, fake.start(t), config.JiraConnection{
		Name: "corp", ProjectKeys: []string{"PROJ", "OPS"},
		Queries: []config.JiraQuery{{Name: "mine", JQL: "assignee = currentUser()"}},
	})

	_, err := collectAll(t, cc)
	require.NoError(t, err)

	require.Len(t, fake.recordedJQLs(), 1)
	assert.Contains(t, fake.recordedJQLs()[0], `project IN ("PROJ", "OPS")`,
		"collection must never exceed what the connection can write to")
	assert.NotContains(t, fake.recordedJQLs()[0], "updated >=",
		"the first pass has no watermark to apply")
}

func TestCollect_SecondPassAppliesStoredWatermark(t *testing.T) {
	fake := &fakeJira{
		accountID: "acct-unjira",
		issues:    []map[string]any{issue("PROJ-42", "PROJ", "2026-08-20T16:00:00.000+0000")},
	}
	cc := testContext(t, fake.start(t), config.JiraConnection{
		Name: "corp", ProjectKeys: []string{"PROJ"},
		Queries: []config.JiraQuery{{Name: "mine", JQL: "assignee = currentUser()"}},
	})

	_, err := collectAll(t, cc)
	require.NoError(t, err)
	_, err = collectAll(t, cc)
	require.NoError(t, err)

	require.Len(t, fake.recordedJQLs(), 2)
	assert.NotContains(t, fake.recordedJQLs()[0], "updated >=")
	assert.Contains(t, fake.recordedJQLs()[1], "updated >=",
		"the second pass must bound the search by what the first already covered")
}

func TestCollect_EmitsChangelogEntriesOlderThanTheWatermark(t *testing.T) {
	// The behaviour most likely to be broken and least likely to be noticed.
	// An issue reassigned to you today has updated=now, so it matches the
	// watermark — but its changelog runs back months, and every one of those
	// entries is new to us. The watermark selects *issues*, never entries.
	fake := &fakeJira{
		accountID: "acct-unjira",
		issues:    []map[string]any{issue("PROJ-42", "PROJ", "2026-08-20T16:00:00.000+0000")},
		changelogs: map[string][]map[string]any{
			"PROJ-42": {
				changelogEntry("10001", "2026-01-04T09:00:00.000+0000", "status", "To Do", "In Progress"),
				changelogEntry("10002", "2026-08-20T15:00:00.000+0000", "status", "In Progress", "Done"),
			},
		},
	}
	cc := testContext(t, fake.start(t), config.JiraConnection{
		Name: "corp", ProjectKeys: []string{"PROJ"},
		Queries: []config.JiraQuery{{Name: "mine", JQL: "assignee = currentUser()"}},
	})

	// Seed a watermark well after the January entry by running a first pass.
	_, err := collectAll(t, cc)
	require.NoError(t, err)

	got, err := collectAll(t, cc)

	require.NoError(t, err)
	var sawJanuary bool
	for _, e := range got {
		if e.ExternalID == "PROJ-42:status:10001" {
			sawJanuary = true
		}
	}
	assert.True(t, sawJanuary,
		"entries predating the watermark are new to us and must be emitted; dedup makes it free")
}

func TestCollect_HittingTheIssueLimitIsLogged(t *testing.T) {
	// The spec requires this be "logged, never silent": a silent cap presents
	// as a clean pass while ignoring work, which docs/design-notes.md calls the
	// hardest class of bug to notice. An untested log line is one refactor away
	// from being deleted, so capture the log output and assert on it.
	var logged bytes.Buffer
	log.SetOutput(&logged)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(log.LstdFlags)
	})

	fake := &fakeJira{
		accountID: "acct-unjira",
		issues: []map[string]any{
			issue("PROJ-1", "PROJ", "2026-08-20T16:00:00.000+0000"),
			issue("PROJ-2", "PROJ", "2026-08-20T16:00:00.000+0000"),
		},
	}
	cc := testContext(t, fake.start(t), config.JiraConnection{
		Name: "corp", ProjectKeys: []string{"PROJ"}, MaxIssuesPerQuery: 2,
		Queries: []config.JiraQuery{{Name: "mine", JQL: "assignee = currentUser()"}},
	})

	_, err := collectAll(t, cc)

	require.NoError(t, err)
	assert.Contains(t, logged.String(), "corp/mine",
		"the message must name the query that was truncated")
	assert.Contains(t, logged.String(), "limit")
}

func TestCollect_OneQueryFailingDoesNotStopTheOthers(t *testing.T) {
	fake := &fakeJira{
		accountID:        "acct-unjira",
		issues:           []map[string]any{issue("PROJ-42", "PROJ", "2026-08-20T16:00:00.000+0000")},
		failChangelogFor: "PROJ-42",
	}
	cc := testContext(t, fake.start(t), config.JiraConnection{
		Name: "corp", ProjectKeys: []string{"PROJ"},
		Queries: []config.JiraQuery{
			{Name: "broken", JQL: "assignee = currentUser()"},
			{Name: "alsobroken", JQL: "watcher = currentUser()"},
		},
	})

	_, err := collectAll(t, cc)

	require.Error(t, err, "the caller must see that queries failed")
	assert.Contains(t, err.Error(), "broken")
	assert.Len(t, fake.recordedJQLs(), 2,
		"the second query must still have been attempted after the first failed")
}

func TestCollect_FailedQueryDoesNotAdvanceItsWatermark(t *testing.T) {
	fake := &fakeJira{
		accountID:        "acct-unjira",
		issues:           []map[string]any{issue("PROJ-42", "PROJ", "2026-08-20T16:00:00.000+0000")},
		failChangelogFor: "PROJ-42",
	}
	cc := testContext(t, fake.start(t), config.JiraConnection{
		Name: "corp", ProjectKeys: []string{"PROJ"},
		Queries: []config.JiraQuery{{Name: "mine", JQL: "assignee = currentUser()"}},
	})

	_, err := collectAll(t, cc)
	require.Error(t, err)

	position, cursorErr := cc.Store.GetCursor("jira", "corp/mine")
	require.NoError(t, cursorErr)
	assert.Empty(t, position,
		"advancing past an issue we could not read would skip it permanently")
}

func TestCollect_WriteOnlyConnectionIsSkipped(t *testing.T) {
	fake := &fakeJira{accountID: "acct-unjira"}
	cc := testContext(t, fake.start(t), config.JiraConnection{
		Name: "corp", ProjectKeys: []string{"PROJ"},
		// No Queries: this connection routes project keys for writes only.
	})

	got, err := collectAll(t, cc)

	require.NoError(t, err)
	assert.Empty(t, got)
	assert.Empty(t, fake.recordedJQLs(), "a connection with no queries must make no requests")
}

func TestCollect_EmptyProjectKeysErrorsNamingTheConnection(t *testing.T) {
	fake := &fakeJira{accountID: "acct-unjira"}
	cc := testContext(t, fake.start(t), config.JiraConnection{
		Name:    "corp",
		Queries: []config.JiraQuery{{Name: "mine", JQL: "assignee = currentUser()"}},
	})

	_, err := collectAll(t, cc)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "corp")
	assert.Contains(t, err.Error(), "project_keys")
	assert.Empty(t, fake.recordedJQLs(), "an unscopeable query must not be sent")
}

func TestCollect_MissingCredentialErrorsNamingTheConnection(t *testing.T) {
	fake := &fakeJira{accountID: "acct-unjira"}
	cc := testContext(t, fake.start(t), config.JiraConnection{
		Name: "corp", ProjectKeys: []string{"PROJ"},
		Queries: []config.JiraQuery{{Name: "mine", JQL: "assignee = currentUser()"}},
	})
	cc.Credentials = credentials.Set{} // zero value: nothing configured

	_, err := collectAll(t, cc)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "corp")
}
```

- [ ] **Step 2: Run to confirm it fails**

Run: `env -u GOROOT -u GOPATH go test ./internal/collector/jira/ -v`

Expected: FAIL to compile — `collectorjira.New` undefined.

- [ ] **Step 3: Implement**

Create `internal/collector/jira/jira.go`:

```go
package jira

import (
	"errors"
	"fmt"
	"log"
	"time"

	jiraclient "github.com/jcogilvie/unjira/internal/clients/jira"
	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/pipeline"
)

// searchFields are the issue fields the search must return.
//
// summary and description are deliberately absent: they arrive as changelog
// events when they change, and requesting them here would invite emitting a
// snapshot on every pass — an event stream of "the description is still X",
// which is not an observation that anything happened.
var searchFields = []string{"key", "project", "updated"}

// Collector reads Jira changes for every configured connection's named queries.
type Collector struct{}

// New builds the collector. Stateless: everything it needs arrives on the
// CollectContext, which is what lets one instance serve every connection.
func New() *Collector { return &Collector{} }

// Name identifies this collector in the registry, the cursors table, and the
// Source field of every event it emits.
func (c *Collector) Name() string { return Name }

// Collect walks every configured connection's queries.
//
// Failure is per query, not per pass: a 403 on one JQL (a revoked project
// permission) must not stop an unrelated query from progressing. Failed queries
// are accumulated and returned together, and their watermarks stay put so the
// next run retries that range automatically.
func (c *Collector) Collect(cc pipeline.CollectContext, visit func(events.Event)) error {
	var failures []error

	for _, conn := range cc.Config.Jira {
		if len(conn.Queries) == 0 {
			// Write-only connection: it routes project keys but collects
			// nothing. Not an error.
			continue
		}

		if err := c.collectConnection(cc, conn, visit); err != nil {
			failures = append(failures, err)
		}
	}

	return errors.Join(failures...)
}

// collectConnection builds one client for a connection and runs its queries.
func (c *Collector) collectConnection(
	cc pipeline.CollectContext,
	conn config.JiraConnection,
	visit func(events.Event),
) error {
	cred, ok := cc.Credentials.For(conn.Name)
	if !ok {
		return fmt.Errorf(
			"jira connection %q has no credential: set it in UNJIRA_JIRA_CREDENTIALS", conn.Name)
	}

	client, err := jiraclient.New(conn.Site, cred.Email, cred.Token)
	if err != nil {
		return fmt.Errorf("building jira client for connection %q: %w", conn.Name, err)
	}

	limit, err := conn.IssueLimit()
	if err != nil {
		return err
	}

	// One Myself() call per connection per pass, not per issue. An empty
	// accountID (a permission that does not allow reading self) degrades to
	// "tag nothing", which is preferable to failing the whole pass — the tag is
	// an optimisation for the reconciler, not a correctness requirement of
	// collection.
	selfAccountID := ""
	if me, meErr := client.Myself(); meErr != nil {
		log.Printf("jira: connection %s: could not read own account id (%v); "+
			"self-authored changes will not be tagged this pass", conn.Name, meErr)
	} else {
		selfAccountID, _ = me["accountId"].(string)
	}

	var failures []error

	for _, query := range conn.Queries {
		if err := c.collectQuery(cc, conn, query, client, selfAccountID, limit, visit); err != nil {
			failures = append(failures, fmt.Errorf("query %s/%s: %w", conn.Name, query.Name, err))
		}
	}

	return errors.Join(failures...)
}

// collectQuery runs one named query: search, then per-issue changelog and
// comments, then advance the watermark.
func (c *Collector) collectQuery(
	cc pipeline.CollectContext,
	conn config.JiraConnection,
	query config.JiraQuery,
	client *jiraclient.Client,
	selfAccountID string,
	limit int,
	visit func(events.Event),
) error {
	effectiveJQL, err := conn.EffectiveJQL(query)
	if err != nil {
		return err
	}

	resource := CursorResource(conn.Name, query.Name)

	position, err := cc.Store.GetCursor(Name, resource)
	if err != nil {
		return err
	}

	searchJQL := effectiveJQL
	if watermark, ok := DecodePosition(position, effectiveJQL); ok {
		// Jira's JQL date literal has minute precision, so this floors to the
		// minute rather than dropping precision it cannot express. Flooring
		// re-examines a little (free, thanks to dedup); rounding up would skip.
		searchJQL = fmt.Sprintf("%s AND updated >= %q", effectiveJQL,
			watermark.UTC().Truncate(time.Minute).Format("2006-01-02 15:04"))
	}

	// The issues are collected before fetching changelogs rather than emitted
	// from inside the visit callback, so a mid-search failure cannot leave the
	// search half-consumed while errors propagate.
	var (
		issues  []map[string]any
		highest time.Time
	)

	if err := client.SearchIssues(searchJQL, searchFields, limit, func(issue map[string]any) {
		issues = append(issues, issue)
	}); err != nil {
		return err
	}

	if len(issues) >= limit {
		// Logged, never silent: a silent cap presents as a clean pass while
		// ignoring work. Because the watermark only advances on a complete
		// query, the next pass re-runs this range rather than skipping ahead.
		log.Printf("jira: query %s/%s hit its %d-issue limit; more issues may be unexamined this pass",
			conn.Name, query.Name, limit)
	}

	for _, issue := range issues {
		updated, err := c.collectIssue(conn, issue, client, selfAccountID, visit)
		if err != nil {
			// One issue's failure fails its query. Advancing past an issue we
			// could not read would step over it permanently.
			return err
		}

		if updated.After(highest) {
			highest = updated
		}
	}

	if highest.IsZero() {
		// Nothing matched; leave the existing watermark alone rather than
		// writing a zero one.
		return nil
	}

	return cc.Store.SetCursor(Name, resource, EncodePosition(effectiveJQL, highest))
}

// collectIssue emits every event for one issue and returns its updated time,
// which feeds the query's watermark.
func (c *Collector) collectIssue(
	conn config.JiraConnection,
	issue map[string]any,
	client *jiraclient.Client,
	selfAccountID string,
	visit func(events.Event),
) (time.Time, error) {
	key, _ := issue["key"].(string)
	if key == "" {
		return time.Time{}, fmt.Errorf("search result has no key field: %v", issue)
	}

	fields, _ := issue["fields"].(map[string]any)
	projectKey := ""
	if project, ok := fields["project"].(map[string]any); ok {
		projectKey, _ = project["key"].(string)
	}

	updated, err := parseJiraTime(fields["updated"])
	if err != nil {
		return time.Time{}, fmt.Errorf("issue %s: updated: %w", key, err)
	}

	ic := IssueContext{
		Key:           key,
		ProjectKey:    projectKey,
		Connection:    conn.Name,
		Site:          conn.Site,
		SelfAccountID: selfAccountID,
	}

	changelog, err := client.GetChangelog(key)
	if err != nil {
		return time.Time{}, fmt.Errorf("changelog for %s: %w", key, err)
	}

	for _, entry := range changelog {
		evts, err := EventsFromChangelogEntry(ic, entry)
		if err != nil {
			return time.Time{}, err
		}

		for _, evt := range evts {
			visit(evt)
		}
	}

	comments, err := client.GetComments(key)
	if err != nil {
		return time.Time{}, fmt.Errorf("comments for %s: %w", key, err)
	}

	for _, comment := range comments {
		evt, err := EventFromComment(ic, comment)
		if err != nil {
			return time.Time{}, err
		}

		visit(evt)
	}

	return updated, nil
}
```

`log.Printf` is the verified convention for this kind of operator notice:
`internal/correlator/correlator.go:928` logs compaction the same way, and `forbidigo` is disabled in
`.golangci.yml`, so it will not be flagged. Do not switch to `fmt.Printf` — that is reserved for
renderers writing to an explicit `io.Writer` (`internal/pipeline/narrate_render.go`).

- [ ] **Step 4: Run to confirm it passes**

Run: `env -u GOROOT -u GOPATH go test ./internal/collector/jira/ -v`

Expected: all tests PASS.

- [ ] **Step 5: Prove the two load-bearing tests discriminate**

0. Delete the `log.Printf` limit notice entirely. Confirm
   `TestCollect_HittingTheIssueLimitIsLogged` FAILS. Restore.
1. Make the watermark filter changelog entries too — skip any entry whose `created` predates the
   watermark. Confirm `TestCollect_EmitsChangelogEntriesOlderThanTheWatermark` FAILS. This is the
   single most important assertion in the slice; a fixture with no old entries would pass either way.
2. Change `collectQuery` to `SetCursor` before the per-issue loop instead of after. Confirm
   `TestCollect_FailedQueryDoesNotAdvanceItsWatermark` FAILS.

Restore after each; paste all six outputs.

- [ ] **Step 6: Commit**

```bash
env -u GOROOT -u GOPATH gofmt -w internal/collector/jira/
env -u GOROOT -u GOPATH golangci-lint run ./internal/collector/jira/...
git add internal/collector/jira/
git commit -m "Add internal/collector/jira: per-query cursors, per-query failure isolation"
```

---

## Task 6: registry wiring + `RunCollect` integration test

**Files:**
- Modify: `cmd/unjira/main.go`
- Modify: `internal/pipeline/collect_test.go`

- [ ] **Step 1: Write the failing integration test**

The unit tests call `Collect` directly. This one goes through `RunCollect` with the collector in a
registry and a real store, which is the only place dedup is observable.

Add to `internal/pipeline/collect_test.go` — adapt the fake-Jira harness by reusing the pattern from
Task 5 (a local `httptest` server in this file; do not export the collector package's test helpers):

```go
func TestRunCollect_JiraCollectorDedupesOnSecondPass(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/myself"):
			_, _ = w.Write([]byte(`{"accountId":"acct-unjira"}`))
		case strings.Contains(r.URL.Path, "/search/jql"):
			_, _ = w.Write([]byte(`{"issues":[{"key":"PROJ-42","fields":{` +
				`"project":{"key":"PROJ"},"updated":"2026-08-20T16:00:00.000+0000"}}]}`))
		case strings.HasSuffix(r.URL.Path, "/changelog"):
			_, _ = w.Write([]byte(`{"values":[{"id":"10001",` +
				`"created":"2026-08-20T14:00:00.000+0000",` +
				`"author":{"accountId":"acct-alice","displayName":"Alice"},` +
				`"items":[{"field":"status","fromString":"To Do","toString":"Done"}]}],"isLast":true}`))
		case strings.HasSuffix(r.URL.Path, "/comment"):
			_, _ = w.Write([]byte(`{"comments":[],"startAt":0,"maxResults":100,"total":0}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	s, err := store.Open(filepath.Join(t.TempDir(), "unjira.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()

	cfg := config.Config{
		Collectors: map[string]map[string]any{"jira": {"enabled": true}},
		Jira: []config.JiraConnection{{
			Name: "corp", Site: srv.URL, ProjectKeys: []string{"PROJ"},
			Queries: []config.JiraQuery{{Name: "mine", JQL: "assignee = currentUser()"}},
		}},
	}
	reg := map[string]func() pipeline.Collector{
		"jira": func() pipeline.Collector { return collectorjira.New() },
	}
	creds := credentials.NewSet(map[string]credentials.Credential{
		"corp": {Email: "dev@example.com", Token: "token"},
	})

	first, err := pipeline.RunCollect(cfg, s, reg, nil, creds)
	require.NoError(t, err)
	assert.Equal(t, 1, first["jira"])

	second, err := pipeline.RunCollect(cfg, s, reg, nil, creds)

	require.NoError(t, err)
	assert.Equal(t, 0, second["jira"],
		"(source, external_id) dedup makes re-running a collector safe; a second insert means it isn't")
}
```

Confirm the `Collectors` map shape and the `RunCollect` return type against the existing tests in this
file and adjust if they differ — do not guess.

- [ ] **Step 2: Run to confirm it fails**

Run: `env -u GOROOT -u GOPATH go test ./internal/pipeline/ -run TestRunCollect_Jira -v`

Expected: FAIL — no `jira` collector in the registry, so the count is `-1` (the "enabled but not
registered" sentinel) rather than 1.

- [ ] **Step 3: Register the collector**

In `cmd/unjira/main.go`, add the import and the registry entry:

```go
	collectorjira "github.com/jcogilvie/unjira/internal/collector/jira"
```

```go
var registry = map[string]func() pipeline.Collector{
	"claude_code": func() pipeline.Collector { return claudecode.New() },
	"jira":        func() pipeline.Collector { return collectorjira.New() },
}
```

- [ ] **Step 4: Run to confirm it passes**

Run: `env -u GOROOT -u GOPATH go test ./internal/pipeline/ ./cmd/... -v`

Expected: the new test PASSES; every pre-existing pipeline and cmd test still passes.

- [ ] **Step 5: Commit**

```bash
env -u GOROOT -u GOPATH gofmt -w cmd/unjira/ internal/pipeline/
git add cmd/unjira/main.go internal/pipeline/collect_test.go
git commit -m "Register the jira collector; assert RunCollect dedupes on a second pass"
```

---

## Task 7: full regression, lint, and container gate

**Files:** none — verification only.

- [ ] **Step 1: Full offline suite**

```bash
env -u GOROOT -u GOPATH go build ./...
env -u GOROOT -u GOPATH go test ./... 2>&1 | tail -40
```

Expected: every package `ok` or `no test files`. No failures anywhere, including packages you did not
touch.

- [ ] **Step 2: Lint**

```bash
export PATH="$HOME/.asdf/installs/golang/1.25.7/bin:$PATH"
env -u GOROOT -u GOPATH golangci-lint run ./...
```

Expected: `0 issues`. Fix anything reported; do not add `nolint` directives without saying why in your
report.

- [ ] **Step 3: The authoritative gate**

```bash
earthly +reviewable
```

Expected: lint and tests both pass in the container. If this fails while the local run passed,
something is environment-dependent — report it rather than working around it.

- [ ] **Step 4: Verify the example config still loads**

```bash
python3 -c "import json; json.load(open('config/unjira.example.json'))" && echo VALID
```

- [ ] **Step 5: Report**

State: which tests you added, the break-it drill results, and anything in this plan you changed and
why. Do not commit if any gate fails — report instead.

---

## Task 8: live-tier test

**Files:**
- Modify: `internal/live/jira_test.go`

Offline fixtures encode *my* belief about Jira's JSON. Only a live call establishes whether that belief
is right — the previous slice's fenced-JSON bug was exactly this class, invisible to fakes.

- [ ] **Step 1: Read the existing live harness**

Read `internal/live/jira_test.go`. It is behind `//go:build live` and `testClient(t)` skips unless
`UNJIRA_LIVE=1` plus `UNJIRA_JIRA_EMAIL`/`UNJIRA_JIRA_TOKEN` are set. Follow its existing patterns for
seeding and cleaning up issues rather than inventing new ones.

- [ ] **Step 2: Add the test**

Seed an issue, comment on it, then run the collector against a real store and assert the events
appear. Sketch — adapt to the file's actual helpers:

```go
func TestLiveCollector_SeesSeededCommentAndTransition(t *testing.T) {
	client := testClient(t)
	key := seedIssue(t, client, "unjira live collector test")

	_, err := client.AddComment(key, "live collector probe")
	require.NoError(t, err)

	s, err := store.Open(filepath.Join(t.TempDir(), "unjira.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()

	cc := pipeline.CollectContext{
		Store: s,
		Config: config.Config{Jira: []config.JiraConnection{{
			Name: "dev", Site: os.Getenv("UNJIRA_JIRA_SITE"),
			ProjectKeys: []string{projectKeyOf(key)},
			Queries: []config.JiraQuery{
				{Name: "probe", JQL: fmt.Sprintf("key = %q", key)},
			},
		}}},
		Credentials: credentials.NewSet(map[string]credentials.Credential{
			"dev": {Email: os.Getenv("UNJIRA_JIRA_EMAIL"), Token: os.Getenv("UNJIRA_JIRA_TOKEN")},
		}),
		Options: map[string]any{},
	}

	var got []events.Event
	require.NoError(t, collectorjira.New().Collect(cc, func(e events.Event) { got = append(got, e) }))

	var sawComment bool
	for _, e := range got {
		if strings.HasPrefix(e.ExternalID, key+":comment:") {
			sawComment = true
			assert.Contains(t, e.Summary, "live collector probe")
			assert.Equal(t, true, e.Artifacts["authored_by_unjira"],
				"we posted this comment, so the self-author tag must be set against the live accountId")
		}
	}
	require.True(t, sawComment, "the comment we just posted must appear as an event")
}
```

The `authored_by_unjira` assertion is the highest-value part: it is the one thing no fixture can
verify, since it depends on the real `Myself()` response matching the real comment author.

- [ ] **Step 3: Verify it compiles and skips cleanly**

```bash
env -u GOROOT -u GOPATH go vet -tags=live ./internal/live/...
env -u GOROOT -u GOPATH go test -tags=live ./internal/live/... -run TestLiveCollector -v
```

Expected: compiles; SKIPs without `UNJIRA_LIVE=1`. **Do not attempt a real run** — that needs
credentials the coordinator will supply. Report that it compiles and skips.

- [ ] **Step 4: Confirm the offline suite is unaffected**

```bash
env -u GOROOT -u GOPATH go test ./... 2>&1 | tail -20
```

The `live` tag excludes `internal/live` entirely, so this must be unchanged from Task 7.

- [ ] **Step 5: Commit**

```bash
env -u GOROOT -u GOPATH gofmt -w internal/live/
git add internal/live/
git commit -m "Add live-tier test: collector sees a seeded comment and tags it self-authored"
```

---

## Out of scope (do not build)

- **Narrative→issue matching.** Nothing in this slice sets `narratives.issue_key`. That is the next
  slice, and adding it here would make this PR unreviewable.
- **Any write path.** This slice is read-only. `AddComment` is used in the live test only, to seed.
- **A renderer cap for long descriptions.** `pipeline.RenderNarrateResult` prints summaries inline and
  a 20,000-character description would swamp it. Real, and a renderer change, not this one.
- **Snapshot events for current summary/description.** Explicitly rejected in the spec in favour of
  discrete change events.
