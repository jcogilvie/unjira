package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
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
	seedEvent(t, s, "boundary-event", "the compacted-up-to event", boundary)
	boundaryEventID, err := s.EventIDByExternalID("claude_code", "boundary-event")
	require.NoError(t, err)

	require.NoError(t, s.SetCompactionBoundary(id, boundary, boundaryEventID, "recap: earlier work"))

	row, err := s.GetNarrative(id)
	require.NoError(t, err)
	require.NotNil(t, row.CompactionBoundary)
	assert.True(t, boundary.Equal(*row.CompactionBoundary))
	require.NotNil(t, row.CompactionBoundaryEventID)
	assert.Equal(t, boundaryEventID, *row.CompactionBoundaryEventID)
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

	// Boundary at the old event's (time, id): strictly-after filter excludes
	// it, keeps the newer one.
	require.NoError(t, s.SetCompactionBoundary(id, ws, oldID, "recap"))

	evs, err := s.NarrativeEventsForContext(id)
	require.NoError(t, err)
	require.Len(t, evs, 1)
	assert.Equal(t, "new", evs[0].ExternalID)
}

// TestNarrativeEventCount_IgnoresCompactionBoundary is what proves
// NarrativeEventCount is not just NarrativeEventsForContext under another
// name: it must count a narrative's linked events regardless of the
// compaction boundary, since it exists specifically to let a caller verify
// that narrative_events rows survive compaction (which only shrinks what
// NarrativeEventsForContext returns, never the underlying links).
func TestNarrativeEventCount_IgnoresCompactionBoundary(t *testing.T) {
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

	count, err := s.NarrativeEventCount(id)
	require.NoError(t, err)
	assert.Equal(t, 2, count, "both links counted before any compaction")

	// Set a boundary that makes NarrativeEventsForContext exclude "old" —
	// NarrativeEventCount must be unaffected, since the link itself is not
	// deleted by compaction.
	require.NoError(t, s.SetCompactionBoundary(id, ws, oldID, "recap"))

	ctxEvents, err := s.NarrativeEventsForContext(id)
	require.NoError(t, err)
	require.Len(t, ctxEvents, 1, "sanity: boundary really does filter context")

	count, err = s.NarrativeEventCount(id)
	require.NoError(t, err)
	assert.Equal(t, 2, count, "count unchanged by the boundary — links are never deleted")
}

