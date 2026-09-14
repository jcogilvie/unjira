package store_test

// reconcilebacklog_test.go covers finding F12: the reconcile remainder counted
// every narrative the reconciler SELECTS rather than every narrative it will act
// on.
//
// reconcileOne computes DeltaEvents and returns early with SkippedNoDelta when it
// is empty — the steady state for a narrative nothing new has happened to. Those
// cost no model call and cannot produce an action, but they were counted, so the
// operator-facing remainder overstated the work. Measured on the live store: 55
// selected, 20 with an empty delta, 35 with a real one.
//
// The predicate here MUST match DeltaEvents' bound exactly, and that is the whole
// risk this file exists to cover. Two queries answering "is there a delta" that
// drift apart produce a count describing a different population than the pass
// examined — the same defect F10 warned about for the matching count, and the
// reason these tests assert the count against an actual delta rather than against
// a hard-coded number.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/store"
)

// reconcileRoles mirrors reconciler.SelectionRoles. Spelled out rather than
// imported because internal/store must not depend on internal/reconciler.
var reconcileRoles = []store.Role{"primary", "same_work", "mentioned"}

// seedLinkedWithEvent creates a narrative carrying a primary link and one linked
// event — the shape the reconciler acts on.
func seedLinkedWithEvent(t *testing.T, s *store.Store, key, extID string) int64 {
	t.Helper()

	base := time.Date(2026, 9, 14, 9, 0, 0, 0, time.UTC)

	id, err := s.InsertNarrative(base, base.Add(time.Hour), "work on "+key, "summary")
	require.NoError(t, err)
	require.NoError(t, s.AddNarrativeIssues(id, []store.NarrativeIssue{
		{IssueKey: key, Role: store.RolePrimary, Provenance: "test", Confidence: 0.9},
	}))

	_, err = s.InsertEvent(
		events.NewEvent("claude_code", extID, base.Add(30*time.Minute), "did the work"))
	require.NoError(t, err)

	eventID, err := s.EventIDByExternalID("claude_code", extID)
	require.NoError(t, err)
	require.NoError(t, s.AddNarrativeEvents(id, []int64{eventID}))

	return id
}

// TestCountNarrativesWithDelta_ExcludesANarrativeWithNothingNew is the finding. A
// narrative whose events all predate its newest action is what reconcileOne skips,
// so counting it tells the operator to re-run for work that does not exist.
func TestCountNarrativesWithDelta_ExcludesANarrativeWithNothingNew(t *testing.T) {
	s := openStore(t)

	stale := seedLinkedWithEvent(t, s, "DEVSBX-1", "evt-stale")

	// An action of ANY status establishes the watermark — DeltaEvents bounds on
	// MAX(created_at) over actions regardless of status, deliberately, since a
	// declined proposal still means the narrative was examined. Inserted after the
	// event so the event falls behind it.
	_, err := s.InsertAction(store.ActionRow{
		NarrativeID: stale,
		Type:        "comment",
		IssueKey:    "DEVSBX-1",
		Payload:     `{"body":"already considered"}`,
		Status:      store.StatusDeclined,
	})
	require.NoError(t, err)

	delta, err := s.DeltaEvents(stale)
	require.NoError(t, err)
	require.Empty(t, delta, "precondition: reconcileOne would skip this narrative")

	counted, err := s.CountNarrativesWithDelta(reconcileRoles)
	require.NoError(t, err)

	assert.Zero(t, counted,
		"a narrative the pass would skip must not appear in the remainder; counting it tells the "+
			"operator to re-run for work that does not exist, and each re-run bills for the sweep")
}

