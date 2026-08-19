# `unjira dev narrate` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship `unjira dev narrate`, which runs one real collect → `Cluster` → `Persist` pass under the
pipeline lease and prints the narratives in enough detail to judge the clustering — the first
execution of slice 3's machinery against real history and a real LLM.

**Architecture:** Five layers, bottom-up. (1) A new `internal/llm` contract package owning the
`Client` interface and normalized `Usage`, mirroring `internal/tasktracker`. (2) `config.Span`, a
day/week-aware duration. (3) Two `internal/store` accessors that assemble `Cluster`'s inputs — a gap
slice 3 left open. (4) Token-usage observability threaded from the client through
`correlator.Stats`. (5) `pipeline.RunNarrate` plus the `dev narrate` CLI and its renderer.

**Tech Stack:** Go 1.26, `github.com/alecthomas/kong` (CLI), `github.com/xhit/go-str2duration/v2`
(new direct dep), `github.com/stretchr/testify`, `modernc.org/sqlite`.

**Spec:** `docs/superpowers/specs/2026-08-19-dev-narrate-design.md` — authoritative where this plan
and it disagree. **This plan's code is a draft you are responsible for**; the slice-3 plan shipped two
real bugs by being transcribed verbatim (an `err == sql.ErrNoRows` that fails on wrapped errors, and a
`time.RFC3339` format that silently truncated a sub-second lease TTL). If something here is wrong,
fix it and say so.

---

## Critical environment notes (every task)

**Stale `GOROOT`/`GOPATH`.** Every `go`/`gofmt` invocation MUST be prefixed with
`env -u GOROOT -u GOPATH`, e.g. `env -u GOROOT -u GOPATH go test ./...`. Without it you get spurious
`compile: version "X" does not match go tool version "Y"` errors unrelated to your change.

**`golangci-lint` works, via an unusual path:**

```bash
export PATH="$HOME/.asdf/installs/golang/1.25.7/bin:$PATH"
env -u GOROOT -u GOPATH golangci-lint run ./...
```

The binary lives under `1.25.7/bin` (a stale-`GOBIN` artifact) but is genuinely
`golangci-lint 2.12.2 built with go1.26.0`. **The branch lints `0 issues` today and must stay
there.** Ignore the `gomodguard is deprecated` warnings — pre-existing config noise.

**Work in the worktree** `/Users/jonathan.ogilvie/workspace/unjira/.claude/worktrees/dev-narrate`
(branch `worktree-dev-narrate`). Run all commands from there. Do not create or switch branches. Do
not touch the session task list — the coordinator owns it.

**Repo conventions** (`CLAUDE.md`, `docs/go-conventions.md`): TDD — failing test first, confirm it
fails for the right reason, then implement. Never silently drop data; error loudly with
`fmt.Errorf("...: %w", err)` naming the relevant id. testify `require` (fatal) / `assert`
(non-fatal), `t.Context()`, `t.TempDir()`. Doc comments on every exported identifier.
`internal/store` must never import `internal/correlator`.

**Proving a regression test works.** For any test guarding against a specific defect, temporarily
break the guarded behavior, confirm the test fails, restore, confirm it passes. Paste both outputs.
Three tests in slice 3 looked like they covered a failure mode but did not; this drill caught all
three.

---

## File structure

- **Create:** `internal/llm/llm.go` — `Client` interface + `Usage`. Types only, no logic, no tests
  (mirrors `internal/tasktracker`).
- **Create:** `internal/config/span.go` — `Span` type, `UnmarshalText`, `Duration()`.
- **Create:** `internal/config/span_test.go` — table-driven parse/reject tests.
- **Create:** `internal/pipeline/narrate.go` — `RunNarrate`, `NarrateOptions`, `NarrateResult`,
  `NarratedNarrative`, `Compaction`.
- **Create:** `internal/pipeline/narrate_test.go` — `RunNarrate` against a real store + fake client.
- **Create:** `internal/pipeline/narrate_render.go` — `RenderNarrateResult`, a pure function.
- **Create:** `internal/pipeline/narrate_render_test.go` — table-driven rendering tests.
- **Modify:** `internal/clients/openai/openai.go` — `Complete` returns `llm.Usage`.
- **Modify:** `internal/clients/openai/openai_test.go` — assert usage is populated.
- **Modify:** `internal/store/store.go` — `UnlinkedEventsInRange`, `NarrativesOverlapping`.
- **Modify:** `internal/store/store_test.go` — accessor tests.
- **Modify:** `internal/correlator/correlator.go` — delete `llmClient`, take `llm.Client`, add
  `Stats`, return it from `Cluster`/`Persist`.
- **Modify:** `internal/correlator/correlator_test.go` — `fakeLLM` returns usage; 27 call sites updated.
- **Modify:** `cmd/unjira/main.go` — `devNarrateCmd`, `UNJIRA_LLM_API_KEY`, registry in `devCmd`.
- **Modify:** `go.mod`/`go.sum` — add `github.com/xhit/go-str2duration/v2`.

Rationale for the split: `narrate.go` (orchestration) and `narrate_render.go` (presentation) are
separate files because the renderer is a pure function that must be table-testable without a store or
a client — mixing them would force every rendering test to build a database.

Sequencing: `internal/llm` (Task 1) is a dependency of the observability refactor (Tasks 4-5), which
`RunNarrate` (Task 7) consumes. `config.Span` (Task 2) and the store accessors (Task 3) are
independent and could be done in any order, but are placed first so the later tasks have everything
they need.

---

## Task 1: `internal/llm` contract package

**Files:**
- Create: `internal/llm/llm.go`

No tests: this is an interface and a data struct with no behavior, exactly like
`internal/tasktracker`.

- [ ] **Step 1: Create the package**

Create `internal/llm/llm.go`:

```go
// Package llm defines the backend-agnostic interface the correlator uses to
// reach a language model, plus the normalized usage every backend reports.
// It has no imports of internal/clients — like internal/tasktracker and
// internal/events, it's a shared contract with multiple producers
// (internal/clients/openai today, an Anthropic-shaped client later) and no
// single owning consumer.
//
// The contract lives here rather than in the consumer or in a specific
// client for a concrete reason: a second backend must be addable without
// importing a competing provider's package to report its own token counts.
package llm

import "context"

// Client is the narrow capability the correlator needs from any LLM backend:
// one non-streaming, single-turn completion. Deliberately minimal — anything
// a backend can't express this way doesn't belong behind this seam.
type Client interface {
	Complete(ctx context.Context, systemPrompt, userPrompt string) (string, Usage, error)
}

// Usage reports what one completion actually consumed, as the server counted
// it. This is distinct from correlator's own estimateTokens heuristic, which
// only has to be good enough to decide whether to split a window before
// spending a call; comparing the two is how that heuristic gets validated.
//
// A backend that reports no usage returns the zero value rather than an
// error — missing telemetry must never fail a completion that succeeded.
type Usage struct {
	PromptTokens     int64
	CompletionTokens int64
	// Model is what the server reported serving, which can differ from the
	// model requested when a gateway (litellm, OpenRouter) remaps it.
	Model string
}
```

- [ ] **Step 2: Verify it compiles and lints**

```bash
env -u GOROOT -u GOPATH gofmt -l internal/llm/
env -u GOROOT -u GOPATH go build ./internal/llm/
env -u GOROOT -u GOPATH go vet ./internal/llm/...
export PATH="$HOME/.asdf/installs/golang/1.25.7/bin:$PATH"
env -u GOROOT -u GOPATH golangci-lint run ./internal/llm/...
```

Expected: no output from `gofmt -l`, build and vet clean, lint `0 issues`.

- [ ] **Step 3: Commit**

```bash
git add internal/llm/llm.go
git commit -m "Add internal/llm: backend-agnostic LLM contract and normalized usage"
```

---

## Task 2: `config.Span` — day/week-aware durations

**Files:**
- Modify: `go.mod`, `go.sum`
- Create: `internal/config/span.go`
- Create: `internal/config/span_test.go`

- [ ] **Step 1: Add the dependency**

```bash
env -u GOROOT -u GOPATH go get github.com/xhit/go-str2duration/v2@v2.1.0
```

Expected: `go.mod` gains `github.com/xhit/go-str2duration/v2 v2.1.0` in the direct-require block.

- [ ] **Step 2: Write the failing test**

Create `internal/config/span_test.go`:

```go
package config_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/config"
)

func TestSpan_UnmarshalText(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    time.Duration
		wantErr string
	}{
		{name: "days", input: "7d", want: 168 * time.Hour},
		{name: "weeks", input: "2w", want: 336 * time.Hour},
		{name: "compound day and hour", input: "7d12h", want: 180 * time.Hour},
		{name: "compound week day hour minute", input: "1w2d3h4m", want: 219*time.Hour + 4*time.Minute},
		{name: "stdlib hours still work", input: "36h", want: 36 * time.Hour},
		{name: "stdlib minutes still work", input: "90m", want: 90 * time.Minute},
		{name: "years unsupported", input: "1y", wantErr: "1y"},
		{name: "uppercase unit rejected", input: "7D", wantErr: "7D"},
		{name: "empty rejected", input: "", wantErr: "empty"},
		{name: "negative rejected", input: "-3d", wantErr: "must be positive"},
		{name: "zero rejected", input: "0s", wantErr: "must be positive"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var s config.Span
			err := s.UnmarshalText([]byte(tt.input))

			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, s.Duration())
		})
	}
}

func TestSpan_UnmarshalsFromJSON(t *testing.T) {
	// TextUnmarshaler is honored by encoding/json too, so a Span works as a
	// config field, not just a CLI flag.
	var payload struct {
		Interval config.Span `json:"interval"`
	}

	require.NoError(t, json.Unmarshal([]byte(`{"interval":"7d"}`), &payload))
	assert.Equal(t, 168*time.Hour, payload.Interval.Duration())
}
```

- [ ] **Step 3: Run it to confirm it fails**

Run: `env -u GOROOT -u GOPATH go test ./internal/config/ -run TestSpan -v`

Expected: FAIL — `undefined: config.Span`.

- [ ] **Step 4: Implement `Span`**

Create `internal/config/span.go`:

