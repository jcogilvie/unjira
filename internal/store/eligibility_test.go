package store_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/store"
)

// seedNarrativeWithEvents creates a narrative, inserts n events, links them,
// and returns the narrative id plus the linked event ids in order.
func seedNarrativeWithEvents(t *testing.T, s *store.Store, count int) (int64, []int64) {
	t.Helper()

	base := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)

	nid, err := s.InsertNarrative(base, base.Add(time.Hour), "t", "s")
	require.NoError(t, err)

	ids := make([]int64, 0, count)
	for i := range count {
		e := events.NewEvent("claude_code", fmt.Sprintf("elig:%d", i), base.Add(time.Duration(i)*time.Minute), "work")
		_, err := s.InsertEvent(e)
		require.NoError(t, err)

		eid, err := s.EventIDByExternalID("claude_code", fmt.Sprintf("elig:%d", i))
		require.NoError(t, err)

		require.NoError(t, s.AddNarrativeEvents(nid, []int64{eid}))
		ids = append(ids, eid)
	}

	return nid, ids
}

// TestEligibleEventIDs_NeverCommittedIsFullyEligible: with no applied action,
// there is no watermark, so nothing is frozen.
func TestEligibleEventIDs_NeverCommittedIsFullyEligible(t *testing.T) {
	s := openStore(t)
	nid, ids := seedNarrativeWithEvents(t, s, 3)

	got, err := s.EligibleEventIDs(nid)

	require.NoError(t, err)
	assert.ElementsMatch(t, ids, got, "with no committed action every link is eligible")
}

// TestEligibleEventIDs_FullyCommittedIsFullyFrozen: an applied action stamped
// AFTER every link freezes all of them.
func TestEligibleEventIDs_FullyCommittedIsFullyFrozen(t *testing.T) {
	s := openStore(t)
	nid, _ := seedNarrativeWithEvents(t, s, 3)

	id, err := s.InsertAction(store.ActionRow{
		NarrativeID: nid, Type: "comment", IssueKey: "PROJ-1",
		Payload: `{"body":"posted"}`, Status: "proposed",
	})
	require.NoError(t, err)
	// applied stamps executed_at = now(), which is after every linked_at above.
	require.NoError(t, s.UpdateActionStatus(id, "applied"))

	got, err := s.EligibleEventIDs(nid)

	require.NoError(t, err)
	assert.Empty(t, got, "every link predates the commit, so none may move")
}

// TestEligibleEventIDs_MixedStateFreezesOnlyThePast is the case that drove the
// design. A narrative can hold an applied AND a proposed action: watch applies
// A, the next tick proposes B for the same narrative. Freezing the whole
// narrative would refuse a merge — but the reviewer's objection is precisely
// that the events behind B do not belong with the events behind A. So the unit
// of freezing is the event link, not the narrative.
func TestEligibleEventIDs_MixedStateFreezesOnlyThePast(t *testing.T) {
	s := openStore(t)
	nid, before := seedNarrativeWithEvents(t, s, 2)

	id, err := s.InsertAction(store.ActionRow{
		NarrativeID: nid, Type: "comment", IssueKey: "PROJ-1",
		Payload: `{"body":"posted"}`, Status: "proposed",
	})
	require.NoError(t, err)
	require.NoError(t, s.UpdateActionStatus(id, "applied"))

	// linked_at uses millisecond precision, so sleep past the commit instant
	// rather than racing it — two links inside the same millisecond would be
	// indistinguishable and make this test flaky rather than wrong.
	time.Sleep(5 * time.Millisecond)

	after := events.NewEvent("claude_code", "elig:after", time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC), "later work")
	_, err = s.InsertEvent(after)
	require.NoError(t, err)
	afterID, err := s.EventIDByExternalID("claude_code", "elig:after")
	require.NoError(t, err)
	require.NoError(t, s.AddNarrativeEvents(nid, []int64{afterID}))

	got, err := s.EligibleEventIDs(nid)

	require.NoError(t, err)
	assert.Equal(t, []int64{afterID}, got,
		"only the link made after the commit may move; the committed ones stay")
	assert.NotContains(t, got, before[0], "a committed event must never be eligible")
}

// TestEligibleEventIDs_AFailedWriteDoesNotFreeze: executed_at is stamped for
// both applied AND failed, because a failed attempt still attempted a write.
// But a failed write changed nothing in the tracker, so it must not freeze
// events — otherwise a narrative whose only write failed would be
// unrestructurable forever, for a reason no human could act on.
func TestEligibleEventIDs_AFailedWriteDoesNotFreeze(t *testing.T) {
	s := openStore(t)
	nid, ids := seedNarrativeWithEvents(t, s, 2)

	id, err := s.InsertAction(store.ActionRow{
		NarrativeID: nid, Type: "comment", IssueKey: "PROJ-1",
		Payload: `{"body":"never landed"}`, Status: "proposed",
	})
	require.NoError(t, err)
	require.NoError(t, s.UpdateActionStatus(id, "failed"))

	got, err := s.EligibleEventIDs(nid)

	require.NoError(t, err)
	assert.ElementsMatch(t, ids, got,
		"a failed write mutated nothing, so it must not freeze the narrative's events")
}

