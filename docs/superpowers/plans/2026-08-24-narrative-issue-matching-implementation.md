# Narrative→Issue Matching Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Write `narratives.issue_key` — the column nothing currently writes — by attributing each
narrative to the issue(s) that own its work, so the reconciler's verification branch becomes
reachable.

**Architecture:** A new `correlator.Match` pass runs after `Persist`, selecting narratives with no
issue link. It gathers candidate keys deterministically from event artifacts, verifies every one via
`tracker.GetIssue`, resolves a single survivor without an LLM, and calls the LLM only to classify
among multiple survivors. Results land in a new `narrative_issues` table plus a denormalized primary
pointer on `narratives`.

**Tech Stack:** Go 1.26, `modernc.org/sqlite`, `github.com/stretchr/testify`, `net/http/httptest`.

**Spec:** `docs/superpowers/specs/2026-08-24-narrative-issue-matching-design.md` — authoritative
where this plan and it disagree.

**This plan's code is a draft you are responsible for.** Plans in this repo have shipped real bugs
when transcribed verbatim: an `err == sql.ErrNoRows` that failed on wrapped errors; a `time.RFC3339`
format that silently truncated a sub-second TTL; a watermark rendered in UTC where the API expected
account-local time, which silently collected nothing. If something here is wrong, fix it and say so
in your report.

---

## Critical environment notes (apply to EVERY task)

**Stale `GOROOT`/`GOPATH`.** Prefix EVERY `go`/`gofmt` invocation with `env -u GOROOT -u GOPATH`.
Without it you get spurious `compile: version "X" does not match go tool version "Y"` errors
unrelated to your change.

**Formatting and lint** (the repo uses gofumpt via golangci-lint, stricter than plain gofmt):

```bash
export PATH="$HOME/.asdf/installs/golang/1.25.7/bin:$PATH"
env -u GOROOT -u GOPATH golangci-lint fmt ./internal/... ./cmd/...
env -u GOROOT -u GOPATH golangci-lint run ./...
```

**The branch must stay at `0 issues`.** Ignore the `gomodguard is deprecated` warning — it is
expected and is not a finding.

**There is a real `.env` at the repo root containing a live Jira API token.** Do NOT read, `cat`,
`grep`, print, or copy it. Never `git add -A` or `git add .` — always explicit paths. If
`git status --short` ever lists `.env`, stop and report.

**Do NOT run the live tests** (`-tags=live` with `UNJIRA_LIVE=1`). They write to a real Jira
instance. Make them compile and skip; the coordinator runs them.

**The authoritative gate is `earthly +reviewable`** — lint plus tests in a clean container with its
own Go toolchain. A local pass with a container failure means something is environment-dependent.

**Work in** `/Users/jonathan.ogilvie/workspace/unjira/.claude/worktrees/slice4-reconciler` on branch
`worktree-narrative-matching`. Do not create or switch branches. Do not use `git stash` (the stack is
shared with other sessions). Do not modify the session task list.

**Repo conventions** (read `docs/go-conventions.md` and `CLAUDE.md` first): TDD — failing test first,
confirm it fails for the right reason, then implement. Never silently drop data; error loudly with
`fmt.Errorf("...: %w", err)` naming the relevant id. testify `require` (fatal) / `assert`
(non-fatal). Doc comments on every exported identifier explaining WHY, not just what.

**Proving a regression test works.** For any test guarding a specific defect, break the guarded
behaviour, confirm the test fails, restore, confirm it passes. Paste both outputs. This project has
caught several tests that looked like coverage and were not — including two whose fixtures could not
distinguish pass from fail.

---

## What already exists (verified — do not re-derive)

```go
// internal/tasktracker
type Issue struct { Key, Summary string; StatusCategory StatusCategory; StatusName string; Labels []string }
type TaskTracker interface {
	GetIssue(key string) (Issue, error)
	SearchIssues(query string, limit int) ([]Issue, error)
	AddComment(key, text string) error
	SetStatus(key string, target StatusCategory) error
	CreateIssue(projectOrRepo, summary, issueType, description string, labels []string) (string, error)
}

// internal/events
func ExtractTicketKeys(text string) []string                       // ordered, deduplicated
func PartitionExcludedKeys(keys []string, compiled []*regexp.Regexp) (kept, excluded []string)

// internal/config
func (c Config) CompiledLinkExclusions() ([]*regexp.Regexp, error)  // verify the exact name before use
type CorrelatorConfig struct { TailSummarizeThresholdTokens, RecentEventsKept int }
func (c CorrelatorConfig) Validate() error

// internal/store
type NarrativeRow struct {
	ID int64; WindowStart, WindowEnd time.Time; Title, Summary, IssueKey string
	Confidence float64; Status string
	CompactionBoundary *time.Time; CompactionBoundaryEventID *int64
}
func (s *Store) NarrativesOverlapping(start, end time.Time) ([]NarrativeRow, error)
func (s *Store) NarrativeEventsForContext(narrativeID int64) ([]events.Event, error)
func (s *Store) WithTx(fn func(*Tx) error) error
func scanNarrativeRow(row scanRow) (NarrativeRow, error)            // unexported; reuse it
func scanEvent(row scanRow) (events.Event, error)                   // unexported; reuse it
type scanRow interface { Scan(dest ...any) error }

// internal/correlator
type Stats struct { Calls, Splits, MergeChecks, Compactions int; PromptTokens, CompletionTokens, EstimatedTokens int64 }
func (s *Stats) Add(other Stats)
func stripJSONFence(raw string) string                              // unexported; models add fences anyway
```

**The `*Store`/`*Tx` twin pattern** (`store.go`, e.g. `ExtendNarrative`): a shared
`fooImpl(db dbConn, ...)` function plus two thin wrappers, one on `*Store` passing `s.db` and one on
`*Tx` passing `t.tx`. `dbConn` is the `Exec`/`QueryRow`/`Query` interface both satisfy. Follow it for
every new accessor that needs a Tx variant.

**`fakeLLM`** (`internal/correlator/correlator_test.go:28-56`) — fields `responses []string`,
`prompts []string`, `systemPrompts []string`, `err error`, `usagePerCall llm.Usage`. Consumes
`responses` in call order, clamping to the last element. It is unexported and file-scoped to
`correlator_test.go`, so it is already available to any new `_test.go` in `package correlator_test`.

**`openStore(t)`** (`internal/store/store_test.go:29-37`) — real temp-file SQLite via
`store.Open(filepath.Join(t.TempDir(), "test.db"))` with `t.Cleanup`. Every store-touching test uses
it; there is no `:memory:` mode anywhere.

---

## File structure

- **Modify** `internal/tasktracker/tasktracker.go` — add `Issue.Description`.
- **Modify** `internal/clients/jira/tracker.go` — `normalizeIssue` populates `Description`.
- **Modify** `internal/clients/local/local.go` — `toIssue` populates `Description`.
- **Modify** `internal/store/store.go` — `narrative_issues` DDL + indexes, `NarrativeIssue`,
  `NarrativeIssueRef`, and six accessors.
- **Modify** `internal/store/store_test.go` — accessor and invariant tests.
- **Modify** `internal/config/config.go` — `MatchConfig` + `Validate`.
- **Modify** `internal/config/config_test.go` — validation tests.
- **Create** `internal/correlator/match_types.go` — `Role`, `Provenance`, `Candidate`,
  `NarrativeIssue` conversion, `MatchResult`, and the role parser. Pure, no I/O.
- **Create** `internal/correlator/match_candidates.go` — deterministic candidate gathering from
  events. Pure, no I/O.
- **Create** `internal/correlator/match.go` — `Match`: verification, resolution, LLM classification,
  persistence.
- **Create** `internal/correlator/match_types_test.go`, `match_candidates_test.go`, `match_test.go`.
- **Modify** `internal/correlator/export_test.go` — expose the new unexported prompt/parser helpers.
- **Create** `internal/pipeline/match.go` — `RunMatch`.
- **Create** `internal/pipeline/match_test.go`.
- **Modify** `internal/pipeline/narrate_render.go` + `narrate.go` — surface links in output.
- **Modify** `cmd/unjira/main.go` — call `RunMatch` after `RunNarrate` in `dev narrate`.
- **Modify** `internal/live/jira_test.go` — live-tier test.
- **Modify** `config/unjira.example.json` — a `match` block.

The three-file split of the correlator work is deliberate: candidate gathering and type/parse logic
are pure functions, and keeping them separate is what lets their tests run with no database, no HTTP,
and no LLM. `match.go` holds the part that genuinely needs all three.

**Sequencing.** Tasks 1–4 are independent of each other (different packages) and may run in
parallel. Task 5 needs 1 and 4. Task 6 needs 2 and 5. Task 7 needs 5 and 6. Task 8 needs 7. Tasks
9–10 come last.

**Do not run two tasks concurrently that touch the same directory** — each task's `git add` would
stage the other's half-finished files.

---

## Task 1: `tasktracker.Issue` gains `Description`

**Files:**
- Modify: `internal/tasktracker/tasktracker.go`
- Modify: `internal/clients/jira/tracker.go`
- Modify: `internal/clients/local/local.go`
- Modify: `internal/clients/jira/tracker_test.go` (create if absent)
- Modify: `internal/clients/local/local_test.go` (create if absent)

Matching classifies among candidates by comparing the narrative against each candidate's text. The
Jira collector's spec names summary *and* description as the signal, but `Issue` carries only
`Summary` and `normalizeIssue` never reads `description`.

This does **not** contradict the collector's decision to exclude `summary`/`description` from its
`searchFields`. That decision is about the *event log's* shape — requesting them there would invite
emitting a snapshot event on every pass ("the description is still X"), which is not an observation
that anything happened. This is the *live read* path, via a `GetIssue` call
`rules/verify-correlations.md` already requires, so the field is free.

- [ ] **Step 1: Write the failing tests**