```go
package config

import (
	"fmt"
	"strings"
	"time"

	str2duration "github.com/xhit/go-str2duration/v2"
)

// Span is a duration that also accepts day ("7d") and week ("2w") units,
// including compound forms ("7d12h"), which time.ParseDuration rejects — it
// stops at "h", deliberately, since a calendar day is not reliably 24h under
// DST. unjira measures spans backward from now in UTC, where a day is exactly
// 24h, so that ambiguity cannot arise here.
//
// Parsing is delegated to github.com/xhit/go-str2duration rather than
// hand-rolled: it handles compound forms a strip-the-suffix approach cannot.
// This type exists to satisfy encoding.TextUnmarshaler (honored by both Kong
// and encoding/json) and to reject non-positive spans.
type Span time.Duration

// UnmarshalText parses a duration string, accepting day and week units on top
// of everything time.ParseDuration handles. Non-positive spans are rejected:
// unjira's windows run backward from now, so a negative span would put the
// window's start after its end and silently produce an empty pass.
func (s *Span) UnmarshalText(text []byte) error {
	raw := strings.TrimSpace(string(text))
	if raw == "" {
		return fmt.Errorf("parsing duration: empty value")
	}

	d, err := str2duration.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("parsing duration %q: %w", raw, err)
	}

	if d <= 0 {
		return fmt.Errorf("parsing duration %q: must be positive", raw)
	}

	*s = Span(d)

	return nil
}

// Duration returns the span as a time.Duration.
func (s Span) Duration() time.Duration {
	return time.Duration(s)
}

// String renders the span using time.Duration's formatting, so help text and
// error messages show a canonical form (168h0m0s) rather than the input.
func (s Span) String() string {
	return time.Duration(s).String()
}
```

- [ ] **Step 5: Run tests + tidy**

```bash
env -u GOROOT -u GOPATH go test ./internal/config/ -run TestSpan -v
env -u GOROOT -u GOPATH go mod tidy
env -u GOROOT -u GOPATH go test ./internal/config/ -v
```

Expected: all `TestSpan*` subtests PASS; every pre-existing config test still passes.

- [ ] **Step 6: Prove the non-positive rejection discriminates**

This is the behavior `Span` adds beyond the library, so it needs the break-it drill. Temporarily
delete the `if d <= 0` block, re-run, confirm the `negative rejected` and `zero rejected` subtests
FAIL, then restore and confirm they pass. Paste both outputs in your report.

- [ ] **Step 7: Commit**

```bash
env -u GOROOT -u GOPATH gofmt -w internal/config/span.go internal/config/span_test.go
env -u GOROOT -u GOPATH go vet ./internal/config/...
git add go.mod go.sum internal/config/span.go internal/config/span_test.go
git commit -m "Add config.Span: durations accepting day and week units"
```

---

## Task 3: store accessors that assemble `Cluster`'s inputs

**Files:**
- Modify: `internal/store/store.go`
- Modify: `internal/store/store_test.go`

This closes the gap slice 3 left: nothing could select "events not yet linked to a narrative" or
"narratives overlapping a window." Both accessors are permanent — `watch` needs the same inputs.

- [ ] **Step 1: Write the failing tests**

Append to `internal/store/store_test.go`:

```go
// -- Cluster input assembly ----------------------------------------------

func TestUnlinkedEventsInRange_ExcludesLinkedEvents(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	seedEvent(t, s, "linked", "already narrated", base)
	seedEvent(t, s, "loose", "not yet narrated", base.Add(time.Minute))

	linkedID, err := s.EventIDByExternalID("claude_code", "linked")
	require.NoError(t, err)
	nid, err := s.InsertNarrative(base, base.Add(time.Hour), "T", "s")
	require.NoError(t, err)
	require.NoError(t, s.AddNarrativeEvents(nid, []int64{linkedID}))

	got, err := s.UnlinkedEventsInRange(base, base.Add(time.Hour))

	require.NoError(t, err)
	require.Len(t, got, 1, "the linked event must not be a clustering candidate")
	assert.Equal(t, "loose", got[0].ExternalID)
}

func TestUnlinkedEventsInRange_HalfOpenBoundary(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	seedEvent(t, s, "before", "just before start", base.Add(-time.Second))
	seedEvent(t, s, "at-start", "exactly at start", base)
	seedEvent(t, s, "at-end", "exactly at end", base.Add(time.Hour))

	got, err := s.UnlinkedEventsInRange(base, base.Add(time.Hour))

	require.NoError(t, err)
	require.Len(t, got, 1, "[start, end): start included, end excluded")
	assert.Equal(t, "at-start", got[0].ExternalID)
}

func TestUnlinkedEventsInRange_EmptyRangeReturnsNoError(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	got, err := s.UnlinkedEventsInRange(base, base.Add(time.Hour))

	require.NoError(t, err)
	assert.Empty(t, got, "no candidates is a normal outcome, not a failure")
}

func TestNarrativesOverlapping_IncludesOverlapAndTouching(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	windowStart := base
	windowEnd := base.Add(time.Hour)

	// Ends exactly at window start — adjacent, must be included.
	touchingBefore, err := s.InsertNarrative(base.Add(-2*time.Hour), windowStart, "touching-before", "s")
	require.NoError(t, err)
	// Straddles the window start.
	overlapping, err := s.InsertNarrative(base.Add(-30*time.Minute), base.Add(30*time.Minute), "overlapping", "s")
	require.NoError(t, err)
	// Begins exactly at window end — adjacent, must be included.
	touchingAfter, err := s.InsertNarrative(windowEnd, windowEnd.Add(time.Hour), "touching-after", "s")
	require.NoError(t, err)
	// Strictly disjoint on both sides — must be excluded.
	_, err = s.InsertNarrative(base.Add(-48*time.Hour), base.Add(-24*time.Hour), "ancient", "s")
	require.NoError(t, err)
	_, err = s.InsertNarrative(base.Add(24*time.Hour), base.Add(48*time.Hour), "future", "s")
	require.NoError(t, err)

	got, err := s.NarrativesOverlapping(windowStart, windowEnd)

	require.NoError(t, err)
	gotIDs := make([]int64, 0, len(got))
	for _, row := range got {
		gotIDs = append(gotIDs, row.ID)
	}
	assert.ElementsMatch(t, []int64{touchingBefore, overlapping, touchingAfter}, gotIDs,
		"touching endpoints count as adjacent; strictly disjoint windows do not")
}

func TestNarrativesOverlapping_CarriesCompactionBoundary(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	seedEvent(t, s, "e1", "an event", base)
	eventID, err := s.EventIDByExternalID("claude_code", "e1")
	require.NoError(t, err)
	nid, err := s.InsertNarrative(base, base.Add(time.Hour), "T", "s")
	require.NoError(t, err)
	require.NoError(t, s.SetCompactionBoundary(nid, base, eventID, "recap"))

	got, err := s.NarrativesOverlapping(base, base.Add(time.Hour))

	require.NoError(t, err)
	require.Len(t, got, 1)
	require.NotNil(t, got[0].CompactionBoundary, "boundary must round-trip for hydration to filter on")
	assert.True(t, base.Equal(*got[0].CompactionBoundary))
	require.NotNil(t, got[0].CompactionBoundaryEventID)
	assert.Equal(t, eventID, *got[0].CompactionBoundaryEventID)
}
```

Note: `seedEvent` and `openStore` already exist in this file — reuse them, do not redefine.

- [ ] **Step 2: Run to confirm it fails**

Run:
`env -u GOROOT -u GOPATH go test ./internal/store/ -run 'TestUnlinkedEventsInRange|TestNarrativesOverlapping' -v`

Expected: FAIL — `s.UnlinkedEventsInRange undefined`, `s.NarrativesOverlapping undefined`.

- [ ] **Step 3: Implement the accessors**

Add to `internal/store/store.go`, in the narratives section (after
`NarrativeEventsForContext`):

```go
// UnlinkedEventsInRange returns events in [start, end) that are not yet
// linked to any narrative, ordered by (occurred_at, id) — the clustering
// candidates a narration pass considers.
//
// "Unlinked" means no narrative_events row at all, not "linked to a narrative
// outside this range": an event belongs to exactly one narrative, so once
// linked it is never a candidate again. Such an event can still reach a
// prompt as context via its narrative's hydration
// (NarrativeEventsForContext), which is why excluding it here does not starve
// the model.
//
// Ordering is composite because occurred_at is stored via time.RFC3339
// (whole seconds — see InsertEvent) and cannot uniquely order events.
func (s *Store) UnlinkedEventsInRange(start, end time.Time) ([]events.Event, error) {
	rows, err := s.db.Query(
		`SELECT e.source, e.external_id, e.occurred_at, e.actor, e.summary, e.artifacts, e.raw_ref
		 FROM events e
		 WHERE e.occurred_at >= ? AND e.occurred_at < ?
		   AND NOT EXISTS (SELECT 1 FROM narrative_events ne WHERE ne.event_id = e.id)
		 ORDER BY e.occurred_at, e.id`,
		start.Format(time.RFC3339), end.Format(time.RFC3339),
	)
	if err != nil {
		return nil, fmt.Errorf("querying unlinked events in [%s, %s): %w",
			start.Format(time.RFC3339), end.Format(time.RFC3339), err)
	}
	defer func() { _ = rows.Close() }()

	var out []events.Event
	for rows.Next() {
		event, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning unlinked event row: %w", err)
		}
		out = append(out, event)
	}

	return out, rows.Err()
}

// NarrativesOverlapping returns narratives whose window overlaps or merely
// touches [start, end), ordered by (window_start, id) — the context a
// narration pass passes to Cluster.
//
// The predicate mirrors correlator's own adjacency filter: keep unless
// strictly disjoint (window_end < start || window_start > end). Touching
// endpoints count as adjacent deliberately — temporal proximity is real
// clustering signal, so a narrative ending exactly when this window opens is
// the most likely thing an early event extends.
//
// status is not filtered: every narrative is 'open' today and nothing sets
// otherwise, so filtering would be speculative. When status becomes
// meaningful, the predicate belongs here.
func (s *Store) NarrativesOverlapping(start, end time.Time) ([]NarrativeRow, error) {
	rows, err := s.db.Query(
		`SELECT id, window_start, window_end, title, summary, issue_key, confidence, status,
		        compaction_boundary, compaction_boundary_event_id
		 FROM narratives
		 WHERE window_end >= ? AND window_start <= ?
		 ORDER BY window_start, id`,
		start.Format(time.RFC3339), end.Format(time.RFC3339),
	)
	if err != nil {
		return nil, fmt.Errorf("querying narratives overlapping [%s, %s): %w",
			start.Format(time.RFC3339), end.Format(time.RFC3339), err)
	}
	defer func() { _ = rows.Close() }()

	var out []NarrativeRow
	for rows.Next() {
		row, err := scanNarrativeRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning overlapping narrative row: %w", err)
		}
		out = append(out, row)
	}

	return out, rows.Err()
}
```

