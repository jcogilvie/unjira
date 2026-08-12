# `internal/correlator.Cluster` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build `internal/correlator.Cluster`, a pure-compute function that groups a window of
`claude_code` events (plus adjacent/overlapping already-narrated events) into narratives via an
LLM call, tagging each result `new` or `extends <narrative_id>`, with a split-by-time-and-merge
overflow path for oversized windows. This is phase-1 implementation slice 2 of 7 — see
`docs/superpowers/specs/2026-08-11-phase1-correlator-design.md` (overall phase-1 design) and
`docs/superpowers/specs/2026-08-12-correlator-cluster-design.md` (this slice's design). No
persistence, no CLI command — `Persist` and real store wiring land in slice 3.

**Architecture:** One new package, `internal/correlator` (sibling to the existing
`internal/correlator/refs` and `internal/correlator/fanout`), with a single file `correlator.go`
holding the types, the consumer-owned `llmClient` interface, and `Cluster` itself. `Cluster` is
recursive: it estimates a fixed-ratio token budget for its assembled prompt, and when the budget
is exceeded it bisects the time window and recurses, merging the two halves' results afterward
(exact-narrative-ID merges deterministically; adjacent same-story `new` clusters via one more LLM
call). Every LLM interaction goes through a small `llmClient` interface matching
`internal/clients/openai.Client.Complete`'s exact signature, so tests use a hand-written fake with
no HTTP server involved — this package never imports `internal/clients/openai` directly, mirroring
`internal/workflow`'s `projectMiner` decoupling from `internal/clients/jira`.

**Tech Stack:** Go 1.26, `github.com/stretchr/testify` (`assert`/`require`), stdlib
`encoding/json`/`context`/`time`/`sort`/`math`. No new external dependencies — `Complete`'s shape
from slice 1 is reused as-is via a local interface declaration, not an import of the concrete
`openai.Client` type.

---

## File structure

- **Create:** `internal/correlator/correlator.go` — `TimeRange`, `Narrative`, `ClusterKind`,
  `ClusterResult`, the `llmClient` interface, `Cluster`, and all unexported helpers (prompt
  building, token estimation, response parsing, window bisection, split-result merging).
- **Create:** `internal/correlator/correlator_test.go` — all tests, `package correlator_test`,
  using a hand-written fake `llmClient` (no `httptest`, no store).

No existing file needs splitting; this is a brand-new package.

---

## Task 1: Package skeleton — types, `llmClient` interface, and a fake for tests

**Files:**
- Create: `internal/correlator/correlator.go`
- Create: `internal/correlator/correlator_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/correlator/correlator_test.go`:

```go
// Package correlator_test exercises Cluster against a hand-written fake
// llmClient, not a real HTTP server — Cluster's own logic (window/adjacency
// filtering, prompt construction, response parsing, split/merge) is what's
// under test here, not any wire protocol. Compare
// internal/workflow/workflow_test.go's fakeMiner, which decouples
// MineProject from a concrete *jira.Client the same way.
package correlator_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/correlator"
)

// fakeLLM satisfies correlator's llmClient interface without making any
// real call. responses is consumed in call order; a test that only cares
// about one canned response can set a single-element slice.
type fakeLLM struct {
	responses []string
	prompts   []string // captured user prompts, in call order, for assertions
	err       error
}

func (f *fakeLLM) Complete(_ context.Context, _ string, userPrompt string) (string, error) {
	f.prompts = append(f.prompts, userPrompt)
	if f.err != nil {
		return "", f.err
	}

	idx := len(f.prompts) - 1
	if idx >= len(f.responses) {
		idx = len(f.responses) - 1
	}

	return f.responses[idx], nil
}

func TestCluster_EmptyEventsReturnsEmptyResult(t *testing.T) {
	llm := &fakeLLM{responses: []string{"[]"}}

	results, err := correlator.Cluster(t.Context(), nil, nil, llm, correlator.TimeRange{}, 128000)

	require.NoError(t, err)
	require.Empty(t, results)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `env -u GOROOT -u GOPATH go test ./internal/correlator/... -v`

Expected: `FAIL` — `internal/correlator/correlator_test.go:XX:X: no non-test Go files in
.../internal/correlator` (the package doesn't exist yet), or once files are half-created,
`undefined: correlator.Cluster`/`correlator.TimeRange`.

- [ ] **Step 3: Write the minimal implementation**

Create `internal/correlator/correlator.go`:

```go
// Package correlator clusters events into narratives — one logical unit of
// work, whatever raw events it took to produce it. Cluster is pure compute:
// it never touches the store (see internal/store's Narrative/ClusterResult
// mirror-shape note below); Persist (a later slice) is what writes results
// to real narratives/narrative_events rows.
//
// See docs/superpowers/specs/2026-08-11-phase1-correlator-design.md for the
// full phase-1 vertical this package is one component of, and
// docs/superpowers/specs/2026-08-12-correlator-cluster-design.md for this
// package's own design (prompt/response contract, overflow handling,
// rationale for every non-obvious choice below).
package correlator

import (
	"context"
	"time"
)

// TimeRange is a half-open [Start, End) window over events.Event.OccurredAt.
type TimeRange struct {
	Start, End time.Time
}

// Narrative mirrors internal/store's `narratives` table row shape so a
// later slice's Persist doesn't have to reshape this type when it starts
// writing real rows — Cluster only reads WindowStart/WindowEnd (for
// overlap/adjacency) and ID/Title/Summary (for prompt context), but the
// full shape is defined now so nothing here changes when persistence
// lands.
type Narrative struct {
	ID          int64
	WindowStart time.Time
	WindowEnd   time.Time
	Title       string
	Summary     string
	IssueKey    string
	Confidence  float64
	Status      string
}

// ClusterKind distinguishes a brand-new narrative from one extending an
// existing row.
type ClusterKind int

const (
	ClusterNew ClusterKind = iota
	ClusterExtends
)

// ClusterResult is one narrative-shaped grouping of events, held in memory
// only. NarrativeID is set only when Kind == ClusterExtends.
type ClusterResult struct {
	Kind        ClusterKind
	NarrativeID int64
	Title       string
	Summary     string
	Events      []Event
}

// llmClient is the narrow capability Cluster needs from an LLM backend — a
// consumer-owned interface (same pattern as internal/workflow's
// projectMiner) so tests use a fake with no real HTTP server.
// *openai.Client (internal/clients/openai) already satisfies this with
// zero adapter code; this package deliberately never imports
// internal/clients/openai to keep that decoupling real, not incidental.
type llmClient interface {
	Complete(ctx context.Context, systemPrompt, userPrompt string) (string, error)
}

// Cluster groups evts (filtered to window) plus any Narrative in existing
// whose window overlaps or is adjacent to window, into narratives — each
// tagged new or extending an existing row. Pure compute: no store access.
func Cluster(
	_ context.Context,
	_ []Event,
	_ []Narrative,
	_ llmClient,
	_ TimeRange,
	_ int,
) ([]ClusterResult, error) {
	return nil, nil
}
```

Note: `Event` here is a placeholder type alias resolved in Task 2 — Step 3 of *this* task only
needs the package to compile and the empty-input test to pass, so `Cluster` is a stub returning
`nil, nil` unconditionally for now. Add this type alias so `correlator_test.go` and this file both
compile:

```go
// Event is an alias for events.Event — Cluster operates on the same
// normalized event shape every collector emits.
type Event = events.Event
```

Add the import: `"github.com/jcogilvie/unjira/internal/events"`.

- [ ] **Step 4: Run test to verify it passes**

Run: `env -u GOROOT -u GOPATH go test ./internal/correlator/... -v`

Expected: `PASS` — `TestCluster_EmptyEventsReturnsEmptyResult` passes (trivially, since `Cluster`
is still a stub).

- [ ] **Step 5: Commit**

```bash
git add internal/correlator/correlator.go internal/correlator/correlator_test.go
git commit -m "Add internal/correlator package skeleton: types, llmClient interface, Cluster stub"
```

---

## Task 2: Implement the happy path — single call, no split

**Files:**
- Modify: `internal/correlator/correlator.go`
- Modify: `internal/correlator/correlator_test.go`

- [ ] **Step 1: Write the failing tests**

Add to `internal/correlator/correlator_test.go`:

```go
func mustEvent(t *testing.T, source, externalID, summary string, occurredAt time.Time) correlator.Event {
	t.Helper()
	e := events.NewEvent(source, externalID, occurredAt, summary)
	return e
}

func TestCluster_SingleNewCluster(t *testing.T) {
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	evts := []correlator.Event{
		mustEvent(t, "claude_code", "e1", "Started investigating flaky test", base),
		mustEvent(t, "claude_code", "e2", "Found root cause: race condition", base.Add(time.Minute)),
	}
	llm := &fakeLLM{responses: []string{
		`[{"kind":"new","title":"Fix flaky test","summary":"Investigated and found a race condition.","event_indices":[0,1]}]`,
	}}

	results, err := correlator.Cluster(t.Context(), evts, nil, llm, correlator.TimeRange{
		Start: base.Add(-time.Hour), End: base.Add(time.Hour),
	}, 128000)

	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, correlator.ClusterNew, results[0].Kind)
	assert.Equal(t, "Fix flaky test", results[0].Title)
	assert.Equal(t, "Investigated and found a race condition.", results[0].Summary)
	require.Len(t, results[0].Events, 2)
	assert.Equal(t, "e1", results[0].Events[0].ExternalID)
	assert.Equal(t, "e2", results[0].Events[1].ExternalID)
}

