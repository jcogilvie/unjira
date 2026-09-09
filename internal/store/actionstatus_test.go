package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/store"
)

// insertActionNarrative gives an ActionRow a valid narrative_id to satisfy
// the actions.narrative_id foreign key.
func insertActionNarrative(t *testing.T, s *store.Store) int64 {
	t.Helper()

	base := time.Date(2026, 9, 3, 9, 0, 0, 0, time.UTC)
	id, err := s.InsertNarrative(base, base.Add(time.Hour), "t", "s")
	require.NoError(t, err)

	return id
}

// TestActionStatusConstants_MatchTheSchemaComment pins the seven string
// values actions.status may hold (per the schema comment in store.go) against
// the named constants, so a typo in either place shows up here rather than
// only at the point some cross-package caller's string comparison silently
// stops matching.
func TestActionStatusConstants_MatchTheSchemaComment(t *testing.T) {
	assert.Equal(t, "proposed", store.StatusProposed)
	assert.Equal(t, "approved", store.StatusApproved)
	assert.Equal(t, "edited", store.StatusEdited)
	assert.Equal(t, "rejected", store.StatusRejected)
	assert.Equal(t, "applied", store.StatusApplied)
	assert.Equal(t, "failed", store.StatusFailed)
	assert.Equal(t, "declined", store.StatusDeclined)
}

// TestUpdateActionStatus_StampsDecidedAtForEveryHumanRulingConstant is the
// existing decided_at/executed_at stamping behaviour
// (TestUpdateActionStatusSetsDecidedAndExecutedTimestamps already covers this
// with bare literals), re-run through the named constants so the switch in
// updateActionStatusImpl is proven to still recognize them after the literals
// there are replaced.
func TestUpdateActionStatus_StampsDecidedAtForEveryHumanRulingConstant(t *testing.T) {
	for _, status := range []string{store.StatusApproved, store.StatusEdited, store.StatusRejected} {
		t.Run(status, func(t *testing.T) {
			s := openStore(t)
			nid := insertActionNarrative(t, s)
			id, err := s.InsertAction(store.ActionRow{
				NarrativeID: nid, Type: "comment", IssueKey: "PROJ-1", Payload: `{}`, Status: store.StatusProposed,
			})
			require.NoError(t, err)

			require.NoError(t, s.UpdateActionStatus(id, status))

			got, err := s.GetAction(id)
			require.NoError(t, err)
			assert.Equal(t, status, got.Status)
			require.NotNil(t, got.DecidedAt)
			assert.Nil(t, got.ExecutedAt, "a decision alone does not reach the tracker")
		})
	}
}

// TestUpdateActionStatus_StampsExecutedAtForEveryTerminalWriteConstant does
// the same for the two statuses a real tracker write can produce.
func TestUpdateActionStatus_StampsExecutedAtForEveryTerminalWriteConstant(t *testing.T) {
	for _, status := range []string{store.StatusApplied, store.StatusFailed} {
		t.Run(status, func(t *testing.T) {
			s := openStore(t)
			nid := insertActionNarrative(t, s)
			id, err := s.InsertAction(store.ActionRow{
				NarrativeID: nid, Type: "comment", IssueKey: "PROJ-1", Payload: `{}`, Status: store.StatusProposed,
			})
			require.NoError(t, err)

			require.NoError(t, s.UpdateActionStatus(id, status))

			got, err := s.GetAction(id)
			require.NoError(t, err)
			assert.Equal(t, status, got.Status)
			require.NotNil(t, got.DecidedAt)
			require.NotNil(t, got.ExecutedAt)
		})
	}
}

// TestStatusDeclined_DoesNotStampDecidedOrExecutedAt: a decline is the
// model's own judgment, never a human ruling or a tracker write, so neither
// timestamp applies — pinning this protects the switch in
// updateActionStatusImpl from silently starting to stamp one when
// StatusDeclined is added to a case it does not belong in.
func TestStatusDeclined_DoesNotStampDecidedOrExecutedAt(t *testing.T) {
	s := openStore(t)
	nid := insertActionNarrative(t, s)
	id, err := s.InsertAction(store.ActionRow{
		NarrativeID: nid, Type: "create", Payload: `{}`, Status: store.StatusDeclined,
	})
	require.NoError(t, err)

	got, err := s.GetAction(id)
	require.NoError(t, err)
	assert.Nil(t, got.DecidedAt)
	assert.Nil(t, got.ExecutedAt)
}