`scanNarrativeRow` does not exist yet. `getNarrativeImpl` currently inlines this scan-and-parse
logic against a `*sql.Row`. **Extract it** into a helper both can use, taking the same `scanRow`
interface `scanEvent` already uses (so it accepts both `*sql.Row` and `*sql.Rows`):

```go
// scanNarrativeRow scans one narratives row and parses its RFC3339 timestamps.
// Shared by GetNarrative (single row) and NarrativesOverlapping (many), so the
// nullable-column handling exists once.
func scanNarrativeRow(row scanRow) (NarrativeRow, error) {
	var (
		out                NarrativeRow
		windowStart        string
		windowEnd          string
		issueKey           sql.NullString
		confidence         sql.NullFloat64
		compactionBoundary sql.NullString
		boundaryEventID    sql.NullInt64
	)

	if err := row.Scan(&out.ID, &windowStart, &windowEnd, &out.Title, &out.Summary,
		&issueKey, &confidence, &out.Status, &compactionBoundary, &boundaryEventID); err != nil {
		return NarrativeRow{}, err
	}

	var err error
	if out.WindowStart, err = time.Parse(time.RFC3339, windowStart); err != nil {
		return NarrativeRow{}, fmt.Errorf("parsing window_start for narrative %d: %w", out.ID, err)
	}
	if out.WindowEnd, err = time.Parse(time.RFC3339, windowEnd); err != nil {
		return NarrativeRow{}, fmt.Errorf("parsing window_end for narrative %d: %w", out.ID, err)
	}

	out.IssueKey = issueKey.String
	if confidence.Valid {
		out.Confidence = confidence.Float64
	}
	if compactionBoundary.Valid {
		parsed, perr := time.Parse(time.RFC3339, compactionBoundary.String)
		if perr != nil {
			return NarrativeRow{}, fmt.Errorf("parsing compaction_boundary for narrative %d: %w", out.ID, perr)
		}
		out.CompactionBoundary = &parsed
	}
	if boundaryEventID.Valid {
		id := boundaryEventID.Int64
		out.CompactionBoundaryEventID = &id
	}

	return out, nil
}
```

Then rewrite `getNarrativeImpl`'s body to use it, preserving its `ErrNarrativeNotFound` behavior:

```go
func getNarrativeImpl(c dbConn, id int64) (NarrativeRow, error) {
	row, err := scanNarrativeRow(c.QueryRow(
		`SELECT id, window_start, window_end, title, summary, issue_key, confidence, status,
		        compaction_boundary, compaction_boundary_event_id
		 FROM narratives WHERE id = ?`, id,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return NarrativeRow{}, ErrNarrativeNotFound
	}
	if err != nil {
		return NarrativeRow{}, fmt.Errorf("querying narrative %d: %w", id, err)
	}

	return row, nil
}
```

Check the exact current body of `getNarrativeImpl` before replacing it — match its column order and
preserve every behavior its existing tests assert. If `scanRow` is unexported and shaped differently
than assumed, adapt rather than forcing it.

- [ ] **Step 4: Run the full store package**

Run: `env -u GOROOT -u GOPATH go test ./internal/store/ -v`

Expected: the 5 new tests PASS **and** every pre-existing store test still passes — the
`getNarrativeImpl` refactor is behavior-preserving, and `TestNarrative_InsertGetRoundTrip` /
`TestGetNarrative_MissingReturnsErrNarrativeNotFound` are what prove it.

- [ ] **Step 5: Commit**

```bash
env -u GOROOT -u GOPATH gofmt -w internal/store/store.go internal/store/store_test.go
env -u GOROOT -u GOPATH go vet ./internal/store/...
export PATH="$HOME/.asdf/installs/golang/1.25.7/bin:$PATH"
env -u GOROOT -u GOPATH golangci-lint run ./internal/store/...
git add internal/store/store.go internal/store/store_test.go
git commit -m "Add UnlinkedEventsInRange and NarrativesOverlapping"
```

Expected: lint `0 issues`.

---

## Task 4: `openai.Client.Complete` returns `llm.Usage`

**Files:**
- Modify: `internal/clients/openai/openai.go`
- Modify: `internal/clients/openai/openai_test.go`

The SDK already returns `resp.Usage` and `resp.Model`; today both are discarded.

- [ ] **Step 1: Write the failing test**

Add to `internal/clients/openai/openai_test.go`:

```go
func TestComplete_ReturnsUsageFromResponse(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{
			"id":      "chatcmpl-1",
			"object":  "chat.completion",
			"model":   "gpt-5-2-served",
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": "4"},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{
				"prompt_tokens":     31,
				"completion_tokens": 7,
				"total_tokens":      38,
			},
		})
	})

	text, usage, err := client.Complete(t.Context(), "You are helpful.", "What is 2+2?")

	require.NoError(t, err)
	assert.Equal(t, "4", text)
	assert.Equal(t, int64(31), usage.PromptTokens)
	assert.Equal(t, int64(7), usage.CompletionTokens)
	assert.Equal(t, "gpt-5-2-served", usage.Model,
		"the served model can differ from the requested one behind a gateway")
}

func TestComplete_MissingUsageIsZeroNotAnError(t *testing.T) {
	// A backend that omits usage must not fail a completion that succeeded.
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{
			"id":     "chatcmpl-2",
			"object": "chat.completion",
			"model":  "gpt-5-2",
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": "ok"},
				"finish_reason": "stop",
			}},
		})
	})

	text, usage, err := client.Complete(t.Context(), "sys", "user")

	require.NoError(t, err)
	assert.Equal(t, "ok", text)
	assert.Zero(t, usage.PromptTokens)
	assert.Zero(t, usage.CompletionTokens)
}
```

Also update the three existing `Complete` call sites in this file (lines ~68, ~87, ~118) for the new
three-value return. The success case at ~68 becomes `result, _, err := client.Complete(...)`; the two
error cases become `_, _, err := client.Complete(...)`.

- [ ] **Step 2: Run to confirm it fails**

Run: `env -u GOROOT -u GOPATH go test ./internal/clients/openai/ -v`

Expected: FAIL to compile — `assignment mismatch: 3 variables but client.Complete returns 2 values`.

- [ ] **Step 3: Change `Complete`**

In `internal/clients/openai/openai.go`, add `"github.com/jcogilvie/unjira/internal/llm"` to the
imports and replace `Complete`:

```go
// Complete sends one non-streaming, single-turn chat completion request and
// returns the assistant's reply text plus what the call consumed.
//
// Usage comes from the server's own accounting, which is why it is returned
// rather than estimated: it is the ground truth the correlator's own
// token-estimate heuristic gets validated against. A response omitting usage
// yields a zero Usage, not an error — missing telemetry must never fail a
// completion that otherwise succeeded.
func (c *Client) Complete(ctx context.Context, systemPrompt, userPrompt string) (string, llm.Usage, error) {
	resp, err := c.upstream.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model: c.model,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(systemPrompt),
			openai.UserMessage(userPrompt),
		},
	})
	if err != nil {
		return "", llm.Usage{}, fmt.Errorf("completing chat prompt: %w", err)
	}

	if len(resp.Choices) == 0 {
		return "", llm.Usage{}, fmt.Errorf("completing chat prompt: response had no choices")
	}

	usage := llm.Usage{
		PromptTokens:     resp.Usage.PromptTokens,
		CompletionTokens: resp.Usage.CompletionTokens,
		Model:            resp.Model,
	}

	return resp.Choices[0].Message.Content, usage, nil
}
```

Add a compile-time assertion that the facade still satisfies the contract, near the type declaration:

```go
// Compile-time proof the facade satisfies the contract the correlator uses.
var _ llm.Client = (*Client)(nil)
```

- [ ] **Step 4: Run to confirm it passes**

Run: `env -u GOROOT -u GOPATH go test ./internal/clients/openai/ -v`

Expected: all PASS, including the two new tests.

- [ ] **Step 5: Commit**

```bash
env -u GOROOT -u GOPATH gofmt -w internal/clients/openai/openai.go internal/clients/openai/openai_test.go
env -u GOROOT -u GOPATH go vet ./internal/clients/openai/...
git add internal/clients/openai/
git commit -m "Return llm.Usage from Complete instead of discarding it"
```

Note: `env -u GOROOT -u GOPATH go build ./...` will now FAIL in `internal/correlator`, whose
`llmClient` interface no longer matches. Task 5 fixes that. Do not patch the correlator here.

---

## Task 5: `correlator` takes `llm.Client` and returns `Stats`

**Files:**
- Modify: `internal/correlator/correlator.go`
- Modify: `internal/correlator/correlator_test.go`

The largest task: it deletes the consumer-owned interface, adds `Stats`, and updates 27 call sites.
Mechanical once the shape is right — Serena or `gofmt`-aware search/replace handles the bulk.

- [ ] **Step 1: Update `fakeLLM` and add the failing test**

In `internal/correlator/correlator_test.go`, replace `fakeLLM` so it returns usage, and add
`"github.com/jcogilvie/unjira/internal/llm"` to the imports:

```go
// fakeLLM satisfies llm.Client without making any real call. responses is
// consumed in call order; a test that only cares about one canned response can
// set a single-element slice. usagePerCall is returned from every call, so a
// test can assert Stats aggregates across a bisected pass.
type fakeLLM struct {
	responses    []string
	prompts      []string // captured user prompts, in call order, for assertions
	err          error
	usagePerCall llm.Usage
}

func (f *fakeLLM) Complete(_ context.Context, _ string, userPrompt string) (string, llm.Usage, error) {
	f.prompts = append(f.prompts, userPrompt)
	if f.err != nil {
		return "", llm.Usage{}, f.err
	}

	idx := len(f.prompts) - 1
	if idx >= len(f.responses) {
		idx = len(f.responses) - 1
	}

	return f.responses[idx], f.usagePerCall, nil
}
```

