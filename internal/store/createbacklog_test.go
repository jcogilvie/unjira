package store_test

// createbacklog_test.go covers the create path's selector and its count: the F12/F26
// livelock shape a fourth time, on the reconciler's create half.
//
// reconciler.ProposeCreates used to select the OLDEST narratives with no issue link
// and only then, in Go, skip the ones it had already handled — an open or applied
// create, a decline with nothing new since, or a narrative made only of unjira's own
// output. Those kept the first reconciler.max_narratives_per_pass slots on every pass,
// so nothing behind them was reached. Measured on a real 30-day store: 129 untracked
// narratives, a cap of 20; pass 1 proposed 11 and declined 9, every later pass proposed
// nothing, and the other 109 were never examined.
//
// The store half of the fix: NarrativesAwaitingCreate excludes, in SQL, everything the
// Go path would skip without a model call, and CountNarrativesAwaitingCreate counts the
// same population through the same predicate. internal/reconciler/create_test.go covers
// the caller wiring and the end-to-end starvation reproduction.

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/store"
)

var createBacklogBase = time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)

// createBacklogFloor is the match.confidence_floor these tests select under: the
// example config's value, so keepTracked's 0.9 primary is confident.
const createBacklogFloor = 0.7

// seedUntrackedNarrative inserts a narrative with one member event and no issue link —
// the shape the create path selects. offset orders it by window_start.
func seedUntrackedNarrative(t *testing.T, s *store.Store, extID string, offset time.Duration) int64 {
	t.Helper()

	at := createBacklogBase.Add(offset)
	id, err := s.InsertNarrative(at, at.Add(time.Hour), "untracked "+extID, "summary")
	require.NoError(t, err)

	linkNewMember(t, s, id, extID, at.Add(time.Minute))

	return id
}

// linkNewMember inserts a fresh event and links it to narrativeID as a member.
func linkNewMember(t *testing.T, s *store.Store, narrativeID int64, extID string, at time.Time) {
	t.Helper()

	_, err := s.InsertEvent(events.NewEvent("claude_code", extID, at, "work "+extID))
	require.NoError(t, err)
	eid, err := s.EventIDByExternalID("claude_code", extID)
	require.NoError(t, err)
	require.NoError(t, s.LinkMembers(narrativeID, []int64{eid}, 1))
}

// linkNewContext inserts a fresh event as a member of holder and gives narrativeID a
// context link to it — a link that is newer than any watermark but is not this
// narrative's work.
func linkNewContext(t *testing.T, s *store.Store, narrativeID, holder int64, extID string, at time.Time) {
	t.Helper()

	linkNewMember(t, s, holder, extID, at)
	eid, err := s.EventIDByExternalID("claude_code", extID)
	require.NoError(t, err)
	require.NoError(t, s.LinkContext(narrativeID, []int64{eid}))
}

// keepTracked takes a narrative out of the create backlog for good by linking it to an
// issue: tracked work is never a create candidate, whatever happens to it later.
func keepTracked(t *testing.T, s *store.Store, narrativeID int64) {
	t.Helper()

	require.NoError(t, s.AddNarrativeIssues(narrativeID, []store.NarrativeIssue{
		{IssueKey: "DEVSBX-9", Role: store.RolePrimary, Confidence: 0.9, Provenance: "branch"},
	}))
}

// insertCreateAction records a create action that reached status the way production
// does: a decline is inserted as one (reconciler.recordDecline), every other status is a
// proposal that a ruling or an application later moved.
func insertCreateAction(t *testing.T, s *store.Store, narrativeID int64, status string) {
	t.Helper()

	inserted := store.StatusProposed
	if status == store.StatusDeclined {
		inserted = store.StatusDeclined
	}

	id, err := s.InsertAction(store.ActionRow{
		NarrativeID: narrativeID, Type: store.ActionTypeCreate,
		Payload: `{"summary":"s","description":"d"}`, Status: inserted,
	})
	require.NoError(t, err)

	if status != inserted {
		require.NoError(t, s.UpdateActionStatus(id, status))
	}
}

