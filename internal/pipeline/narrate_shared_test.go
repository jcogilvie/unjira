package pipeline_test

// narrate_shared_test.go runs the shared-context slice end to end through a real
// store: the cluster response, the dispute re-ask, Persist, and what the pass reports
// — read back from the store, because the printout once disagreed with it (F37).

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/pipeline"
	"github.com/jcogilvie/unjira/internal/store"
)

func narrateEventID(t *testing.T, s *store.Store, extID string) int64 {
	t.Helper()

	id, err := s.EventIDByExternalID("claude_code", extID)
	require.NoError(t, err)

	return id
}

func narratedIDs(evts []correlator.Event) []string {
	out := make([]string, 0, len(evts))
	for _, e := range evts {
		out = append(out, e.ExternalID)
	}

	return out
}

// TestRunNarrate_DoubleAssignmentPersistsOneHomeAndNoEmptyNarrative is F37 end to end.
// The response is the finding's shape, [NEW{E}, EXTENDS 5{E, F}]. Before this slice,
// relinkEvents applied both in order and left the new narrative open with zero
// events. Now the dispute re-ask asks which owns E; the model says the new one, so
// E is its member and narrative 5's background, F is 5's member, and no open
// narrative is empty.
func TestRunNarrate_DoubleAssignmentPersistsOneHomeAndNoEmptyNarrative(t *testing.T) {
	s := narrateStore(t)
	base := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	seedNarrateEvent(t, s, "prior", "earlier cache work", base.Add(-time.Hour))
	five, err := s.InsertNarrative(base.Add(-time.Hour), base, "Cache rework", "cache so far")
	require.NoError(t, err)
	require.NoError(t, s.LinkMembers(five, []int64{narrateEventID(t, s, "prior")}, 0.9))
	seedNarrateEvent(t, s, "E", "investigated the eviction bug", base.Add(time.Minute))
	seedNarrateEvent(t, s, "F", "more cache rework", base.Add(2*time.Minute))

	client := &narrateLLM{responses: []string{
		// Numbered: 0=E, 1=F (in-window), 2=prior (five's eligible member).
		`[{"kind":"new","title":"Eviction bug","summary":"found and fixed","confidence":0.8,"event_indices":[0]},` +
			`{"kind":"extends","narrative_id":` + strconv.FormatInt(five, 10) + `,"summary":"cache continued","confidence":0.9,"event_indices":[0,1]}]`,
		`[{"rationale":"E is the bug investigation itself","event_index":0,"member":{"cluster_position":0},"confidence":0.75}]`,
	}}
	window := correlator.TimeRange{Start: base, End: base.Add(time.Hour)}

	got, err := pipeline.RunNarrate(t.Context(), s, client, narrateConfig(), window, pipeline.NarrateOptions{})

	require.NoError(t, err)
	require.Len(t, client.prompts, 2, "one clustering call and one dispute re-ask")
	require.Len(t, got.Narratives, 2)
	newID := got.Narratives[0].ID

	e := narrateEventID(t, s, "E")
	l, err := s.NarrativeEventLink(newID, e)
	require.NoError(t, err)
	assert.Equal(t, store.LinkMember, l.Kind, "E's one home is the cluster the model chose")
	require.NotNil(t, l.MemberConfidence)
	assert.InDelta(t, 0.75, *l.MemberConfidence, 1e-9, "at the dispute's per-event confidence")
	l, err = s.NarrativeEventLink(five, e)
	require.NoError(t, err)
	assert.Equal(t, store.LinkContext, l.Kind, "the other claimant keeps E as background")
	l, err = s.NarrativeEventLink(five, narrateEventID(t, s, "F"))
	require.NoError(t, err)
	assert.Equal(t, store.LinkMember, l.Kind)

	for _, n := range []int64{newID, five} {
		members, err := s.MemberEventCount(n)
		require.NoError(t, err)
		assert.Positive(t, members, "narrative %d must not be left empty (F37)", n)
	}

	// The report matches the store, not merely the cluster response.
	assert.Equal(t, []string{"E"}, narratedIDs(got.Narratives[0].Events))
	assert.Equal(t, []string{"F"}, narratedIDs(got.Narratives[1].Events))
	assert.Equal(t, []string{"E"}, narratedIDs(got.Narratives[1].ContextEvents))
	assert.Equal(t, 1, got.Stats.DisputedEvents)
	assert.Equal(t, 1, got.Stats.ContextLinks)

	rendered := pipeline.RenderNarrateResult(got)
	assert.Contains(t, rendered, "dispute  1 event(s) placed in more than one cluster; resolved 1 by one re-ask")
	assert.Contains(t, rendered, `claude_code/E: new "Eviction bug" chosen over narrative `+strconv.FormatInt(five, 10))
	assert.Contains(t, rendered, "context  1 context link(s) across 1 shared event(s); largest fan-out 1")
	assert.Contains(t, rendered, "context (1) — another narrative's work")
}