Then add these tests:

```go
func TestCluster_StatsCountsCallsAndAggregatesUsage(t *testing.T) {
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	evts := []correlator.Event{
		mustEvent(t, "claude_code", "e1", "did a thing", base),
	}
	llmFake := &fakeLLM{
		responses:    []string{`[{"kind":"new","title":"T","summary":"s","event_indices":[0]}]`},
		usagePerCall: llm.Usage{PromptTokens: 100, CompletionTokens: 20, Model: "m"},
	}

	_, stats, err := correlator.Cluster(t.Context(), evts, nil, llmFake, correlator.TimeRange{
		Start: base, End: base.Add(time.Hour),
	}, 128000)

	require.NoError(t, err)
	assert.Equal(t, 1, stats.Calls)
	assert.Equal(t, int64(100), stats.PromptTokens)
	assert.Equal(t, int64(20), stats.CompletionTokens)
	assert.Zero(t, stats.Splits, "a window that fits needs no bisection")
	assert.Positive(t, stats.EstimatedTokens, "the len/4 estimate is recorded for comparison")
}

func TestCluster_StatsAggregatesAcrossBisection(t *testing.T) {
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	// Two events far enough apart that bisection separates them, with
	// summaries large enough to overflow a tiny context budget.
	evts := []correlator.Event{
		mustEvent(t, "claude_code", "e1", strings.Repeat("x", 400), base),
		mustEvent(t, "claude_code", "e2", strings.Repeat("y", 400), base.Add(50*time.Minute)),
	}
	llmFake := &fakeLLM{
		// Both halves yield an adjacent ClusterNew result, so Cluster makes a
		// third, merge-boundary same-story call after the two split-half
		// calls; its response must parse as a sameStoryResponse, not a
		// cluster-response array.
		responses: []string{
			`[{"kind":"new","title":"T","summary":"s","event_indices":[0]}]`,
			`[{"kind":"new","title":"T","summary":"s","event_indices":[0]}]`,
			`{"same_story":false}`,
		},
		usagePerCall: llm.Usage{PromptTokens: 10, CompletionTokens: 2},
	}

	// 300 sits strictly between each half's own prompt estimate (~268 tokens,
	// dominated by the fixed system prompt) and the combined two-event
	// estimate (~382): tight enough that the top-level window must bisect,
	// loose enough that each half fits on its own and doesn't recurse again.
	// A smaller budget (the 200 an earlier draft of this plan used) makes each
	// half overflow too, so Cluster hits the irreducible-unit error and the
	// assertions below are never reached.
	_, stats, err := correlator.Cluster(t.Context(), evts, nil, llmFake, correlator.TimeRange{
		Start: base, End: base.Add(time.Hour),
	}, 300)

	require.NoError(t, err)
	assert.Positive(t, stats.Splits, "an over-budget window must record its bisection")
	assert.GreaterOrEqual(t, stats.Calls, 2, "each half costs a call")
	// Ground truth is llmFake.prompts, not stats.Calls. An assertion phrased
	// against stats.Calls itself is self-referential: dropping a recursive Add
	// loses one call AND its tokens, preserving the ratio, so
	//   assert.Equal(int64(10)*int64(stats.Calls), stats.PromptTokens)
	// passes at 2/20 exactly as it does at 3/30 — verified. Only an
	// independent count sees the bug.
	require.Len(t, llmFake.prompts, 3, "two split-half calls plus one merge-boundary same-story check")
	assert.Equal(t, 3, stats.Calls, "Stats.Calls must match every completion actually made")
	assert.Equal(t, int64(10*len(llmFake.prompts)), stats.PromptTokens,
		"usage must sum across the whole recursion, not just the top call")
}

func TestPersist_StatsRecordsCompaction(t *testing.T) {
	s := persistStore(t)
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	id, err := s.InsertNarrative(base, base.Add(5*time.Hour), "Long", strings.Repeat("x", 4000))
	require.NoError(t, err)
	linkIDs := make([]int64, 0, 5)
	for i := range 5 {
		extID := fmt.Sprintf("old-%d", i)
		seedPersistedEvent(t, s, extID, strings.Repeat("y", 500), base.Add(time.Duration(i)*time.Hour))
		eid, eerr := s.EventIDByExternalID("claude_code", extID)
		require.NoError(t, eerr)
		linkIDs = append(linkIDs, eid)
	}
	require.NoError(t, s.AddNarrativeEvents(id, linkIDs))

	newE := seedPersistedEvent(t, s, "new-1", "newest", base.Add(6*time.Hour))
	llmFake := &fakeLLM{
		responses:    []string{"recap: earlier work compacted"},
		usagePerCall: llm.Usage{PromptTokens: 500, CompletionTokens: 40},
	}
	cfg := config.CorrelatorConfig{TailSummarizeThresholdTokens: 500, RecentEventsKept: 2}

	_, stats, err := correlator.Persist(t.Context(), s, llmFake, []correlator.ClusterResult{{
		Kind: correlator.ClusterExtends, NarrativeID: id, Title: "Long", Summary: strings.Repeat("z", 4000),
		Events: []correlator.Event{newE},
	}}, cfg)

	require.NoError(t, err)
	assert.Equal(t, 1, stats.Compactions)
	assert.Equal(t, 1, stats.Calls)
	assert.Equal(t, int64(500), stats.PromptTokens)
}
```

- [ ] **Step 2: Run to confirm it fails**

Run: `env -u GOROOT -u GOPATH go test ./internal/correlator/ -v`

Expected: FAIL to compile — `Cluster` returns 2 values, not 3; `Stats` undefined.

- [ ] **Step 3: Add `Stats` and delete `llmClient`**

In `internal/correlator/correlator.go`: add `"github.com/jcogilvie/unjira/internal/llm"` to the
imports, **delete** the entire `llmClient` interface declaration and its doc comment, and add:

```go
// Stats is what one Cluster or Persist call spent, and why. Splits and
// MergeChecks are what separate an expensive-but-correct pass (a wide window
// that legitimately bisected) from a pathological one, which a bare call count
// cannot distinguish.
//
// EstimatedTokens records what estimateTokens guessed for the top-level
// prompt, so it can be compared against the PromptTokens the server actually
// counted — that heuristic decides when to bisect, and has never otherwise
// been checked against reality.
type Stats struct {
	Calls            int
	Splits           int // window bisections
	MergeChecks      int // same-story checks at split seams
	Compactions      int // Persist only
	PromptTokens     int64
	CompletionTokens int64
	EstimatedTokens  int
}

// Add folds other into s, so a recursive Cluster call's cost rolls up into its
// parent's and a top-level call reports the whole tree. EstimatedTokens is
// summed like everything else: each level estimated its own prompt, and the
// total is what the pass would have had to fit.
//
// Exported because internal/pipeline merges Cluster's and Persist's stats into
// one pass total.
func (s *Stats) Add(other Stats) {
	s.Calls += other.Calls
	s.Splits += other.Splits
	s.MergeChecks += other.MergeChecks
	s.Compactions += other.Compactions
	s.PromptTokens += other.PromptTokens
	s.CompletionTokens += other.CompletionTokens
	s.EstimatedTokens += other.EstimatedTokens
}

// addUsage folds one completion's server-reported usage into s and counts the
// call. Unexported: only this package makes completions, so nothing outside it
// has a Usage to fold.
func (s *Stats) addUsage(u llm.Usage) {
	s.Calls++
	s.PromptTokens += u.PromptTokens
	s.CompletionTokens += u.CompletionTokens
}
```

Then change every signature and threading. `Cluster`:

```go
func Cluster(
	ctx context.Context,
	evts []Event,
	existing []Narrative,
	client llm.Client,
	window TimeRange,
	contextWindowTokens int,
) ([]ClusterResult, Stats, error) {
	filtered := filterEventsInWindow(evts, window)
	relevant := filterAdjacentOrOverlapping(existing, window)

	systemPrompt, userPrompt := buildClusterPrompt(filtered, relevant)

	var stats Stats
	estimated := estimateTokens(systemPrompt + userPrompt)
	stats.EstimatedTokens = estimated
	if estimated > contextWindowTokens {
		return clusterWithSplit(ctx, evts, existing, client, window, contextWindowTokens, filtered, stats)
	}

	raw, usage, err := client.Complete(ctx, systemPrompt, userPrompt)
	if err != nil {
		return nil, stats, fmt.Errorf("clustering events in window [%s, %s): %w", window.Start, window.End, err)
	}
	stats.addUsage(usage)

	results, err := parseClusterResponse(raw, filtered)
	if err != nil {
		return nil, stats, err
	}

	return results, stats, nil
}
```

`clusterWithSplit` takes the parent's `stats` and rolls both halves into it, incrementing `Splits`:

```go
func clusterWithSplit(
	ctx context.Context,
	evts []Event,
	existing []Narrative,
	client llm.Client,
	window TimeRange,
	contextWindowTokens int,
	filtered []Event,
	stats Stats,
) ([]ClusterResult, Stats, error) {
	if len(filtered) <= 1 {
		return nil, stats, irreducibleUnitError(window, filtered)
	}

	mid := window.Start.Add(window.End.Sub(window.Start) / 2)
	firstHalf := TimeRange{Start: window.Start, End: mid}
	secondHalf := TimeRange{Start: mid, End: window.End}

	firstFiltered := filterEventsInWindow(filtered, firstHalf)
	secondFiltered := filterEventsInWindow(filtered, secondHalf)

	if len(firstFiltered) == len(filtered) || len(secondFiltered) == len(filtered) {
		return nil, stats, irreducibleUnitError(window, filtered)
	}

	stats.Splits++

	firstResults, firstStats, err := Cluster(ctx, evts, existing, client, firstHalf, contextWindowTokens)
	stats.Add(firstStats)
	if err != nil {
		return nil, stats, err
	}

	secondResults, secondStats, err := Cluster(ctx, evts, existing, client, secondHalf, contextWindowTokens)
	stats.Add(secondStats)
	if err != nil {
		return nil, stats, err
	}

	merged, mergeStats, err := mergeSplitResults(ctx, client, firstResults, secondResults)
	stats.Add(mergeStats)
	if err != nil {
		return nil, stats, err
	}

	return merged, stats, nil
}
```