Add to `internal/clients/jira/tracker_test.go` (check the file's existing package clause and helpers
first; if the file does not exist, create it as `package jira` or `package jira_test` to match how
`normalizeIssue`'s visibility requires — it is unexported, so an in-package test file is needed):

```go
func TestNormalizeIssue_CarriesDescription(t *testing.T) {
	raw := map[string]any{
		"key": "PROJ-42",
		"fields": map[string]any{
			"summary":     "Fix the flaky correlator test",
			"description": "The tail-compaction test races the clock; see the boundary tiebreaker.",
		},
	}

	got := normalizeIssue(raw)

	assert.Equal(t, "PROJ-42", got.Key)
	assert.Equal(t, "Fix the flaky correlator test", got.Summary)
	assert.Equal(t, "The tail-compaction test races the clock; see the boundary tiebreaker.",
		got.Description,
		"description is the strongest matching signal; dropping it is why matching needs this field")
}

func TestNormalizeIssue_MissingDescriptionIsEmptyNotError(t *testing.T) {
	// A ticket with no description is ordinary, not exceptional.
	raw := map[string]any{
		"key":    "PROJ-42",
		"fields": map[string]any{"summary": "No body on this one"},
	}

	got := normalizeIssue(raw)

	assert.Empty(t, got.Description)
	assert.Equal(t, "No body on this one", got.Summary)
}

func TestNormalizeIssue_NonStringDescriptionIsIgnored(t *testing.T) {
	// Jira Cloud's v3 API can return an Atlassian Document Format object here
	// rather than a string. Coercing that to text is out of scope; the type
	// assertion must simply not panic and must leave Description empty.
	raw := map[string]any{
		"key": "PROJ-42",
		"fields": map[string]any{
			"summary":     "ADF description",
			"description": map[string]any{"type": "doc", "version": 1},
		},
	}

	got := normalizeIssue(raw)

	assert.Empty(t, got.Description, "an ADF object must not be stringified into garbage")
}
```

Add to `internal/clients/local/local_test.go`:

```go
func TestLocalGetIssue_CarriesDescription(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	key, err := s.InsertLocalIssue("PROJ", "Fix the flaky test",
		"Task", "Races the clock in tail compaction.", nil)
	require.NoError(t, err)

	got, err := local.New(s).GetIssue(key)

	require.NoError(t, err)
	assert.Equal(t, "Races the clock in tail compaction.", got.Description,
		"store.LocalIssue.Description already exists; it was only dropped at the tasktracker boundary")
}
```

Confirm `InsertLocalIssue`'s real signature before using it — the plan states it as
`(project, summary, issueType, description string, labels []string) (string, error)`, verified at
`store.go:394`, but check.

- [ ] **Step 2: Run to confirm they fail**

```bash
env -u GOROOT -u GOPATH go test ./internal/clients/jira/ ./internal/clients/local/ -run Description -v
```

Expected: compile failure — `got.Description` undefined on `tasktracker.Issue`.

- [ ] **Step 3: Add the field**

In `internal/tasktracker/tasktracker.go`, extend `Issue`:

```go
// Issue is a backend-normalized view of one tracked work item.
type Issue struct {
	Key     string
	Summary string
	// Description is the issue body. Populated on the live read path only —
	// the Jira collector deliberately does not emit description snapshots as
	// events, so this is the only place a never-edited ticket's body is
	// available. Narrative→issue matching compares against it, since a
	// one-line summary frequently cannot distinguish the ticket a narrative
	// implements from one it merely mentions.
	//
	// Empty when the backend has none, or when Jira returns an Atlassian
	// Document Format object rather than a string (v3 API): rendering ADF to
	// text is deliberately out of scope, and an empty description degrades
	// matching rather than breaking it.
	Description    string
	StatusCategory StatusCategory
	StatusName     string // native display value; "" if the backend has none
	Labels         []string
}
```

In `internal/clients/jira/tracker.go`, inside `normalizeIssue`, immediately after the existing
`issue.Summary, _ = fields["summary"].(string)` line:

```go
	// A failed assertion leaves this empty, which is the intended degradation
	// for an ADF object (Jira Cloud v3) rather than a string.
	issue.Description, _ = fields["description"].(string)
```

In `internal/clients/local/local.go`, inside `toIssue`, add the field to the returned literal:

```go
		Description:    issue.Description,
```

- [ ] **Step 4: Run to confirm they pass**

```bash
env -u GOROOT -u GOPATH go test ./internal/clients/jira/ ./internal/clients/local/ ./internal/tasktracker/ -v 2>&1 | tail -20
env -u GOROOT -u GOPATH go build ./...
```

Expected: new tests PASS, every pre-existing test in those packages still passes, build clean.

- [ ] **Step 5: Prove the ADF test discriminates**

Temporarily change the assertion to a plain `fields["description"]` assignment via `fmt.Sprint`
(i.e. stringify whatever is there). Confirm `TestNormalizeIssue_NonStringDescriptionIsIgnored` FAILS
because `Description` is now `map[type:doc version:1]`. Restore. Paste both outputs.

- [ ] **Step 6: Commit**

```bash
export PATH="$HOME/.asdf/installs/golang/1.25.7/bin:$PATH"
env -u GOROOT -u GOPATH golangci-lint fmt ./internal/tasktracker/... ./internal/clients/...
env -u GOROOT -u GOPATH golangci-lint run ./internal/tasktracker/... ./internal/clients/...
git add internal/tasktracker/tasktracker.go internal/clients/jira/tracker.go \
        internal/clients/jira/tracker_test.go internal/clients/local/local.go \
        internal/clients/local/local_test.go
git commit -m "Add tasktracker.Issue.Description for matching's live read path"
```

---

## Task 2: `config.MatchConfig`

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `config/unjira.example.json`

- [ ] **Step 1: Write the failing tests**

Add to `internal/config/config_test.go`:

```go
func TestMatchConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     config.MatchConfig
		wantErr string // substring; "" means no error expected
	}{
		{name: "zero max means default", cfg: config.MatchConfig{ConfidenceFloor: 0.7}},
		{name: "explicit max", cfg: config.MatchConfig{MaxCandidatesPerNarrative: 5, ConfidenceFloor: 0.7}},
		{name: "floor zero is valid", cfg: config.MatchConfig{ConfidenceFloor: 0}},
		{name: "floor one is valid", cfg: config.MatchConfig{ConfidenceFloor: 1}},
		{
			name:    "negative max",
			cfg:     config.MatchConfig{MaxCandidatesPerNarrative: -1},
			wantErr: "max_candidates_per_narrative",
		},
		{
			name:    "floor above one never promotes",
			cfg:     config.MatchConfig{ConfidenceFloor: 1.5},
			wantErr: "confidence_floor",
		},
		{
			name:    "negative floor",
			cfg:     config.MatchConfig{ConfidenceFloor: -0.1},
			wantErr: "confidence_floor",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()

			if tt.wantErr == "" {
				require.NoError(t, err)

				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr,
				"the message must name the JSON key so an operator can find it")
		})
	}
}

func TestMatchConfig_CandidateLimitDefaults(t *testing.T) {
	assert.Equal(t, config.DefaultMaxCandidatesPerNarrative,
		config.MatchConfig{}.CandidateLimit())
	assert.Equal(t, 5, config.MatchConfig{MaxCandidatesPerNarrative: 5}.CandidateLimit())
}

func TestLoad_ParsesMatchBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unjira.config.json")
	body := `{"match":{"max_candidates_per_narrative":7,"confidence_floor":0.8}}`
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	cfg, err := config.Load(path)

	require.NoError(t, err)
	assert.Equal(t, 7, cfg.Match.MaxCandidatesPerNarrative)
	assert.InDelta(t, 0.8, cfg.Match.ConfidenceFloor, 1e-9)
}
```

- [ ] **Step 2: Run to confirm they fail**

```bash
env -u GOROOT -u GOPATH go test ./internal/config/ -run Match -v
```

Expected: compile failure — `undefined: config.MatchConfig`.

- [ ] **Step 3: Implement**

In `internal/config/config.go`:

```go
// DefaultMaxCandidatesPerNarrative bounds how many candidate issue keys one
// narrative's matching pass examines. A limit is required rather than optional
// because each candidate costs a GetIssue call and a slot in the LLM prompt, so
// a narrative that mentions thirty tickets would otherwise be unboundedly
// expensive.
const DefaultMaxCandidatesPerNarrative = 10

// MatchConfig tunes narrative→issue matching. See
// docs/superpowers/specs/2026-08-24-narrative-issue-matching-design.md.
type MatchConfig struct {
	// MaxCandidatesPerNarrative caps candidates examined per narrative. Zero
	// means DefaultMaxCandidatesPerNarrative.
	MaxCandidatesPerNarrative int `json:"max_candidates_per_narrative"`
	// ConfidenceFloor governs only whether a primary is promoted into the
	// denormalized narratives.issue_key. Below it, every narrative_issues row
	// is still written — including the primary — but narratives.issue_key stays
	// NULL. The floor governs what unjira asserts, not what it records:
	// dropping the rows would make a low-confidence match indistinguishable
	// from finding nothing at all.
	ConfidenceFloor float64 `json:"confidence_floor"`
}

// CandidateLimit returns the effective per-narrative candidate cap.
func (c MatchConfig) CandidateLimit() int {
	if c.MaxCandidatesPerNarrative == 0 {
		return DefaultMaxCandidatesPerNarrative
	}

	return c.MaxCandidatesPerNarrative
}

// Validate rejects configurations that would fail silently at runtime.
//
// A ConfidenceFloor above 1 is the important case: confidence is a 0..1 score,
// so a floor of, say, 1.5 would never promote any primary, and matching would
// present as broken rather than as misconfigured.
func (c MatchConfig) Validate() error {
	if c.MaxCandidatesPerNarrative < 0 {
		return fmt.Errorf(
			"match.max_candidates_per_narrative is %d: must be positive, or omitted for the default of %d",
			c.MaxCandidatesPerNarrative, DefaultMaxCandidatesPerNarrative,
		)
	}

	if c.ConfidenceFloor < 0 || c.ConfidenceFloor > 1 {
		return fmt.Errorf(
			"match.confidence_floor is %v: must be within [0, 1] — confidence is a 0..1 score, so a "+
				"floor above 1 would never promote a primary and matching would look broken",
			c.ConfidenceFloor,
		)
	}

	return nil
}
```

Add the field to `Config` (find the struct and place it beside `Correlator`):

```go
	Match MatchConfig `json:"match"`
```

Check how `CorrelatorConfig.Validate` is invoked from `Config`-level validation (or from
`cmd/unjira`) and wire `Match.Validate()` the same way — grep for `Correlator.Validate` to find every
call site. If `CorrelatorConfig.Validate` is called from `cmd/unjira`'s startup rather than from a
`Config.Validate`, follow that; report which you found.

- [ ] **Step 4: Add the example config block**

In `config/unjira.example.json`, add a sibling to the existing `correlator` block, matching the
file's indentation exactly:

```json
  "match": {
    "max_candidates_per_narrative": 10,
    "confidence_floor": 0.7
  },
```

- [ ] **Step 5: Run and verify**

```bash
env -u GOROOT -u GOPATH go test ./internal/config/ -v 2>&1 | tail -20
python3 -c "import json; json.load(open('config/unjira.example.json'))" && echo VALID
env -u GOROOT -u GOPATH go build ./...
```

- [ ] **Step 6: Prove the floor test discriminates**

Temporarily remove the `c.ConfidenceFloor > 1` half of the condition. Confirm the
`floor above one never promotes` subtest FAILS. Restore. Paste both outputs.

- [ ] **Step 7: Commit**

```bash
export PATH="$HOME/.asdf/installs/golang/1.25.7/bin:$PATH"
env -u GOROOT -u GOPATH golangci-lint fmt ./internal/config/...
env -u GOROOT -u GOPATH golangci-lint run ./internal/config/...
git add internal/config/config.go internal/config/config_test.go config/unjira.example.json
git commit -m "Add config.MatchConfig with a floor that cannot silently never-promote"
```

---

## Task 3: `narrative_issues` schema, types, and store accessors

**Files:**
- Modify: `internal/store/store.go`
- Modify: `internal/store/store_test.go`

The biggest task, and the one the slice-3 lesson is about: that slice shipped without any way to
assemble `Cluster`'s inputs because every test hand-built them. All six accessors are specified up
front here so none is discovered mid-implementation.

- [ ] **Step 1: Write the failing tests**

Add to `internal/store/store_test.go`. Note the existing `openStore(t)` helper and `makeEvent(id)`
helper — read them first and reuse rather than duplicating.

```go
// insertNarrativeForTest creates a narrative and returns its id.
func insertNarrativeForTest(t *testing.T, s *store.Store, title string) int64 {
	t.Helper()

	start := time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)
	id, err := s.InsertNarrative(start, start.Add(time.Hour), title, "summary text")
	require.NoError(t, err)

	return id
}

func TestNarrativesWithoutIssueKey_ExcludesLinkedOnes(t *testing.T) {
	s := openStore(t)

	unlinked := insertNarrativeForTest(t, s, "unlinked work")
	linked := insertNarrativeForTest(t, s, "linked work")

	require.NoError(t, s.WithTx(func(tx *store.Tx) error {
		return tx.SetNarrativeIssueLink(linked, "PROJ-1", 0.9)
	}))

	got, err := s.NarrativesWithoutIssueKey(10)

	require.NoError(t, err)
	ids := make([]int64, 0, len(got))
	for _, n := range got {
		ids = append(ids, n.ID)
	}
	assert.Equal(t, []int64{unlinked}, ids,
		"a narrative with a primary must not be re-matched every pass")
}

func TestNarrativesWithoutIssueKey_RespectsLimit(t *testing.T) {
	s := openStore(t)
	for i := range 5 {
		insertNarrativeForTest(t, s, fmt.Sprintf("narrative %d", i))
	}

	got, err := s.NarrativesWithoutIssueKey(3)

	require.NoError(t, err)
	assert.Len(t, got, 3)
}

func TestSetNarrativeIssueLink_SetsKeyAndConfidence(t *testing.T) {
	s := openStore(t)
	id := insertNarrativeForTest(t, s, "work")

	require.NoError(t, s.WithTx(func(tx *store.Tx) error {
		return tx.SetNarrativeIssueLink(id, "PROJ-42", 0.83)
	}))

	row, err := s.GetNarrative(id)

	require.NoError(t, err)
	assert.Equal(t, "PROJ-42", row.IssueKey)
	assert.InDelta(t, 0.83, row.Confidence, 1e-9)
}

func TestAddNarrativeIssues_RoundTrips(t *testing.T) {
	s := openStore(t)
	id := insertNarrativeForTest(t, s, "work")

	links := []store.NarrativeIssue{
		{IssueKey: "PAAS-1", Role: "primary", Provenance: "branch", Confidence: 0.9, Connection: "corp"},
		{IssueKey: "SUMO-2", Role: "same_work", Provenance: "prose_first", Confidence: 0.7, Connection: "corp"},
		{IssueKey: "OTHER-3", Role: "mentioned", Provenance: "prose_later", Confidence: 0.2, Connection: "corp"},
	}
	require.NoError(t, s.WithTx(func(tx *store.Tx) error {
		return tx.AddNarrativeIssues(id, links)
	}))

	got, err := s.NarrativeIssues(id)

	require.NoError(t, err)
	require.Len(t, got, 3)
	byKey := map[string]store.NarrativeIssue{}
	for _, l := range got {
		byKey[l.IssueKey] = l
	}
	assert.Equal(t, "primary", byKey["PAAS-1"].Role)
	assert.Equal(t, "branch", byKey["PAAS-1"].Provenance)
	assert.Equal(t, "same_work", byKey["SUMO-2"].Role)
	assert.InDelta(t, 0.7, byKey["SUMO-2"].Confidence, 1e-9)
	assert.Equal(t, "corp", byKey["SUMO-2"].Connection)
}

func TestAddNarrativeIssues_IsIdempotent(t *testing.T) {
	// Matching is re-runnable over a backlog, so a second pass over the same
	// narrative must not duplicate rows — the property (source, external_id)
	// gives collectors.
	s := openStore(t)
	id := insertNarrativeForTest(t, s, "work")
	links := []store.NarrativeIssue{
		{IssueKey: "PAAS-1", Role: "primary", Provenance: "branch", Confidence: 0.9},
	}

	for range 2 {
		require.NoError(t, s.WithTx(func(tx *store.Tx) error {
			return tx.AddNarrativeIssues(id, links)
		}))
	}

	got, err := s.NarrativeIssues(id)

	require.NoError(t, err)
	assert.Len(t, got, 1, "re-running matching must not duplicate links")
}

func TestAddNarrativeIssues_RejectsSecondPrimary(t *testing.T) {
	// narratives.issue_key denormalizes the primary, so two primaries would
	// make that column arbitrary. Enforced by a partial unique index, not by
	// convention.
	s := openStore(t)
	id := insertNarrativeForTest(t, s, "work")

	require.NoError(t, s.WithTx(func(tx *store.Tx) error {
		return tx.AddNarrativeIssues(id, []store.NarrativeIssue{
			{IssueKey: "PAAS-1", Role: "primary", Provenance: "branch", Confidence: 0.9},
		})
	}))

	err := s.WithTx(func(tx *store.Tx) error {
		return tx.AddNarrativeIssues(id, []store.NarrativeIssue{
			{IssueKey: "OTHER-9", Role: "primary", Provenance: "prose_first", Confidence: 0.5},
		})
	})

	require.Error(t, err, "the database must refuse a second primary for one narrative")
}

func TestAddNarrativeIssues_AllowsSiblingsAndOtherNarrativesPrimary(t *testing.T) {
	// The partial index must constrain only role='primary' within one
	// narrative — not same_work/mentioned siblings, and not other narratives.
	s := openStore(t)
	first := insertNarrativeForTest(t, s, "first")
	second := insertNarrativeForTest(t, s, "second")

	require.NoError(t, s.WithTx(func(tx *store.Tx) error {
		return tx.AddNarrativeIssues(first, []store.NarrativeIssue{
			{IssueKey: "PAAS-1", Role: "primary", Provenance: "branch", Confidence: 0.9},
			{IssueKey: "SUMO-2", Role: "same_work", Provenance: "prose_first", Confidence: 0.7},
			{IssueKey: "SUMO-3", Role: "same_work", Provenance: "prose_later", Confidence: 0.6},
			{IssueKey: "NOPE-4", Role: "mentioned", Provenance: "prose_later", Confidence: 0.1},
		})
	}))

	err := s.WithTx(func(tx *store.Tx) error {
		return tx.AddNarrativeIssues(second, []store.NarrativeIssue{
			{IssueKey: "AAA-1", Role: "primary", Provenance: "branch", Confidence: 0.9},
		})
	})

	require.NoError(t, err, "each narrative gets its own primary")
}

func TestNarrativesForIssue_FindsEveryNarrativeAcrossRoles(t *testing.T) {
	// The reverse direction. The reconciler needs it to avoid commenting twice
	// on a SUMO ticket shared by two narratives.
	s := openStore(t)
	first := insertNarrativeForTest(t, s, "first")
	second := insertNarrativeForTest(t, s, "second")

	require.NoError(t, s.WithTx(func(tx *store.Tx) error {
		if err := tx.AddNarrativeIssues(first, []store.NarrativeIssue{
			{IssueKey: "SUMO-9", Role: "same_work", Provenance: "prose_first", Confidence: 0.7},
		}); err != nil {
			return err
		}

		return tx.AddNarrativeIssues(second, []store.NarrativeIssue{
			{IssueKey: "SUMO-9", Role: "primary", Provenance: "branch", Confidence: 0.95},
		})
	}))

	got, err := s.NarrativesForIssue("SUMO-9")

	require.NoError(t, err)
	require.Len(t, got, 2)
	roles := map[int64]store.Role{}
	for _, r := range got {
		roles[r.NarrativeID] = r.Role
	}
	assert.Equal(t, store.Role("same_work"), roles[first])
	assert.Equal(t, store.Role("primary"), roles[second])
}

func TestAllNarrativeEvents_IncludesPreCompactionBoundaryEvents(t *testing.T) {
	// THE most important test in this task. NarrativeEventsForContext
	// deliberately excludes events at or before the compaction boundary;
	// matching must see every event ever linked, or a compacted narrative
	// loses the git_branch artifact carrying its strongest signal.
	//
	// The fixture MUST set a compaction boundary — without one, this test
	// passes against NarrativeEventsForContext too and proves nothing.
	s := openStore(t)
	id := insertNarrativeForTest(t, s, "compacted work")

	early := makeEvent("early-event")
	early.OccurredAt = time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)
	early.Artifacts["git_branch"] = "feature/PROJ-42"
	late := makeEvent("late-event")
	late.OccurredAt = time.Date(2026, 8, 24, 11, 0, 0, 0, time.UTC)

	for _, e := range []events.Event{early, late} {
		inserted, err := s.InsertEvent(e)
		require.NoError(t, err)
		require.True(t, inserted)
	}

	earlyID, err := s.EventIDByExternalID(early.Source, early.ExternalID)
	require.NoError(t, err)
	lateID, err := s.EventIDByExternalID(late.Source, late.ExternalID)
	require.NoError(t, err)
	require.NoError(t, s.AddNarrativeEvents(id, []int64{earlyID, lateID}))

	// Compact past the early event.
	require.NoError(t, s.SetCompactionBoundary(id, early.OccurredAt, earlyID, "recap"))

	context, err := s.NarrativeEventsForContext(id)
	require.NoError(t, err)
	require.Len(t, context, 1, "fixture sanity: the boundary must actually hide the early event")

	all, err := s.AllNarrativeEvents(id)

	require.NoError(t, err)
	require.Len(t, all, 2, "matching must see pre-boundary events")
	assert.Equal(t, "early-event", all[0].ExternalID)
	assert.Equal(t, "feature/PROJ-42", all[0].Artifacts["git_branch"],
		"the branch artifact is the strongest provenance signal and lives on the oldest event")
}
```

Verify `makeEvent`'s real behaviour first (`store_test.go:17-27`): it uses a fixed timestamp and sets
`ticket_keys`. Overriding `OccurredAt` as above is required for the boundary test. Also confirm
`SetCompactionBoundary`'s exact signature and `EventIDByExternalID`'s parameter order.

- [ ] **Step 2: Run to confirm they fail**

```bash
env -u GOROOT -u GOPATH go test ./internal/store/ -run 'NarrativeIssue|NarrativesForIssue|NarrativesWithout|AllNarrativeEvents|SetNarrativeIssueLink' -v
```

Expected: compile failure — `undefined: store.NarrativeIssue`, `tx.SetNarrativeIssueLink undefined`,
etc.

- [ ] **Step 3: Add the DDL**

In `internal/store/store.go`'s schema string, after the `narrative_events` table:

```sql
CREATE TABLE IF NOT EXISTS narrative_issues (
    narrative_id INTEGER NOT NULL REFERENCES narratives (id),
    issue_key    TEXT    NOT NULL,
    role         TEXT    NOT NULL,   -- primary | same_work | mentioned
    provenance   TEXT    NOT NULL,   -- branch | jira_event | prose_first | prose_later
    confidence   REAL,
    connection   TEXT,               -- which JiraConnection resolved it
    created_at   TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    PRIMARY KEY (narrative_id, issue_key)
);

-- Exactly one primary per narrative, enforced by the database rather than by
-- convention: narratives.issue_key denormalizes the primary, so a second
-- primary row would make that column arbitrary. The composite PRIMARY KEY
-- above prevents duplicate keys per narrative but permits two rows both marked
-- primary, which is the case this closes.
CREATE UNIQUE INDEX IF NOT EXISTS one_primary_per_narrative
    ON narrative_issues (narrative_id) WHERE role = 'primary';

-- Supports the reverse lookup (issue_key -> narratives), which is otherwise
-- unindexed since issue_key is the trailing column of the composite key.
CREATE INDEX IF NOT EXISTS narrative_issues_by_key
    ON narrative_issues (issue_key);
```

- [ ] **Step 4: Add the types and accessors**

```go
// Role is which relationship a narrative has to an issue. A closed set: an
// unrecognized value is a parse error upstream, never persisted.
type Role string

// NarrativeIssue is one (narrative, issue) link — the narrative_issues row
// shape, mirroring how NarrativeRow mirrors narratives.
//
// There is deliberately no issue-to-issue column: every row relates a
// narrative to an issue, so co-representation of one body of work across two
// tickets is expressed by two rows sharing a narrative_id, and symmetry is
// derived rather than stored in two places that could disagree.
type NarrativeIssue struct {
	IssueKey   string
	Role       Role
	Provenance string
	Confidence float64
	// Connection is the config.JiraConnection.Name that resolved this key,
	// recorded because a co-representation can live on a different site than
	// the primary.
	Connection string
}

// NarrativeIssueRef is one row of the reverse lookup: the caller already knows
// the issue key and needs the narrative.
type NarrativeIssueRef struct {
	NarrativeID int64
	Role        Role
	Confidence  float64
}

// NarrativesWithoutIssueKey returns up to limit narratives with no primary
// issue link, oldest window first.
//
// This is matching's work queue, and selecting on the absence of a link is what
// makes the pass re-runnable: a tracker outage defers matching rather than
// losing it, and a narrative whose confidence fell below the floor stays
// selectable so a later pass can revisit it.
func (s *Store) NarrativesWithoutIssueKey(limit int) ([]NarrativeRow, error) {
	rows, err := s.db.Query(
		`SELECT id, window_start, window_end, title, summary, issue_key, confidence, status,
		        compaction_boundary, compaction_boundary_event_id
		 FROM narratives
		 WHERE issue_key IS NULL OR issue_key = ''
		 ORDER BY window_start, id
		 LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("querying narratives without an issue key: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []NarrativeRow
	for rows.Next() {
		row, err := scanNarrativeRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning unmatched narrative row: %w", err)
		}
		out = append(out, row)
	}

	return out, rows.Err()
}

