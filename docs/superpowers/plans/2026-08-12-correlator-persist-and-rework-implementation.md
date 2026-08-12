# Correlator Path-B rework + `Persist` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Restore the phase-1 "Path B" intent — the correlator clusters against the *raw events* of
overlapping/adjacent narratives, not just their summaries — and land narrative persistence with
tail-summarization and a leased pipeline lock. Combines the slice-2 rework
(`docs/superpowers/specs/2026-08-12-correlator-hydrated-context-rework.md`) and slice 3
(`docs/superpowers/specs/2026-08-12-correlator-persist-design.md`), which are entangled and ship
together.

**Architecture:** Two layers. (1) `internal/correlator` — rework the merged `Cluster` so
`Narrative` carries caller-hydrated `Events`, the prompt has two sections (in-window events =
numbered/assignable; narrative events = context-only, no integer handles), and add `Persist` (writes
narratives/narrative_events, runs tail-summarization compaction via the LLM). (2) `internal/store` —
new narrative accessors, the `compaction_boundary` column with an idempotent migration, a
transaction seam so `Persist` is all-or-nothing, the `NarrativeEventsForContext` hydration accessor,
and the `pipeline_lock` lease. Plus `internal/config` gains `CorrelatorConfig`.

**Tech Stack:** Go 1.26, `github.com/stretchr/testify`, `modernc.org/sqlite` (already the driver),
stdlib `database/sql`/`context`/`time`. No new deps.

---

## Critical environment note (every task)

This shell has stale `GOROOT`/`GOPATH` env vars. **Every `go`/`gofmt` invocation MUST be prefixed
with `env -u GOROOT -u GOPATH`**, e.g. `env -u GOROOT -u GOPATH go test ./...`. Without it you get
spurious `compile: version "X" does not match go tool version "Y"` errors unrelated to your change.
Work in the worktree at `/Users/jonathan.ogilvie/workspace/unjira/.claude/worktrees/phase1-persist-and-rework`;
run all commands from there. Do not touch the session task list (the coordinator owns it).

## File structure

- **Modify:** `internal/correlator/correlator.go` — `Narrative.Events` field; two-section
  `buildClusterPrompt`; rewritten `clusterSystemPrompt`; add `Persist` + compaction helpers.
- **Modify:** `internal/correlator/correlator_test.go` — rework prompt-content tests; add `Persist`
  tests (real store + fake LLM).
- **Modify:** `internal/store/store.go` — `compaction_boundary` column + migration in `Open`;
  `NarrativeRow`; `ErrNarrativeNotFound`/`ErrEventNotFound`; narrative accessors; transaction seam;
  `pipeline_lock` schema + lease methods.
- **Modify:** `internal/store/store_test.go` — accessor tests, lease-lock tests, migration test.
- **Modify:** `internal/config/config.go` — `CorrelatorConfig` + `Validate` + `Config.Correlator`.
- **Modify:** `internal/config/config_test.go` — `CorrelatorConfig` tests, example-parse.
- **Modify:** `config/unjira.example.json` — `correlator` block.
- **Modify:** `docs/superpowers/specs/2026-08-11-phase1-correlator-design.md` — slice-3 status +
  tail-summarization/Path-B reconciliation note (final task).

Sequencing rationale: the store layer (Tasks 3-7) is a dependency of `Persist` (Task 8), so it lands
first. The `Cluster` prompt rework (Tasks 1-2) is independent of the store and can land first of all,
keeping each task's diff focused.

---

## Task 1: `Cluster` prompt rework — `Narrative.Events` + two-section prompt

**Files:**
- Modify: `internal/correlator/correlator.go`
- Modify: `internal/correlator/correlator_test.go`

- [ ] **Step 1: Update the failing test first (prompt-content assertions)**

The existing `TestCluster_ExcludesNarrativesOutsideWindowAndNotAdjacent` asserts only that narrative
*titles* appear in the prompt. Replace its body's assertions to also require the narrative's *events*
appear, and to assert the two-section structure. Find that test and replace it with:

```go
func TestCluster_HydratedNarrativeEventsAppearAsContextNotAssignable(t *testing.T) {
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	window := correlator.TimeRange{Start: base, End: base.Add(time.Hour)}

	existing := []correlator.Narrative{
		{
			ID: 9, Title: "Cache rework", Summary: "Reworking the shared cache",
			WindowStart: base.Add(-2 * time.Hour), WindowEnd: base,
			Events: []correlator.Event{
				mustEvent(t, "github", "pr-412", "PR #412 add cache layer", base.Add(-2*time.Hour)),
			},
		},
		{
			ID: 3, Title: "far-away", Summary: "unrelated",
			WindowStart: base.Add(-48 * time.Hour), WindowEnd: base.Add(-24 * time.Hour),
			Events: []correlator.Event{
				mustEvent(t, "github", "pr-1", "PR #1 ancient", base.Add(-48*time.Hour)),
			},
		},
	}
	inWindow := []correlator.Event{
		mustEvent(t, "claude_code", "e1", "debugging cache eviction", base.Add(5*time.Minute)),
	}
	llm := &fakeLLM{responses: []string{"[]"}}

	_, err := correlator.Cluster(t.Context(), inWindow, existing, llm, window, 128000)

	require.NoError(t, err)
	require.Len(t, llm.prompts, 1)
	prompt := llm.prompts[0]
	// Adjacent narrative 9 and its event are present as context.
	assert.Contains(t, prompt, "Cache rework")
	assert.Contains(t, prompt, "PR #412 add cache layer")
	// Non-adjacent narrative 3 is excluded entirely.
	assert.NotContains(t, prompt, "far-away")
	assert.NotContains(t, prompt, "PR #1 ancient")
	// The in-window event is present and numbered/assignable.
	assert.Contains(t, prompt, "debugging cache eviction")
	// Structural: two labeled sections exist, and the context section warns
	// against reassigning its events.
	assert.Contains(t, prompt, "Events to cluster")
	assert.Contains(t, prompt, "CONTEXT ONLY")
}
```

- [ ] **Step 2: Run it to confirm it fails**

Run: `env -u GOROOT -u GOPATH go test ./internal/correlator/ -run TestCluster_HydratedNarrativeEventsAppearAsContextNotAssignable -v`

Expected: FAIL — `Narrative` has no `Events` field (compile error), and once that's added, the prompt
lacks the "CONTEXT ONLY" / "Events to cluster" strings and the narrative event text.

- [ ] **Step 3: Add `Events` to `Narrative`**

In `internal/correlator/correlator.go`, find the `Narrative` struct and add the field (after
`Status`):

```go
	Status      string
	// Events is the narrative's context events, hydrated by the caller
	// (see store.NarrativeEventsForContext) before Cluster is called —
	// everything newer than the narrative's compaction boundary; the recap
	// of older events lives in Summary. Cluster reads these for context
	// only and never fetches them itself, keeping Cluster pure compute.
	Events      []Event
```

- [ ] **Step 4: Rewrite `buildClusterPrompt` for two sections**

Replace the existing `buildClusterPrompt` and `clusterSystemPrompt` with:

```go
// buildClusterPrompt renders the fixed system prompt and a two-section user
// prompt: the in-window events to cluster (numbered 0..N-1, assignable via
// event_indices), then the overlapping/adjacent narratives as CONTEXT ONLY
// (their events carry no index, so the model structurally cannot reassign
// them). See docs/superpowers/specs/2026-08-12-correlator-hydrated-context-rework.md.
func buildClusterPrompt(evts []Event, existing []Narrative) (systemPrompt, userPrompt string) {
	systemPrompt = clusterSystemPrompt

	var b strings.Builder
	b.WriteString("Events to cluster (assign each to exactly one cluster by its index):\n")
	for i, e := range evts {
		fmt.Fprintf(&b, "%d. [%s] %q (occurred_at=%s)\n", i, e.Source, e.Summary, e.OccurredAt.Format(time.RFC3339))
	}

	b.WriteString("\nExisting narratives (CONTEXT ONLY — do not reassign these events; use them to decide whether an event above extends one of these narratives or starts a new one):\n")
	if len(existing) == 0 {
		b.WriteString("(none)\n")
	}
	for _, n := range existing {
		fmt.Fprintf(&b, "narrative_id=%d title=%q window=[%s, %s)\n",
			n.ID, n.Title, n.WindowStart.Format(time.RFC3339), n.WindowEnd.Format(time.RFC3339))
		fmt.Fprintf(&b, "  summary: %q\n", n.Summary)
		if len(n.Events) > 0 {
			b.WriteString("  events:\n")
			for _, e := range n.Events {
				fmt.Fprintf(&b, "    - [%s] %q (occurred_at=%s)\n", e.Source, e.Summary, e.OccurredAt.Format(time.RFC3339))
			}
		}
	}

	return systemPrompt, b.String()
}

const clusterSystemPrompt = `Cluster the given events into narratives. You are shown two sections: "Events to cluster" (each has an integer index) and "Existing narratives" (context only).

