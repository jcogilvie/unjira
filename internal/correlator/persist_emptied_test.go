package correlator_test

// persist_emptied_test.go pins what Persist does to a narrative its member moves
// leave holding no work (finding F40): it is marked store.StatusSplit, its context
// links go with it, and the pass reports it.

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/store"
)

func narrativeStatus(t *testing.T, s *store.Store, nid int64) string {
	t.Helper()

	row, err := s.GetNarrative(nid)
	require.NoError(t, err)

	return row.Status
}

// TestPersist_AReshuffleThatEmptiesANarrativeMarksItSplit: A's two eligible members
// go to two different clusters, a new narrative and an extend of B. A holds no work,
// so it is marked split and its one context link is deleted. The narratives that
// received the work stay open.
func TestPersist_AReshuffleThatEmptiesANarrativeMarksItSplit(t *testing.T) {
	s := persistStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	a1 := seedPersistedEvent(t, s, "a1", "first half", base)
	a2 := seedPersistedEvent(t, s, "a2", "second half", base.Add(time.Minute))
	b1 := seedPersistedEvent(t, s, "b1", "B's own work", base.Add(2*time.Minute))
	bg := seedPersistedEvent(t, s, "bg", "H's investigation", base.Add(-time.Hour))
	h, err := s.InsertNarrative(bg.OccurredAt, bg.OccurredAt, "H", "h")
	require.NoError(t, err)
	require.NoError(t, s.LinkMembers(h, []int64{eventID(t, s, bg)}, 0.9))
	a, err := s.InsertNarrative(base, base.Add(time.Minute), "A", "a")
	require.NoError(t, err)
	require.NoError(t, s.LinkMembers(a, []int64{eventID(t, s, a1), eventID(t, s, a2)}, 0.9))
	require.NoError(t, s.LinkContext(a, []int64{eventID(t, s, bg)}))
	b, err := s.InsertNarrative(b1.OccurredAt, b1.OccurredAt, "B", "b")
	require.NoError(t, err)
	require.NoError(t, s.LinkMembers(b, []int64{eventID(t, s, b1)}, 0.9))

	got, stats, err := correlator.Persist(t.Context(), s, &fakeLLM{}, []correlator.ClusterResult{
		{Kind: correlator.ClusterNew, Title: "X", Summary: "x", Confidence: 0.8, Events: []correlator.Event{a1}},
		{Kind: correlator.ClusterExtends, NarrativeID: b, Summary: "b and a2", Events: []correlator.Event{a2}},
	}, roomyCorrelatorConfig)

	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, store.StatusSplit, narrativeStatus(t, s, a), "A holds no work")
	assert.Equal(t, store.StatusOpen, narrativeStatus(t, s, got[0].ID))
	assert.Equal(t, store.StatusOpen, narrativeStatus(t, s, b))
	_, err = s.NarrativeEventLink(a, eventID(t, s, bg))
	require.ErrorIs(t, err, sql.ErrNoRows, "A's background goes with it")
	assert.Equal(t, store.LinkMember, linkKind(t, s, h, bg), "and the event keeps its member home")
	assert.Equal(t, []correlator.EmptiedNarrative{{NarrativeID: a, Title: "A", ContextLinksDeleted: 1}}, stats.Emptied)
}

// TestPersist_APartialReshuffleLeavesTheNarrativeOpen: one of A's members moves, one
// stays. A still holds work, so nothing about it changes but the moved link.
func TestPersist_APartialReshuffleLeavesTheNarrativeOpen(t *testing.T) {
	s := persistStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	a1 := seedPersistedEvent(t, s, "a1", "moves", base)
	a2 := seedPersistedEvent(t, s, "a2", "stays", base.Add(time.Minute))
	a, err := s.InsertNarrative(base, base.Add(time.Minute), "A", "a")
	require.NoError(t, err)
	require.NoError(t, s.LinkMembers(a, []int64{eventID(t, s, a1), eventID(t, s, a2)}, 0.9))

	_, stats, err := correlator.Persist(t.Context(), s, &fakeLLM{}, []correlator.ClusterResult{
		{Kind: correlator.ClusterNew, Title: "X", Summary: "x", Confidence: 0.8, Events: []correlator.Event{a1}},
	}, roomyCorrelatorConfig)

	require.NoError(t, err)
	assert.Equal(t, store.StatusOpen, narrativeStatus(t, s, a))
	assert.Empty(t, stats.Emptied)
}

// TestPersist_AnEmptiedNarrativeGivenOnlyContextIsStillEmptied: the model moves A's
// only member to a new narrative and keeps it as A's background, as an extend of A
// carrying only context. Background for no work is still no work: A is marked split,
// the context link this pass wrote goes too, and the narrative Persist returns for
// the extend says so rather than reporting A open.
func TestPersist_AnEmptiedNarrativeGivenOnlyContextIsStillEmptied(t *testing.T) {
	s := persistStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	e := seedPersistedEvent(t, s, "e", "the only work", base)
	a, err := s.InsertNarrative(base, base, "A", "a")
	require.NoError(t, err)
	require.NoError(t, s.LinkMembers(a, []int64{eventID(t, s, e)}, 0.9))

	got, stats, err := correlator.Persist(t.Context(), s, &fakeLLM{}, []correlator.ClusterResult{
		{Kind: correlator.ClusterNew, Title: "X", Summary: "x", Confidence: 0.8, Events: []correlator.Event{e}},
		{Kind: correlator.ClusterExtends, NarrativeID: a, ContextEvents: []correlator.Event{e}},
	}, roomyCorrelatorConfig)

	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, store.StatusSplit, narrativeStatus(t, s, a))
	assert.Equal(t, store.StatusSplit, got[1].Status, "the returned narrative matches the store")
	_, err = s.NarrativeEventLink(a, eventID(t, s, e))
	require.ErrorIs(t, err, sql.ErrNoRows)
	assert.Equal(t, store.LinkMember, linkKind(t, s, got[0].ID, e))
	assert.Equal(t, 1, stats.ContextLinks, "the link was written, then deleted with A")
	assert.Equal(t, []correlator.EmptiedNarrative{{NarrativeID: a, Title: "A", ContextLinksDeleted: 1}}, stats.Emptied)
}

// TestPersist_AFailedPassEmptiesNothing: the mark runs inside Persist's transaction,
// so a pass that fails its commit check leaves the narrative it would have emptied
// exactly as it was.
func TestPersist_AFailedPassEmptiesNothing(t *testing.T) {
	s := persistStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	e := seedPersistedEvent(t, s, "e", "the only work", base)
	homeless := seedPersistedEvent(t, s, "homeless", "nobody's work", base.Add(time.Minute))
	a, err := s.InsertNarrative(base, base, "A", "a")
	require.NoError(t, err)
	require.NoError(t, s.LinkMembers(a, []int64{eventID(t, s, e)}, 0.9))

	_, _, err = correlator.Persist(t.Context(), s, &fakeLLM{}, []correlator.ClusterResult{{
		Kind: correlator.ClusterNew, Title: "X", Summary: "x", Confidence: 0.8,
		Events: []correlator.Event{e}, ContextEvents: []correlator.Event{homeless},
	}}, roomyCorrelatorConfig)

	require.ErrorContains(t, err, "no member home")
	assert.Equal(t, store.StatusOpen, narrativeStatus(t, s, a))
	assert.Equal(t, store.LinkMember, linkKind(t, s, a, e), "the move rolled back with it")
}