`mergeSplitResults` and `checkSameStory` return `Stats` too. `checkSameStory` increments
`MergeChecks` and records its usage:

```go
func checkSameStory(ctx context.Context, client llm.Client, a, b ClusterResult) (bool, ClusterResult, Stats, error) {
	var stats Stats
	stats.MergeChecks++

	userPrompt := fmt.Sprintf(
		"Cluster A: title=%q summary=%q\nCluster B: title=%q summary=%q",
		a.Title, a.Summary, b.Title, b.Summary,
	)

	raw, usage, err := client.Complete(ctx, sameStorySystemPrompt, userPrompt)
	if err != nil {
		return false, ClusterResult{}, stats, fmt.Errorf("checking same-story merge for split boundary: %w", err)
	}
	stats.addUsage(usage)

	var resp sameStoryResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		return false, ClusterResult{}, stats, fmt.Errorf("parsing same-story response %q: %w", raw, err)
	}

	if !resp.SameStory {
		return false, ClusterResult{}, stats, nil
	}

	return true, ClusterResult{
		Kind:    ClusterNew,
		Title:   resp.Title,
		Summary: resp.Summary,
		Events:  append(append([]Event{}, a.Events...), b.Events...),
	}, stats, nil
}
```

Read `mergeSplitResults`'s current body before editing: thread a local `Stats` through it, `Add` each
`checkSameStory` result, and return it. Do not change its merge logic — the extends-merge path makes
no LLM call and must still make none (`TestCluster_ExtendsResultsSharingNarrativeIDMergeAcrossSplit`
asserts this).

`Persist` returns `Stats`, and `compactNarrativeTail` reports its own:

```go
func Persist(
	ctx context.Context,
	s *store.Store,
	client llm.Client,
	results []ClusterResult,
	cfg config.CorrelatorConfig,
) ([]Narrative, Stats, error)
```

`compactNarrativeTail` gains a `Stats` return; on a real compaction it sets `Compactions = 1` and
calls `addUsage`. When it returns a zero boundary (nothing to compact) it makes no call, so its
`Stats` must be zero-valued — including `Compactions`. `prepareExtend`/`prepareResults` thread it up.

- [ ] **Step 4: Update the 27 existing call sites**

`Cluster` has 14 test call sites and `Persist` 13. Each gains a middle return. Where the test does
not assert on stats, discard it: `results, _, err := correlator.Cluster(...)`. Do not delete or
weaken any existing assertion — every one of the 25 pre-existing tests must still pass unchanged in
intent.

- [ ] **Step 5: Run the package**

Run: `env -u GOROOT -u GOPATH go test ./internal/correlator/ -v`

Expected: all 28 tests PASS (25 pre-existing + 3 new).

- [ ] **Step 6: Prove the aggregation test discriminates**

Temporarily change `stats.Add(firstStats)` in `clusterWithSplit` to a no-op, confirm
`TestCluster_StatsAggregatesAcrossBisection` FAILS on the `PromptTokens` assertion, restore, confirm
it passes. Paste both outputs. Without this, a stats-threading regression would be invisible.

- [ ] **Step 7: Commit**

```bash
env -u GOROOT -u GOPATH gofmt -w internal/correlator/
env -u GOROOT -u GOPATH go vet ./internal/correlator/...
export PATH="$HOME/.asdf/installs/golang/1.25.7/bin:$PATH"
env -u GOROOT -u GOPATH golangci-lint run ./internal/correlator/...
git add internal/correlator/
git commit -m "Take llm.Client and return Stats from Cluster and Persist"
```

Expected: lint `0 issues`. `env -u GOROOT -u GOPATH go build ./...` now succeeds again.

---

## Task 6: `pipeline.RunNarrate`

**Files:**
- Create: `internal/pipeline/narrate.go`
- Create: `internal/pipeline/narrate_test.go`

The orchestration `watch` will reuse. **The lease is not acquired here** — `dev narrate` wraps one
stage in a lease, `watch` will wrap three in a single one. Acquiring internally would make `watch`
deadlock against itself.

- [ ] **Step 1: Write the failing tests**

Create `internal/pipeline/narrate_test.go`:

```go
// Package pipeline_test exercises RunNarrate against a real temp-file store
// and a fake llm.Client — the orchestration (input assembly, hydration,
// dry-run gating, stats merging) is what's under test, not Cluster's own
// clustering logic, which internal/correlator covers.
package pipeline_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/llm"
	"github.com/jcogilvie/unjira/internal/pipeline"
	"github.com/jcogilvie/unjira/internal/store"
)

// narrateLLM is a fake llm.Client returning canned responses in call order.
type narrateLLM struct {
	responses    []string
	prompts      []string
	usagePerCall llm.Usage
}

func (f *narrateLLM) Complete(_ context.Context, _ string, userPrompt string) (string, llm.Usage, error) {
	f.prompts = append(f.prompts, userPrompt)
	idx := len(f.prompts) - 1
	if idx >= len(f.responses) {
		idx = len(f.responses) - 1
	}

	return f.responses[idx], f.usagePerCall, nil
}

func narrateStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	return s
}

func seedNarrateEvent(t *testing.T, s *store.Store, extID, summary string, at time.Time) {
	t.Helper()
	_, err := s.InsertEvent(events.NewEvent("claude_code", extID, at, summary))
	require.NoError(t, err)
}

func narrateConfig() config.Config {
	return config.Config{
		LLM:        config.LLMConfig{Model: "test-model", ContextWindowTokens: 128000},
		Correlator: config.CorrelatorConfig{TailSummarizeThresholdTokens: 1_000_000, RecentEventsKept: 20},
	}
}

func TestRunNarrate_PersistsNewNarrative(t *testing.T) {
	s := narrateStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	seedNarrateEvent(t, s, "e1", "started the work", base)
	seedNarrateEvent(t, s, "e2", "finished the work", base.Add(time.Minute))

	client := &narrateLLM{
		responses:    []string{`[{"kind":"new","title":"Did work","summary":"start to finish","event_indices":[0,1]}]`},
		usagePerCall: llm.Usage{PromptTokens: 120, CompletionTokens: 30},
	}
	window := correlator.TimeRange{Start: base, End: base.Add(time.Hour)}

	got, err := pipeline.RunNarrate(t.Context(), s, client, narrateConfig(), window, pipeline.NarrateOptions{})

	require.NoError(t, err)
	assert.Equal(t, 2, got.UnlinkedEvents)
	assert.Zero(t, got.ContextNarratives)
	require.Len(t, got.Narratives, 1)
	assert.Equal(t, correlator.ClusterNew, got.Narratives[0].Kind)
	assert.Equal(t, "Did work", got.Narratives[0].Title)
	require.NotZero(t, got.Narratives[0].ID, "a persisted narrative has a real id")
	assert.Len(t, got.Narratives[0].Events, 2, "member events justify the grouping")
	assert.Equal(t, int64(120), got.Stats.PromptTokens)

	// Persisted for real: the events are now linked, so a second pass sees none.
	linked, err := s.NarrativeEventCount(got.Narratives[0].ID)
	require.NoError(t, err)
	assert.Equal(t, 2, linked)
}

func TestRunNarrate_DryRunPersistsNothingButReportsWhatItWould(t *testing.T) {
	s := narrateStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	seedNarrateEvent(t, s, "e1", "some work", base)

	client := &narrateLLM{
		responses: []string{`[{"kind":"new","title":"Work","summary":"s","event_indices":[0]}]`},
	}
	window := correlator.TimeRange{Start: base, End: base.Add(time.Hour)}

	got, err := pipeline.RunNarrate(t.Context(), s, client, narrateConfig(), window,
		pipeline.NarrateOptions{DryRun: true})

	require.NoError(t, err)
	require.Len(t, client.prompts, 1, "dry run still makes the real LLM call")
	require.Len(t, got.Narratives, 1, "and still reports what it would have written")
	assert.Zero(t, got.Narratives[0].ID, "but nothing was persisted, so there is no id")

	// The events must still be unlinked afterward.
	remaining, err := s.UnlinkedEventsInRange(base, base.Add(time.Hour))
	require.NoError(t, err)
	assert.Len(t, remaining, 1, "dry run must not consume candidates")
}

func TestRunNarrate_NoCandidatesMakesNoLLMCall(t *testing.T) {
	s := narrateStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	client := &narrateLLM{} // would panic on an empty responses slice if called
	window := correlator.TimeRange{Start: base, End: base.Add(time.Hour)}

	got, err := pipeline.RunNarrate(t.Context(), s, client, narrateConfig(), window, pipeline.NarrateOptions{})

	require.NoError(t, err, "an empty window is a normal outcome, not a failure")
	assert.Empty(t, client.prompts, "no candidates means no spend")
	assert.Empty(t, got.Narratives)
	assert.Zero(t, got.Stats.Calls)
}

func TestRunNarrate_IrreducibleUnitErrorPropagates(t *testing.T) {
	// A single event too large for the context budget cannot be split further.
	// Cluster errors naming the window and the event; RunNarrate must surface
	// that rather than swallowing it — silently narrating nothing would drop
	// real work.
	s := narrateStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	seedNarrateEvent(t, s, "huge", strings.Repeat("x", 8000), base)

	client := &narrateLLM{responses: []string{"[]"}}
	cfg := narrateConfig()
	cfg.LLM.ContextWindowTokens = 50 // smaller than one event's prompt
	window := correlator.TimeRange{Start: base, End: base.Add(time.Hour)}

	_, err := pipeline.RunNarrate(t.Context(), s, client, cfg, window, pipeline.NarrateOptions{})

	require.Error(t, err)
	assert.Empty(t, client.prompts, "an unsplittable window must not spend a call")
}

func TestRunNarrate_HydratesOverlappingNarrativeEventsAsContext(t *testing.T) {
	s := narrateStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	// An existing narrative whose window ends where ours begins, with a linked event.
	seedNarrateEvent(t, s, "old", "PR #412 add cache layer", base.Add(-time.Hour))
	oldID, err := s.EventIDByExternalID("claude_code", "old")
	require.NoError(t, err)
	nid, err := s.InsertNarrative(base.Add(-2*time.Hour), base, "Cache rework", "reworking the cache")
	require.NoError(t, err)
	require.NoError(t, s.AddNarrativeEvents(nid, []int64{oldID}))

	seedNarrateEvent(t, s, "new", "fixed the cache eviction bug", base.Add(time.Minute))

	client := &narrateLLM{
		responses: []string{`[{"kind":"new","title":"T","summary":"s","event_indices":[0]}]`},
	}
	window := correlator.TimeRange{Start: base, End: base.Add(time.Hour)}

	got, err := pipeline.RunNarrate(t.Context(), s, client, narrateConfig(), window, pipeline.NarrateOptions{})

	require.NoError(t, err)
	assert.Equal(t, 1, got.ContextNarratives)
	require.Len(t, client.prompts, 1)
	// The Path-B round trip: the existing narrative's raw event, hydrated from
	// the store, reached the prompt as context.
	assert.Contains(t, client.prompts[0], "PR #412 add cache layer",
		"hydrated context events must reach the prompt, not just the summary")
	assert.Contains(t, client.prompts[0], "reworking the cache")
}
```