func TestCluster_SingleExtendsCluster(t *testing.T) {
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	evts := []correlator.Event{
		mustEvent(t, "claude_code", "e1", "Continued the fix", base),
	}
	existing := []correlator.Narrative{
		{ID: 42, WindowStart: base.Add(-time.Hour), WindowEnd: base, Title: "Fix flaky test", Summary: "prior work"},
	}
	llm := &fakeLLM{responses: []string{
		`[{"kind":"extends","narrative_id":42,"title":"Fix flaky test","summary":"Continued and finished the fix.","event_indices":[0]}]`,
	}}

	results, err := correlator.Cluster(t.Context(), evts, existing, llm, correlator.TimeRange{
		Start: base.Add(-time.Minute), End: base.Add(time.Hour),
	}, 128000)

	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, correlator.ClusterExtends, results[0].Kind)
	assert.Equal(t, int64(42), results[0].NarrativeID)
	require.Len(t, results[0].Events, 1)
	assert.Equal(t, "e1", results[0].Events[0].ExternalID)
}

func TestCluster_MixedBatchOfNewAndExtends(t *testing.T) {
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	evts := []correlator.Event{
		mustEvent(t, "claude_code", "e1", "unrelated new work", base),
		mustEvent(t, "claude_code", "e2", "continuing prior work", base.Add(time.Minute)),
	}
	existing := []correlator.Narrative{
		{ID: 7, WindowStart: base.Add(-time.Hour), WindowEnd: base, Title: "Prior", Summary: "s"},
	}
	llm := &fakeLLM{responses: []string{
		`[{"kind":"new","title":"New thing","summary":"s1","event_indices":[0]},` +
			`{"kind":"extends","narrative_id":7,"title":"Prior","summary":"s2","event_indices":[1]}]`,
	}}

	results, err := correlator.Cluster(t.Context(), evts, existing, llm, correlator.TimeRange{
		Start: base.Add(-time.Minute), End: base.Add(time.Hour),
	}, 128000)

	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, correlator.ClusterNew, results[0].Kind)
	assert.Equal(t, correlator.ClusterExtends, results[1].Kind)
	assert.Equal(t, int64(7), results[1].NarrativeID)
}
```

Add `"time"` and `"github.com/stretchr/testify/assert"` to the test file's import block (the
skeleton test only needed `require`), and `"github.com/jcogilvie/unjira/internal/events"` for
`mustEvent`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `env -u GOROOT -u GOPATH go test ./internal/correlator/... -v`

Expected: `FAIL` on all three new tests — `Cluster` still returns `nil, nil` unconditionally, so
`require.Len(t, results, 1)` (or `2`) fails.

- [ ] **Step 3: Write the minimal implementation**

Replace the stub `Cluster` function body in `internal/correlator/correlator.go` with the real
happy-path implementation (no split/merge yet — that's Tasks 5-6):

```go
// Cluster groups evts (filtered to window) plus any Narrative in existing
// whose window overlaps or is adjacent to window, into narratives — each
// tagged new or extending an existing row. Pure compute: no store access.
func Cluster(
	ctx context.Context,
	evts []Event,
	existing []Narrative,
	llm llmClient,
	window TimeRange,
	contextWindowTokens int,
) ([]ClusterResult, error) {
	filtered := filterEventsInWindow(evts, window)
	relevant := filterAdjacentOrOverlapping(existing, window)

	systemPrompt, userPrompt := buildClusterPrompt(filtered, relevant)

	estimated := estimateTokens(systemPrompt + userPrompt)
	if estimated > contextWindowTokens {
		return clusterWithSplit(ctx, evts, existing, llm, window, contextWindowTokens, filtered)
	}

	raw, err := llm.Complete(ctx, systemPrompt, userPrompt)
	if err != nil {
		return nil, fmt.Errorf("clustering events in window [%s, %s): %w", window.Start, window.End, err)
	}

	return parseClusterResponse(raw, filtered)
}

