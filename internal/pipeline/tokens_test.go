package pipeline_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/pipeline"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/store/storetest"
)

// Rates per million: input 1, output 10, cache write 100, cache read 1000, so each
// kind's share of a cost is readable from its digits.
var tokenPricing = config.TokenPricing{
	"claude": {Input: new(1.0), Output: new(10.0), CacheWrite: new(100.0), CacheRead: new(1000.0)},
	"own":    {Input: new(2.0), Output: new(20.0), CacheWrite: new(200.0), CacheRead: new(0.5)},
}

func openTokenStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// seedAttribution: ev-a in N-a (primary A-1, 0.9), ev-low in N-low (primary L-1, 0.5,
// below the 0.8 floor), ev-none in N-none (no primary), ev-bare in no narrative.
func seedAttribution(t *testing.T, s *store.Store) storetest.IDs {
	t.Helper()
	ev, nar := storetest.Event, storetest.Narrative
	ids := storetest.Seed(t, s, storetest.Fixture{
		Events: []events.Event{ev("ev-a").Build(), ev("ev-low").Build(), ev("ev-none").Build(), ev("ev-bare").Build()},
		Narratives: []storetest.NarrativeSpec{
			nar("N-a").Members("ev-a").Build(), nar("N-low").Members("ev-low").Build(),
			nar("N-none").Members("ev-none").Build(),
		},
	})
	for narrative, p := range map[string]struct {
		key        string
		confidence float64
	}{"N-a": {"A-1", 0.9}, "N-low": {"L-1", 0.5}} {
		require.NoError(t, s.AddNarrativeIssues(ids[narrative], []store.NarrativeIssue{{
			IssueKey: p.key, Role: store.RolePrimary, Provenance: "branch", Confidence: p.confidence, Connection: "corp",
		}}))
	}
	return ids
}

func TestBuildTokenReport_Messages(t *testing.T) {
	s := openTokenStore(t)
	ids := seedAttribution(t, s)
	msg := storetest.MessageUsage
	require.NoError(t, s.UpsertMessageUsage([]store.MessageUsage{
		msg("m1").Model("claude").Covering("ev-a").Tokens(1, 2, 3, 4).Build(),
		msg("m2").Model("claude").Covering("ev-a").Tokens(10, 0, 0, 0).Build(),
		msg("m3").Model("own").Covering("ev-a").Tokens(1, 0, 0, 0).Build(),
		msg("m4").Model("claude").Covering("ev-low").Tokens(0, 1, 0, 0).Build(),
		msg("m5").Model("claude").Covering("ev-none").Tokens(0, 0, 1, 0).Build(),
		msg("m6").Model("claude").Covering("ev-bare").Tokens(0, 0, 0, 1).Build(),
		msg("m7").Model("claude").Tokens(2, 0, 0, 0).Build(),
	}))

	got, err := pipeline.BuildTokenReport(s, tokenPricing, 0.8, storetest.UTC(12, 0, 0))
	require.NoError(t, err)

	claude := func(in, out, cw, cr int64) []pipeline.ModelTokens {
		return []pipeline.ModelTokens{{Model: "claude", Unlogged: pipeline.TokenAmount{
			Input: in, Output: out, CacheWrite: cw, CacheRead: cr,
			Cost: (float64(in) + float64(out)*10 + float64(cw)*100 + float64(cr)*1000) / 1e6,
		}}}
	}
	want := pipeline.TokenReport{
		Messages: 7,
		Tickets: []pipeline.TicketTokens{{Connection: "corp", IssueKey: "A-1", Models: append(claude(11, 2, 3, 4),
			pipeline.ModelTokens{Model: "own", Unlogged: pipeline.TokenAmount{Input: 1, Cost: 2 / 1e6}})}},
		Untracked: []pipeline.UntrackedTokens{
			{NarrativeID: ids["N-low"], CandidateKey: "L-1", Models: claude(0, 1, 0, 0)},
			{NarrativeID: ids["N-none"], Models: claude(0, 0, 1, 0)},
		},
		Unattributed: []pipeline.UnattributedTokens{
			{State: store.AttributionNoEvent, Messages: 1, Models: claude(2, 0, 0, 0)},
			{State: store.AttributionUnattributed, Messages: 1, Models: claude(0, 0, 0, 1)},
		},
	}
	if ids["N-low"] > ids["N-none"] {
		want.Untracked[0], want.Untracked[1] = want.Untracked[1], want.Untracked[0]
	}
	if diff := cmp.Diff(want, got, cmpopts.IgnoreFields(pipeline.TokenReport{}, "OwnSpend"),
		cmpopts.EquateApprox(0, 1e-12)); diff != "" {
		t.Errorf("report (-want +got):\n%s", diff)
	}
}

