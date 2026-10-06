package reconciler

// create_backlog_test.go covers the create path's starvation: narratives the pass had
// already handled kept their selection slots forever.
//
// ProposeCreates selected the oldest untracked narratives by (window_start, id) and only
// then, in proposeCreateOne, skipped the ones it had already handled — an open or
// applied create, a decline with nothing new since, a narrative of unjira's own output
// only. Stable order plus a skip the selector cannot see is design-notes #29's livelock.
// Measured on a real 30-day store: 129 untracked narratives under a cap of 20; pass 1
// proposed 11 and declined 9, every later pass proposed nothing, and 109 narratives were
// never examined. The store half (one shared predicate for the selector and the count)
// is pinned in internal/store/createbacklog_test.go; this file pins the wiring.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/store"
)

// selfAuthored builds an event unjira itself wrote, which dropSelfAuthored removes.
func selfAuthored(externalID string) events.Event {
	e := events.NewEvent("jira", externalID,
		time.Date(2026, 8, 28, 9, 30, 0, 0, time.UTC), "unjira commented on DEVSBX-9")
	e.Artifacts = map[string]any{events.ArtifactAuthoredByUnjira: true}

	return e
}

// linkMember inserts e and links it to narrativeID as a member.
func linkMember(t *testing.T, s *store.Store, narrativeID int64, e events.Event) {
	t.Helper()

	_, err := s.InsertEvent(e)
	require.NoError(t, err)
	eid, err := s.EventIDByExternalID(e.Source, e.ExternalID)
	require.NoError(t, err)
	require.NoError(t, s.LinkMembers(narrativeID, []int64{eid}, 1))
}

func capOf(n int) config.ReconcilerConfig {
	cfg := testConfig()
	cfg.MaxNarrativesPerPass = n

	return cfg
}

// TestProposeCreates_HandledNarrativesYieldTheirSlots is the starvation reproduction in
// miniature: a cap of 3, the three oldest narratives each handled a different way by
// pass 1 (proposed, declined, self-authored only), and a fourth behind them. Pass 2 must
// reach the fourth. Before the fix it re-selected the same three, skipped all of them in
// Go, and the fourth was never examined on any pass.
func TestProposeCreates_HandledNarrativesYieldTheirSlots(t *testing.T) {
	s := reconcileStore(t)
	proposed := seedUntracked(t, s, codeEvent("sv:1", "reworked the retry path"))
	declinedID := seedUntracked(t, s, codeEvent("sv:2", "ten minutes on a flaky test"))
	ownOutput := seedUntracked(t, s, selfAuthored("sv:3"))
	behind := seedUntracked(t, s, codeEvent("sv:4", "built the export command"))

	client := &fakeLLM{responses: []string{worthTracking, declined, worthTracking}}

	pass1, _, err := ProposeCreates(context.Background(), s, client, capOf(3), nil, nil)
	require.NoError(t, err)
	require.Equal(t, []int64{proposed, declinedID, ownOutput}, resultIDs(pass1),
		"precondition: the cap admits the three oldest")
	_, err = Persist(s, pass1)
	require.NoError(t, err)

	pass2, _, err := ProposeCreates(context.Background(), s, client, capOf(3), nil, nil)
	require.NoError(t, err)
	require.Equal(t, []int64{behind}, resultIDs(pass2),
		"every narrative pass 1 handled must yield its slot, or the work behind them is starved")
	assert.Len(t, pass2[0].Proposed, 1)
	assert.Len(t, client.prompts, 3,
		"one model call per narrative that needed a judgment; none spent re-skipping handled ones")
	_, err = Persist(s, pass2)
	require.NoError(t, err)

	pass3, _, err := ProposeCreates(context.Background(), s, client, capOf(3), nil, nil)
	require.NoError(t, err)
	assert.Empty(t, pass3, "a drained create backlog selects nothing")
}

// TestProposeCreates_ASelfAuthoredNarrativeIsExaminedOnce: the self-authored skip must
// leave a trace, because unlike a decline it drafts nothing, and an outcome that leaves
// no trace under a stable order is the livelock. New member work re-opens it.
func TestProposeCreates_ASelfAuthoredNarrativeIsExaminedOnce(t *testing.T) {
	s := reconcileStore(t)
	nid := seedUntracked(t, s, selfAuthored("so:1"))
	client := &fakeLLM{responses: []string{worthTracking}}

	pass1, _, err := ProposeCreates(context.Background(), s, client, testConfig(), nil, nil)
	require.NoError(t, err)
	require.Len(t, pass1, 1)
	assert.Contains(t, pass1[0].Suppressed[0], "unjira's own output")

	pass2, _, err := ProposeCreates(context.Background(), s, client, testConfig(), nil, nil)
	require.NoError(t, err)
	assert.Empty(t, pass2, "examined and entirely self-authored: not selected again")

	linkMember(t, s, nid, events.NewEvent("claude_code", "so:2",
		time.Date(2026, 8, 28, 15, 0, 0, 0, time.UTC), "real work arrived"))

	pass3, _, err := ProposeCreates(context.Background(), s, client, testConfig(), nil, nil)
	require.NoError(t, err)
	require.Equal(t, []int64{nid}, resultIDs(pass3), "new member work re-opens the narrative")
	assert.Len(t, pass3[0].Proposed, 1)
	assert.Len(t, client.prompts, 1, "the model is asked only once something real exists")
}