- [ ] **Step 2: Run to confirm it fails**

Run: `env -u GOROOT -u GOPATH go test ./internal/pipeline/ -run TestRunNarrate -v`

Expected: FAIL — `undefined: pipeline.RunNarrate`.

- [ ] **Step 3: Implement `RunNarrate`**

Create `internal/pipeline/narrate.go`:

```go
package pipeline

import (
	"context"
	"fmt"
	"time"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/llm"
	"github.com/jcogilvie/unjira/internal/store"
)

// NarrateOptions configures one narration pass.
type NarrateOptions struct {
	// DryRun runs the full pass — including the real LLM calls — but skips
	// Persist, so nothing is written. The reported narratives are exactly what
	// would have been persisted, with zero IDs since no rows were inserted.
	DryRun bool
}

// NarrateResult is one pass's outcome, carrying enough detail for a human to
// judge the clustering without re-querying the store.
type NarrateResult struct {
	Window            correlator.TimeRange
	UnlinkedEvents    int // clustering candidates considered
	ContextNarratives int // existing narratives passed to Cluster as context
	DryRun            bool
	Stats             correlator.Stats
	Narratives        []NarratedNarrative
	Compactions       []Compaction
}

// NarratedNarrative is one narrative this pass produced, with the member
// events that justify its grouping — the detail that makes the clustering
// judgeable rather than merely reportable.
type NarratedNarrative struct {
	Kind correlator.ClusterKind
	// ID is the persisted narrative id, or 0 under DryRun.
	ID int64
	// PriorWindowEnd is what window_end was before this pass extended it;
	// zero when Kind is ClusterNew.
	PriorWindowEnd time.Time
	WindowStart    time.Time
	WindowEnd      time.Time
	Title          string
	Summary        string
	Events         []correlator.Event
}

// Compaction records one tail-summarization, so the lossy step is visible in
// the pass output rather than only in logs.
type Compaction struct {
	NarrativeID  int64
	EventsFolded int
	Boundary     time.Time
}

// RunNarrate runs one narration pass over window: assemble Cluster's inputs
// from the store, cluster them, and (unless DryRun) persist the results.
//
// It does NOT acquire the pipeline lease. That is the caller's job, because
// the scope differs per caller: dev narrate wraps this one stage, while watch
// will wrap collect + narrate + reconcile in a single lease. Acquiring here
// would make watch contend with itself.
//
// Inputs are fetched once for the whole window and handed to Cluster as-is.
// Cluster re-derives its own in-window and adjacency filters at every
// bisection level (clusterWithSplit recurses with the full slices, narrowing
// only the window), so re-querying per sub-window would be both wrong and
// impossible — Cluster has no store by design.
func RunNarrate(
	ctx context.Context,
	s *store.Store,
	client llm.Client,
	cfg config.Config,
	window correlator.TimeRange,
	opts NarrateOptions,
) (NarrateResult, error) {
	result := NarrateResult{Window: window, DryRun: opts.DryRun}

	candidates, err := s.UnlinkedEventsInRange(window.Start, window.End)
	if err != nil {
		return NarrateResult{}, fmt.Errorf("assembling clustering candidates: %w", err)
	}
	result.UnlinkedEvents = len(candidates)

	if len(candidates) == 0 {
		// Nothing to narrate is a normal outcome. Return before spending a
		// call so an idle window costs nothing.
		return result, nil
	}

	existing, err := hydrateContextNarratives(s, window)
	if err != nil {
		return NarrateResult{}, err
	}
	result.ContextNarratives = len(existing)

	clustered, clusterStats, err := correlator.Cluster(
		ctx, candidates, existing, client, window, cfg.LLM.ContextWindowTokens)
	result.Stats.Add(clusterStats)
	if err != nil {
		return NarrateResult{}, fmt.Errorf("clustering: %w", err)
	}

	if opts.DryRun {
		result.Narratives = describeUnpersisted(clustered)

		return result, nil
	}

	// Capture pre-pass window ends so the output can show what an extend moved.
	priorEnds := make(map[int64]time.Time, len(existing))
	for _, n := range existing {
		priorEnds[n.ID] = n.WindowEnd
	}

	persisted, persistStats, err := correlator.Persist(ctx, s, client, clustered, cfg.Correlator)
	result.Stats.Add(persistStats)
	if err != nil {
		return NarrateResult{}, fmt.Errorf("persisting narratives: %w", err)
	}

	result.Narratives = describePersisted(clustered, persisted, priorEnds)

	result.Compactions, err = collectCompactions(s, persisted, persistStats)
	if err != nil {
		return NarrateResult{}, err
	}

	return result, nil
}

// hydrateContextNarratives loads the narratives overlapping or touching window
// and fills each one's Events from the store, which is what makes Cluster's
// context section carry raw events rather than just summaries (Path B).
func hydrateContextNarratives(s *store.Store, window correlator.TimeRange) ([]correlator.Narrative, error) {
	rows, err := s.NarrativesOverlapping(window.Start, window.End)
	if err != nil {
		return nil, fmt.Errorf("assembling context narratives: %w", err)
	}

	out := make([]correlator.Narrative, 0, len(rows))
	for _, row := range rows {
		contextEvents, err := s.NarrativeEventsForContext(row.ID)
		if err != nil {
			return nil, fmt.Errorf("hydrating context events for narrative %d: %w", row.ID, err)
		}

		out = append(out, correlator.Narrative{
			ID:          row.ID,
			WindowStart: row.WindowStart,
			WindowEnd:   row.WindowEnd,
			Title:       row.Title,
			Summary:     row.Summary,
			IssueKey:    row.IssueKey,
			Confidence:  row.Confidence,
			Status:      row.Status,
			Events:      contextEvents,
		})
	}

	return out, nil
}

// describeUnpersisted renders dry-run results, which have no ids because
// nothing was written. Window bounds come from the clustered events
// themselves, matching what Persist would have computed.
func describeUnpersisted(clustered []correlator.ClusterResult) []NarratedNarrative {
	out := make([]NarratedNarrative, 0, len(clustered))
	for _, r := range clustered {
		lo, hi := eventWindow(r.Events)
		out = append(out, NarratedNarrative{
			Kind:        r.Kind,
			ID:          0,
			WindowStart: lo,
			WindowEnd:   hi,
			Title:       r.Title,
			Summary:     r.Summary,
			Events:      r.Events,
		})
	}

	return out
}

// describePersisted pairs each persisted narrative with the clustered result
// that produced it, so the output can show member events (which Persist's
// return does not carry) alongside real ids and window bounds (which the
// cluster result does not carry).
func describePersisted(
	clustered []correlator.ClusterResult,
	persisted []correlator.Narrative,
	priorEnds map[int64]time.Time,
) []NarratedNarrative {
	out := make([]NarratedNarrative, 0, len(persisted))
	for i, n := range persisted {
		narrated := NarratedNarrative{
			Kind:        clusterKindAt(clustered, i),
			ID:          n.ID,
			WindowStart: n.WindowStart,
			WindowEnd:   n.WindowEnd,
			Title:       n.Title,
			Summary:     n.Summary,
		}
		if i < len(clustered) {
			narrated.Events = clustered[i].Events
		}
		if narrated.Kind == correlator.ClusterExtends {
			narrated.PriorWindowEnd = priorEnds[n.ID]
		}
		out = append(out, narrated)
	}

	return out
}

// clusterKindAt reports the kind of the i-th cluster result. Persist returns
// narratives in the same order it received results, so index alignment holds;
// this guards the bound rather than assuming it.
func clusterKindAt(clustered []correlator.ClusterResult, i int) correlator.ClusterKind {
	if i < len(clustered) {
		return clustered[i].Kind
	}

	return correlator.ClusterNew
}

// eventWindow returns the earliest and latest OccurredAt among evts.
func eventWindow(evts []correlator.Event) (lo, hi time.Time) {
	for i, e := range evts {
		if i == 0 || e.OccurredAt.Before(lo) {
			lo = e.OccurredAt
		}
		if i == 0 || e.OccurredAt.After(hi) {
			hi = e.OccurredAt
		}
	}

	return lo, hi
}

// collectCompactions reports which narratives this pass compacted, by reading
// back the boundary Persist just wrote. Stats.Compactions says how many
// happened; this says which, so the lossy step is auditable from the output.
func collectCompactions(
	s *store.Store,
	persisted []correlator.Narrative,
	stats correlator.Stats,
) ([]Compaction, error) {
	if stats.Compactions == 0 {
		return nil, nil
	}

	var out []Compaction
	for _, n := range persisted {
		row, err := s.GetNarrative(n.ID)
		if err != nil {
			return nil, fmt.Errorf("reading compaction boundary for narrative %d: %w", n.ID, err)
		}
		if row.CompactionBoundary == nil {
			continue
		}

		linked, err := s.NarrativeEventCount(n.ID)
		if err != nil {
			return nil, fmt.Errorf("counting linked events for narrative %d: %w", n.ID, err)
		}
		visible, err := s.NarrativeEventsForContext(n.ID)
		if err != nil {
			return nil, fmt.Errorf("counting context events for narrative %d: %w", n.ID, err)
		}

		out = append(out, Compaction{
			NarrativeID:  n.ID,
			EventsFolded: linked - len(visible),
			Boundary:     *row.CompactionBoundary,
		})
	}

	return out, nil
}
```