// NarrativeIssues returns every issue link on one narrative.
func (s *Store) NarrativeIssues(narrativeID int64) ([]NarrativeIssue, error) {
	rows, err := s.db.Query(
		`SELECT issue_key, role, provenance, confidence, connection
		 FROM narrative_issues
		 WHERE narrative_id = ?
		 ORDER BY issue_key`,
		narrativeID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying issue links for narrative %d: %w", narrativeID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []NarrativeIssue
	for rows.Next() {
		var (
			link       NarrativeIssue
			confidence sql.NullFloat64
			connection sql.NullString
		)
		if err := rows.Scan(&link.IssueKey, &link.Role, &link.Provenance,
			&confidence, &connection); err != nil {
			return nil, fmt.Errorf("scanning issue link for narrative %d: %w", narrativeID, err)
		}
		link.Confidence = confidence.Float64
		link.Connection = connection.String
		out = append(out, link)
	}

	return out, rows.Err()
}

// NarrativesForIssue is the reverse direction: every narrative linking this
// issue, in any role.
//
// Matching never needs it — it walks narrative -> issues. The reconciler will:
// before commenting on a same_work ticket it must ask whether another narrative
// already updated it, and a change-management ticket shared across projects
// makes that a real duplicate-comment risk rather than a hypothetical one.
func (s *Store) NarrativesForIssue(issueKey string) ([]NarrativeIssueRef, error) {
	rows, err := s.db.Query(
		`SELECT narrative_id, role, confidence
		 FROM narrative_issues
		 WHERE issue_key = ?
		 ORDER BY narrative_id`,
		issueKey,
	)
	if err != nil {
		return nil, fmt.Errorf("querying narratives for issue %s: %w", issueKey, err)
	}
	defer func() { _ = rows.Close() }()

	var out []NarrativeIssueRef
	for rows.Next() {
		var (
			ref        NarrativeIssueRef
			confidence sql.NullFloat64
		)
		if err := rows.Scan(&ref.NarrativeID, &ref.Role, &confidence); err != nil {
			return nil, fmt.Errorf("scanning narrative ref for issue %s: %w", issueKey, err)
		}
		ref.Confidence = confidence.Float64
		out = append(out, ref)
	}

	return out, rows.Err()
}

// AllNarrativeEvents returns every event ever linked to a narrative, oldest
// first — including events at or before a compaction boundary.
//
// This is deliberately NOT NarrativeEventsForContext, which excludes
// pre-boundary events because the LLM reads a recap of them instead. Matching
// must see them all: candidate keys come from event artifacts, and the
// git_branch artifact carrying the strongest provenance signal sits on a
// narrative's oldest events — exactly the ones compaction hides.
func (s *Store) AllNarrativeEvents(narrativeID int64) ([]events.Event, error) {
	rows, err := s.db.Query(
		`SELECT e.source, e.external_id, e.occurred_at, e.actor, e.summary, e.artifacts, e.raw_ref
		 FROM events e
		 JOIN narrative_events ne ON ne.event_id = e.id
		 WHERE ne.narrative_id = ?
		 ORDER BY e.occurred_at, e.id`,
		narrativeID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying all events for narrative %d: %w", narrativeID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []events.Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning event for narrative %d: %w", narrativeID, err)
		}
		out = append(out, e)
	}

	return out, rows.Err()
}

// setNarrativeIssueLinkImpl backs both receiver variants.
func setNarrativeIssueLinkImpl(db dbConn, id int64, issueKey string, confidence float64) error {
	if _, err := db.Exec(
		`UPDATE narratives SET issue_key = ?, confidence = ? WHERE id = ?`,
		issueKey, confidence, id,
	); err != nil {
		return fmt.Errorf("setting issue link %s on narrative %d: %w", issueKey, id, err)
	}

	return nil
}

// SetNarrativeIssueLink promotes one issue key into the narrative's
// denormalized primary pointer.
//
// Separate from AddNarrativeIssues because promotion is conditional on
// MatchConfig.ConfidenceFloor while recording the link is not: below the floor
// every narrative_issues row is still written and this is simply not called.
func (s *Store) SetNarrativeIssueLink(id int64, issueKey string, confidence float64) error {
	return setNarrativeIssueLinkImpl(s.db, id, issueKey, confidence)
}

// SetNarrativeIssueLink is the *Tx-scoped variant of
// (*Store).SetNarrativeIssueLink.
func (t *Tx) SetNarrativeIssueLink(id int64, issueKey string, confidence float64) error {
	return setNarrativeIssueLinkImpl(t.tx, id, issueKey, confidence)
}

// addNarrativeIssuesImpl backs both receiver variants.
func addNarrativeIssuesImpl(db dbConn, narrativeID int64, links []NarrativeIssue) error {
	for _, link := range links {
		// Idempotence is expressed as an explicit upsert on the composite key,
		// NOT as INSERT OR IGNORE.
		//
		// This is measured, not assumed: OR IGNORE also swallows the
		// one_primary_per_narrative violation. Inserting a second primary with
		// a different issue_key returns nil and silently keeps the FIRST
		// primary, so a wrong early match would quietly outrank the right one
		// and AddNarrativeIssues would report success. Verified against
		// modernc.org/sqlite v1.56.0: OR IGNORE -> nil error, 1 primary row;
		// plain INSERT -> "UNIQUE constraint failed:
		// narrative_issues.narrative_id (2067)".
		//
		// ON CONFLICT names the composite key explicitly, so re-running a pass
		// is still free, while a second-primary conflict is on a different
		// index and therefore still raised.
		if _, err := db.Exec(
			`INSERT INTO narrative_issues
			   (narrative_id, issue_key, role, provenance, confidence, connection)
			 VALUES (?, ?, ?, ?, ?, ?)
			 ON CONFLICT (narrative_id, issue_key) DO UPDATE SET
			   role       = excluded.role,
			   provenance = excluded.provenance,
			   confidence = excluded.confidence,
			   connection = excluded.connection`,
			narrativeID, link.IssueKey, string(link.Role), link.Provenance,
			link.Confidence, link.Connection,
		); err != nil {
			return fmt.Errorf("adding issue link %s (%s) to narrative %d: %w",
				link.IssueKey, link.Role, narrativeID, err)
		}
	}

	return nil
}

// AddNarrativeIssues records issue links for a narrative, idempotently.
func (s *Store) AddNarrativeIssues(narrativeID int64, links []NarrativeIssue) error {
	return addNarrativeIssuesImpl(s.db, narrativeID, links)
}

