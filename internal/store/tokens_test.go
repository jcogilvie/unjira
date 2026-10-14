package store_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/store/storetest"
)

func TestUpsertMessageUsage(t *testing.T) {
	msg := storetest.MessageUsage
	// Two subagent transcripts holding the same id: A's line is earlier, so A owns it.
	inA := func() *storetest.MessageUsageBuilder {
		return msg("m1").Transcript("t-a", "agent-a").At(storetest.UTC(12, 0, 0)).
			Model("opus").Covering("ev-a").Tokens(1, 10, 0, 0)
	}
	inB := func() *storetest.MessageUsageBuilder {
		return msg("m1").Transcript("t-b", "agent-b").At(storetest.UTC(12, 0, 5)).
			Model("sonnet").Covering("ev-b").Tokens(1, 40, 0, 0)
	}
	ownedByA := inA().Tokens(1, 40, 0, 0).Build()

	tests := []struct {
		name   string
		passes [][]store.MessageUsage
		want   []store.MessageUsage
	}{
		{
			name:   "the same row twice is one row",
			passes: [][]store.MessageUsage{{inA().Build()}, {inA().Build()}},
			want:   []store.MessageUsage{inA().Build()},
		},
		{
			name: "usage that grows gives the larger",
			passes: [][]store.MessageUsage{{
				msg("m1").Tokens(1, 10, 2, 3).Build(), msg("m1").Tokens(1, 40, 2, 3).Build(),
			}},
			want: []store.MessageUsage{msg("m1").Tokens(1, 40, 2, 3).Build()},
		},
		{
			name: "usage that shrinks keeps the larger, per counter",
			passes: [][]store.MessageUsage{{
				msg("m1").Tokens(5, 40, 0, 9).Build(), msg("m1").Tokens(7, 10, 4, 0).Build(),
			}},
			want: []store.MessageUsage{msg("m1").Tokens(7, 40, 4, 9).Build()},
		},
		{
			name: "the covering id moves to a newer snapshot's id",
			passes: [][]store.MessageUsage{
				{msg("m1").Covering("ev-old").Build()}, {msg("m1").Covering("ev-new").Build()},
			},
			want: []store.MessageUsage{msg("m1").Covering("ev-new").Build()},
		},
		{
			name: "a NULL covering id does not erase a set one",
			passes: [][]store.MessageUsage{
				{msg("m1").Covering("ev-1").Build()}, {msg("m1").Build()},
			},
			want: []store.MessageUsage{msg("m1").Covering("ev-1").Build()},
		},
		{
			name: "occurred_at keeps the earliest within the holder",
			passes: [][]store.MessageUsage{{
				msg("m1").At(storetest.UTC(12, 0, 9)).Build(), msg("m1").At(storetest.UTC(12, 0, 1)).Build(),
			}},
			want: []store.MessageUsage{msg("m1").At(storetest.UTC(12, 0, 1)).Build()},
		},
		{
			name:   "two transcripts in one pass, A then B: A owns, B raises counters",
			passes: [][]store.MessageUsage{{inA().Build(), inB().Build()}},
			want:   []store.MessageUsage{ownedByA},
		},
		{
			name:   "two transcripts in one pass, B then A: A still owns",
			passes: [][]store.MessageUsage{{inB().Build(), inA().Build()}},
			want:   []store.MessageUsage{ownedByA},
		},
		{
			name:   "B in an earlier pass, A in a later one: A owns",
			passes: [][]store.MessageUsage{{inB().Build()}, {inA().Build()}},
			want:   []store.MessageUsage{ownedByA},
		},
		{
			name: "a later holder's NULL covering id is not the owner's",
			passes: [][]store.MessageUsage{
				{inA().Build()}, {inB().Covering("").Build()},
			},
			want: []store.MessageUsage{ownedByA},
		},
		{
			name: "a takeover with a NULL covering id clears the old holder's",
			passes: [][]store.MessageUsage{
				{inB().Build()}, {inA().Covering("").Build()},
			},
			want: []store.MessageUsage{inA().Covering("").Tokens(1, 40, 0, 0).Build()},
		},
		{
			name: "same occurred_at: the smaller transcript id owns, in either order",
			passes: [][]store.MessageUsage{
				{inB().At(storetest.UTC(12, 0, 0)).Build()}, {inA().Build()},
			},
			want: []store.MessageUsage{ownedByA},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := openStore(t)
			for _, pass := range tt.passes {
				require.NoError(t, s.UpsertMessageUsage(pass))
			}

			got, err := s.MessageUsageRows()
			require.NoError(t, err)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("token_messages (-want +got):\n%s", diff)
			}
		})
	}
}

