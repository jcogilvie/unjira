package correlator_test

// summarycap_test.go covers F16's input half: nothing bounded how much event text
// entered a clustering prompt.
//
// Measured by zeroing each payload site: 93.7% of a 140k-token prompt was the events
// hydrated under context narratives, and the distribution is extreme — 15 events (4.6%)
// held 52% of the characters, the largest a 15,037-char Jira description. Capping
// per-event summaries fits a 365-day window in one call where 90 days previously
// bisected.
//
// The cap REPORTS what it truncated, with real lengths. That is the reviewer's stated
// requirement and it is the right shape: a silent cap reads as "nothing was left out",
// which is the same defect F25 was about, and an operator cannot choose a better value
// without knowing what the current one is cutting.

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/events"
)

// capNarrative builds a context narrative whose events carry summaries of the given
// lengths, in both render fields.
func capNarrative(lengths ...int) correlator.Narrative {
	now := time.Now()

	evts := make([]correlator.Event, 0, len(lengths))
	for i, n := range lengths {
		e := events.NewEvent("jira", string(rune('a'+i)), now, strings.Repeat("x", n))
		evts = append(evts, e)
	}

	return correlator.Narrative{
		ID: 1, Title: "t", Summary: "s",
		WindowStart: now.Add(-time.Hour), WindowEnd: now,
		Events: evts, EligibleEvents: evts,
	}
}

// TestCapEventSummaries_TruncatesAtBothRenderSites is the fix. Events render twice —
// EligibleEvents as numbered candidates, Events as context detail — and an earlier
// attempt that touched only one measured exactly 0.0% (design-notes #32).
func TestCapEventSummaries_TruncatesAtBothRenderSites(t *testing.T) {
	in := []correlator.Narrative{capNarrative(5000)}

	out, report := correlator.CapEventSummariesForTest(in, 500)

	require.Len(t, out, 1)

	// 500 chars plus the truncation marker. Asserted as cap+marker rather than a
	// hand-computed byte count, because "…" is 3 bytes in UTF-8 and the first version
	// of this test guessed 1.
	want := 500 + len(correlator.TruncationMarkerForTest)
	assert.Len(t, out[0].Events[0].Summary, want,
		"Events is the context-detail render site")
	assert.Len(t, out[0].EligibleEvents[0].Summary, want,
		"EligibleEvents is the numbered-candidate render site, and the one that measured 93.7%")

	assert.Equal(t, 1, report.Truncated)
	assert.Equal(t, []int{5000}, report.OriginalLengths,
		"the REAL length must be reported, so an operator can pick a better cap without guessing")
}

// TestCapEventSummaries_ZeroMeansUnlimited keeps the feature inert on arrival. Nobody's
// existing behaviour should shift until they choose a value.
func TestCapEventSummaries_ZeroMeansUnlimited(t *testing.T) {
	in := []correlator.Narrative{capNarrative(15037)}

	out, report := correlator.CapEventSummariesForTest(in, 0)

	assert.Len(t, out[0].Events[0].Summary, 15037, "zero must not truncate")
	assert.Zero(t, report.Truncated)
	assert.Empty(t, report.OriginalLengths)
}

// TestCapEventSummaries_ReportsEveryTruncationWithItsLength is the reporting
// requirement in full. A count alone tells an operator that something was cut but not
// whether raising the cap by 100 or 10,000 would recover it.
func TestCapEventSummaries_ReportsEveryTruncationWithItsLength(t *testing.T) {
	in := []correlator.Narrative{capNarrative(100, 3000, 200, 15037)}

	_, report := correlator.CapEventSummariesForTest(in, 500)

	assert.Equal(t, 2, report.Truncated, "only the two over the cap")
	assert.Equal(t, []int{3000, 15037}, report.OriginalLengths,
		"sorted ascending, so the largest — the one that decides the next cap — reads last")
	assert.Equal(t, 15037, report.LongestOriginal)
}

// TestCapEventSummaries_CountsAnEventOnceEvenThoughItRendersTwice guards the number
// against the double-render. An event in both Events and EligibleEvents is ONE event
// that got truncated, and reporting it twice would overstate what was cut.
func TestCapEventSummaries_CountsAnEventOnceEvenThoughItRendersTwice(t *testing.T) {
	in := []correlator.Narrative{capNarrative(5000)}

	_, report := correlator.CapEventSummariesForTest(in, 500)

	assert.Equal(t, 1, report.Truncated,
		"one event, truncated at two render sites, is one truncation — not two")
}

// TestCapEventSummaries_LeavesTheInputAlone matters because Cluster's caller holds the
// same slice: mutating in place would shrink the summaries the store hydrated, and a
// second pass over the same narratives would truncate an already-truncated string.
func TestCapEventSummaries_LeavesTheInputAlone(t *testing.T) {
	in := []correlator.Narrative{capNarrative(5000)}

	_, _ = correlator.CapEventSummariesForTest(in, 500)

	assert.Len(t, in[0].Events[0].Summary, 5000, "the caller's copy must be untouched")
}

// TestCapEventSummaries_DegenerateInputs keeps callers from needing a guard.
func TestCapEventSummaries_DegenerateInputs(t *testing.T) {
	out, report := correlator.CapEventSummariesForTest(nil, 500)
	assert.Empty(t, out)
	assert.Zero(t, report.Truncated)

	out, report = correlator.CapEventSummariesForTest([]correlator.Narrative{{ID: 1}}, 500)
	assert.Len(t, out, 1, "a narrative with no events is returned unchanged")
	assert.Zero(t, report.Truncated)
}