// AddNarrativeIssues is the *Tx-scoped variant of
// (*Store).AddNarrativeIssues.
func (t *Tx) AddNarrativeIssues(narrativeID int64, links []NarrativeIssue) error {
	return addNarrativeIssuesImpl(t.tx, narrativeID, links)
}
```

**Scrutinise this draft.** Three specific things:

1. **The upsert form is load-bearing — do not "simplify" it to `INSERT OR IGNORE`.** I wrote
   `OR IGNORE` in the first draft of this plan and measured it wrong: it swallows the
   `one_primary_per_narrative` violation entirely (nil error, first primary retained), so a wrong
   early match would silently outrank the correct one while the call reported success. The explicit
   `ON CONFLICT (narrative_id, issue_key)` keeps re-runs free without suppressing the other index.
   `TestAddNarrativeIssues_RejectsSecondPrimary` is the test that catches a regression here; if you
   ever see it fail, check whether the statement drifted back to `OR IGNORE`.

   Note the `DO UPDATE` also means a re-run *refreshes* role/provenance/confidence rather than
   leaving stale values — which is what you want when a later pass has better information (e.g. once
   the rules loader lands). Confirm that is acceptable for your reading of idempotence; the
   alternative (`DO NOTHING`) would freeze the first verdict forever.
2. **`NarrativesWithoutIssueKey`'s predicate.** `issue_key IS NULL OR issue_key = ''` — check what
   `InsertNarrative` actually stores for an unset key (NULL, or empty string?). Reading
   `insertNarrativeImpl` answers it. If it stores NULL, the `= ''` half is harmless defence; if empty
   string, the `IS NULL` half is.
3. **`sql` import.** `sql.NullFloat64`/`sql.NullString` need `database/sql`, already imported in
   `store.go` — confirm.

- [ ] **Step 5: Run to confirm they pass**

```bash
env -u GOROOT -u GOPATH go test -count=1 ./internal/store/ -v 2>&1 | tail -30
env -u GOROOT -u GOPATH go test -count=1 ./... 2>&1 | grep -Ev "^ok|no test files"; echo "(blank = all pass)"
```

- [ ] **Step 6: Break-it drills (REQUIRED — two of them)**

**Drill A — `AllNarrativeEvents` really sees pre-boundary events.** Change `AllNarrativeEvents` to
delegate to `NarrativeEventsForContext`. Confirm
`TestAllNarrativeEvents_IncludesPreCompactionBoundaryEvents` FAILS on the length assertion. Restore.

This drill is why the fixture sets a compaction boundary and asserts `context` has exactly 1 event
first: without that sanity check, a fixture with no boundary would make both functions behave
identically and the test would pass either way.

**Drill B — the partial index really constrains.** Remove the `WHERE role = 'primary'` clause from
the index (making it a plain unique index on `narrative_id`). Confirm
`TestAddNarrativeIssues_AllowsSiblingsAndOtherNarrativesPrimary` now FAILS, because the
`same_work` sibling collides. Restore, and confirm both primary tests pass.

Paste all four outputs.

- [ ] **Step 7: Commit**

```bash
export PATH="$HOME/.asdf/installs/golang/1.25.7/bin:$PATH"
env -u GOROOT -u GOPATH golangci-lint fmt ./internal/store/...
env -u GOROOT -u GOPATH golangci-lint run ./internal/store/...
git add internal/store/store.go internal/store/store_test.go
git commit -m "Add narrative_issues: links, reverse lookup, one-primary invariant"
```

---

## Task 4: `Role`, `Provenance`, and the response parser (pure)

**Files:**
- Create: `internal/correlator/match_types.go`
- Create: `internal/correlator/match_types_test.go`
- Modify: `internal/correlator/export_test.go`

Pure types and parsing, no I/O. Split out so its tests need no database, no HTTP, and no LLM.

- [ ] **Step 1: Write the failing tests**

Create `internal/correlator/match_types_test.go` (`package correlator_test`; `fakeLLM` and other
helpers already exist there — do not redeclare them):

```go
func TestParseRole_AcceptsTheClosedSet(t *testing.T) {
	for _, want := range []correlator.Role{
		correlator.RolePrimary, correlator.RoleSameWork, correlator.RoleMentioned,
	} {
		got, err := correlator.ParseRoleForTest(string(want))

		require.NoError(t, err)
		assert.Equal(t, want, got)
	}
}

func TestParseRole_RejectsAnythingElse(t *testing.T) {
	// An open vocabulary would rot: if the model emits "relates", "related-to",
	// "blocked-by" and "blocker" as synonyms, every consumer must pattern-match
	// a set that grows without bound. Rejecting loudly is the alternative.
	tests := []string{"", "gating", "caused_by", "discovered_while", "relates", "PRIMARY", "primary "}

	for _, in := range tests {
		t.Run(fmt.Sprintf("%q", in), func(t *testing.T) {
			_, err := correlator.ParseRoleForTest(in)

			require.Error(t, err, "an unrecognized role must be a parse error, never coerced")
			assert.Contains(t, err.Error(), "role")
		})
	}
}

func TestParseMatchResponse_HappyPath(t *testing.T) {
	raw := `[
	  {"issue_key":"PAAS-1","role":"primary","confidence":0.92,"rationale":"the branch names it and the description matches"},
	  {"issue_key":"SUMO-2","role":"same_work","confidence":0.80,"rationale":"change-management record for the same deploy"},
	  {"issue_key":"NOPE-3","role":"mentioned","confidence":0.10,"rationale":"cited as prior art only"}
	]`

	got, err := correlator.ParseMatchResponseForTest(raw)

	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, "PAAS-1", got[0].IssueKey)
	assert.Equal(t, correlator.RolePrimary, got[0].Role)
	assert.InDelta(t, 0.92, got[0].Confidence, 1e-9)
	assert.Contains(t, got[0].Rationale, "branch")
	assert.Equal(t, correlator.RoleSameWork, got[1].Role)
}

func TestParseMatchResponse_StripsMarkdownFence(t *testing.T) {
	// Models add fences despite being told not to. The Jira collector slice hit
	// exactly this against a real model, so every parser in this package runs
	// raw output through stripJSONFence first.
	raw := "```json\n[{\"issue_key\":\"PAAS-1\",\"role\":\"primary\",\"confidence\":0.9,\"rationale\":\"x\"}]\n```"

	got, err := correlator.ParseMatchResponseForTest(raw)

	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "PAAS-1", got[0].IssueKey)
}

func TestParseMatchResponse_ErrorCases(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantMsg string
	}{
		{name: "not json", raw: "I think it's PAAS-1", wantMsg: "parsing"},
		{name: "not an array", raw: `{"issue_key":"PAAS-1"}`, wantMsg: "parsing"},
		{
			name:    "unknown role",
			raw:     `[{"issue_key":"PAAS-1","role":"gating","confidence":0.9,"rationale":"x"}]`,
			wantMsg: "role",
		},
		{
			name:    "missing issue key",
			raw:     `[{"role":"primary","confidence":0.9,"rationale":"x"}]`,
			wantMsg: "issue_key",
		},
		{
			name:    "two primaries",
			raw:     `[{"issue_key":"A-1","role":"primary","confidence":0.9,"rationale":"x"},{"issue_key":"B-2","role":"primary","confidence":0.8,"rationale":"y"}]`,
			wantMsg: "primary",
		},
		{
			name:    "confidence out of range",
			raw:     `[{"issue_key":"A-1","role":"primary","confidence":1.5,"rationale":"x"}]`,
			wantMsg: "confidence",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := correlator.ParseMatchResponseForTest(tt.raw)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantMsg)
		})
	}
}
```

The `two primaries` case matters: the database enforces one primary per narrative, so a response
with two would fail at write time with a constraint error naming a column rather than the model's
mistake. Catching it in the parser produces a diagnosable message instead.

- [ ] **Step 2: Run to confirm they fail**

```bash
env -u GOROOT -u GOPATH go test ./internal/correlator/ -run 'ParseRole|ParseMatchResponse' -v
```

Expected: compile failure — `undefined: correlator.ParseRoleForTest`.

- [ ] **Step 3: Implement**

Create `internal/correlator/match_types.go`:

```go
package correlator

import (
	"encoding/json"
	"fmt"

	"github.com/jcogilvie/unjira/internal/store"
)

// Role is which relationship a narrative has to an issue.
//
// Closed deliberately. Each role must justify itself by changing what a
// consumer does; otherwise the enum models English rather than consequences.
// Rejected: `gating` (the motivating PAAS/SUMO case turned out to be
// co-representation, not dependency — a true blocker is another narrative's
// work), and `caused_by`/`discovered_while` (no statable behavioural difference
// from `mentioned`; all three mean "do not attribute the work here"). The set
// grows additively when an action type needs the distinction.
type Role = store.Role

// The closed set. See docs/superpowers/specs/2026-08-24-narrative-issue-matching-design.md.
const (
	// RolePrimary is the record this work is principally tracked in. Exactly
	// one per narrative, enforced by a partial unique index in the schema, and
	// denormalized into narratives.issue_key.
	RolePrimary Role = "primary"
	// RoleSameWork is a co-representation of the same body of work in another
	// tracking context — e.g. an engineering ticket and the change-management
	// ticket recording the same deploy. Both are the work.
	RoleSameWork Role = "same_work"
	// RoleMentioned is referenced in the events but not the work. Recorded
	// rather than dropped so a narrative that looks untracked can be explained.
	RoleMentioned Role = "mentioned"
)

// Provenance records how a candidate key was found, so a wrong match is
// diagnosable afterwards rather than only visible as a bad outcome.
//
// The tiers are limited to what today's artifacts actually support. There is no
// commit-trailer tier because nothing records commit trailers, and prose keys
// are not separable from one another because claudecode dedupes them into one
// ordered slice — only first-mention order survives.
type Provenance string

const (
	// ProvenanceBranch came from the git_branch artifact. Strongest available.
	ProvenanceBranch Provenance = "branch"
	// ProvenanceJiraEvent came from a jira-source event's issue_key artifact.
	ProvenanceJiraEvent Provenance = "jira_event"
	// ProvenanceProseFirst is the first key mentioned in transcript text.
	ProvenanceProseFirst Provenance = "prose_first"
	// ProvenanceProseLater is a subsequent prose mention.
	ProvenanceProseLater Provenance = "prose_later"
)

// Rank orders provenance by how much it justifies attributing work. Lower is
// stronger. Used only as a tiebreaker and as a prior in the prompt; it never
// substitutes for verification.
func (p Provenance) Rank() int {
	switch p {
	case ProvenanceBranch:
		return 0
	case ProvenanceJiraEvent:
		return 1
	case ProvenanceProseFirst:
		return 2
	case ProvenanceProseLater:
		return 3
	default:
		return 4
	}
}

// parseRole converts a model-supplied string into a Role, rejecting anything
// outside the closed set.
func parseRole(raw string) (Role, error) {
	switch Role(raw) {
	case RolePrimary, RoleSameWork, RoleMentioned:
		return Role(raw), nil
	default:
		return "", fmt.Errorf(
			"unrecognized role %q: must be one of %q, %q, %q",
			raw, RolePrimary, RoleSameWork, RoleMentioned,
		)
	}
}

// matchVerdict is one classified candidate, as the model returns it.
type matchVerdict struct {
	IssueKey   string
	Role       Role
	Confidence float64
	Rationale  string
}

// rawVerdict is the wire shape, kept separate so Role can be validated during
// conversion rather than silently accepting any string.
type rawVerdict struct {
	IssueKey   string  `json:"issue_key"`
	Role       string  `json:"role"`
	Confidence float64 `json:"confidence"`
	Rationale  string  `json:"rationale"`
}

// parseMatchResponse decodes and validates the classification response.
//
// Validation is strict because every failure mode here is cheaper to catch as a
// parse error than downstream: an unknown role would otherwise be persisted and
// break consumers, and two primaries would hit the one_primary_per_narrative
// index and surface as a constraint error naming a column rather than naming the
// model's mistake.
func parseMatchResponse(raw string) ([]matchVerdict, error) {
	var wire []rawVerdict
	if err := json.Unmarshal([]byte(stripJSONFence(raw)), &wire); err != nil {
		return nil, fmt.Errorf("parsing match response: %w", err)
	}

	out := make([]matchVerdict, 0, len(wire))
	primaries := 0

	for i, w := range wire {
		if w.IssueKey == "" {
			return nil, fmt.Errorf("match response entry %d has an empty issue_key", i)
		}

		role, err := parseRole(w.Role)
		if err != nil {
			return nil, fmt.Errorf("match response entry %d (%s): %w", i, w.IssueKey, err)
		}

		if w.Confidence < 0 || w.Confidence > 1 {
			return nil, fmt.Errorf(
				"match response entry %d (%s) has confidence %v: must be within [0, 1]",
				i, w.IssueKey, w.Confidence)
		}

		if role == RolePrimary {
			primaries++
		}

		out = append(out, matchVerdict{
			IssueKey:   w.IssueKey,
			Role:       role,
			Confidence: w.Confidence,
			Rationale:  w.Rationale,
		})
	}

	if primaries > 1 {
		return nil, fmt.Errorf(
			"match response names %d primary issues: exactly one narrative_issues row per narrative "+
				"may be primary, since narratives.issue_key denormalizes it", primaries)
	}

	return out, nil
}
```

Note `Role` is a type alias to `store.Role` (`type Role = store.Role`, not `type Role store.Role`) so
a `correlator.Role` can be assigned straight into a `store.NarrativeIssue` without conversion.
**Verify that compiles** — if the alias creates an import cycle or reads badly, use a distinct type
plus an explicit conversion at the store boundary and say which you chose.

Add to `internal/correlator/export_test.go`:

```go
// ParseRoleForTest and ParseMatchResponseForTest expose the response parsers to
// the correlator_test package. They are unexported in production because
// nothing outside this package parses a model response, but their rejection
// behaviour is the main thing worth testing directly.
func ParseRoleForTest(raw string) (Role, error) { return parseRole(raw) }

func ParseMatchResponseForTest(raw string) ([]MatchVerdictForTest, error) {
	verdicts, err := parseMatchResponse(raw)
	if err != nil {
		return nil, err
	}

	out := make([]MatchVerdictForTest, 0, len(verdicts))
	for _, v := range verdicts {
		out = append(out, MatchVerdictForTest(v))
	}

	return out, nil
}

// MatchVerdictForTest mirrors the unexported matchVerdict so tests can assert
// on its fields.
type MatchVerdictForTest struct {
	IssueKey   string
	Role       Role
	Confidence float64
	Rationale  string
}
```

The `MatchVerdictForTest(v)` conversion requires identical field order and types — if it does not
compile, construct the struct field by field instead.

- [ ] **Step 4: Run to confirm they pass**

```bash
env -u GOROOT -u GOPATH go test -count=1 ./internal/correlator/ -v 2>&1 | tail -30
```

Expected: new tests PASS, every pre-existing correlator test still passes.

- [ ] **Step 5: Prove the unknown-role test discriminates**

Change `parseRole`'s `default` branch to `return Role(raw), nil` (accept anything). Confirm
`TestParseRole_RejectsAnythingElse` FAILS for every case, and that the `unknown role` subtest of
`TestParseMatchResponse_ErrorCases` also FAILS. Restore. Paste both outputs.

- [ ] **Step 6: Commit**

```bash
export PATH="$HOME/.asdf/installs/golang/1.25.7/bin:$PATH"
env -u GOROOT -u GOPATH golangci-lint fmt ./internal/correlator/...
env -u GOROOT -u GOPATH golangci-lint run ./internal/correlator/...
git add internal/correlator/match_types.go internal/correlator/match_types_test.go \
        internal/correlator/export_test.go
git commit -m "Add correlator Role/Provenance closed enums and a strict response parser"
```

---

## Task 5: deterministic candidate gathering (pure)

**Files:**
- Create: `internal/correlator/match_candidates.go`
- Create: `internal/correlator/match_candidates_test.go`

Pure: events in, ranked candidates out. No I/O, so the provenance rules are cheap to test
exhaustively.

- [ ] **Step 1: Write the failing tests**

Create `internal/correlator/match_candidates_test.go`:

```go
func claudeEvent(t *testing.T, externalID, branch string, ticketKeys ...string) events.Event {
	t.Helper()

	e := events.NewEvent("claude_code", externalID,
		time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC), "session summary")
	if branch != "" {
		e.Artifacts["git_branch"] = branch
	}
	if len(ticketKeys) > 0 {
		anyKeys := make([]any, 0, len(ticketKeys))
		for _, k := range ticketKeys {
			anyKeys = append(anyKeys, k)
		}
		e.Artifacts["ticket_keys"] = anyKeys
	}

	return e
}

func jiraEvent(t *testing.T, key string) events.Event {
	t.Helper()

	e := events.NewEvent("jira", key+":status:1",
		time.Date(2026, 8, 24, 10, 30, 0, 0, time.UTC), key+" status: To Do → Done")
	e.Artifacts["issue_key"] = key
	e.Artifacts["project_key"] = "PROJ"
	e.Artifacts["connection"] = "corp"

	return e
}