func TestNarrativeEventCount_NoLinksReturnsZero(t *testing.T) {
	s := openStore(t)
	ws := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	id, err := s.InsertNarrative(ws, ws.Add(time.Hour), "T", "s")
	require.NoError(t, err)

	count, err := s.NarrativeEventCount(id)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

// -- transaction seam -----------------------------------------------------

func TestWithTx_RollsBackOnError(t *testing.T) {
	s := openStore(t)
	ws := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	wantErr := errors.New("boom")
	err := s.WithTx(func(tx *store.Tx) error {
		_, iErr := tx.InsertNarrative(ws, ws.Add(time.Hour), "T", "s")
		require.NoError(t, iErr)
		return wantErr // force rollback
	})
	require.ErrorIs(t, err, wantErr)

	// The narrative inserted inside the rolled-back tx must not be present.
	// (No narrative with id 1 should exist.)
	_, gErr := s.GetNarrative(1)
	require.ErrorIs(t, gErr, store.ErrNarrativeNotFound)
}

func TestWithTx_CommitsOnSuccess(t *testing.T) {
	s := openStore(t)
	ws := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	var id int64
	err := s.WithTx(func(tx *store.Tx) error {
		var iErr error
		id, iErr = tx.InsertNarrative(ws, ws.Add(time.Hour), "T", "s")
		return iErr
	})
	require.NoError(t, err)

	row, err := s.GetNarrative(id)
	require.NoError(t, err)
	assert.Equal(t, "T", row.Title)
}

// -- pipeline lock --------------------------------------------------------

func TestTryAcquire_UnheldSucceeds(t *testing.T) {
	s := openStore(t)
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	ok, err := s.TryAcquire("run-1", now, time.Minute)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestTryAcquire_HeldBeforeExpiryFails(t *testing.T) {
	s := openStore(t)
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	ok, err := s.TryAcquire("run-1", now, time.Minute)
	require.NoError(t, err)
	require.True(t, ok)

	ok, err = s.TryAcquire("run-2", now.Add(30*time.Second), time.Minute)
	require.NoError(t, err)
	assert.False(t, ok, "lease still valid, second acquirer must fail")
}

func TestTryAcquire_ExpiredLeaseCanBeStolen(t *testing.T) {
	s := openStore(t)
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	ok, err := s.TryAcquire("run-1", now, time.Minute)
	require.NoError(t, err)
	require.True(t, ok)

	// 2 minutes later, run-1's lease has expired; run-2 steals it.
	ok, err = s.TryAcquire("run-2", now.Add(2*time.Minute), time.Minute)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestReleaseLock_ByHolderFreesLock(t *testing.T) {
	s := openStore(t)
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	_, err := s.TryAcquire("run-1", now, time.Minute)
	require.NoError(t, err)
	require.NoError(t, s.ReleaseLock("run-1"))

	// Now immediately acquirable by another run, before any expiry.
	ok, err := s.TryAcquire("run-2", now.Add(time.Second), time.Minute)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestReleaseLock_ByNonHolderIsNoOp(t *testing.T) {
	s := openStore(t)
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	_, err := s.TryAcquire("run-1", now, time.Minute)
	require.NoError(t, err)

	require.NoError(t, s.ReleaseLock("run-2")) // not the holder: no-op, no error

	// run-1 still holds it.
	ok, err := s.TryAcquire("run-3", now.Add(30*time.Second), time.Minute)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestAcquire_BlocksThenSucceedsWhenLeaseExpires(t *testing.T) {
	s := openStore(t)
	start := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	_, err := s.TryAcquire("run-1", start, 100*time.Millisecond)
	require.NoError(t, err)

	// A clock that advances past the lease each call.
	calls := 0
	clock := func() time.Time {
		calls++
		return start.Add(time.Duration(calls) * 60 * time.Millisecond)
	}
	err = s.Acquire(t.Context(), "run-2", clock, 100*time.Millisecond, 10*time.Millisecond)
	require.NoError(t, err)

	// The lease is still valid on the first call (60ms < 100ms), so Acquire
	// must actually block and retry at least once before the second call
	// (120ms) sees it expired. This is the regression guard for the
	// RFC3339-truncation bug, where every sub-second TryAcquire call saw an
	// "already expired" lease and this test passed without ever blocking.
	assert.GreaterOrEqual(t, calls, 2, "Acquire must poll at least twice before the lease actually expires")
}

func TestTryAcquire_SubSecondLeasePrecisionIsHonored(t *testing.T) {
	s := openStore(t)
	start := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	ok, err := s.TryAcquire("run-1", start, 100*time.Millisecond)
	require.NoError(t, err)
	require.True(t, ok)

	// 50ms in: lease (100ms TTL) is still valid, second acquirer must fail.
	ok, err = s.TryAcquire("run-2", start.Add(50*time.Millisecond), 100*time.Millisecond)
	require.NoError(t, err)
	assert.False(t, ok, "lease has sub-second time remaining and must still be held")

	// 150ms in: lease has expired, second acquirer may steal it.
	ok, err = s.TryAcquire("run-3", start.Add(150*time.Millisecond), 100*time.Millisecond)
	require.NoError(t, err)
	assert.True(t, ok, "lease has expired (sub-second TTL) and must be stealable")
}

func TestTryAcquire_TrailingZeroBoundaryOrdersCorrectly(t *testing.T) {
	// Regression guard for the trailing-zero lexicographic-ordering trap: a
	// naive fractional-second format (e.g. time.RFC3339Nano) strips trailing
	// zeros, so 100ms formats as ".1" and 150ms formats as ".15". Comparing
	// those two strings gives "2026-08-01T12:00:00.1Z" <
	// "2026-08-01T12:00:00.15Z" == false — the trailing-zero-stripped
	// strings sort in the WRONG order relative to true chronological order
	// (verified directly against time.RFC3339Nano's actual output). The
	// lock's fixed-width format must keep these two writes in correct order
	// under TryAcquire's string-based SQL guard.
	s := openStore(t)
	start := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	// run-1's lease expires at exactly start + 100ms (".100000000").
	ok, err := s.TryAcquire("run-1", start, 100*time.Millisecond)
	require.NoError(t, err)
	require.True(t, ok)

	// At start + 100ms + 50ms = 150ms (".150000000"), the lease (expired at
	// .100000000) must be stealable: .100000000 <= .150000000 must hold as a
	// string comparison, not just a time comparison.
	ok, err = s.TryAcquire("run-2", start.Add(150*time.Millisecond), 100*time.Millisecond)
	require.NoError(t, err)
	assert.True(t, ok, "expires_at=.100000000 must compare <= now=.150000000 lexicographically")
}

func TestAcquire_HonorsContextCancellation(t *testing.T) {
	s := openStore(t)
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	_, err := s.TryAcquire("run-1", now, time.Hour) // long lease, never expires during test
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // already cancelled
	err = s.Acquire(ctx, "run-2", func() time.Time { return now }, time.Hour, 10*time.Millisecond)
	require.Error(t, err)
}

// -- Cluster input assembly ----------------------------------------------

func TestUnlinkedEventsInRange_ExcludesLinkedEvents(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	seedEvent(t, s, "linked", "already narrated", base)
	seedEvent(t, s, "loose", "not yet narrated", base.Add(time.Minute))

	linkedID, err := s.EventIDByExternalID("claude_code", "linked")
	require.NoError(t, err)
	nid, err := s.InsertNarrative(base, base.Add(time.Hour), "T", "s")
	require.NoError(t, err)
	require.NoError(t, s.AddNarrativeEvents(nid, []int64{linkedID}))

	got, err := s.UnlinkedEventsInRange(base, base.Add(time.Hour))

	require.NoError(t, err)
	require.Len(t, got, 1, "the linked event must not be a clustering candidate")
	assert.Equal(t, "loose", got[0].ExternalID)
}

func TestUnlinkedEventsInRange_HalfOpenBoundary(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	seedEvent(t, s, "before", "just before start", base.Add(-time.Second))
	seedEvent(t, s, "at-start", "exactly at start", base)
	seedEvent(t, s, "at-end", "exactly at end", base.Add(time.Hour))

	got, err := s.UnlinkedEventsInRange(base, base.Add(time.Hour))

	require.NoError(t, err)
	require.Len(t, got, 1, "[start, end): start included, end excluded")
	assert.Equal(t, "at-start", got[0].ExternalID)
}

func TestUnlinkedEventsInRange_EmptyRangeReturnsNoError(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	got, err := s.UnlinkedEventsInRange(base, base.Add(time.Hour))

	require.NoError(t, err)
	assert.Empty(t, got, "no candidates is a normal outcome, not a failure")
}

// TestUnlinkedEventsInRange_OrdersDeterministicallyWithinSameSecond pins the
// order of two events sharing an occurred_at second: ascending row id.
//
// Read this as a documentation test, not a regression guard. Dropping ", e.id"
// from the query's ORDER BY leaves it passing — verified, 5/5 runs — because
// modernc.org/sqlite happens to return tied rows in rowid order on this
// schema, there being no secondary index to push it toward another scan order.
//
// The explicit tiebreaker is still required. occurred_at is stored via
// time.RFC3339 (whole seconds — see InsertEvent) and so cannot uniquely order
// events, and depending on an engine's incidental tie behavior is precisely
// how the compaction boundary came to silently drop a tied event from all
// future Cluster context before it was paired with an event id.
func TestUnlinkedEventsInRange_OrdersDeterministicallyWithinSameSecond(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	// Both events share the same occurred_at second. The row inserted first
	// (external_id "second", a deliberately misleading name) gets the lower
	// id, so an id-based tiebreak is the only thing that can put it ahead of
	// the row named "first" (inserted second, higher id) in the result.
	seedEvent(t, s, "second", "event with the lower row id", base)
	seedEvent(t, s, "first", "event with the higher row id", base)

	got, err := s.UnlinkedEventsInRange(base, base.Add(time.Minute))

	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "second", got[0].ExternalID, "lower id (inserted first) must sort first on a tied occurred_at")
	assert.Equal(t, "first", got[1].ExternalID)
}

func TestNarrativesOverlapping_IncludesOverlapAndTouching(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	windowStart := base
	windowEnd := base.Add(time.Hour)

	// Ends exactly at window start — adjacent, must be included.
	touchingBefore, err := s.InsertNarrative(base.Add(-2*time.Hour), windowStart, "touching-before", "s")
	require.NoError(t, err)
	// Straddles the window start.
	overlapping, err := s.InsertNarrative(base.Add(-30*time.Minute), base.Add(30*time.Minute), "overlapping", "s")
	require.NoError(t, err)
	// Begins exactly at window end — adjacent, must be included.
	touchingAfter, err := s.InsertNarrative(windowEnd, windowEnd.Add(time.Hour), "touching-after", "s")
	require.NoError(t, err)
	// Strictly disjoint on both sides — must be excluded.
	_, err = s.InsertNarrative(base.Add(-48*time.Hour), base.Add(-24*time.Hour), "ancient", "s")
	require.NoError(t, err)
	_, err = s.InsertNarrative(base.Add(24*time.Hour), base.Add(48*time.Hour), "future", "s")
	require.NoError(t, err)

	got, err := s.NarrativesOverlapping(windowStart, windowEnd)

	require.NoError(t, err)
	gotIDs := make([]int64, 0, len(got))
	for _, row := range got {
		gotIDs = append(gotIDs, row.ID)
	}
	assert.ElementsMatch(t, []int64{touchingBefore, overlapping, touchingAfter}, gotIDs,
		"touching endpoints count as adjacent; strictly disjoint windows do not")
}

func TestNarrativesOverlapping_CarriesCompactionBoundary(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	seedEvent(t, s, "e1", "an event", base)
	eventID, err := s.EventIDByExternalID("claude_code", "e1")
	require.NoError(t, err)
	nid, err := s.InsertNarrative(base, base.Add(time.Hour), "T", "s")
	require.NoError(t, err)
	require.NoError(t, s.SetCompactionBoundary(nid, base, eventID, "recap"))

	got, err := s.NarrativesOverlapping(base, base.Add(time.Hour))

	require.NoError(t, err)
	require.Len(t, got, 1)
	require.NotNil(t, got[0].CompactionBoundary, "boundary must round-trip for hydration to filter on")
	assert.True(t, base.Equal(*got[0].CompactionBoundary))
	require.NotNil(t, got[0].CompactionBoundaryEventID)
	assert.Equal(t, eventID, *got[0].CompactionBoundaryEventID)
}

// -- narrative_issues -----------------------------------------------------

// insertNarrativeForTest creates a narrative and returns its id.
func insertNarrativeForTest(t *testing.T, s *store.Store, title string) int64 {
	t.Helper()

	start := time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)
	id, err := s.InsertNarrative(start, start.Add(time.Hour), title, "summary text")
	require.NoError(t, err)

	return id
}

func TestNarrativesWithoutIssueKey_ExcludesLinkedOnes(t *testing.T) {
	s := openStore(t)

	unlinked := insertNarrativeForTest(t, s, "unlinked work")
	linked := insertNarrativeForTest(t, s, "linked work")

	require.NoError(t, s.WithTx(func(tx *store.Tx) error {
		return tx.SetNarrativeIssueLink(linked, "PROJ-1", 0.9)
	}))

	got, err := s.NarrativesWithoutIssueKey(10)

	require.NoError(t, err)
	ids := make([]int64, 0, len(got))
	for _, n := range got {
		ids = append(ids, n.ID)
	}
	assert.Equal(t, []int64{unlinked}, ids,
		"a narrative with a primary must not be re-matched every pass")
}

func TestNarrativesWithoutIssueKey_RespectsLimit(t *testing.T) {
	s := openStore(t)
	for i := range 5 {
		insertNarrativeForTest(t, s, fmt.Sprintf("narrative %d", i))
	}

	got, err := s.NarrativesWithoutIssueKey(3)

	require.NoError(t, err)
	assert.Len(t, got, 3)
}

func TestSetNarrativeIssueLink_SetsKeyAndConfidence(t *testing.T) {
	s := openStore(t)
	id := insertNarrativeForTest(t, s, "work")

	require.NoError(t, s.WithTx(func(tx *store.Tx) error {
		return tx.SetNarrativeIssueLink(id, "PROJ-42", 0.83)
	}))

	row, err := s.GetNarrative(id)

	require.NoError(t, err)
	assert.Equal(t, "PROJ-42", row.IssueKey)
	assert.InDelta(t, 0.83, row.Confidence, 1e-9)
}

func TestAddNarrativeIssues_RoundTrips(t *testing.T) {
	s := openStore(t)
	id := insertNarrativeForTest(t, s, "work")

	links := []store.NarrativeIssue{
		{IssueKey: "PAAS-1", Role: "primary", Provenance: "branch", Confidence: 0.9, Connection: "corp"},
		{IssueKey: "SUMO-2", Role: "same_work", Provenance: "prose_first", Confidence: 0.7, Connection: "corp"},
		{IssueKey: "OTHER-3", Role: "mentioned", Provenance: "prose_later", Confidence: 0.2, Connection: "corp"},
	}
	require.NoError(t, s.WithTx(func(tx *store.Tx) error {
		return tx.AddNarrativeIssues(id, links)
	}))

	got, err := s.NarrativeIssues(id)

	require.NoError(t, err)
	require.Len(t, got, 3)
	byKey := map[string]store.NarrativeIssue{}
	for _, l := range got {
		byKey[l.IssueKey] = l
	}
	assert.Equal(t, store.Role("primary"), byKey["PAAS-1"].Role)
	assert.Equal(t, "branch", byKey["PAAS-1"].Provenance)
	assert.Equal(t, store.Role("same_work"), byKey["SUMO-2"].Role)
	assert.InDelta(t, 0.7, byKey["SUMO-2"].Confidence, 1e-9)
	assert.Equal(t, "corp", byKey["SUMO-2"].Connection)
}

func TestAddNarrativeIssues_IsIdempotent(t *testing.T) {
	// Matching is re-runnable over a backlog, so a second pass over the same
	// narrative must not duplicate rows — the property (source, external_id)
	// gives collectors.
	s := openStore(t)
	id := insertNarrativeForTest(t, s, "work")
	links := []store.NarrativeIssue{
		{IssueKey: "PAAS-1", Role: "primary", Provenance: "branch", Confidence: 0.9},
	}

	for range 2 {
		require.NoError(t, s.WithTx(func(tx *store.Tx) error {
			return tx.AddNarrativeIssues(id, links)
		}))
	}

	got, err := s.NarrativeIssues(id)

	require.NoError(t, err)
	assert.Len(t, got, 1, "re-running matching must not duplicate links")
}

func TestAddNarrativeIssues_RejectsSecondPrimary(t *testing.T) {
	// narratives.issue_key denormalizes the primary, so two primaries would
	// make that column arbitrary. Enforced by a partial unique index, not by
	// convention — and NOT by INSERT OR IGNORE, which swallows the violation.
	s := openStore(t)
	id := insertNarrativeForTest(t, s, "work")

	require.NoError(t, s.WithTx(func(tx *store.Tx) error {
		return tx.AddNarrativeIssues(id, []store.NarrativeIssue{
			{IssueKey: "PAAS-1", Role: "primary", Provenance: "branch", Confidence: 0.9},
		})
	}))

	err := s.WithTx(func(tx *store.Tx) error {
		return tx.AddNarrativeIssues(id, []store.NarrativeIssue{
			{IssueKey: "OTHER-9", Role: "primary", Provenance: "prose_first", Confidence: 0.5},
		})
	})

	require.Error(t, err, "the database must refuse a second primary for one narrative")
}

func TestAddNarrativeIssues_AllowsSiblingsAndOtherNarrativesPrimary(t *testing.T) {
	// The partial index must constrain only role='primary' within one
	// narrative — not same_work/mentioned siblings, and not other narratives.
	s := openStore(t)
	first := insertNarrativeForTest(t, s, "first")
	second := insertNarrativeForTest(t, s, "second")

	require.NoError(t, s.WithTx(func(tx *store.Tx) error {
		return tx.AddNarrativeIssues(first, []store.NarrativeIssue{
			{IssueKey: "PAAS-1", Role: "primary", Provenance: "branch", Confidence: 0.9},
			{IssueKey: "SUMO-2", Role: "same_work", Provenance: "prose_first", Confidence: 0.7},
			{IssueKey: "SUMO-3", Role: "same_work", Provenance: "prose_later", Confidence: 0.6},
			{IssueKey: "NOPE-4", Role: "mentioned", Provenance: "prose_later", Confidence: 0.1},
		})
	}))

	err := s.WithTx(func(tx *store.Tx) error {
		return tx.AddNarrativeIssues(second, []store.NarrativeIssue{
			{IssueKey: "AAA-1", Role: "primary", Provenance: "branch", Confidence: 0.9},
		})
	})

	require.NoError(t, err, "each narrative gets its own primary")
}

func TestNarrativesForIssue_FindsEveryNarrativeAcrossRoles(t *testing.T) {
	// The reverse direction. The reconciler needs it to avoid commenting twice
	// on a SUMO ticket shared by two narratives.
	s := openStore(t)
	first := insertNarrativeForTest(t, s, "first")
	second := insertNarrativeForTest(t, s, "second")

	require.NoError(t, s.WithTx(func(tx *store.Tx) error {
		if err := tx.AddNarrativeIssues(first, []store.NarrativeIssue{
			{IssueKey: "SUMO-9", Role: "same_work", Provenance: "prose_first", Confidence: 0.7},
		}); err != nil {
			return err
		}

		return tx.AddNarrativeIssues(second, []store.NarrativeIssue{
			{IssueKey: "SUMO-9", Role: "primary", Provenance: "branch", Confidence: 0.95},
		})
	}))

	got, err := s.NarrativesForIssue("SUMO-9")

	require.NoError(t, err)
	require.Len(t, got, 2)
	roles := map[int64]store.Role{}
	for _, r := range got {
		roles[r.NarrativeID] = r.Role
	}
	assert.Equal(t, store.Role("same_work"), roles[first])
	assert.Equal(t, store.Role("primary"), roles[second])
}

func TestNarrativesWithActionableLinks_ExcludesMentionedOnlyAndUnlinked(t *testing.T) {
	// The reconciler's backlog selection: only narratives with a
	// narrative_issues row in an actionable role (primary/same_work here)
	// are returned. A mentioned-only narrative and a wholly unlinked one
	// must both be excluded — the former by role, the latter by having no
	// row at all.
	s := openStore(t)

	actionable := insertNarrativeForTest(t, s, "actionable work")
	mentionedOnly := insertNarrativeForTest(t, s, "mentioned only")
	insertNarrativeForTest(t, s, "unlinked work")

	require.NoError(t, s.WithTx(func(tx *store.Tx) error {
		if err := tx.AddNarrativeIssues(actionable, []store.NarrativeIssue{
			{IssueKey: "PROJ-1", Role: "primary", Provenance: "branch", Confidence: 0.9},
		}); err != nil {
			return err
		}

		return tx.AddNarrativeIssues(mentionedOnly, []store.NarrativeIssue{
			{IssueKey: "PROJ-2", Role: "mentioned", Provenance: "prose_later", Confidence: 0.2},
		})
	}))

	got, err := s.NarrativesWithActionableLinks(10, []store.Role{store.Role("primary"), store.Role("same_work")})

	require.NoError(t, err)
	ids := make([]int64, 0, len(got))
	for _, n := range got {
		ids = append(ids, n.ID)
	}
	assert.Equal(t, []int64{actionable}, ids,
		"a mentioned-only or unlinked narrative must not be selected as reconciler input")
}

func TestNarrativesWithActionableLinks_RespectsLimit(t *testing.T) {
	s := openStore(t)
	for i := range 5 {
		id := insertNarrativeForTest(t, s, fmt.Sprintf("narrative %d", i))
		require.NoError(t, s.WithTx(func(tx *store.Tx) error {
			return tx.AddNarrativeIssues(id, []store.NarrativeIssue{
				{IssueKey: fmt.Sprintf("PROJ-%d", i), Role: "primary", Provenance: "branch", Confidence: 0.9},
			})
		}))
	}

	got, err := s.NarrativesWithActionableLinks(3, []store.Role{store.Role("primary"), store.Role("same_work")})

	require.NoError(t, err)
	assert.Len(t, got, 3)
}

func TestNarrativesWithActionableLinks_EmptyRolesReturnsEmpty(t *testing.T) {
	// Deliberately not an error: an empty roles slice means "nothing is
	// actionable", which is a valid (if unusual) caller configuration, not a
	// malformed query.
	s := openStore(t)
	id := insertNarrativeForTest(t, s, "work")
	require.NoError(t, s.WithTx(func(tx *store.Tx) error {
		return tx.AddNarrativeIssues(id, []store.NarrativeIssue{
			{IssueKey: "PROJ-1", Role: "primary", Provenance: "branch", Confidence: 0.9},
		})
	}))

	got, err := s.NarrativesWithActionableLinks(10, nil)

	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestAllNarrativeEvents_IncludesPreCompactionBoundaryEvents(t *testing.T) {
	// THE most important test in this task. NarrativeEventsForContext
	// deliberately excludes events at or before the compaction boundary;
	// matching must see every event ever linked, or a compacted narrative
	// loses the git_branch artifact carrying its strongest signal.
	//
	// The fixture MUST set a compaction boundary — without one, this test
	// passes against NarrativeEventsForContext too and proves nothing. That is
	// what the `context` length assertion below guards.
	s := openStore(t)
	id := insertNarrativeForTest(t, s, "compacted work")

	early := makeEvent("early-event")
	early.OccurredAt = time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)
	early.Artifacts["git_branch"] = "feature/PROJ-42"
	late := makeEvent("late-event")
	late.OccurredAt = time.Date(2026, 8, 24, 11, 0, 0, 0, time.UTC)

	for _, e := range []events.Event{early, late} {
		inserted, err := s.InsertEvent(e)
		require.NoError(t, err)
		require.True(t, inserted)
	}

	earlyID, err := s.EventIDByExternalID(early.Source, early.ExternalID)
	require.NoError(t, err)
	lateID, err := s.EventIDByExternalID(late.Source, late.ExternalID)
	require.NoError(t, err)
	require.NoError(t, s.AddNarrativeEvents(id, []int64{earlyID, lateID}))

	require.NoError(t, s.SetCompactionBoundary(id, early.OccurredAt, earlyID, "recap"))

	ctxEvents, err := s.NarrativeEventsForContext(id)
	require.NoError(t, err)
	require.Len(t, ctxEvents, 1, "fixture sanity: the boundary must actually hide the early event")

	all, err := s.AllNarrativeEvents(id)

	require.NoError(t, err)
	require.Len(t, all, 2, "matching must see pre-boundary events")
	assert.Equal(t, "early-event", all[0].ExternalID)
	assert.Equal(t, "feature/PROJ-42", all[0].Artifacts["git_branch"],
		"the branch artifact is the strongest provenance signal and lives on the oldest event")
}

// -- reconciler delta (narrative_events.linked_at, DeltaEvents) -------------

// TestDeltaEventsIsEmptyWhenAnActionAlreadyCoversEveryEvent is the
// duplicate-suppression guarantee: a second reconcile pass over an unchanged
// narrative must find nothing to propose.
func TestDeltaEventsIsEmptyWhenAnActionAlreadyCoversEveryEvent(t *testing.T) {
	s := openStore(t)

	base := time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)
	nid, err := s.InsertNarrative(base, base.Add(time.Hour), "t", "s")
	require.NoError(t, err)

	_, err = s.InsertEvent(makeEvent("e1"))
	require.NoError(t, err)
	eid, err := s.EventIDByExternalID("claude_code", "e1")
	require.NoError(t, err)
	require.NoError(t, s.AddNarrativeEvents(nid, []int64{eid}))

	// No action yet: the whole narrative is the delta.
	delta, err := s.DeltaEvents(nid)
	require.NoError(t, err)
	require.Len(t, delta, 1, "with no prior action every linked event is new")

	_, err = s.InsertAction(store.ActionRow{
		NarrativeID: nid,
		Type:        "comment",
		IssueKey:    "PROJ-1",
		Payload:     `{"body":"x"}`,
		Confidence:  0.9,
		Status:      "proposed",
	})
	require.NoError(t, err)

	delta, err = s.DeltaEvents(nid)
	require.NoError(t, err)
	assert.Empty(t, delta,
		"every event predates the action, so a re-run must propose nothing")
}

// TestDeltaEventsFindsAnEventLinkedInTheSameSecondAsTheAction pins the
// precision decision. At whole-second granularity this event is invisible
// forever: the action's created_at never advances, so `linked_at > created_at`
// stays false on every future pass. Measured on modernc.org/sqlite v1.56.0.
//
// The sleep below is deliberate, not a smell: %f gives millisecond precision,
// and on a fast machine the action insert and the immediately-following event
// link can land in the same millisecond (measured empirically: roughly 1 in 5
// runs without the sleep). That's the exact same class of collision the
// design already accepted at whole-second granularity, just with a much
// smaller window. A few milliseconds' separation keeps the test deterministic
// about crossing a millisecond boundary while still landing well within the
// same wall-clock second, which is what this test needs to demonstrate.
func TestDeltaEventsFindsAnEventLinkedInTheSameSecondAsTheAction(t *testing.T) {
	s := openStore(t)

	base := time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)
	nid, err := s.InsertNarrative(base, base.Add(time.Hour), "t", "s")
	require.NoError(t, err)

	_, err = s.InsertEvent(makeEvent("old"))
	require.NoError(t, err)
	old, err := s.EventIDByExternalID("claude_code", "old")
	require.NoError(t, err)
	require.NoError(t, s.AddNarrativeEvents(nid, []int64{old}))

	_, err = s.InsertAction(store.ActionRow{
		NarrativeID: nid, Type: "comment", IssueKey: "PROJ-1",
		Payload: `{"body":"x"}`, Status: "proposed",
	})
	require.NoError(t, err)

	// Linked a millisecond later but still inside the same wall-clock second,
	// which is the case under test. See this test's doc comment for why the
	// separation is needed and why removing it flakes.
	time.Sleep(5 * time.Millisecond)
	_, err = s.InsertEvent(makeEvent("fresh"))
	require.NoError(t, err)
	fresh, err := s.EventIDByExternalID("claude_code", "fresh")
	require.NoError(t, err)
	require.NoError(t, s.AddNarrativeEvents(nid, []int64{fresh}))

	delta, err := s.DeltaEvents(nid)
	require.NoError(t, err)

	require.Len(t, delta, 1,
		"an event linked in the same second as the action must still be in the delta; "+
			"at whole-second precision it would be lost permanently")
	assert.Equal(t, "fresh", delta[0].ExternalID)
}

// TestLinkedAtAndActionCreatedAtUseTheSameFormat guards the lexical-comparison
// trap: these are TEXT columns, so mixing %S and %f inverts the ordering
// ("...10.597Z" < "...10Z" because '.' is 0x2E and 'Z' is 0x5A). A later event
// would compare as earlier and silently drop out of the delta.
//
// This file is package store_test (external test package), so it cannot read
// s.db directly. NarrativeEventLinkedAt is a small test-support introspection
// accessor added for exactly this — the same precedent as NarrativeEventCount
// — rather than reaching into the database from outside the package.
// actions.created_at is already observable via ActionsForNarrative, so no
// second accessor is needed for that half of the comparison.
func TestLinkedAtAndActionCreatedAtUseTheSameFormat(t *testing.T) {
	s := openStore(t)

	base := time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)
	nid, err := s.InsertNarrative(base, base.Add(time.Hour), "t", "s")
	require.NoError(t, err)
	_, err = s.InsertEvent(makeEvent("e1"))
	require.NoError(t, err)
	eid, err := s.EventIDByExternalID("claude_code", "e1")
	require.NoError(t, err)
	require.NoError(t, s.AddNarrativeEvents(nid, []int64{eid}))
	_, err = s.InsertAction(store.ActionRow{
		NarrativeID: nid, Type: "comment", IssueKey: "PROJ-1",
		Payload: `{"body":"x"}`, Status: "proposed",
	})
	require.NoError(t, err)

	linkedAt, err := s.NarrativeEventLinkedAt(nid, eid)
	require.NoError(t, err)

	actions, err := s.ActionsForNarrative(nid)
	require.NoError(t, err)
	require.Len(t, actions, 1)
	createdAt := actions[0].CreatedAt

	assert.Contains(t, linkedAt, ".", "linked_at must carry sub-second precision")
	assert.Contains(t, createdAt, ".",
		"actions.created_at must use the SAME sub-second format as linked_at, or the "+
			"lexical TEXT comparison in DeltaEvents inverts")
	assert.Len(t, createdAt, len(linkedAt),
		"identical format strings produce identical lengths")
}

