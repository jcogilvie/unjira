package store_test

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver

	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/store"
)

func makeEvent(externalID string) events.Event {
	e := events.NewEvent(
		"claude_code",
		externalID,
		time.Date(2026, 7, 11, 14, 30, 0, 0, time.UTC),
		"Claude Code session in unjira: 3 user messages.",
	)
	e.Artifacts["ticket_keys"] = []any{"PROJ-1"}

	return e
}

func openStore(t *testing.T) *store.Store {
	t.Helper()

	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	return s
}

func TestInsertEvent_DedupesOnSourceAndExternalID(t *testing.T) {
	s := openStore(t)

	inserted, err := s.InsertEvent(makeEvent("s1:100"))
	require.NoError(t, err)
	assert.True(t, inserted)

	inserted, err = s.InsertEvent(makeEvent("s1:100"))
	require.NoError(t, err)
	assert.False(t, inserted)

	inserted, err = s.InsertEvent(makeEvent("s1:200"))
	require.NoError(t, err)
	assert.True(t, inserted)
}

func TestEventsOn_FiltersByDay(t *testing.T) {
	s := openStore(t)

	_, err := s.InsertEvent(makeEvent("s1:100"))
	require.NoError(t, err)

	rows, err := s.EventsOn(time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	assert.Len(t, rows, 1)

	rows, err = s.EventsOn(time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	assert.Empty(t, rows)
}

func TestCursor_Roundtrip(t *testing.T) {
	s := openStore(t)

	pos, err := s.GetCursor("claude_code", "/a/b.jsonl")
	require.NoError(t, err)
	assert.Empty(t, pos)

	require.NoError(t, s.SetCursor("claude_code", "/a/b.jsonl", "123:456"))

	pos, err = s.GetCursor("claude_code", "/a/b.jsonl")
	require.NoError(t, err)
	assert.Equal(t, "123:456", pos)

	require.NoError(t, s.SetCursor("claude_code", "/a/b.jsonl", "789:456"))

	pos, err = s.GetCursor("claude_code", "/a/b.jsonl")
	require.NoError(t, err)
	assert.Equal(t, "789:456", pos)
}

func TestEventCountsBySource_GroupsAndCounts(t *testing.T) {
	s := openStore(t)

	_, err := s.InsertEvent(makeEvent("s1:100"))
	require.NoError(t, err)
	_, err = s.InsertEvent(makeEvent("s1:200"))
	require.NoError(t, err)

	counts, err := s.EventCountsBySource()
	require.NoError(t, err)
	require.Len(t, counts, 1)
	assert.Equal(t, "claude_code", counts[0].Source)
	assert.Equal(t, 2, counts[0].Count)
}

func TestCursorCounts_GroupsAndCounts(t *testing.T) {
	s := openStore(t)

	require.NoError(t, s.SetCursor("claude_code", "/a.jsonl", "1"))
	require.NoError(t, s.SetCursor("claude_code", "/b.jsonl", "2"))

	counts, err := s.CursorCounts()
	require.NoError(t, err)
	require.Len(t, counts, 1)
	assert.Equal(t, "claude_code", counts[0].Collector)
	assert.Equal(t, 2, counts[0].Count)
}

// -- local issues --------------------------------------------------------

func TestGetLocalIssue_MissingReturnsErrLocalIssueNotFound(t *testing.T) {
	s := openStore(t)

	_, err := s.GetLocalIssue("PROJ-1")

	require.Error(t, err)
	assert.ErrorIs(t, err, store.ErrLocalIssueNotFound)
}

func TestInsertLocalIssue_AssignsSequentialKeyPerProject(t *testing.T) {
	s := openStore(t)

	key1, err := s.InsertLocalIssue("PROJ", "First issue", "Task", "", nil)
	require.NoError(t, err)
	assert.Equal(t, "PROJ-1", key1)

	key2, err := s.InsertLocalIssue("PROJ", "Second issue", "Task", "", nil)
	require.NoError(t, err)
	assert.Equal(t, "PROJ-2", key2)

	otherKey, err := s.InsertLocalIssue("OTHER", "Different project", "Task", "", nil)
	require.NoError(t, err)
	assert.Equal(t, "OTHER-1", otherKey)
}

func TestSetLocalIssueStatus_UpdatesCategory(t *testing.T) {
	s := openStore(t)
	key, err := s.InsertLocalIssue("PROJ", "Do the thing", "Task", "", nil)
	require.NoError(t, err)

	require.NoError(t, s.SetLocalIssueStatus(key, "in_progress"))

	issue, err := s.GetLocalIssue(key)
	require.NoError(t, err)
	assert.Equal(t, "in_progress", issue.StatusCategory)
}

func TestSetLocalIssueStatus_MissingReturnsErrLocalIssueNotFound(t *testing.T) {
	s := openStore(t)

	err := s.SetLocalIssueStatus("PROJ-1", "done")

	require.Error(t, err)
	assert.ErrorIs(t, err, store.ErrLocalIssueNotFound)
}

func TestInsertLocalIssueComment_LocalIssueComments_RoundTrips(t *testing.T) {
	s := openStore(t)
	key, err := s.InsertLocalIssue("PROJ", "Do the thing", "Task", "", nil)
	require.NoError(t, err)

	require.NoError(t, s.InsertLocalIssueComment(key, "first comment"))
	require.NoError(t, s.InsertLocalIssueComment(key, "second comment"))

	comments, err := s.LocalIssueComments(key)
	require.NoError(t, err)
	assert.Equal(t, []string{"first comment", "second comment"}, comments)
}

func TestInsertLocalIssueComment_MissingReturnsErrLocalIssueNotFound(t *testing.T) {
	s := openStore(t)

	err := s.InsertLocalIssueComment("PROJ-1", "a comment")

	require.Error(t, err)
	assert.ErrorIs(t, err, store.ErrLocalIssueNotFound)
}

func TestSearchLocalIssues_NoQueryReturnsAll(t *testing.T) {
	s := openStore(t)
	_, err := s.InsertLocalIssue("PROJ", "Fix the bug", "Task", "", nil)
	require.NoError(t, err)
	_, err = s.InsertLocalIssue("PROJ", "Write the docs", "Task", "", nil)
	require.NoError(t, err)

	issues, err := s.SearchLocalIssues("", 10)
	require.NoError(t, err)
	assert.Len(t, issues, 2)
}

func TestSearchLocalIssues_CaseInsensitiveSubstringMatchesSubset(t *testing.T) {
	s := openStore(t)
	_, err := s.InsertLocalIssue("PROJ", "Fix the bug", "Task", "", nil)
	require.NoError(t, err)
	_, err = s.InsertLocalIssue("PROJ", "Write the docs", "Task", "", nil)
	require.NoError(t, err)

	issues, err := s.SearchLocalIssues("BUG", 10)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, "Fix the bug", issues[0].Summary)
}

func TestSearchLocalIssues_RespectsLimit(t *testing.T) {
	s := openStore(t)
	_, err := s.InsertLocalIssue("PROJ", "First", "Task", "", nil)
	require.NoError(t, err)
	_, err = s.InsertLocalIssue("PROJ", "Second", "Task", "", nil)
	require.NoError(t, err)

	issues, err := s.SearchLocalIssues("", 1)
	require.NoError(t, err)
	assert.Len(t, issues, 1)
}

func TestInsertLocalIssue_GetLocalIssue_RoundTrips(t *testing.T) {
	s := openStore(t)

	key, err := s.InsertLocalIssue("PROJ", "Do the thing", "Task", "a description", []string{"bug", "urgent"})
	require.NoError(t, err)

	issue, err := s.GetLocalIssue(key)
	require.NoError(t, err)
	assert.Equal(t, key, issue.Key)
	assert.Equal(t, "PROJ", issue.Project)
	assert.Equal(t, "Do the thing", issue.Summary)
	assert.Equal(t, "a description", issue.Description)
	assert.Equal(t, "Task", issue.IssueType)
	assert.Equal(t, "todo", issue.StatusCategory)
	assert.Equal(t, []string{"bug", "urgent"}, issue.Labels)
}

// -- narratives.compaction_boundary migration ----------------------------

func TestOpen_CompactionBoundaryColumnPresentAndReopenable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s1, err := store.Open(path)
	require.NoError(t, err)
	require.NoError(t, s1.Close())

	// Reopen must not error (idempotent ALTER guard).
	s2, err := store.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s2.Close() })

	// Column exists (queryable without error).
	cols, err := s2.NarrativeColumns()
	require.NoError(t, err)
	assert.Contains(t, cols, "compaction_boundary")
}