// filterEventsInWindow returns the events in evts whose OccurredAt falls in
// window's half-open [Start, End) range, order-preserving.
func filterEventsInWindow(evts []Event, window TimeRange) []Event {
	var out []Event
	for _, e := range evts {
		if !e.OccurredAt.Before(window.Start) && e.OccurredAt.Before(window.End) {
			out = append(out, e)
		}
	}
	return out
}

// filterAdjacentOrOverlapping returns narratives whose window overlaps
// window or sits immediately adjacent to it (touching at a boundary) —
// temporal proximity is real clustering signal, so this is deliberately
// broader than a strict overlap check.
func filterAdjacentOrOverlapping(existing []Narrative, window TimeRange) []Narrative {
	var out []Narrative
	for _, n := range existing {
		if n.WindowEnd.Before(window.Start) || n.WindowStart.After(window.End) {
			continue
		}
		out = append(out, n)
	}
	return out
}

// estimateTokens gives a conservative, non-exact token count estimate —
// exactness isn't the goal, only a margin safe enough to decide whether to
// split before spending a real call. ~4 characters per token is the
// standard rough ratio for English text against OpenAI-family tokenizers.
func estimateTokens(text string) int {
	return (len(text) + 3) / 4
}
```

Add `"fmt"` to the import block. `buildClusterPrompt` and `parseClusterResponse` (below) are the
real, final implementations — no stubbing needed for either, since neither depends on the
split/merge path. `clusterWithSplit` (called above when the estimate exceeds budget) does need to
exist for this to compile, but stays a stub returning a loud error until Task 5 implements it for
real:

```go
// buildClusterPrompt renders the fixed system prompt and a user prompt
// listing evts (numbered 0..N-1) and existing narratives, per this
// package's documented prompt/response contract (see
// docs/superpowers/specs/2026-08-12-correlator-cluster-design.md).
func buildClusterPrompt(evts []Event, existing []Narrative) (systemPrompt, userPrompt string) {
	systemPrompt = clusterSystemPrompt

	var b strings.Builder
	b.WriteString("Events:\n")
	for i, e := range evts {
		fmt.Fprintf(&b, "%d. [%s] %s (occurred_at=%s)\n", i, e.Source, e.Summary, e.OccurredAt.Format(time.RFC3339))
	}
	b.WriteString("\nExisting narratives:\n")
	if len(existing) == 0 {
		b.WriteString("(none)\n")
	}
	for _, n := range existing {
		fmt.Fprintf(&b, "narrative_id=%d title=%q summary=%q window=[%s, %s)\n",
			n.ID, n.Title, n.Summary, n.WindowStart.Format(time.RFC3339), n.WindowEnd.Format(time.RFC3339))
	}

	return systemPrompt, b.String()
}

const clusterSystemPrompt = `Cluster the given events into narratives. Every event index belongs to exactly one cluster. Tag each cluster "new" or "extends"; if "extends", include the narrative_id of the existing narrative it continues. Return ONLY a JSON array matching this shape, no prose, no markdown fences:
[{"kind":"new"|"extends","narrative_id":<int, only if extends>,"title":"...","summary":"...","event_indices":[0,2,5]}]`

// clusterResponseItem is the wire shape of one element in the model's JSON
// array response.
type clusterResponseItem struct {
	Kind          string `json:"kind"`
	NarrativeID   int64  `json:"narrative_id"`
	Title         string `json:"title"`
	Summary       string `json:"summary"`
	EventIndices  []int  `json:"event_indices"`
}