Assign every indexed event to exactly one cluster. Tag each cluster "new" or "extends"; if "extends", include the narrative_id of the existing narrative it continues. Use the existing narratives — including their listed events — only to judge whether an indexed event continues one of them; never put an existing narrative's events into event_indices (they have no index and are already assigned).

Return ONLY a JSON array matching this shape, no prose, no markdown fences:
[{"kind":"new"|"extends","narrative_id":<int, only if extends>,"title":"...","summary":"...","event_indices":[0,2,5]}]`
```

Note: `event_indices` still references only the numbered in-window events; `parseClusterResponse` is
unchanged (it already maps indices into the `filtered` in-window slice it was given). Confirm
`parseClusterResponse` is still called with `filtered` (the in-window events), NOT any narrative
events — this is the load-bearing invariant that keeps context events unassignable. It already is;
do not change it.

- [ ] **Step 5: Run the reworked test + full package**

Run: `env -u GOROOT -u GOPATH go test ./internal/correlator/ -v`

Expected: PASS, including the new `TestCluster_HydratedNarrativeEventsAppearAsContextNotAssignable`
and every pre-existing test (the happy-path/extends/split/merge tests set no `Narrative.Events`, so
their prompts just have empty context event lists — still valid).

- [ ] **Step 6: gofmt + vet + commit**

```bash
env -u GOROOT -u GOPATH gofmt -w internal/correlator/correlator.go internal/correlator/correlator_test.go
env -u GOROOT -u GOPATH go vet ./internal/correlator/...
git add internal/correlator/correlator.go internal/correlator/correlator_test.go
git commit -m "Rework Cluster prompt: hydrated narrative events as context-only section"
```

---

## Task 2: prove hydrated events inform an `extends` decision

**Files:**
- Modify: `internal/correlator/correlator_test.go`

Test-only. Proves the context events are actually usable signal (not just present in the string).

- [ ] **Step 1: Add the test**

```go
func TestCluster_UsesNarrativeContextEventsForExtendsDecision(t *testing.T) {
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	window := correlator.TimeRange{Start: base, End: base.Add(time.Hour)}
	existing := []correlator.Narrative{{
		ID: 9, Title: "Cache rework", Summary: "Reworking the shared cache",
		WindowStart: base.Add(-2 * time.Hour), WindowEnd: base,
		Events: []correlator.Event{
			mustEvent(t, "github", "pr-412", "PR #412 add cache layer", base.Add(-2*time.Hour)),
		},
	}}
	inWindow := []correlator.Event{
		mustEvent(t, "claude_code", "e1", "fixed the cache eviction bug from PR #412", base.Add(5*time.Minute)),
	}
	// Model, having seen narrative 9's events, extends it.
	llm := &fakeLLM{responses: []string{
		`[{"kind":"extends","narrative_id":9,"title":"Cache rework","summary":"Reworking the shared cache; fixed eviction","event_indices":[0]}]`,
	}}

	results, err := correlator.Cluster(t.Context(), inWindow, existing, llm, window, 128000)

	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, correlator.ClusterExtends, results[0].Kind)
	assert.Equal(t, int64(9), results[0].NarrativeID)
	require.Len(t, results[0].Events, 1)
	assert.Equal(t, "e1", results[0].Events[0].ExternalID)
}
```

- [ ] **Step 2: Run + commit**

Run: `env -u GOROOT -u GOPATH go test ./internal/correlator/ -run TestCluster_UsesNarrativeContextEventsForExtendsDecision -v` → PASS (the fake returns the extends result; the test asserts Cluster threads it through correctly).

```bash
git add internal/correlator/correlator_test.go
git commit -m "Test: hydrated narrative context events inform extends decisions"
```

---

## Task 3: `CorrelatorConfig` in `internal/config`

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `config/unjira.example.json`

- [ ] **Step 1: Write failing tests**

Add to `internal/config/config_test.go` (after the `TestLLMConfig_Validate` table, before
`TestLoad_ParsesLLMBlock` or wherever the LLM tests sit):

```go
func TestCorrelatorConfig_Validate(t *testing.T) {
	tests := []struct {
		name        string
		cfg         config.CorrelatorConfig
		wantErrText string
	}{
		{
			name:        "requires positive threshold",
			cfg:         config.CorrelatorConfig{TailSummarizeThresholdTokens: 0, RecentEventsKept: 20},
			wantErrText: "tail_summarize_threshold_tokens",
		},
		{
			name:        "requires positive recent-events-kept",
			cfg:         config.CorrelatorConfig{TailSummarizeThresholdTokens: 6000, RecentEventsKept: 0},
			wantErrText: "recent_events_kept",
		},
		{
			name: "passes when both positive",
			cfg:  config.CorrelatorConfig{TailSummarizeThresholdTokens: 6000, RecentEventsKept: 20},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if tt.wantErrText == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErrText)
		})
	}
}

func TestLoad_ParsesCorrelatorBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unjira.config.json")
	body := `{"correlator": {"tail_summarize_threshold_tokens": 6000, "recent_events_kept": 20}}`
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	cfg, err := config.Load(path)

	require.NoError(t, err)
	assert.Equal(t, 6000, cfg.Correlator.TailSummarizeThresholdTokens)
	assert.Equal(t, 20, cfg.Correlator.RecentEventsKept)
}
```

- [ ] **Step 2: Run to confirm fail**

Run: `env -u GOROOT -u GOPATH go test ./internal/config/ -run 'TestCorrelatorConfig|TestLoad_ParsesCorrelatorBlock' -v`
Expected: FAIL — `undefined: config.CorrelatorConfig`, `cfg.Correlator undefined`.

- [ ] **Step 3: Add the type + field**

In `internal/config/config.go`, add after the `LLMConfig`/`Validate` block (before `type Config`):

```go
// CorrelatorConfig configures narrative persistence and compaction. Both
// fields are required (positive), validated by Validate. See
// docs/superpowers/specs/2026-08-12-correlator-persist-design.md.
type CorrelatorConfig struct {
	// TailSummarizeThresholdTokens: once a narrative's post-boundary history
	// (recap + raw tail) estimates above this, Persist compacts the old tail
	// into the summary's recap prefix via one LLM call.
	TailSummarizeThresholdTokens int `json:"tail_summarize_threshold_tokens"`
	// RecentEventsKept: how many of the newest events stay raw after a
	// compaction — a count, so it's well-defined regardless of event density.
	RecentEventsKept int `json:"recent_events_kept"`
}

// Validate reports whether both correlator limits are set to usable values.
func (c CorrelatorConfig) Validate() error {
	if c.TailSummarizeThresholdTokens <= 0 {
		return fmt.Errorf("correlator.tail_summarize_threshold_tokens must be a positive number of tokens")
	}
	if c.RecentEventsKept <= 0 {
		return fmt.Errorf("correlator.recent_events_kept must be a positive count")
	}

	return nil
}
```

Then add the field to `Config` (after `LLM`):

```go
	LLM        LLMConfig     `json:"llm"`
	Correlator CorrelatorConfig `json:"correlator"`
	DBPath     string        `json:"db_path"`
```

- [ ] **Step 4: Add the example block**

In `config/unjira.example.json`, insert between the `llm` block and `db_path`:

```json
  "correlator": {
    "tail_summarize_threshold_tokens": 6000,
    "recent_events_kept": 20
  },
```

- [ ] **Step 5: gofmt, run, verify example parses, commit**

```bash
env -u GOROOT -u GOPATH gofmt -w internal/config/config.go internal/config/config_test.go
env -u GOROOT -u GOPATH go test ./internal/config/ -v
python3 -c "import json; json.load(open('config/unjira.example.json'))" && echo VALID
git add internal/config/config.go internal/config/config_test.go config/unjira.example.json
git commit -m "Add CorrelatorConfig with tail-summarize threshold and recent-events-kept"
```

Expected: all config tests pass; `VALID`.

---

## Task 4: `compaction_boundary` column + idempotent migration

**Files:**
- Modify: `internal/store/store.go`
- Modify: `internal/store/store_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/store/store_test.go`:

