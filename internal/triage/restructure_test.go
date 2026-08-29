package triage

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/store"
)

func TestResolveMergeTarget(t *testing.T) {
	cases := []struct {
		name          string
		a, b          commitState
		wantTarget    int64
		wantSource    int64
		wantErrPhrase string
	}{
		{
			name: "neither committed: first named wins",
			a:    commitState{NarrativeID: 7}, b: commitState{NarrativeID: 9},
			wantTarget: 7, wantSource: 9,
		},
		{
			name: "a committed: a absorbs b",
			a:    commitState{NarrativeID: 7, Committed: true}, b: commitState{NarrativeID: 9},
			wantTarget: 7, wantSource: 9,
		},
		{
			name: "b committed: b absorbs a, IGNORING argument order",
			a:    commitState{NarrativeID: 7}, b: commitState{NarrativeID: 9, Committed: true},
			wantTarget: 9, wantSource: 7,
		},
		{
			name:          "both committed: refused, naming both",
			a:             commitState{NarrativeID: 7, Committed: true},
			b:             commitState{NarrativeID: 9, Committed: true},
			wantErrPhrase: "both have committed actions",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target, source, err := resolveMergeTarget(tc.a, tc.b)

			if tc.wantErrPhrase != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErrPhrase)
				assert.Contains(t, err.Error(), "7")
				assert.Contains(t, err.Error(), "9",
					"the error must name BOTH narratives so a human can go look at them")

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.wantTarget, target)
			assert.Equal(t, tc.wantSource, source)
		})
	}
}

// TestStoreHandler_MergeMovesOnlyEligibleEvents is the end-to-end merge against
// a real store: the committed narrative keeps its frozen events, the source's
// uncommitted ones move, and nothing ends up double-linked.
func TestStoreHandler_MergeMovesOnlyEligibleEvents(t *testing.T) {
	s := handlerTestStore(t)
	base := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)

	target, targetEvents := seedHandlerNarrative(t, s, "target", base, "sh:t1", "sh:t2")
	commitAgainstNarrative(t, s, target)

	time.Sleep(5 * time.Millisecond)
	source, sourceEvents := seedHandlerNarrative(t, s, "source", base.Add(time.Hour), "sh:s1", "sh:s2")

	h := NewStoreHandler(s)
	moved, err := h.MergeNarratives(target, source)

	require.NoError(t, err)
	assert.ElementsMatch(t, sourceEvents, moved, "all of the source's uncommitted events moved")

	targetCount, err := s.NarrativeEventCount(target)
	require.NoError(t, err)
	sourceCount, err := s.NarrativeEventCount(source)
	require.NoError(t, err)

	assert.Equal(t, 4, targetCount, "target holds its own 2 frozen plus the source's 2")
	assert.Equal(t, 0, sourceCount, "source is emptied, not double-linked")

	// The target's committed events are STILL frozen: nothing laundered them.
	stillEligible, err := s.EligibleEventIDs(target)
	require.NoError(t, err)
	for _, id := range targetEvents {
		assert.NotContains(t, stillEligible, id, "a committed event must remain frozen after a merge")
	}
}

// TestStoreHandler_MergeRefusesAFullyCommittedSource: if every event on the
// source is already described by a tracker mutation, there is nothing to move,
// and silently succeeding would report a merge that changed nothing.
func TestStoreHandler_MergeRefusesAFullyCommittedSource(t *testing.T) {
	s := handlerTestStore(t)
	base := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)

	target, _ := seedHandlerNarrative(t, s, "target", base, "sh:t1")
	source, _ := seedHandlerNarrative(t, s, "source", base.Add(time.Hour), "sh:s1")
	commitAgainstNarrative(t, s, source)

	_, err := NewStoreHandler(s).MergeNarratives(target, source)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no uncommitted events")
}

func handlerTestStore(t *testing.T) *store.Store {
	t.Helper()

	s, err := store.Open(filepath.Join(t.TempDir(), "triage.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	return s
}

func seedHandlerNarrative(
	t *testing.T, s *store.Store, title string, at time.Time, extIDs ...string,
) (int64, []int64) {
	t.Helper()

	nid, err := s.InsertNarrative(at, at.Add(time.Hour), title, "s")
	require.NoError(t, err)

	ids := make([]int64, 0, len(extIDs))
	for i, ext := range extIDs {
		e := events.NewEvent("claude_code", ext, at.Add(time.Duration(i)*time.Minute), "w:"+ext)
		_, err := s.InsertEvent(e)
		require.NoError(t, err)

		eid, err := s.EventIDByExternalID("claude_code", ext)
		require.NoError(t, err)
		require.NoError(t, s.AddNarrativeEvents(nid, []int64{eid}))
		ids = append(ids, eid)
	}

	return nid, ids
}

func commitAgainstNarrative(t *testing.T, s *store.Store, nid int64) {
	t.Helper()

	id, err := s.InsertAction(store.ActionRow{
		NarrativeID: nid, Type: "comment", IssueKey: "PROJ-1",
		Payload: `{"body":"posted"}`, Status: "proposed",
	})
	require.NoError(t, err)
	require.NoError(t, s.UpdateActionStatus(id, "applied"))
}

// TestStoreHandler_ResolveMergeTargetReadsRealCommitState wires the direction
// rule to the store: the committed narrative absorbs the other regardless of
// argument order.
func TestStoreHandler_ResolveMergeTargetReadsRealCommitState(t *testing.T) {
	s := handlerTestStore(t)
	base := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)

	uncommitted, _ := seedHandlerNarrative(t, s, "uncommitted", base, "rm:u1")
	committed, _ := seedHandlerNarrative(t, s, "committed", base.Add(time.Hour), "rm:c1")
	commitAgainstNarrative(t, s, committed)

	h := NewStoreHandler(s)

	// Name the UNCOMMITTED one first: direction must still favour the committed.
	target, source, err := h.ResolveMergeTarget(uncommitted, committed)

	require.NoError(t, err)
	assert.Equal(t, committed, target, "the committed narrative is the workstream of record")
	assert.Equal(t, uncommitted, source)
}

// TestStoreHandler_ResolveMergeTargetRefusesTwoCommitted: two tracker mutations
// already claim this work, so which issue is authoritative is not a decision a
// review loop should make silently.
func TestStoreHandler_ResolveMergeTargetRefusesTwoCommitted(t *testing.T) {
	s := handlerTestStore(t)
	base := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)

	a, _ := seedHandlerNarrative(t, s, "a", base, "rm:a1")
	b, _ := seedHandlerNarrative(t, s, "b", base.Add(time.Hour), "rm:b1")
	commitAgainstNarrative(t, s, a)
	commitAgainstNarrative(t, s, b)

	_, _, err := NewStoreHandler(s).ResolveMergeTarget(a, b)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "both have committed actions")
}
