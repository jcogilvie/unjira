package reconciler

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/store"
)

// seedUntracked inserts a narrative with events and NO narrative_issues rows.
func seedUntracked(t *testing.T, s *store.Store, evts ...events.Event) int64 {
	t.Helper()

	base := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)
	id, err := s.InsertNarrative(base, base.Add(time.Hour),
		"rebuilt the retry path", "reworked how the client retries")
	require.NoError(t, err)

	ids := make([]int64, 0, len(evts))
	for _, e := range evts {
		_, err := s.InsertEvent(e)
		require.NoError(t, err)
		eid, err := s.EventIDByExternalID(e.Source, e.ExternalID)
		require.NoError(t, err)
		ids = append(ids, eid)
	}
	if len(ids) > 0 {
		require.NoError(t, s.LinkMembers(id, ids, 1))
	}

	return id
}

const worthTracking = `{"worth_tracking":true,"summary":"Rework the client retry path",` +
	`"description":"Replaced fixed retries with exponential backoff.","confidence":0.8,` +
	`"rationale":"substantial, lasting outcome"}`

// TestProposeCreates_ProposesForAnUntrackedNarrative is the fix for #168. Before
// this path existed, an unlinked narrative was invisible to the reconciler —
// NarrativesWithActionableLinks requires a link by construction, so adding
// "create" to draftSystemPrompt would have changed nothing.
func TestProposeCreates_ProposesForAnUntrackedNarrative(t *testing.T) {
	s := reconcileStore(t)
	nid := seedUntracked(t, s, codeEvent("cr:1", "reworked the retry path"))

	client := &fakeLLM{responses: []string{worthTracking}}

	got, _, err := ProposeCreates(context.Background(), s, client, testConfig(), nil, nil)

	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Len(t, got[0].Proposed, 1, "untracked work must yield an action, not silence")
	assert.Equal(t, nid, got[0].NarrativeID)
	assert.Equal(t, ActionCreate, got[0].Proposed[0].Type)
	assert.Equal(t, "Rework the client retry path", got[0].Proposed[0].Summary)
	assert.Equal(t, "Replaced fixed retries with exponential backoff.", got[0].Proposed[0].Body)
}

// TestProposeCreates_RespectsARefusal is the half that keeps this feature from
// being harmful. Most unmatched narratives should NOT become tickets. If the
// model cannot decline, unjira turns every unrecognized fragment of work into a
// ticket somebody has to close.
func TestProposeCreates_RespectsARefusal(t *testing.T) {
	s := reconcileStore(t)
	seedUntracked(t, s, codeEvent("cr:1", "poked at a flaky test for ten minutes"))

	client := &fakeLLM{responses: []string{
		`{"worth_tracking":false,"confidence":0.9,"rationale":"investigation with no outcome"}`,
	}}

	got, _, err := ProposeCreates(context.Background(), s, client, testConfig(), nil, nil)

	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Empty(t, got[0].Proposed, "a declined narrative must not produce a ticket")
	require.Len(t, got[0].Suppressed, 1,
		"'declined' and 'never considered' are different facts; only one is recorded here")
	assert.Contains(t, got[0].Suppressed[0], "investigation with no outcome")
}

// TestProposeCreates_SkipsNarrativesThatAlreadyHaveALink: a narrative with a
// confident primary (at or above match.confidence_floor) is tracked, and proposing a
// create for it would open a duplicate ticket for work an issue already covers.
//
// A primary BELOW the floor is the other case, and it is not tracked: the model doubted
// it, so the work is proposed as a create naming it (no_confident_primary_test.go,
// finding F64). Matching is done with both, since a primary link at any confidence takes
// a narrative out of its backlog (finding F11).
func TestProposeCreates_SkipsNarrativesThatAlreadyHaveALink(t *testing.T) {
	s := reconcileStore(t)
	seedLinkedNarrative(t, s, "DEVSBX-9", store.RolePrimary, codeEvent("cr:1", "work"))

	backlog, err := s.NarrativesWithoutPrimaryLink(10)
	require.NoError(t, err)
	require.Empty(t, backlog,
		"precondition: a narrative with a primary link is not matching's work any more")

	client := &fakeLLM{responses: []string{worthTracking}}

	got, _, err := ProposeCreates(context.Background(), s, client, testConfig(), nil, nil,
		WithConfidenceFloor(0.9))

	require.NoError(t, err)
	assert.Empty(t, got, "a linked narrative is tracked; creating for it would duplicate")
	assert.Empty(t, client.prompts)
}

