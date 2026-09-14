package store_test

// remaindercounts_test.go pins the two counts F10 added to the queries they are
// meant to describe.
//
// The risk these cover is not "does COUNT(*) work". It is DRIFT. Each count
// duplicates its selection query's WHERE clause, so a future edit to one and not
// the other yields a count that is still a valid number and still plausible —
// it just describes a different population than the pass actually examined. A
// remainder that lies is worse than no remainder, because the operator stops
// re-running while a backlog waits.
//
// So each test asserts the count equals the length of an UNBOUNDED selection over
// the same seeded data, rather than asserting a hard-coded number. A hard-coded
// expectation would keep passing if BOTH queries drifted the same way; comparing
// them to each other is what actually catches divergence.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/store"
)

// bigEnoughToHoldEverything is a selection limit no test here comes near, so the
// selection returns its whole population and its length IS the remainder.
const bigEnoughToHoldEverything = 1000

func TestCountNarrativesWithoutPrimaryLink_MatchesItsSelection(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 9, 2, 9, 0, 0, 0, time.UTC)

	// Three unmatched, one attributed. The attributed one is what makes this test
	// meaningful: a count that forgot the predicate entirely would return 4 and
	// the selection would return 3.
	for _, title := range []string{"unmatched one", "unmatched two", "unmatched three"} {
		_, err := s.InsertNarrative(base, base.Add(time.Hour), title, "s")
		require.NoError(t, err)
	}

	// Attribution is a primary LINK ROW, not a column — see F11. Expressed this
	// way so the test survives the column's removal rather than pinning it.
	attributed, err := s.InsertNarrative(base, base.Add(time.Hour), "already attributed", "s")
	require.NoError(t, err)
	require.NoError(t, s.AddNarrativeIssues(attributed, []store.NarrativeIssue{
		{IssueKey: "DEVSBX-1", Role: store.RolePrimary, Provenance: "test", Confidence: 0.9},
	}))

	selected, err := s.NarrativesWithoutPrimaryLink(bigEnoughToHoldEverything)
	require.NoError(t, err)

	counted, err := s.CountNarrativesWithoutPrimaryLink()
	require.NoError(t, err)

	assert.Equal(t, len(selected), counted,
		"the count and the selection must describe the same population; if they diverge, "+
			"the rendered remainder describes narratives a matching pass would not have looked at")
	assert.Equal(t, 3, counted, "and the attributed narrative must be excluded")
}

func TestCountNarrativesWithActionableLinks_MatchesItsSelection(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 9, 2, 9, 0, 0, 0, time.UTC)

	roles := []store.Role{"primary", "same_work", "mentioned"}

	// One narrative per actionable role, plus one with a role OUTSIDE the set and
	// one with no link at all. Both exclusions are load-bearing: the reconciler
	// selects on role, so a count that ignored the IN clause would report a
	// backlog containing narratives no reconcile pass would ever consider.
	for _, role := range roles {
		id, err := s.InsertNarrative(base, base.Add(time.Hour), "actionable "+string(role), "s")
		require.NoError(t, err)
		require.NoError(t, s.AddNarrativeIssues(id, []store.NarrativeIssue{
			{IssueKey: "DEVSBX-2", Role: role, Provenance: "test", Confidence: 0.9},
		}))
	}

	unactionable, err := s.InsertNarrative(base, base.Add(time.Hour), "linked but not actionable", "s")
	require.NoError(t, err)
	require.NoError(t, s.AddNarrativeIssues(unactionable, []store.NarrativeIssue{
		{IssueKey: "DEVSBX-3", Role: "co_representation", Provenance: "test", Confidence: 0.9},
	}))

	_, err = s.InsertNarrative(base, base.Add(time.Hour), "no links at all", "s")
	require.NoError(t, err)

	selected, err := s.NarrativesWithActionableLinks(bigEnoughToHoldEverything, roles)
	require.NoError(t, err)

	counted, err := s.CountNarrativesWithActionableLinks(roles)
	require.NoError(t, err)

	assert.Equal(t, len(selected), counted,
		"the count must mirror NarrativesWithActionableLinks' predicate exactly")
	assert.Equal(t, len(roles), counted,
		"and neither the unactionable-role nor the unlinked narrative may be counted")
}

// TestCountNarrativesWithActionableLinks_CountsANarrativeOnce is the bug the
// EXISTS subquery exists to prevent. A narrative legitimately carries several
// links — that is the whole point of NarrativeIssue's multi-row design — so a
// count written as a JOIN would return one row per LINK and inflate the
// remainder. The selection has the same shape, so both would be wrong together;
// this asserts the absolute number rather than only agreement.
func TestCountNarrativesWithActionableLinks_CountsANarrativeOnce(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 9, 2, 9, 0, 0, 0, time.UTC)

	roles := []store.Role{"primary", "same_work", "mentioned"}

	id, err := s.InsertNarrative(base, base.Add(time.Hour), "one narrative, three links", "s")
	require.NoError(t, err)
	require.NoError(t, s.AddNarrativeIssues(id, []store.NarrativeIssue{
		{IssueKey: "DEVSBX-4", Role: "primary", Provenance: "test", Confidence: 0.9},
		{IssueKey: "DEVSBX-5", Role: "same_work", Provenance: "test", Confidence: 0.8},
		{IssueKey: "DEVSBX-6", Role: "mentioned", Provenance: "test", Confidence: 0.7},
	}))

	counted, err := s.CountNarrativesWithActionableLinks(roles)
	require.NoError(t, err)

	assert.Equal(t, 1, counted,
		"three links on one narrative is one narrative to reconcile, not three")
}

// TestCountNarrativesWithActionableLinks_NoRolesCountsNothing mirrors the
// selection's own guard. Both return early on an empty role set rather than
// building `role IN ()`, which SQLite rejects — and a caller passing no roles is
// asking about an empty population, so 0 is the honest answer rather than an error.
func TestCountNarrativesWithActionableLinks_NoRolesCountsNothing(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 9, 2, 9, 0, 0, 0, time.UTC)

	id, err := s.InsertNarrative(base, base.Add(time.Hour), "linked", "s")
	require.NoError(t, err)
	require.NoError(t, s.AddNarrativeIssues(id, []store.NarrativeIssue{
		{IssueKey: "DEVSBX-7", Role: "primary", Provenance: "test", Confidence: 0.9},
	}))

	counted, err := s.CountNarrativesWithActionableLinks(nil)
	require.NoError(t, err, "an empty role set is a question, not a malformed query")
	assert.Zero(t, counted)
}