func TestUnlinkNarrativeEvents_RemovesOnlyTheNamedLinks(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)

	a, err := s.InsertNarrative(base, base.Add(time.Hour), "A", "s")
	require.NoError(t, err)

	ids := make([]int64, 0, 3)
	for i, ext := range []string{"u:1", "u:2", "u:3"} {
		e := events.NewEvent("claude_code", ext, base.Add(time.Duration(i)*time.Minute), "w")
		_, err := s.InsertEvent(e)
		require.NoError(t, err)
		eid, err := s.EventIDByExternalID("claude_code", ext)
		require.NoError(t, err)
		require.NoError(t, s.AddNarrativeEvents(a, []int64{eid}))
		ids = append(ids, eid)
	}

	require.NoError(t, s.UnlinkNarrativeEvents(a, []int64{ids[0], ids[2]}))

	n, err := s.NarrativeEventCount(a)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "only the un-named link survives")
}

func TestUnlinkNarrativeEvents_MissingLinkErrors(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)
	a, err := s.InsertNarrative(base, base.Add(time.Hour), "A", "s")
	require.NoError(t, err)

	err = s.UnlinkNarrativeEvents(a, []int64{999999})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no such link")
}

// TestEligibleEvents_IsTheRedraftDeltaThatDeltaEventsCannotBe is the reason
// EligibleEvents exists at all. DeltaEvents is bounded by
// max(actions.created_at), so the moment ANY action exists it returns nothing —
// correct for "should we propose again", and exactly wrong for "redraft the
// action that already exists", because the action being edited is itself what
// suppresses its own source events.
//
// This test asserts BOTH accessors on the same fixture, so the contrast is the
// assertion rather than a comment claiming it.
func TestEligibleEvents_IsTheRedraftDeltaThatDeltaEventsCannotBe(t *testing.T) {
	s := openStore(t)
	nid, ids := seedNarrativeWithEvents(t, s, 2)

	// A proposed action — the one a reviewer is about to edit.
	_, err := s.InsertAction(store.ActionRow{
		NarrativeID: nid, Type: "comment", IssueKey: "PROJ-1",
		Payload: `{"body":"first draft"}`, Status: "proposed",
	})
	require.NoError(t, err)

	delta, err := s.DeltaEvents(nid)
	require.NoError(t, err)
	assert.Empty(t, delta,
		"DeltaEvents is bounded by max(created_at), so the action being edited hides its own events")

	eligible, err := s.EligibleEvents(nid)
	require.NoError(t, err)
	assert.Len(t, eligible, len(ids),
		"EligibleEvents is bounded by the COMMIT watermark, so a proposed action hides nothing")
}

// TestEligibleEvents_ExcludesWhatACommitAlreadyDescribed: the redraft delta
// obeys the same watermark every restructure does, so an edit and a merge agree
// about which events are still in play.
func TestEligibleEvents_ExcludesWhatACommitAlreadyDescribed(t *testing.T) {
	s := openStore(t)
	nid, _ := seedNarrativeWithEvents(t, s, 2)

	id, err := s.InsertAction(store.ActionRow{
		NarrativeID: nid, Type: "comment", IssueKey: "PROJ-1",
		Payload: `{"body":"posted"}`, Status: "proposed",
	})
	require.NoError(t, err)
	require.NoError(t, s.UpdateActionStatus(id, "applied"))

	time.Sleep(5 * time.Millisecond)

	later := events.NewEvent("claude_code", "elig:later", time.Date(2026, 8, 28, 11, 0, 0, 0, time.UTC), "more work")
	_, err = s.InsertEvent(later)
	require.NoError(t, err)
	laterID, err := s.EventIDByExternalID("claude_code", "elig:later")
	require.NoError(t, err)
	require.NoError(t, s.AddNarrativeEvents(nid, []int64{laterID}))

	got, err := s.EligibleEvents(nid)

	require.NoError(t, err)
	require.Len(t, got, 1, "only the post-commit event is redraftable")
	assert.Equal(t, "elig:later", got[0].ExternalID,
		"a redraft must not re-describe work an applied comment already covered")
}

// TestEligibleEvents_AgreesWithEligibleEventIDs: the two accessors run the same
// watermark predicate over the same rows. They are separate SQL statements, so a
// change to one that missed the other would silently let an edit and a merge
// disagree about eligibility.
func TestEligibleEvents_AgreesWithEligibleEventIDs(t *testing.T) {
	s := openStore(t)
	nid, _ := seedNarrativeWithEvents(t, s, 3)

	id, err := s.InsertAction(store.ActionRow{
		NarrativeID: nid, Type: "comment", IssueKey: "PROJ-1",
		Payload: `{"body":"posted"}`, Status: "proposed",
	})
	require.NoError(t, err)
	require.NoError(t, s.UpdateActionStatus(id, "applied"))

	time.Sleep(5 * time.Millisecond)
	for i, ext := range []string{"agree:1", "agree:2"} {
		e := events.NewEvent("claude_code", ext, time.Date(2026, 8, 28, 12, i, 0, 0, time.UTC), "w")
		_, err := s.InsertEvent(e)
		require.NoError(t, err)
		eid, err := s.EventIDByExternalID("claude_code", ext)
		require.NoError(t, err)
		require.NoError(t, s.AddNarrativeEvents(nid, []int64{eid}))
	}

	byID, err := s.EligibleEventIDs(nid)
	require.NoError(t, err)
	hydrated, err := s.EligibleEvents(nid)
	require.NoError(t, err)

	assert.Len(t, hydrated, len(byID),
		"EligibleEvents and EligibleEventIDs must select the same links")
}