func TestBuildTokenReport_UnpricedModelErrorsNamingIt(t *testing.T) {
	for name, seed := range map[string]func(*store.Store) error{
		"a transcript model": func(s *store.Store) error {
			return s.UpsertMessageUsage([]store.MessageUsage{storetest.MessageUsage("m1").Model("mystery").Build()})
		},
		"the configured LLM model": func(s *store.Store) error {
			return s.RecordLLMCall(store.LLMCall{CalledAt: storetest.UTC(11, 0, 0), Stage: "match", Model: "mystery"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := openTokenStore(t)
			require.NoError(t, seed(s))

			_, err := pipeline.BuildTokenReport(s, tokenPricing, 0.8, storetest.UTC(12, 0, 0))

			require.Error(t, err)
			assert.Contains(t, err.Error(), `"mystery"`)
		})
	}
}

func TestBuildTokenReport_OwnSpendWindows(t *testing.T) {
	// Noon on 2026-10-14 in UTC-7: local midnight is 07:00 UTC.
	loc := time.FixedZone("UTC-7", -7*3600)
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, loc)
	cached := int64(40)
	call := func(at time.Time, cached *int64) store.LLMCall {
		return store.LLMCall{CalledAt: at, Stage: "match", Model: "own", PromptTokens: 100, CompletionTokens: 10, CachedTokens: cached}
	}
	// (100-40)*2 + 40*0.5 + 10*20 = 340; with nil cached: 100*2 + 10*20 = 400.
	const withCache, noCache = 340 / 1e6, 400 / 1e6
	midnight := time.Date(2026, 10, 14, 0, 0, 0, 0, loc)

	s := openTokenStore(t)
	for _, c := range []store.LLMCall{
		call(now.Add(-30*24*time.Hour-time.Second), &cached), // outside every window
		call(now.Add(-30*24*time.Hour+time.Second), &cached), // 30d
		call(now.Add(-7*24*time.Hour-time.Second), &cached),  // 30d
		call(now.Add(-7*24*time.Hour+time.Second), &cached),  // 30d, 7d
		call(now.Add(-24*time.Hour-time.Second), &cached),    // 30d, 7d
		call(now.Add(-24*time.Hour+time.Second), &cached),    // 30d, 7d, 1d
		call(midnight.Add(-time.Second), &cached),            // 30d, 7d, 1d
		call(midnight.Add(time.Second), nil),                 // all four
		call(now.Add(time.Second), &cached),                  // in the future: none
	} {
		require.NoError(t, s.RecordLLMCall(c))
	}

	got, err := pipeline.BuildTokenReport(s, tokenPricing, 0.8, now)
	require.NoError(t, err)

	window := func(name string, withCached int, upper bool) pipeline.OwnSpendWindow {
		calls := withCached
		cost := float64(withCached) * withCache
		if upper {
			calls++
			cost += noCache
		}
		return pipeline.OwnSpendWindow{
			Window: name, Calls: calls, PromptTokens: int64(100 * calls), CompletionTokens: int64(10 * calls),
			Cached: int64(40 * withCached), Cost: cost, UpperBound: upper,
		}
	}
	want := []pipeline.OwnSpendWindow{
		window("30d", 6, true), window("7d", 4, true), window("1d", 2, true), window("today", 0, true),
	}
	if diff := cmp.Diff(want, got.OwnSpend, cmpopts.EquateApprox(0, 1e-12)); diff != "" {
		t.Errorf("own spend (-want +got):\n%s", diff)
	}
}

func TestRenderTokens(t *testing.T) {
	report := pipeline.TokenReport{
		Messages: 3,
		Tickets: []pipeline.TicketTokens{{Connection: "corp", IssueKey: "A-1", Models: []pipeline.ModelTokens{
			{Model: "claude", Unlogged: pipeline.TokenAmount{Input: 1, Output: 2, CacheWrite: 3, CacheRead: 4, Cost: 0.0043}},
		}}},
		Untracked: []pipeline.UntrackedTokens{{NarrativeID: 7, CandidateKey: "L-1", Models: []pipeline.ModelTokens{
			{Model: "claude", Unlogged: pipeline.TokenAmount{Output: 1, Cost: 0.00001}},
		}}},
		Unattributed: []pipeline.UnattributedTokens{{State: store.AttributionNoEvent, Messages: 1}},
		OwnSpend: []pipeline.OwnSpendWindow{
			{Window: "30d", Calls: 2, PromptTokens: 200, Cached: 40, CompletionTokens: 20, Cost: 0.00074, UpperBound: true},
			{Window: "today"},
		},
	}

	want := `# unjira tokens

## Per ticket
A-1 (corp)
  claude  unlogged: in 1, out 2, cache write 3, cache read 4, $0.0043  logged: $0.0000  pending: $0.0000

## Untracked work, by narrative id
narrative 7 (candidate L-1, below the confidence floor)
  claude  unlogged: in 0, out 1, cache write 0, cache read 0, $0.0000  logged: $0.0000  pending: $0.0000

## Unattributed messages
no_event: 1 message(s)

## unjira's own LLM spend
  30d    2 call(s)  prompt 200 (cached 40)  completion 20  $0.0007 (upper bound)
  today  0 call(s)  prompt 0 (cached 0)  completion 0  $0.0000
  An upper bound: some calls reported no cached count, so all their prompt tokens are priced at the input rate.
`
	assert.Equal(t, want, pipeline.RenderTokens(report))
}

func TestRenderTokens_EmptyLedger(t *testing.T) {
	assert.Contains(t, pipeline.RenderTokens(pipeline.TokenReport{}), "No ledger rows yet. Run: unjira collect")
}