func TestUpsertMessageUsage_LargeBatchInOneTransaction(t *testing.T) {
	s := openStore(t)
	rows := make([]store.MessageUsage, 4500)
	for i := range rows {
		rows[i] = storetest.MessageUsage(fmt.Sprintf("m%04d", i)).Tokens(1, 2, 3, 4).Build()
	}

	start := time.Now()
	require.NoError(t, s.UpsertMessageUsage(rows))
	t.Logf("4500 rows in %s", time.Since(start))

	got, err := s.MessageUsageRows()
	require.NoError(t, err)
	require.Len(t, got, 4500)
}

func TestRecordLLMCall(t *testing.T) {
	s := openStore(t)
	cached := int64(30)
	calls := []store.LLMCall{
		{
			CalledAt: storetest.Pacific(9, 1, 2, 345), Stage: "narrate", Model: "m",
			PromptTokens: 100, CompletionTokens: 20, CachedTokens: &cached,
		},
		{CalledAt: storetest.UTC(1, 0, 0), Stage: "match", Model: "m", PromptTokens: 5, CompletionTokens: 1},
	}
	for _, c := range calls {
		require.NoError(t, s.RecordLLMCall(c))
	}

	got, err := s.LLMCallRows()
	require.NoError(t, err)
	// Times scan back in UTC; the instant is what must be exact.
	if diff := cmp.Diff(calls, got, cmp.Comparer(func(a, b time.Time) bool { return a.Equal(b) })); diff != "" {
		t.Errorf("llm_calls (-want +got):\n%s", diff)
	}
}

