# `unjira triage` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** An interactive review surface over the actions queue where a reviewer can approve, reject, reword, retarget, and — most importantly — **re-cluster** proposed actions before anything is written to Jira.

**Architecture:** A new `internal/triage` package holds the review session as a state machine that never touches a terminal; `cmd/unjira/triage.go` is a thin I/O shell implementing a `Prompter` interface. Merge and split ride on the existing `correlator.Cluster`/`Persist` primitives rather than new operations, unlocked by replacing `Cluster`'s coarse "all existing narratives are frozen" rule with a per-event-link watermark. Dispositions accumulate in memory; one final confirmation hands the approved set to `gate.Applier`.

**Tech Stack:** Go 1.26, `modernc.org/sqlite`, Kong (CLI), testify, Earthly.

**Design:** `docs/superpowers/specs/2026-08-28-triage-design.md` — read it first. The invariant in Task 1 is the load-bearing idea; every restructure depends on it.

---

## Read before starting

- `CLAUDE.md` — architecture invariants, and **"Keep the docs true in the PR that changes the code."** Task 12 is not optional.
- `docs/go-conventions.md` — error handling, testing idioms, package layout.
- Doc comments in this repo explain **why**, citing the concrete failure that motivated the code. Match that density; it is unusually high for Go and deliberate.
- **TDD is required.** Failing test first, every task.

### Tooling traps, learned the hard way here

- `golangci-lint` via the asdf shim **silently no-ops** (prints "No version is set", exits 0). Always use the absolute path: `~/.asdf/installs/golang/1.25.7/bin/golangci-lint`. It must report `0 issues.`
- Always append `; echo "exit=$?"` to bash commands. Piping through `head`/`tail` swallows exit codes and has produced a false "BUILD OK" in this project.
- Report real test counts. Baseline on `main` at the time of writing: **602 assertions (474 top-level + 128 subtests), 0 skipped.**

---

## File structure

**Create:**

| File | Responsibility |
|---|---|
| `internal/store/eligibility.go` | The commit-watermark query: which of a narrative's event links are frozen vs eligible. One responsibility, kept out of the already-large `store.go`. |
| `internal/store/eligibility_test.go` | Table-driven invariant tests — the highest-value tests in this slice. |
| `internal/triage/triage.go` | `Session` state machine: batch, dispositions, `Prompter` seam, `Commit`. |
| `internal/triage/restructure.go` | Merge/split/retarget operations. Separate file because these are the risky mutations and deserve to be read on their own. |
| `internal/triage/triage_test.go` | Session tests driven by a scripted prompter. |
| `internal/triage/restructure_test.go` | Restructure tests, including the mixed-state case. |
| `cmd/unjira/triage.go` | Kong command + terminal `Prompter` implementation. No logic. |
| `cmd/unjira/triage_test.go` | Command-level: flag parsing, `--auto-approve` still respects all three gates. |

**Modify:**

| File | Change |
|---|---|
| `internal/correlator/correlator.go` | `buildClusterPrompt` partitions by eligibility instead of freezing all existing narratives; `Narrative` gains an eligible-events field. |
| `internal/reconciler/draft.go` | Add `Redraft` — `draft` plus reviewer feedback in the prompt. |
| `internal/store/store.go` | Add `RemoveNarrativeIssue` (the one genuinely new mutation). |
| `cmd/unjira/main.go` | Register `triage` in the CLI struct. |
| `README.md` | CLI surface + Status section. |
| `docs/superpowers/specs/2026-08-11-phase1-correlator-design.md` | Mark slice 6 complete. |
| `docs/superpowers/specs/2026-08-28-triage-design.md` | `Status: design` → landed, with evidence. |

**Task order rationale:** Task 1 (the invariant) first because Tasks 2 and 6 both depend on it. Tasks 2–4 are independent seams — the correlator prompt change, `Redraft`, and the store delete — and can be done in any order. Task 5 (Session) needs 3. Task 6 (restructures) needs 1, 2, and 4. Tasks 7–8 wire the CLI. Tasks 9–12 are verification and docs.

---

### Task 1: The commit watermark — which event links are eligible

The invariant everything else rests on: **an event linked to a narrative before that narrative's last committed action is frozen; everything else is eligible for reshuffling.**

No schema change. `narrative_events.linked_at` and `actions.executed_at` are both written with `strftime('%Y-%m-%dT%H:%M:%fZ')` — verified identical, and the same property `DeltaEvents` already depends on (see the `actions.created_at` schema comment for why mixing `%f` with `%S` silently inverts a lexical comparison).

**Files:**
- Create: `internal/store/eligibility.go`
- Create: `internal/store/eligibility_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/store/eligibility_test.go`:

```go
package store_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/store"
)

// seedNarrativeWithEvents creates a narrative, inserts n events, links them,
// and returns the narrative id plus the linked event ids in order.
func seedNarrativeWithEvents(t *testing.T, s *store.Store, count int) (int64, []int64) {
	t.Helper()

	base := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)

	nid, err := s.InsertNarrative(base, base.Add(time.Hour), "t", "s")
	require.NoError(t, err)

	ids := make([]int64, 0, count)
	for i := range count {
		e := events.NewEvent("claude_code", fmt.Sprintf("elig:%d", i), base.Add(time.Duration(i)*time.Minute), "work")
		_, err := s.InsertEvent(e)
		require.NoError(t, err)

		eid, err := s.EventIDByExternalID("claude_code", fmt.Sprintf("elig:%d", i))
		require.NoError(t, err)

		require.NoError(t, s.AddNarrativeEvents(nid, []int64{eid}))
		ids = append(ids, eid)
	}

	return nid, ids
}

// TestEligibleEventIDs_NeverCommittedIsFullyEligible: with no applied action,
// there is no watermark, so nothing is frozen.
func TestEligibleEventIDs_NeverCommittedIsFullyEligible(t *testing.T) {
	s := openStore(t)
	nid, ids := seedNarrativeWithEvents(t, s, 3)

	got, err := s.EligibleEventIDs(nid)

	require.NoError(t, err)
	assert.ElementsMatch(t, ids, got, "with no committed action every link is eligible")
}

// TestEligibleEventIDs_FullyCommittedIsFullyFrozen: an applied action stamped
// AFTER every link freezes all of them.
func TestEligibleEventIDs_FullyCommittedIsFullyFrozen(t *testing.T) {
	s := openStore(t)
	nid, _ := seedNarrativeWithEvents(t, s, 3)

	id, err := s.InsertAction(store.ActionRow{
		NarrativeID: nid, Type: "comment", IssueKey: "PROJ-1",
		Payload: `{"body":"posted"}`, Status: "proposed",
	})
	require.NoError(t, err)
	// applied stamps executed_at = now(), which is after every linked_at above.
	require.NoError(t, s.UpdateActionStatus(id, "applied"))

	got, err := s.EligibleEventIDs(nid)

	require.NoError(t, err)
	assert.Empty(t, got, "every link predates the commit, so none may move")
}

// TestEligibleEventIDs_MixedStateFreezesOnlyThePast is the case that drove the
// design. A narrative can hold an applied AND a proposed action: watch applies
// A, the next tick proposes B for the same narrative. Freezing the whole
// narrative would refuse a merge — but the reviewer's objection is precisely
// that the events behind B do not belong with the events behind A. So the unit
// of freezing is the event link, not the narrative.
func TestEligibleEventIDs_MixedStateFreezesOnlyThePast(t *testing.T) {
	s := openStore(t)
	nid, before := seedNarrativeWithEvents(t, s, 2)

	id, err := s.InsertAction(store.ActionRow{
		NarrativeID: nid, Type: "comment", IssueKey: "PROJ-1",
		Payload: `{"body":"posted"}`, Status: "proposed",
	})
	require.NoError(t, err)
	require.NoError(t, s.UpdateActionStatus(id, "applied"))

	// linked_at uses millisecond precision, so sleep past the commit instant
	// rather than racing it — two links inside the same millisecond would be
	// indistinguishable and make this test flaky rather than wrong.
	time.Sleep(5 * time.Millisecond)

	after := events.NewEvent("claude_code", "elig:after", time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC), "later work")
	_, err = s.InsertEvent(after)
	require.NoError(t, err)
	afterID, err := s.EventIDByExternalID("claude_code", "elig:after")
	require.NoError(t, err)
	require.NoError(t, s.AddNarrativeEvents(nid, []int64{afterID}))

	got, err := s.EligibleEventIDs(nid)

	require.NoError(t, err)
	assert.Equal(t, []int64{afterID}, got,
		"only the link made after the commit may move; the committed ones stay")
	assert.NotContains(t, got, before[0], "a committed event must never be eligible")
}
```

- [ ] **Step 2: Run the test to verify it fails**

```
cd /Users/jonathan.ogilvie/workspace/unjira/.claude/worktrees/triage
go test ./internal/store/ -run TestEligibleEventIDs -v 2>&1 | tail -20; echo "exit=$?"
```

Expected: compile failure — `s.EligibleEventIDs undefined`. If `LinkNarrativeEvents` or `EventIDByExternalID` also come back undefined, check their real names with `grep -n "func (s \*Store)" internal/store/store.go | grep -i "link\|EventID"` and fix the test to match rather than inventing seams.

- [ ] **Step 3: Write the implementation**

Create `internal/store/eligibility.go`:

```go
package store

import "fmt"

// EligibleEventIDs returns the narrative's event links that may still be
// reshuffled by a reviewer-driven re-cluster: those linked AFTER the
// narrative's most recent committed action.
//
// The unit is the event link, not the narrative, and that distinction is the
// whole design. A narrative can hold both an applied and a proposed action —
// watch applies A, the next tick proposes B against the same narrative. If a
// single applied action froze the entire narrative, triage would refuse to
// merge or split it. But the reviewer's objection in that situation is
// precisely that the events behind B do not belong with the events behind A,
// so a narrative-level freeze would reject the correction exactly when the
// correction is right.
//
// What stays frozen is what a posted comment already described: an unposted
// comment can be redrafted, a posted one cannot be unposted. The watermark for
// "the past" is therefore the last commit, not the narrative's existence.
//
// The comparison is lexical on TEXT columns and safe because linked_at and
// executed_at share the identical strftime('%Y-%m-%dT%H:%M:%fZ') format —
// deliberately, and the same property DeltaEvents depends on. Mixing %f with
// %S would silently invert it ('.' 0x2E sorts before 'Z' 0x5A), which is why
// the actions.created_at schema comment spells that out.
//
// A narrative with no committed action has no watermark, so every link is
// eligible — expressed as "max(executed_at) IS NULL" rather than a separate
// query, so there is one code path rather than two that could disagree.
func (s *Store) EligibleEventIDs(narrativeID int64) ([]int64, error) {
	rows, err := s.db.Query(
		`SELECT ne.event_id
		 FROM narrative_events ne
		 WHERE ne.narrative_id = ?
		   AND (
		     (SELECT max(executed_at) FROM actions
		       WHERE narrative_id = ? AND executed_at IS NOT NULL) IS NULL
		     OR ne.linked_at > (SELECT max(executed_at) FROM actions
		       WHERE narrative_id = ? AND executed_at IS NOT NULL)
		   )
		 ORDER BY ne.event_id`,
		narrativeID, narrativeID, narrativeID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying eligible event links for narrative %d: %w", narrativeID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning eligible event id for narrative %d: %w", narrativeID, err)
		}
		out = append(out, id)
	}

	return out, rows.Err()
}
```

- [ ] **Step 4: Run the test to verify it passes**

```
go test ./internal/store/ -run TestEligibleEventIDs -v 2>&1 | tail -20; echo "exit=$?"
```

Expected: all three PASS.

- [ ] **Step 5: Drill the watermark — an inverted comparison must fail loudly**

This is the most important drill in the slice. An inverted watermark freezes the wrong half: it would look like it protects committed work while actually exposing it, which is worse than no check at all.

Temporarily change `ne.linked_at >` to `ne.linked_at <` in `eligibility.go`, then:

```
go test ./internal/store/ -run TestEligibleEventIDs -v 2>&1 | tail -25; echo "exit=$?"
```

Expected — this exact output, captured by running it while writing this plan:

```
--- FAIL: TestEligibleEventIDs_FullyCommittedIsFullyFrozen
        Error: Should be empty, but was [1 2 3]
--- FAIL: TestEligibleEventIDs_MixedStateFreezesOnlyThePast
        Error: []int64{1, 2} should not contain 1
```

The second message is the one that matters: a **committed** event appearing in the eligible set. Then restore `>` and re-run to confirm green.

> **Note for the implementer:** every code block in this task was extracted, compiled (`go vet`), and run against the real store while this plan was written — all three tests pass as written, and the drill produces the output above. If something does not compile for you, suspect a merge with `main` rather than the plan.

- [ ] **Step 6: Commit**

```bash
git add internal/store/eligibility.go internal/store/eligibility_test.go
git commit -m "store: the commit watermark — which event links may be reshuffled

An event linked to a narrative before that narrative's last committed
action is frozen; everything else is eligible. The unit is the event
LINK, not the narrative, because a narrative can hold both an applied
and a proposed action, and a narrative-level freeze would refuse a
reviewer's merge precisely when the merge is right.

No schema change: linked_at and executed_at already share the identical
%f strftime, the same property DeltaEvents depends on.

Drilled: inverting the comparison fails the mixed-state and
fully-committed tests, which is the failure mode that matters — an
inverted watermark looks protective while exposing committed work."
```

---

### Task 2: `Cluster` partitions existing narratives by eligibility

Today `buildClusterPrompt` prints **every** existing narrative's events under `CONTEXT ONLY`, without indices, so the model structurally cannot reassign them. That is a blunt form of Task 1's invariant: *all* existing narratives frozen.

This task refines it: a narrative's **eligible** events move into the numbered, assignable section; its **frozen** events stay in the context section. `watch`'s behaviour is unchanged in practice — by the time it runs, prior narratives normally have committed actions, so everything stays frozen — but that becomes a *consequence* of the invariant rather than a separate rule. And `triage` then needs no special re-clustering mode at all.

**Files:**
- Modify: `internal/correlator/correlator.go` (`Narrative` struct ~line 47; `buildClusterPrompt` ~line 263; `clusterSystemPrompt` ~line 300)
- Modify: `internal/pipeline/narrate.go:199` (the single `correlator.Narrative{...}` construction site)
- Test: `internal/correlator/correlator_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/correlator/correlator_test.go`:

```go
// TestCluster_EligibleNarrativeEventsAreAssignable is the prompt-level half of
// the commit watermark (store.EligibleEventIDs is the data half). A narrative's
// eligible events must appear in the NUMBERED section the model may assign
// from; its frozen events must stay in the context section without indices.
//
// This is the safety property, so it is asserted on prompt content rather than
// on a return value: if a frozen event ever gained an index, the model could
// reparent work a posted comment already described.
func TestCluster_EligibleNarrativeEventsAreAssignable(t *testing.T) {
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	window := correlator.TimeRange{Start: base, End: base.Add(time.Hour)}

	existing := []correlator.Narrative{{
		ID: 9, Title: "Cache rework", Summary: "Reworking the shared cache",
		WindowStart: base.Add(-2 * time.Hour), WindowEnd: base,
		Events: []correlator.Event{
			mustEvent(t, "github", "pr-412", "FROZEN committed work", base.Add(-2*time.Hour)),
		},
		EligibleEvents: []correlator.Event{
			mustEvent(t, "github", "pr-500", "ELIGIBLE uncommitted work", base.Add(-1*time.Hour)),
		},
	}}
	inWindow := []correlator.Event{
		mustEvent(t, "claude_code", "e1", "in-window work", base.Add(5*time.Minute)),
	}
	llm := &fakeLLM{responses: []string{"[]"}}

	_, _, err := correlator.Cluster(t.Context(), inWindow, existing, llm, window, 128000)
	require.NoError(t, err)
	require.Len(t, llm.prompts, 1)

	prompt := llm.prompts[0]
	toCluster, context, found := strings.Cut(prompt, "Existing narratives (CONTEXT ONLY):")
	require.True(t, found, "the prompt must still have both labeled sections")

	assert.Contains(t, toCluster, "ELIGIBLE uncommitted work",
		"an eligible narrative event must be numbered and assignable")
	assert.NotContains(t, context, "ELIGIBLE uncommitted work",
		"an eligible event must not ALSO appear as context — it would be listed twice")

	assert.Contains(t, context, "FROZEN committed work",
		"a frozen event stays in the context section")
	assert.NotContains(t, toCluster, "FROZEN committed work",
		"a frozen event must never be assignable: a posted comment already describes it")
}

// TestCluster_NoEligibleEventsMatchesTodaysBehaviour pins the compatibility
// half: with EligibleEvents empty — every existing narrative fully committed,
// which is watch's normal case — the prompt is exactly what it was before this
// change. This is what makes "the invariant subsumes the old rule" a checked
// claim rather than an assertion.
func TestCluster_NoEligibleEventsMatchesTodaysBehaviour(t *testing.T) {
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	window := correlator.TimeRange{Start: base, End: base.Add(time.Hour)}

	existing := []correlator.Narrative{{
		ID: 9, Title: "Cache rework", Summary: "Reworking the shared cache",
		WindowStart: base.Add(-2 * time.Hour), WindowEnd: base,
		Events: []correlator.Event{
			mustEvent(t, "github", "pr-412", "PR #412 add cache layer", base.Add(-2*time.Hour)),
		},
		// EligibleEvents deliberately nil.
	}}
	inWindow := []correlator.Event{
		mustEvent(t, "claude_code", "e1", "debugging cache eviction", base.Add(5*time.Minute)),
	}
	llm := &fakeLLM{responses: []string{"[]"}}

	_, _, err := correlator.Cluster(t.Context(), inWindow, existing, llm, window, 128000)
	require.NoError(t, err)

	prompt := llm.prompts[0]
	toCluster, _, _ := strings.Cut(prompt, "Existing narratives (CONTEXT ONLY):")
	assert.NotContains(t, toCluster, "PR #412 add cache layer",
		"with nothing eligible, no existing-narrative event is assignable")
	assert.Contains(t, prompt, "CONTEXT ONLY")
}
```

Add `"strings"` to that file's imports if it is not already there.

- [ ] **Step 2: Run the test to verify it fails**

```
go test ./internal/correlator/ -run 'TestCluster_EligibleNarrativeEventsAreAssignable|TestCluster_NoEligibleEventsMatchesTodaysBehaviour' -v 2>&1 | tail -20; echo "exit=$?"
```

Expected: compile failure — `unknown field EligibleEvents in struct literal`.

- [ ] **Step 3: Add the field to `Narrative`**

In `internal/correlator/correlator.go`, inside `type Narrative struct`, directly after the `Events` field:

```go
	// EligibleEvents are this narrative's events that a reviewer-driven
	// re-cluster may reassign: those linked after the narrative's last
	// committed action (see store.EligibleEventIDs). They are rendered in the
	// NUMBERED section alongside in-window events, so the model can move them;
	// Events above stay context-only and cannot be reassigned.
	//
	// Empty for every routine watch pass, because by the time watch runs a
	// prior narrative normally has a committed action. That is why this change
	// refines the old "all existing narratives are frozen" rule rather than
	// weakening it: the freeze now ends at each narrative's commit watermark
	// instead of at its existence, and nothing about watch's behaviour moves.
	//
	// A caller must not put the same event in both slices. Doing so would list
	// it twice in one prompt and invite the model to assign a frozen event by
	// its index — see hydrateContextNarratives, which partitions rather than
	// duplicating.
	EligibleEvents []Event
```

- [ ] **Step 4: Render eligible events in the assignable section**

In `buildClusterPrompt`, replace the "Events to cluster" loop and the context loop. The whole function becomes:

```go
func buildClusterPrompt(evts []Event, existing []Narrative, learnedRules []rules.Rule) (systemPrompt, userPrompt string) {
	systemPrompt = clusterSystemPrompt
	if rendered := rules.Render(learnedRules); rendered != "" {
		systemPrompt += "\n\n" + rendered
	}

	// assignable is the in-window events plus every existing narrative's
	// eligible events, sharing one index space: the model assigns by index and
	// has no reason to care which bucket an event came from.
	assignable := make([]Event, 0, len(evts))
	assignable = append(assignable, evts...)
	for _, n := range existing {
		assignable = append(assignable, n.EligibleEvents...)
	}

	var b strings.Builder
	b.WriteString("Events to cluster:\n")
	for i, e := range assignable {
		// %q on Summary (not %s): event summaries come from arbitrary
		// upstream session/commit text, so an embedded newline or a
		// fabricated "N. [source] ..." line could otherwise inject a
		// spurious entry into this numbered list as the model reads it.
		// Quoting escapes those, matching the %q already used for the
		// narrative fields below.
		fmt.Fprintf(&b, "%d. [%s] %q (occurred_at=%s)\n", i, e.Source, e.Summary, e.OccurredAt.Format(time.RFC3339))
	}

	b.WriteString("\nExisting narratives (CONTEXT ONLY):\n")
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
```

- [ ] **Step 4b: Resolve indices against the SAME slice — the dangerous part**

The prompt now numbers a *combined* slice, but `parseClusterResponse(raw, filtered)` at `correlator.go:190` resolves `event_indices` against `filtered` — the in-window events **only**. Verified by reading it: `if idx < 0 || idx >= len(evts) { ...out of range... }` then `evts[idx]`. So without this step an eligible event's index either errors as out-of-range or, worse, silently resolves to a *different* event.

Build the combined slice once in `Cluster` and hand it to both. At `correlator.go:175`, replace:

```go
	systemPrompt, userPrompt := buildClusterPrompt(filtered, relevant, o.rules)
```

with:

```go
	// assignable shares one index space between the prompt and the parser:
	// buildClusterPrompt numbers this exact slice, and parseClusterResponse
	// resolves event_indices against it. They MUST be the same slice — passing
	// `filtered` to the parser while the prompt numbered a longer slice would
	// make an eligible event's index resolve to the wrong event, silently.
	assignable := assignableEvents(filtered, relevant)
	systemPrompt, userPrompt := buildClusterPrompt(assignable, relevant, o.rules)
```

and at `correlator.go:190`, change `parseClusterResponse(raw, filtered)` to `parseClusterResponse(raw, assignable)`.