func TestGatherCandidates_BranchOutranksProse(t *testing.T) {
	// The branch is the strongest attribution signal available today: a session
	// mentioning three keys but committing on feature/PROJ-42 is implementing
	// PROJ-42.
	evts := []events.Event{
		claudeEvent(t, "s1", "feature/PROJ-42", "PROJ-100", "PROJ-205", "PROJ-42"),
	}

	got := correlator.GatherCandidatesForTest(evts, nil, 10)

	require.NotEmpty(t, got)
	assert.Equal(t, "PROJ-42", got[0].IssueKey)
	assert.Equal(t, correlator.ProvenanceBranch, got[0].Provenance)
}

func TestGatherCandidates_ProseOrderIsPreserved(t *testing.T) {
	evts := []events.Event{claudeEvent(t, "s1", "", "PROJ-100", "PROJ-205")}

	got := correlator.GatherCandidatesForTest(evts, nil, 10)

	require.Len(t, got, 2)
	assert.Equal(t, "PROJ-100", got[0].IssueKey)
	assert.Equal(t, correlator.ProvenanceProseFirst, got[0].Provenance)
	assert.Equal(t, "PROJ-205", got[1].IssueKey)
	assert.Equal(t, correlator.ProvenanceProseLater, got[1].Provenance)
}

func TestGatherCandidates_JiraArtifactIsACandidate(t *testing.T) {
	evts := []events.Event{jiraEvent(t, "PROJ-7")}

	got := correlator.GatherCandidatesForTest(evts, nil, 10)

	require.Len(t, got, 1)
	assert.Equal(t, "PROJ-7", got[0].IssueKey)
	assert.Equal(t, correlator.ProvenanceJiraEvent, got[0].Provenance)
	assert.Equal(t, "corp", got[0].Connection,
		"the connection artifact records which Jira site resolved this key")
}

func TestGatherCandidates_DedupesKeepingStrongestProvenance(t *testing.T) {
	// The same key can appear as prose in one event and as a branch in another.
	// It must appear once, at its strongest provenance.
	evts := []events.Event{
		claudeEvent(t, "s1", "", "PROJ-42"),
		claudeEvent(t, "s2", "feature/PROJ-42"),
	}

	got := correlator.GatherCandidatesForTest(evts, nil, 10)

	require.Len(t, got, 1, "one key, one candidate")
	assert.Equal(t, correlator.ProvenanceBranch, got[0].Provenance)
}

func TestGatherCandidates_HonorsExcludeFromLinking(t *testing.T) {
	// A placeholder key satisfying a commit linter is not a real link. It must
	// be dropped from candidacy but reported, so a narrative whose only key was
	// excluded still shows up as untracked and annotated rather than vanishing.
	compiled, err := events.CompileLinkExclusionPatterns([]string{`^NOJIRA-\d+$`})
	require.NoError(t, err)
	evts := []events.Event{claudeEvent(t, "s1", "", "NOJIRA-1", "PROJ-42")}

	got := correlator.GatherCandidatesForTest(evts, compiled, 10)

	require.Len(t, got, 1)
	assert.Equal(t, "PROJ-42", got[0].IssueKey)

	excluded := correlator.ExcludedCandidatesForTest(evts, compiled)
	assert.Equal(t, []string{"NOJIRA-1"}, excluded,
		"an excluded key must be recorded, not silently dropped")
}

func TestGatherCandidates_RespectsLimitStrongestFirst(t *testing.T) {
	// The cap bounds GetIssue fan-out and prompt size. Truncating must keep the
	// strongest candidates, or the limit would discard the answer.
	evts := []events.Event{
		claudeEvent(t, "s1", "feature/PROJ-9", "PROJ-1", "PROJ-2", "PROJ-3", "PROJ-4"),
	}

	got := correlator.GatherCandidatesForTest(evts, nil, 2)

	require.Len(t, got, 2)
	assert.Equal(t, "PROJ-9", got[0].IssueKey, "the branch candidate must survive truncation")
	assert.Equal(t, correlator.ProvenanceBranch, got[0].Provenance)
}

func TestGatherCandidates_NoKeysIsEmptyNotError(t *testing.T) {
	evts := []events.Event{claudeEvent(t, "s1", "main")}

	got := correlator.GatherCandidatesForTest(evts, nil, 10)

	assert.Empty(t, got, "untracked work is the default path, not an error")
}
```

- [ ] **Step 2: Run to confirm they fail**

```bash
env -u GOROOT -u GOPATH go test ./internal/correlator/ -run GatherCandidates -v
```

Expected: compile failure — `undefined: correlator.GatherCandidatesForTest`.

- [ ] **Step 3: Implement**

Create `internal/correlator/match_candidates.go`:

```go
package correlator

import (
	"regexp"
	"slices"

	"github.com/jcogilvie/unjira/internal/events"
)

// Candidate is one issue key a narrative might be attributed to, with how it
// was found.
type Candidate struct {
	IssueKey   string
	Provenance Provenance
	// Connection is the configured Jira connection name, when the key came from
	// a jira-source event that recorded one. Empty otherwise.
	Connection string
}

// gatherCandidates extracts every candidate issue key from a narrative's
// events, strongest provenance first, capped at limit.
//
// Deterministic and I/O-free by design: this is the correlator's "dumb"
// half, and keeping judgment out of it is what makes the LLM's job a narrow
// question ("which of these owns the work?") rather than an open one.
//
// The same key found twice keeps its strongest provenance, so a key mentioned
// in prose and also present in a branch name ranks as a branch candidate.
func gatherCandidates(
	evts []events.Event,
	linkExclusions []*regexp.Regexp,
	limit int,
) []Candidate {
	best := map[string]Candidate{}

	note := func(key string, prov Provenance, connection string) {
		if key == "" {
			return
		}

		existing, seen := best[key]
		if seen && existing.Provenance.Rank() <= prov.Rank() {
			// Keep the stronger provenance already recorded. Also keep its
			// connection: a jira_event candidate carries one and a prose
			// mention does not, so overwriting would lose it.
			return
		}

		if seen && connection == "" {
			connection = existing.Connection
		}

		best[key] = Candidate{IssueKey: key, Provenance: prov, Connection: connection}
	}

	for _, e := range evts {
		// A jira-source event names its issue structurally — no text parsing.
		if key, ok := e.Artifacts["issue_key"].(string); ok {
			connection, _ := e.Artifacts["connection"].(string)
			note(key, ProvenanceJiraEvent, connection)
		}

		// The branch is re-parsed here rather than read from ticket_keys
		// because claudecode flattens branch-derived and prose-derived keys into
		// one artifact, losing exactly the distinction that matters most.
		if branch, ok := e.Artifacts["git_branch"].(string); ok {
			for _, key := range events.ExtractTicketKeys(branch) {
				note(key, ProvenanceBranch, "")
			}
		}

		for i, key := range ticketKeys(e) {
			prov := ProvenanceProseLater
			if i == 0 {
				prov = ProvenanceProseFirst
			}
			note(key, prov, "")
		}
	}

	out := make([]Candidate, 0, len(best))
	for _, c := range best {
		out = append(out, c)
	}

	// Deterministic order: provenance strength, then key, so a pass is
	// reproducible and truncation is not arbitrary.
	slices.SortFunc(out, func(a, b Candidate) int {
		if r := a.Provenance.Rank() - b.Provenance.Rank(); r != 0 {
			return r
		}

		return strings.Compare(a.IssueKey, b.IssueKey)
	})

	out = dropExcluded(out, linkExclusions)

	if limit > 0 && len(out) > limit {
		// Truncate the weakest, never the strongest: the cap exists to bound
		// cost, and discarding a branch candidate to keep a prose mention would
		// throw away the answer.
		out = out[:limit]
	}

	return out
}

// dropExcluded removes candidates matching exclude_from_linking.
func dropExcluded(candidates []Candidate, linkExclusions []*regexp.Regexp) []Candidate {
	if len(linkExclusions) == 0 {
		return candidates
	}

	keys := make([]string, 0, len(candidates))
	for _, c := range candidates {
		keys = append(keys, c.IssueKey)
	}

	kept, _ := events.PartitionExcludedKeys(keys, linkExclusions)
	keep := make(map[string]bool, len(kept))
	for _, k := range kept {
		keep[k] = true
	}

	out := make([]Candidate, 0, len(kept))
	for _, c := range candidates {
		if keep[c.IssueKey] {
			out = append(out, c)
		}
	}

	return out
}

// excludedCandidates reports which keys exclude_from_linking removed, so a
// narrative that looks untracked can be explained by the placeholder that was
// dropped rather than appearing to have had no candidates at all.
func excludedCandidates(evts []events.Event, linkExclusions []*regexp.Regexp) []string {
	if len(linkExclusions) == 0 {
		return nil
	}

	all := gatherCandidates(evts, nil, 0)
	keys := make([]string, 0, len(all))
	for _, c := range all {
		keys = append(keys, c.IssueKey)
	}

	_, excluded := events.PartitionExcludedKeys(keys, linkExclusions)

	return excluded
}

// ticketKeys reads the claudecode ticket_keys artifact, which is stored as a
// []any of strings after a JSON round-trip through the events table.
func ticketKeys(e events.Event) []string {
	raw, ok := e.Artifacts["ticket_keys"].([]any)
	if !ok {
		return nil
	}

	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}

	return out
}
```

Add `"strings"` to the imports (used by `strings.Compare`).

**On `ticketKeys`' `[]any` assumption** — verified, so do not "defensively" widen it:
`claudecode` calls `toAnySlice(meta.orderedKeys)` *before* assigning the artifact
(`claudecode.go:239`), so the value is `[]any` in memory as well as after a JSON round-trip through
the events table. `internal/pipeline/digest.go`'s `stringArtifact` makes the same single-shape
assumption and is the existing convention (it is unexported, so it cannot be reused from here).

The one thing to get right is the **test fixtures**: `claudeEvent` above builds `[]any` deliberately.
If you write a fixture that assigns `[]string{...}` directly, `ticketKeys` returns nil, every prose
candidate silently disappears, and the test passes for the wrong reason. Keep the conversion in the
helper.

Add to `internal/correlator/export_test.go`:

```go
// GatherCandidatesForTest and ExcludedCandidatesForTest expose the pure
// candidate-gathering functions. They are unexported in production because only
// Match calls them, but the provenance ranking and exclusion rules are worth
// testing without a store or a tracker.
func GatherCandidatesForTest(
	evts []Event, linkExclusions []*regexp.Regexp, limit int,
) []Candidate {
	return gatherCandidates(evts, linkExclusions, limit)
}

func ExcludedCandidatesForTest(evts []Event, linkExclusions []*regexp.Regexp) []string {
	return excludedCandidates(evts, linkExclusions)
}
```

`export_test.go` will need `"regexp"` imported.

- [ ] **Step 4: Run to confirm they pass**

```bash
env -u GOROOT -u GOPATH go test -count=1 ./internal/correlator/ -v 2>&1 | tail -30
```

- [ ] **Step 5: Prove two tests discriminate**

1. Change the truncation to `out = out[len(out)-limit:]` (keep the weakest). Confirm
   `TestGatherCandidates_RespectsLimitStrongestFirst` FAILS.
2. Remove the `dropExcluded` call. Confirm `TestGatherCandidates_HonorsExcludeFromLinking` FAILS.

Restore after each; paste all four outputs.

- [ ] **Step 6: Commit**

```bash
export PATH="$HOME/.asdf/installs/golang/1.25.7/bin:$PATH"
env -u GOROOT -u GOPATH golangci-lint fmt ./internal/correlator/...
env -u GOROOT -u GOPATH golangci-lint run ./internal/correlator/...
git add internal/correlator/match_candidates.go internal/correlator/match_candidates_test.go \
        internal/correlator/export_test.go
git commit -m "Add deterministic candidate gathering with provenance ranking"
```

---

## Task 6: `correlator.Match`

**Files:**
- Create: `internal/correlator/match.go`
- Create: `internal/correlator/match_test.go`
- Modify: `internal/correlator/export_test.go`

The pass itself: verification, resolution, classification, persistence.

- [ ] **Step 1: Write the failing tests**

Create `internal/correlator/match_test.go`:

```go
// fakeTracker satisfies tasktracker.TaskTracker without a network call.
//
// issues is the set of keys that resolve; anything else returns notFound, which
// is how a hallucinated or stale key is simulated. getErr forces a transport
// failure for one key, to exercise per-narrative isolation.
type fakeTracker struct {
	issues   map[string]tasktracker.Issue
	getErr   map[string]error
	getCalls []string
}

func (f *fakeTracker) GetIssue(key string) (tasktracker.Issue, error) {
	f.getCalls = append(f.getCalls, key)

	if err, ok := f.getErr[key]; ok {
		return tasktracker.Issue{}, err
	}

	issue, ok := f.issues[key]
	if !ok {
		return tasktracker.Issue{}, fmt.Errorf("issue %s not found", key)
	}

	return issue, nil
}

func (f *fakeTracker) SearchIssues(string, int) ([]tasktracker.Issue, error) { return nil, nil }
func (f *fakeTracker) AddComment(string, string) error                      { return nil }
func (f *fakeTracker) SetStatus(string, tasktracker.StatusCategory) error   { return nil }
func (f *fakeTracker) CreateIssue(_, _, _, _ string, _ []string) (string, error) {
	return "", nil
}

// matchStore builds a store with one narrative whose events carry the given
// artifacts, and returns the narrative id.
func matchStore(t *testing.T, evts []events.Event) (*store.Store, int64) {
	t.Helper()

	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	start := time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)
	id, err := s.InsertNarrative(start, start.Add(2*time.Hour),
		"debugging the flaky correlator test", "summary text")
	require.NoError(t, err)

	ids := make([]int64, 0, len(evts))
	for _, e := range evts {
		inserted, err := s.InsertEvent(e)
		require.NoError(t, err)
		require.True(t, inserted)
		eid, err := s.EventIDByExternalID(e.Source, e.ExternalID)
		require.NoError(t, err)
		ids = append(ids, eid)
	}
	require.NoError(t, s.AddNarrativeEvents(id, ids))

	return s, id
}

func defaultMatchConfig() config.MatchConfig {
	return config.MatchConfig{MaxCandidatesPerNarrative: 10, ConfidenceFloor: 0.5}
}