// parseClusterResponse unmarshals raw (the model's response body) against
// evts (the exact slice sent in the prompt, so indices map back correctly).
// Any malformed shape — invalid JSON, an out-of-range index, an unknown
// kind — is a loud error including the raw response, never a partial or
// best-effort result.
func parseClusterResponse(raw string, evts []Event) ([]ClusterResult, error) {
	var items []clusterResponseItem
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return nil, fmt.Errorf("parsing cluster response %q: %w", raw, err)
	}

	results := make([]ClusterResult, 0, len(items))
	for _, item := range items {
		var kind ClusterKind
		switch item.Kind {
		case "new":
			kind = ClusterNew
		case "extends":
			kind = ClusterExtends
		default:
			return nil, fmt.Errorf("parsing cluster response %q: unknown kind %q", raw, item.Kind)
		}

		clusterEvents := make([]Event, 0, len(item.EventIndices))
		for _, idx := range item.EventIndices {
			if idx < 0 || idx >= len(evts) {
				return nil, fmt.Errorf("parsing cluster response %q: event_indices value %d out of range [0,%d)", raw, idx, len(evts))
			}
			clusterEvents = append(clusterEvents, evts[idx])
		}

		results = append(results, ClusterResult{
			Kind:        kind,
			NarrativeID: item.NarrativeID,
			Title:       item.Title,
			Summary:     item.Summary,
			Events:      clusterEvents,
		})
	}

	return results, nil
}

// clusterWithSplit is implemented in Task 5 (window bisection + merge).
// This task only needs it to exist so Cluster compiles; it errors loudly
// rather than silently proceeding with an oversized request.
func clusterWithSplit(
	_ context.Context,
	_ []Event,
	_ []Narrative,
	_ llmClient,
	window TimeRange,
	_ int,
	filtered []Event,
) ([]ClusterResult, error) {
	return nil, fmt.Errorf(
		"context budget exceeded for window [%s, %s) with %d event(s): split-and-merge not yet implemented",
		window.Start, window.End, len(filtered),
	)
}
```

Add `"encoding/json"` and `"strings"` to the import block.

- [ ] **Step 4: Run tests to verify they pass**

Run: `env -u GOROOT -u GOPATH go test ./internal/correlator/... -v`

Expected: `PASS` on `TestCluster_EmptyEventsReturnsEmptyResult`,
`TestCluster_SingleNewCluster`, `TestCluster_SingleExtendsCluster`,
`TestCluster_MixedBatchOfNewAndExtends`.

- [ ] **Step 5: Commit**

```bash
git add internal/correlator/correlator.go internal/correlator/correlator_test.go
git commit -m "Implement Cluster's happy path: window filtering, prompt, response parsing"
```

---

## Task 3: Response-parsing error cases (malformed JSON, bad index, unknown kind)

**Files:**
- Modify: `internal/correlator/correlator_test.go`

No production code changes expected in this task — `parseClusterResponse` (Task 2) already
handles all three cases correctly; this task's job is proving it with tests, per TDD discipline
(the behavior exists, but isn't yet demonstrated to be correct under adversarial input).

- [ ] **Step 1: Write the test**

Add to `internal/correlator/correlator_test.go`. This is one table-driven test — four scenarios
that are all "Cluster is given a bad response/call outcome, assert the resulting error contains a
specific substring" — matching the same shape already established for
`internal/clients/jira/tracker_test.go`'s `TestTracker_GetIssue_MapsStatusCategoriesToNormalizedBuckets`
and `internal/config/config_test.go`'s `TestLLMConfig_Validate`, not four separate functions:

```go
func TestCluster_ResponseAndCallFailuresErrorLoudly(t *testing.T) {
	tests := []struct {
		name        string
		responses   []string
		llmErr      error
		wantErrText string
	}{
		{
			name:        "malformed JSON response",
			responses:   []string{"not json"},
			wantErrText: "not json",
		},
		{
			name:        "out of range event index",
			responses:   []string{`[{"kind":"new","title":"t","summary":"s","event_indices":[5]}]`},
			wantErrText: "out of range",
		},
		{
			name:        "unknown kind",
			responses:   []string{`[{"kind":"maybe","title":"t","summary":"s","event_indices":[0]}]`},
			wantErrText: `unknown kind "maybe"`,
		},
		{
			name:        "LLM call failure wrapped",
			llmErr:      errors.New("rate limited"),
			wantErrText: "rate limited",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := time.Now()
			llm := &fakeLLM{responses: tt.responses, err: tt.llmErr}
			evts := []correlator.Event{mustEvent(t, "claude_code", "e1", "x", base)}

			_, err := correlator.Cluster(t.Context(), evts, nil, llm, correlator.TimeRange{
				Start: base.Add(-time.Hour), End: base.Add(time.Hour),
			}, 128000)

			require.Error(t, err)
			assert.ErrorContains(t, err, tt.wantErrText)
		})
	}
}
```

Add `"errors"` to the test file's import block.

- [ ] **Step 2: Run test to verify it passes**

Run: `env -u GOROOT -u GOPATH go test ./internal/correlator/... -v -run TestCluster_ResponseAndCallFailuresErrorLoudly`

Expected: `PASS` on all four subtests. If the "LLM call failure wrapped" case fails, check that
`Cluster`'s `llm.Complete` error branch uses `%w` (added in Task 2) so `assert.ErrorContains` can
see the wrapped message.

- [ ] **Step 3: Commit**

```bash
git add internal/correlator/correlator_test.go
git commit -m "Add response-parsing and LLM-failure error-case tests for Cluster"
```

---

## Task 4: Window/adjacency selection test (proves the prompt content, not just the result)

**Files:**
- Modify: `internal/correlator/correlator_test.go`

- [ ] **Step 1: Write the test**

Add to `internal/correlator/correlator_test.go`:

```go
func TestCluster_ExcludesNarrativesOutsideWindowAndNotAdjacent(t *testing.T) {
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	window := correlator.TimeRange{Start: base, End: base.Add(time.Hour)}

	existing := []correlator.Narrative{
		{ID: 1, Title: "adjacent-before", WindowStart: base.Add(-2 * time.Hour), WindowEnd: base},
		{ID: 2, Title: "overlapping", WindowStart: base.Add(30 * time.Minute), WindowEnd: base.Add(2 * time.Hour)},
		{ID: 3, Title: "far-away", WindowStart: base.Add(-24 * time.Hour), WindowEnd: base.Add(-12 * time.Hour)},
	}
	llm := &fakeLLM{responses: []string{"[]"}}

	_, err := correlator.Cluster(t.Context(), nil, existing, llm, window, 128000)

	require.NoError(t, err)
	require.Len(t, llm.prompts, 1)
	assert.Contains(t, llm.prompts[0], "adjacent-before")
	assert.Contains(t, llm.prompts[0], "overlapping")
	assert.NotContains(t, llm.prompts[0], "far-away")
}
```

- [ ] **Step 2: Run test to verify it passes**

Run: `env -u GOROOT -u GOPATH go test ./internal/correlator/... -v -run TestCluster_ExcludesNarrativesOutsideWindowAndNotAdjacent`

Expected: `PASS`. If it fails, check `filterAdjacentOrOverlapping`'s boundary condition — it should
treat exact-boundary touches (narrative's `WindowEnd == window.Start`) as adjacent, not excluded;
`n.WindowEnd.Before(window.Start)` (strictly before) is what makes that inclusive.

- [ ] **Step 3: Commit**

```bash
git add internal/correlator/correlator_test.go
git commit -m "Add window/adjacency selection test asserting actual prompt content"
```

---

## Task 5: Implement window bisection + irreducible-unit detection

**Files:**
- Modify: `internal/correlator/correlator.go`
- Modify: `internal/correlator/correlator_test.go`

- [ ] **Step 1: Write the failing tests**

Add to `internal/correlator/correlator_test.go`:

```go
func TestCluster_OversizedWindowSplitsAndCallsLLMPerHalf(t *testing.T) {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	// Two events spaced an hour apart so a time-midpoint split cleanly
	// separates them into two non-empty halves.
	evts := []correlator.Event{
		mustEvent(t, "claude_code", "e1", strings.Repeat("a", 4000), base),
		mustEvent(t, "claude_code", "e2", strings.Repeat("b", 4000), base.Add(time.Hour)),
	}
	llm := &fakeLLM{responses: []string{
		`[{"kind":"new","title":"first","summary":"s1","event_indices":[0]}]`,
		`[{"kind":"new","title":"second","summary":"s2","event_indices":[0]}]`,
	}}

	// contextWindowTokens sized so one event alone fits comfortably but
	// both together (in one prompt) don't.
	results, err := correlator.Cluster(t.Context(), evts, nil, llm, correlator.TimeRange{
		Start: base, End: base.Add(2 * time.Hour),
	}, 1200)

	require.NoError(t, err)
	require.Len(t, llm.prompts, 2, "expected one Complete call per split half, not one for the whole window")
	require.Len(t, results, 2)
	titles := []string{results[0].Title, results[1].Title}
	assert.ElementsMatch(t, []string{"first", "second"}, titles)
}