Add the helper next to `buildClusterPrompt`, and make `buildClusterPrompt` take the already-combined slice (drop the `assignable := ...` lines from Step 4's version, keeping `for i, e := range evts`):

```go
// assignableEvents is the single index space Cluster's prompt numbers and its
// response parser resolves against: the in-window events, then every existing
// narrative's eligible events in `existing` order.
//
// One function so the two sides cannot disagree. They were separate call sites
// (buildClusterPrompt(filtered,...) and parseClusterResponse(raw, filtered))
// before eligible narrative events became assignable, and keeping them separate
// would have meant an eligible event's index resolving to a different event —
// silent misattribution rather than a loud error.
func assignableEvents(inWindow []Event, existing []Narrative) []Event {
	out := make([]Event, 0, len(inWindow))
	out = append(out, inWindow...)
	for _, n := range existing {
		out = append(out, n.EligibleEvents...)
	}

	return out
}
```

Add a test pinning the shared index space:

```go
// TestCluster_EligibleEventIndexResolvesToTheRightEvent guards the one silent
// failure this change could introduce: the prompt numbering a combined slice
// while the parser resolves against the in-window slice alone. The model here
// picks index 1 — the eligible narrative event, not any in-window event — so a
// mismatched index space would either error or return the wrong event.
func TestCluster_EligibleEventIndexResolvesToTheRightEvent(t *testing.T) {
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	window := correlator.TimeRange{Start: base, End: base.Add(time.Hour)}

	existing := []correlator.Narrative{{
		ID: 9, Title: "Cache rework", Summary: "s",
		WindowStart: base.Add(-2 * time.Hour), WindowEnd: base,
		EligibleEvents: []correlator.Event{
			mustEvent(t, "github", "pr-500", "THE ELIGIBLE ONE", base.Add(-1*time.Hour)),
		},
	}}
	inWindow := []correlator.Event{
		mustEvent(t, "claude_code", "e1", "in-window work", base.Add(5*time.Minute)),
	}
	// index 0 = in-window, index 1 = the eligible narrative event.
	llm := &fakeLLM{responses: []string{
		`[{"kind":"extends","narrative_id":9,"title":"t","summary":"s","event_indices":[1]}]`,
	}}

	got, _, err := correlator.Cluster(t.Context(), inWindow, existing, llm, window, 128000)

	require.NoError(t, err, "index 1 must be in range: the parser sees the combined slice")
	require.Len(t, got, 1)
	require.Len(t, got[0].Events, 1)
	assert.Equal(t, "THE ELIGIBLE ONE", got[0].Events[0].Summary,
		"index 1 must resolve to the eligible narrative event, not an in-window one")
}
```

Drill it: pass `filtered` to `parseClusterResponse` instead of `assignable`. Expected failure, captured by running it while writing this plan:

```
event_indices value 1 out of range [0,1)
```

Record the output, then restore.

> **Verified while writing:** Task 2's correlator changes were implemented against real code, the index-space test passed, the drill produced the error above, and `go test ./...` stayed green — so "watch is unaffected" is a checked claim, not an assumption. The scratch implementation was then reverted; the plan is what remains.

- [ ] **Step 5: Run the tests**

```
go test ./internal/correlator/ 2>&1 | tail -20; echo "exit=$?"
```

Expected: the two new tests PASS, and every pre-existing correlator test still passes — especially `TestCluster_ContextIsAdjacentNarrativesOnly` (which asserts `CONTEXT ONLY` is present) and `TestCluster_UsesNarrativeContextEventsForExtendsDecision`.

- [ ] **Step 6: Update the one construction site**

`internal/pipeline/narrate.go`'s `hydrateContextNarratives` must partition rather than duplicate. Replace its loop body:

```go
	out := make([]correlator.Narrative, 0, len(rows))
	for _, row := range rows {
		contextEvents, err := s.NarrativeEventsForContext(row.ID)
		if err != nil {
			return nil, fmt.Errorf("hydrating context events for narrative %d: %w", row.ID, err)
		}

		// Partition by the commit watermark: eligible events become assignable,
		// the rest stay context-only. An event must land in exactly one slice —
		// putting it in both would list it twice in the prompt and let the model
		// assign a frozen event by index.
		eligibleIDs, err := s.EligibleEventIDs(row.ID)
		if err != nil {
			return nil, fmt.Errorf("resolving eligible events for narrative %d: %w", row.ID, err)
		}
		eligible := make(map[int64]bool, len(eligibleIDs))
		for _, id := range eligibleIDs {
			eligible[id] = true
		}

		var frozen, assignable []correlator.Event
		for _, e := range contextEvents {
			id, err := s.EventIDByExternalID(e.Source, e.ExternalID)
			if err != nil {
				return nil, fmt.Errorf("resolving event id for %s/%s: %w", e.Source, e.ExternalID, err)
			}
			if eligible[id] {
				assignable = append(assignable, e)
			} else {
				frozen = append(frozen, e)
			}
		}

		out = append(out, correlator.Narrative{
			ID:             row.ID,
			WindowStart:    row.WindowStart,
			WindowEnd:      row.WindowEnd,
			Title:          row.Title,
			Summary:        row.Summary,
			IssueKey:       row.IssueKey,
			Confidence:     row.Confidence,
			Status:         row.Status,
			Events:         frozen,
			EligibleEvents: assignable,
		})
	}
```

**If `events.Event` has no `ExternalID` field**, check its real shape (`grep -n "type Event struct" -A 12 internal/events/events.go`) and use whatever `EventIDByExternalID` actually needs. Do not invent a field.

- [ ] **Step 7: Full correlator + pipeline suite**

```
go test ./internal/correlator/ ./internal/pipeline/ 2>&1 | tail -10; echo "exit=$?"
```

Expected: all pass. A failure in `internal/pipeline` here most likely means `watch`'s behaviour *did* change — investigate rather than adjusting the test, because "watch is unaffected" is a claim this task makes.

- [ ] **Step 8: Drill — a frozen event must never become assignable**

In `buildClusterPrompt`, temporarily append `n.Events` to `assignable` as well as `n.EligibleEvents`. Run:

```
go test ./internal/correlator/ -run TestCluster_EligibleNarrativeEventsAreAssignable -v 2>&1 | tail -15; echo "exit=$?"
```

Expected: FAILS on `"a frozen event must never be assignable: a posted comment already describes it"`. Record the output, then restore.

- [ ] **Step 9: Commit**

```bash
git add internal/correlator/correlator.go internal/correlator/correlator_test.go internal/pipeline/narrate.go
git commit -m "correlator: freeze existing narratives up to their commit watermark

buildClusterPrompt printed every existing narrative's events as
CONTEXT ONLY, so the model structurally could not reassign them. That is
a blunt version of the commit watermark: all existing narratives frozen.

Now a narrative's eligible events (linked after its last committed
action) render in the numbered assignable section, while its frozen
events stay context-only. watch is unaffected in practice — prior
narratives normally have committed actions, so EligibleEvents is empty —
which TestCluster_NoEligibleEventsMatchesTodaysBehaviour pins rather than
asserts. This is what lets triage restructure with no special
re-clustering mode.

hydrateContextNarratives partitions rather than duplicating: an event in
both slices would be listed twice and could be assigned by index despite
being frozen.

Drilled: adding n.Events to the assignable slice fails the
frozen-must-not-be-assignable assertion."
```

---

### Task 3: `reconciler.Redraft` — redraft one action with reviewer feedback

`[e]dit` reuses the verified links already computed this pass and re-runs **only** the drafting call, with the reviewer's feedback appended to the prompt. Live state was confirmed moments ago, so the reconciler's verify-before-proposing invariant holds without a second Jira round-trip.

**Files:**
- Modify: `internal/reconciler/draft.go`
- Test: `internal/reconciler/draft_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/reconciler/draft_test.go`. **Verified while writing this plan:** that file is `package reconciler` (internal), so it can construct the unexported `verifiedLink` and call `Redraft` unqualified. It already provides `fakeLLM` and `codeEvent(externalID, summary)` (the latter in `reconciler_test.go:172`). The existing `draft` tests build `narrative`/`verified` **inline** — there are no `draftTestNarrative`-style helpers, so these tests do the same:

```go
// TestRedraft_PutsReviewerFeedbackInThePrompt is the whole point of Redraft:
// the reviewer's correction must reach the model, not merely be persisted.
func TestRedraft_PutsReviewerFeedbackInThePrompt(t *testing.T) {
	f := &fakeLLM{responses: []string{
		`[{"issue_key":"PROJ-1","type":"comment","body":"revised body","confidence":0.9,"rationale":"reworded per reviewer"}]`,
	}}

	narrative := store.NarrativeRow{ID: 1, Title: "retry logic", Summary: "added retries"}
	verified := []verifiedLink{{
		Link:  store.NarrativeIssue{IssueKey: "PROJ-1", Role: store.Role("primary")},
		Issue: tasktracker.Issue{Key: "PROJ-1", Summary: "add retries"},
	}}

	got, _, err := Redraft(t.Context(), f, narrative,
		[]events.Event{codeEvent("e1", "added retry logic")}, verified,
		nil, "too vague — name the actual PR")

	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "revised body", got[0].Body)

	require.Len(t, f.prompts, 1)
	assert.Contains(t, f.prompts[0], "too vague — name the actual PR",
		"the reviewer's feedback must appear in the user prompt the model actually receives")
}

// TestRedraft_EmptyFeedbackIsAnError: Redraft exists to carry a correction. An
// empty one means the caller lost the reviewer's text somewhere, which should
// fail loudly rather than silently spending an LLM call to reproduce the same
// draft.
func TestRedraft_EmptyFeedbackIsAnError(t *testing.T) {
	f := &fakeLLM{responses: []string{`[]`}}

	narrative := store.NarrativeRow{ID: 1, Title: "retry logic", Summary: "added retries"}
	verified := []verifiedLink{{
		Link:  store.NarrativeIssue{IssueKey: "PROJ-1", Role: store.Role("primary")},
		Issue: tasktracker.Issue{Key: "PROJ-1", Summary: "add retries"},
	}}

	_, _, err := Redraft(t.Context(), f, narrative,
		[]events.Event{codeEvent("e1", "added retry logic")}, verified, nil, "   ")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "feedback")
	assert.Empty(t, f.prompts, "no LLM call should be spent on an empty correction")
}
```

**Verified while writing this plan:** `internal/reconciler/draft_test.go` is `package reconciler` (internal), so it can construct the unexported `verifiedLink` and call `Redraft` **directly**. Drop the `ForTest` suffix from the test above — call `reconciler.Redraft` as `Redraft(...)`, with no package qualifier — and skip the shim at the end of Step 3 entirely.

- [ ] **Step 2: Run to verify it fails**

```
go test ./internal/reconciler/ -run TestRedraft -v 2>&1 | tail -15; echo "exit=$?"
```

Expected: compile failure — `undefined: Redraft`.

- [ ] **Step 3: Implement**

In `internal/reconciler/draft.go`, after `draft`:

```go
// Redraft re-runs drafting for one narrative with a reviewer's free-text
// correction added to the prompt. It is `triage`'s [e]dit disposition.
//
// It deliberately reuses the caller's already-verified links rather than
// re-verifying: live tracker state was confirmed earlier in this same pass, so
// the "never propose without verifying" invariant (rules/intent-not-outcome.md)
// still holds, and a second round-trip would cost a Jira call to learn what we
// just learned. The tradeoff is a narrow staleness window — if the issue
// changed in the seconds since verification, this redraft is against slightly
// old state. That is the same window every action in the batch already has
// between propose and apply, so Redraft introduces no new exposure.
//
// Empty feedback is an error, not a no-op: Redraft exists to carry a
// correction, so blank text means the caller dropped the reviewer's words
// somewhere upstream. Failing loudly beats spending an LLM call to regenerate
// the draft the reviewer just rejected.
func Redraft(
	ctx context.Context,
	client llm.Client,
	narrative store.NarrativeRow,
	delta []events.Event,
	verified []verifiedLink,
	learnedRules []rules.Rule,
	feedback string,
) ([]ProposedAction, correlator.Stats, error) {
	if strings.TrimSpace(feedback) == "" {
		return nil, correlator.Stats{}, fmt.Errorf(
			"redrafting narrative %d: reviewer feedback is empty", narrative.ID)
	}

	var stats correlator.Stats

	prompt := buildDraftPrompt(narrative, delta, verified) +
		"\n\nThe reviewer rejected the previous draft with this correction. " +
		"Address it directly:\n" + feedback

	systemPrompt := draftSystemPrompt
	if rendered := rules.Render(learnedRules); rendered != "" {
		systemPrompt += "\n\n" + rendered
	}

	raw, usage, err := client.Complete(ctx, systemPrompt, prompt)
	if err != nil {
		return nil, stats, fmt.Errorf("redrafting narrative %d: %w", narrative.ID, err)
	}
	stats.AddUsage(usage)

	verdicts, err := parseDraftResponse(raw)
	if err != nil {
		return nil, stats, fmt.Errorf("redrafting narrative %d: %w", narrative.ID, err)
	}

	// Same verdict->action mapping draft() uses, including the
	// unrecognized-issue_key skip and toProposedAction's confidence flooring.
	// Extract draft()'s loop into a shared helper and call it from both rather
	// than copying it here — a divergence would mean triage's redrafts floor
	// confidence differently from watch's drafts, which nothing would catch.
	byKey := make(map[string]verifiedLink, len(verified))
	for _, v := range verified {
		byKey[v.Link.IssueKey] = v
	}

	var out []ProposedAction
	for _, verdict := range verdicts {
		v, ok := byKey[verdict.IssueKey]
		if !ok {
			log.Printf(
				"reconciler: narrative %d redraft named unrecognized issue_key %q, ignoring",
				narrative.ID, verdict.IssueKey,
			)

			continue
		}

		out = append(out, toProposedAction(verdict, v))
	}

	return out, stats, nil
}
```

Add `"log"` to the imports if absent (it is already there — `draft` uses it for the same skip).

**Verified while writing this plan:** the parser is `parseDraftResponse(raw string) ([]draftVerdict, error)` at `draft.go:177`, and `draft` maps verdicts inline (`draft.go:94-116`) via `toProposedAction(verdict, v)` at `draft.go:121`. There is **no** `actionsFromVerdicts` helper — the loop above reproduces `draft`'s real one. **Prefer extracting that loop into one shared function called by both** over leaving two copies: the copies would drift on confidence flooring, and no test would notice.

Add `"strings"` to the imports if absent.

- [ ] **Step 4: Run the tests**

```
go test ./internal/reconciler/ 2>&1 | tail -10; echo "exit=$?"
```

Expected: both new tests PASS, every existing reconciler test still PASSES.

- [ ] **Step 5: Drill — the feedback must actually reach the prompt**

Remove the `+ "\n\nThe reviewer rejected..."` concatenation so the prompt is just `buildDraftPrompt(...)`. Run:

```
go test ./internal/reconciler/ -run TestRedraft_PutsReviewerFeedbackInThePrompt -v 2>&1 | tail -12; echo "exit=$?"
```

Expected: FAILS on `"the reviewer's feedback must appear in the user prompt the model actually receives"`. Record it, restore.

- [ ] **Step 6: Commit**

```bash
git add internal/reconciler/draft.go internal/reconciler/draft_test.go
git commit -m "reconciler: Redraft — redraft one action with reviewer feedback

triage's [e]dit disposition. Reuses the links already verified this pass
rather than re-verifying: live state was confirmed moments ago, so
intent-not-outcome still holds and a second Jira round-trip would only
re-learn what we know. The residual staleness window is the same one
every batched action already has between propose and apply.

Empty feedback errors rather than no-oping — blank text means the caller
dropped the reviewer's words, and regenerating the draft they just
rejected would spend an LLM call to produce the same thing.

Drilled: dropping the feedback concatenation fails the
does-it-reach-the-prompt assertion, which is the only thing that makes
this function worth having."
```

---

### Task 4: the two restructure seams — `RemoveNarrativeIssue` and `UnlinkNarrativeEvents`

Both restructures need a *removal* seam the store does not have. They are paired here because they
are the same shape (a scoped DELETE with a RowsAffected check) and because discovering the second
one invalidated a claim in the design spec.

**`[t]arget`** needs to remove a narrative→issue link: `AddNarrativeIssues` only upserts, and the partial unique index `one_primary_per_narrative` rejects a second `primary`, so retarget is impossible without a delete.

**`[m]erge`** needs to remove a narrative→**event** link, which the spec originally said it did not. `Persist`'s `ClusterExtends` path calls `AddNarrativeEvents` — `INSERT OR IGNORE`, which adds the destination link and never removes the source. Probed directly while planning:

```
after "merge" A->B: A has 1, B has 1
CONFIRMED double-link: merge must unlink from A explicitly
```

Without the unlink, a merged event stays attached to both narratives: the source looks alive, keeps feeding future `Cluster` calls as context, and the work is double-counted.

**Merge direction is determined by commitment, and that is what makes this seam safe to have.** The committed narrative is always the target — a posted comment made it the workstream of record. Both-committed is refused outright (two tracker issues each already claiming the work; that is an org decision, not a review-loop one). In the real 19-narrative backlog, 15 had proposed actions and **zero** had committed ones, so uncommitted-to-uncommitted is the case that actually happens.

That rule closes a hazard the naive implementation has. The watermark is per-narrative, so relinking a *frozen* event onto a never-committed narrative makes it eligible again — probed directly:

```
BEFORE:                on A eligible=[]  (empty = frozen)
AFTER relink onto B:   on B eligible=[1]
CONFIRMED: frozen on A, eligible on B — the watermark is PER-NARRATIVE
```

Moving events *away from* the committed narrative would launder frozen events into unfrozen ones. Direction-by-commitment makes that **structurally unreachable**: frozen events live on the committed narrative, which is always the target, so a frozen event is never relinked at all.

`UnlinkNarrativeEvents` still takes explicit event ids rather than "all of narrative N", so a caller cannot ask for the unsafe thing in one call.

**Files:**
- Modify: `internal/store/store.go` (next to `AddNarrativeIssues`, ~line 1204)
- Test: `internal/store/store_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/store/store_test.go`:

```go
// TestRemoveNarrativeIssue_ThenAddPrimarySucceeds is the retarget path, and the
// reason this method has to exist: one_primary_per_narrative is a partial
// unique index, so adding a second primary fails while the first is present.
// Retargeting is therefore remove-then-add, not upsert.
func TestRemoveNarrativeIssue_ThenAddPrimarySucceeds(t *testing.T) {
	s := openStore(t)

	base := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)
	nid, err := s.InsertNarrative(base, base.Add(time.Hour), "t", "s")
	require.NoError(t, err)

	require.NoError(t, s.AddNarrativeIssues(nid, []store.NarrativeIssue{
		{IssueKey: "PROJ-1", Role: store.Role("primary"), Provenance: "jira_event", Confidence: 0.9},
	}))

	// Prove the constraint is real before proving the fix: a second primary
	// must fail while the first exists. Without this the test could pass for
	// the wrong reason.
	err = s.AddNarrativeIssues(nid, []store.NarrativeIssue{
		{IssueKey: "PROJ-2", Role: store.Role("primary"), Provenance: "reviewer", Confidence: 1.0},
	})
	require.Error(t, err, "one_primary_per_narrative must reject a second primary")

	require.NoError(t, s.RemoveNarrativeIssue(nid, "PROJ-1"))

	require.NoError(t, s.AddNarrativeIssues(nid, []store.NarrativeIssue{
		{IssueKey: "PROJ-2", Role: store.Role("primary"), Provenance: "reviewer", Confidence: 1.0},
	}), "after removing the old primary the new one must be insertable")

	links, err := s.NarrativeIssues(nid)
	require.NoError(t, err)
	require.Len(t, links, 1)
	assert.Equal(t, "PROJ-2", links[0].IssueKey)
	assert.Equal(t, "reviewer", links[0].Provenance,
		"a human's assertion is recorded as reviewer-provenance, not a model's inference")
}

// TestRemoveNarrativeIssue_MissingLinkErrors: silently succeeding would let a
// retarget report success having changed nothing.
func TestRemoveNarrativeIssue_MissingLinkErrors(t *testing.T) {
	s := openStore(t)

	base := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)
	nid, err := s.InsertNarrative(base, base.Add(time.Hour), "t", "s")
	require.NoError(t, err)

	err = s.RemoveNarrativeIssue(nid, "NOPE-1")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "NOPE-1")
}
```

**Verified while writing this plan:** `store.Role` is a bare `type Role string` with **no exported constants** — so `store.Role("primary")` above is correct and `store.RolePrimary` does not exist. The schema comment documents the three values as `primary | same_work | mentioned`.

- [ ] **Step 2: Run to verify it fails**

```
go test ./internal/store/ -run TestRemoveNarrativeIssue -v 2>&1 | tail -12; echo "exit=$?"
```

Expected: `s.RemoveNarrativeIssue undefined`.

- [ ] **Step 3: Implement**

In `internal/store/store.go`, directly after `AddNarrativeIssues` and its `*Tx` variant:

```go
// RemoveNarrativeIssue deletes one narrative→issue link. It exists for
// `triage`'s retarget disposition, and it has to exist because
// AddNarrativeIssues only upserts: the partial unique index
// one_primary_per_narrative rejects a second primary while the first is
// present, so "this belongs on a different ticket" is remove-then-add rather
// than an update.
//
// A missing link is an error, not a silent success. Retarget's caller uses the
// error to abort before adding the replacement — otherwise a typo'd key would
// report a successful retarget having changed nothing, and the reviewer would
// believe an attribution moved when it did not.
func (s *Store) RemoveNarrativeIssue(narrativeID int64, issueKey string) error {
	return removeNarrativeIssueImpl(s.db, narrativeID, issueKey)
}

// RemoveNarrativeIssue is the *Tx-scoped variant of
// (*Store).RemoveNarrativeIssue. Retarget uses it so the remove and the
// replacing add land in one transaction: a crash between them would leave a
// narrative with no primary at all.
func (t *Tx) RemoveNarrativeIssue(narrativeID int64, issueKey string) error {
	return removeNarrativeIssueImpl(t.tx, narrativeID, issueKey)
}

func removeNarrativeIssueImpl(c dbConn, narrativeID int64, issueKey string) error {
	res, err := c.Exec(
		`DELETE FROM narrative_issues WHERE narrative_id = ? AND issue_key = ?`,
		narrativeID, issueKey,
	)
	if err != nil {
		return fmt.Errorf("removing issue link %s from narrative %d: %w", issueKey, narrativeID, err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking rows affected removing %s from narrative %d: %w", issueKey, narrativeID, err)
	}
	if affected == 0 {
		return fmt.Errorf("removing issue link %s from narrative %d: no such link", issueKey, narrativeID)
	}

	return nil
}
```


#### Step 3b: `UnlinkNarrativeEvents` — merge's removal seam

> **Verified while writing this plan:** implemented and run; both tests below pass as written.

Append these tests to `internal/store/store_test.go`:

```go
package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/events"
)

func TestUnlinkNarrativeEvents_RemovesOnlyTheNamedLinks(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)

	a, err := s.InsertNarrative(base, base.Add(time.Hour), "A", "s")
	require.NoError(t, err)

	var ids []int64
	for i, ext := range []string{"u:1", "u:2", "u:3"} {
		e := events.NewEvent("claude_code", ext, base.Add(time.Duration(i)*time.Minute), "w")
		_, err := s.InsertEvent(e)
		require.NoError(t, err)
		eid, err := s.EventIDByExternalID("claude_code", ext)
		require.NoError(t, err)
		require.NoError(t, s.AddNarrativeEvents(a, []int64{eid}))
		ids = append(ids, eid)
	}

	require.NoError(t, s.UnlinkNarrativeEvents(a, []int64{ids[0], ids[2]}))

	n, err := s.NarrativeEventCount(a)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "only the un-named link survives")
}

func TestUnlinkNarrativeEvents_MissingLinkErrors(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)
	a, err := s.InsertNarrative(base, base.Add(time.Hour), "A", "s")
	require.NoError(t, err)

	err = s.UnlinkNarrativeEvents(a, []int64{999999})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no such link")
}
```

And add to `internal/store/eligibility.go` (it belongs with the watermark it protects, not in the already-large `store.go`):

```go
// UnlinkNarrativeEvents removes specific event links from a narrative.
func (s *Store) UnlinkNarrativeEvents(narrativeID int64, eventIDs []int64) error {
	return unlinkNarrativeEventsImpl(s.db, narrativeID, eventIDs)
}

func (t *Tx) UnlinkNarrativeEvents(narrativeID int64, eventIDs []int64) error {
	return unlinkNarrativeEventsImpl(t.tx, narrativeID, eventIDs)
}

func unlinkNarrativeEventsImpl(c dbConn, narrativeID int64, eventIDs []int64) error {
	if len(eventIDs) == 0 {
		return nil
	}

	for _, eventID := range eventIDs {
		res, err := c.Exec(
			`DELETE FROM narrative_events WHERE narrative_id = ? AND event_id = ?`,
			narrativeID, eventID,
		)
		if err != nil {
			return fmt.Errorf("unlinking event %d from narrative %d: %w", eventID, narrativeID, err)
		}

		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("checking rows affected unlinking event %d from narrative %d: %w", eventID, narrativeID, err)
		}
		if affected == 0 {
			return fmt.Errorf("unlinking event %d from narrative %d: no such link", eventID, narrativeID)
		}
	}

	return nil
}
```

Note the signature takes **explicit event ids**, not "everything on narrative N". That is deliberate: Task 6 may only unlink *eligible* events, and an all-of-narrative variant would let a caller ask for the laundering case in a single call. Making the unsafe thing inexpressible beats documenting it.

- [ ] **Step 4: Run**

```
go test ./internal/store/ 2>&1 | tail -6; echo "exit=$?"
```

Expected: both new tests PASS, whole store package still PASSES.

- [ ] **Step 5: Drill — a missing link must not silently succeed**

Delete the `if affected == 0 { ... }` block. Run:

```
go test ./internal/store/ -run TestRemoveNarrativeIssue_MissingLinkErrors -v 2>&1 | tail -10; echo "exit=$?"
```

Expected: FAILS with "An error is expected but got nil". Record, restore.

- [ ] **Step 6: Commit**

```bash
git add internal/store/store.go internal/store/store_test.go
git commit -m "store: RemoveNarrativeIssue — retarget needs a delete, not an upsert

triage's [t]arget disposition moves a narrative to a different issue.
AddNarrativeIssues only upserts, and one_primary_per_narrative is a
partial unique index that rejects a second primary while the first
exists — so retarget is remove-then-add. The test proves the constraint
is real before proving the fix, so it cannot pass for the wrong reason.

A missing link errors: silently succeeding would let a typo'd key report
a successful retarget having moved nothing.

The *Tx variant exists so remove and add land atomically — a crash
between them would leave a narrative with no primary at all."
```

---

### Task 5: `triage.Session` — the state machine that never touches a terminal

The review session: walk the batch, collect one decision per action, apply **nothing**. `cmd/` implements `Prompter` against a terminal; tests implement it as a script. That seam is what makes the restructures in Task 6 testable at all.

**Files:**
- Create: `internal/triage/triage.go`
- Create: `internal/triage/triage_test.go`

> **Verified while writing this plan:** both files below were created, compiled, and run. Both tests pass as written. The prototype also found a real design bug — see Step 3.

- [ ] **Step 1: Write the failing tests**

Create `internal/triage/triage_test.go`:

```go
package triage_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/triage"
)

// scriptedPrompter answers with a fixed sequence, recording what it was shown.
type scriptedPrompter struct {
	answers []triage.Decision
	shown   []triage.Item
	confirm bool
}

func (p *scriptedPrompter) Ask(item triage.Item) (triage.Decision, error) {
	p.shown = append(p.shown, item)
	if len(p.answers) == 0 {
		return triage.Decision{Verb: triage.VerbSkip}, nil
	}
	d := p.answers[0]
	p.answers = p.answers[1:]

	return d, nil
}

func (p *scriptedPrompter) Confirm(string) (bool, error) { return p.confirm, nil }

func batchOf(ids ...int64) []store.ActionRow {
	out := make([]store.ActionRow, 0, len(ids))
	for _, id := range ids {
		out = append(out, store.ActionRow{
			ID: id, NarrativeID: id, Type: "comment",
			IssueKey: "PROJ-1", Payload: `{"body":"b"}`, Status: "proposed",
		})
	}

	return out
}

func TestSession_CollectsOneDecisionPerActionAndAppliesNothing(t *testing.T) {
	p := &scriptedPrompter{answers: []triage.Decision{
		{Verb: triage.VerbApprove},
		{Verb: triage.VerbReject, Text: "not worth posting"},
		{Verb: triage.VerbApprove},
	}}
	s := triage.NewSession(batchOf(1, 2, 3), p)

	require.NoError(t, s.Run())

	require.Len(t, p.shown, 3)
	assert.Equal(t, 1, p.shown[0].Position)
	assert.Equal(t, 3, p.shown[0].Total)

	approved := s.Approved()
	require.Len(t, approved, 2, "only the two approvals")
	assert.Equal(t, int64(1), approved[0].ID)
	assert.Equal(t, int64(3), approved[1].ID)
}

func TestSession_QuitAbandonsWithoutApplying(t *testing.T) {
	p := &scriptedPrompter{answers: []triage.Decision{
		{Verb: triage.VerbApprove},
		{Verb: triage.VerbQuit},
	}}
	s := triage.NewSession(batchOf(1, 2, 3), p)

	err := s.Run()

	require.ErrorIs(t, err, triage.ErrAbandoned)
	assert.Empty(t, s.Approved(),
		"quit must discard even decisions already recorded: nothing was applied, so nothing is owed")
}
```

- [ ] **Step 2: Run to verify it fails**

```
go test ./internal/triage/ -v 2>&1 | tail -20; echo "exit=$?"
```

Expected: the package does not exist yet — `no Go files in .../internal/triage`.

- [ ] **Step 3: Implement**

Create `internal/triage/triage.go`:

```go
// Package triage is the interactive review session over the actions queue.
package triage

import (
	"fmt"
	"strings"

	"github.com/jcogilvie/unjira/internal/store"
)

// Verb is one disposition a reviewer chooses for one action.
type Verb string

const (
	VerbApprove Verb = "approve"
	VerbReject  Verb = "reject"
	VerbEdit    Verb = "edit"
	VerbMerge   Verb = "merge"
	VerbSplit   Verb = "split"
	VerbSkip    Verb = "skip"
	VerbQuit    Verb = "quit"
	VerbTarget  Verb = "target"
)

// Decision is a reviewer's answer for one presented action.
type Decision struct {
	Verb Verb
	// Text carries reject/edit free-text, or the issue key for target.
	Text string
	// Positions carries merge/split's 1-based batch positions.
	Positions []int
}

// Item is one action as presented for review.
type Item struct {
	Action   store.ActionRow
	Position int
	Total    int
}

// Prompter is how a Session asks a human. cmd/unjira implements it against a
// terminal; tests implement it as a script. The Session never touches stdin.
type Prompter interface {
	Ask(item Item) (Decision, error)
	Confirm(summary string) (bool, error)
}

// Session holds one review pass in memory.
type Session struct {
	batch     []store.ActionRow
	decisions map[int64]Decision
	prompter  Prompter
}

func NewSession(batch []store.ActionRow, p Prompter) *Session {
	return &Session{
		batch:     batch,
		decisions: make(map[int64]Decision, len(batch)),
		prompter:  p,
	}
}

// Run walks the batch, collecting a decision per action. It applies nothing.
func (s *Session) Run() error {
	for i, a := range s.batch {
		d, err := s.prompter.Ask(Item{Action: a, Position: i + 1, Total: len(s.batch)})
		if err != nil {
			return fmt.Errorf("prompting for action %d: %w", a.ID, err)
		}
		if d.Verb == VerbQuit {
			// Discard every decision, not just this one. Nothing has been
			// applied yet — that is the whole point of batch apply — so a
			// reviewer who quits owes nothing and expects nothing to have
			// happened. Keeping earlier approvals would make quit a partial
			// commit, which is the surprise batch apply exists to avoid.
			s.decisions = make(map[int64]Decision, len(s.batch))

			return ErrAbandoned
		}
		s.decisions[a.ID] = d
	}

	return nil
}

// Approved returns the actions the reviewer approved, in batch order.
func (s *Session) Approved() []store.ActionRow {
	var out []store.ActionRow
	for _, a := range s.batch {
		if s.decisions[a.ID].Verb == VerbApprove {
			out = append(out, a)
		}
	}

	return out
}

// Summary renders the pending dispositions for the final confirmation.
func (s *Session) Summary() string {
	counts := map[Verb]int{}
	for _, d := range s.decisions {
		counts[d.Verb]++
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d approve, %d reject, %d edit, %d skip",
		counts[VerbApprove], counts[VerbReject], counts[VerbEdit], counts[VerbSkip])

	return b.String()
}

// ErrAbandoned is returned when the reviewer quits: nothing is applied.
var ErrAbandoned = fmt.Errorf("triage abandoned by reviewer")
```

**Self-review gap, fixed here rather than left for Task 6 to discover:** `Run` as written records every verb but only *acts* on approve/reject/skip/quit. Merge, split, target, and edit need somewhere to go. Add a `Handler` seam so `Session` stays free of correlator/reconciler knowledge and Task 6 has a defined place to plug in:

```go
// Handler performs the verbs that do more than record a disposition. Session
// calls it and re-presents whatever comes back; it knows nothing about
// clustering, drafting, or the store.
//
// Separate from Prompter because these are two different axes: Prompter is how
// we ask a human, Handler is what happens when the answer requires work. A test
// can script one and stub the other.
type Handler interface {
	// Redraft returns a replacement action for an edit.
	Redraft(action store.ActionRow, feedback string) (store.ActionRow, error)
	// Restructure performs a merge/split/retarget and returns the actions that
	// replace the affected ones. Returning a slice (not one action) is why
	// re-presentation exists: a split yields more actions than it consumed.
	Restructure(d Decision, batch []store.ActionRow) ([]store.ActionRow, error)
}
```

`Run` then dispatches: approve/reject/skip record and advance; edit and the three restructures call the Handler and splice the result into the batch for re-presentation. A nil Handler is valid — it makes those verbs return a "not available" error the prompt loop shows and re-prompts on, which is exactly what `--dry-run` wants.

**The quit behaviour is a bug the prototype caught, not a nicety.** The first version returned `ErrAbandoned` without clearing `s.decisions`, so `Approved()` still returned the approval recorded *before* the quit. That makes quit a partial commit — precisely the surprise batch apply exists to prevent. `TestSession_QuitAbandonsWithoutApplying` failed with:

```
Should be empty, but was [{1 1 comment PROJ-1 {"body":"b"} 0  proposed   <nil> <nil> }]
```

- [ ] **Step 4: Run**

```
go test ./internal/triage/ -v 2>&1 | tail -12; echo "exit=$?"
```

Expected: both PASS.

- [ ] **Step 5: Drill — quit must discard everything**

Remove the `s.decisions = make(...)` line from the `VerbQuit` branch. Run:

```
go test ./internal/triage/ -run TestSession_QuitAbandons -v 2>&1 | tail -10; echo "exit=$?"
```

Expected: the failure quoted in Step 3. Restore.

- [ ] **Step 6: Commit**

```bash
git add internal/triage/
git commit -m "triage: Session — collect dispositions, apply nothing

The review pass as a state machine with a Prompter seam, so the session
is drivable from a scripted test and never touches stdin. That is what
makes Task 6's restructures testable without driving a terminal.

Quit discards every decision, not just the one that quit. Nothing has been
applied — the point of batch apply — so a reviewer who quits expects
nothing to have happened; keeping earlier approvals would make quit a
partial commit. Caught by the prototype rather than by review."
```

---

### Task 6: the restructures — merge, split, retarget

The point of the command. Merge and split ride on `correlator.Cluster` + `Persist`, unlocked by Task 2. Retarget uses Task 4's two removal seams.

> **Verified while writing this plan:** the store-level merge flow below was implemented against a real store and passes, including the no-laundering guarantee.

**Files:**
- Create: `internal/triage/restructure.go`
- Create: `internal/triage/restructure_test.go`
- Test (store-level flow): `internal/store/eligibility_test.go`

#### The rule this task enforces

**Merge direction is determined by commitment**, not by the reviewer's argument order:

| merge(X, Y) | target |
|---|---|
| both uncommitted | either — the common case (15 of 19 real narratives had proposed actions, **zero** had committed ones) |
| exactly one committed | **the committed one**, always — unjira already mutated the tracker on its behalf, making it the workstream of record |
| both committed | **refuse**, naming both narratives and their applied actions |

Both-committed is refused because unjira cannot unpost a comment, un-transition a status, or un-create an issue. Two committed workstreams means two tracker issues each already claiming this work; deciding which is real is an org-level call, not a review-loop one.

This rule also makes the laundering hazard **structurally unreachable**: frozen events live on the committed narrative, the committed narrative is always the target, so a frozen event is never relinked.

- [ ] **Step 1: Write the store-level flow test first**

This proves the mechanics before any LLM is involved. Append to `internal/store/eligibility_test.go`:

```go
func seedN(t *testing.T, s *store.Store, title string, extIDs ...string) (int64, []int64) {
	t.Helper()
	base := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)
	nid, err := s.InsertNarrative(base, base.Add(time.Hour), title, "s")
	require.NoError(t, err)

	var ids []int64
	for i, ext := range extIDs {
		e := events.NewEvent("claude_code", ext, base.Add(time.Duration(i)*time.Minute), "w:"+ext)
		_, err := s.InsertEvent(e)
		require.NoError(t, err)
		eid, err := s.EventIDByExternalID("claude_code", ext)
		require.NoError(t, err)
		require.NoError(t, s.AddNarrativeEvents(nid, []int64{eid}))
		ids = append(ids, eid)
	}

	return nid, ids
}

func commitAgainst(t *testing.T, s *store.Store, nid int64) {
	t.Helper()
	id, err := s.InsertAction(store.ActionRow{
		NarrativeID: nid, Type: "comment", IssueKey: "PROJ-1",
		Payload: `{"body":"posted"}`, Status: "proposed",
	})
	require.NoError(t, err)
	require.NoError(t, s.UpdateActionStatus(id, "applied"))
}

// TestMergeFlow_CommittedNarrativeIsTheTarget is the whole merge mechanic under
// direction-by-commitment, proven without an LLM: A is committed, B is not, so A
// is the target and B's events move onto A. A's frozen events never move, which
// is what makes the per-narrative watermark safe under restructuring.
func TestMergeFlow_CommittedNarrativeIsTheTarget(t *testing.T) {
	s := openStore(t)

	a, aEvents := seedN(t, s, "A committed", "m:a1", "m:a2")
	commitAgainst(t, s, a)

	// linked_at has millisecond precision, so sleep past the commit instant
	// rather than racing it.
	time.Sleep(5 * time.Millisecond)
	b, bEvents := seedN(t, s, "B uncommitted", "m:b1")

	eligibleOnB, err := s.EligibleEventIDs(b)
	require.NoError(t, err)
	require.ElementsMatch(t, bEvents, eligibleOnB, "B never committed: all of B is eligible")

	eligibleOnA, err := s.EligibleEventIDs(a)
	require.NoError(t, err)
	require.Empty(t, eligibleOnA, "A's events predate its commit: frozen")

	// Direction: A committed, B not => A is the target. Move only B's ELIGIBLE
	// events onto A, then unlink them from B so nothing is double-linked.
	require.NoError(t, s.AddNarrativeEvents(a, eligibleOnB))
	require.NoError(t, s.UnlinkNarrativeEvents(b, eligibleOnB))

	countA, err := s.NarrativeEventCount(a)
	require.NoError(t, err)
	countB, err := s.NarrativeEventCount(b)
	require.NoError(t, err)

	assert.Equal(t, 3, countA, "A holds its own 2 frozen events plus B's 1")
	assert.Equal(t, 0, countB, "B is emptied, not double-linked")

	// Nothing laundered A's committed events into eligibility.
	stillFrozen, err := s.EligibleEventIDs(a)
	require.NoError(t, err)
	assert.NotContains(t, stillFrozen, aEvents[0], "A's committed event must remain frozen")
	assert.NotContains(t, stillFrozen, aEvents[1], "A's committed event must remain frozen")
	assert.Contains(t, stillFrozen, bEvents[0], "the newly-moved event is eligible: linked after A's commit")
}
```

Run it: `go test ./internal/store/ -run TestMergeFlow -v`. **Verified to pass as written.** Three assertions carry the weight: A ends with 3 events (its 2 frozen plus B's 1), B ends with **0** (not double-linked), and A's frozen events are *still frozen* afterward.

- [ ] **Step 2: Drill the no-laundering guarantee**

Swap the direction — make B (uncommitted) the target:

```go
	require.NoError(t, s.AddNarrativeEvents(b, eligibleOnA))   // WRONG DIRECTION
	require.NoError(t, s.UnlinkNarrativeEvents(a, eligibleOnA))
```

`eligibleOnA` is empty (A is frozen), so this moves nothing and the count assertions fail. That failure **is** the guarantee: there is no way to express "move A's frozen events" through these seams. Record the output, restore.

- [ ] **Step 3: `internal/triage/restructure.go` — direction resolution**

Write the direction rule as its own pure function, because it is the part most worth testing in isolation:

```go
package triage

import (
	"fmt"

	"github.com/jcogilvie/unjira/internal/store"
)

// commitState is whether unjira has already mutated the tracker for a
// narrative. A struct rather than a bare bool pair so the both-committed case
// gets named treatment instead of an inline `if a && b` a reader has to decode.
type commitState struct {
	NarrativeID int64
	Committed   bool
}

// resolveMergeTarget applies direction-by-commitment: the committed narrative
// absorbs the other, because unjira has already mutated the tracker on its
// behalf and that makes it the workstream of record.
//
// "Mutated" covers three things, and a comment is the mildest of them. A
// transition destroyed the prior status (Discovery -> Done loses "it was in
// Discovery" outside the changelog), and a create produced an object other
// people now reference. unjira can undo none of the three.
//
// Refuses both-committed for that reason: two committed workstreams means two
// tracker issues already claim this work, and choosing which is authoritative
// is an org-level decision, not one a review loop should make silently.
//
// This is also what keeps the per-narrative watermark sound under
// restructuring. EligibleEventIDs compares against the narrative's OWN
// max(executed_at), so relinking a frozen event onto a never-committed
// narrative would make it eligible again — probed directly while designing
// this: frozen on A, eligible on B. Because the committed narrative is always
// the target, frozen events are never relinked at all, so that hazard is
// unreachable rather than merely forbidden.
func resolveMergeTarget(a, b commitState) (target, source int64, err error) {
	switch {
	case a.Committed && b.Committed:
		return 0, 0, fmt.Errorf(
			"cannot merge narratives %d and %d: both have committed actions, so two tracker "+
				"issues already claim this work. unjira cannot retract a comment, un-transition a "+
				"status, or un-create an issue — decide which issue is authoritative in the "+
				"tracker first", a.NarrativeID, b.NarrativeID)
	case a.Committed:
		return a.NarrativeID, b.NarrativeID, nil
	case b.Committed:
		return b.NarrativeID, a.NarrativeID, nil
	default:
		// Neither committed: the reviewer's first-named narrative wins, keeping
		// `m 1 3` predictable. Nothing is at stake either way, since no tracker
		// mutation references either narrative yet.
		return a.NarrativeID, b.NarrativeID, nil
	}
}

// hasCommittedAction reports whether any action for this narrative actually
// reached the tracker.
//
// Filters on status == "applied", NOT on executed_at being set:
// UpdateActionStatus stamps executed_at for both applied AND failed, because a
// failed attempt still attempted a write. But a failed write mutated nothing,
// so it must not make a narrative the workstream of record.
func hasCommittedAction(s *store.Store, narrativeID int64) (bool, error) {
	actions, err := s.ActionsForNarrative(narrativeID)
	if err != nil {
		return false, fmt.Errorf("checking commit state of narrative %d: %w", narrativeID, err)
	}

	for _, a := range actions {
		if a.Status == "applied" {
			return true, nil
		}
	}

	return false, nil
}
```

`ActionsForNarrative` exists and returns `[]store.ActionRow` (`store.go:1534`) — verified.

> **An inconsistency to RESOLVE, not preserve.** `hasCommittedAction` treats only `applied` as committed, but Task 1's `EligibleEventIDs` SQL filters on `executed_at IS NOT NULL`, which also matches `failed`. So a narrative whose only write **failed** is "uncommitted" for merge direction but has a *frozen* watermark. That is incoherent, and it is my inconsistency, not a subtlety worth keeping. Pick one and make both agree:
>
> - **Filter the SQL on `status = 'applied'` too** (recommended): a failed write changed nothing in the tracker, so it should not freeze events either.
> - Or keep the watermark conservative and document why, i.e. that a failed write may have partially landed and its events should not move until a human confirms.
>
> Whichever you choose, change both call sites and write the reason down. Do not leave them disagreeing.

- [ ] **Step 4: Test direction resolution, table-driven**

Create `internal/triage/restructure_test.go` (`package triage`, internal — it tests unexported functions):

```go
func TestResolveMergeTarget(t *testing.T) {
	cases := []struct {
		name          string
		a, b          commitState
		wantTarget    int64
		wantSource    int64
		wantErrPhrase string
	}{
		{
			name: "neither committed: first named wins",
			a:    commitState{NarrativeID: 7}, b: commitState{NarrativeID: 9},
			wantTarget: 7, wantSource: 9,
		},
		{
			name: "a committed: a absorbs b",
			a:    commitState{NarrativeID: 7, Committed: true}, b: commitState{NarrativeID: 9},
			wantTarget: 7, wantSource: 9,
		},
		{
			name: "b committed: b absorbs a, IGNORING argument order",
			a:    commitState{NarrativeID: 7}, b: commitState{NarrativeID: 9, Committed: true},
			wantTarget: 9, wantSource: 7,
		},
		{
			name:          "both committed: refused, naming both",
			a:             commitState{NarrativeID: 7, Committed: true},
			b:             commitState{NarrativeID: 9, Committed: true},
			wantErrPhrase: "both have committed actions",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target, source, err := resolveMergeTarget(tc.a, tc.b)

			if tc.wantErrPhrase != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErrPhrase)
				assert.Contains(t, err.Error(), "7")
				assert.Contains(t, err.Error(), "9",
					"the error must name BOTH narratives so a human can go look at them")

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.wantTarget, target)
			assert.Equal(t, tc.wantSource, source)
		})
	}
}
```

The third case is the one that matters: it proves argument order does **not** decide direction when commitment does.

- [ ] **Step 5: Drill direction resolution**

Change `case b.Committed:` to return `(a.NarrativeID, b.NarrativeID)`. Expected: the third case fails with `expected: 9, actual: 7`. That is the laundering bug in its most direct form, so this test must catch it. Record, restore.

- [ ] **Step 6: Wire merge into the Session**

Merge is: resolve direction → gather the source's **eligible** events → `Cluster` with the reviewer's instruction → `Persist` → unlink the moved events from the source → invalidate both narratives' `proposed` actions → re-derive → re-present.

**A constraint found while planning, absent from the design doc:** `reconciler.Reconcile` selects its own backlog via `NarrativesWithActionableLinks(limit, roles)` and **cannot be pointed at specific narratives**. So a restructure cannot simply "re-reconcile narratives 7 and 9." Three options — choose one and justify it in the doc comment:

1. **Add a `WithNarratives([]int64)` option to `Reconcile`.** Mirrors the `WithRules` option pattern, keeps narrative selection inside the reconciler, smallest diff. **Recommended.**
2. Export a per-narrative seam. `reconcileOne` is unexported, so this means a new exported function either way — more surface for the same result.
3. Re-run the whole `Reconcile` and let the restructured narratives be picked up among others. Simplest to write, but re-drafts unrelated narratives and re-spends their LLM calls, which is exactly the cost `--dry-run`'s "including LLM calls" warning exists to make visible.

Re-presentation, not silent replacement: the reviewer has not seen the new text, so freshly derived actions go back into the batch for disposition. Dispositions already recorded for *unaffected* actions survive — that is what batch apply buys.

- [ ] **Step 7: Full gate**

```
go build ./... ; echo "exit=$?"
go test ./... ; echo "exit=$?"
go test -race ./... ; echo "exit=$?"
go vet -tags=live ./... ; echo "exit=$?"
~/.asdf/installs/golang/1.25.7/bin/golangci-lint run ./... ; echo "exit=$?"
earthly +reviewable
```

Lint must report `0 issues.` and earthly must say SUCCESS.

- [ ] **Step 8: Commit**

```bash
git add internal/triage/ internal/store/
git commit -m "triage: merge, split, and retarget

Merge direction is determined by commitment, not by the reviewer's
argument order: the committed narrative absorbs the other, because unjira
already mutated the tracker on its behalf and that makes it the workstream
of record. 'Mutated' covers comment, transition, and create — and unjira
can undo none of them.

Both-committed is refused, naming both narratives: two tracker issues
already claim the work, and choosing which is authoritative is an
org-level decision rather than one a review loop makes silently.

The rule also makes a laundering hazard unreachable rather than forbidden.
The watermark is per-narrative, so relinking a frozen event onto a
never-committed narrative would thaw it (probed: frozen on A, eligible on
B). Because the committed narrative is always the target, frozen events are
never relinked.

Drilled: reversing direction when only b is committed fails the
argument-order-does-not-decide test, which is the laundering bug in its
most direct form."
```

---

### Task 7: the terminal `Prompter` and the `triage` command

`cmd/unjira/triage.go` is the I/O shell: reads a line, parses a verb, prints an action. It holds no logic — every decision belongs to `triage.Session`.

**Files:**
- Create: `cmd/unjira/triage.go`
- Create: `cmd/unjira/triage_test.go`
- Modify: `cmd/unjira/main.go` (the `cli` struct, alongside `Actions actionsCmd`)

- [ ] **Step 1: Write the failing test for verb parsing**

Verb parsing is the only real logic in this file, so it is the only thing worth unit-testing here. Create `cmd/unjira/triage_test.go`:

```go
func TestParseDecision(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		wantVerb  triage.Verb
		wantText  string
		wantPos   []int
		wantErr   string
	}{
		{name: "approve", input: "a", wantVerb: triage.VerbApprove},
		{name: "reject with reason", input: "r not worth posting", wantVerb: triage.VerbReject, wantText: "not worth posting"},
		{name: "edit with correction", input: "e name the actual PR", wantVerb: triage.VerbEdit, wantText: "name the actual PR"},
		{name: "skip", input: "k", wantVerb: triage.VerbSkip},
		{name: "quit", input: "q", wantVerb: triage.VerbQuit},
		{name: "merge two positions", input: "m 1 3", wantVerb: triage.VerbMerge, wantPos: []int{1, 3}},
		{name: "split one position", input: "s 4", wantVerb: triage.VerbSplit, wantPos: []int{4}},
		{name: "target an issue key", input: "t PAAS-4010", wantVerb: triage.VerbTarget, wantText: "PAAS-4010"},
		{name: "whitespace tolerated", input: "  a  ", wantVerb: triage.VerbApprove},
		{name: "unknown verb", input: "z", wantErr: "unknown"},
		{name: "empty line", input: "", wantErr: "unknown"},
		{
			name:    "edit with no text is an error, not an empty correction",
			input:   "e",
			wantErr: "requires text",
		},
		{
			name:    "merge with one position is an error",
			input:   "m 1",
			wantErr: "two positions",
		},
		{
			name:    "merge with a non-numeric position",
			input:   "m 1 x",
			wantErr: "position",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseDecision(tc.input)

			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.wantVerb, got.Verb)
			assert.Equal(t, tc.wantText, got.Text)
			assert.Equal(t, tc.wantPos, got.Positions)
		})
	}
}
```

**Note the `e` case.** An edit with no text must error rather than produce an empty correction, because `reconciler.Redraft` (Task 3) errors on empty feedback — catching it here gives the reviewer a retry prompt instead of a failed LLM round-trip.

- [ ] **Step 2: Run to verify it fails**

```
go test ./cmd/unjira/ -run TestParseDecision -v 2>&1 | tail -12; echo "exit=$?"
```

Expected: `undefined: parseDecision`.

- [ ] **Step 3: Implement the parser and the Prompter**

Create `cmd/unjira/triage.go`:

```go
package main

// triage.go is `unjira triage` — the interactive review surface. It is
// deliberately thin: parse a keystroke, print an action, hand the answer to
// internal/triage. Every decision (what a merge means, which narrative is the
// target, what gets applied) lives in that package, so the session is testable
// without a terminal. See
// docs/superpowers/specs/2026-08-28-triage-design.md.

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/jcogilvie/unjira/internal/gate"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/triage"
)

// triageCmd is `unjira triage`.
type triageCmd struct {
	AutoApprove bool `help:"Approve every action without prompting. Bypasses the prompt AND auto_commit.graduated (the approve path never consults gate.Decide) — only jira[].writable_project_keys still applies."`
	Refresh     bool `help:"Block until any in-flight watch pass finishes, so the batch is not mid-change."`
	DryRun      bool `help:"Walk the batch and show dispositions, but never write to the store or a tracker."`
}

// parseDecision turns one line of reviewer input into a triage.Decision.
//
// Single letters, all distinct: a r e m s k t q. `skip` takes k precisely
// because s belongs to split — a collision here would make one disposition
// unreachable.
//
// Verbs that need an argument error when it is missing rather than defaulting.
// An `e` with no text would reach reconciler.Redraft, which errors on empty
// feedback (by design — blank text means the correction was lost), so catching
// it here turns a wasted LLM round-trip into an immediate re-prompt.
func parseDecision(line string) (triage.Decision, error) {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) == 0 {
		return triage.Decision{}, fmt.Errorf("unknown verb %q: expected one of a r e m s k t q", line)
	}

	verb, rest := fields[0], fields[1:]

	switch verb {
	case "a":
		return triage.Decision{Verb: triage.VerbApprove}, nil
	case "k":
		return triage.Decision{Verb: triage.VerbSkip}, nil
	case "q":
		return triage.Decision{Verb: triage.VerbQuit}, nil
	case "r":
		// Reject takes optional free text: a reviewer may simply not want the
		// action, with nothing to teach slice 7's rules.Distill.
		return triage.Decision{Verb: triage.VerbReject, Text: strings.Join(rest, " ")}, nil
	case "e":
		if len(rest) == 0 {
			return triage.Decision{}, fmt.Errorf("edit requires text: e <what to change>")
		}

		return triage.Decision{Verb: triage.VerbEdit, Text: strings.Join(rest, " ")}, nil
	case "t":
		if len(rest) != 1 {
			return triage.Decision{}, fmt.Errorf("target requires exactly one issue key: t <KEY>")
		}

		return triage.Decision{Verb: triage.VerbTarget, Text: rest[0]}, nil
	case "m":
		if len(rest) != 2 {
			return triage.Decision{}, fmt.Errorf("merge requires two positions: m <n> <n>")
		}
		positions, err := parsePositions(rest)
		if err != nil {
			return triage.Decision{}, err
		}

		return triage.Decision{Verb: triage.VerbMerge, Positions: positions}, nil
	case "s":
		if len(rest) != 1 {
			return triage.Decision{}, fmt.Errorf("split requires one position: s <n>")
		}
		positions, err := parsePositions(rest)
		if err != nil {
			return triage.Decision{}, err
		}

		return triage.Decision{Verb: triage.VerbSplit, Positions: positions}, nil
	default:
		return triage.Decision{}, fmt.Errorf("unknown verb %q: expected one of a r e m s k t q", verb)
	}
}

func parsePositions(fields []string) ([]int, error) {
	out := make([]int, 0, len(fields))
	for _, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil {
			return nil, fmt.Errorf("position %q is not a number", f)
		}
		if n < 1 {
			return nil, fmt.Errorf("position %d is not valid: batch positions start at 1", n)
		}
		out = append(out, n)
	}

	return out, nil
}

// terminalPrompter implements triage.Prompter against stdin/stdout. It is the
// only thing in this slice that touches a terminal.
type terminalPrompter struct {
	in  *bufio.Scanner
	out io.Writer
}

func newTerminalPrompter() *terminalPrompter {
	return &terminalPrompter{in: bufio.NewScanner(os.Stdin), out: os.Stdout}
}

// Ask prints one action in full — body and rationale, never truncated — then
// reads a disposition. Full bodies are deliberate: a reviewer must not approve
// text they did not read, and real drafted bodies run past 200 characters, so a
// one-line summary would invite exactly that.
func (p *terminalPrompter) Ask(item triage.Item) (triage.Decision, error) {
	a := item.Action

	fmt.Fprintf(p.out, "\n[%d/%d] %s  %s  confidence %.2f\n",
		item.Position, item.Total, a.Type, a.IssueKey, a.Confidence)
	fmt.Fprintf(p.out, "%s\n", indent(bodyOf(a), "  "))
	if a.Rationale != "" {
		fmt.Fprintf(p.out, "\n  why: %s\n", a.Rationale)
	}

	for {
		fmt.Fprint(p.out, "\n  [a]pprove [r]eject [e]dit [m]erge [s]plit s[k]ip [t]arget [q]uit > ")

		if !p.in.Scan() {
			if err := p.in.Err(); err != nil {
				return triage.Decision{}, fmt.Errorf("reading disposition: %w", err)
			}
			// EOF (piped input exhausted, or Ctrl-D): treat as quit rather than
			// looping forever on a closed stdin.
			return triage.Decision{Verb: triage.VerbQuit}, nil
		}

		d, err := parseDecision(p.in.Text())
		if err != nil {
			// A typo re-prompts rather than aborting: losing a half-reviewed
			// batch to a fat finger would be worse than the noise.
			fmt.Fprintf(p.out, "  %v\n", err)

			continue
		}

		return d, nil
	}
}

// Confirm is the single gate before anything is written.
func (p *terminalPrompter) Confirm(summary string) (bool, error) {
	fmt.Fprintf(p.out, "\n== %s ==\napply? [y/N] ", summary)

	if !p.in.Scan() {
		return false, p.in.Err()
	}

	answer := strings.ToLower(strings.TrimSpace(p.in.Text()))

	return answer == "y" || answer == "yes", nil
}

// bodyOf extracts the human-readable text from an action's JSON payload for
// display. A payload that will not decode is shown raw rather than hidden: the
// reviewer needs to see that it is malformed, since gate.Applier would fail on
// it too.
func bodyOf(a store.ActionRow) string {
	var p struct {
		Body    string `json:"body"`
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal([]byte(a.Payload), &p); err != nil {
		return a.Payload
	}
	if p.Body != "" {
		return p.Body
	}
	if p.Summary != "" {
		return p.Summary
	}

	return a.Payload
}

func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}

	return strings.Join(lines, "\n")
}
```

**Add `encoding/json` and `io` to the imports** — `bodyOf` needs the first, `terminalPrompter.out` the second.

- [ ] **Step 4: Run**

```
go test ./cmd/unjira/ -run TestParseDecision -v 2>&1 | tail -20; echo "exit=$?"
```

Expected: all subtests PASS.

- [ ] **Step 5: Register the command**

In `cmd/unjira/main.go`'s `cli` struct, after `Actions actionsCmd`:

```go
	Triage  triageCmd  `cmd:"" help:"Review the queue one action at a time: approve, reject, reword, re-cluster, retarget. Applies nothing until you confirm."`
```

- [ ] **Step 6: Implement `Run`, wiring Session to the store and Applier**

`triageCmd.Run` loads `ActionsByStatus("proposed")`, builds the `Applier` exactly as `approveAction` does (`gate.NewApplier(a.store, writer, a.config.Tracker.DefaultProject, a.config.Jira)` — all four args), constructs the Session with a `terminalPrompter`, runs it, and on confirmation applies the approved set.

`--refresh` calls `store.Acquire` (blocking) before loading the batch; release it after. `--dry-run` skips both `Persist` and the apply, and **says which stages it skipped** rather than going quiet — matching `watch --dry-run`'s discipline.

- [ ] **Step 7: Commit**

```bash
git add cmd/unjira/triage.go cmd/unjira/triage_test.go cmd/unjira/main.go
git commit -m "cmd: unjira triage — the terminal shell over triage.Session

Thin by construction: parse a keystroke, print an action, hand the answer
to internal/triage. Verb parsing is the only logic here, so it is the only
thing unit-tested at this layer.

Keys a r e m s k t q, all distinct — skip takes k because s belongs to
split. Verbs needing an argument error when it is missing: an empty edit
would reach reconciler.Redraft, which rejects empty feedback by design, so
catching it here turns a wasted LLM round-trip into a re-prompt.

A parse error re-prompts rather than aborting; losing a half-reviewed batch
to a typo would be worse than the noise. EOF is treated as quit rather than
looping on a closed stdin.

Bodies print in full. Real drafted bodies run past 200 characters, and a
truncated summary would invite approving text nobody read."
```

---

### Task 8: `--auto-approve`, and what it does not bypass

**Files:**
- Modify: `cmd/unjira/triage.go`
- Test: `cmd/unjira/triage_test.go`

- [ ] **Step 1: Write the failing test**

```go
// TestTriage_AutoApproveRespectsWriteScope pins what --auto-approve actually
// protects, and deliberately does NOT claim more.
//
// gate.Applier enforces exactly one gate: jira[].writable_project_keys.
// Graduated and ConfidenceFloor live in gate.Decide, which the approve path
// never calls — by design, since the auto-commit gate governs UNATTENDED
// writes and a human passing this flag is attending. A test asserting all
// three gates hold would encode a safety property that does not exist.
func TestTriage_AutoApproveRespectsWriteScope(t *testing.T) {
	s := triageTestStore(t)
	paas := seedProposedAction(t, s, "PAAS-1")
	devsbx := seedProposedAction(t, s, "DEVSBX-1")

	writer := &recordingWriter{}
	applier := gate.NewApplier(s, writer, "DEVSBX", []config.JiraConnection{{
		Name:                "dev",
		ProjectKeys:         []string{"PAAS", "DEVSBX"},
		WritableProjectKeys: []string{"DEVSBX"},
	}})

	// Auto-approve both.
	require.Error(t, applier.Apply(paas), "PAAS is not writable")
	require.NoError(t, applier.Apply(devsbx), "DEVSBX is writable")

	assert.Equal(t, []string{"AddComment:DEVSBX-1"}, writer.calls,
		"only the writable-project action reached the tracker")

	failed, err := s.GetAction(paas.ID)
	require.NoError(t, err)
	assert.Equal(t, "failed", failed.Status)
	assert.Contains(t, failed.Error, "writable_project_keys",
		"the refusal reason must name the config key, so an operator knows what to change")
}
```

Reuse `recordingWriter` from `internal/gate`'s tests if it is exported for reuse; otherwise define a local one implementing `tasktracker.TaskWriter`'s three methods and recording call strings.

- [ ] **Step 2: `--help` must not overstate the flag**

The flag's help string (Task 7, Step 3) already says it bypasses `auto_commit.graduated`. Verify it renders:

```
go run ./cmd/unjira triage --help 2>&1 | grep -A 3 auto-approve; echo "exit=$?"
```

Expected: the text names both what it bypasses and what still applies. **This wording is the whole safety story for this flag** — a reviewer who believes `Graduated: false` protects them here would be wrong.

- [ ] **Step 3: Commit**

```bash
git add cmd/unjira/triage.go cmd/unjira/triage_test.go
git commit -m "cmd: --auto-approve, and an honest --help about it

The flag skips the prompt for every action. It does NOT skip
auto_commit.graduated or confidence_floor, because the approve path never
consults gate.Decide — that gate governs unattended writes, and a human
typing this flag is attending. Only jira[].writable_project_keys still
stands between it and a write.

The test asserts exactly that and no more: PAAS refused with the config key
named in actions.error, DEVSBX applied, one tracker call. An earlier draft
of the design doc claimed all three gates held here and asked for a test
proving it — which would have encoded a safety property that does not
exist."
```

---

### Task 9: resolve the applied-vs-failed watermark inconsistency

Task 6 flagged this and it must not survive into the merge. `EligibleEventIDs` (Task 1) filters on `executed_at IS NOT NULL`, which matches **both** `applied` and `failed`. `hasCommittedAction` (Task 6) filters on `status == "applied"`. So a narrative whose only write *failed* is "uncommitted" for merge direction but carries a *frozen* watermark.

**Resolution: filter both on `applied`.** A failed write mutated nothing in the tracker — no comment posted, no status moved, no issue created — so it must not freeze events either. The alternative (a failed write might have partially landed) is not real for these three action types: `AddComment`, `SetStatus`, and `CreateIssue` are each a single API call that either happened or did not.

**Files:**
- Modify: `internal/store/eligibility.go`
- Test: `internal/store/eligibility_test.go`

- [ ] **Step 1: Write the failing test**

```go
// TestEligibleEventIDs_AFailedWriteDoesNotFreeze: executed_at is stamped for
// both applied AND failed, because a failed attempt still attempted a write.
// But a failed write changed nothing in the tracker, so it must not freeze
// events — otherwise a narrative whose only write failed would be
// unrestructurable forever, for no reason a human could act on.
func TestEligibleEventIDs_AFailedWriteDoesNotFreeze(t *testing.T) {
	s := openStore(t)
	nid, ids := seedNarrativeWithEvents(t, s, 2)

	id, err := s.InsertAction(store.ActionRow{
		NarrativeID: nid, Type: "comment", IssueKey: "PROJ-1",
		Payload: `{"body":"never landed"}`, Status: "proposed",
	})
	require.NoError(t, err)
	require.NoError(t, s.UpdateActionStatus(id, "failed"))

	got, err := s.EligibleEventIDs(nid)

	require.NoError(t, err)
	assert.ElementsMatch(t, ids, got,
		"a failed write mutated nothing, so it must not freeze the narrative's events")
}
```

- [ ] **Step 2: Run to verify it fails**

```
go test ./internal/store/ -run TestEligibleEventIDs_AFailedWrite -v 2>&1 | tail -10; echo "exit=$?"
```

Expected: FAIL — the events come back empty, because `executed_at` is set.

- [ ] **Step 3: Fix the query**

In `internal/store/eligibility.go`, add `AND status = 'applied'` to **both** subqueries:

```go
		`SELECT ne.event_id
		 FROM narrative_events ne
		 WHERE ne.narrative_id = ?
		   AND (
		     (SELECT max(executed_at) FROM actions
		       WHERE narrative_id = ? AND status = 'applied') IS NULL
		     OR ne.linked_at > (SELECT max(executed_at) FROM actions
		       WHERE narrative_id = ? AND status = 'applied')
		   )
		 ORDER BY ne.event_id`,
```

And extend the doc comment:

```go
// Only status='applied' counts, not merely executed_at being set.
// UpdateActionStatus stamps executed_at for failed writes too — a failed
// attempt still attempted one — but a failed write posted no comment, moved no
// status, and created no issue, so there is nothing for the watermark to
// protect. Freezing on failure would make a narrative whose only write failed
// permanently unrestructurable, for a reason no human could act on. This
// matches triage's hasCommittedAction, deliberately: the two disagreeing was a
// real inconsistency caught while planning, not a subtlety.
```

- [ ] **Step 4: Run the full store suite**

```
go test ./internal/store/ 2>&1 | tail -4; echo "exit=$?"
```

Expected: the new test passes and every earlier eligibility test still does — `FullyCommittedIsFullyFrozen` uses `applied`, so it is unaffected.

- [ ] **Step 5: Commit**

```bash
git add internal/store/eligibility.go internal/store/eligibility_test.go
git commit -m "store: only an APPLIED action freezes events, not a failed one

EligibleEventIDs filtered on executed_at IS NOT NULL, which matches failed
writes too, while triage's hasCommittedAction filtered on status='applied'.
The two disagreed: a narrative whose only write failed was 'uncommitted'
for merge direction but carried a frozen watermark.

Resolved toward applied. A failed write posted no comment, moved no status,
and created no issue, so the watermark has nothing to protect — and
freezing on failure would leave that narrative permanently unrestructurable
for a reason no human could act on."
```

---

### Task 10: full offline gate

- [ ] **Step 1: Run every check, recording real output**

```
go build ./... ; echo "exit=$?"
go test ./... ; echo "exit=$?"
go test -race ./... ; echo "exit=$?"
go vet -tags=live ./... ; echo "exit=$?"
~/.asdf/installs/golang/1.25.7/bin/golangci-lint run ./... ; echo "exit=$?"
```

Lint must print `0 issues.` — remember the asdf shim silently no-ops, so use the absolute path.

- [ ] **Step 2: Count tests honestly**

```
go test -count=1 ./... -v 2>/dev/null | grep -c "^--- PASS"
go test -count=1 ./... -v 2>/dev/null | grep -cE "^\s+--- PASS"
go test -count=1 ./... -v 2>/dev/null | grep -cE "(^|\s)--- SKIP"
```

Baseline before this slice: 474 top-level + 128 subtests = 602, 0 skipped. Report the real new numbers; do not estimate.

- [ ] **Step 3: `earthly +reviewable`**

```
earthly +reviewable
```

This is the authoritative gate — it must say SUCCESS. It also runs in a container, which is where `internal/llm`'s `WaitDelay` timeout test can actually fail (it cannot fail on darwin; see its doc comment).

---

### Task 11: live-tier test — compile it, do not run it

**Files:**
- Create or extend: `internal/live/triage_test.go`

- [ ] **Step 1: Write a live test for the one thing fakes cannot prove**

Follow `internal/live/autocommit_test.go` exactly: `//go:build live`, the `UNJIRA_LIVE=1` gate, config built **in-process** (never reading the real `unjira.config.json`), a throwaway issue created and deleted via `t.Cleanup`, and `WritableProjectKeys` set explicitly to `testProject()`.

The valuable assertion is the retarget path end to end: create two throwaway issues, seed a narrative linked to the first, run retarget onto the second, and confirm via a fresh `GetIssue` that the comment landed on the **second** issue and not the first.

- [ ] **Step 2: Compile it, do not run it**

```
go vet -tags=live ./internal/live/ ; echo "exit=$?"
```

**Do NOT run `UNJIRA_LIVE=1 go test -tags=live`.** It writes to real Jira. Say in the report that it compiles and is unrun, and let the operator decide — the same handling PR #24's live test got.

---

### Task 12: docs, in this PR

Per `CLAUDE.md`'s "Keep the docs true in the PR that changes the code" — an audit on 2026-08-27 found the README claiming "Zero write risk" while the write path was live, and every instance traced to landing without touching the docs.

- [ ] **Step 1: README**

- Add `triage` to the CLI line in **Layout** (currently `collect | digest | status | watch | actions | dev`).
- Add `internal/triage/` to the layout block with a one-line purpose.
- In **Status**, note that the review surface now exists: `actions` is the machine-facing half, `triage` the human-facing one. Slice 6 moves from 🚧 to ✅.
- In **Quickstart**, add `./unjira triage` after the `actions` examples, and say it applies nothing until you confirm.

- [ ] **Step 2: The triage design spec**

Move `Status: design, approved by the user 2026-08-28` to `## Status: landed <date>`, with:
- the real test count from Task 10
- the `--auto-approve` correction (it bypasses `Graduated`, not just the prompt)
- the merge-needs-an-unlink correction
- the applied-vs-failed resolution from Task 9
- whether the live test was run

Leave the original reasoning as written. Do not edit the design into looking like it predicted what shipped.

- [ ] **Step 3: The phase-1 spec**

Slice 6's entry currently says "🚧 first half landed" and names `triage` as remaining. Update to ✅ with what actually shipped, and note that `unjira rules list|decide` is still slice 7's.

- [ ] **Step 4: `docs/design-notes.md` — one new numbered incident**

This slice produced a genuine architectural lesson, which is the bar that file sets (it is why-we-are-shaped-this-way, not a changelog):

**A per-entity watermark is defeated by moving the entity.** The commit watermark froze events per narrative; a merge could relink a frozen event onto a never-committed narrative and thaw it. Fixed not by guarding the move but by making direction determined — the committed narrative is always the target, so frozen events are never relinked. The general lesson: when a rule is keyed on an entity's *current* parent, any operation that reparents can launder it, and the durable fix is to make the unsafe reparenting inexpressible rather than checked.

- [ ] **Step 5: Commit and open the PR**

```bash
git add README.md docs/
git commit -m "docs: triage landed — README, specs, and a new design note"
```

PR body must state plainly: what was verified, what was drilled, the live test's unrun status, and the corrections this slice made to its own design doc.