func TestMatch_SingleCandidateNeedsNoLLM(t *testing.T) {
	// One surviving candidate is not a judgment call. Spending an LLM call on it
	// would be waste, and it is the common case.
	s, id := matchStore(t, []events.Event{claudeEvent(t, "s1", "feature/PROJ-42")})
	tracker := &fakeTracker{issues: map[string]tasktracker.Issue{
		"PROJ-42": {Key: "PROJ-42", Summary: "Fix the flaky test"},
	}}
	llmFake := &fakeLLM{}

	results, stats, err := correlator.Match(context.Background(), s, tracker, llmFake, defaultMatchConfig())

	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "PROJ-42", results[0].Primary)
	assert.Equal(t, 0, stats.Calls, "a single candidate must be resolved deterministically")
	assert.Empty(t, llmFake.prompts)

	row, err := s.GetNarrative(id)
	require.NoError(t, err)
	assert.Equal(t, "PROJ-42", row.IssueKey)
}

func TestMatch_ZeroCandidatesTouchesNothing(t *testing.T) {
	// Untracked work is the default path, not a special case: no tracker call,
	// no LLM call, narrative stays unlinked.
	s, id := matchStore(t, []events.Event{claudeEvent(t, "s1", "main")})
	tracker := &fakeTracker{}
	llmFake := &fakeLLM{}

	results, stats, err := correlator.Match(context.Background(), s, tracker, llmFake, defaultMatchConfig())

	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Empty(t, results[0].Primary)
	assert.Empty(t, results[0].Links)
	assert.Empty(t, tracker.getCalls, "no candidates means no verification calls")
	assert.Equal(t, 0, stats.Calls)

	row, err := s.GetNarrative(id)
	require.NoError(t, err)
	assert.Empty(t, row.IssueKey)
}

func TestMatch_VerifiesEveryCandidateAndDropsUnresolvable(t *testing.T) {
	// rules/verify-correlations.md: a hallucinated match can be high-confidence,
	// so resolution is mandatory regardless of provenance strength. A branch
	// name can also simply be stale.
	s, _ := matchStore(t, []events.Event{
		claudeEvent(t, "s1", "feature/GONE-1", "PROJ-42"),
	})
	tracker := &fakeTracker{issues: map[string]tasktracker.Issue{
		"PROJ-42": {Key: "PROJ-42", Summary: "Fix the flaky test"},
	}}
	llmFake := &fakeLLM{}

	results, _, err := correlator.Match(context.Background(), s, tracker, llmFake, defaultMatchConfig())

	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Contains(t, tracker.getCalls, "GONE-1", "the branch candidate must still be verified")
	assert.Equal(t, []string{"GONE-1"}, results[0].Unresolved)
	assert.Equal(t, "PROJ-42", results[0].Primary,
		"the surviving candidate wins once the stronger one fails to resolve")
}

func TestMatch_MultipleCandidatesClassifiedByLLM(t *testing.T) {
	// The PAAS/SUMO case: two tickets, one body of work, both legitimate.
	s, id := matchStore(t, []events.Event{
		claudeEvent(t, "s1", "feature/PAAS-1", "SUMO-2", "NOPE-3"),
	})
	tracker := &fakeTracker{issues: map[string]tasktracker.Issue{
		"PAAS-1": {Key: "PAAS-1", Summary: "Add the widget endpoint",
			Description: "Implement and ship the widget endpoint."},
		"SUMO-2": {Key: "SUMO-2", Summary: "System Change: widget endpoint to prod",
			Description: "Change-management record gating the prod deploy."},
		"NOPE-3": {Key: "NOPE-3", Summary: "Unrelated older ticket"},
	}}
	llmFake := &fakeLLM{responses: []string{`[
	  {"issue_key":"PAAS-1","role":"primary","confidence":0.93,"rationale":"the branch names it and it is the engineering record"},
	  {"issue_key":"SUMO-2","role":"same_work","confidence":0.85,"rationale":"change-management record for the same deploy"},
	  {"issue_key":"NOPE-3","role":"mentioned","confidence":0.10,"rationale":"cited only as prior art"}
	]`}}

	results, stats, err := correlator.Match(context.Background(), s, tracker, llmFake, defaultMatchConfig())

	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, 1, stats.Calls, "exactly one classification call for the whole narrative")
	assert.Equal(t, "PAAS-1", results[0].Primary)

	links, err := s.NarrativeIssues(id)
	require.NoError(t, err)
	require.Len(t, links, 3, "every candidate is recorded, including the merely mentioned one")

	byKey := map[string]store.NarrativeIssue{}
	for _, l := range links {
		byKey[l.IssueKey] = l
	}
	assert.Equal(t, store.Role("primary"), byKey["PAAS-1"].Role)
	assert.Equal(t, store.Role("same_work"), byKey["SUMO-2"].Role)
	assert.Equal(t, store.Role("mentioned"), byKey["NOPE-3"].Role)
	assert.Equal(t, "branch", byKey["PAAS-1"].Provenance)
}

func TestMatch_PromptCarriesSummaryAndDescription(t *testing.T) {
	// Description is why tasktracker.Issue gained the field: a one-line summary
	// often cannot distinguish the ticket being implemented from one mentioned.
	s, _ := matchStore(t, []events.Event{
		claudeEvent(t, "s1", "feature/PAAS-1", "SUMO-2"),
	})
	tracker := &fakeTracker{issues: map[string]tasktracker.Issue{
		"PAAS-1": {Key: "PAAS-1", Summary: "Add the widget endpoint",
			Description: "UNIQUEBODYONE implement and ship it"},
		"SUMO-2": {Key: "SUMO-2", Summary: "System Change: widget to prod",
			Description: "UNIQUEBODYTWO change-management gate"},
	}}
	llmFake := &fakeLLM{responses: []string{`[
	  {"issue_key":"PAAS-1","role":"primary","confidence":0.9,"rationale":"x"},
	  {"issue_key":"SUMO-2","role":"same_work","confidence":0.8,"rationale":"y"}
	]`}}

	_, _, err := correlator.Match(context.Background(), s, tracker, llmFake, defaultMatchConfig())

	require.NoError(t, err)
	require.Len(t, llmFake.prompts, 1)
	prompt := llmFake.prompts[0]
	assert.Contains(t, prompt, "UNIQUEBODYONE")
	assert.Contains(t, prompt, "UNIQUEBODYTWO")
	assert.Contains(t, prompt, "debugging the flaky correlator test", "the narrative title")
	assert.Contains(t, prompt, "branch", "provenance is a prior the model should see")
}

func TestMatch_ConfidenceFloorBlocksPromotionButNotRecording(t *testing.T) {
	// The floor governs what unjira asserts, not what it records. Dropping the
	// rows would make a weak match indistinguishable from finding nothing.
	s, id := matchStore(t, []events.Event{
		claudeEvent(t, "s1", "feature/PAAS-1", "SUMO-2"),
	})
	tracker := &fakeTracker{issues: map[string]tasktracker.Issue{
		"PAAS-1": {Key: "PAAS-1", Summary: "one"},
		"SUMO-2": {Key: "SUMO-2", Summary: "two"},
	}}
	llmFake := &fakeLLM{responses: []string{`[
	  {"issue_key":"PAAS-1","role":"primary","confidence":0.20,"rationale":"weak"},
	  {"issue_key":"SUMO-2","role":"mentioned","confidence":0.10,"rationale":"weaker"}
	]`}}

	results, _, err := correlator.Match(context.Background(), s, tracker, llmFake,
		config.MatchConfig{MaxCandidatesPerNarrative: 10, ConfidenceFloor: 0.5})

	require.NoError(t, err)
	assert.Empty(t, results[0].Primary, "0.20 is below the 0.5 floor")

	row, err := s.GetNarrative(id)
	require.NoError(t, err)
	assert.Empty(t, row.IssueKey, "the denormalized pointer stays unset")

	links, err := s.NarrativeIssues(id)
	require.NoError(t, err)
	assert.Len(t, links, 2, "every link is still recorded as evidence")

	// And it stays selectable, so a later pass can revisit it.
	pending, err := s.NarrativesWithoutIssueKey(10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, id, pending[0].ID)
}

func TestMatch_ExcludedKeysAreRecordedNotSilentlyDropped(t *testing.T) {
	s, _ := matchStore(t, []events.Event{claudeEvent(t, "s1", "", "NOJIRA-1")})
	tracker := &fakeTracker{}
	llmFake := &fakeLLM{}
	compiled, err := events.CompileLinkExclusionPatterns([]string{`^NOJIRA-\d+$`})
	require.NoError(t, err)

	results, _, err := correlator.Match(context.Background(), s, tracker, llmFake,
		defaultMatchConfig(), correlator.WithLinkExclusionsForTest(compiled))

	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Empty(t, results[0].Primary)
	assert.Equal(t, []string{"NOJIRA-1"}, results[0].Excluded,
		"the narrative reads as untracked *because* a placeholder was excluded, not for no reason")
	assert.Empty(t, tracker.getCalls, "an excluded key is never verified")
}

func TestMatch_OneNarrativeFailingDoesNotStopTheOthers(t *testing.T) {
	// Per-narrative isolation: a tracker 500 on one narrative leaves it
	// unmatched and moves on. Unmatched is a valid resting state, so a failed
	// pass costs a retry and nothing else.
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	start := time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)
	broken, err := s.InsertNarrative(start, start.Add(time.Hour), "broken", "s")
	require.NoError(t, err)
	fine, err := s.InsertNarrative(start.Add(2*time.Hour), start.Add(3*time.Hour), "fine", "s")
	require.NoError(t, err)

	link := func(nid int64, e events.Event) {
		inserted, ierr := s.InsertEvent(e)
		require.NoError(t, ierr)
		require.True(t, inserted)
		eid, ierr := s.EventIDByExternalID(e.Source, e.ExternalID)
		require.NoError(t, ierr)
		require.NoError(t, s.AddNarrativeEvents(nid, []int64{eid}))
	}
	link(broken, claudeEvent(t, "s-broken", "feature/BOOM-1"))
	link(fine, claudeEvent(t, "s-fine", "feature/OK-1"))

	tracker := &fakeTracker{
		issues: map[string]tasktracker.Issue{"OK-1": {Key: "OK-1", Summary: "fine"}},
		getErr: map[string]error{"BOOM-1": errors.New("jira 500")},
	}

	results, _, err := correlator.Match(context.Background(), s, tracker, &fakeLLM{}, defaultMatchConfig())

	require.Error(t, err, "the caller must see that a narrative failed")
	assert.Contains(t, err.Error(), "jira 500")

	fineRow, gerr := s.GetNarrative(fine)
	require.NoError(t, gerr)
	assert.Equal(t, "OK-1", fineRow.IssueKey, "the healthy narrative still matched")

	brokenRow, gerr := s.GetNarrative(broken)
	require.NoError(t, gerr)
	assert.Empty(t, brokenRow.IssueKey, "the failed narrative stays unmatched for a retry")

	_ = results
}

func TestMatch_IsIdempotent(t *testing.T) {
	s, id := matchStore(t, []events.Event{claudeEvent(t, "s1", "feature/PROJ-42")})
	tracker := &fakeTracker{issues: map[string]tasktracker.Issue{
		"PROJ-42": {Key: "PROJ-42", Summary: "Fix the flaky test"},
	}}

	for range 2 {
		_, _, err := correlator.Match(context.Background(), s, tracker, &fakeLLM{}, defaultMatchConfig())
		require.NoError(t, err)
	}

	links, err := s.NarrativeIssues(id)
	require.NoError(t, err)
	assert.Len(t, links, 1, "a second pass must not duplicate links")
}
```

`TestMatch_IsIdempotent` has a subtlety: after the first pass sets `issue_key`, the narrative is no
longer returned by `NarrativesWithoutIssueKey`, so the second `Match` is a no-op over an empty queue.
That is the intended behaviour and the test still guards it — but if you want the upsert path itself
exercised, call `AddNarrativeIssues` twice directly in the store test (Task 3 already does).

- [ ] **Step 2: Run to confirm they fail**

```bash
env -u GOROOT -u GOPATH go test ./internal/correlator/ -run TestMatch -v
```

Expected: compile failure — `undefined: correlator.Match`.

- [ ] **Step 3: Implement**

Create `internal/correlator/match.go`:

```go
package correlator

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/llm"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// MatchResult is what one narrative's resolution produced, for rendering and
// for tests.
type MatchResult struct {
	NarrativeID int64
	// Links is every recorded (issue, role) pair. Empty when no candidate
	// survived verification.
	Links []store.NarrativeIssue
	// Primary is the promoted key, or "" when there was none or when confidence
	// fell below MatchConfig.ConfidenceFloor.
	Primary string
	// Excluded and Unresolved carry dropped candidates and why, so a narrative
	// that looks untracked can be explained without re-running the pass.
	Excluded   []string
	Unresolved []string
	// Rationale is the model's reason when a classification call was made;
	// empty on the deterministic single- and zero-candidate paths.
	Rationale string
}

// MatchOption configures Match.
type MatchOption func(*matchOptions)

type matchOptions struct {
	linkExclusions []*regexp.Regexp
}

// WithLinkExclusions applies compiled exclude_from_linking patterns, so a
// placeholder key that satisfies a commit linter is not treated as a real link.
func WithLinkExclusions(compiled []*regexp.Regexp) MatchOption {
	return func(o *matchOptions) { o.linkExclusions = compiled }
}

// Match attributes unmatched narratives to the issue(s) that own their work.
//
// It runs as a separate pass after Persist rather than inside it: matching makes
// GetIssue network calls and an LLM call, and Persist's contract is that no
// SQLite transaction is held open across an LLM round-trip. Selecting on the
// absence of an issue link also makes this re-runnable over a backlog, so a
// tracker outage defers matching rather than losing it.
//
// Failure is per narrative, accumulated with errors.Join: unmatched is a valid
// resting state, so one narrative's tracker error costs a retry and nothing
// else. Match does not acquire the pipeline lease; the caller does, matching
// RunNarrate's convention.
func Match(
	ctx context.Context,
	s *store.Store,
	tracker tasktracker.TaskTracker,
	client llm.Client,
	cfg config.MatchConfig,
	opts ...MatchOption,
) ([]MatchResult, Stats, error) {
	var options matchOptions
	for _, opt := range opts {
		opt(&options)
	}

	limit := cfg.CandidateLimit()

	pending, err := s.NarrativesWithoutIssueKey(limit)
	if err != nil {
		return nil, Stats{}, err
	}

	var (
		results  []MatchResult
		stats    Stats
		failures []error
	)

	for _, narrative := range pending {
		result, callStats, err := matchOne(ctx, s, tracker, client, cfg, options, narrative)
		stats.Add(callStats)

		if err != nil {
			failures = append(failures, fmt.Errorf("narrative %d: %w", narrative.ID, err))

			continue
		}

		results = append(results, result)
	}

	return results, stats, errors.Join(failures...)
}

