package store_test

// corrections_test.go covers the read side of slice 7's input: the reviewer rulings that
// carry free-text feedback.
//
// actions.feedback has been written since triage landed and read by nothing. This is its
// first consumer, and the query's shape is the whole design — which rows count as a
// correction, and which do not.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/store"
)

func correctionsStore(t *testing.T) (*store.Store, int64) {
	t.Helper()

	s, err := store.Open(t.TempDir() + "/c.db")
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	id, err := s.InsertNarrative(time.Now().Add(-time.Hour), time.Now(), "work", "s")
	require.NoError(t, err)

	return s, id
}

// insertRuled inserts an action and then RULES on it, because InsertAction does not stamp
// decided_at — only the status-update path does (actions.go: "any human ruling sets
// decided_at"). Building the row directly in its final status would leave decided_at NULL
// and silently bypass the watermark this query filters on, which is exactly the trap the
// first version of this helper fell into.
func insertRuled(t *testing.T, s *store.Store, narrativeID int64, status, feedback string) int64 {
	t.Helper()

	id, err := s.InsertAction(store.ActionRow{
		NarrativeID: narrativeID, Type: "comment", IssueKey: "DEVSBX-1",
		Payload: `{"body":"drafted prose"}`, Status: store.StatusProposed,
	})
	require.NoError(t, err)
	require.NoError(t, s.UpdateActionStatusAndFeedback(id, status, feedback))

	return id
}

// insertDirect writes a row in its final status without a ruling, as the reconciler does
// for its own machine outcomes.
func insertDirect(t *testing.T, s *store.Store, narrativeID int64, status, feedback string) {
	t.Helper()

	_, err := s.InsertAction(store.ActionRow{
		NarrativeID: narrativeID, Type: "comment", IssueKey: "DEVSBX-9",
		Payload: `{"body":"machine outcome"}`, Status: status, Feedback: feedback,
	})
	require.NoError(t, err)
}

// TestCorrectionsSince_ReturnsRejectedAndEdited is the population. Both verbs carry a
// reviewer's reasoning and both are worth learning from — a rejection says "not this",
// an edit says "this, but differently".
func TestCorrectionsSince_ReturnsRejectedAndEdited(t *testing.T) {
	s, nid := correctionsStore(t)
	insertRuled(t, s, nid, store.StatusRejected, "rejected because X")
	insertRuled(t, s, nid, store.StatusEdited, "edited because Y")

	got, err := s.CorrectionsSince(time.Time{})
	require.NoError(t, err)

	require.Len(t, got, 2)
	assert.Equal(t, "drafted prose", got[0].Body,
		"the drafted body must come along: a correction is only legible against what it corrected")
}

// TestCorrectionsSince_ExcludesRowsWithNoFeedback is the one that keeps the corpus
// signal-rich. A reviewer may reject without explaining — reject takes optional text —
// and a rejection with no reasoning teaches nothing a model can generalise.
func TestCorrectionsSince_ExcludesRowsWithNoFeedback(t *testing.T) {
	s, nid := correctionsStore(t)
	insertRuled(t, s, nid, store.StatusRejected, "")
	insertRuled(t, s, nid, store.StatusRejected, "   ")
	insertRuled(t, s, nid, store.StatusRejected, "real reasoning")

	got, err := s.CorrectionsSince(time.Time{})
	require.NoError(t, err)

	require.Len(t, got, 1, "a ruling with no reasoning is not a correction to learn from")
	assert.Equal(t, "real reasoning", got[0].Feedback)
}

// TestCorrectionsSince_ExcludesMachineDecisions is the distinction the schema comment
// already draws: suppressed is a deterministic filter's refusal and declined is the
// MODEL's judgment. Neither is a human teaching anything, and feeding them to
// distillation would produce rules from unjira's own output.
func TestCorrectionsSince_ExcludesMachineDecisions(t *testing.T) {
	s, nid := correctionsStore(t)
	// Machine outcomes are inserted directly, which is how the reconciler writes them —
	// and the point of the test is that they are excluded regardless.
	insertDirect(t, s, nid, store.StatusSuppressed, "no evidence of work in the delta")
	insertDirect(t, s, nid, store.StatusDeclined, "not worth a ticket")
	insertDirect(t, s, nid, store.StatusApplied, "")
	insertRuled(t, s, nid, store.StatusRejected, "a human wrote this")

	got, err := s.CorrectionsSince(time.Time{})
	require.NoError(t, err)

	require.Len(t, got, 1,
		"suppressed and declined are machine outcomes, not reviewer corrections — distilling them "+
			"would turn unjira's own filters into agent norms")
	assert.Equal(t, "a human wrote this", got[0].Feedback)
}

// TestCorrectionsSince_HonorsTheWatermark is what makes a learn-interval possible: a
// second learn-check must not re-distil rules from corrections it already saw.
func TestCorrectionsSince_HonorsTheWatermark(t *testing.T) {
	s, nid := correctionsStore(t)
	insertRuled(t, s, nid, store.StatusRejected, "old correction")

	// Everything so far is before the watermark.
	cutoff := time.Now().UTC().Add(time.Second)

	got, err := s.CorrectionsSince(cutoff)
	require.NoError(t, err)
	assert.Empty(t, got, "corrections older than the watermark must not be re-distilled")

	got, err = s.CorrectionsSince(time.Time{})
	require.NoError(t, err)
	assert.Len(t, got, 1, "and a zero watermark means everything, for a first run")
}

// TestCorrectionsSince_CarriesTheActionType because a norm about comments is not a norm
// about transitions, and a candidate that conflated them would be applied to both.
func TestCorrectionsSince_CarriesTheActionType(t *testing.T) {
	s, nid := correctionsStore(t)
	id, err := s.InsertAction(store.ActionRow{
		NarrativeID: nid, Type: "transition", IssueKey: "DEVSBX-2",
		Payload: `{"target_status":"Done"}`, Status: store.StatusProposed,
	})
	require.NoError(t, err)
	require.NoError(t, s.UpdateActionStatusAndFeedback(id, store.StatusRejected,
		"do not close on a merge alone"))

	got, err := s.CorrectionsSince(time.Time{})
	require.NoError(t, err)

	require.Len(t, got, 1)
	assert.Equal(t, id, got[0].ActionID)
	assert.Equal(t, "transition", got[0].ActionType)
	assert.Equal(t, "DEVSBX-2", got[0].IssueKey)
}
