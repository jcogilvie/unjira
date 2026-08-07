package store_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