// matchOne resolves and persists one narrative's links.
func matchOne(
	ctx context.Context,
	s *store.Store,
	tracker tasktracker.TaskTracker,
	client llm.Client,
	cfg config.MatchConfig,
	options matchOptions,
	narrative store.NarrativeRow,
) (MatchResult, Stats, error) {
	var stats Stats

	// AllNarrativeEvents, not NarrativeEventsForContext: candidate keys live in
	// event artifacts, and the git_branch artifact carrying the strongest
	// provenance sits on a narrative's oldest events — exactly the ones a
	// compaction boundary hides.
	evts, err := s.AllNarrativeEvents(narrative.ID)
	if err != nil {
		return MatchResult{}, stats, err
	}

	result := MatchResult{
		NarrativeID: narrative.ID,
		Excluded:    excludedCandidates(evts, options.linkExclusions),
	}

	candidates := gatherCandidates(evts, options.linkExclusions, cfg.CandidateLimit())
	if len(candidates) == 0 {
		// Untracked work: the default path, not a special case. No tracker
		// call, no LLM call, nothing written.
		return result, stats, nil
	}

	verified, unresolved, err := verifyCandidates(tracker, candidates)
	if err != nil {
		return MatchResult{}, stats, err
	}

	result.Unresolved = unresolved

	if len(verified) == 0 {
		return result, stats, nil
	}

	var links []store.NarrativeIssue

	switch len(verified) {
	case 1:
		// Not a judgment call: one survivor is the primary. Spending an LLM
		// call here would be waste, and this is the common case.
		links = []store.NarrativeIssue{{
			IssueKey:   verified[0].candidate.IssueKey,
			Role:       RolePrimary,
			Provenance: string(verified[0].candidate.Provenance),
			Confidence: 1.0,
			Connection: verified[0].candidate.Connection,
		}}
	default:
		classified, callStats, cerr := classifyCandidates(ctx, client, narrative, verified)
		stats.Add(callStats)
		if cerr != nil {
			return MatchResult{}, stats, cerr
		}

		links = classified.links
		result.Rationale = classified.rationale
	}

	result.Links = links

	if err := persistLinks(s, narrative.ID, links, cfg, &result); err != nil {
		return MatchResult{}, stats, err
	}

	return result, stats, nil
}

// verifiedCandidate pairs a candidate with the live issue that resolved it.
type verifiedCandidate struct {
	candidate Candidate
	issue     tasktracker.Issue
}

// verifyCandidates resolves every candidate against the tracker.
//
// Mandatory regardless of provenance strength, per rules/verify-correlations.md:
// "a hallucinated match can be high-confidence." A branch name can also simply
// be stale. A key that does not resolve is reported, not silently discarded.
//
// A not-found is a normal outcome and drops the candidate. A transport error is
// returned, failing this narrative so the next pass retries it — the two are
// distinguished so a Jira outage does not look like a repo full of bad keys.
func verifyCandidates(
	tracker tasktracker.TaskTracker,
	candidates []Candidate,
) (verified []verifiedCandidate, unresolved []string, err error) {
	for _, c := range candidates {
		issue, gerr := tracker.GetIssue(c.IssueKey)
		if gerr != nil {
			if isTransportError(gerr) {
				return nil, nil, fmt.Errorf("verifying candidate %s: %w", c.IssueKey, gerr)
			}

			unresolved = append(unresolved, c.IssueKey)

			continue
		}

		verified = append(verified, verifiedCandidate{candidate: c, issue: issue})
	}

	return verified, unresolved, nil
}

// persistLinks writes the links and, when confidence allows, promotes the
// primary into narratives.issue_key — both in one transaction.
func persistLinks(
	s *store.Store,
	narrativeID int64,
	links []store.NarrativeIssue,
	cfg config.MatchConfig,
	result *MatchResult,
) error {
	var promote *store.NarrativeIssue

	for i := range links {
		if links[i].Role != RolePrimary {
			continue
		}

		if links[i].Confidence < cfg.ConfidenceFloor {
			// Recorded but not asserted. The narrative stays selectable by
			// NarrativesWithoutIssueKey so a later pass revisits it.
			log.Printf("match: narrative %d primary %s confidence %.2f is below the floor %.2f; "+
				"link recorded but narratives.issue_key left unset",
				narrativeID, links[i].IssueKey, links[i].Confidence, cfg.ConfidenceFloor)

			break
		}

		promote = &links[i]

		break
	}

	if err := s.WithTx(func(tx *store.Tx) error {
		if err := tx.AddNarrativeIssues(narrativeID, links); err != nil {
			return err
		}

		if promote != nil {
			return tx.SetNarrativeIssueLink(narrativeID, promote.IssueKey, promote.Confidence)
		}

		return nil
	}); err != nil {
		return err
	}

	if promote != nil {
		result.Primary = promote.IssueKey
	}

	return nil
}
```

**You must supply two things this draft references but does not define**, and both need a decision:

1. **`isTransportError(err)`** — distinguishes "this key does not exist" (drop the candidate) from
   "the tracker is unreachable" (fail this narrative and retry next pass). Getting it wrong in one
   direction turns a Jira outage into a repo full of permanently-unresolved keys; in the other, a
   genuinely deleted ticket fails the narrative forever. Look at what `internal/clients/jira`
   actually returns — there is a `jira.Error` type with a `Status` field — and treat a 404 as
   not-found while a 5xx, a timeout, or a connection error is transport. `internal/clients/local`
   returns `store.ErrLocalIssueNotFound`, so `errors.Is` against that is the local not-found signal.
   The `fakeTracker` above returns a bare `fmt.Errorf("issue %s not found")` for missing keys and a
   distinct `errors.New("jira 500")` for failures, so whatever you write must classify those two
   correctly — adjust the fake to carry a typed error if that reads better, and say so.

2. **`classifyCandidates(ctx, client, narrative, verified)`** — the LLM call. Build a system prompt
   and a user prompt in the style of `clusterSystemPrompt`/`buildClusterPrompt` (read them first),
   returning `struct{ links []store.NarrativeIssue; rationale string }` plus `Stats`. The user prompt
   must include: the narrative's title and summary, each candidate's key, **summary, description**,
   and status, and each candidate's provenance as a stated prior. The system prompt must define the
   three roles in the same words as `match_types.go`'s doc comments, require exactly one `primary`,
   and demand a bare JSON array with no markdown fence (then still run the response through
   `parseMatchResponse`, which strips fences anyway — models add them regardless).

   Convert each `matchVerdict` into a `store.NarrativeIssue`, carrying `Provenance` and `Connection`
   from the matching `verifiedCandidate` rather than from the model: provenance is a fact we
   determined and must not be something the model can restate incorrectly.

   Set `Stats.Calls` and add usage via the existing unexported `addUsage` helper — read how `Cluster`
   does it.

Add to `internal/correlator/export_test.go`:

```go
// WithLinkExclusionsForTest mirrors WithLinkExclusions. It exists so the test
// package can pass compiled patterns without importing regexp semantics that
// differ from production's.
func WithLinkExclusionsForTest(compiled []*regexp.Regexp) MatchOption {
	return WithLinkExclusions(compiled)
}
```

Since `WithLinkExclusions` is already exported, prefer calling it directly from the test and delete
this shim — it earns its place only if the test package cannot see it. Decide and say which.

- [ ] **Step 4: Run to confirm they pass**

```bash
env -u GOROOT -u GOPATH go test -count=1 ./internal/correlator/ -v 2>&1 | tail -40
env -u GOROOT -u GOPATH go test -race -count=1 ./internal/correlator/
```

- [ ] **Step 5: Break-it drill — verification must actually gate (REQUIRED)**

This is the `rules/verify-correlations.md` failure mode, and the most important drill in the plan.

Change `matchOne` to skip verification when a candidate's provenance is `ProvenanceBranch` (trusting
the branch without calling `GetIssue`). Confirm
`TestMatch_VerifiesEveryCandidateAndDropsUnresolvable` FAILS — `GONE-1` would be promoted as primary
despite not existing. Restore and confirm it passes.

Paste both outputs.

- [ ] **Step 6: Break-it drill — the floor must not suppress recording**

Change `persistLinks` to skip `AddNarrativeIssues` entirely when the primary is below the floor.
Confirm `TestMatch_ConfidenceFloorBlocksPromotionButNotRecording` FAILS on the `Len(links, 2)`
assertion. Restore.

Paste both outputs.

- [ ] **Step 7: Commit**

```bash
export PATH="$HOME/.asdf/installs/golang/1.25.7/bin:$PATH"
env -u GOROOT -u GOPATH golangci-lint fmt ./internal/correlator/...
env -u GOROOT -u GOPATH golangci-lint run ./internal/correlator/...
git add internal/correlator/match.go internal/correlator/match_test.go \
        internal/correlator/export_test.go
git commit -m "Add correlator.Match: verify every candidate, classify only when ambiguous"
```

---

## Task 7: `pipeline.RunMatch` and renderer surfacing

**Files:**
- Create: `internal/pipeline/match.go`
- Create: `internal/pipeline/match_test.go`
- Modify: `internal/pipeline/narrate_render.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/pipeline/match_test.go` (`package pipeline_test`). Reuse the `fakeTracker` shape from
Task 6 — it is file-scoped to `internal/correlator/match_test.go` and therefore **not** visible here,
so declare a local equivalent rather than trying to import it.

```go
func TestRunMatch_ReturnsRenderableResult(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()

	start := time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)
	id, err := s.InsertNarrative(start, start.Add(time.Hour), "widget work", "summary")
	require.NoError(t, err)

	e := events.NewEvent("claude_code", "s1", start.Add(30*time.Minute), "session")
	e.Artifacts["git_branch"] = "feature/PROJ-42"
	inserted, err := s.InsertEvent(e)
	require.NoError(t, err)
	require.True(t, inserted)
	eid, err := s.EventIDByExternalID(e.Source, e.ExternalID)
	require.NoError(t, err)
	require.NoError(t, s.AddNarrativeEvents(id, []int64{eid}))

	tracker := &pipelineFakeTracker{issues: map[string]tasktracker.Issue{
		"PROJ-42": {Key: "PROJ-42", Summary: "Fix the widget"},
	}}
	cfg := config.Config{Match: config.MatchConfig{MaxCandidatesPerNarrative: 10, ConfidenceFloor: 0.5}}

	got, err := pipeline.RunMatch(context.Background(), s, tracker, &pipelineFakeLLM{}, cfg)

	require.NoError(t, err)
	require.Len(t, got.Matched, 1)
	assert.Equal(t, "PROJ-42", got.Matched[0].Primary)
	assert.Equal(t, id, got.Matched[0].NarrativeID)
}

func TestRunMatch_AppliesConfiguredLinkExclusions(t *testing.T) {
	// RunMatch is where compiled exclusions are threaded in — the correlator
	// takes them as an option and cannot read config itself.
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()

	start := time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)
	id, err := s.InsertNarrative(start, start.Add(time.Hour), "placeholder work", "summary")
	require.NoError(t, err)

	e := events.NewEvent("claude_code", "s1", start.Add(30*time.Minute), "session")
	e.Artifacts["ticket_keys"] = []any{"NOJIRA-1"}
	inserted, err := s.InsertEvent(e)
	require.NoError(t, err)
	require.True(t, inserted)
	eid, err := s.EventIDByExternalID(e.Source, e.ExternalID)
	require.NoError(t, err)
	require.NoError(t, s.AddNarrativeEvents(id, []int64{eid}))

	tracker := &pipelineFakeTracker{}
	cfg := config.Config{
		ExcludeFromLinking: []string{`^NOJIRA-\d+$`},
		Match:              config.MatchConfig{MaxCandidatesPerNarrative: 10, ConfidenceFloor: 0.5},
	}

	got, err := pipeline.RunMatch(context.Background(), s, tracker, &pipelineFakeLLM{}, cfg)

	require.NoError(t, err)
	require.Len(t, got.Matched, 1)
	assert.Equal(t, []string{"NOJIRA-1"}, got.Matched[0].Excluded)
	assert.Empty(t, tracker.getCalls, "an excluded placeholder must never be verified")
}

func TestRenderMatchResult_ShowsLinksAndReasons(t *testing.T) {
	got := pipeline.RenderMatchResult(pipeline.MatchRunResult{
		Matched: []correlator.MatchResult{
			{
				NarrativeID: 7,
				Primary:     "PAAS-1",
				Links: []store.NarrativeIssue{
					{IssueKey: "PAAS-1", Role: "primary", Provenance: "branch", Confidence: 0.93},
					{IssueKey: "SUMO-2", Role: "same_work", Provenance: "prose_first", Confidence: 0.85},
				},
				Rationale: "PAAS is the engineering record; SUMO gates the deploy",
			},
			{NarrativeID: 8, Excluded: []string{"NOJIRA-9"}},
		},
	})

	assert.Contains(t, got, "PAAS-1")
	assert.Contains(t, got, "primary")
	assert.Contains(t, got, "SUMO-2")
	assert.Contains(t, got, "same_work")
	assert.Contains(t, got, "branch", "provenance must be visible to judge a wrong match")
	assert.Contains(t, got, "0.93")
	assert.Contains(t, got, "NOJIRA-9",
		"an unmatched narrative must say WHY, or it reads as a silent failure")
	assert.Contains(t, got, "unmatched")
}
```

Verify `config.Config`'s exclusion field name (`ExcludeFromLinking`) and the compiled-accessor name
before using them — the plan states `CompiledLinkExclusions()` but grep to confirm; `cmd/unjira`
already calls it for `RunCollect`.

- [ ] **Step 2: Run to confirm they fail**

```bash
env -u GOROOT -u GOPATH go test ./internal/pipeline/ -run 'RunMatch|RenderMatch' -v
```

Expected: compile failure — `undefined: pipeline.RunMatch`.

- [ ] **Step 3: Implement `RunMatch`**

Create `internal/pipeline/match.go`:

```go
package pipeline