func TestOpen_MigratesPreExistingNarrativesTableWithoutColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	// Simulate a pre-column database: create a narratives table WITHOUT
	// compaction_boundary, using a raw connection, then close it.
	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = raw.Exec(`CREATE TABLE narratives (
		id INTEGER PRIMARY KEY, window_start TEXT NOT NULL, window_end TEXT NOT NULL,
		title TEXT NOT NULL, summary TEXT NOT NULL, issue_key TEXT, confidence REAL,
		status TEXT NOT NULL DEFAULT 'open',
		created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
	)`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	// store.Open must migrate it (add the column) without error.
	s, err := store.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	cols, err := s.NarrativeColumns()
	require.NoError(t, err)
	assert.Contains(t, cols, "compaction_boundary")
}

// -- narrative store accessors -------------------------------------------

func seedEvent(t *testing.T, s *store.Store, externalID, summary string, at time.Time) {
	t.Helper()
	e := events.NewEvent("claude_code", externalID, at, summary)
	_, err := s.InsertEvent(e)
	require.NoError(t, err)
}

func TestNarrative_InsertGetRoundTrip(t *testing.T) {
	s := openStore(t)
	ws := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	we := ws.Add(time.Hour)

	id, err := s.InsertNarrative(ws, we, "Title", "Summary")
	require.NoError(t, err)

	row, err := s.GetNarrative(id)
	require.NoError(t, err)
	assert.Equal(t, id, row.ID)
	assert.Equal(t, "Title", row.Title)
	assert.Equal(t, "Summary", row.Summary)
	assert.Equal(t, "open", row.Status)
	assert.True(t, ws.Equal(row.WindowStart))
	assert.True(t, we.Equal(row.WindowEnd))
	assert.Empty(t, row.IssueKey)
	assert.Nil(t, row.CompactionBoundary)
}