// TestRunNarrate_AContextOnlyNewIsRejected: a NEW cluster carrying only
// context_indices is not a narrative — it would be an empty one whose title describes
// somebody else's work. Rejected before anything persists.
func TestRunNarrate_AContextOnlyNewIsRejected(t *testing.T) {
	s := narrateStore(t)
	base := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	seedNarrateEvent(t, s, "E", "work", base)
	client := &narrateLLM{responses: []string{
		`[{"kind":"new","title":"Real","summary":"s","confidence":0.9,"event_indices":[0]},` +
			`{"kind":"new","title":"Only background","summary":"s","context_indices":[0]}]`,
	}}

	_, err := pipeline.RunNarrate(t.Context(), s, client, narrateConfig(),
		correlator.TimeRange{Start: base, End: base.Add(time.Hour)}, pipeline.NarrateOptions{})

	require.ErrorContains(t, err, `new narrative "Only background" with no member events`)
	remaining, err := s.UnlinkedEventsInRange(base, base.Add(time.Hour))
	require.NoError(t, err)
	assert.Len(t, remaining, 1, "nothing persisted")
}

// TestHydrateContextNarratives_AFrozenMemberIsNeverNumbered (§5): an event whose member
// home has an applied action is frozen, so it has no number and cannot be placed. It
// is shown — as its narrative's event, and in full under another narrative that holds
// it as background — but an index for it does not exist.
func TestHydrateContextNarratives_AFrozenMemberIsNeverNumbered(t *testing.T) {
	s := narrateStore(t)
	base := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	seedNarrateEvent(t, s, "frozen", "FROZEN POSTED WORK", base.Add(-time.Hour))
	home, err := s.InsertNarrative(base.Add(-time.Hour), base, "Home", "h")
	require.NoError(t, err)
	require.NoError(t, s.LinkMembers(home, []int64{narrateEventID(t, s, "frozen")}, 0.9))
	reader, err := s.InsertNarrative(base.Add(-time.Hour), base, "Reader", "r")
	require.NoError(t, err)
	seedNarrateEvent(t, s, "reader-own", "reader's own work", base.Add(-30*time.Minute))
	require.NoError(t, s.LinkMembers(reader, []int64{narrateEventID(t, s, "reader-own")}, 0.9))
	require.NoError(t, s.LinkContext(reader, []int64{narrateEventID(t, s, "frozen")}))
	commitAction(t, s, home)
	seedNarrateEvent(t, s, "new", "new work", base.Add(time.Minute))

	client := &narrateLLM{responses: []string{
		// 0 = new (in-window), 1 = reader-own (reader's eligible member). Index 2 does not exist.
		`[{"kind":"new","title":"T","summary":"s","confidence":0.9,"event_indices":[0,2]}]`,
	}}

	_, err = pipeline.RunNarrate(t.Context(), s, client, narrateConfig(),
		correlator.TimeRange{Start: base, End: base.Add(time.Hour)}, pipeline.NarrateOptions{})

	require.ErrorContains(t, err, "event_indices value 2 out of range [0,2)")
	numbered, context, found := strings.Cut(client.prompts[0], "Existing narratives (CONTEXT ONLY):")
	require.True(t, found)
	assert.NotContains(t, numbered, "FROZEN POSTED WORK", "a frozen member is never numbered")
	assert.Contains(t, numbered, "reader's own work", "an eligible member is")
	assert.Equal(t, 2, strings.Count(context, "FROZEN POSTED WORK"),
		"shown as its home's event, and in full (no number to refer back to) as the reader's background")
	assert.Contains(t, context, "background (another narrative's work, linked here as context):")
}

func commitAction(t *testing.T, s *store.Store, nid int64) {
	t.Helper()

	id, err := s.InsertAction(store.ActionRow{
		NarrativeID: nid, Type: "comment", IssueKey: "PROJ-1",
		Payload: `{"body":"posted"}`, Status: store.StatusProposed,
	})
	require.NoError(t, err)
	require.NoError(t, s.UpdateActionStatus(id, store.StatusApplied))
}
