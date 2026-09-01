package triage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/store"
)

// twoClusters is the response a model gives when it agrees the events are two
// stories. Indices refer to the "Events to cluster" numbering.
const twoClusters = `[{"kind":"new","title":"story A","summary":"the first thing","event_indices":[0]},` +
	`{"kind":"new","title":"story B","summary":"the second thing","event_indices":[1]}]`

// oneCluster is the response when the model disagrees with the reviewer.
const oneCluster = `[{"kind":"new","title":"actually one story","summary":"all of it",` +
	`"event_indices":[0,1]}]`

// splitStore seeds a narrative holding count events, none linked to an issue,
// returning the narrative id and its event ids in order.
func splitStore(t *testing.T, count int) (*store.Store, int64, []int64) {
	t.Helper()

	s, err := store.Open(filepath.Join(t.TempDir(), "split.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	base := time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC)
	nid, err := s.InsertNarrative(base, base.Add(time.Duration(count)*time.Hour),
		"two stories in one", "a summary covering both")
	require.NoError(t, err)

	ids := make([]int64, 0, count)
	for i := range count {
		ext := "sp:" + string(rune('a'+i))
		e := events.NewEvent("claude_code", ext, base.Add(time.Duration(i)*time.Hour), "some work")
		_, err := s.InsertEvent(e)
		require.NoError(t, err)
		eid, err := s.EventIDByExternalID("claude_code", ext)
		require.NoError(t, err)
		require.NoError(t, s.AddNarrativeEvents(nid, []int64{eid}))
		ids = append(ids, eid)
	}

	return s, nid, ids
}

func splitHandler(s *store.Store, client *wireLLM) *StoreHandler {
	return NewStoreHandler(s, nil, client, nil, testCorrelatorConfig(), 1000000)
}

// TestSplitNarrative_ProducesSeparateNarratives is the headline: the last unwired
// verb now does the thing.
func TestSplitNarrative_ProducesSeparateNarratives(t *testing.T) {
	s, nid, _ := splitStore(t, 2)
	client := &wireLLM{responses: []string{twoClusters}}

	got, err := splitHandler(s, client).SplitNarrative(context.Background(), nid)

	require.NoError(t, err)
	require.Len(t, got, 2, "a split must yield more than one narrative")
	assert.Equal(t, "story A", got[0].Title)
	assert.Equal(t, "story B", got[1].Title)

	for _, n := range got {
		count, err := s.NarrativeEventCount(n.ID)
		require.NoError(t, err)
		assert.Equal(t, 1, count, "narrative %d must hold its own events", n.ID)
	}
}

// TestSplitNarrative_MarksTheEmptiedSourceSplit: without this the source stays
// selectable by NarrativesOverlapping and is rendered into every later Cluster
// prompt as an existing narrative holding zero events.
func TestSplitNarrative_MarksTheEmptiedSourceSplit(t *testing.T) {
	s, nid, _ := splitStore(t, 2)
	client := &wireLLM{responses: []string{twoClusters}}

	_, err := splitHandler(s, client).SplitNarrative(context.Background(), nid)
	require.NoError(t, err)

	count, err := s.NarrativeEventCount(nid)
	require.NoError(t, err)
	require.Zero(t, count, "precondition: Persist's relinkEvents emptied the source")

	src, err := s.GetNarrative(nid)
	require.NoError(t, err)
	assert.Equal(t, store.StatusSplit, src.Status)

	// The real consequence, asserted through the query that matters.
	base := time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC)
	ctxNarratives, err := s.NarrativesOverlapping(base, base.Add(24*time.Hour))
	require.NoError(t, err)
	for _, n := range ctxNarratives {
		assert.NotEqual(t, nid, n.ID,
			"an emptied narrative must not be fed to Cluster as context")
	}
}

// TestSplitNarrative_KeepsCommittedEventsOnTheSource is the mixed-state case, and
// the reason split does not simply refuse when an action has been applied. A
// posted comment describes specific events; those stay, so the comment stays true
// about exactly what it described. Everything since is free to move.
func TestSplitNarrative_KeepsCommittedEventsOnTheSource(t *testing.T) {
	s, nid, ids := splitStore(t, 1)

	// One applied action freezes the event linked before it.
	aid, err := s.InsertAction(store.ActionRow{
		NarrativeID: nid, Type: "comment", IssueKey: "DEVSBX-9",
		Payload: `{"body":"posted about the first event"}`, Status: "proposed",
	})
	require.NoError(t, err)
	require.NoError(t, s.UpdateActionStatus(aid, "applied"))

	// linked_at has millisecond precision; sleep past the commit instant.
	time.Sleep(5 * time.Millisecond)

	base := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	for i, ext := range []string{"sp:later1", "sp:later2"} {
		e := events.NewEvent("claude_code", ext, base.Add(time.Duration(i)*time.Hour), "later work")
		_, err := s.InsertEvent(e)
		require.NoError(t, err)
		eid, err := s.EventIDByExternalID("claude_code", ext)
		require.NoError(t, err)
		require.NoError(t, s.AddNarrativeEvents(nid, []int64{eid}))
	}

	client := &wireLLM{responses: []string{twoClusters}}

	got, err := splitHandler(s, client).SplitNarrative(context.Background(), nid)

	require.NoError(t, err)
	require.Len(t, got, 2)

	remaining, err := s.NarrativeEventCount(nid)
	require.NoError(t, err)
	assert.Equal(t, 1, remaining,
		"the committed event stays: unjira cannot unpost the comment describing it")

	// And the source is NOT marked split, because it is still a live story.
	src, err := s.GetNarrative(nid)
	require.NoError(t, err)
	assert.Equal(t, store.StatusOpen, src.Status,
		"a narrative that kept committed events is still live and must stay in clustering")

	// Its applied action survives untouched.
	actions, err := s.ActionsForNarrative(nid)
	require.NoError(t, err)
	require.Len(t, actions, 1)
	assert.Equal(t, "applied", actions[0].Status)

	_ = ids
}

// TestSplitNarrative_OnlyEligibleEventsAreOffered: the committed event must not
// even reach the model, or it could be assigned to a new narrative — the exact
// laundering the watermark exists to prevent.
func TestSplitNarrative_OnlyEligibleEventsAreOffered(t *testing.T) {
	s, nid, _ := splitStore(t, 1)

	aid, err := s.InsertAction(store.ActionRow{
		NarrativeID: nid, Type: "comment", IssueKey: "DEVSBX-9",
		Payload: `{"body":"posted"}`, Status: "proposed",
	})
	require.NoError(t, err)
	require.NoError(t, s.UpdateActionStatus(aid, "applied"))

	time.Sleep(5 * time.Millisecond)

	base := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	for i, ext := range []string{"sp:free1", "sp:free2"} {
		e := events.NewEvent("claude_code", ext, base.Add(time.Duration(i)*time.Hour),
			"uncommitted work "+ext)
		_, err := s.InsertEvent(e)
		require.NoError(t, err)
		eid, err := s.EventIDByExternalID("claude_code", ext)
		require.NoError(t, err)
		require.NoError(t, s.AddNarrativeEvents(nid, []int64{eid}))
	}

	client := &wireLLM{responses: []string{twoClusters}}

	_, err = splitHandler(s, client).SplitNarrative(context.Background(), nid)
	require.NoError(t, err)

	require.Len(t, client.prompts, 1)
	assert.NotContains(t, client.prompts[0], "some work",
		"the committed event's summary must never reach the clusterer")
	assert.Contains(t, client.prompts[0], "uncommitted work sp:free1")
}

// TestSplitNarrative_CarriesTheReviewersInstruction — without it the model is
// asked to cluster events it has no reason to separate, and will most often
// return them as one story. The instruction IS the feature.
func TestSplitNarrative_CarriesTheReviewersInstruction(t *testing.T) {
	s, nid, _ := splitStore(t, 2)
	client := &wireLLM{responses: []string{twoClusters}}

	_, err := splitHandler(s, client).SplitNarrative(context.Background(), nid)
	require.NoError(t, err)

	require.Len(t, client.systemPrompts, 1)
	assert.Contains(t, client.systemPrompts[0], "do NOT belong to a single narrative",
		"the reviewer's judgment must reach the model")
}

// TestSplitNarrative_ReportsWhenTheModelDisagrees: reported, not forced. The
// reviewer may be wrong, and inventing a seam to satisfy the instruction would be
// worse than saying the model declined.
func TestSplitNarrative_ReportsWhenTheModelDisagrees(t *testing.T) {
	s, nid, _ := splitStore(t, 2)
	client := &wireLLM{responses: []string{oneCluster}}

	_, err := splitHandler(s, client).SplitNarrative(context.Background(), nid)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "kept narrative")
	assert.Contains(t, err.Error(), "nothing was changed")

	count, err := s.NarrativeEventCount(nid)
	require.NoError(t, err)
	assert.Equal(t, 2, count, "a refused split must leave the narrative intact")

	src, err := s.GetNarrative(nid)
	require.NoError(t, err)
	assert.Equal(t, store.StatusOpen, src.Status, "and must not mark it split")
}