func awaitingIDs(t *testing.T, s *store.Store) []int64 {
	t.Helper()

	got, err := s.NarrativesAwaitingCreate(100, createBacklogFloor)
	require.NoError(t, err)

	ids := make([]int64, 0, len(got))
	for _, n := range got {
		ids = append(ids, n.ID)
	}

	return ids
}

// TestNarrativesAwaitingCreate_CreateStatus pins which create statuses take a
// narrative out of the backlog. A live proposal (proposed, approved) or a successful
// write (applied) does: proposing again would double the review queue or open a
// duplicate ticket. A rejected or failed create does NOT — reconciler.openOrAppliedCreate
// treats neither as a blocker, and the selector must exclude exactly what the pass
// would skip, no more.
func TestNarrativesAwaitingCreate_CreateStatus(t *testing.T) {
	cases := []struct {
		status   string
		awaiting bool
	}{
		{store.StatusProposed, false},
		{store.StatusApproved, false},
		{store.StatusApplied, false},
		{store.StatusRejected, true},
		{store.StatusFailed, true},
	}

	for _, tc := range cases {
		t.Run(tc.status, func(t *testing.T) {
			s := openStore(t)
			id := seedUntrackedNarrative(t, s, "cs:"+tc.status, 0)
			insertCreateAction(t, s, id, tc.status)

			got := awaitingIDs(t, s)
			if tc.awaiting {
				assert.Equal(t, []int64{id}, got, "a %s create must leave the narrative awaiting", tc.status)
			} else {
				assert.Empty(t, got, "a %s create must take the narrative out of the backlog", tc.status)
			}

			count, err := s.CountNarrativesAwaitingCreate(createBacklogFloor)
			require.NoError(t, err)
			assert.Len(t, got, count, "the count and the selector share one predicate")
		})
	}
}

// TestNarrativesAwaitingCreate_ANonCreateActionDoesNotCount: the exclusion is keyed on
// CREATE actions. A comment drafted while the narrative was briefly linked says nothing
// about whether it deserves a ticket.
func TestNarrativesAwaitingCreate_ANonCreateActionDoesNotCount(t *testing.T) {
	s := openStore(t)
	id := seedUntrackedNarrative(t, s, "nc:1", 0)

	_, err := s.InsertAction(store.ActionRow{
		NarrativeID: id, Type: "comment", IssueKey: "DEVSBX-1",
		Payload: `{"body":"b"}`, Status: store.StatusProposed,
	})
	require.NoError(t, err)

	assert.Equal(t, []int64{id}, awaitingIDs(t, s))
}

// TestNarrativesAwaitingCreate_ADeclineIsAWatermark: a declined create excludes the
// narrative until a MEMBER event is linked after the narrative's latest action, which
// is when proposeCreateOne would ask again. A context link does not re-admit it,
// because DeltaEvents never returns one and the pass would skip it.
func TestNarrativesAwaitingCreate_ADeclineIsAWatermark(t *testing.T) {
	s := openStore(t)
	id := seedUntrackedNarrative(t, s, "dw:1", 0)
	other := seedUntrackedNarrative(t, s, "dw:other", time.Hour)
	keepTracked(t, s, other) // keep `other` out of the way

	insertCreateAction(t, s, id, store.StatusDeclined)
	assert.Empty(t, awaitingIDs(t, s), "declined, nothing new since: excluded")

	linkNewContext(t, s, id, other, "dw:ctx", createBacklogBase.Add(2*time.Hour))
	assert.Empty(t, awaitingIDs(t, s),
		"a context link is another narrative's work; the pass would skip it, so the selector must")

	linkNewMember(t, s, id, "dw:2", createBacklogBase.Add(3*time.Hour))
	assert.Equal(t, []int64{id}, awaitingIDs(t, s),
		"new member work re-admits a declined narrative: a decline is not a permanent veto")
}

