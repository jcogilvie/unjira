package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/store"
)

func seedN(t *testing.T, s *store.Store, title string, extIDs ...string) (int64, []int64) {
	t.Helper()
	base := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)
	nid, err := s.InsertNarrative(base, base.Add(time.Hour), title, "s")
	require.NoError(t, err)

	ids := make([]int64, 0, len(extIDs))
	for i, ext := range extIDs {
		e := events.NewEvent("claude_code", ext, base.Add(time.Duration(i)*time.Minute), "w:"+ext)
		_, err := s.InsertEvent(e)
		require.NoError(t, err)
		eid, err := s.EventIDByExternalID("claude_code", ext)
		require.NoError(t, err)
		require.NoError(t, s.AddNarrativeEvents(nid, []int64{eid}))
		ids = append(ids, eid)
	}

	return nid, ids
}

func commitAgainst(t *testing.T, s *store.Store, nid int64) {
	t.Helper()
	id, err := s.InsertAction(store.ActionRow{
		NarrativeID: nid, Type: "comment", IssueKey: "PROJ-1",
		Payload: `{"body":"posted"}`, Status: "proposed",
	})
	require.NoError(t, err)
	require.NoError(t, s.UpdateActionStatus(id, "applied"))
}

// TestMergeFlow_CommittedNarrativeIsTheTarget is the whole merge mechanic under
// direction-by-commitment, proven without an LLM: A is committed, B is not, so A
// is the target and B's events move onto A. A's frozen events never move, which
// is what makes the per-narrative watermark safe under restructuring.
func TestMergeFlow_CommittedNarrativeIsTheTarget(t *testing.T) {
	s := openStore(t)

	a, aEvents := seedN(t, s, "A committed", "m:a1", "m:a2")
	commitAgainst(t, s, a)

	// linked_at has millisecond precision, so sleep past the commit instant
	// rather than racing it.
	time.Sleep(5 * time.Millisecond)
	b, bEvents := seedN(t, s, "B uncommitted", "m:b1")

	eligibleOnB, err := s.EligibleEventIDs(b)
	require.NoError(t, err)
	require.ElementsMatch(t, bEvents, eligibleOnB, "B never committed: all of B is eligible")

	eligibleOnA, err := s.EligibleEventIDs(a)
	require.NoError(t, err)
	require.Empty(t, eligibleOnA, "A's events predate its commit: frozen")

	// Direction: A committed, B not => A is the target. Move only B's ELIGIBLE
	// events onto A, then unlink them from B so nothing is double-linked.
	require.NoError(t, s.AddNarrativeEvents(a, eligibleOnB))
	require.NoError(t, s.UnlinkNarrativeEvents(b, eligibleOnB))

	countA, err := s.NarrativeEventCount(a)
	require.NoError(t, err)
	countB, err := s.NarrativeEventCount(b)
	require.NoError(t, err)

	assert.Equal(t, 3, countA, "A holds its own 2 frozen events plus B's 1")
	assert.Equal(t, 0, countB, "B is emptied, not double-linked")

	// Nothing laundered A's committed events into eligibility.
	stillFrozen, err := s.EligibleEventIDs(a)
	require.NoError(t, err)
	assert.NotContains(t, stillFrozen, aEvents[0], "A's committed event must remain frozen")
	assert.NotContains(t, stillFrozen, aEvents[1], "A's committed event must remain frozen")
	assert.Contains(t, stillFrozen, bEvents[0], "the newly-moved event is eligible: linked after A's commit")
}