```go
func TestOpen_AddsCompactionBoundaryColumnToExistingNarrativesTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	// First open: applies current schema (includes the column for a fresh DB).
	s1, err := store.Open(path)
	require.NoError(t, err)
	require.NoError(t, s1.Close())

	// Second open must succeed (idempotent migration must not error when the
	// column already exists).
	s2, err := store.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s2.Close() })

	// The column is usable: insert a narrative and read it back.
	id, err := s2.InsertNarrative(
		time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 1, 1, 0, 0, 0, time.UTC),
		"t", "s",
	)
	require.NoError(t, err)
	row, err := s2.GetNarrative(id)
	require.NoError(t, err)
	assert.Nil(t, row.CompactionBoundary)
}
```

Note: this test depends on `InsertNarrative`/`GetNarrative`/`NarrativeRow.CompactionBoundary` from
Task 5. To keep Task 4 self-contained and red→green on the migration alone, for THIS task first
write a narrower test that only asserts double-`Open` succeeds and the column exists via
`PRAGMA table_info`:

```go
func TestOpen_CompactionBoundaryColumnPresentAndReopenable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s1, err := store.Open(path)
	require.NoError(t, err)
	require.NoError(t, s1.Close())

	// Reopen must not error (idempotent ALTER guard).
	s2, err := store.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s2.Close() })

	// Column exists (queryable without error).
	cols, err := s2.NarrativeColumns() // tiny test-support accessor added in Step 3
	require.NoError(t, err)
	assert.Contains(t, cols, "compaction_boundary")
}
```

- [ ] **Step 2: Run to confirm fail**

Run: `env -u GOROOT -u GOPATH go test ./internal/store/ -run TestOpen_CompactionBoundaryColumnPresentAndReopenable -v`
Expected: FAIL — `NarrativeColumns` undefined and/or column absent.

- [ ] **Step 3: Add the column to the schema + idempotent migration in `Open`**

In `internal/store/store.go`, add `compaction_boundary TEXT` to the `narratives` `CREATE TABLE`
(after `status`, before `created_at`):

```sql
    status       TEXT NOT NULL DEFAULT 'open',
    compaction_boundary TEXT,
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
```

Then, in `Open`, after the `db.Exec(schema)` block, add an idempotent migration that adds the column
to a pre-existing `narratives` table (where `CREATE TABLE IF NOT EXISTS` was a no-op):

```go
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("applying schema to %s: %w", dbPath, err)
	}

	if err := ensureNarrativeCompactionBoundary(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrating narratives.compaction_boundary in %s: %w", dbPath, err)
	}
```

Add the migration helper (checks presence via `PRAGMA table_info`, adds only if absent — no
error-string matching, fully idempotent):