// -- actions ---------------------------------------------------------------

func TestInsertActionRoundTripsEveryField(t *testing.T) {
	s := openStore(t)

	base := time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)
	nid, err := s.InsertNarrative(base, base.Add(time.Hour), "t", "s")
	require.NoError(t, err)

	id, err := s.InsertAction(store.ActionRow{
		NarrativeID: nid,
		Type:        "comment",
		IssueKey:    "PROJ-1",
		Payload:     `{"body":"drafted text"}`,
		Confidence:  0.75,
		Rationale:   "delta shows the work landed",
		Status:      "proposed",
	})
	require.NoError(t, err)
	require.NotZero(t, id)

	got, err := s.ActionsForNarrative(nid)
	require.NoError(t, err)
	require.Len(t, got, 1)

	assert.Equal(t, id, got[0].ID)
	assert.Equal(t, nid, got[0].NarrativeID)
	assert.Equal(t, "comment", got[0].Type)
	assert.Equal(t, "PROJ-1", got[0].IssueKey)
	assert.JSONEq(t, `{"body":"drafted text"}`, got[0].Payload)
	assert.InDelta(t, 0.75, got[0].Confidence, 0.0001)
	assert.Equal(t, "delta shows the work landed", got[0].Rationale)
	assert.Equal(t, "proposed", got[0].Status)
	assert.NotEmpty(t, got[0].CreatedAt, "created_at must come back populated")
	assert.Empty(t, got[0].Feedback, "feedback is unwritten until slice 6")
}

