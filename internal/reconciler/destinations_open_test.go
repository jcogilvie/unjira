package reconciler

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/store"
)

// TestProposeCreates_AnOpenCreateBlocksOnlyItsOwnScope: a create already awaiting review
// in one destination must not be proposed again there, and does not block another once
// the pass looks again. It looks again when new member work arrives: with nothing new,
// the create path's decision for the narrative stands (store.NarrativesAwaitingCreate),
// so a model that chose one of two destinations is not overruled a pass later.
func TestProposeCreates_AnOpenCreateBlocksOnlyItsOwnScope(t *testing.T) {
	s := reconcileStore(t)
	nid := seedUntracked(t, s, eventIn("e1", false, "github.com/partner/sdk"))
	_, err := s.InsertAction(store.ActionRow{
		NarrativeID: nid, Type: string(ActionCreate), Status: StatusProposed,
		Payload: `{"summary":"S","description":"D","scope":"DEVSBX"}`,
	})
	require.NoError(t, err)

	client := &fakeLLM{responses: []string{worthTracking}}
	assert.Empty(t, proposeWithPolicy(t, s, client), "nothing new since the create: its decision stands")
	assert.Empty(t, client.prompts)

	linkLaterMember(t, s, nid, "e2")

	got := proposeWithPolicy(t, s, client)
	require.Len(t, got, 1)
	require.Len(t, got[0].Proposed, 1, "ops is still open; one destination means no choice to ask for")
	assert.Equal(t, "OPS", got[0].Proposed[0].Scope)
}

// TestProposeCreates_ACreateWithNoScopeBlocksEveryDestination: a create recorded before
// destinations existed names no scope, so which one it targets is unknown.
func TestProposeCreates_ACreateWithNoScopeBlocksEveryDestination(t *testing.T) {
	s := reconcileStore(t)
	nid := seedUntracked(t, s, eventIn("e1", false, "github.com/partner/sdk"))
	_, err := s.InsertAction(store.ActionRow{
		NarrativeID: nid, Type: string(ActionCreate), Status: StatusProposed,
		Payload: `{"summary":"S","description":"D"}`,
	})
	require.NoError(t, err)

	linkLaterMember(t, s, nid, "e2")

	got := proposeWithPolicy(t, s, &fakeLLM{responses: []string{worthTracking}})

	require.Len(t, got, 1, "new member work is what makes the pass look again")
	assert.Empty(t, got[0].Proposed)
	require.Len(t, got[0].Suppressed, 1)
	assert.Contains(t, got[0].Suppressed[0], "already proposes")
}

// linkLaterMember links a new member event to narrativeID, an hour after the seeded one:
// new work, which is what re-admits a narrative whose create-path decision stands.
func linkLaterMember(t *testing.T, s *store.Store, narrativeID int64, externalID string) {
	t.Helper()

	later := eventIn(externalID, false, "github.com/partner/sdk")
	later.OccurredAt = later.OccurredAt.Add(time.Hour)
	_, err := s.InsertEvent(later)
	require.NoError(t, err)
	eid, err := s.EventIDByExternalID(later.Source, later.ExternalID)
	require.NoError(t, err)
	require.NoError(t, s.LinkMembers(narrativeID, []int64{eid}, 1))
}