func TestCluster_SingleEventOverBudgetErrorsLoudlyWithoutCallingLLM(t *testing.T) {
	base := time.Now()
	evts := []correlator.Event{
		mustEvent(t, "claude_code", "e1", strings.Repeat("a", 10000), base),
	}
	llm := &fakeLLM{responses: []string{"[]"}}

	_, err := correlator.Cluster(t.Context(), evts, nil, llm, correlator.TimeRange{
		Start: base.Add(-time.Minute), End: base.Add(time.Minute),
	}, 100)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "e1")
	assert.Empty(t, llm.prompts, "no Complete call should be made for a doomed-to-fail oversized request")
}

func TestCluster_DegenerateBisectionOfIdenticalTimestampsErrorsLoudly(t *testing.T) {
	base := time.Now()
	// All events share the exact same timestamp, so a time-midpoint
	// bisection can never separate them into two non-empty halves.
	evts := []correlator.Event{
		mustEvent(t, "claude_code", "e1", strings.Repeat("a", 4000), base),
		mustEvent(t, "claude_code", "e2", strings.Repeat("b", 4000), base),
		mustEvent(t, "claude_code", "e3", strings.Repeat("c", 4000), base),
	}
	llm := &fakeLLM{responses: []string{"[]"}}

	_, err := correlator.Cluster(t.Context(), evts, nil, llm, correlator.TimeRange{
		Start: base.Add(-time.Minute), End: base.Add(time.Minute),
	}, 1000)

	require.Error(t, err)
}
```

Add `"strings"` to the test file's import block if not already present from an earlier task.

- [ ] **Step 2: Run tests to verify they fail**

Run: `env -u GOROOT -u GOPATH go test ./internal/correlator/... -v`

Expected: `FAIL` on all three — `clusterWithSplit` is still the Task 2 stub that always errors
"split-and-merge not yet implemented", so `TestCluster_OversizedWindowSplitsAndCallsLLMPerHalf`
fails on `require.NoError`, and the other two fail because the stub's error message doesn't
contain what they assert (or, for the degenerate case, because there's no real bisection logic to
detect the degenerate condition yet — the stub errors for the wrong reason).

- [ ] **Step 3: Write the minimal implementation**

Replace `clusterWithSplit`'s stub body in `internal/correlator/correlator.go`:

```go
// clusterWithSplit bisects window in half by time and recurses on each
// half, then merges the results (see mergeSplitResults). If bisection
// cannot make progress — the filtered event count doesn't decrease from
// the parent window to at least one non-empty half smaller than the whole
// — the window is irreducible: return a loud error naming what didn't fit
// rather than looping forever or silently truncating.
func clusterWithSplit(
	ctx context.Context,
	evts []Event,
	existing []Narrative,
	llm llmClient,
	window TimeRange,
	contextWindowTokens int,
	filtered []Event,
) ([]ClusterResult, error) {
	if len(filtered) <= 1 {
		return nil, irreducibleUnitError(window, filtered)
	}

	mid := window.Start.Add(window.End.Sub(window.Start) / 2)
	firstHalf := TimeRange{Start: window.Start, End: mid}
	secondHalf := TimeRange{Start: mid, End: window.End}

	firstFiltered := filterEventsInWindow(filtered, firstHalf)
	secondFiltered := filterEventsInWindow(filtered, secondHalf)

	if len(firstFiltered) == len(filtered) || len(secondFiltered) == len(filtered) {
		// Bisection made no progress (e.g. every remaining event shares the
		// same timestamp) — recursing again would loop forever on an
		// unchanged set. Treat as irreducible now.
		return nil, irreducibleUnitError(window, filtered)
	}

	firstResults, err := Cluster(ctx, evts, existing, llm, firstHalf, contextWindowTokens)
	if err != nil {
		return nil, err
	}

	secondResults, err := Cluster(ctx, evts, existing, llm, secondHalf, contextWindowTokens)
	if err != nil {
		return nil, err
	}

	return mergeSplitResults(ctx, llm, firstResults, secondResults)
}