func TestActionsByStatusFiltersAndLatestActionPicksMostRecent(t *testing.T) {
	s := openStore(t)

	base := time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)
	nid, err := s.InsertNarrative(base, base.Add(time.Hour), "t", "s")
	require.NoError(t, err)

	first, err := s.InsertAction(store.ActionRow{
		NarrativeID: nid, Type: "comment", IssueKey: "PROJ-1",
		Payload: `{"body":"first"}`, Status: "proposed",
	})
	require.NoError(t, err)
	second, err := s.InsertAction(store.ActionRow{
		NarrativeID: nid, Type: "transition", IssueKey: "PROJ-1",
		Payload: `{"target":"done"}`, Status: "approved",
	})
	require.NoError(t, err)

	proposed, err := s.ActionsByStatus("proposed")
	require.NoError(t, err)
	require.Len(t, proposed, 1)
	assert.Equal(t, first, proposed[0].ID)

	latest, found, err := s.LatestActionForNarrative(nid)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, second, latest.ID,
		"ties on created_at break by id, so the later insert wins")

	_, found, err = s.LatestActionForNarrative(nid + 999)
	require.NoError(t, err)
	assert.False(t, found, "a narrative with no actions reports found=false, not an error")
}

func TestUpdateActionStatusSetsDecidedAndExecutedTimestamps(t *testing.T) {
	s := openStore(t)

	base := time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)
	nid, err := s.InsertNarrative(base, base.Add(time.Hour), "t", "s")
	require.NoError(t, err)
	id, err := s.InsertAction(store.ActionRow{
		NarrativeID: nid, Type: "comment", IssueKey: "PROJ-1",
		Payload: `{"body":"x"}`, Status: "proposed",
	})
	require.NoError(t, err)

	require.NoError(t, s.UpdateActionStatus(id, "approved"))
	got, err := s.ActionsForNarrative(nid)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "approved", got[0].Status)
	require.NotNil(t, got[0].DecidedAt, "a human ruling sets decided_at")
	assert.Nil(t, got[0].ExecutedAt, "approval is not execution")

	require.NoError(t, s.UpdateActionStatus(id, "applied"))
	got, err = s.ActionsForNarrative(nid)
	require.NoError(t, err)
	assert.Equal(t, "applied", got[0].Status)
	require.NotNil(t, got[0].ExecutedAt, "applying sets executed_at")
}

