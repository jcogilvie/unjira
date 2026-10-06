package pipeline_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/pipeline"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// TestUnroutedStoredKeys_ReportsKeysNoTrackerOwns is the stored-key-no-longer-routes
// report: routing is not stored, so a config change can strand a stored key. It is
// reported with its reason, and never dropped from the store.
func TestUnroutedStoredKeys_ReportsKeysNoTrackerOwns(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "unjira.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	id, err := s.InsertNarrative(time.Now(), time.Now(), "work", "s")
	require.NoError(t, err)
	require.NoError(t, s.WithTx(func(tx *store.Tx) error {
		return tx.AddNarrativeIssues(id, []store.NarrativeIssue{
			{IssueKey: "PAAS-1", Role: "primary", Provenance: "branch", Confidence: 0.9},
			{IssueKey: "SUMO-2", Role: "mentioned", Provenance: "prose_later", Confidence: 0.1},
			{IssueKey: "crossplane/crossplane#7", Role: "mentioned", Provenance: "prose_later", Confidence: 0.1},
		})
	}))

	never := func() (tasktracker.TaskReader, error) { panic("the report must not open a backend") }
	r := tasktracker.NewResolver(tasktracker.Route{Tracker: "work", Scopes: []string{"PAAS"}, OpenReader: never}).
		WithReadFallback("work")

	got, err := pipeline.UnroutedStoredKeys(s, r)

	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "SUMO-2", got[0].Key)
	assert.Contains(t, got[0].Reason, `read through tracker "work"`, "a fallback read still works, but says so")
	assert.Equal(t, "crossplane/crossplane#7", got[1].Key)
	assert.Contains(t, got[1].Reason, "no configured tracker's scopes cover")

	keys, err := s.StoredIssueKeys()
	require.NoError(t, err)
	assert.Len(t, keys, 3, "reporting a key never deletes it")
}