// irreducibleUnitError reports a window that cannot be split further and
// still doesn't fit the configured budget — the "error loudly rather than
// silently drop" floor for this overflow path.
func irreducibleUnitError(window TimeRange, filtered []Event) error {
	if len(filtered) == 0 {
		return fmt.Errorf(
			"context budget exceeded for window [%s, %s) with existing-narrative context alone (no events): cannot split further",
			window.Start, window.End,
		)
	}

	return fmt.Errorf(
		"context budget exceeded for irreducible event %q (source=%s) in window [%s, %s): cannot split further",
		filtered[0].ExternalID, filtered[0].Source, window.Start, window.End,
	)
}
```

`mergeSplitResults` is implemented in Task 6; stub it for now so this compiles:

```go
// mergeSplitResults is implemented in Task 6 (extends-merge + LLM
// same-story merge at the split boundary).
func mergeSplitResults(_ context.Context, _ llmClient, first, second []ClusterResult) ([]ClusterResult, error) {
	return append(first, second...), nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `env -u GOROOT -u GOPATH go test ./internal/correlator/... -v`

Expected: `PASS` on `TestCluster_OversizedWindowSplitsAndCallsLLMPerHalf` and
`TestCluster_SingleEventOverBudgetErrorsLoudlyWithoutCallingLLM` and
`TestCluster_DegenerateBisectionOfIdenticalTimestampsErrorsLoudly`. Also re-run the full package
(`go test ./internal/correlator/... -v`) to confirm every earlier task's tests still pass — Task 2's
happy-path tests must not regress now that `Cluster` calls itself recursively for the split case.

- [ ] **Step 5: Commit**

```bash
git add internal/correlator/correlator.go internal/correlator/correlator_test.go
git commit -m "Implement window bisection with irreducible-unit detection"
```

---

## Task 6: Implement split-result merging (extends-merge + LLM same-story merge)

**Files:**
- Modify: `internal/correlator/correlator.go`
- Modify: `internal/correlator/correlator_test.go`

- [ ] **Step 1: Write the failing tests**

Add to `internal/correlator/correlator_test.go`:

```go
func TestCluster_ExtendsResultsSharingNarrativeIDMergeAcrossSplit(t *testing.T) {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	evts := []correlator.Event{
		mustEvent(t, "claude_code", "e1", strings.Repeat("a", 4000), base),
		mustEvent(t, "claude_code", "e2", strings.Repeat("b", 4000), base.Add(time.Hour)),
	}
	existing := []correlator.Narrative{
		{ID: 9, Title: "Ongoing", Summary: "s", WindowStart: base.Add(-time.Hour), WindowEnd: base},
	}
	llm := &fakeLLM{responses: []string{
		`[{"kind":"extends","narrative_id":9,"title":"Ongoing","summary":"s1","event_indices":[0]}]`,
		`[{"kind":"extends","narrative_id":9,"title":"Ongoing","summary":"s2","event_indices":[0]}]`,
	}}

	results, err := correlator.Cluster(t.Context(), evts, existing, llm, correlator.TimeRange{
		Start: base, End: base.Add(2 * time.Hour),
	}, 1200)

	require.NoError(t, err)
	require.Len(t, results, 1, "both halves' extends-9 results should merge into one")
	assert.Equal(t, correlator.ClusterExtends, results[0].Kind)
	assert.Equal(t, int64(9), results[0].NarrativeID)
	require.Len(t, results[0].Events, 2, "merged result should union both halves' events")
}

func TestCluster_AdjacentNewClustersMergeViaLLMWhenSameStory(t *testing.T) {
	tests := []struct {
		name            string
		sameStoryReply  string
		wantResultCount int
		wantTitle       string
		wantSummary     string
	}{
		{
			name:            "same story merges into one result",
			sameStoryReply:  `{"same_story":true,"title":"Merged story","summary":"unified summary"}`,
			wantResultCount: 1,
			wantTitle:       "Merged story",
			wantSummary:     "unified summary",
		},
		{
			name:            "distinct stories stay separate",
			sameStoryReply:  `{"same_story":false}`,
			wantResultCount: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
			evts := []correlator.Event{
				mustEvent(t, "claude_code", "e1", strings.Repeat("a", 4000), base),
				mustEvent(t, "claude_code", "e2", strings.Repeat("b", 4000), base.Add(time.Hour)),
			}
			llm := &fakeLLM{responses: []string{
				`[{"kind":"new","title":"Part 1","summary":"s1","event_indices":[0]}]`,
				`[{"kind":"new","title":"Part 2","summary":"s2","event_indices":[0]}]`,
				tt.sameStoryReply,
			}}

			results, err := correlator.Cluster(t.Context(), evts, nil, llm, correlator.TimeRange{
				Start: base, End: base.Add(2 * time.Hour),
			}, 1200)

			require.NoError(t, err)
			require.Len(t, llm.prompts, 3, "expected exactly one merge-boundary call after the two split-half calls")
			require.Len(t, results, tt.wantResultCount)

			if tt.wantResultCount == 1 {
				assert.Equal(t, tt.wantTitle, results[0].Title)
				assert.Equal(t, tt.wantSummary, results[0].Summary)
				require.Len(t, results[0].Events, 2)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `env -u GOROOT -u GOPATH go test ./internal/correlator/... -v`

Expected: `FAIL` on both this test's subtests and `TestCluster_ExtendsResultsSharingNarrativeIDMergeAcrossSplit`
— `mergeSplitResults` is still the Task 5 stub that only concatenates, so the extends-merge test
sees 2 results instead of 1, and the same-story test's "same story merges" subtest sees no third
`Complete` call at all (the "distinct stories" subtest may pass by coincidence — 2 results either
way — but re-verify it once the real implementation lands in Step 3, since the *reason* it passes
changes).

- [ ] **Step 3: Write the minimal implementation**

Replace `mergeSplitResults`'s stub body in `internal/correlator/correlator.go`:

```go
// mergeSplitResults combines two split halves' results: ClusterExtends
// results sharing a NarrativeID merge deterministically (union events, keep
// the earlier half's title/summary); at most one adjacent-boundary pair of
// ClusterNew results (the last of first, the first of second) gets one
// extra LLM call asking whether they're the same emerging story, merging
// on yes.
func mergeSplitResults(ctx context.Context, llm llmClient, first, second []ClusterResult) ([]ClusterResult, error) {
	merged := make([]ClusterResult, 0, len(first)+len(second))
	usedFromSecond := make(map[int]bool)

	for _, f := range first {
		if f.Kind != ClusterExtends {
			merged = append(merged, f)
			continue
		}

		mergedWithSecond := false
		for j, s := range second {
			if usedFromSecond[j] || s.Kind != ClusterExtends || s.NarrativeID != f.NarrativeID {
				continue
			}
			merged = append(merged, ClusterResult{
				Kind:        ClusterExtends,
				NarrativeID: f.NarrativeID,
				Title:       f.Title,
				Summary:     f.Summary,
				Events:      append(append([]Event{}, f.Events...), s.Events...),
			})
			usedFromSecond[j] = true
			mergedWithSecond = true
			break
		}
		if !mergedWithSecond {
			merged = append(merged, f)
		}
	}

	var remainingSecond []ClusterResult
	for j, s := range second {
		if !usedFromSecond[j] {
			remainingSecond = append(remainingSecond, s)
		}
	}

	// Adjacent-boundary same-story check: last ClusterNew of `merged` (from
	// first) vs first ClusterNew of remainingSecond.
	lastNewIdx := lastNewIndex(merged)
	firstNewIdx := firstNewIndex(remainingSecond)

	if lastNewIdx == -1 || firstNewIdx == -1 {
		return append(merged, remainingSecond...), nil
	}

	a, b := merged[lastNewIdx], remainingSecond[firstNewIdx]

	sameStory, mergedResult, err := checkSameStory(ctx, llm, a, b)
	if err != nil {
		return nil, err
	}

	if !sameStory {
		return append(merged, remainingSecond...), nil
	}

	merged[lastNewIdx] = mergedResult
	remainingSecond = append(remainingSecond[:firstNewIdx], remainingSecond[firstNewIdx+1:]...)

	return append(merged, remainingSecond...), nil
}

func lastNewIndex(results []ClusterResult) int {
	for i := len(results) - 1; i >= 0; i-- {
		if results[i].Kind == ClusterNew {
			return i
		}
	}
	return -1
}

func firstNewIndex(results []ClusterResult) int {
	for i, r := range results {
		if r.Kind == ClusterNew {
			return i
		}
	}
	return -1
}

// sameStoryResponse is the wire shape of the merge-boundary judgment call.
type sameStoryResponse struct {
	SameStory bool   `json:"same_story"`
	Title     string `json:"title"`
	Summary   string `json:"summary"`
}

const sameStorySystemPrompt = `You will be shown two narrative clusters that sit on either side of a time-window split. Decide whether they describe the same emerging story (the split point fell in the middle of one continuous narrative) or two distinct stories. Return ONLY a JSON object, no prose, no markdown fences: {"same_story": true|false, "title": "...", "summary": "..."} — title and summary are only meaningful when same_story is true (the unified title/summary for the merged cluster); omit or ignore them when false.`

// checkSameStory asks the model whether a and b (both ClusterNew) are the
// same emerging story. On yes, returns the merged ClusterResult (unioned
// events, model-provided title/summary). On no, mergedResult is the zero
// value and must be ignored.
func checkSameStory(ctx context.Context, llm llmClient, a, b ClusterResult) (bool, ClusterResult, error) {
	userPrompt := fmt.Sprintf(
		"Cluster A: title=%q summary=%q\nCluster B: title=%q summary=%q",
		a.Title, a.Summary, b.Title, b.Summary,
	)

	raw, err := llm.Complete(ctx, sameStorySystemPrompt, userPrompt)
	if err != nil {
		return false, ClusterResult{}, fmt.Errorf("checking same-story merge for split boundary: %w", err)
	}

	var resp sameStoryResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		return false, ClusterResult{}, fmt.Errorf("parsing same-story response %q: %w", raw, err)
	}

	if !resp.SameStory {
		return false, ClusterResult{}, nil
	}

	return true, ClusterResult{
		Kind:    ClusterNew,
		Title:   resp.Title,
		Summary: resp.Summary,
		Events:  append(append([]Event{}, a.Events...), b.Events...),
	}, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `env -u GOROOT -u GOPATH go test ./internal/correlator/... -v`

Expected: `PASS` on `TestCluster_ExtendsResultsSharingNarrativeIDMergeAcrossSplit` and both
subtests of `TestCluster_AdjacentNewClustersMergeViaLLMWhenSameStory`, and every earlier task's
test still passes. Run the whole package once more explicitly: `env -u GOROOT -u GOPATH go test
./internal/correlator/... -v | tail -40` and confirm no `FAIL` anywhere.

- [ ] **Step 5: Commit**

```bash
git add internal/correlator/correlator.go internal/correlator/correlator_test.go
git commit -m "Implement split-result merging: extends-merge and LLM same-story merge"
```

---

## Task 7: Full regression + lint

**Files:** none (verification only)

- [ ] **Step 1: Run the full offline test suite**

Run: `env -u GOROOT -u GOPATH go test ./... -v 2>&1 | tail -60`

Expected: every package reports `ok`, including the new `internal/correlator`.

- [ ] **Step 2: Run `go vet`**

Run: `env -u GOROOT -u GOPATH go vet ./...`

Expected: no output, exit code 0.

- [ ] **Step 3: Run the full lint+test gate**

Run: `earthly +reviewable` (or `earth +reviewable` — see `docs/go-conventions.md`'s note on which
binary name is on `PATH`; if neither `earthly` nor `earth` works in this environment, fall back to
`env -u GOROOT -u GOPATH golangci-lint run ./...` directly).

Expected: `0 issues` from `golangci-lint`, all tests pass, overall `SUCCESS`. Pay particular
attention to:
- `gocognit`/cyclomatic-complexity-adjacent findings on `Cluster`/`clusterWithSplit`/
  `mergeSplitResults` — these are the most branch-heavy functions in this package; if lint flags
  one, prefer extracting a named helper over suppressing the finding.
- Doc-comment coverage on every exported identifier (`TimeRange`, `Narrative`, `ClusterKind`,
  `ClusterNew`/`ClusterExtends`, `ClusterResult`, `Cluster`, `Event`) — all should already have
  one from the tasks above, but confirm lint agrees.

If lint finds anything, fix it minimally and re-run this step before moving on — do not commit
past a red `+reviewable`.

- [ ] **Step 4: Commit if Step 3 required fixes**

If `earthly +reviewable` was clean on the first run, skip this step. Otherwise:

```bash
git add -A
git commit -m "Fix lint findings in internal/correlator"
```

---

## Task 8: Update the phase-1 spec's status for this slice

**Files:**
- Modify: `docs/superpowers/specs/2026-08-11-phase1-correlator-design.md`

- [ ] **Step 1: Mark slice 2 as landed**

Open `docs/superpowers/specs/2026-08-11-phase1-correlator-design.md`. Find the "Implementation
slices" section's second item:

```
2. **`internal/correlator`** (compute only) — `Cluster` against `claude_code` events, unit-tested
   with a fake LLM client, including the split-by-time-and-merge overflow path (needed from this
   slice on — it's part of `Cluster`'s own contract, not an add-on). No persistence, no CLI
   command yet.
```

Replace with:

```
2. **`internal/correlator`** (compute only) — ✅ landed. `Cluster` against `claude_code` events,
   unit-tested with a fake LLM client, including the split-by-time-and-merge overflow path. No
   persistence, no CLI command yet. See
   `docs/superpowers/specs/2026-08-12-correlator-cluster-design.md` for this slice's design and
   `docs/superpowers/plans/2026-08-12-correlator-cluster-implementation.md` for the implementation
   plan.
```

- [ ] **Step 2: Commit**

```bash
git add docs/superpowers/specs/2026-08-11-phase1-correlator-design.md
git commit -m "Mark phase-1 slice 2 (internal/correlator.Cluster) as landed"
```

---

## Verification checklist (spec coverage for this slice)

- [x] `Cluster`'s signature matches the design doc exactly — Task 1/2.
- [x] Window filtering + adjacency/overlap selection for existing narratives — Task 2 (logic),
  Task 4 (proves actual prompt content, not just final result).
- [x] Index-based prompt/response contract, strict-JSON-only, no `response_format` SDK feature
  used — Task 2.
- [x] Malformed response / out-of-range index / unknown kind all error loudly with raw response
  attached — Task 2 (implementation), Task 3 (tests).
- [x] LLM call failures wrapped with `%w`, never silently swallowed — Task 2 (implementation),
  Task 3 (test).
- [x] Split-by-time-and-merge overflow path, including the degenerate-bisection
  infinite-recursion guard — Task 5.
- [x] Extends-merge (same `NarrativeID` across split halves) and LLM same-story merge for
  adjacent `new` clusters at the split boundary — Task 6.
- [x] Irreducible-unit floor: loud error, never silent truncation, no wasted `Complete` call on a
  doomed-to-fail request — Task 5.
- [x] No store access anywhere in this package — verified by inspection (no `internal/store`
  import in `correlator.go`) at Task 7's full regression.
- [x] No `internal/clients/openai` import — the `llmClient` interface is declared locally and
  `*openai.Client` satisfies it structurally; verified by inspection at Task 7.

Not in scope for this slice (deferred to later slices per the phase-1 spec's own sequencing):
`Persist`, real `narratives`/`narrative_events` table writes, tail-summarization
(`config.Correlator.{TailSummarizeThresholdTokens, RecentEventsKept}`), the lease lock, any CLI
command, `internal/reconciler` and everything downstream of a persisted narrative.