// TestNarrativesAwaitingCreate_ACreateExaminationIsAWatermark: a narrative examined and
// found to hold only unjira's own output is excluded until a member event is linked
// after the examination.
func TestNarrativesAwaitingCreate_ACreateExaminationIsAWatermark(t *testing.T) {
	s := openStore(t)
	id := seedUntrackedNarrative(t, s, "ce:1", 0)
	other := seedUntrackedNarrative(t, s, "ce:other", time.Hour)
	keepTracked(t, s, other)

	require.NoError(t, s.RecordCreateExamined(id, "every member event is authored_by_unjira"))
	assert.Empty(t, awaitingIDs(t, s), "examined and entirely self-authored: excluded")

	linkNewContext(t, s, id, other, "ce:ctx", createBacklogBase.Add(2*time.Hour))
	assert.Empty(t, awaitingIDs(t, s), "a context link must not re-admit it")

	linkNewMember(t, s, id, "ce:2", createBacklogBase.Add(3*time.Hour))
	assert.Equal(t, []int64{id}, awaitingIDs(t, s),
		"a member link past the watermark re-admits it: that event may be real work")
}

// TestNarrativesAwaitingCreate_DeclineThenSelfAuthoredDelta covers the two watermarks
// together, the shape proposeCreateOne reaches when a declined narrative's only new
// events are unjira's own: the decline no longer excludes it (there IS a member link
// past it), so the pass records a create examination, which must.
func TestNarrativesAwaitingCreate_DeclineThenSelfAuthoredDelta(t *testing.T) {
	s := openStore(t)
	id := seedUntrackedNarrative(t, s, "ds:1", 0)
	insertCreateAction(t, s, id, store.StatusDeclined)

	linkNewMember(t, s, id, "ds:2", createBacklogBase.Add(time.Hour))
	require.Equal(t, []int64{id}, awaitingIDs(t, s), "precondition: the new link re-admits it")

	require.NoError(t, s.RecordCreateExamined(id, "every delta event is authored_by_unjira"))
	assert.Empty(t, awaitingIDs(t, s))
}

// TestCreateAndReconcileExaminationsAreSeparate pins why the create path has its own
// watermark table instead of reusing reconcile_examinations.
//
// The two record different facts. A reconcile examination says the narrative's DELTA
// (since its latest action) was all unjira's own; earlier member events may be real
// work. A create examination says EVERY member event was. Sharing a row would let a
// linked narrative that later lost its links be skipped by the create path on the
// strength of a delta-only judgment — real work never offered as a ticket.
func TestCreateAndReconcileExaminationsAreSeparate(t *testing.T) {
	s := openStore(t)
	id := seedUntrackedNarrative(t, s, "sep:1", 0)

	require.NoError(t, s.RecordReconcileExamined(id, "every delta event is authored_by_unjira"))
	assert.Equal(t, []int64{id}, awaitingIDs(t, s),
		"a reconcile examination must not exclude a narrative from the create backlog")

	linked := seedActionableLinked(t, s, "DEVSBX-7", "sep:2", createBacklogBase.Add(time.Hour))
	require.NoError(t, s.RecordCreateExamined(linked, "every member event is authored_by_unjira"))
	got, err := s.NarrativesWithActionableLinks(10, reconcileRoles)
	require.NoError(t, err)
	require.Len(t, got, 1, "a create examination must not exclude a narrative from the reconcile backlog")
	assert.Equal(t, linked, got[0].ID)
}

// TestRecordCreateExamined_OneRowPerNarrative keeps re-examination from accumulating
// rows without bound, as RecordReconcileExamined does for its own table.
func TestRecordCreateExamined_OneRowPerNarrative(t *testing.T) {
	s := openStore(t)
	id := seedUntrackedNarrative(t, s, "one:1", 0)

	require.NoError(t, s.RecordCreateExamined(id, "first"))
	linkNewMember(t, s, id, "one:2", createBacklogBase.Add(time.Hour))
	require.NoError(t, s.RecordCreateExamined(id, "second"))

	n, err := s.QueryStringForTest(`SELECT COUNT(*) FROM create_examinations WHERE narrative_id = ?`, id)
	require.NoError(t, err)
	assert.Equal(t, "1", n)
	assert.Empty(t, awaitingIDs(t, s), "the upsert moved the watermark past the second link")
}