// TestProposeCreates_DoesNotProposeASecondCreateWhenOneIsApplied: the
// duplicate-ticket case. An applied create means an issue exists, so the narrative is
// not selected at all. proposeCreateOne's own refusal is the backstop, pinned in
// TestProposeCreateOne_BackstopsRemainInPlace.
func TestProposeCreates_DoesNotProposeASecondCreateWhenOneIsApplied(t *testing.T) {
	s := reconcileStore(t)
	nid := seedUntracked(t, s, codeEvent("cr:1", "work"))

	id, err := s.InsertAction(store.ActionRow{
		NarrativeID: nid, Type: "create",
		Payload: `{"summary":"s","description":"d"}`, Status: "proposed",
	})
	require.NoError(t, err)
	require.NoError(t, s.UpdateActionStatus(id, "applied"))

	client := &fakeLLM{responses: []string{worthTracking}}

	got, _, err := ProposeCreates(context.Background(), s, client, testConfig(), nil, nil)

	require.NoError(t, err)
	assert.Empty(t, got, "a narrative already created for must not take a create slot")
	assert.Empty(t, client.prompts, "no LLM call should be spent on a narrative already created for")
}

// TestProposeCreates_DoesNotProposeASecondCreateWhileOneAwaitsReview: two live
// proposals for the same untracked work would double the review queue and, if both
// were approved, open two tickets.
func TestProposeCreates_DoesNotProposeASecondCreateWhileOneAwaitsReview(t *testing.T) {
	s := reconcileStore(t)
	nid := seedUntracked(t, s, codeEvent("cr:1", "work"))

	_, err := s.InsertAction(store.ActionRow{
		NarrativeID: nid, Type: "create",
		Payload: `{"summary":"s","description":"d"}`, Status: "proposed",
	})
	require.NoError(t, err)

	client := &fakeLLM{responses: []string{worthTracking}}

	got, _, err := ProposeCreates(context.Background(), s, client, testConfig(), nil, nil)

	require.NoError(t, err)
	assert.Empty(t, got, "a narrative whose create awaits review must not take a create slot")
	assert.Empty(t, client.prompts)
}

// TestProposeCreates_ARejectedCreateDoesNotBlockForever: a human rejected THAT
// draft. A later pass with more events may be right, and rejecting again is cheap.
// Permanent suppression would make one "no" final for work that grows.
func TestProposeCreates_ARejectedCreateDoesNotBlockForever(t *testing.T) {
	s := reconcileStore(t)
	nid := seedUntracked(t, s, codeEvent("cr:1", "work"))

	id, err := s.InsertAction(store.ActionRow{
		NarrativeID: nid, Type: "create",
		Payload: `{"summary":"s","description":"d"}`, Status: "proposed",
	})
	require.NoError(t, err)
	require.NoError(t, s.UpdateActionStatus(id, "rejected"))

	client := &fakeLLM{responses: []string{worthTracking}}

	got, _, err := ProposeCreates(context.Background(), s, client, testConfig(), nil, nil)

	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Len(t, got[0].Proposed, 1,
		"a rejected draft is a ruling on that draft, not a permanent veto on the work")
}

// TestProposeCreates_RefusesToOpenABlankIssue: a "worth_tracking": true with no
// summary would reach gate.Applier and open a blank ticket. Substituting the
// narrative title would silently produce an issue whose text no model wrote.
func TestProposeCreates_RefusesToOpenABlankIssue(t *testing.T) {
	s := reconcileStore(t)
	seedUntracked(t, s, codeEvent("cr:1", "work"))

	client := &fakeLLM{responses: []string{
		`{"worth_tracking":true,"summary":"","description":"d","confidence":0.9,"rationale":"r"}`,
	}}

	got, _, err := ProposeCreates(context.Background(), s, client, testConfig(), nil, nil)

	// Per-narrative isolation means the pass survives; the narrative proposes
	// nothing.
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Empty(t, got[0].Proposed, "a blank issue must never be proposed")
}

// TestProposeCreates_IgnoresNarrativesMadeOnlyOfUnjirasOwnOutput: proposing a
// ticket about unjira's own commentary is the feedback loop dropSelfAuthored
// exists to prevent.
func TestProposeCreates_IgnoresNarrativesMadeOnlyOfUnjirasOwnOutput(t *testing.T) {
	s := reconcileStore(t)

	own := events.NewEvent("jira", "cr:own",
		time.Date(2026, 8, 28, 9, 30, 0, 0, time.UTC), "unjira commented on DEVSBX-9")
	own.Artifacts = map[string]any{"authored_by_unjira": true}
	seedUntracked(t, s, own)

	client := &fakeLLM{responses: []string{worthTracking}}

	got, _, err := ProposeCreates(context.Background(), s, client, testConfig(), nil, nil)

	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Empty(t, got[0].Proposed)
	assert.Contains(t, got[0].Suppressed[0], "unjira's own output")
	assert.Empty(t, client.prompts)
}

