package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/store"
)

func TestSetNarrativeStatus_RecordsTheStatus(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC)

	id, err := s.InsertNarrative(base, base.Add(time.Hour), "t", "s")
	require.NoError(t, err)

	require.NoError(t, s.SetNarrativeStatus(id, store.StatusSplit))

	got, err := s.GetNarrative(id)
	require.NoError(t, err)
	assert.Equal(t, store.StatusSplit, got.Status)
}

// TestSetNarrativeStatus_MissingNarrativeErrors: a no-op UPDATE would hide a
// caller naming a narrative that does not exist.
func TestSetNarrativeStatus_MissingNarrativeErrors(t *testing.T) {
	s := openStore(t)

	err := s.SetNarrativeStatus(999999, store.StatusSplit)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no such narrative")
}

// TestNarrativesOverlapping_ExcludesSplitNarratives is the reason
// SetNarrativeStatus exists. An emptied narrative that stays selectable is
// rendered into every subsequent Cluster prompt as an existing narrative holding
// zero events — the model asked to reason about a story whose substance moved.
func TestNarrativesOverlapping_ExcludesSplitNarratives(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC)

	live, err := s.InsertNarrative(base, base.Add(time.Hour), "still live", "s")
	require.NoError(t, err)
	gone, err := s.InsertNarrative(base, base.Add(time.Hour), "was split apart", "s")
	require.NoError(t, err)

	before, err := s.NarrativesOverlapping(base, base.Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, before, 2, "precondition: both are selected while both are open")

	require.NoError(t, s.SetNarrativeStatus(gone, store.StatusSplit))

	after, err := s.NarrativesOverlapping(base, base.Add(time.Hour))

	require.NoError(t, err)
	require.Len(t, after, 1, "a split narrative must not be fed to Cluster as context")
	assert.Equal(t, live, after[0].ID)
}

// TestNarrativesOverlapping_KeepsUnknownStatuses: only 'split' is excluded, not
// "anything not open". A status this query has never seen is more likely a new
// lifecycle state than a reason to hide work from clustering.
func TestNarrativesOverlapping_KeepsUnknownStatuses(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC)

	id, err := s.InsertNarrative(base, base.Add(time.Hour), "some future state", "s")
	require.NoError(t, err)
	require.NoError(t, s.SetNarrativeStatus(id, "archived-by-some-future-slice"))

	got, err := s.NarrativesOverlapping(base, base.Add(time.Hour))

	require.NoError(t, err)
	assert.Len(t, got, 1,
		"an unrecognized status must not silently drop a narrative from clustering")
}

// TestSetNarrativeStatus_PreservesTheRowAndItsActions: the row is kept rather than
// deleted precisely because actions still reference it — including an APPLIED
// action, whose record of "unjira posted this about this work" must survive.
func TestSetNarrativeStatus_PreservesTheRowAndItsActions(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC)

	id, err := s.InsertNarrative(base, base.Add(time.Hour), "had a comment posted", "s")
	require.NoError(t, err)

	aid, err := s.InsertAction(store.ActionRow{
		NarrativeID: id, Type: "comment", IssueKey: "PROJ-1",
		Payload: `{"body":"posted"}`, Status: "proposed",
	})
	require.NoError(t, err)
	require.NoError(t, s.UpdateActionStatus(aid, "applied"))

	require.NoError(t, s.SetNarrativeStatus(id, store.StatusSplit))

	got, err := s.GetNarrative(id)
	require.NoError(t, err)
	assert.Equal(t, store.StatusSplit, got.Status, "the row survives")

	actions, err := s.ActionsForNarrative(id)
	require.NoError(t, err)
	require.Len(t, actions, 1)
	assert.Equal(t, "applied", actions[0].Status,
		"the record of what unjira actually posted must outlive the restructure")
}