// TestProposeCreates_ADeclineWhoseNewEventsAreUnjirasIsExaminedOnce: a declined
// narrative whose only new member events are unjira's own passes the decline clause
// (there is a link past it), and proposeCreateOne then skips it on the self-authored
// delta. That skip must leave a trace too, or it becomes the same livelock one step on.
func TestProposeCreates_ADeclineWhoseNewEventsAreUnjirasIsExaminedOnce(t *testing.T) {
	s := reconcileStore(t)
	nid := seedUntracked(t, s, codeEvent("dd:1", "poked at something"))
	client := &fakeLLM{responses: []string{declined}}

	_, _, err := ProposeCreates(context.Background(), s, client, testConfig(), nil, nil)
	require.NoError(t, err)

	linkMember(t, s, nid, selfAuthored("dd:2"))

	pass2, _, err := ProposeCreates(context.Background(), s, client, testConfig(), nil, nil)
	require.NoError(t, err)
	require.Equal(t, []int64{nid}, resultIDs(pass2), "precondition: the new link re-admits it")
	assert.Contains(t, pass2[0].Suppressed[0], "nothing new has happened since")

	pass3, _, err := ProposeCreates(context.Background(), s, client, testConfig(), nil, nil)
	require.NoError(t, err)
	assert.Empty(t, pass3, "the self-authored delta was recorded, so the slot is yielded")
	assert.Len(t, client.prompts, 1)
}

// TestProposeCreates_AnEmptiedNarrativeYieldsItsSlot is the create-path half of finding
// F50, which the self-authored watermark reaches without a status clause: a narrative
// whose work moved away (marked split) holds no member events, proposeCreateOne finds
// nothing that is not unjira's own, and the examination it now records excludes it.
// Probed before this fix with the same shape: the split narrative held the one slot for
// 3 of 3 passes and the real narrative behind it was never examined.
func TestProposeCreates_AnEmptiedNarrativeYieldsItsSlot(t *testing.T) {
	s := reconcileStore(t)
	emptied := seedUntracked(t, s)
	require.NoError(t, s.SetNarrativeStatus(emptied, store.StatusSplit))
	realWork := seedUntracked(t, s, codeEvent("sp:1", "built the export command"))

	client := &fakeLLM{responses: []string{worthTracking}}

	pass1, _, err := ProposeCreates(context.Background(), s, client, capOf(1), nil, nil)
	require.NoError(t, err)
	require.Equal(t, []int64{emptied}, resultIDs(pass1), "precondition: the older, emptied one is first")

	pass2, _, err := ProposeCreates(context.Background(), s, client, capOf(1), nil, nil)
	require.NoError(t, err)
	require.Equal(t, []int64{realWork}, resultIDs(pass2),
		"an emptied narrative is examined once, then yields its slot to real work")
	assert.Len(t, pass2[0].Proposed, 1)
}

// TestProposeCreateOne_BackstopsRemainInPlace: the selector now excludes every
// narrative these checks would skip, so in production they are unreachable — and they
// stay, because the cost of a selector bug here is a duplicate ticket. Driven through
// proposeCreateOne directly, since ProposeCreates no longer hands it such a narrative.
func TestProposeCreateOne_BackstopsRemainInPlace(t *testing.T) {
	cases := []struct {
		name   string
		status string
		reason string
	}{
		{"applied create", store.StatusApplied, "would open a duplicate"},
		{"create awaiting review", store.StatusProposed, "awaiting review"},
		{"approved create", store.StatusApproved, "awaiting review"},
		{"decline with nothing new", store.StatusDeclined, "nothing new has happened since"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := reconcileStore(t)
			nid := seedUntracked(t, s, codeEvent("bs:1", "work"))

			inserted := tc.status
			if inserted != store.StatusDeclined {
				inserted = store.StatusProposed
			}
			id, err := s.InsertAction(store.ActionRow{
				NarrativeID: nid, Type: store.ActionTypeCreate,
				Payload: `{"summary":"s","description":"d"}`, Status: inserted,
			})
			require.NoError(t, err)
			if tc.status != inserted {
				require.NoError(t, s.UpdateActionStatus(id, tc.status))
			}

			awaiting, err := s.NarrativesAwaitingCreate(10)
			require.NoError(t, err)
			require.Empty(t, awaiting, "precondition: the selector already excludes it")

			client := &fakeLLM{responses: []string{worthTracking}}
			got, _, err := proposeCreateOne(context.Background(), s, client, store.NarrativeRow{ID: nid}, nil, nil)

			require.NoError(t, err)
			assert.Empty(t, got.Proposed)
			require.Len(t, got.Suppressed, 1)
			assert.Contains(t, got.Suppressed[0], tc.reason)
			assert.Empty(t, client.prompts, "a backstop skip costs no model call")
		})
	}
}

func resultIDs(results []ReconcileResult) []int64 {
	ids := make([]int64, 0, len(results))
	for _, r := range results {
		ids = append(ids, r.NarrativeID)
	}

	return ids
}