// TestProposeCreates_PromptCarriesEveryEventNotADelta: there is no prior action to
// compute a delta against, and the issue is being opened for the whole body of
// work at once. A delta-shaped prompt would describe only the tail.
func TestProposeCreates_PromptCarriesEveryEventNotADelta(t *testing.T) {
	s := reconcileStore(t)
	seedUntracked(t, s,
		codeEvent("cr:1", "the earliest decision"),
		codeEvent("cr:2", "the latest change"))

	client := &fakeLLM{responses: []string{worthTracking}}

	_, _, err := ProposeCreates(context.Background(), s, client, testConfig(), nil, nil)

	require.NoError(t, err)
	require.Len(t, client.prompts, 1)
	assert.Contains(t, client.prompts[0], "the earliest decision",
		"a create describes the whole body of work, so the oldest event must be present")
	assert.Contains(t, client.prompts[0], "the latest change")
}

// TestParseCreateResponse tolerates the shapes a model actually produces and
// rejects the ones that would open a bad ticket.
func TestParseCreateResponse(t *testing.T) {
	cases := []struct {
		name          string
		raw           string
		wantTracking  bool
		wantErrPhrase string
	}{
		{
			name:         "bare object",
			raw:          `{"worth_tracking":true,"summary":"s","description":"d"}`,
			wantTracking: true,
		},
		{
			name:         "fenced object, the shape every prompt forbids and models still send",
			raw:          "```json\n{\"worth_tracking\":true,\"summary\":\"s\"}\n```",
			wantTracking: true,
		},
		{
			name:         "refusal needs no summary",
			raw:          `{"worth_tracking":false,"rationale":"too small"}`,
			wantTracking: false,
		},
		{
			name:          "tracking with no summary is refused, not defaulted",
			raw:           `{"worth_tracking":true,"summary":"","description":"d"}`,
			wantErrPhrase: "refusing to open a blank issue",
		},
		{
			name:          "whitespace-only summary is still blank",
			raw:           `{"worth_tracking":true,"summary":"   ","description":"d"}`,
			wantErrPhrase: "refusing to open a blank issue",
		},
		{
			name:          "malformed json names the response",
			raw:           `{"worth_tracking":true,`,
			wantErrPhrase: "parsing create response",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCreateResponse(tc.raw)

			if tc.wantErrPhrase != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErrPhrase)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.wantTracking, got.WorthTracking)
		})
	}
}

// TestClampConfidence bounds a malformed score. No floorConfidence equivalent
// exists for creates, deliberately: that function lowers a score using facts about
// a live issue, and a create has none — inventing a floor would fabricate rigor.
func TestClampConfidence(t *testing.T) {
	assert.InDelta(t, 0.0, clampConfidence(-1), 0.001)
	assert.InDelta(t, 1.0, clampConfidence(9), 0.001)
	assert.InDelta(t, 0.7, clampConfidence(0.7), 0.001)
}

const declined = `{"worth_tracking":false,"confidence":0.9,` +
	`"rationale":"investigation with no lasting outcome"}`

// TestProposeCreates_ADeclineIsRememberedSoTheNextPassIsFree is the fix that
// replaced the removed opt-in flag. Before it, a declined narrative was re-judged
// on every pass forever — one LLM call per untracked narrative per watch tick,
// with no memory of the refusal. That recurring cost was the real problem the flag
// had been papering over, and a flag "solved" it by making the feature unusable.
//
// Three passes, one call.
func TestProposeCreates_ADeclineIsRememberedSoTheNextPassIsFree(t *testing.T) {
	s := reconcileStore(t)
	seedUntracked(t, s, codeEvent("dc:1", "ten minutes poking at a flaky test"))

	client := &fakeLLM{responses: []string{declined, declined, declined}}

	got, _, err := ProposeCreates(context.Background(), s, client, testConfig(), nil, nil)
	require.NoError(t, err, "pass 1")
	require.Len(t, got, 1)
	assert.Empty(t, got[0].Proposed, "pass 1 declines")

	for pass := 2; pass <= 3; pass++ {
		got, _, err := ProposeCreates(context.Background(), s, client, testConfig(), nil, nil)
		require.NoError(t, err, "pass %d", pass)
		assert.Empty(t, got,
			"pass %d must not select the declined narrative: it would hold a slot for nothing", pass)
	}

	assert.Len(t, client.prompts, 1,
		"the model is asked ONCE; the decline is remembered, not re-derived every pass")
}

