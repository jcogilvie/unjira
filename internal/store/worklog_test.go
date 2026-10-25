package store_test

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/store/storetest"
)

// TestInsertAction_ZeroNarrativeIsNull: a worklog action is ticket-scoped, so its
// NarrativeID of 0 is stored as NULL — no FK error, and no narrative's actions see it.
func TestInsertAction_ZeroNarrativeIsNull(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := store.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	base := time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC)
	nid, err := s.InsertNarrative(base, base.Add(time.Hour), "t", "s")
	require.NoError(t, err)

	id, err := s.InsertAction(store.ActionRow{
		NarrativeID: 0, Type: "worklog", IssueKey: "PROJ-1", Payload: "{}", Status: "proposed",
	})
	require.NoError(t, err)

	got, err := s.GetAction(id)
	require.NoError(t, err)
	assert.Equal(t, int64(0), got.NarrativeID)

	proposed, err := s.ActionsByStatus("proposed")
	require.NoError(t, err)
	require.Len(t, proposed, 1)
	assert.Equal(t, id, proposed[0].ID)

	forNarrative, err := s.ActionsForNarrative(nid)
	require.NoError(t, err)
	assert.Empty(t, forNarrative)

	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	var isNull bool
	require.NoError(t, db.QueryRow(`SELECT narrative_id IS NULL FROM actions WHERE id = ?`, id).Scan(&isNull))
	assert.True(t, isNull)
}

// seedMessages ledgers one bare message per id.
func seedMessages(t *testing.T, s *store.Store, ids ...string) {
	t.Helper()
	rows := make([]store.MessageUsage, 0, len(ids))
	for _, id := range ids {
		rows = append(rows, storetest.MessageUsage(id).Build())
	}
	require.NoError(t, s.UpsertMessageUsage(rows))
}

func TestMarkWorklogged(t *testing.T) {
	s := openStore(t)
	seedMessages(t, s, "m1", "m2")

	require.NoError(t, s.MarkWorklogged("PROJ-1", "10001", []string{"m1", "m2"}))
	want := map[string]string{"m1": "PROJ-1/10001", "m2": "PROJ-1/10001"}
	got, err := s.LoggedAmong([]string{"m1", "m2"})
	require.NoError(t, err)
	assert.Equal(t, want, got)

	// The same pair again is a no-op.
	require.NoError(t, s.MarkWorklogged("PROJ-1", "10001", []string{"m1", "m2"}))

	// Another worklog's marker is never overwritten, and the batch is all-or-nothing.
	err = s.MarkWorklogged("PROJ-1", "10002", []string{"m2", "m1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "m2")
	assert.Contains(t, err.Error(), "10001")
	got, err = s.LoggedAmong([]string{"m1", "m2"})
	require.NoError(t, err)
	assert.Equal(t, want, got)

	seedMessages(t, s, "m3")
	err = s.MarkWorklogged("PROJ-1", "10003", []string{"m3", "m-unknown"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "m-unknown")
	got, err = s.LoggedAmong([]string{"m3"})
	require.NoError(t, err)
	assert.Empty(t, got, "the failed batch rolled back m3's marker")
}

func TestClearWorklogMarkers(t *testing.T) {
	s := openStore(t)
	seedMessages(t, s, "m1", "m2", "m3", "m4")
	require.NoError(t, s.MarkWorklogged("PROJ-1", "10001", []string{"m1", "m2", "m3"}))
	require.NoError(t, s.MarkWorklogged("PROJ-1", "10009", []string{"m4"}))

	n, err := s.ClearWorklogMarkers("PROJ-1", "10001", []string{"m2"})
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	got, err := s.LoggedAmong([]string{"m1", "m2", "m3", "m4"})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"m2": "PROJ-1/10001", "m4": "PROJ-1/10009"}, got)

	n, err = s.ClearWorklogMarkers("PROJ-1", "10001", nil)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	got, err = s.LoggedAmong([]string{"m1", "m2", "m3", "m4"})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"m4": "PROJ-1/10009"}, got)
}

func TestLoggedAmong(t *testing.T) {
	s := openStore(t)
	seedMessages(t, s, "m1", "m2")
	require.NoError(t, s.MarkWorklogged("PROJ-1", "10001", []string{"m1"}))

	got, err := s.LoggedAmong([]string{"m1", "m2", "m-unknown"})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"m1": "PROJ-1/10001"}, got)

	got, err = s.LoggedAmong(nil)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestActionsForIssue(t *testing.T) {
	s := openStore(t)
	insert := func(key string) int64 {
		id, err := s.InsertAction(store.ActionRow{Type: "worklog", IssueKey: key, Payload: "{}", Status: "proposed"})
		require.NoError(t, err)
		return id
	}
	a1 := insert("PROJ-1")
	insert("PROJ-2")
	a2 := insert("PROJ-1")

	got, err := s.ActionsForIssue("PROJ-1")
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, []int64{a1, a2}, []int64{got[0].ID, got[1].ID})
}

func TestSinkEnabledAt(t *testing.T) {
	s := openStore(t)
	first := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

	got, err := s.SinkEnabledAt("corp", "worklog", first)
	require.NoError(t, err)
	assert.True(t, got.Equal(first))

	got, err = s.SinkEnabledAt("corp", "worklog", first.Add(24*time.Hour))
	require.NoError(t, err)
	assert.True(t, got.Equal(first), "the floor is the first pass, never moved")

	later := first.Add(48 * time.Hour)
	got, err = s.SinkEnabledAt("other", "worklog", later)
	require.NoError(t, err)
	assert.True(t, got.Equal(later))
}