- [ ] **Step 4: Run to confirm it passes**

Run: `env -u GOROOT -u GOPATH go test ./internal/pipeline/ -v`

Expected: the 5 new tests PASS, and the pre-existing collect/digest tests still pass.

- [ ] **Step 5: Prove the dry-run test discriminates**

Temporarily make `RunNarrate` ignore `opts.DryRun` (always call `Persist`), confirm
`TestRunNarrate_DryRunPersistsNothingButReportsWhatItWould` FAILS on the "must not consume
candidates" assertion, restore, confirm it passes. Paste both outputs — a dry run that silently
writes is the worst failure this command could have.

- [ ] **Step 6: Commit**

```bash
env -u GOROOT -u GOPATH gofmt -w internal/pipeline/ internal/correlator/
env -u GOROOT -u GOPATH go vet ./internal/pipeline/... ./internal/correlator/...
git add internal/pipeline/narrate.go internal/pipeline/narrate_test.go internal/correlator/
git commit -m "Add pipeline.RunNarrate: assemble inputs, cluster, persist"
```

---

## Task 7: the output renderer

**Files:**
- Create: `internal/pipeline/narrate_render.go`
- Create: `internal/pipeline/narrate_render_test.go`

A pure function so it is table-testable without a store or a client.

- [ ] **Step 1: Write the failing tests**

Create `internal/pipeline/narrate_render_test.go`:

```go
package pipeline_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/pipeline"
)

func TestRenderNarrateResult(t *testing.T) {
	base := time.Date(2026, 8, 14, 9, 12, 0, 0, time.UTC)
	window := correlator.TimeRange{Start: base, End: base.Add(8 * time.Hour)}

	full := pipeline.NarrateResult{
		Window:            window,
		UnlinkedEvents:    5,
		ContextNarratives: 2,
		Stats: correlator.Stats{
			Calls: 4, Splits: 1, MergeChecks: 1,
			PromptTokens: 38412, CompletionTokens: 1205, EstimatedTokens: 41000,
		},
		Narratives: []pipeline.NarratedNarrative{
			{
				Kind: correlator.ClusterNew, ID: 14,
				WindowStart: base, WindowEnd: base.Add(2 * time.Hour),
				Title:   "Fix flaky correlator tests",
				Summary: "Chased an intermittent failure in the split path.",
				Events: []correlator.Event{
					events.NewEvent("claude_code", "e1", base, "unjira: 3 user messages."),
				},
			},
			{
				Kind: correlator.ClusterExtends, ID: 9,
				PriorWindowEnd: base.Add(-2 * time.Hour),
				WindowStart:    base.Add(-6 * time.Hour), WindowEnd: base.Add(time.Hour),
				Title: "Cache rework", Summary: "Continued the cache work.",
			},
		},
		Compactions: []pipeline.Compaction{
			{NarrativeID: 9, EventsFolded: 12, Boundary: base.Add(-4 * time.Hour)},
		},
	}

	t.Run("full pass shows stats, narratives, and member events", func(t *testing.T) {
		out := pipeline.RenderNarrateResult(full)

		assert.Contains(t, out, "5 unlinked candidate")
		assert.Contains(t, out, "2 narrative")
		assert.Contains(t, out, "4 call")
		assert.Contains(t, out, "1 split")
		assert.Contains(t, out, "38412")
		assert.Contains(t, out, "41000", "the estimate is shown next to the actual")
		assert.Contains(t, out, "[NEW #14]")
		assert.Contains(t, out, "Fix flaky correlator tests")
		assert.Contains(t, out, "unjira: 3 user messages.", "member events make the grouping judgeable")
		assert.Contains(t, out, "[EXTENDS #9]")
		assert.Contains(t, out, "folded 12 event")
	})

	t.Run("dry run marks ids as unpersisted and says so", func(t *testing.T) {
		dry := full
		dry.DryRun = true
		dry.Narratives = []pipeline.NarratedNarrative{{
			Kind: correlator.ClusterNew, ID: 0,
			WindowStart: base, WindowEnd: base.Add(time.Hour),
			Title: "Would be written", Summary: "s",
		}}

		out := pipeline.RenderNarrateResult(dry)

		assert.Contains(t, out, "nothing persisted")
		assert.Contains(t, out, "[NEW #-]", "a dry run has no id to show")
		assert.NotContains(t, out, "#0", "0 must never be rendered as an id")
	})

	t.Run("empty pass says so instead of printing nothing", func(t *testing.T) {
		out := pipeline.RenderNarrateResult(pipeline.NarrateResult{Window: window})

		assert.Contains(t, out, "no narratives produced")
	})
}
```

- [ ] **Step 2: Run to confirm it fails**

Run: `env -u GOROOT -u GOPATH go test ./internal/pipeline/ -run TestRenderNarrateResult -v`

Expected: FAIL — `undefined: pipeline.RenderNarrateResult`.

- [ ] **Step 3: Implement the renderer**

Create `internal/pipeline/narrate_render.go`:

```go
package pipeline

import (
	"fmt"
	"strings"
	"time"

	"github.com/jcogilvie/unjira/internal/correlator"
)

// RenderNarrateResult formats one pass for a human deciding whether the
// clustering is any good. A pure function of its input, so it can be tested
// without a store or an LLM.
//
// Member events are included per narrative deliberately: a title and a count
// cannot show whether a grouping is defensible, and judging that is the whole
// point of the command.
func RenderNarrateResult(r NarrateResult) string {
	var b strings.Builder

	b.WriteString("== narration pass ==\n")
	fmt.Fprintf(&b, "window   %s .. %s\n",
		r.Window.Start.Format(time.RFC3339), r.Window.End.Format(time.RFC3339))
	fmt.Fprintf(&b, "events   %d unlinked candidate(s), %d narrative(s) as context\n",
		r.UnlinkedEvents, r.ContextNarratives)
	fmt.Fprintf(&b, "llm      %d call(s), %d split(s), %d merge check(s)\n",
		r.Stats.Calls, r.Stats.Splits, r.Stats.MergeChecks)
	fmt.Fprintf(&b, "tokens   %d prompt + %d completion (estimated %d)\n",
		r.Stats.PromptTokens, r.Stats.CompletionTokens, r.Stats.EstimatedTokens)

	for _, c := range r.Compactions {
		fmt.Fprintf(&b, "compact  narrative %d: folded %d event(s) up to %s\n",
			c.NarrativeID, c.EventsFolded, c.Boundary.Format(time.RFC3339))
	}

	if r.DryRun {
		b.WriteString("dry run: nothing persisted\n")
	}

	if len(r.Narratives) == 0 {
		b.WriteString("\nno narratives produced\n")

		return b.String()
	}

	for _, n := range r.Narratives {
		b.WriteString("\n")
		fmt.Fprintf(&b, "[%s #%s] %s .. %s", narrativeKindLabel(n.Kind), narrativeIDLabel(n.ID),
			n.WindowStart.Format(time.RFC3339), n.WindowEnd.Format(time.RFC3339))
		if n.Kind == correlator.ClusterExtends && !n.PriorWindowEnd.IsZero() {
			fmt.Fprintf(&b, "  (window_end was %s)", n.PriorWindowEnd.Format(time.RFC3339))
		}
		b.WriteString("\n")
		fmt.Fprintf(&b, "  %q\n", n.Title)
		fmt.Fprintf(&b, "  %s\n", n.Summary)

		if len(n.Events) == 0 {
			continue
		}
		fmt.Fprintf(&b, "  events (%d):\n", len(n.Events))
		for _, e := range n.Events {
			fmt.Fprintf(&b, "    - [%s] %s  %s\n", e.Source, e.Summary, e.OccurredAt.Format(time.RFC3339))
		}
	}

	return b.String()
}

// narrativeKindLabel renders a cluster kind for the output's leading tag.
func narrativeKindLabel(k correlator.ClusterKind) string {
	if k == correlator.ClusterExtends {
		return "EXTENDS"
	}

	return "NEW"
}

// narrativeIDLabel renders a narrative id, or "-" when there isn't one — a
// dry run persists nothing, and printing "#0" would look like a real row.
func narrativeIDLabel(id int64) string {
	if id == 0 {
		return "-"
	}

	return fmt.Sprintf("%d", id)
}
```

- [ ] **Step 4: Run to confirm it passes**

Run: `env -u GOROOT -u GOPATH go test ./internal/pipeline/ -v`

Expected: all subtests PASS.

- [ ] **Step 5: Commit**

```bash
env -u GOROOT -u GOPATH gofmt -w internal/pipeline/
env -u GOROOT -u GOPATH go vet ./internal/pipeline/...
git add internal/pipeline/narrate_render.go internal/pipeline/narrate_render_test.go
git commit -m "Add narration pass renderer"
```

---

## Task 8: the `dev narrate` CLI command

**Files:**
- Modify: `cmd/unjira/main.go`

- [ ] **Step 1: Add the API-key flag**

In `cmd/unjira/main.go`, add to the `cli` struct after `JiraCredentials`:

```go
	LLMAPIKey string `env:"UNJIRA_LLM_API_KEY" help:"API key for the LLM backend."`
```

Thread it into `appContext` — add the field to the struct:

```go
type appContext struct {
	config          config.Config
	store           *store.Store
	jiraCredentials JiraCredentials
	llmAPIKey       string
}
```

and populate it in `run()`'s final call:

```go
	return ctx.Run(&appContext{
		config:          cfg,
		store:           s,
		jiraCredentials: cli.JiraCredentials,
		llmAPIKey:       cli.LLMAPIKey,
	})
```

- [ ] **Step 2: Add an `appContext` helper that builds the LLM client**

Add beside the other `appContext` helpers:

```go
// llmClient builds the configured LLM client, validating the config and the
// credential first so a misconfiguration fails before any collector runs or
// any lease is taken.
//
// The key comes from the environment (UNJIRA_LLM_API_KEY), never a config
// file — the same rule UNJIRA_JIRA_CREDENTIALS follows.
func (a *appContext) llmClient() (llm.Client, error) {
	if err := a.config.LLM.Validate(); err != nil {
		return nil, err
	}
	// LLMConfig.Validate covers Model and ContextWindowTokens but not BaseURL
	// (verified in internal/config/config.go). An empty base URL would send
	// unjira's prompts to the SDK's own default endpoint — api.openai.com —
	// which is both wrong and a credential-leak risk when the configured
	// backend was meant to be a local gateway. Fail loudly instead.
	if a.config.LLM.BaseURL == "" {
		return nil, fmt.Errorf("llm.base_url is required: refusing to fall back to the SDK's default endpoint")
	}
	if a.llmAPIKey == "" {
		return nil, fmt.Errorf("UNJIRA_LLM_API_KEY is not set: the LLM backend needs a credential")
	}

	return openai.New(a.config.LLM.BaseURL, a.llmAPIKey, a.config.LLM.Model), nil
}
```

Add imports: `"github.com/jcogilvie/unjira/internal/clients/openai"`,
`"github.com/jcogilvie/unjira/internal/llm"`, `"github.com/jcogilvie/unjira/internal/correlator"`.

- [ ] **Step 3: Add the command**

Add the command type and its `Run`:

```go
type devNarrateCmd struct {
	Since  config.Span `default:"24h" help:"How far back to narrate (e.g. 36h, 7d, 2w, 7d12h)."`
	DryRun bool        `help:"Run the full pass, including real LLM calls, but persist nothing."`
}

func (c *devNarrateCmd) Run(app *appContext) error {
	client, err := app.llmClient()
	if err != nil {
		return err
	}
	if err := app.config.Correlator.Validate(); err != nil {
		return err
	}

	linkExclusions, err := app.config.CompiledLinkExclusions()
	if err != nil {
		return err
	}

	ctx := context.Background()
	runID := fmt.Sprintf("dev-narrate-%d", os.Getpid())

	// Blocking acquire: a concurrent pass makes this wait rather than fail.
	// The lease is held here, not inside RunNarrate, because watch will need
	// one lease spanning collect + narrate + reconcile.
	if err := app.store.Acquire(ctx, runID, time.Now, narrateLeaseTTL, narrateLeasePoll); err != nil {
		return err
	}
	defer func() {
		if relErr := app.store.ReleaseLock(runID); relErr != nil {
			log.Printf("releasing pipeline lock: %v", relErr)
		}
	}()

	if _, err := pipeline.RunCollect(app.config, app.store, registry, linkExclusions); err != nil {
		return err
	}

	now := time.Now().UTC()
	window := correlator.TimeRange{Start: now.Add(-c.Since.Duration()), End: now}

	result, err := pipeline.RunNarrate(ctx, app.store, client, app.config, window,
		pipeline.NarrateOptions{DryRun: c.DryRun})
	if err != nil {
		return err
	}

	fmt.Print(pipeline.RenderNarrateResult(result))

	return nil
}
```

Add the lease constants near the top of the file, beside `registry`:

```go
const (
	// narrateLeaseTTL bounds how long a crashed narration pass can hold the
	// pipeline lock before another run may steal it. Generous relative to a
	// pass (minutes), since stealing a live lease is worse than waiting.
	narrateLeaseTTL = 30 * time.Minute
	// narrateLeasePoll is how often a blocked Acquire retries.
	narrateLeasePoll = 2 * time.Second
)
```

Register it in `devCmd`:

```go
type devCmd struct {
	Seed     devSeedCmd     `cmd:"" help:"Create labeled test issues and generate changelog history."`
	Reset    devResetCmd    `cmd:"" help:"Delete every seed-labeled issue in the project."`
	Workflow devWorkflowCmd `cmd:"" help:"Mine and print the observed workflow graph for a project."`
	Narrate  devNarrateCmd  `cmd:"" help:"Run one collect+cluster+persist pass and print the narratives."`
}
```

Add imports for `"context"`, `"log"`, `"os"`, and `"time"` if not already present.

- [ ] **Step 4: Verify it builds and the flag parses**

```bash
env -u GOROOT -u GOPATH go build ./...
env -u GOROOT -u GOPATH go run ./cmd/unjira dev narrate --help
```

Expected: the build succeeds; help shows `--since` (default `24h0m0s`) and `--dry-run`.

Then verify `Span` is wired correctly through Kong — this is the integration the unit tests cannot
cover:

```bash
env -u GOROOT -u GOPATH go run ./cmd/unjira dev narrate --since 7d --help
env -u GOROOT -u GOPATH go run ./cmd/unjira dev narrate --since bogus 2>&1 | head -3
```

Expected: `7d` is accepted; `bogus` produces a parse error naming the value. If Kong reports
`--since: unsupported type` or similar, `Span` is not satisfying `encoding.TextUnmarshaler` — check
that `UnmarshalText` is defined on the **pointer** receiver.

- [ ] **Step 5: Verify the missing-key error fires before any work**

```bash
env -u UNJIRA_LLM_API_KEY -u GOROOT -u GOPATH go run ./cmd/unjira dev narrate --dry-run 2>&1 | head -3
```

Expected: an error naming `UNJIRA_LLM_API_KEY`, with no collector output and no lease taken. If
collectors run first, the ordering in `Run` is wrong — the credential check must come before
`RunCollect`.

- [ ] **Step 6: Commit**

```bash
env -u GOROOT -u GOPATH gofmt -w cmd/unjira/main.go
env -u GOROOT -u GOPATH go vet ./cmd/...
git add cmd/unjira/main.go
git commit -m "Add unjira dev narrate command with UNJIRA_LLM_API_KEY wiring"
```

---

## Task 9: full regression + lint

**Files:** none (verification only)

- [ ] **Step 1:** `env -u GOROOT -u GOPATH go test ./... -count=1 2>&1 | tail -20` — every package `ok`.
- [ ] **Step 2:** `env -u GOROOT -u GOPATH go vet ./...` — clean.
- [ ] **Step 3:** lint must be `0 issues`:

```bash
export PATH="$HOME/.asdf/installs/golang/1.25.7/bin:$PATH"
env -u GOROOT -u GOPATH golangci-lint run ./...
```

Watch for: missing doc comments on the new exported identifiers (`llm.Client`, `llm.Usage`,
`config.Span`, `Stats` and its methods, `RunNarrate`, `NarrateResult`, `NarratedNarrative`,
`Compaction`, `RenderNarrateResult`); `gocognit` on `RunNarrate` and `RenderNarrateResult` (decompose
rather than disabling the linter); `gci` import grouping in every new file. Fix findings minimally and
re-run.

- [ ] **Step 4:** `earthly +reviewable` — the canonical CI gate, which runs lint and tests in a clean
  container with its own Go 1.26 toolchain. Expected: `SUCCESS`. This is the authoritative check; a
  local pass with a container failure means something is environment-dependent.

- [ ] **Step 5:** confirm the dependency direction invariants hold:

```bash
grep -rn "internal/clients" internal/llm/ || echo "OK: llm imports no clients"
grep -rn "internal/correlator" internal/store/ || echo "OK: store does not import correlator"
grep -rn "internal/correlator\|internal/pipeline" internal/clients/ || echo "OK: clients import neither"
```

Expected: all three `OK` lines. The first is what makes a second LLM backend addable; the third is
what keeps the facade from depending on its consumers.

- [ ] **Step 6:** commit if any step required fixes:

```bash
git add -A
git commit -m "Fix lint findings in the dev narrate slice"
```

---

## Task 10: record the slice in the phase-1 spec

**Files:**
- Modify: `docs/superpowers/specs/2026-08-11-phase1-correlator-design.md`

- [ ] **Step 1: Add a note under the slice-3 entry**

The slice-3 entry already ends with the two design corrections. Append a third bullet to that list,
recording that its machinery now has a caller:

```markdown
   - **`unjira dev narrate` exercises this slice end to end.** Slice 3 shipped with no caller, which
     hid a gap: nothing in the store could assemble `Cluster`'s inputs — neither "events not yet
     linked to a narrative" nor "narratives overlapping a window" had a query, invisible while every
     test built its inputs by hand. Both accessors, plus `pipeline.RunNarrate` (the orchestration
     `watch` reuses) and real token-usage observability, landed with that command. See
     `docs/superpowers/specs/2026-08-19-dev-narrate-design.md`.
```

- [ ] **Step 2: Commit**

```bash
git add docs/superpowers/specs/2026-08-11-phase1-correlator-design.md
git commit -m "Record dev narrate and the Cluster-input accessors in the phase-1 spec"
```

---

## Verification checklist (spec coverage)

- [x] `internal/llm` contract package, no `internal/clients` imports — Task 1.
- [x] `config.Span` via `go-str2duration`, non-positive rejected — Task 2.
- [x] `UnlinkedEventsInRange`: never-linked semantics, half-open range, composite ordering — Task 3.
- [x] `NarrativesOverlapping`: touching endpoints included, `NarrativeRow` returned, `status` ignored — Task 3.
- [x] `Complete` returns `llm.Usage`; missing usage is zero, not an error — Task 4.
- [x] `correlator.Stats` with calls/splits/merge-checks/compactions/tokens/estimate, aggregating
  across bisection — Task 5.
- [x] `llmClient` deleted; `Cluster`/`Persist` take `llm.Client` — Task 5.
- [x] `RunNarrate` fetches once, hydrates context, no lease acquisition — Task 6.
- [x] Error handling: irreducible-unit error propagates; `ReleaseLock` deferred and logged,
  never masking a pass failure — Tasks 6, 8.
- [x] `--dry-run`: real LLM, no persistence, lease still held — Tasks 6, 8.
- [x] Output with member events + pass stats + compactions — Task 7.
- [x] `UNJIRA_LLM_API_KEY`, failing before any work — Task 8.
- [x] One window, `Cluster` bisects adaptively — Task 8 (`window` built from `--since` only).

Not in scope (later slices): the reconciler, actions, issue-matching, the auto-commit gate, and
`watch` itself.