// TestSplitNarrative_RefusesASingleEligibleEvent: one event cannot be two stories.
// An error beats a "split" that silently returns the narrative unchanged.
func TestSplitNarrative_RefusesASingleEligibleEvent(t *testing.T) {
	s, nid, _ := splitStore(t, 1)
	client := &wireLLM{}

	_, err := splitHandler(s, client).SplitNarrative(context.Background(), nid)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "nothing to split")
	assert.Empty(t, client.prompts, "no LLM call should be spent on an unsplittable narrative")
}

// TestSplitNarrative_RefusesWhenEverythingIsCommitted: every event frozen means
// there is nothing left that may be reattributed.
func TestSplitNarrative_RefusesWhenEverythingIsCommitted(t *testing.T) {
	s, nid, _ := splitStore(t, 3)

	aid, err := s.InsertAction(store.ActionRow{
		NarrativeID: nid, Type: "comment", IssueKey: "DEVSBX-9",
		Payload: `{"body":"posted about all of it"}`, Status: "proposed",
	})
	require.NoError(t, err)
	require.NoError(t, s.UpdateActionStatus(aid, "applied"))

	client := &wireLLM{}

	_, err = splitHandler(s, client).SplitNarrative(context.Background(), nid)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "nothing to split")
	assert.Empty(t, client.prompts)
}