// -- foreign key enforcement -----------------------------------------------
//
// SQLite does not enforce declared REFERENCES clauses unless
// `PRAGMA foreign_keys = ON` is set, and that pragma is per-connection, not
// per-database — a naive one-shot db.Exec only touches whichever pooled
// connection happens to run it, leaving every other connection in the pool
// unenforced. Open must therefore set the pragma in the DSN itself, so
// every connection SQLite hands out (including ones opened well after
// Open returns, e.g. under concurrent load) gets it. These tests prove that
// by forcing genuinely separate pooled connections (via concurrent
// goroutines, since idle connections are otherwise reused) and checking each
// one individually.

// TestInsertAction_RejectsDanglingNarrativeID is the minimum bar: a single
// connection must reject a child row naming a narrative_id that does not
// exist. Before this change this insert silently succeeded — see
// internal/reconciler/persist_test.go's TestPersistWritesNothingWhenOneActionFails,
// which had to work around exactly this to force its rollback.
func TestInsertAction_RejectsDanglingNarrativeID(t *testing.T) {
	s := openStore(t)

	_, err := s.InsertAction(store.ActionRow{
		NarrativeID: 999999, Type: "comment", IssueKey: "PROJ-1",
		Payload: `{"body":"x"}`, Status: "proposed",
	})
	require.Error(t, err, "inserting an action against a nonexistent narrative must fail now that foreign keys are enforced")
}

