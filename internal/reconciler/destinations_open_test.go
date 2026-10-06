package reconciler

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/store"
)

// TestProposeCreates_AnOpenCreateBlocksOnlyItsOwnScope: a create already awaiting review
// in one destination must not be proposed again there, and does not block another.
func TestProposeCreates_AnOpenCreateBlocksOnlyItsOwnScope(t *testing.T) {
	s := reconcileStore(t)
	nid := seedUntracked(t, s, eventIn("e1", false, "github.com/partner/sdk"))
	_, err := s.InsertAction(store.ActionRow{
		NarrativeID: nid, Type: string(ActionCreate), Status: StatusProposed,
		Payload: `{"summary":"S","description":"D","scope":"DEVSBX"}`,
	})
	require.NoError(t, err)

	client := &fakeLLM{responses: []string{worthTracking}}
	got := proposeWithPolicy(t, s, client)

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

	got := proposeWithPolicy(t, s, &fakeLLM{responses: []string{worthTracking}})

	assert.Empty(t, got[0].Proposed)
	require.Len(t, got[0].Suppressed, 1)
	assert.Contains(t, got[0].Suppressed[0], "already proposes")
}