// TestSplitNarrative_ReportsUnavailableWithoutAnLLMClient: the handler is
// constructible without one, so split must say so rather than panic on a nil.
func TestSplitNarrative_ReportsUnavailableWithoutAnLLMClient(t *testing.T) {
	s, nid, _ := splitStore(t, 2)

	h := NewStoreHandler(s, nil, nil, nil, testCorrelatorConfig(), 1000000)

	_, err := h.SplitNarrative(context.Background(), nid)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "unavailable")
}

// TestSplitNarrative_ForcesResultsToNewEvenIfTheModelSaysExtends: Cluster was
// given no existing narratives, so a narrative_id in the response is fabricated.
// Trusting it would have Persist extend an unrelated narrative — moving this work
// somewhere nobody asked for.
func TestSplitNarrative_ForcesResultsToNewEvenIfTheModelSaysExtends(t *testing.T) {
	s, nid, _ := splitStore(t, 2)

	// A bystander narrative the model's fabricated id points at.
	base := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	bystander, err := s.InsertNarrative(base, base.Add(time.Hour), "unrelated work", "s")
	require.NoError(t, err)

	client := &wireLLM{responses: []string{
		`[{"kind":"extends","narrative_id":` + itoa(bystander) + `,"title":"hijacked",` +
			`"summary":"s","event_indices":[0]},` +
			`{"kind":"new","title":"story B","summary":"s","event_indices":[1]}]`,
	}}

	got, err := splitHandler(s, client).SplitNarrative(context.Background(), nid)

	require.NoError(t, err)
	require.Len(t, got, 2)

	count, err := s.NarrativeEventCount(bystander)
	require.NoError(t, err)
	assert.Zero(t, count,
		"a fabricated narrative_id must not pull this work onto an unrelated narrative")
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}

	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}

	return string(digits)
}