// TestProposeCreates_ADeclineIsRecordedAsADistinctStatus: "declined" is the model
// judging, "rejected" is a human ruling. Conflating them would let slice 7's
// rules.Distill learn from unjira's own refusals as though a reviewer made them —
// training the model on its own output.
func TestProposeCreates_ADeclineIsRecordedAsADistinctStatus(t *testing.T) {
	s := reconcileStore(t)
	nid := seedUntracked(t, s, codeEvent("dc:1", "not much"))

	client := &fakeLLM{responses: []string{declined}}

	_, _, err := ProposeCreates(context.Background(), s, client, testConfig(), nil, nil)
	require.NoError(t, err)

	actions, err := s.ActionsForNarrative(nid)
	require.NoError(t, err)
	require.Len(t, actions, 1, "the decline is persisted, not merely reported")
	assert.Equal(t, StatusDeclined, actions[0].Status)
	assert.NotEqual(t, "rejected", actions[0].Status,
		"a model's refusal must not read as a human ruling")
	assert.Contains(t, actions[0].Rationale, "no lasting outcome",
		"a human asking why nothing was filed needs the reason on the row")

	// The decline must not appear in the review queue: there is nothing to review.
	queued, err := s.ActionsByStatus("proposed")
	require.NoError(t, err)
	assert.Empty(t, queued, "a decline is a record, not a proposal")
}

// TestProposeCreates_ADeclineIsReconsideredWhenNewWorkArrives is why the memory is
// "ask again on change" rather than a permanent veto. A narrative that looked like
// a ten-minute investigation can become substantial by its fourth day, and a
// permanent no would make the first judgment final for work that grew.
func TestProposeCreates_ADeclineIsReconsideredWhenNewWorkArrives(t *testing.T) {
	s := reconcileStore(t)
	nid := seedUntracked(t, s, codeEvent("dc:1", "started poking at something"))

	client := &fakeLLM{responses: []string{declined, worthTracking}}

	// Pass 1: declined.
	got, _, err := ProposeCreates(context.Background(), s, client, testConfig(), nil, nil)
	require.NoError(t, err)
	require.Empty(t, got[0].Proposed)

	later := events.NewEvent("claude_code", "dc:2",
		time.Date(2026, 8, 28, 14, 0, 0, 0, time.UTC), "it turned into a real project")
	_, err = s.InsertEvent(later)
	require.NoError(t, err)
	eid, err := s.EventIDByExternalID("claude_code", "dc:2")
	require.NoError(t, err)
	require.NoError(t, s.LinkMembers(nid, []int64{eid}, 1))

	// Pass 2: the work changed, so ask again.
	got, _, err = ProposeCreates(context.Background(), s, client, testConfig(), nil, nil)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Len(t, got[0].Proposed, 1,
		"new events mean the earlier judgment was made on less information")
	assert.Len(t, client.prompts, 2, "exactly two calls: one per genuine change")
}

// TestProposeCreates_DeclinedAndNeverConsideredStayDistinct: "declined earlier" and
// "never considered" are different facts. A declined narrative is no longer selected,
// so a pass's output cannot tell them apart; the create backlog count does, since it
// counts the narrative a cap kept from the model and not the one the model declined.
func TestProposeCreates_DeclinedAndNeverConsideredStayDistinct(t *testing.T) {
	s := reconcileStore(t)
	seedUntracked(t, s, codeEvent("dc:1", "not much"))
	neverConsidered := seedUntracked(t, s, codeEvent("dc:2", "behind the cap"))

	client := &fakeLLM{responses: []string{declined}}

	_, _, err := ProposeCreates(context.Background(), s, client, capOf(1), nil, nil)
	require.NoError(t, err)

	awaiting, err := s.NarrativesAwaitingCreate(10, 0)
	require.NoError(t, err)
	require.Len(t, awaiting, 1)
	assert.Equal(t, neverConsidered, awaiting[0].ID,
		"only the narrative the model never saw still awaits a decision")

	count, err := s.CountNarrativesAwaitingCreate(0)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

// TestProposeCreates_ADeclineDoesNotBlockACommentOnTheSameNarrative: a decline
// concerns opening an ISSUE. If matching later links the narrative, the reconciler
// must still be able to comment on it — hasDecline is keyed on create actions
// specifically for that reason.
func TestProposeCreates_ADeclineDoesNotBlockACommentOnTheSameNarrative(t *testing.T) {
	s := reconcileStore(t)
	nid := seedUntracked(t, s, codeEvent("dc:1", "not much"))

	client := &fakeLLM{responses: []string{declined}}
	_, _, err := ProposeCreates(context.Background(), s, client, testConfig(), nil, nil)
	require.NoError(t, err)

	// A comment action lands normally.
	_, err = s.InsertAction(store.ActionRow{
		NarrativeID: nid, Type: "comment", IssueKey: "DEVSBX-9",
		Payload: `{"body":"now that it is linked"}`, Status: StatusProposed,
	})
	require.NoError(t, err)

	queued, err := s.ActionsByStatus(StatusProposed)
	require.NoError(t, err)
	assert.Len(t, queued, 1, "a declined create must not suppress commenting")
}
