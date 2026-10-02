package triage

// shared_context_test.go covers triage's merge and split under member and context
// links (docs/superpowers/specs/2026-10-02-shared-context-design.md §6), and the
// below-floor attributions a reviewer is asked to confirm (§1).

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/store"
)

func insertTriageEvent(t *testing.T, s *store.Store, ext string, at time.Time) int64 {
	t.Helper()

	_, err := s.InsertEvent(events.NewEvent("claude_code", ext, at, "work "+ext))
	require.NoError(t, err)
	id, err := s.EventIDByExternalID("claude_code", ext)
	require.NoError(t, err)

	return id
}

func kindOf(t *testing.T, s *store.Store, nid, eid int64) store.LinkKind {
	t.Helper()

	l, err := s.NarrativeEventLink(nid, eid)
	require.NoError(t, err)

	return l.Kind
}

// TestStoreHandler_MergeOntoATargetHoldingTheEventAsContextKeepsItAMember is the
// merge-ordering test the spec says to write first. The target already holds E as
// background; the source holds E as work. The old order — add through INSERT OR
// IGNORE, then unlink — would keep the target's context row, delete the source's
// member link, and leave E with no home at all, with no error.
func TestStoreHandler_MergeOntoATargetHoldingTheEventAsContextKeepsItAMember(t *testing.T) {
	s := handlerTestStore(t)
	base := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	target, _ := seedHandlerNarrative(t, s, "target", base, "mc:t1")
	source, _ := seedHandlerNarrative(t, s, "source", base.Add(time.Hour), "mc:s1")
	e := insertTriageEvent(t, s, "mc:shared", base.Add(2*time.Hour))
	require.NoError(t, s.LinkMembers(source, []int64{e}, 0.4))
	require.NoError(t, s.LinkContext(target, []int64{e}))

	got, err := NewStoreHandler(s, nil, nil, nil, testCorrelatorConfig(), 0).MergeNarratives(target, source)

	require.NoError(t, err)
	assert.Contains(t, got.Members, e)
	l, err := s.NarrativeEventLink(target, e)
	require.NoError(t, err)
	assert.Equal(t, store.LinkMember, l.Kind, "the target's background became its work")
	require.NotNil(t, l.MemberConfidence)
	assert.InDelta(t, store.ReviewerMemberConfidence, *l.MemberConfidence, 1e-9,
		"the reviewer's merge is a human attribution")
	_, err = s.NarrativeEventLink(source, e)
	require.ErrorIs(t, err, sql.ErrNoRows)
}

func TestStoreHandler_MergeRehomesTheSourcesEligibleContextLinks(t *testing.T) {
	s := handlerTestStore(t)
	base := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	target, _ := seedHandlerNarrative(t, s, "target", base, "mr:t1")
	source, _ := seedHandlerNarrative(t, s, "source", base.Add(time.Hour), "mr:s1")
	elsewhere, _ := seedHandlerNarrative(t, s, "home", base, "mr:h1")
	bg := insertTriageEvent(t, s, "mr:bg", base.Add(3*time.Hour))
	require.NoError(t, s.LinkMembers(elsewhere, []int64{bg}, 0.9))
	require.NoError(t, s.LinkContext(source, []int64{bg}))

	got, err := NewStoreHandler(s, nil, nil, nil, testCorrelatorConfig(), 0).MergeNarratives(target, source)

	require.NoError(t, err)
	assert.Equal(t, []int64{bg}, got.Context)
	assert.Equal(t, store.LinkContext, kindOf(t, s, target, bg))
	_, err = s.NarrativeEventLink(source, bg)
	require.ErrorIs(t, err, sql.ErrNoRows, "moved, not copied")
	assert.Equal(t, store.LinkMember, kindOf(t, s, elsewhere, bg), "the member home is untouched")
}