// TestRestructure_SplitResolvesTheBatchPosition: the reviewer names a batch
// position, not a narrative id. A mis-resolved index would split the wrong story.
func TestRestructure_SplitResolvesTheBatchPosition(t *testing.T) {
	s, nid, _ := splitStore(t, 2)

	// Two actions; the reviewer names position 2.
	batch := []store.ActionRow{
		{ID: 1, NarrativeID: 999, Type: "comment", IssueKey: "DEVSBX-1"},
		{ID: 2, NarrativeID: nid, Type: "comment", IssueKey: "DEVSBX-2"},
	}

	client := &wireLLM{responses: []string{twoClusters}}
	h := splitHandler(s, client)

	replaced, err := h.Restructure(
		context.Background(), Decision{Verb: VerbSplit, Positions: []int{2}}, batch)

	require.NoError(t, err)
	assert.Empty(t, replaced,
		"a split replaces no actions: the next reconcile pass drafts for the new narratives")

	src, err := s.GetNarrative(nid)
	require.NoError(t, err)
	assert.Equal(t, store.StatusSplit, src.Status, "position 2's narrative is the one that split")
}

// TestRestructure_SplitRejectsABadPosition: an out-of-range index must not resolve
// to some other narrative.
func TestRestructure_SplitRejectsABadPosition(t *testing.T) {
	s, _, _ := splitStore(t, 2)
	client := &wireLLM{}
	h := splitHandler(s, client)

	batch := []store.ActionRow{{ID: 1, NarrativeID: 1}}

	for _, positions := range [][]int{{0}, {2}, {}, {1, 2}} {
		_, err := h.Restructure(
			context.Background(), Decision{Verb: VerbSplit, Positions: positions}, batch)
		require.Error(t, err, "positions %v must be refused", positions)
	}

	assert.Empty(t, client.prompts, "a bad position must not reach the model")
}

// TestSplitNarrative_EndToEndThroughASession drives the whole surface: a scripted
// reviewer types `s 1`, and the batch, the store, and the clustering context all
// end up in the state a split should leave them in. The unit tests above each
// check one property; this checks they compose.
func TestSplitNarrative_EndToEndThroughASession(t *testing.T) {
	s, nid, _ := splitStore(t, 2)
	client := &wireLLM{responses: []string{twoClusters}}
	h := splitHandler(s, client)

	// One proposed action on the narrative about to be split.
	aid, err := s.InsertAction(store.ActionRow{
		NarrativeID: nid, Type: "comment", IssueKey: "DEVSBX-9",
		Payload: `{"body":"describes both stories at once"}`, Confidence: 0.7,
		Status: "proposed",
	})
	require.NoError(t, err)
	action, err := s.GetAction(aid)
	require.NoError(t, err)

	p := &scriptedSplitPrompter{answers: []Decision{{Verb: VerbSplit, Positions: []int{1}}}}
	session := NewSession(context.Background(), []store.ActionRow{action}, p, h)

	require.NoError(t, session.Run())

	// The batch is empty: the action described a story that is no longer one story.
	assert.Empty(t, session.Approved(),
		"the stale action is dropped, not carried forward for approval")

	// Two new narratives exist, each holding one event.
	base := time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC)
	live, err := s.NarrativesOverlapping(base, base.Add(24*time.Hour))
	require.NoError(t, err)
	require.Len(t, live, 2, "exactly the two narratives the split produced are live")
	for _, n := range live {
		assert.NotEqual(t, nid, n.ID, "the emptied source is not among them")
	}

	// Nothing was applied: a restructure touches no tracker.
	applied, err := s.ActionsByStatus("applied")
	require.NoError(t, err)
	assert.Empty(t, applied, "a split must not reach a tracker")
}

// scriptedSplitPrompter answers once, then skips, and records notices so a test
// can see a refusal reached the reviewer.
type scriptedSplitPrompter struct {
	answers []Decision
	notices []string
}

func (p *scriptedSplitPrompter) Ask(Item) (Decision, error) {
	if len(p.answers) == 0 {
		return Decision{Verb: VerbSkip}, nil
	}
	d := p.answers[0]
	p.answers = p.answers[1:]

	return d, nil
}

func (p *scriptedSplitPrompter) Confirm(string) (bool, error) { return false, nil }

func (p *scriptedSplitPrompter) Notify(message string) error {
	p.notices = append(p.notices, message)

	return nil
}