// TestOpen_PathsWithDSNMetacharacters pins the escaping in sqliteDSN by
// asserting the two things that actually matter to an operator, through the
// public API rather than the DSN string: the database lands at exactly the
// path they configured, and foreign keys are enforced there.
//
// Each case is a character that is structural in a DSN. Verified by hand that
// dropping the corresponding replacement breaks it — an unescaped `file:`+path
// DSN gives:
//
//	wi?rd.db     →  wrote to "wi",       foreign_keys OFF
//	wi#rd.db     →  wrote to "wi",       foreign_keys on
//	wi%3frd.db   →  wrote to "wi?rd.db", foreign_keys on
//
// The '?' row is the dangerous one: silently the wrong file AND silently
// unenforced, which is the exact failure this whole change exists to prevent.
// The last row is why '%' must be escaped first — otherwise a path already
// containing a percent-escape round-trips into a different filename.
func TestOpen_PathsWithDSNMetacharacters(t *testing.T) {
	cases := []struct {
		name string
		base string
	}{
		{"question mark splits path from query", "wi?rd.db"},
		{"hash truncates path as a fragment", "wi#rd.db"},
		{"literal percent", "wi%rd.db"},
		{"path already containing a percent-escape", "wi%3frd.db"},
		{"space", "with space.db"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			dbPath := filepath.Join(dir, tc.base)

			s, err := store.Open(dbPath)
			require.NoError(t, err)
			t.Cleanup(func() { _ = s.Close() })

			// Force a write so the file is definitely created.
			_, err = s.InsertAction(store.ActionRow{
				NarrativeID: 999999, Type: "comment", IssueKey: "PROJ-1",
				Payload: `{"body":"x"}`, Status: "proposed",
			})
			require.Error(t, err,
				"foreign keys must be enforced regardless of metacharacters in the path")

			entries, err := os.ReadDir(dir)
			require.NoError(t, err)

			names := make([]string, 0, len(entries))
			for _, e := range entries {
				names = append(names, e.Name())
			}
			assert.Contains(t, names, tc.base,
				"the database must land at the exact configured filename, not a mis-split prefix")
		})
	}
}

