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
