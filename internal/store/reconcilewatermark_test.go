package store_test

// reconcilewatermark_test.go covers finding F26: a narrative whose entire delta is
// unjira's own output (every event carries events.ArtifactAuthoredByUnjira) is
// selected by the reconciler, emptied by reconciler.dropSelfAuthored, and hits
// SkippedNoDelta writing nothing — every pass, forever, because
// NarrativesWithActionableLinks' (window_start, id) order is stable and this outcome
// leaves no trace for hasUnexaminedDelta to see.
//
// Third instance of design-notes #29's shape, after F12 (StatusSuppressed, an
// action-table watermark) and F22 (match_examinations, a side-table watermark). This
// follows F22's shape rather than F12's: dropSelfAuthored runs in the reconciler,
// after the store-level delta test already passed, so there is no action to persist —
// nothing was drafted, nothing was suppressed by a drafted action's filter chain. A
// side table recording "examined, and it was all self-authored" is the fix, the same
// way match_examinations records "examined, and there were no candidates."
//
// This file tests the store half only: the schema, the predicate, and the watermark
// property. internal/reconciler/reconciler_test.go covers the caller wiring
// (dropSelfAuthored -> RecordReconcileExamined) and the end-to-end livelock repro.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/store"
)

// seedActionableLinked creates a narrative with a primary link and one linked event —
// the shape NarrativesWithActionableLinks selects.
func seedActionableLinked(t *testing.T, s *store.Store, key, extID string, at time.Time) int64 {
	t.Helper()

	id, err := s.InsertNarrative(at, at.Add(time.Hour), "work on "+key, "summary")
	require.NoError(t, err)
	require.NoError(t, s.AddNarrativeIssues(id, []store.NarrativeIssue{
		{IssueKey: key, Role: store.RolePrimary, Provenance: "test", Confidence: 0.9},
	}))

	_, err = s.InsertEvent(events.NewEvent("jira", extID, at.Add(30*time.Minute), "did the work"))
	require.NoError(t, err)

	eventID, err := s.EventIDByExternalID("jira", extID)
	require.NoError(t, err)
	require.NoError(t, s.AddNarrativeEvents(id, []int64{eventID}))

	return id
}

// TestNarrativesWithActionableLinks_SkipsExaminedSelfAuthoredOnly is the fix. A
// narrative recorded as "examined, delta was entirely self-authored" must yield to
// the narratives behind it in the stable selection order, or the backlog never
// drains — exactly the shape measured for F26: 7 narratives re-selected every pass,
// emptied, and hitting SkippedNoDelta.
func TestNarrativesWithActionableLinks_SkipsExaminedSelfAuthoredOnly(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 9, 17, 9, 0, 0, 0, time.UTC)

	selfAuthored := seedActionableLinked(t, s, "DEVSBX-1", "sa:1", base)
	realWork := seedActionableLinked(t, s, "DEVSBX-2", "sa:2", base.Add(time.Minute))

	// Before the watermark: a cap of 1 returns only the older one, forever.
	got, err := s.NarrativesWithActionableLinks(1, reconcileRoles)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, selfAuthored, got[0].ID, "stable order puts the older one first")

	require.NoError(t, s.RecordReconcileExamined(selfAuthored, "every delta event is authored_by_unjira"))

	// After: the same cap reaches the narrative behind it.
	got, err = s.NarrativesWithActionableLinks(1, reconcileRoles)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, realWork, got[0].ID,
		"an examined self-authored-only narrative must yield, or a real narrative behind it in the "+
			"stable order is never reached")
}

// TestNarrativesWithActionableLinks_ReconcileExaminedIsAWatermarkNotATombstone is the
// property that keeps this from being data loss. New events can arrive later —
// including from a source other than unjira's own writes — and must re-open the
// narrative for reconciliation.
func TestNarrativesWithActionableLinks_ReconcileExaminedIsAWatermarkNotATombstone(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 9, 17, 9, 0, 0, 0, time.UTC)

	id := seedActionableLinked(t, s, "DEVSBX-3", "sa:3", base)
	require.NoError(t, s.RecordReconcileExamined(id, "every delta event is authored_by_unjira"))

	got, err := s.NarrativesWithActionableLinks(10, reconcileRoles)
	require.NoError(t, err)
	require.Empty(t, got, "examined and entirely self-authored: correctly skipped")

	// A later, non-self-authored event links past the watermark.
	e := events.NewEvent("claude_code", "later-commit", base.Add(2*time.Hour), "real work")
	_, err = s.InsertEvent(e)
	require.NoError(t, err)
	evtID, err := s.EventIDByExternalID(e.Source, e.ExternalID)
	require.NoError(t, err)
	require.NoError(t, s.AddNarrativeEvents(id, []int64{evtID}))

	got, err = s.NarrativesWithActionableLinks(10, reconcileRoles)
	require.NoError(t, err)
	require.Len(t, got, 1,
		"new work past the watermark must re-open the narrative: recording 'never reconcile this' "+
			"would make an early self-authored-only delta permanent")
	assert.Equal(t, id, got[0].ID)
}

// TestCountNarrativesWithDelta_MatchesTheSelectorAfterReconcileExamined guards the
// invariant both F22's and F12's precedents state explicitly: the count and the
// selector must move together, or the remainder reports progress against a
// population the pass never examined.
func TestCountNarrativesWithDelta_MatchesTheSelectorAfterReconcileExamined(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 9, 17, 9, 0, 0, 0, time.UTC)

	a := seedActionableLinked(t, s, "DEVSBX-4", "sa:4", base)
	seedActionableLinked(t, s, "DEVSBX-5", "sa:5", base.Add(time.Minute))
	seedActionableLinked(t, s, "DEVSBX-6", "sa:6", base.Add(2*time.Minute))

	count, err := s.CountNarrativesWithDelta(reconcileRoles)
	require.NoError(t, err)
	selected, err := s.NarrativesWithActionableLinks(100, reconcileRoles)
	require.NoError(t, err)
	require.Len(t, selected, count, "before any watermark")

	require.NoError(t, s.RecordReconcileExamined(a, "every delta event is authored_by_unjira"))

	count, err = s.CountNarrativesWithDelta(reconcileRoles)
	require.NoError(t, err)
	selected, err = s.NarrativesWithActionableLinks(100, reconcileRoles)
	require.NoError(t, err)
	assert.Len(t, selected, count,
		"a count the selector disagrees with is how a backlog gets reported that the pass cannot "+
			"actually reach")
	assert.Equal(t, 2, count)
}

// TestRecordReconcileExamined_IsIdempotentPerPass keeps a re-examination from
// accumulating rows without bound — the same property RecordMatchExamined pins for
// its own table.
func TestRecordReconcileExamined_IsIdempotentPerPass(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 9, 17, 9, 0, 0, 0, time.UTC)

	id := seedActionableLinked(t, s, "DEVSBX-7", "sa:7", base)

	require.NoError(t, s.RecordReconcileExamined(id, "every delta event is authored_by_unjira"))
	require.NoError(t, s.RecordReconcileExamined(id, "every delta event is authored_by_unjira"))
	require.NoError(t, s.RecordReconcileExamined(id, "still all self-authored"))

	got, err := s.NarrativesWithActionableLinks(10, reconcileRoles)
	require.NoError(t, err)
	assert.Empty(t, got, "repeated examination stays skipped")

	n, err := s.CountReconcileExaminations(id)
	require.NoError(t, err)
	assert.Equal(t, 1, n,
		"one watermark per narrative, not one per pass: a pass that re-examines must not grow the "+
			"table without bound")
}