// TestSplitNarrative_WithSharedEventsEmptiesAndMarksTheSource is the spec's split test.
// The source holds two members and one context link. The split moves both members to
// the new narratives, so the source holds only background — no work — and must be
// marked split, its context link deleted and counted. markSourceIfEmptied used to
// count ALL links, which would leave such a source open in every later prompt.
func TestSplitNarrative_WithSharedEventsEmptiesAndMarksTheSource(t *testing.T) {
	s, nid, ids := splitStore(t, 2)
	base := time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC)
	home, err := s.InsertNarrative(base, base, "home", "h")
	require.NoError(t, err)
	bg := insertTriageEvent(t, s, "sp:bg", base.Add(30*time.Minute))
	require.NoError(t, s.LinkMembers(home, []int64{bg}, 0.9))
	require.NoError(t, s.LinkContext(nid, []int64{bg}))
	client := &wireLLM{responses: []string{
		// The halves may share background: story A's event is context for story B.
		`[{"kind":"new","title":"story A","summary":"a","confidence":0.9,"event_indices":[0]},` +
			`{"kind":"new","title":"story B","summary":"b","confidence":0.9,"event_indices":[1],"context_indices":[0]}]`,
	}}

	got, err := splitHandler(s, client).SplitNarrative(context.Background(), nid)

	require.NoError(t, err)
	require.Len(t, got.Narratives, 2)
	assert.Equal(t, 1, got.SourceContextLinks)
	assert.True(t, got.SourceContextLinksDeleted)
	src, err := s.GetNarrative(nid)
	require.NoError(t, err)
	assert.Equal(t, store.StatusSplit, src.Status, "a source holding only background holds no work")
	n, err := s.NarrativeEventCount(nid)
	require.NoError(t, err)
	assert.Zero(t, n, "its context link went with it")
	assert.Equal(t, store.LinkMember, kindOf(t, s, home, bg), "which loses nothing: the event's home is elsewhere")

	storyA, storyB := got.Narratives[0].ID, got.Narratives[1].ID
	assert.Equal(t, store.LinkMember, kindOf(t, s, storyA, ids[0]))
	assert.Equal(t, store.LinkContext, kindOf(t, s, storyB, ids[0]), "context between the halves is allowed")
	assert.Equal(t, store.LinkMember, kindOf(t, s, storyB, ids[1]))
}

// TestSplitNarrative_ASourceThatKeepsWorkKeepsItsContextLinks: a frozen member stays
// on the source, so the source is still a live story, and its background stays too.
func TestSplitNarrative_ASourceThatKeepsWorkKeepsItsContextLinks(t *testing.T) {
	s, nid, _ := splitStore(t, 1)
	base := time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC)
	commitAgainstNarrative(t, s, nid)
	later := []int64{
		insertTriageEvent(t, s, "sp:later1", base.Add(5*time.Hour)),
		insertTriageEvent(t, s, "sp:later2", base.Add(6*time.Hour)),
	}
	require.NoError(t, s.LinkMembers(nid, later, 0.9))
	home, err := s.InsertNarrative(base, base, "home", "h")
	require.NoError(t, err)
	bg := insertTriageEvent(t, s, "sp:bg", base.Add(30*time.Minute))
	require.NoError(t, s.LinkMembers(home, []int64{bg}, 0.9))
	require.NoError(t, s.LinkContext(nid, []int64{bg}))

	got, err := splitHandler(s, &wireLLM{responses: []string{twoClusters}}).SplitNarrative(context.Background(), nid)

	require.NoError(t, err)
	assert.Equal(t, 1, got.SourceContextLinks)
	assert.False(t, got.SourceContextLinksDeleted)
	assert.Equal(t, store.LinkContext, kindOf(t, s, nid, bg))
	src, err := s.GetNarrative(nid)
	require.NoError(t, err)
	assert.NotEqual(t, store.StatusSplit, src.Status)
}

// TestStoreHandler_NarrativeContextSurfacesAttributionsToConfirm: below the floor, a
// member link reaches the reviewer; at the default floor of 0, nothing does.
func TestStoreHandler_NarrativeContextSurfacesAttributionsToConfirm(t *testing.T) {
	s := handlerTestStore(t)
	base := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	nid, err := s.InsertNarrative(base, base, "work", "s")
	require.NoError(t, err)
	sure := insertTriageEvent(t, s, "ac:sure", base)
	doubtful := insertTriageEvent(t, s, "ac:doubtful", base.Add(time.Minute))
	require.NoError(t, s.LinkMembers(nid, []int64{sure}, 0.95))
	require.NoError(t, s.LinkMembers(nid, []int64{doubtful}, 0.35))

	cfg := testCorrelatorConfig()
	off, err := NewStoreHandler(s, nil, nil, nil, cfg, 0).NarrativeContext(nid)
	require.NoError(t, err)
	assert.Empty(t, off.ToConfirm, "the floor ships off")

	cfg.MemberConfidenceFloor = 0.5
	on, err := NewStoreHandler(s, nil, nil, nil, cfg, 0).NarrativeContext(nid)
	require.NoError(t, err)
	require.Len(t, on.ToConfirm, 1)
	assert.Equal(t, "work ac:doubtful", on.ToConfirm[0].Summary)
	assert.InDelta(t, 0.35, on.ToConfirm[0].Confidence, 1e-9)
}
