package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/store"
)

// TestStoredIssueKeys_ListsEveryKeyOnceFromLinksAndActions: the keys a config change
// could leave unrouted live in two tables, and a startup report must see both.
func TestStoredIssueKeys_ListsEveryKeyOnceFromLinksAndActions(t *testing.T) {
	s := openStore(t)
	id := insertNarrativeForTest(t, s, "work")
	require.NoError(t, s.WithTx(func(tx *store.Tx) error {
		return tx.AddNarrativeIssues(id, []store.NarrativeIssue{
			{IssueKey: "PAAS-1", Role: "primary", Provenance: "branch", Confidence: 0.9},
			{IssueKey: "SUMO-2", Role: "mentioned", Provenance: "prose_later", Confidence: 0.1},
		})
	}))
	_, err := s.InsertAction(store.ActionRow{NarrativeID: id, Type: "comment", IssueKey: "PAAS-1", Payload: `{}`})
	require.NoError(t, err)
	_, err = s.InsertAction(store.ActionRow{NarrativeID: id, Type: "comment", IssueKey: "OPS-3", Payload: `{}`})
	require.NoError(t, err)
	_, err = s.InsertAction(store.ActionRow{NarrativeID: id, Type: "create", Payload: `{}`})
	require.NoError(t, err)

	got, err := s.StoredIssueKeys()

	require.NoError(t, err)
	assert.Equal(t, []string{"OPS-3", "PAAS-1", "SUMO-2"}, got, "sorted, each once, and a create's empty key is not a key")
}
