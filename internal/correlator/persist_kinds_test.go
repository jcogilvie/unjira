package correlator_test

// persist_kinds_test.go pins Persist's half of the shared-context design (spec §4):
// members are moved, context is added and never deletes, windows and summaries come
// from members only, and an event with only context links fails the commit.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/store"
)

var roomyCorrelatorConfig = config.CorrelatorConfig{TailSummarizeThresholdTokens: 1_000_000, RecentEventsKept: 20}

func eventID(t *testing.T, s *store.Store, e correlator.Event) int64 {
	t.Helper()

	id, err := s.EventIDByExternalID(e.Source, e.ExternalID)
	require.NoError(t, err)

	return id
}

func linkKind(t *testing.T, s *store.Store, nid int64, e correlator.Event) store.LinkKind {
	t.Helper()

	l, err := s.NarrativeEventLink(nid, eventID(t, s, e))
	require.NoError(t, err)

	return l.Kind
}

func TestPersist_ContextOnlyExtendLeavesSummaryAndWindowAlone(t *testing.T) {
	s := persistStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	own := seedPersistedEvent(t, s, "own", "own work", base)
	later := seedPersistedEvent(t, s, "later", "another stream's later work", base.Add(48*time.Hour))
	reader, err := s.InsertNarrative(base, base.Add(time.Hour), "Reader", "the reader's own summary")
	require.NoError(t, err)
	require.NoError(t, s.LinkMembers(reader, []int64{eventID(t, s, own)}, 0.9))
	home, err := s.InsertNarrative(base, base.Add(72*time.Hour), "Home", "h")
	require.NoError(t, err)
	require.NoError(t, s.LinkMembers(home, []int64{eventID(t, s, later)}, 0.9))
	tiny := config.CorrelatorConfig{TailSummarizeThresholdTokens: 1, RecentEventsKept: 1}
	llm := &fakeLLM{}

	got, stats, err := correlator.Persist(t.Context(), s, llm, []correlator.ClusterResult{{
		Kind: correlator.ClusterExtends, NarrativeID: reader, Summary: "REWRITTEN AROUND THE OTHER STREAM",
		ContextEvents: []correlator.Event{later},
	}}, tiny)

	require.NoError(t, err)
	assert.Empty(t, llm.prompts, "no compaction: a context-only extend adds no member history")
	row, err := s.GetNarrative(reader)
	require.NoError(t, err)
	assert.Equal(t, "the reader's own summary", row.Summary, "no new work, so the summary is not rewritten")
	assert.True(t, base.Add(time.Hour).Equal(row.WindowEnd), "nor does window_end move to the context event")
	require.Len(t, got, 1)
	assert.Equal(t, "the reader's own summary", got[0].Summary)
	assert.Equal(t, store.LinkContext, linkKind(t, s, reader, later))
	assert.Equal(t, store.LinkMember, linkKind(t, s, home, later), "the member home is untouched")
	assert.Equal(t, 1, stats.ContextLinks)
}

func TestPersist_WindowsComeFromMemberEventsOnly(t *testing.T) {
	s := persistStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	old := seedPersistedEvent(t, s, "old", "weeks-old investigation", base.Add(-21*24*time.Hour))
	home, err := s.InsertNarrative(old.OccurredAt, old.OccurredAt, "Home", "h")
	require.NoError(t, err)
	require.NoError(t, s.LinkMembers(home, []int64{eventID(t, s, old)}, 0.9))
	fresh := seedPersistedEvent(t, s, "fresh", "today's fix", base)

	got, _, err := correlator.Persist(t.Context(), s, &fakeLLM{}, []correlator.ClusterResult{{
		Kind: correlator.ClusterNew, Title: "Fix", Summary: "s", Confidence: 0.9,
		Events: []correlator.Event{fresh}, ContextEvents: []correlator.Event{old},
	}}, roomyCorrelatorConfig)

	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.True(t, base.Equal(got[0].WindowStart),
		"a context event dated weeks earlier must not widen the window, or every narrative it supports "+
			"would overlap more windows and be hydrated into more prompts")
}