```go
// ensureNarrativeCompactionBoundary adds narratives.compaction_boundary to a
// database whose narratives table predates the column. CREATE TABLE IF NOT
// EXISTS is a no-op on an existing table, so a fresh DB gets the column from
// the schema while an older DB needs this explicit ALTER. Idempotent: it
// checks column presence first and does nothing when already present.
func ensureNarrativeCompactionBoundary(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(narratives)`)
	if err != nil {
		return fmt.Errorf("reading narratives table info: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			cid                        int
			name, colType              string
			notNull, primaryKey        int
			dfltValue                  sql.NullString
		)
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &primaryKey); err != nil {
			return fmt.Errorf("scanning narratives table info: %w", err)
		}
		if name == "compaction_boundary" {
			return rows.Err() // already present
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	if _, err := db.Exec(`ALTER TABLE narratives ADD COLUMN compaction_boundary TEXT`); err != nil {
		return fmt.Errorf("adding compaction_boundary column: %w", err)
	}

	return nil
}
```

Add the tiny test-support accessor `NarrativeColumns` (used only by the migration test):

```go
// NarrativeColumns returns the column names of the narratives table — a thin
// introspection helper for tests verifying the compaction_boundary migration.
func (s *Store) NarrativeColumns() ([]string, error) {
	rows, err := s.db.Query(`PRAGMA table_info(narratives)`)
	if err != nil {
		return nil, fmt.Errorf("reading narratives table info: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var cols []string
	for rows.Next() {
		var (
			cid                 int
			name, colType       string
			notNull, primaryKey int
			dfltValue           sql.NullString
		)
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &primaryKey); err != nil {
			return nil, fmt.Errorf("scanning narratives table info: %w", err)
		}
		cols = append(cols, name)
	}

	return cols, rows.Err()
}
```

- [ ] **Step 4: Run + commit**

Run: `env -u GOROOT -u GOPATH go test ./internal/store/ -run TestOpen_CompactionBoundaryColumnPresentAndReopenable -v` → PASS.
Also run the whole store package to confirm no regression: `env -u GOROOT -u GOPATH go test ./internal/store/ -v`.

```bash
env -u GOROOT -u GOPATH gofmt -w internal/store/store.go internal/store/store_test.go
git add internal/store/store.go internal/store/store_test.go
git commit -m "Add narratives.compaction_boundary column with idempotent migration"
```

---

## Task 5: narrative store accessors (`InsertNarrative`/`GetNarrative`/`ExtendNarrative`/`SetCompactionBoundary`/`AddNarrativeEvents`/`EventIDByExternalID`)

**Files:**
- Modify: `internal/store/store.go`
- Modify: `internal/store/store_test.go`

- [ ] **Step 1: Write failing tests**

Add to `internal/store/store_test.go`:

```go
func seedEvent(t *testing.T, s *store.Store, externalID, summary string, at time.Time) {
	t.Helper()
	e := events.NewEvent("claude_code", externalID, at, summary)
	_, err := s.InsertEvent(e)
	require.NoError(t, err)
}

func TestNarrative_InsertGetRoundTrip(t *testing.T) {
	s := openStore(t)
	ws := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	we := ws.Add(time.Hour)

	id, err := s.InsertNarrative(ws, we, "Title", "Summary")
	require.NoError(t, err)

	row, err := s.GetNarrative(id)
	require.NoError(t, err)
	assert.Equal(t, id, row.ID)
	assert.Equal(t, "Title", row.Title)
	assert.Equal(t, "Summary", row.Summary)
	assert.Equal(t, "open", row.Status)
	assert.True(t, ws.Equal(row.WindowStart))
	assert.True(t, we.Equal(row.WindowEnd))
	assert.Empty(t, row.IssueKey)
	assert.Nil(t, row.CompactionBoundary)
}

func TestGetNarrative_MissingReturnsErrNarrativeNotFound(t *testing.T) {
	s := openStore(t)
	_, err := s.GetNarrative(999)
	require.ErrorIs(t, err, store.ErrNarrativeNotFound)
}

func TestExtendNarrative_UpdatesWindowEndAndSummary(t *testing.T) {
	s := openStore(t)
	ws := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	id, err := s.InsertNarrative(ws, ws.Add(time.Hour), "T", "old")
	require.NoError(t, err)

	newEnd := ws.Add(3 * time.Hour)
	require.NoError(t, s.ExtendNarrative(id, newEnd, "new summary"))

	row, err := s.GetNarrative(id)
	require.NoError(t, err)
	assert.True(t, newEnd.Equal(row.WindowEnd))
	assert.Equal(t, "new summary", row.Summary)
}

func TestSetCompactionBoundary_PersistsBoundaryAndRecap(t *testing.T) {
	s := openStore(t)
	ws := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	id, err := s.InsertNarrative(ws, ws.Add(time.Hour), "T", "s")
	require.NoError(t, err)

	boundary := ws.Add(30 * time.Minute)
	require.NoError(t, s.SetCompactionBoundary(id, boundary, "recap: earlier work"))

	row, err := s.GetNarrative(id)
	require.NoError(t, err)
	require.NotNil(t, row.CompactionBoundary)
	assert.True(t, boundary.Equal(*row.CompactionBoundary))
	assert.Equal(t, "recap: earlier work", row.Summary)
}

func TestAddNarrativeEvents_IsIdempotent(t *testing.T) {
	s := openStore(t)
	ws := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	seedEvent(t, s, "e1", "first", ws)
	id, err := s.InsertNarrative(ws, ws.Add(time.Hour), "T", "s")
	require.NoError(t, err)
	eid, err := s.EventIDByExternalID("claude_code", "e1")
	require.NoError(t, err)

	require.NoError(t, s.AddNarrativeEvents(id, []int64{eid}))
	require.NoError(t, s.AddNarrativeEvents(id, []int64{eid})) // re-link: no error

	evs, err := s.NarrativeEventsForContext(id)
	require.NoError(t, err)
	require.Len(t, evs, 1)
	assert.Equal(t, "e1", evs[0].ExternalID)
}

func TestEventIDByExternalID_MissingReturnsErrEventNotFound(t *testing.T) {
	s := openStore(t)
	_, err := s.EventIDByExternalID("claude_code", "nope")
	require.ErrorIs(t, err, store.ErrEventNotFound)
}

func TestNarrativeEventsForContext_ExcludesAtOrBeforeBoundary(t *testing.T) {
	s := openStore(t)
	ws := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	seedEvent(t, s, "old", "old event", ws)
	seedEvent(t, s, "new", "new event", ws.Add(time.Hour))
	id, err := s.InsertNarrative(ws, ws.Add(2*time.Hour), "T", "s")
	require.NoError(t, err)
	oldID, err := s.EventIDByExternalID("claude_code", "old")
	require.NoError(t, err)
	newID, err := s.EventIDByExternalID("claude_code", "new")
	require.NoError(t, err)
	require.NoError(t, s.AddNarrativeEvents(id, []int64{oldID, newID}))

	// Boundary at the old event's time: strictly-after filter excludes it,
	// keeps the newer one.
	require.NoError(t, s.SetCompactionBoundary(id, ws, "recap"))

	evs, err := s.NarrativeEventsForContext(id)
	require.NoError(t, err)
	require.Len(t, evs, 1)
	assert.Equal(t, "new", evs[0].ExternalID)
}
```

- [ ] **Step 2: Run to confirm fail**

Run: `env -u GOROOT -u GOPATH go test ./internal/store/ -run 'TestNarrative|TestGetNarrative|TestExtendNarrative|TestSetCompactionBoundary|TestAddNarrativeEvents|TestEventIDByExternalID' -v`
Expected: FAIL — accessors undefined.

- [ ] **Step 3: Implement the accessors + types**

Add to `internal/store/store.go`. First the sentinels near the existing `ErrLocalIssueNotFound`:

```go
// ErrNarrativeNotFound is returned by GetNarrative when no row matches.
var ErrNarrativeNotFound = errors.New("narrative not found")

// ErrEventNotFound is returned by EventIDByExternalID when no event matches
// the given (source, external_id).
var ErrEventNotFound = errors.New("event not found")
```

Then a `-- narratives --` section:

```go
// NarrativeRow mirrors a narratives table row. correlator.Persist maps this
// to/from its own correlator.Narrative domain type (keeping store free of any
// correlator import — the dependency runs correlator -> store).
type NarrativeRow struct {
	ID                 int64
	WindowStart        time.Time
	WindowEnd          time.Time
	Title              string
	Summary            string
	IssueKey           string
	Confidence         float64
	Status             string
	CompactionBoundary *time.Time
}

// InsertNarrative inserts a new narrative row (status 'open', no compaction
// boundary) and returns its id.
func (s *Store) InsertNarrative(windowStart, windowEnd time.Time, title, summary string) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO narratives (window_start, window_end, title, summary) VALUES (?, ?, ?, ?)`,
		windowStart.Format(time.RFC3339), windowEnd.Format(time.RFC3339), title, summary,
	)
	if err != nil {
		return 0, fmt.Errorf("inserting narrative %q: %w", title, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("getting inserted narrative id for %q: %w", title, err)
	}

	return id, nil
}

// GetNarrative returns the narrative with the given id, or
// ErrNarrativeNotFound.
func (s *Store) GetNarrative(id int64) (NarrativeRow, error) {
	var (
		row                     NarrativeRow
		windowStart, windowEnd  string
		issueKey                sql.NullString
		confidence              sql.NullFloat64
		compactionBoundary      sql.NullString
	)
	err := s.db.QueryRow(
		`SELECT id, window_start, window_end, title, summary, issue_key, confidence, status, compaction_boundary
		 FROM narratives WHERE id = ?`, id,
	).Scan(&row.ID, &windowStart, &windowEnd, &row.Title, &row.Summary,
		&issueKey, &confidence, &row.Status, &compactionBoundary)
	if err == sql.ErrNoRows {
		return NarrativeRow{}, ErrNarrativeNotFound
	}
	if err != nil {
		return NarrativeRow{}, fmt.Errorf("querying narrative %d: %w", id, err)
	}

	if row.WindowStart, err = time.Parse(time.RFC3339, windowStart); err != nil {
		return NarrativeRow{}, fmt.Errorf("parsing window_start for narrative %d: %w", id, err)
	}
	if row.WindowEnd, err = time.Parse(time.RFC3339, windowEnd); err != nil {
		return NarrativeRow{}, fmt.Errorf("parsing window_end for narrative %d: %w", id, err)
	}
	row.IssueKey = issueKey.String
	if confidence.Valid {
		row.Confidence = confidence.Float64
	}
	if compactionBoundary.Valid {
		parsed, err := time.Parse(time.RFC3339, compactionBoundary.String)
		if err != nil {
			return NarrativeRow{}, fmt.Errorf("parsing compaction_boundary for narrative %d: %w", id, err)
		}
		row.CompactionBoundary = &parsed
	}

	return row, nil
}

// ExtendNarrative advances a narrative's window_end and overwrites its
// summary.
func (s *Store) ExtendNarrative(id int64, windowEnd time.Time, summary string) error {
	_, err := s.db.Exec(
		`UPDATE narratives SET window_end = ?, summary = ? WHERE id = ?`,
		windowEnd.Format(time.RFC3339), summary, id,
	)
	if err != nil {
		return fmt.Errorf("extending narrative %d: %w", id, err)
	}

	return nil
}

// SetCompactionBoundary records the occurred_at of the newest compacted event
// and stores the recap-prefixed summary.
func (s *Store) SetCompactionBoundary(id int64, boundary time.Time, recapSummary string) error {
	_, err := s.db.Exec(
		`UPDATE narratives SET compaction_boundary = ?, summary = ? WHERE id = ?`,
		boundary.Format(time.RFC3339), recapSummary, id,
	)
	if err != nil {
		return fmt.Errorf("setting compaction boundary for narrative %d: %w", id, err)
	}

	return nil
}

// AddNarrativeEvents links events to a narrative (INSERT OR IGNORE, so
// re-linking an already-linked event is a harmless no-op).
func (s *Store) AddNarrativeEvents(narrativeID int64, eventIDs []int64) error {
	for _, eid := range eventIDs {
		if _, err := s.db.Exec(
			`INSERT OR IGNORE INTO narrative_events (narrative_id, event_id) VALUES (?, ?)`,
			narrativeID, eid,
		); err != nil {
			return fmt.Errorf("linking event %d to narrative %d: %w", eid, narrativeID, err)
		}
	}

	return nil
}

// EventIDByExternalID returns the row id of the event with the given
// (source, external_id), or ErrEventNotFound.
func (s *Store) EventIDByExternalID(source, externalID string) (int64, error) {
	var id int64
	err := s.db.QueryRow(
		`SELECT id FROM events WHERE source = ? AND external_id = ?`, source, externalID,
	).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, ErrEventNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("querying event id for %s/%s: %w", source, externalID, err)
	}

	return id, nil
}

// NarrativeEventsForContext returns a narrative's events with occurred_at
// strictly after its compaction_boundary (all of them when the boundary is
// NULL), ordered by occurred_at — the events the caller hydrates into
// correlator.Narrative.Events. The recap of anything at/before the boundary
// already lives in the summary.
func (s *Store) NarrativeEventsForContext(narrativeID int64) ([]events.Event, error) {
	rows, err := s.db.Query(
		`SELECT e.source, e.external_id, e.occurred_at, e.actor, e.summary, e.artifacts, e.raw_ref
		 FROM events e
		 JOIN narrative_events ne ON ne.event_id = e.id
		 WHERE ne.narrative_id = ?
		   AND (
		     (SELECT compaction_boundary FROM narratives WHERE id = ?) IS NULL
		     OR e.occurred_at > (SELECT compaction_boundary FROM narratives WHERE id = ?)
		   )
		 ORDER BY e.occurred_at`,
		narrativeID, narrativeID, narrativeID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying context events for narrative %d: %w", narrativeID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []events.Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning context event row: %w", err)
		}
		out = append(out, e)
	}

	return out, rows.Err()
}
```

Confirm `errors` is imported (it is — `ErrLocalIssueNotFound` uses it).

- [ ] **Step 4: Run + commit**

Run: `env -u GOROOT -u GOPATH go test ./internal/store/ -v` → all pass.

```bash
env -u GOROOT -u GOPATH gofmt -w internal/store/store.go internal/store/store_test.go
env -u GOROOT -u GOPATH go vet ./internal/store/...
git add internal/store/store.go internal/store/store_test.go
git commit -m "Add narrative store accessors + NarrativeEventsForContext hydration query"
```

---

## Task 6: transaction seam for atomic `Persist`

**Files:**
- Modify: `internal/store/store.go`
- Modify: `internal/store/store_test.go`

`Persist` must be all-or-nothing. Provide a `WithTx` helper that runs a function inside a
transaction, committing on nil error and rolling back otherwise, and `*Tx`-scoped variants of the
write accessors `Persist` uses.

- [ ] **Step 1: Write the failing test**

```go
func TestWithTx_RollsBackOnError(t *testing.T) {
	s := openStore(t)
	ws := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	wantErr := errors.New("boom")
	err := s.WithTx(func(tx *store.Tx) error {
		_, iErr := tx.InsertNarrative(ws, ws.Add(time.Hour), "T", "s")
		require.NoError(t, iErr)
		return wantErr // force rollback
	})
	require.ErrorIs(t, err, wantErr)

	// The narrative inserted inside the rolled-back tx must not be present.
	// (No narrative with id 1 should exist.)
	_, gErr := s.GetNarrative(1)
	require.ErrorIs(t, gErr, store.ErrNarrativeNotFound)
}

func TestWithTx_CommitsOnSuccess(t *testing.T) {
	s := openStore(t)
	ws := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	var id int64
	err := s.WithTx(func(tx *store.Tx) error {
		var iErr error
		id, iErr = tx.InsertNarrative(ws, ws.Add(time.Hour), "T", "s")
		return iErr
	})
	require.NoError(t, err)

	row, err := s.GetNarrative(id)
	require.NoError(t, err)
	assert.Equal(t, "T", row.Title)
}
```

Add `"errors"` to `store_test.go` imports if not present.

- [ ] **Step 2: Run to confirm fail** — `undefined: store.Tx`, `s.WithTx undefined`.

- [ ] **Step 3: Implement `Tx` + `WithTx`**

The cleanest shape given the existing accessors are `*Store` methods: introduce a small `dbExec`
interface satisfied by both `*sql.DB` and `*sql.Tx`, have the write accessors delegate to an
unexported implementation taking that interface, and expose `*Tx` wrappers. To keep this task
bounded, implement `WithTx` + a `*Tx` type that carries a `*sql.Tx` and re-implements exactly the
write methods `Persist` needs (`InsertNarrative`, `ExtendNarrative`, `SetCompactionBoundary`,
`AddNarrativeEvents`) plus the reads it needs inside the tx (`GetNarrative`, `EventIDByExternalID`).

```go
// Tx is a transaction-scoped handle exposing the narrative write/read methods
// correlator.Persist needs to run atomically. Obtain one via WithTx.
type Tx struct {
	tx *sql.Tx
}

// WithTx runs fn inside a single transaction, committing if fn returns nil and
// rolling back (preserving fn's error) otherwise. This is how Persist gets its
// all-or-nothing guarantee.
func (s *Store) WithTx(fn func(*Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}

	if err := fn(&Tx{tx: tx}); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			return fmt.Errorf("rolling back after error %v: %w", err, rbErr)
		}
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing transaction: %w", err)
	}

	return nil
}
```

Refactor the six accessors from Task 5 so their SQL bodies live in unexported helpers taking a
`dbExec`/`dbQuerier` interface, and both `*Store` and `*Tx` expose the public methods delegating to
those helpers. Concretely, introduce:

```go
type dbConn interface {
	Exec(query string, args ...any) (sql.Result, error)
	QueryRow(query string, args ...any) *sql.Row
	Query(query string, args ...any) (*sql.Rows, error)
}
```

Both `*sql.DB` and `*sql.Tx` satisfy `dbConn`. Move each accessor's body into a package-level
`func fooImpl(c dbConn, ...) ...`, and make `(*Store).Foo` call `fooImpl(s.db, ...)` and
`(*Tx).Foo` call `fooImpl(t.tx, ...)`. Add the `*Tx` methods for exactly: `InsertNarrative`,
`GetNarrative`, `ExtendNarrative`, `SetCompactionBoundary`, `AddNarrativeEvents`,
`EventIDByExternalID`. (No `*Tx` variant needed for `NarrativeEventsForContext` — compaction reads
happen before the tx; see Task 8.)

This is a mechanical refactor; the Task-5 tests must all still pass unchanged afterward, proving the
`*Store` methods kept their behavior.

- [ ] **Step 4: Run + commit**

Run: `env -u GOROOT -u GOPATH go test ./internal/store/ -v` → all pass (Task 5 tests + the two new tx tests).

```bash
env -u GOROOT -u GOPATH gofmt -w internal/store/store.go internal/store/store_test.go
env -u GOROOT -u GOPATH go vet ./internal/store/...
git add internal/store/store.go internal/store/store_test.go
git commit -m "Add WithTx transaction seam + Tx-scoped narrative accessors"
```

---

## Task 7: `pipeline_lock` lease

**Files:**
- Modify: `internal/store/store.go`
- Modify: `internal/store/store_test.go`

- [ ] **Step 1: Write failing tests**

```go
func TestTryAcquire_UnheldSucceeds(t *testing.T) {
	s := openStore(t)
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	ok, err := s.TryAcquire("run-1", now, time.Minute)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestTryAcquire_HeldBeforeExpiryFails(t *testing.T) {
	s := openStore(t)
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	ok, err := s.TryAcquire("run-1", now, time.Minute)
	require.NoError(t, err)
	require.True(t, ok)

	ok, err = s.TryAcquire("run-2", now.Add(30*time.Second), time.Minute)
	require.NoError(t, err)
	assert.False(t, ok, "lease still valid, second acquirer must fail")
}

func TestTryAcquire_ExpiredLeaseCanBeStolen(t *testing.T) {
	s := openStore(t)
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	ok, err := s.TryAcquire("run-1", now, time.Minute)
	require.NoError(t, err)
	require.True(t, ok)

	// 2 minutes later, run-1's lease has expired; run-2 steals it.
	ok, err = s.TryAcquire("run-2", now.Add(2*time.Minute), time.Minute)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestReleaseLock_ByHolderFreesLock(t *testing.T) {
	s := openStore(t)
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	_, err := s.TryAcquire("run-1", now, time.Minute)
	require.NoError(t, err)
	require.NoError(t, s.ReleaseLock("run-1"))

	// Now immediately acquirable by another run, before any expiry.
	ok, err := s.TryAcquire("run-2", now.Add(time.Second), time.Minute)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestReleaseLock_ByNonHolderIsNoOp(t *testing.T) {
	s := openStore(t)
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	_, err := s.TryAcquire("run-1", now, time.Minute)
	require.NoError(t, err)

	require.NoError(t, s.ReleaseLock("run-2")) // not the holder: no-op, no error

	// run-1 still holds it.
	ok, err := s.TryAcquire("run-3", now.Add(30*time.Second), time.Minute)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestAcquire_BlocksThenSucceedsWhenLeaseExpires(t *testing.T) {
	s := openStore(t)
	start := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	_, err := s.TryAcquire("run-1", start, 100*time.Millisecond)
	require.NoError(t, err)

	// A clock that advances past the lease each call.
	calls := 0
	clock := func() time.Time {
		calls++
		return start.Add(time.Duration(calls) * 60 * time.Millisecond)
	}
	err = s.Acquire(t.Context(), "run-2", clock, 100*time.Millisecond, 10*time.Millisecond)
	require.NoError(t, err)
}

func TestAcquire_HonorsContextCancellation(t *testing.T) {
	s := openStore(t)
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	_, err := s.TryAcquire("run-1", now, time.Hour) // long lease, never expires during test
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // already cancelled
	err = s.Acquire(ctx, "run-2", func() time.Time { return now }, time.Hour, 10*time.Millisecond)
	require.Error(t, err)
}
```

Add `"context"` to `store_test.go` imports if not present.

- [ ] **Step 2: Run to confirm fail** — lease methods undefined.

- [ ] **Step 3: Add the schema + methods**

Add to the `schema` string (after the `local_issue_comments` table):

```sql
CREATE TABLE IF NOT EXISTS pipeline_lock (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    run_id     TEXT NOT NULL,
    held_since TEXT NOT NULL,
    expires_at TEXT NOT NULL
);
```

Add a `-- pipeline lock --` section:

```go
// TryAcquire attempts to take the singleton pipeline lock without blocking.
// It succeeds when the lock is unheld or its lease has expired (expires_at <
// now), replacing the row with a fresh lease for runID; otherwise it returns
// false immediately. Stealing an expired lease logs a warning naming the
// stale run_id and how long it was held — the crash-recovery path. now is
// passed in (not time.Now()) so steal-on-expiry is deterministically testable.
func (s *Store) TryAcquire(runID string, now time.Time, ttl time.Duration) (bool, error) {
	var (
		heldRunID  string
		expiresAt  string
	)
	err := s.db.QueryRow(`SELECT run_id, expires_at FROM pipeline_lock WHERE id = 1`).Scan(&heldRunID, &expiresAt)
	switch {
	case err == sql.ErrNoRows:
		// unheld
	case err != nil:
		return false, fmt.Errorf("reading pipeline lock: %w", err)
	default:
		exp, perr := time.Parse(time.RFC3339, expiresAt)
		if perr != nil {
			return false, fmt.Errorf("parsing pipeline lock expiry: %w", perr)
		}
		if !now.Before(exp) {
			log.Printf("pipeline lock: stealing expired lease from run_id=%s (expired %s ago)", heldRunID, now.Sub(exp))
		} else {
			return false, nil // still held
		}
	}

	if _, err := s.db.Exec(
		`INSERT INTO pipeline_lock (id, run_id, held_since, expires_at) VALUES (1, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET run_id = excluded.run_id, held_since = excluded.held_since, expires_at = excluded.expires_at`,
		runID, now.Format(time.RFC3339), now.Add(ttl).Format(time.RFC3339),
	); err != nil {
		return false, fmt.Errorf("acquiring pipeline lock for %s: %w", runID, err)
	}

	return true, nil
}

// Acquire is TryAcquire's blocking sibling: it polls every poll interval
// until it can take the lock (unheld or lease expired), honoring ctx
// cancellation. now is a clock func since Acquire loops.
func (s *Store) Acquire(ctx context.Context, runID string, now func() time.Time, ttl, poll time.Duration) error {
	for {
		ok, err := s.TryAcquire(runID, now(), ttl)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("acquiring pipeline lock for %s: %w", runID, ctx.Err())
		case <-time.After(poll):
		}
	}
}

// ReleaseLock releases the pipeline lock iff it is currently held by runID
// (a no-op otherwise, so it never clobbers a newer holder that stole an
// expired lease).
func (s *Store) ReleaseLock(runID string) error {
	if _, err := s.db.Exec(`DELETE FROM pipeline_lock WHERE id = 1 AND run_id = ?`, runID); err != nil {
		return fmt.Errorf("releasing pipeline lock for %s: %w", runID, err)
	}

	return nil
}
```

Add `"log"` and `"context"` to `store.go`'s imports if not present.

- [ ] **Step 4: Run + commit**

Run: `env -u GOROOT -u GOPATH go test ./internal/store/ -v` → all pass.

```bash
env -u GOROOT -u GOPATH gofmt -w internal/store/store.go internal/store/store_test.go
env -u GOROOT -u GOPATH go vet ./internal/store/...
git add internal/store/store.go internal/store/store_test.go
git commit -m "Add pipeline_lock lease: TryAcquire/Acquire/ReleaseLock with steal-on-expiry"
```

---

## Task 8: `correlator.Persist` (new + extend + compaction)

**Files:**
- Modify: `internal/correlator/correlator.go`
- Modify: `internal/correlator/correlator_test.go`

`Persist` is the payload. It maps `ClusterResult`s to store writes atomically, and runs
tail-summarization compaction. Per the design, the compaction LLM call happens **before** the write
transaction (read current state → decide → LLM recap → then apply links+summary+boundary in one tx),
so no SQLite transaction is held open across an LLM round-trip; the lease lock serializes passes so
there is no concurrent-writer race.

- [ ] **Step 1: Write failing tests**

Add to `internal/correlator/correlator_test.go` (these need a real store; import
`github.com/jcogilvie/unjira/internal/store` and add an `openStore` helper mirroring the store
package's, plus a seed helper):

```go
func persistStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func seedPersistedEvent(t *testing.T, s *store.Store, extID, summary string, at time.Time) correlator.Event {
	t.Helper()
	e := events.NewEvent("claude_code", extID, at, summary)
	_, err := s.InsertEvent(e)
	require.NoError(t, err)
	return e
}

func TestPersist_NewNarrativeRoundTrips(t *testing.T) {
	s := persistStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	e1 := seedPersistedEvent(t, s, "e1", "started work", base)
	e2 := seedPersistedEvent(t, s, "e2", "more work", base.Add(time.Minute))
	llm := &fakeLLM{} // no LLM call expected for a new narrative

	cfg := config.CorrelatorConfig{TailSummarizeThresholdTokens: 1_000_000, RecentEventsKept: 20}
	got, err := correlator.Persist(t.Context(), s, llm, []correlator.ClusterResult{{
		Kind: correlator.ClusterNew, Title: "New story", Summary: "did some work",
		Events: []correlator.Event{e1, e2},
	}}, cfg)

	require.NoError(t, err)
	require.Len(t, got, 1)
	require.NotZero(t, got[0].ID)
	assert.Empty(t, llm.prompts, "new narrative needs no compaction call")

	row, err := s.GetNarrative(got[0].ID)
	require.NoError(t, err)
	assert.Equal(t, "New story", row.Title)
	assert.True(t, base.Equal(row.WindowStart))
	assert.True(t, base.Add(time.Minute).Equal(row.WindowEnd))
	ctxEvents, err := s.NarrativeEventsForContext(got[0].ID)
	require.NoError(t, err)
	assert.Len(t, ctxEvents, 2)
}

func TestPersist_ExtendUpdatesExistingNarrative(t *testing.T) {
	s := persistStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	e1 := seedPersistedEvent(t, s, "e1", "start", base)
	id, err := s.InsertNarrative(base, base.Add(time.Minute), "Story", "old summary")
	require.NoError(t, err)
	eid, err := s.EventIDByExternalID("claude_code", "e1")
	require.NoError(t, err)
	require.NoError(t, s.AddNarrativeEvents(id, []int64{eid}))

	e2 := seedPersistedEvent(t, s, "e2", "continued", base.Add(time.Hour))
	cfg := config.CorrelatorConfig{TailSummarizeThresholdTokens: 1_000_000, RecentEventsKept: 20}
	got, err := correlator.Persist(t.Context(), s, &fakeLLM{}, []correlator.ClusterResult{{
		Kind: correlator.ClusterExtends, NarrativeID: id, Title: "Story", Summary: "new cumulative summary",
		Events: []correlator.Event{e2},
	}}, cfg)

	require.NoError(t, err)
	require.Len(t, got, 1)
	row, err := s.GetNarrative(id)
	require.NoError(t, err)
	assert.True(t, base.Add(time.Hour).Equal(row.WindowEnd), "window_end advanced")
	assert.Equal(t, "new cumulative summary", row.Summary)
	ctxEvents, err := s.NarrativeEventsForContext(id)
	require.NoError(t, err)
	assert.Len(t, ctxEvents, 2, "both events now linked")
}

func TestPersist_ExtendUnknownNarrativeIDErrorsLoudly(t *testing.T) {
	s := persistStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	e1 := seedPersistedEvent(t, s, "e1", "x", base)
	cfg := config.CorrelatorConfig{TailSummarizeThresholdTokens: 1_000_000, RecentEventsKept: 20}

	_, err := correlator.Persist(t.Context(), s, &fakeLLM{}, []correlator.ClusterResult{{
		Kind: correlator.ClusterExtends, NarrativeID: 999, Title: "x", Summary: "x",
		Events: []correlator.Event{e1},
	}}, cfg)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "999")
}

func TestPersist_EventNotInStoreErrorsLoudly(t *testing.T) {
	s := persistStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	// Event constructed but never inserted into the store.
	ghost := events.NewEvent("claude_code", "ghost", base, "never persisted")
	cfg := config.CorrelatorConfig{TailSummarizeThresholdTokens: 1_000_000, RecentEventsKept: 20}

	_, err := correlator.Persist(t.Context(), s, &fakeLLM{}, []correlator.ClusterResult{{
		Kind: correlator.ClusterNew, Title: "x", Summary: "x", Events: []correlator.Event{ghost},
	}}, cfg)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "ghost")
}

func TestPersist_AllOrNothingRollsBackFirstResultOnLaterFailure(t *testing.T) {
	s := persistStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	e1 := seedPersistedEvent(t, s, "e1", "ok", base)
	cfg := config.CorrelatorConfig{TailSummarizeThresholdTokens: 1_000_000, RecentEventsKept: 20}

	// First result is a valid new narrative; second references a missing id.
	_, err := correlator.Persist(t.Context(), s, &fakeLLM{}, []correlator.ClusterResult{
		{Kind: correlator.ClusterNew, Title: "First", Summary: "s", Events: []correlator.Event{e1}},
		{Kind: correlator.ClusterExtends, NarrativeID: 999, Title: "x", Summary: "x", Events: []correlator.Event{e1}},
	}, cfg)

	require.Error(t, err)
	// The first result's narrative must NOT have been persisted (rollback).
	_, gErr := s.GetNarrative(1)
	require.ErrorIs(t, gErr, store.ErrNarrativeNotFound)
}

func TestPersist_CompactsWhenHistoryExceedsThreshold(t *testing.T) {
	s := persistStore(t)
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	// Seed a narrative with several linked events; make its summary large so
	// the post-boundary history estimate exceeds a small threshold.
	id, err := s.InsertNarrative(base, base.Add(5*time.Hour), "Long", strings.Repeat("x", 4000))
	require.NoError(t, err)
	var linkIDs []int64
	for i := 0; i < 5; i++ {
		e := seedPersistedEvent(t, s, fmt.Sprintf("old-%d", i), strings.Repeat("y", 500), base.Add(time.Duration(i)*time.Hour))
		eid, err := s.EventIDByExternalID("claude_code", fmt.Sprintf("old-%d", i))
		require.NoError(t, err)
		linkIDs = append(linkIDs, eid)
	}
	require.NoError(t, s.AddNarrativeEvents(id, linkIDs))

	newE := seedPersistedEvent(t, s, "new-1", "newest", base.Add(6*time.Hour))
	llm := &fakeLLM{responses: []string{"recap: earlier work compacted"}}
	cfg := config.CorrelatorConfig{TailSummarizeThresholdTokens: 500, RecentEventsKept: 2}

	_, err = correlator.Persist(t.Context(), s, llm, []correlator.ClusterResult{{
		Kind: correlator.ClusterExtends, NarrativeID: id, Title: "Long", Summary: strings.Repeat("z", 4000),
		Events: []correlator.Event{newE},
	}}, cfg)

	require.NoError(t, err)
	require.Len(t, llm.prompts, 1, "compaction should make exactly one LLM call")

	row, err := s.GetNarrative(id)
	require.NoError(t, err)
	require.NotNil(t, row.CompactionBoundary, "boundary set after compaction")
	assert.Contains(t, row.Summary, "recap: earlier work compacted")

	// narrative_events rows are never deleted: all 6 events still linked in
	// the store even though context now returns only the recent tail.
	// (Query raw link count via a helper or NarrativeEventsForContext before/
	// after boundary — here we assert the recent tail is bounded.)
	ctxEvents, err := s.NarrativeEventsForContext(id)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(ctxEvents), cfg.RecentEventsKept+1) // recent kept + the new one, all post-boundary
}

func TestPersist_NoCompactionBelowThreshold(t *testing.T) {
	s := persistStore(t)
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	e1 := seedPersistedEvent(t, s, "e1", "start", base)
	id, err := s.InsertNarrative(base, base.Add(time.Minute), "Small", "tiny")
	require.NoError(t, err)
	eid, _ := s.EventIDByExternalID("claude_code", "e1")
	require.NoError(t, s.AddNarrativeEvents(id, []int64{eid}))

	e2 := seedPersistedEvent(t, s, "e2", "next", base.Add(time.Hour))
	llm := &fakeLLM{}
	cfg := config.CorrelatorConfig{TailSummarizeThresholdTokens: 1_000_000, RecentEventsKept: 20}

	_, err = correlator.Persist(t.Context(), s, llm, []correlator.ClusterResult{{
		Kind: correlator.ClusterExtends, NarrativeID: id, Title: "Small", Summary: "still tiny",
		Events: []correlator.Event{e2},
	}}, cfg)

	require.NoError(t, err)
	assert.Empty(t, llm.prompts, "below threshold: no compaction call")
	row, err := s.GetNarrative(id)
	require.NoError(t, err)
	assert.Nil(t, row.CompactionBoundary)
}

func TestPersist_EmptyResultsIsNoOp(t *testing.T) {
	s := persistStore(t)
	got, err := correlator.Persist(t.Context(), s, &fakeLLM{}, nil,
		config.CorrelatorConfig{TailSummarizeThresholdTokens: 100, RecentEventsKept: 2})
	require.NoError(t, err)
	assert.Empty(t, got)
}
```

Add imports to `correlator_test.go`: `"path/filepath"`, `"fmt"`, `"github.com/jcogilvie/unjira/internal/config"`,
`"github.com/jcogilvie/unjira/internal/store"` (and `"strings"` if not already present from slice 2).

- [ ] **Step 2: Run to confirm fail** — `correlator.Persist undefined`.

- [ ] **Step 3: Implement `Persist` + compaction helpers**

Add to `internal/correlator/correlator.go` (imports: add `"github.com/jcogilvie/unjira/internal/config"`
and `"github.com/jcogilvie/unjira/internal/store"`):

```go
// Persist writes Cluster's results to the narratives/narrative_events tables.
// See the compute/persist split in
// docs/superpowers/specs/2026-08-12-correlator-persist-design.md. Pure store
// writes plus at most one LLM call per extended narrative for tail-summarize
// compaction (made before the write transaction, never during it). Returns
// the narratives touched this run. All-or-nothing: any failure rolls the
// whole pass back and persists nothing.
func Persist(
	ctx context.Context,
	s *store.Store,
	llm llmClient,
	results []ClusterResult,
	cfg config.CorrelatorConfig,
) ([]Narrative, error) {
	if len(results) == 0 {
		return nil, nil
	}

	// Phase 1 (pre-transaction): resolve event ids, validate extends targets,
	// and compute any compaction recaps (the only LLM calls) so no LLM round-
	// trip happens inside the transaction.
	type prepared struct {
		result     ClusterResult
		eventIDs   []int64
		windowLo   time.Time
		windowHi   time.Time
		// compaction, if any (extends only):
		doCompact  bool
		recap      string
		boundary   time.Time
	}

	var preps []prepared
	for _, r := range results {
		p := prepared{result: r}

		// resolve event ids + window bounds
		for i, e := range r.Events {
			eid, err := s.EventIDByExternalID(e.Source, e.ExternalID)
			if err != nil {
				return nil, fmt.Errorf("resolving event %s/%s for narrative %q: %w", e.Source, e.ExternalID, r.Title, err)
			}
			p.eventIDs = append(p.eventIDs, eid)
			if i == 0 || e.OccurredAt.Before(p.windowLo) {
				p.windowLo = e.OccurredAt
			}
			if i == 0 || e.OccurredAt.After(p.windowHi) {
				p.windowHi = e.OccurredAt
			}
		}

		if r.Kind == ClusterExtends {
			row, err := s.GetNarrative(r.NarrativeID)
			if err != nil {
				return nil, fmt.Errorf("extending narrative %d: %w", r.NarrativeID, err)
			}
			// decide compaction: estimate the post-extend history size.
			// (recap prefix already in row.Summary + the incoming summary is
			// what future context carries; use the incoming cumulative summary
			// as the size proxy, matching the design's summary-carried model.)
			if estimateTokens(r.Summary) > cfg.TailSummarizeThresholdTokens {
				recap, boundary, err := compactNarrativeTail(ctx, s, llm, r.NarrativeID, row.Summary, cfg.RecentEventsKept)
				if err != nil {
					return nil, err
				}
				p.doCompact = true
				p.recap = recap
				p.boundary = boundary
			}
		}

		preps = append(preps, p)
	}

	// Phase 2 (transaction): apply all writes atomically.
	var touched []Narrative
	err := s.WithTx(func(tx *store.Tx) error {
		for _, p := range preps {
			r := p.result
			switch r.Kind {
			case ClusterNew:
				id, err := tx.InsertNarrative(p.windowLo, p.windowHi, r.Title, r.Summary)
				if err != nil {
					return err
				}
				if err := tx.AddNarrativeEvents(id, p.eventIDs); err != nil {
					return err
				}
				touched = append(touched, Narrative{
					ID: id, WindowStart: p.windowLo, WindowEnd: p.windowHi,
					Title: r.Title, Summary: r.Summary, Status: "open",
				})
			case ClusterExtends:
				row, err := tx.GetNarrative(r.NarrativeID)
				if err != nil {
					return fmt.Errorf("extending narrative %d: %w", r.NarrativeID, err)
				}
				newEnd := row.WindowEnd
				if p.windowHi.After(newEnd) {
					newEnd = p.windowHi
				}
				if err := tx.ExtendNarrative(r.NarrativeID, newEnd, r.Summary); err != nil {
					return err
				}
				if err := tx.AddNarrativeEvents(r.NarrativeID, p.eventIDs); err != nil {
					return err
				}
				if p.doCompact {
					if err := tx.SetCompactionBoundary(r.NarrativeID, p.boundary, p.recap); err != nil {
						return err
					}
				}
				touched = append(touched, Narrative{
					ID: r.NarrativeID, WindowStart: row.WindowStart, WindowEnd: newEnd,
					Title: r.Title, Summary: r.Summary, Status: row.Status,
				})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return touched, nil
}

// compactNarrativeTail summarizes a narrative's older events (everything but
// the newest recentEventsKept, among its current post-boundary context
// events) into a recap via one LLM call, and returns the recap plus the new
// compaction boundary (the occurred_at of the newest compacted event). It
// does not write anything — the caller applies the recap/boundary inside the
// persist transaction. Logs what it folded, for auditability.
func compactNarrativeTail(
	ctx context.Context,
	s *store.Store,
	llm llmClient,
	narrativeID int64,
	existingSummary string,
	recentEventsKept int,
) (recap string, boundary time.Time, err error) {
	ctxEvents, err := s.NarrativeEventsForContext(narrativeID)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("loading context events to compact narrative %d: %w", narrativeID, err)
	}
	if len(ctxEvents) <= recentEventsKept {
		// Nothing to compact (not enough history beyond the recent tail).
		// Return a zero boundary the caller treats as "no compaction".
		return "", time.Time{}, nil
	}

	toCompact := ctxEvents[:len(ctxEvents)-recentEventsKept]
	boundary = toCompact[len(toCompact)-1].OccurredAt

	var b strings.Builder
	fmt.Fprintf(&b, "Existing recap/summary:\n%s\n\nOlder events to fold into a concise recap:\n", existingSummary)
	for _, e := range toCompact {
		fmt.Fprintf(&b, "- [%s] %q (occurred_at=%s)\n", e.Source, e.Summary, e.OccurredAt.Format(time.RFC3339))
	}

	recap, err = llm.Complete(ctx, compactionSystemPrompt, b.String())
	if err != nil {
		return "", time.Time{}, fmt.Errorf("compacting narrative %d tail: %w", narrativeID, err)
	}

	log.Printf("correlator: compacted narrative %d — folded %d event(s) up to %s into recap",
		narrativeID, len(toCompact), boundary.Format(time.RFC3339))

	return recap, boundary, nil
}

const compactionSystemPrompt = `You are compacting the older history of an ongoing work narrative. Given the existing recap/summary and a list of older events, produce a single concise recap paragraph that preserves the decisions, problems, and outcomes a future reader would need. Return ONLY the recap text, no preamble, no markdown.`
```

Add `"log"` to `correlator.go`'s imports.

**Note on the compaction-boundary decision detail:** `compactNarrativeTail` returns a zero
`boundary` when there aren't enough events to compact. The caller (`Persist`) must treat a zero
`boundary` as "no compaction happened" even if `doCompact` was tentatively set — adjust Step 3's
`Persist` so `p.doCompact` is only true when `compactNarrativeTail` returns a non-zero boundary:

```go
			if estimateTokens(r.Summary) > cfg.TailSummarizeThresholdTokens {
				recap, boundary, err := compactNarrativeTail(ctx, s, llm, r.NarrativeID, row.Summary, cfg.RecentEventsKept)
				if err != nil {
					return nil, err
				}
				if !boundary.IsZero() {
					p.doCompact = true
					p.recap = recap
					p.boundary = boundary
				}
			}
```

(Fold this guard into the code you write — it's called out separately only for emphasis.)

- [ ] **Step 4: Run to confirm pass**

Run: `env -u GOROOT -u GOPATH go test ./internal/correlator/ -v`

Expected: all `TestPersist_*` pass, plus every Cluster test still green. If
`TestPersist_CompactsWhenHistoryExceedsThreshold` fails on the context-count assertion, re-check
that `NarrativeEventsForContext` filters strictly-after the boundary and that `RecentEventsKept` is
honored.

- [ ] **Step 5: gofmt + vet + commit**

```bash
env -u GOROOT -u GOPATH gofmt -w internal/correlator/correlator.go internal/correlator/correlator_test.go
env -u GOROOT -u GOPATH go vet ./internal/correlator/...
git add internal/correlator/correlator.go internal/correlator/correlator_test.go
git commit -m "Add correlator.Persist: new/extend narratives + pre-transaction tail-summarize compaction"
```

---

## Task 9: full regression + lint

**Files:** none (verification only)

- [ ] **Step 1:** `env -u GOROOT -u GOPATH go test ./... 2>&1 | tail -30` — every package `ok`.
- [ ] **Step 2:** `env -u GOROOT -u GOPATH go vet ./...` — clean.
- [ ] **Step 3:** `earthly +reviewable` (binary is `earthly` on PATH; if unavailable, fall back to
  `env -u GOROOT -u GOPATH golangci-lint run ./...`). Expected `0 issues`. Watch for: `modernize`
  (backward loops → `slices.Backward`), missing doc comments on exported identifiers, import
  ordering (`gci`). Fix any finding minimally and re-run before proceeding.
- [ ] **Step 4:** `grep -rn "narrative" internal/store/store.go | head` sanity — confirm no leftover
  Path-A `MaxSummaryTokens` references anywhere (`grep -rn "MaxSummaryTokens" .` should be empty —
  that field was from the withdrawn draft and must not exist in code).
- [ ] **Step 5:** commit if Step 3 required fixes:

```bash
git add -A
git commit -m "Fix lint findings in correlator/store persist slice"
```

---

## Task 10: update the phase-1 spec's slice-3 status + Path-B reconciliation

**Files:**
- Modify: `docs/superpowers/specs/2026-08-11-phase1-correlator-design.md`

- [ ] **Step 1:** Find the "Implementation slices" item 3 (`Persistence + narrate groundwork`) and
  replace it with a ✅-landed version that also records the Path-B reconciliation — that slice 2's
  hydrated-context rework landed alongside it, and that tail-summarization is retained (not the
  withdrawn `MaxSummaryTokens` guard). Reference both design docs
  (`2026-08-12-correlator-hydrated-context-rework.md`,
  `2026-08-12-correlator-persist-design.md`) and this plan.

  Replace:
  ```
  3. **Persistence + `narrate` groundwork** — `Persist`, the extend-vs-new logic against real
     `narratives`/`narrative_events` rows, tail-summarization overflow handling (`config.Correlator.
     {TailSummarizeThresholdTokens, RecentEventsKept}`), the lease lock (needed even for a
     single-command first cut, since crash-recovery correctness shouldn't be deferred).
  ```
  with:
  ```
  3. **Persistence + narrative groundwork** — ✅ landed. `Persist` (extend-vs-new against real
     `narratives`/`narrative_events` rows), tail-summarization compaction (`config.Correlator.
     {TailSummarizeThresholdTokens, RecentEventsKept}`, via a `compaction_boundary` column;
     `narrative_events` rows never deleted), and the `pipeline_lock` lease (built + tested, no
     caller until slice 5). Landed together with a rework of slice 2's `Cluster` so it clusters
     against the raw events of overlapping narratives (not just their summaries) — restoring this
     spec's original "full available context" intent, which slice 2 had silently narrowed to
     summaries. See `docs/superpowers/specs/2026-08-12-correlator-hydrated-context-rework.md`,
     `docs/superpowers/specs/2026-08-12-correlator-persist-design.md`, and
     `docs/superpowers/plans/2026-08-12-correlator-persist-and-rework-implementation.md`.
  ```

- [ ] **Step 2:** commit.

```bash
git add docs/superpowers/specs/2026-08-11-phase1-correlator-design.md
git commit -m "Mark phase-1 slice 3 landed; record Path-B context rework"
```

---

## Verification checklist (spec coverage)

- [x] `Cluster` clusters against hydrated raw narrative events, context-only (no integer handles) —
  Tasks 1-2.
- [x] `Persist` new/extend against real tables, all-or-nothing via transaction — Tasks 6, 8.
- [x] Hallucinated/stale `narrative_id` on extend → loud error (deferred slice-2 item) — Task 8.
- [x] Event-id resolution by (source, external_id); missing → loud error — Tasks 5, 8.
- [x] Tail-summarization compaction with `compaction_boundary`; events never deleted; LLM call
  before the transaction — Tasks 4, 5, 8.
- [x] `NarrativeEventsForContext` hydration accessor (post-boundary only) — Task 5.
- [x] `pipeline_lock` lease: TryAcquire/Acquire/Release, steal-on-expiry, ctx cancel — Task 7.
- [x] `CorrelatorConfig` required/validated + example — Task 3.
- [x] Idempotent `compaction_boundary` migration for existing DBs — Task 4.
- [x] B↔A reversibility preserved: links always written, summary always current — Tasks 5, 8.

Not in scope (later slices): reconciler, actions, issue-matching, any CLI command (the pipeline
wiring that calls `Cluster`+`Persist` and acquires the lease is slice 5's `watch`).