// TestCountNarrativesWithDelta_CountsANarrativeWithNewWork is the other half: the
// fix must not zero out the remainder. Unexamined work is the whole point of it.
func TestCountNarrativesWithDelta_CountsANarrativeWithNewWork(t *testing.T) {
	s := openStore(t)

	fresh := seedLinkedWithEvent(t, s, "DEVSBX-2", "evt-fresh")

	delta, err := s.DeltaEvents(fresh)
	require.NoError(t, err)
	require.NotEmpty(t, delta, "precondition: a narrative with no action has no watermark")

	counted, err := s.CountNarrativesWithDelta(reconcileRoles)
	require.NoError(t, err)

	assert.Equal(t, 1, counted, "a narrative with a real delta is work the operator should know about")
}

// TestCountNarrativesWithDelta_CountsANarrativeWithEventsNewerThanItsAction is the
// case that rules out the cheaper predicate "narratives with no action yet". A
// narrative can carry an old action AND newer work — that is the normal state for a
// ticket that moved again after being reconciled — and it must still be counted.
func TestCountNarrativesWithDelta_CountsANarrativeWithEventsNewerThanItsAction(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 9, 14, 9, 0, 0, 0, time.UTC)

	id := seedLinkedWithEvent(t, s, "DEVSBX-3", "evt-old")

	_, err := s.InsertAction(store.ActionRow{
		NarrativeID: id,
		Type:        "comment",
		IssueKey:    "DEVSBX-3",
		Payload:     `{"body":"considered once"}`,
		Status:      store.StatusDeclined,
	})
	require.NoError(t, err)

	// New work arrives AFTER that action, so linked_at lands past the watermark.
	//
	// The sleep is load-bearing, not padding. Both timestamps default to the wall
	// clock at MILLISECOND precision (%f — see narrative_events.linked_at's schema
	// comment for why not whole seconds), and DeltaEvents compares them lexically as
	// TEXT with a strict `>`. Inserting both inside the same millisecond makes the
	// new event read as NOT newer, which flaked this test roughly one run in four
	// before the sleep was added. Waiting is the honest fix: the invariant under
	// test is "an event linked after the action counts", so the fixture has to
	// actually satisfy it rather than assume scheduling will.
	time.Sleep(2 * time.Millisecond)

	_, err = s.InsertEvent(
		events.NewEvent("claude_code", "evt-new", base.Add(2*time.Hour), "more work"))
	require.NoError(t, err)

	newerID, err := s.EventIDByExternalID("claude_code", "evt-new")
	require.NoError(t, err)
	require.NoError(t, s.AddNarrativeEvents(id, []int64{newerID}))

	counted, err := s.CountNarrativesWithDelta(reconcileRoles)
	require.NoError(t, err)

	assert.Equal(t, 1, counted,
		"having an action is not being done: a narrative with newer events than its last action has "+
			"work to do, which is why this cannot be counted as 'narratives with no action yet'")
}

// TestCountNarrativesWithDelta_IgnoresAnUnlinkedNarrative keeps the role filter
// honest. A narrative with no actionable link is matching's business, not the
// reconciler's, however much unexamined evidence it carries.
func TestCountNarrativesWithDelta_IgnoresAnUnlinkedNarrative(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 9, 14, 9, 0, 0, 0, time.UTC)

	id, err := s.InsertNarrative(base, base.Add(time.Hour), "untracked work", "summary")
	require.NoError(t, err)

	_, err = s.InsertEvent(
		events.NewEvent("claude_code", "evt-unlinked", base.Add(time.Minute), "work"))
	require.NoError(t, err)

	eventID, err := s.EventIDByExternalID("claude_code", "evt-unlinked")
	require.NoError(t, err)
	require.NoError(t, s.AddNarrativeEvents(id, []int64{eventID}))

	counted, err := s.CountNarrativesWithDelta(reconcileRoles)
	require.NoError(t, err)

	assert.Zero(t, counted, "no actionable link means the reconciler never selects it")
}

