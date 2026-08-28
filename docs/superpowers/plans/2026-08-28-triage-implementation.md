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