// TestCountNarrativesAwaitingCreate_MatchesTheSelector: one fixture holding every class
// the predicate distinguishes, asserted through both accessors. A count describing a
// different population than the pass examines is F10's failure mode — it would report a
// backlog the pass cannot reach, or hide one it never will.
func TestCountNarrativesAwaitingCreate_MatchesTheSelector(t *testing.T) {
	s := openStore(t)

	want := make([]int64, 0, 3)
	step := 0
	next := func(label string) int64 {
		step++

		return seedUntrackedNarrative(t, s, fmt.Sprintf("mix:%s", label), time.Duration(step)*time.Hour)
	}

	want = append(want, next("fresh"))
	insertCreateAction(t, s, next("proposed"), store.StatusProposed)
	insertCreateAction(t, s, next("applied"), store.StatusApplied)

	rejected := next("rejected")
	insertCreateAction(t, s, rejected, store.StatusRejected)
	want = append(want, rejected)

	insertCreateAction(t, s, next("declined"), store.StatusDeclined)

	readmitted := next("declined-then-new")
	insertCreateAction(t, s, readmitted, store.StatusDeclined)
	linkNewMember(t, s, readmitted, "mix:readmitted-new", createBacklogBase.Add(100*time.Hour))
	want = append(want, readmitted)

	require.NoError(t, s.RecordCreateExamined(next("self-authored"), "all unjira"))

	keepTracked(t, s, next("confidently linked"))

	citesOnly := next("mentioned only")
	require.NoError(t, s.AddNarrativeIssues(citesOnly, []store.NarrativeIssue{
		{IssueKey: "DEVSBX-8", Role: store.Role("mentioned"), Provenance: "test", Confidence: 0.2},
	}))
	want = append(want, citesOnly)

	doubted := next("sub-floor primary")
	require.NoError(t, s.AddNarrativeIssues(doubted, []store.NarrativeIssue{
		{IssueKey: "DEVSBX-7", Role: store.RolePrimary, Provenance: "test", Confidence: 0.3},
	}))
	want = append(want, doubted)

	assert.Equal(t, want, awaitingIDs(t, s), "selected in (window_start, id) order")

	count, err := s.CountNarrativesAwaitingCreate(createBacklogFloor)
	require.NoError(t, err)
	assert.Equal(t, len(want), count, "the count must describe exactly the selector's population")
}

// TestNarrativesAwaitingCreate_AnOpenCreateIsAWatermark: a live or applied create
// excludes the narrative only until new member work arrives. Destinations are decided
// per narrative, and a create open in one scope does not settle another, so new work
// is when the pass must look again (it then blocks the taken scope per destination).
// Without new work the create path's decision stands, including the model's choice of
// which destinations to propose in: re-offering the rest on every pass would override
// that choice the moment only one destination remained.
func TestNarrativesAwaitingCreate_AnOpenCreateIsAWatermark(t *testing.T) {
	for _, status := range []string{store.StatusProposed, store.StatusApproved, store.StatusApplied} {
		t.Run(status, func(t *testing.T) {
			s := openStore(t)
			id := seedUntrackedNarrative(t, s, "ow:"+status, 0)
			insertCreateAction(t, s, id, status)
			assert.Empty(t, awaitingIDs(t, s), "nothing new since the create: excluded")

			linkNewMember(t, s, id, "ow:new:"+status, createBacklogBase.Add(3*time.Hour))
			assert.Equal(t, []int64{id}, awaitingIDs(t, s), "new member work re-admits it")
		})
	}
}

// TestNarrativesAwaitingCreate_ASuppressionIsAWatermark: an untracked narrative with no
// allowed destination is recorded as a suppression, not a create action. It must leave
// the backlog until something new happens, or every narrative whose work went upstream
// would hold a slot on every pass — the starvation this predicate exists to end, through
// a door destinations opened.
func TestNarrativesAwaitingCreate_ASuppressionIsAWatermark(t *testing.T) {
	s := openStore(t)
	id := seedUntrackedNarrative(t, s, "sw:1", 0)
	_, err := s.InsertAction(store.ActionRow{
		NarrativeID: id, Type: "comment", Payload: "{}", Status: store.StatusSuppressed,
		Rationale: "no allowed destination: the work happened in tracker \"upstream\"'s scope",
	})
	require.NoError(t, err)
	assert.Empty(t, awaitingIDs(t, s), "suppressed, nothing new since: excluded")

	linkNewMember(t, s, id, "sw:2", createBacklogBase.Add(3*time.Hour))
	assert.Equal(t, []int64{id}, awaitingIDs(t, s), "new member work re-admits it")
}