// TestCountNarrativesWithDelta_NoRolesCountsNothing mirrors the guard its sibling
// selectors carry: an empty role set is a caller asking about an empty population,
// which is a question with the answer 0, not a malformed query.
func TestCountNarrativesWithDelta_NoRolesCountsNothing(t *testing.T) {
	s := openStore(t)
	seedLinkedWithEvent(t, s, "DEVSBX-4", "evt-any")

	counted, err := s.CountNarrativesWithDelta(nil)
	require.NoError(t, err)
	assert.Zero(t, counted)
}

// TestNarrativesWithActionableLinks_SkipsNarrativesWithNoDelta is the residual half
// of F12: the selector spent its cap on rows the pass would skip.
//
// The cap is documented as a SPEND bound — config.DefaultMaxNarrativesPerPass says
// "each narrative costs at least one GetIssue per link plus one LLM drafting call,
// so an unbounded pass is unbounded spend". Once suppression started advancing the
// watermark, that stopped being true: measured on the live store, a cap of 20
// selected 20 rows of which 19 were free skips and exactly 1 cost a model call. The
// cap had silently become a row limit, which is the confusion
// MaxNarrativesPerPass was split out of MaxCandidatesPerNarrative to prevent.
//
// Applying the delta test in the selector restores the documented meaning: every
// row it returns is a row that will cost something.
func TestNarrativesWithActionableLinks_SkipsNarrativesWithNoDelta(t *testing.T) {
	s := openStore(t)

	// Two linked narratives. The first is already examined (an action row puts its
	// event behind the watermark), the second is not.
	examined := seedLinkedWithEvent(t, s, "DEVSBX-1", "evt-examined")
	_, err := s.InsertAction(store.ActionRow{
		NarrativeID: examined,
		Type:        "comment",
		IssueKey:    "DEVSBX-1",
		Payload:     `{"body":"considered"}`,
		Status:      store.StatusSuppressed,
	})
	require.NoError(t, err)

	fresh := seedLinkedWithEvent(t, s, "DEVSBX-2", "evt-fresh")

	// A cap of 1 is the whole point: under the old predicate the examined narrative
	// took the only slot (it sorts first — same window_start, lower id) and the one
	// with real work was never reached. That is the starvation, in miniature.
	got, err := s.NarrativesWithActionableLinks(1, reconcileRoles)
	require.NoError(t, err)

	require.Len(t, got, 1, "the cap must still bound the result")
	assert.Equal(t, fresh, got[0].ID,
		"the cap is a spend bound, so every slot must go to a narrative that will actually cost a "+
			"GetIssue and a drafting call; spending it on one the pass skips for free is how the "+
			"tail went unreached for 30+ passes")
}

// TestNarrativesWithActionableLinks_MatchesItsCount pins the selector to the count
// that reports it, which is the drift F10 warned about and F12 re-learned. If these
// two disagree, the operator-facing remainder describes a population the pass never
// examines — and this time they are two independent copies of the same two-part
// predicate, so there is real room to diverge.
func TestNarrativesWithActionableLinks_MatchesItsCount(t *testing.T) {
	s := openStore(t)

	// One of each shape: examined, unexamined, and linked-but-unactionable.
	examined := seedLinkedWithEvent(t, s, "DEVSBX-1", "evt-examined")
	_, err := s.InsertAction(store.ActionRow{
		NarrativeID: examined, Type: "comment", IssueKey: "DEVSBX-1",
		Payload: `{"body":"considered"}`, Status: store.StatusSuppressed,
	})
	require.NoError(t, err)

	seedLinkedWithEvent(t, s, "DEVSBX-2", "evt-fresh")
	seedLinkedWithEvent(t, s, "DEVSBX-3", "evt-fresh-two")

	selected, err := s.NarrativesWithActionableLinks(bigEnoughToHoldEverything, reconcileRoles)
	require.NoError(t, err)

	counted, err := s.CountNarrativesWithDelta(reconcileRoles)
	require.NoError(t, err)

	assert.Len(t, selected, counted,
		"the count and the selection must describe the same population")
	assert.Equal(t, 2, counted, "the already-examined narrative is in neither")
}
