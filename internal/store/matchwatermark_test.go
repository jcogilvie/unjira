package store_test

// matchwatermark_test.go covers finding F22: matching livelocked on narratives that
// name no ticket anywhere, and every create proposal sat behind them.
//
// The mechanism, measured on a freshly rebuilt store: NarrativesWithoutPrimaryLink
// selects ORDER BY (window_start, id) LIMIT 20 — a STABLE order — and matchOne
// correctly writes nothing when gatherCandidates finds no keys. Stable order plus an
// outcome that leaves no trace is a livelock: eleven consecutive passes all reported
// "49 narrative(s) still unmatched", never 48. Of those 49, 33 had zero candidate
// keys, and 18 of the first 20 selected were among them.
//
// Worse than a stalled counter: creates are gated on matching being caught up (F13's
// precondition), so a count that can never reach zero means NO create is ever
// proposed and the review queue cannot rebuild at all.
//
// This is design-notes #29 in a path that fix never touched. The reconciler solved it
// by writing a StatusSuppressed row per examined narrative; this is the matching-side
// equivalent.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/store"
)

// watermarkStore opens a temp store.
func watermarkStore(t *testing.T) *store.Store {
	t.Helper()

	s, err := store.Open(t.TempDir() + "/wm.db")
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	return s
}

// seedNarrative inserts one narrative dated at base+offset, so the stable
// (window_start, id) order is predictable.
func seedNarrative(t *testing.T, s *store.Store, title string, at time.Time) int64 {
	t.Helper()

	id, err := s.InsertNarrative(at, at.Add(time.Hour), title, "s")
	require.NoError(t, err)

	return id
}

// TestNarrativesWithoutPrimaryLink_SkipsExaminedWithNoCandidates is the fix. A
// narrative recorded as examined-with-nothing-to-match must yield to the ones behind
// it, or the backlog never drains.
func TestNarrativesWithoutPrimaryLink_SkipsExaminedWithNoCandidates(t *testing.T) {
	s := watermarkStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	unmatchable := seedNarrative(t, s, "session naming no ticket", base)
	matchable := seedNarrative(t, s, "session naming PAAS-1", base.Add(time.Minute))

	// Before the watermark: a cap of 1 returns only the first, forever.
	got, err := s.NarrativesWithoutPrimaryLink(1)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, unmatchable, got[0].ID, "stable order puts the older one first")

	require.NoError(t, s.RecordMatchExamined(unmatchable, "no candidate keys in any event"))

	// After: the same cap reaches the one behind it.
	got, err = s.NarrativesWithoutPrimaryLink(1)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, matchable, got[0].ID,
		"an examined narrative must yield, or the 16 with real candidates sit behind the 33 without "+
			"them permanently")
}

// TestNarrativesWithoutPrimaryLink_ExaminedIsAWatermarkNotATombstone is the property
// that keeps this from being data loss. A narrative can gain events later via
// ClusterExtends, and those events can carry keys it did not have when examined — so
// a new event past the watermark must bring it back.
func TestNarrativesWithoutPrimaryLink_ExaminedIsAWatermarkNotATombstone(t *testing.T) {
	s := watermarkStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	id := seedNarrative(t, s, "session naming no ticket yet", base)
	require.NoError(t, s.RecordMatchExamined(id, "no candidate keys"))

	got, err := s.NarrativesWithoutPrimaryLink(10)
	require.NoError(t, err)
	require.Empty(t, got, "examined and quiet: correctly skipped")

	// An extend links a new event, dated after the examination.
	e := events.NewEvent("claude_code", "later-commit", base.Add(2*time.Hour), "a later commit")
	_, err = s.InsertEvent(e)
	require.NoError(t, err)
	evtID, err := s.EventIDByExternalID(e.Source, e.ExternalID)
	require.NoError(t, err)
	require.NoError(t, s.AddNarrativeEvents(id, []int64{evtID}))

	got, err = s.NarrativesWithoutPrimaryLink(10)
	require.NoError(t, err)
	require.Len(t, got, 1,
		"new work past the watermark must re-open the narrative for matching: recording 'never match "+
			"this' would make an early absence permanent")
	assert.Equal(t, id, got[0].ID)
}

// TestCountNarrativesWithoutPrimaryLink_MatchesTheSelector guards the invariant the
// selector's own doc comment warns about: a count that disagrees with the selector
// once crashed a drain (F11's neighbourhood). The two predicates must move together.
func TestCountNarrativesWithoutPrimaryLink_MatchesTheSelector(t *testing.T) {
	s := watermarkStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	a := seedNarrative(t, s, "one", base)
	seedNarrative(t, s, "two", base.Add(time.Minute))
	seedNarrative(t, s, "three", base.Add(2*time.Minute))

	count, err := s.CountNarrativesWithoutPrimaryLink()
	require.NoError(t, err)
	selected, err := s.NarrativesWithoutPrimaryLink(100)
	require.NoError(t, err)
	require.Len(t, selected, count, "before any watermark")

	require.NoError(t, s.RecordMatchExamined(a, "no candidate keys"))

	count, err = s.CountNarrativesWithoutPrimaryLink()
	require.NoError(t, err)
	selected, err = s.NarrativesWithoutPrimaryLink(100)
	require.NoError(t, err)
	assert.Len(t, selected, count,
		"and after: a count the selector disagrees with is how the deferred-creates message "+
			"reports a backlog that cannot be reached")
	assert.Equal(t, 2, count)
}

// TestRecordMatchExamined_IsIdempotentPerPass keeps a re-examination from
// accumulating rows without bound. The narrative is examined again whenever new
// events arrive, and each examination should replace the prior watermark rather than
// stack on it.
func TestRecordMatchExamined_IsIdempotentPerPass(t *testing.T) {
	s := watermarkStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	id := seedNarrative(t, s, "quiet session", base)

	require.NoError(t, s.RecordMatchExamined(id, "no candidate keys"))
	require.NoError(t, s.RecordMatchExamined(id, "no candidate keys"))
	require.NoError(t, s.RecordMatchExamined(id, "still none"))

	got, err := s.NarrativesWithoutPrimaryLink(10)
	require.NoError(t, err)
	assert.Empty(t, got, "repeated examination stays skipped")

	n, err := s.CountMatchExaminations(id)
	require.NoError(t, err)
	assert.Equal(t, 1, n,
		"one watermark per narrative, not one per pass: a pass that re-examines must not grow the "+
			"table without bound")
}

// TestNarrativesWithoutPrimaryLink_APrimaryLinkStillWins confirms the watermark did
// not become the only predicate. A narrative that acquires a primary link is out of
// the backlog whether or not it was ever examined.
func TestNarrativesWithoutPrimaryLink_APrimaryLinkStillWins(t *testing.T) {
	s := watermarkStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	id := seedNarrative(t, s, "matched session", base)
	require.NoError(t, s.AddNarrativeIssues(id, []store.NarrativeIssue{
		{IssueKey: "PAAS-1", Role: store.RolePrimary, Confidence: 0.9, Provenance: "branch"},
	}))

	got, err := s.NarrativesWithoutPrimaryLink(10)
	require.NoError(t, err)
	assert.Empty(t, got)
}