func TestGetNarrative_MissingReturnsErrNarrativeNotFound(t *testing.T) {
	s := openStore(t)
	_, err := s.GetNarrative(999)
	require.ErrorIs(t, err, store.ErrNarrativeNotFound)
}

func TestExtendNarrative_UpdatesWindowEndAndSummary(t *testing.T) {
	s := openStore(t)
	ws := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	id, err := s.InsertNarrative(ws, ws.Add(time.Hour), "T", "old")
	require.NoError(t, err)

	newEnd := ws.Add(3 * time.Hour)
	require.NoError(t, s.ExtendNarrative(id, newEnd, "new summary"))

	row, err := s.GetNarrative(id)
	require.NoError(t, err)
	assert.True(t, newEnd.Equal(row.WindowEnd))
	assert.Equal(t, "new summary", row.Summary)
}

func TestSetCompactionBoundary_PersistsBoundaryAndRecap(t *testing.T) {
	s := openStore(t)
	ws := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	id, err := s.InsertNarrative(ws, ws.Add(time.Hour), "T", "s")
	require.NoError(t, err)

	boundary := ws.Add(30 * time.Minute)
	require.NoError(t, s.SetCompactionBoundary(id, boundary, "recap: earlier work"))

	row, err := s.GetNarrative(id)
	require.NoError(t, err)
	require.NotNil(t, row.CompactionBoundary)
	assert.True(t, boundary.Equal(*row.CompactionBoundary))
	assert.Equal(t, "recap: earlier work", row.Summary)
}

func TestAddNarrativeEvents_IsIdempotent(t *testing.T) {
	s := openStore(t)
	ws := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	seedEvent(t, s, "e1", "first", ws)
	id, err := s.InsertNarrative(ws, ws.Add(time.Hour), "T", "s")
	require.NoError(t, err)
	eid, err := s.EventIDByExternalID("claude_code", "e1")
	require.NoError(t, err)

	require.NoError(t, s.AddNarrativeEvents(id, []int64{eid}))
	require.NoError(t, s.AddNarrativeEvents(id, []int64{eid})) // re-link: no error

	evs, err := s.NarrativeEventsForContext(id)
	require.NoError(t, err)
	require.Len(t, evs, 1)
	assert.Equal(t, "e1", evs[0].ExternalID)
}

func TestEventIDByExternalID_MissingReturnsErrEventNotFound(t *testing.T) {
	s := openStore(t)
	_, err := s.EventIDByExternalID("claude_code", "nope")
	require.ErrorIs(t, err, store.ErrEventNotFound)
}

func TestNarrativeEventsForContext_ExcludesAtOrBeforeBoundary(t *testing.T) {
	s := openStore(t)
	ws := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	seedEvent(t, s, "old", "old event", ws)
	seedEvent(t, s, "new", "new event", ws.Add(time.Hour))
	id, err := s.InsertNarrative(ws, ws.Add(2*time.Hour), "T", "s")
	require.NoError(t, err)
	oldID, err := s.EventIDByExternalID("claude_code", "old")
	require.NoError(t, err)
	newID, err := s.EventIDByExternalID("claude_code", "new")
	require.NoError(t, err)
	require.NoError(t, s.AddNarrativeEvents(id, []int64{oldID, newID}))

	// Boundary at the old event's time: strictly-after filter excludes it,
	// keeps the newer one.
	require.NoError(t, s.SetCompactionBoundary(id, ws, "recap"))

	evs, err := s.NarrativeEventsForContext(id)
	require.NoError(t, err)
	require.Len(t, evs, 1)
	assert.Equal(t, "new", evs[0].ExternalID)
}
