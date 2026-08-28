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