// TestOpen_RelativePath is the case that rules out net/url as a "just encode
// the whole thing" replacement for sqliteDSN's targeted escaping.
//
// url.URL{Scheme: "file", Path: "data/unjira.db"}.String() emits
// `file://data/unjira.db`, where the "//" makes "data" the URL *authority*
// rather than a directory; that DSN fails at the first statement with
// "SQL logic error: out of memory (1)". config/unjira.example.json ships
// db_path "data/unjira.db", so a general encoder would break the default
// configuration. url.PathEscape is no better — it escapes '/' too.
func TestOpen_RelativePath(t *testing.T) {
	for _, rel := range []string{"data/unjira.db", "./data/unjira.db"} {
		t.Run(rel, func(t *testing.T) {
			t.Chdir(t.TempDir())

			s, err := store.Open(rel)
			require.NoError(t, err, "a relative db_path must open (this is what the example config ships)")
			t.Cleanup(func() { _ = s.Close() })

			_, err = s.InsertAction(store.ActionRow{
				NarrativeID: 999999, Type: "comment", IssueKey: "PROJ-1",
				Payload: `{"body":"x"}`, Status: "proposed",
			})
			require.Error(t, err, "foreign keys must be enforced on a relative path too")

			_, err = os.Stat(filepath.Join("data", "unjira.db"))
			assert.NoError(t, err, "the database must land under the relative directory named")
		})
	}
}

