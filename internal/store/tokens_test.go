package store_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/require"

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