import (
	"context"
	"fmt"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/llm"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// MatchRunResult is one matching pass, shaped for rendering.
type MatchRunResult struct {
	Matched []correlator.MatchResult
	Stats   correlator.Stats
}

// RunMatch attributes unmatched narratives to their issues.
//
// This is the stage that turns narratives.issue_key from a column nothing writes
// into the reconciler's входной signal. It compiles exclude_from_linking here
// rather than in the correlator because the correlator takes patterns as an
// option and deliberately does not read config — the same split RunCollect uses.
//
// Like RunNarrate, it does NOT acquire the pipeline lease: the scope differs per
// caller, and dev narrate wraps one stage while watch will wrap collect +
// narrate + match + reconcile in a single lease.
func RunMatch(
	ctx context.Context,
	s *store.Store,
	tracker tasktracker.TaskTracker,
	client llm.Client,
	cfg config.Config,
) (MatchRunResult, error) {
	if err := cfg.Match.Validate(); err != nil {
		return MatchRunResult{}, err
	}

	linkExclusions, err := cfg.CompiledLinkExclusions()
	if err != nil {
		return MatchRunResult{}, fmt.Errorf("compiling exclude_from_linking for matching: %w", err)
	}

	matched, stats, err := correlator.Match(ctx, s, tracker, client, cfg.Match,
		correlator.WithLinkExclusions(linkExclusions))

	// Return the partial result alongside the error: Match isolates failures per
	// narrative, so a non-nil error here still means the healthy narratives
	// matched and their results are worth rendering.
	return MatchRunResult{Matched: matched, Stats: stats}, err
}
```

**Remove the stray non-ASCII word in that doc comment** ("входной") — it is a transcription artifact.
Say "input" instead. Flagging it deliberately: if you copy the block verbatim without noticing, that
is a signal you are not reading what you paste.

- [ ] **Step 4: Implement the renderer**

Add to `internal/pipeline/narrate_render.go` (read `RenderNarrateResult` first and match its style —
it builds into a `*strings.Builder` with `fmt.Fprintf`):

```go
// RenderMatchResult renders a matching pass for a human judging whether the
// attributions are right.
//
// Provenance and confidence are shown per link, and an unmatched narrative
// states why: without that, "no links" is indistinguishable from a silent
// failure, and the excluded-placeholder case would look like the matcher simply
// found nothing.
func RenderMatchResult(r MatchRunResult) string {
	var b strings.Builder

	fmt.Fprintf(&b, "matched  %d narrative(s)\n", len(r.Matched))
	fmt.Fprintf(&b, "llm      %d call(s)\n", r.Stats.Calls)
	fmt.Fprintf(&b, "tokens   %d prompt + %d completion\n",
		r.Stats.PromptTokens, r.Stats.CompletionTokens)

	for _, m := range r.Matched {
		b.WriteString("\n")

		if len(m.Links) == 0 {
			fmt.Fprintf(&b, "[unmatched #%d]", m.NarrativeID)
			switch {
			case len(m.Excluded) > 0:
				fmt.Fprintf(&b, "  excluded: %s\n", strings.Join(m.Excluded, ", "))
			case len(m.Unresolved) > 0:
				fmt.Fprintf(&b, "  unresolved: %s\n", strings.Join(m.Unresolved, ", "))
			default:
				b.WriteString("  no candidate keys in its events\n")
			}

			continue
		}

		fmt.Fprintf(&b, "[matched #%d] primary %s\n", m.NarrativeID, primaryLabel(m.Primary))
		for _, l := range m.Links {
			fmt.Fprintf(&b, "    %-10s %-12s via %-12s %.2f\n",
				l.Role, l.IssueKey, l.Provenance, l.Confidence)
		}
		if m.Rationale != "" {
			fmt.Fprintf(&b, "    why: %s\n", m.Rationale)
		}
		if len(m.Unresolved) > 0 {
			fmt.Fprintf(&b, "    unresolved: %s\n", strings.Join(m.Unresolved, ", "))
		}
		if len(m.Excluded) > 0 {
			fmt.Fprintf(&b, "    excluded: %s\n", strings.Join(m.Excluded, ", "))
		}
	}

	return b.String()
}

// primaryLabel renders an unpromoted primary distinctly, so a below-floor match
// does not read as if no primary was found at all.
func primaryLabel(primary string) string {
	if primary == "" {
		return "(none promoted — below the confidence floor)"
	}

	return primary
}
```

- [ ] **Step 5: Run to confirm they pass**

```bash
env -u GOROOT -u GOPATH go test -count=1 ./internal/pipeline/ -v 2>&1 | tail -30
env -u GOROOT -u GOPATH go build ./...
```

- [ ] **Step 6: Prove the unmatched-reason test discriminates**

Delete the `switch` inside the `len(m.Links) == 0` branch, leaving only the `[unmatched #N]` header.
Confirm `TestRenderMatchResult_ShowsLinksAndReasons` FAILS on the `NOJIRA-9` assertion. Restore.
Paste both outputs.

- [ ] **Step 7: Commit**

```bash
export PATH="$HOME/.asdf/installs/golang/1.25.7/bin:$PATH"
env -u GOROOT -u GOPATH golangci-lint fmt ./internal/pipeline/...
env -u GOROOT -u GOPATH golangci-lint run ./internal/pipeline/...
git add internal/pipeline/match.go internal/pipeline/match_test.go internal/pipeline/narrate_render.go
git commit -m "Add pipeline.RunMatch and a renderer that says why a narrative is unmatched"
```

---

## Task 8: wire `dev narrate` to run matching

**Files:**
- Modify: `cmd/unjira/main.go`

- [ ] **Step 1: Read the existing command**

```bash
grep -n "devNarrateCmd" -A 60 cmd/unjira/main.go | head -80
grep -n "trackerFor\|tasktracker\|TrackerBackend" cmd/unjira/main.go | head
```

You need: how `devNarrateCmd.Run` obtains the store and LLM client, and how a `tasktracker.TaskTracker`
is constructed for the configured backend (there is existing machinery for `jira` vs `local` — find
it; do not write a second resolver).

- [ ] **Step 2: Add matching after narration**

In `devNarrateCmd.Run`, after the existing `RunNarrate` + render, add:

```go
	// Matching runs as its own pass after narration so a tracker outage defers
	// attribution rather than failing the narration that already succeeded.
	tracker, err := app.tracker()
	if err != nil {
		return err
	}

	matchResult, matchErr := pipeline.RunMatch(ctx, app.store, tracker, client, app.config)

	// Render before returning the error: Match isolates failures per narrative,
	// so the healthy narratives matched and the operator should see them
	// alongside whatever failed.
	fmt.Print(pipeline.RenderMatchResult(matchResult))

	if matchErr != nil {
		return fmt.Errorf("matching narratives to issues: %w", matchErr)
	}

	return nil
```

Replace `app.tracker()` with whatever the real accessor is called. If none exists, write one
following `appContext`'s existing pattern (see `jiraClientForProject`) and say so in your report —
`cfg.TrackerBackend()` returns `"jira"` or `"local"`, and `internal/clients/local.New(store)` /
`internal/clients/jira.NewTracker(client)` are the two constructors.

Consider whether `--dry-run` should skip matching. It should: `dev narrate --dry-run` currently makes
no writes, and matching writes rows. Gate it, and say what you did.

- [ ] **Step 3: Verify by observation**

The registry and command wiring live in `package main`, which is unimportable, so prove it runs:

```bash
env -u GOROOT -u GOPATH go build ./...
cd /tmp && rm -rf matchwire && mkdir matchwire && cd matchwire
cat > unjira.config.json <<'EOF'
{
  "collectors": { "claude_code": { "enabled": true } },
  "tracker": { "backend": "local" },
  "correlator": { "tail_summarize_threshold_tokens": 4000, "recent_events_kept": 20 },
  "match": { "max_candidates_per_narrative": 10, "confidence_floor": 0.7 }
}
EOF
cd /Users/jonathan.ogilvie/workspace/unjira/.claude/worktrees/slice4-reconciler
env -u GOROOT -u GOPATH go run ./cmd/unjira dev narrate --help 2>&1 | head -20
```

Then run it for real against the local backend with an empty event log. With no events there is
nothing to narrate, so expect a clean "no unlinked events" style exit rather than a matching error —
confirm it does not panic and does not require an LLM key to reach that point:

```bash
env -u GOROOT -u GOPATH go run ./cmd/unjira dev narrate --since 1h \
  --config /tmp/matchwire/unjira.config.json 2>&1 | head -20
rm -rf /tmp/matchwire
```

Report exactly what you saw. If it demands an LLM API key before deciding there is nothing to do,
that is pre-existing behaviour, not something to fix here — note it and move on.

- [ ] **Step 4: Commit**

```bash
export PATH="$HOME/.asdf/installs/golang/1.25.7/bin:$PATH"
env -u GOROOT -u GOPATH golangci-lint fmt ./cmd/...
env -u GOROOT -u GOPATH golangci-lint run ./cmd/...
git add cmd/unjira/main.go
git commit -m "Run matching after narration in dev narrate"
```

---

## Task 9: full regression, lint, and the container gate

**Files:** none — verification only.

- [ ] **Step 1: Full offline suite**

```bash
env -u GOROOT -u GOPATH go build ./...
env -u GOROOT -u GOPATH go test -count=1 ./... 2>&1 | grep -Ev "^ok|no test files"; echo "(blank = all pass)"
env -u GOROOT -u GOPATH go test -race -count=1 ./internal/correlator/ ./internal/store/ ./internal/pipeline/
```

- [ ] **Step 2: Lint**

```bash
export PATH="$HOME/.asdf/installs/golang/1.25.7/bin:$PATH"
env -u GOROOT -u GOPATH golangci-lint run ./...
```

Expected: `0 issues`. Do not add `nolint` directives without saying why in your report.

- [ ] **Step 3: Example config parses**

```bash
python3 -c "import json; json.load(open('config/unjira.example.json'))" && echo VALID
```

- [ ] **Step 4: The authoritative gate**

```bash
earthly +reviewable
```

Expected: SUCCESS. If this fails while the local run passed, something is environment-dependent —
report it rather than working around it.

- [ ] **Step 5: Confirm `.env` was never staged**

```bash
git status --short
git log --stat -8 | grep -c "^ .env" | sed 's/^0$/0 — .env never committed, good/'
```

- [ ] **Step 6: Report**

State: which tests you added, every break-it drill result, anything in this plan you changed and why,
and any place you had to make a decision the plan left to you (`isTransportError`'s classification,
the `Role` alias, `--dry-run` gating).

---

## Task 10: live-tier test

**Files:**
- Modify: `internal/live/jira_test.go`

Offline fixtures encode what we *believe* `GetIssue` returns. Only a live call establishes whether
`Description` actually arrives — and that field is the whole reason Task 1 exists.

- [ ] **Step 1: Read the existing harness**

`internal/live/jira_test.go` is behind `//go:build live`, loads `.env` in `TestMain`, skips unless
`UNJIRA_LIVE=1`, and resolves credentials from `UNJIRA_JIRA_CREDENTIALS` keyed by
`liveConnectionName()` (default `dev`). `testClient(t)` returns a `*jira.Client`; `testProject()`
returns the project key. Follow those; do not add a second credential path.

- [ ] **Step 2: Add the test**

```go
// TestLiveMatchingSignalIsAvailable verifies the two things matching depends on
// that no fixture can establish: that GetIssue resolves a real key through the
// tasktracker seam, and that Description actually arrives populated.
//
// Description is the reason tasktracker.Issue gained the field — the Jira
// collector deliberately never emits description snapshots as events, so a
// never-edited ticket's body is available only on this live read path.
func TestLiveMatchingSignalIsAvailable(t *testing.T) {
	client := testClient(t)
	tracker := jira.NewTracker(client)

	const body = "Live matching probe: this body is the signal matching compares against."

	key, err := client.CreateIssue(
		testProject(),
		"[seed] live matching signal probe",
		"Task",
		body,
		[]string{jira.SeedLabel},
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.DeleteIssue(key) })

	issue, err := tracker.GetIssue(key)

	require.NoError(t, err, "a key we just created must resolve — this is what verification relies on")
	assert.Equal(t, key, issue.Key)
	assert.Equal(t, "[seed] live matching signal probe", issue.Summary)
	assert.Equal(t, body, issue.Description,
		"Description must survive normalizeIssue; if this fails, Jira returned ADF rather than a "+
			"string and matching loses its strongest signal")
	assert.NotEmpty(t, issue.StatusCategory, "status is part of the classification prompt")
}

// TestLiveMatchingUnresolvableKeyIsNotFound confirms the not-found signal
// verification depends on, distinguished from a transport failure.
//
// A stale branch name or a hallucinated key must drop the candidate, not fail
// the narrative — so this must return an error that isTransportError classifies
// as not-found.
func TestLiveMatchingUnresolvableKeyIsNotFound(t *testing.T) {
	client := testClient(t)
	tracker := jira.NewTracker(client)

	_, err := tracker.GetIssue(testProject() + "-99999999")

	require.Error(t, err, "a nonexistent key must error rather than returning a zero Issue")
	assert.False(t, isTransportErrorForLiveTest(err),
		"a 404 must classify as not-found, or a stale branch name would fail the whole narrative")
}
```

The second test references `isTransportErrorForLiveTest`. `isTransportError` lives in
`internal/correlator` and is unexported, so either export a test shim from `correlator/export_test.go`
(which is NOT visible to `internal/live` — `export_test.go` files only compile for their own
package's tests) or move the classifier somewhere shared. **Decide and report:** the cleanest option
is probably to make the classifier a small exported function on `internal/tasktracker` (e.g.
`tasktracker.IsNotFound(err) bool`), since both the correlator and the live tier need it and it is
genuinely a property of the tracker contract rather than of matching. If you do that, update Task 6's
`verifyCandidates` to call it and say so.

- [ ] **Step 3: Verify it compiles and skips**

```bash
env -u GOROOT -u GOPATH go vet -tags=live ./internal/live/...
env -u GOROOT -u GOPATH go test -tags=live -count=1 ./internal/live/... -run TestLiveMatching -v 2>&1 | tail -10
```

Expected: compiles; both tests SKIP without `UNJIRA_LIVE=1`; exit 0.

**Do NOT run these for real.** They create and delete issues on a live instance. The coordinator runs
them.

- [ ] **Step 4: Confirm the offline suite is unaffected**

```bash
env -u GOROOT -u GOPATH go test -count=1 ./... 2>&1 | grep -Ev "^ok|no test files"; echo "(blank = unchanged)"
```

The `live` tag excludes `internal/live` from normal builds, so this must be identical to Task 9.

- [ ] **Step 5: Commit**

```bash
export PATH="$HOME/.asdf/installs/golang/1.25.7/bin:$PATH"
env -u GOROOT -u GOPATH golangci-lint fmt ./internal/live/...
git add internal/live/jira_test.go
git commit -m "Add live-tier test: Description arrives populated and 404 is not-found"
```

---

## Out of scope (do not build)

- **`internal/reconciler`** and any action policy for `same_work`. This slice records links; the
  reconciler decides behaviour. Recording a "which co-representation should receive updates" flag now
  would be a policy call ahead of its consumer.
- **The `rules/` loader.** `rules/README.md` claims the correlator loads rules into its prompt each
  pass; no code does. It is its own slice so it serves all three declared scopes
  (`correlator`/`reconciler`/`estimator`). Matching classifies `same_work` without it.
- **Semantic / vector search.** Settled: the keys are usually already in the text. The trigger for
  revisiting is evidence that zero-candidate narratives are common — which this slice records.
- **Reading Jira issue links.** The system of record for ticket-to-ticket relationships, but new
  collector scope with unproven classification benefit.
- **Any write path to Jira.** This slice reads. `CreateIssue`/`DeleteIssue` appear only in the
  live-tier test, to seed and clean up.
- **Rendering ADF descriptions to text.** A non-string `description` yields an empty `Description`,
  which degrades matching rather than breaking it.