func TestMessageAttributions(t *testing.T) {
	ev, nar, msg := storetest.Event, storetest.Narrative, storetest.MessageUsage
	primary := func(narrative, key string, confidence float64) primaryLink {
		return primaryLink{narrative: narrative, key: key, confidence: confidence}
	}
	// One ticket per narrative is the common shape: N1 holds A-1, N2 holds B-2.
	twoTickets := storetest.Fixture{
		Events: []events.Event{ev("ev-1").Build(), ev("ev-2").Build()},
		Narratives: []storetest.NarrativeSpec{
			nar("N1").Members("ev-1").Build(), nar("N2").Members("ev-2").Build(),
		},
	}
	twoPrimaries := []primaryLink{primary("N1", "A-1", 0.9), primary("N2", "B-2", 0.9)}

	tests := []struct {
		name      string
		fixture   storetest.Fixture
		primaries []primaryLink
		// context links each named event to the named narrative as context.
		context  map[string][]string
		messages []store.MessageUsage
		// then runs after the messages are ledgered, never re-upserting them.
		then  func(t *testing.T, s *store.Store, ids storetest.IDs)
		floor float64
		want  []wantAttribution
	}{
		{
			name: "a subagent's message follows its own event's member link, not the parent's",
			fixture: storetest.Fixture{
				Events: []events.Event{ev("ev-parent").Build(), ev("ev-sub").Build()},
				Narratives: []storetest.NarrativeSpec{
					nar("N-parent").Members("ev-parent").Build(), nar("N-sub").Members("ev-sub").Build(),
				},
			},
			primaries: []primaryLink{primary("N-parent", "A-1", 0.9), primary("N-sub", "B-2", 0.9)},
			messages: []store.MessageUsage{
				msg("m-parent").Covering("ev-parent").Tokens(1, 2, 3, 4).Build(),
				msg("m-sub").Transcript("t-sub", "agent-1").Covering("ev-sub").Build(),
			},
			want: []wantAttribution{
				{"m-parent", "N-parent", "A-1", store.AttributionTracked},
				{"m-sub", "N-sub", "B-2", store.AttributionTracked},
			},
		},
		{
			name:      "after MoveMember the message reports the new narrative's ticket",
			fixture:   twoTickets,
			primaries: twoPrimaries,
			messages:  []store.MessageUsage{msg("m1").Covering("ev-1").Build()},
			then: func(t *testing.T, s *store.Store, ids storetest.IDs) {
				t.Helper()
				require.NoError(t, s.WithTx(func(tx *store.Tx) error {
					return tx.MoveMember(ids["N2"], ids["ev-1"], 1, store.PlacedByModel)
				}))
			},
			want: []wantAttribution{{"m1", "N2", "B-2", store.AttributionTracked}},
		},
		{
			name:      "after the primary is relinked the message reports the new key",
			fixture:   twoTickets,
			primaries: twoPrimaries,
			messages:  []store.MessageUsage{msg("m1").Covering("ev-1").Build()},
			then: func(t *testing.T, s *store.Store, ids storetest.IDs) {
				t.Helper()
				require.NoError(t, s.RemoveNarrativeIssue(ids["N1"], "A-1"))
				require.NoError(t, s.AddNarrativeIssues(ids["N1"], []store.NarrativeIssue{{
					IssueKey: "A-9", Role: store.RolePrimary, Provenance: "reviewer", Confidence: 1, Connection: "corp",
				}}))
			},
			want: []wantAttribution{{"m1", "N1", "A-9", store.AttributionTracked}},
		},
		{
			name:     "a narrative with no primary is untracked, with no candidate key",
			fixture:  twoTickets,
			messages: []store.MessageUsage{msg("m1").Covering("ev-1").Build()},
			want:     []wantAttribution{{"m1", "N1", "", store.AttributionUntracked}},
		},
		{
			name:      "a primary below the floor is untracked, labeled with its candidate key",
			fixture:   twoTickets,
			primaries: []primaryLink{primary("N1", "A-1", 0.5), primary("N2", "B-2", 0.8)},
			messages: []store.MessageUsage{
				msg("m1").Covering("ev-1").Build(), msg("m2").Covering("ev-2").Build(),
			},
			floor: 0.8,
			want: []wantAttribution{
				{"m1", "N1", "A-1", store.AttributionUntracked},
				// AT the floor is confident, as matching promotes it.
				{"m2", "N2", "B-2", store.AttributionTracked},
			},
		},
		{
			name: "a covering event with only a context link, or no link, is unattributed",
			fixture: storetest.Fixture{
				Events:     []events.Event{ev("ev-1").Build(), ev("ev-ctx").Build(), ev("ev-bare").Build()},
				Narratives: []storetest.NarrativeSpec{nar("N1").Members("ev-1").Build()},
			},
			primaries: []primaryLink{primary("N1", "A-1", 0.9)},
			context:   map[string][]string{"N1": {"ev-ctx"}},
			messages: []store.MessageUsage{
				msg("m-bare").Covering("ev-bare").Build(), msg("m-ctx").Covering("ev-ctx").Build(),
			},
			want: []wantAttribution{
				{"m-bare", "", "", store.AttributionUnattributed},
				{"m-ctx", "", "", store.AttributionUnattributed},
			},
		},
		{
			name: "no covering id, an id never inserted, or another source's id is no_event",
			fixture: storetest.Fixture{
				Events:     []events.Event{ev("ev-jira").Source("jira").Build()},
				Narratives: []storetest.NarrativeSpec{nar("N1").Members("ev-jira").Build()},
			},
			primaries: []primaryLink{primary("N1", "A-1", 0.9)},
			messages: []store.MessageUsage{
				msg("m-jira").Covering("ev-jira").Build(),
				msg("m-missing").Covering("ev-never").Build(),
				msg("m-null").Build(),
			},
			want: []wantAttribution{
				{"m-jira", "", "", store.AttributionNoEvent},
				{"m-missing", "", "", store.AttributionNoEvent},
				{"m-null", "", "", store.AttributionNoEvent},
			},
		},
		{
			name: "only the row's covering snapshot counts, never an older snapshot's link",
			fixture: storetest.Fixture{
				Events: []events.Event{ev("t:100:0").Build(), ev("t:200:0").Build()},
				Narratives: []storetest.NarrativeSpec{
					nar("N-old").Members("t:100:0").Build(), nar("N-new").Members("t:200:0").Build(),
				},
			},
			primaries: []primaryLink{primary("N-old", "A-1", 0.9), primary("N-new", "B-2", 0.9)},
			messages:  []store.MessageUsage{msg("m1").Covering("t:200:0").Build()},
			want:      []wantAttribution{{"m1", "N-new", "B-2", store.AttributionTracked}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := openStore(t)
			ids := storetest.Seed(t, s, tt.fixture)
			for _, p := range tt.primaries {
				require.NoError(t, s.AddNarrativeIssues(ids[p.narrative], []store.NarrativeIssue{{
					IssueKey: p.key, Role: store.RolePrimary, Provenance: "branch", Confidence: p.confidence,
					Connection: "corp",
				}}))
			}
			for narrative, evs := range tt.context {
				for _, e := range evs {
					require.NoError(t, s.LinkContext(ids[narrative], []int64{ids[e]}))
				}
			}
			require.NoError(t, s.UpsertMessageUsage(tt.messages))
			if tt.then != nil {
				tt.then(t, s, ids)
			}

			byID := make(map[string]store.MessageUsage, len(tt.messages))
			for _, m := range tt.messages {
				byID[m.MessageID] = m
			}
			want := make([]store.MessageAttribution, 0, len(tt.want))
			for _, w := range tt.want {
				a := store.MessageAttribution{MessageUsage: byID[w.message], IssueKey: w.key, State: w.state}
				if w.narrative != "" {
					a.NarrativeID = ids[w.narrative]
				}
				if w.key != "" {
					a.Connection = "corp"
				}
				want = append(want, a)
			}

			got, err := s.MessageAttributions(tt.floor)
			require.NoError(t, err)
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("MessageAttributions (-want +got):\n%s", diff)
			}
		})
	}
}

type primaryLink struct {
	narrative, key string
	confidence     float64
}

// wantAttribution names a message's expected attribution by fixture names; the test
// fills in the row's usage, the narrative's id and the connection.
type wantAttribution struct {
	message, narrative, key string
	state                   store.AttributionState
}