// TestPersist_AContextOnlyNewIsRejected is the spec's "a context-only NEW is
// rejected": a new narrative is its member work, so background alone cannot make one.
func TestPersist_AContextOnlyNewIsRejected(t *testing.T) {
	s := persistStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	e := seedPersistedEvent(t, s, "e", "work", base)
	home, err := s.InsertNarrative(base, base, "Home", "h")
	require.NoError(t, err)
	require.NoError(t, s.LinkMembers(home, []int64{eventID(t, s, e)}, 0.9))

	_, _, err = correlator.Persist(t.Context(), s, &fakeLLM{}, []correlator.ClusterResult{{
		Kind: correlator.ClusterNew, Title: "Only background", Summary: "s",
		ContextEvents: []correlator.Event{e},
	}}, roomyCorrelatorConfig)

	require.ErrorContains(t, err, `new narrative "Only background": it has no member events`)
}

// TestPersist_AContextOnlyEventFailsTheCommit: an event linked only as context would
// never be in any delta. The commit check refuses the whole pass — nothing persists.
func TestPersist_AContextOnlyEventFailsTheCommit(t *testing.T) {
	s := persistStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	work := seedPersistedEvent(t, s, "work", "work", base)
	homeless := seedPersistedEvent(t, s, "homeless", "nobody's work", base.Add(time.Minute))

	_, _, err := correlator.Persist(t.Context(), s, &fakeLLM{}, []correlator.ClusterResult{{
		Kind: correlator.ClusterNew, Title: "T", Summary: "s", Confidence: 0.9,
		Events: []correlator.Event{work}, ContextEvents: []correlator.Event{homeless},
	}}, roomyCorrelatorConfig)

	require.ErrorContains(t, err, "linked only as context, with no member home: claude_code/homeless")
	remaining, err := s.UnlinkedEventsInRange(base, base.Add(time.Hour))
	require.NoError(t, err)
	assert.Len(t, remaining, 2, "all-or-nothing: the member link rolled back too")
}

// TestPersist_ContextLinksDoNotDependOnResultOrder: E is A's eligible member; this
// pass moves it to a new narrative and leaves A holding it as background. A context
// link added while E is still A's member is a no-op (member wins), so applying the
// results in response order would lose A's link whenever the extend came first.
// Every member is placed before any context link is added.
func TestPersist_ContextLinksDoNotDependOnResultOrder(t *testing.T) {
	for _, extendFirst := range []bool{true, false} {
		name := "new first"
		if extendFirst {
			name = "extend first"
		}
		t.Run(name, func(t *testing.T) {
			s := persistStore(t)
			base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
			e := seedPersistedEvent(t, s, "e", "investigation", base)
			a, err := s.InsertNarrative(base, base, "A", "a")
			require.NoError(t, err)
			require.NoError(t, s.LinkMembers(a, []int64{eventID(t, s, e)}, 0.9))

			extend := correlator.ClusterResult{
				Kind: correlator.ClusterExtends, NarrativeID: a, ContextEvents: []correlator.Event{e},
			}
			newOne := correlator.ClusterResult{
				Kind: correlator.ClusterNew, Title: "X", Summary: "x", Confidence: 0.8, Events: []correlator.Event{e},
			}
			results := []correlator.ClusterResult{newOne, extend}
			if extendFirst {
				results = []correlator.ClusterResult{extend, newOne}
			}

			got, _, err := correlator.Persist(t.Context(), s, &fakeLLM{}, results, roomyCorrelatorConfig)

			require.NoError(t, err)
			x := got[0].ID
			if extendFirst {
				x = got[1].ID
			}
			assert.Equal(t, store.LinkMember, linkKind(t, s, x, e))
			assert.Equal(t, store.LinkContext, linkKind(t, s, a, e), "A keeps the event as background")
		})
	}
}