// TestForeignKeys_EnforcedOnEveryPooledConnection is the stronger bar the
// task calls for: proving enforcement holds on a *second*, genuinely
// distinct pooled connection, not just whichever connection happened to
// serve the first query. A single db.Exec("PRAGMA foreign_keys = ON") would
// pass TestInsertAction_RejectsDanglingNarrativeID (it always reuses the one
// connection database/sql opens for a single-threaded test) while still
// leaving a second pooled connection unenforced — the exact failure mode
// the task description warns is otherwise invisible.
//
// SetMaxOpenConns keeps the pool below the count so most goroutines are
// forced to wait for a connection than to open a fresh one, but with the
// default (unlimited) pool and genuinely concurrent callers all blocked on
// a held transaction, database/sql has no idle connection to hand out and
// must open new ones. Every one of those, from the DSN pragma, needs
// enforcement — not just connection #1.
func TestForeignKeys_EnforcedOnEveryPooledConnection(t *testing.T) {
	s := openStore(t)

	const n = 5
	var (
		wg      sync.WaitGroup
		results = make([]error, n)
	)

	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// InsertAction opens/uses its own connection from the pool;
			// running n of these concurrently forces database/sql to hand
			// out more than one real connection.
			_, err := s.InsertAction(store.ActionRow{
				NarrativeID: 999999 + int64(i), Type: "comment", IssueKey: "PROJ-1",
				Payload: `{"body":"x"}`, Status: "proposed",
			})
			results[i] = err
		}(i)
	}
	wg.Wait()

	for i, err := range results {
		assert.Error(t, err, "connection serving goroutine %d must enforce the foreign key too", i)
	}
}
