package gate_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/gate"
	"github.com/jcogilvie/unjira/internal/store"
)

// scopedWriter mints <scope>-<n> keys, so two creates for one narrative get two keys.
type scopedWriter struct {
	creates []string
}

func (w *scopedWriter) AddComment(string, string) error { return nil }
func (w *scopedWriter) SetStatus(string, string) error  { return nil }
func (w *scopedWriter) CreateIssue(scope, _, _, _ string, _ []string) (string, error) {
	w.creates = append(w.creates, scope)

	return fmt.Sprintf("%s-%d", scope, len(w.creates)), nil
}

func twoWritableTrackers() []config.Tracker {
	return []config.Tracker{
		{Name: "work", Scopes: []string{"DEVSBX"}, WritableScopes: []string{"DEVSBX"}, DefaultScope: "DEVSBX"},
		{Name: "ops", Scopes: []string{"OPS"}, WritableScopes: []string{"OPS"}, DefaultScope: "OPS"},
	}
}

// TestApplier_Create_LandsInTheScopeItsDestinationChose: a create carries its
// destination's scope, and the write-scope check is made against that scope.
func TestApplier_Create_LandsInTheScopeItsDestinationChose(t *testing.T) {
	s := applierStore(t)
	w := &scopedWriter{}
	action := insertAction(t, s, store.ActionRow{
		Type: "create", Payload: `{"summary":"S","description":"D","scope":"OPS"}`,
	})

	require.NoError(t, gate.NewApplier(s, w, "DEVSBX", twoWritableTrackers()).Apply(action))

	assert.Equal(t, []string{"OPS"}, w.creates)
}

func TestApplier_Create_RefusesAnUnwritableDestinationScope(t *testing.T) {
	s := applierStore(t)
	w := &scopedWriter{}
	action := insertAction(t, s, store.ActionRow{
		Type: "create", Payload: `{"summary":"S","description":"D","scope":"PAAS"}`,
	})

	err := gate.NewApplier(s, w, "DEVSBX", twoWritableTrackers()).Apply(action)

	require.ErrorContains(t, err, "PAAS")
	assert.Empty(t, w.creates)
}

// TestApplier_Create_ASecondDestinationIsNotADuplicateOfTheFirst: one narrative ticketed
// in two trackers. The first create's link is in another tracker, so it does not make
// the second a duplicate; the second is linked as same_work, since a narrative has one
// primary.
func TestApplier_Create_ASecondDestinationIsNotADuplicateOfTheFirst(t *testing.T) {
	s := applierStore(t)
	w := &scopedWriter{}
	first := insertAction(t, s, store.ActionRow{
		Type: "create", Payload: `{"summary":"S","description":"D","scope":"DEVSBX"}`,
	})
	second := first
	second.Payload = `{"summary":"S","description":"D","scope":"OPS"}`
	id, err := s.InsertAction(store.ActionRow{
		NarrativeID: first.NarrativeID, Type: "create", Payload: second.Payload, Status: "proposed",
	})
	require.NoError(t, err)
	second.ID = id

	applier := gate.NewApplier(s, w, "DEVSBX", twoWritableTrackers())
	require.NoError(t, applier.Apply(first))
	require.NoError(t, applier.Apply(second))

	links, err := s.NarrativeIssues(first.NarrativeID)
	require.NoError(t, err)
	roles := map[string]store.Role{}
	for _, l := range links {
		roles[l.IssueKey] = l.Role
	}
	assert.Equal(t, map[string]store.Role{"DEVSBX-1": "primary", "OPS-2": "same_work"}, roles)
}

// TestApplier_Create_APrimaryInTheSameTrackerStillBlocks: F13's backstop holds per
// destination — work matched to an issue in the destination tracker is tracked there.
func TestApplier_Create_APrimaryInTheSameTrackerStillBlocks(t *testing.T) {
	s := applierStore(t)
	w := &scopedWriter{}
	action := insertAction(t, s, store.ActionRow{
		Type: "create", Payload: `{"summary":"S","description":"D","scope":"OPS"}`,
	})
	require.NoError(t, s.WithTx(func(tx *store.Tx) error {
		return tx.AddNarrativeIssues(action.NarrativeID, []store.NarrativeIssue{
			{IssueKey: "OPS-7", Role: "primary", Provenance: "branch", Confidence: 1},
		})
	}))

	err := gate.NewApplier(s, w, "DEVSBX", twoWritableTrackers()).Apply(action)

	require.ErrorContains(t, err, "OPS-7")
	assert.Empty(t, w.creates)
}