func TestPersist_RecordsConfidenceAndReportsSharing(t *testing.T) {
	s := persistStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	shared := seedPersistedEvent(t, s, "shared", "root investigation", base)
	one := seedPersistedEvent(t, s, "one", "fix one", base.Add(time.Minute))
	two := seedPersistedEvent(t, s, "two", "fix two", base.Add(2*time.Minute))
	cfg := roomyCorrelatorConfig
	cfg.MemberConfidenceFloor = 0.5

	_, stats, err := correlator.Persist(t.Context(), s, &fakeLLM{}, []correlator.ClusterResult{
		{
			Kind: correlator.ClusterNew, Title: "Investigation", Summary: "s", Confidence: 0.9,
			Events: []correlator.Event{shared},
		},
		{
			Kind: correlator.ClusterNew, Title: "Fix one", Summary: "s", Confidence: 0.4,
			Events: []correlator.Event{one}, ContextEvents: []correlator.Event{shared},
		},
		{
			Kind: correlator.ClusterNew, Title: "Fix two", Summary: "s", Confidence: 0.9,
			Events:           []correlator.Event{two},
			MemberConfidence: map[string]float64{correlator.EventKey(two): 0.3},
			ContextEvents:    []correlator.Event{shared},
		},
	}, cfg)

	require.NoError(t, err)
	assert.Equal(t, 2, stats.ContextLinks)
	assert.Equal(t, 1, stats.SharedEvents)
	assert.Equal(t, 2, stats.MaxContextFanOut)
	assert.Equal(t, 2, stats.MembersBelowFloor, "fix one at its cluster's 0.4, fix two at its override 0.3")

	for _, tc := range []struct {
		e    correlator.Event
		want float64
	}{{shared, 0.9}, {one, 0.4}, {two, 0.3}} {
		var home int64
		for _, n := range []int64{1, 2, 3} {
			if l, err := s.NarrativeEventLink(n, eventID(t, s, tc.e)); err == nil && l.Kind == store.LinkMember {
				home = n
				require.NotNil(t, l.MemberConfidence)
				assert.InDelta(t, tc.want, *l.MemberConfidence, 1e-9, tc.e.ExternalID)
			}
		}
		assert.NotZero(t, home, "%s has a member home", tc.e.ExternalID)
	}
}

// TestPersist_RefusesAnEventTwoResultsClaimAsWork: Cluster resolves double placements
// before Persist, so a duplicate here is a caller bug. Applying it would let the later
// MoveMember win — last-writer-wins, F37's mechanism — so it is refused before any
// write, and nothing persists.
func TestPersist_RefusesAnEventTwoResultsClaimAsWork(t *testing.T) {
	s := persistStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	e := seedPersistedEvent(t, s, "e", "work", base)

	_, _, err := correlator.Persist(t.Context(), s, &fakeLLM{}, []correlator.ClusterResult{
		{Kind: correlator.ClusterNew, Title: "first", Summary: "s", Confidence: 0.9, Events: []correlator.Event{e}},
		{Kind: correlator.ClusterNew, Title: "second", Summary: "s", Confidence: 0.9, Events: []correlator.Event{e}},
	}, roomyCorrelatorConfig)

	require.ErrorContains(t, err, `claude_code/e is a member of two results (new "first" and new "second")`)
	remaining, err := s.UnlinkedEventsInRange(base, base.Add(time.Hour))
	require.NoError(t, err)
	assert.Len(t, remaining, 1)
}

func TestPersist_RejectsAnExtendNamingNothing(t *testing.T) {
	s := persistStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	id, err := s.InsertNarrative(base, base, "T", "s")
	require.NoError(t, err)

	_, _, err = correlator.Persist(t.Context(), s, &fakeLLM{}, []correlator.ClusterResult{{
		Kind: correlator.ClusterExtends, NarrativeID: id,
	}}, roomyCorrelatorConfig)

	require.ErrorContains(t, err, "no member and no context event")
}
