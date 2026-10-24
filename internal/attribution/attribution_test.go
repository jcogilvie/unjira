package attribution_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/attribution"
	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/store"
)

func msg(id string, state store.AttributionState, issue, wlIssue, wlID string) store.MessageAttribution {
	return store.MessageAttribution{
		MessageUsage: store.MessageUsage{MessageID: id, Model: "m"},
		IssueKey:     issue, State: state,
		WorklogID: wlID, WorklogIssueKey: wlIssue,
	}
}

func TestBuildTicketStates(t *testing.T) {
	tracked := store.AttributionTracked
	act := store.ActionRow{ID: 7, Type: "worklog", IssueKey: "PROJ-1", Payload: `{}`}
	tests := []struct {
		name    string
		msgs    []store.MessageAttribution
		actions map[string][]store.ActionRow
		want    []attribution.TicketState
	}{
		{
			name: "tracked unmarked is unlogged",
			msgs: []store.MessageAttribution{msg("a", tracked, "PROJ-1", "", "")},
			want: []attribution.TicketState{{
				IssueKey: "PROJ-1",
				Unlogged: []store.MessageAttribution{msg("a", tracked, "PROJ-1", "", "")},
			}},
		},
		{
			name: "marked and still tracked is logged",
			msgs: []store.MessageAttribution{msg("a", tracked, "PROJ-1", "PROJ-1", "10001")},
			want: []attribution.TicketState{{
				IssueKey: "PROJ-1",
				Logged:   map[string][]store.MessageAttribution{"10001": {msg("a", tracked, "PROJ-1", "PROJ-1", "10001")}},
			}},
		},
		{
			name: "marked and now tracked elsewhere is moved on the logged ticket only",
			msgs: []store.MessageAttribution{msg("a", tracked, "PROJ-2", "PROJ-1", "10001")},
			want: []attribution.TicketState{{
				IssueKey: "PROJ-1",
				Moved:    map[string][]store.MessageAttribution{"10001": {msg("a", tracked, "PROJ-2", "PROJ-1", "10001")}},
			}},
		},
		{
			name: "marked and now untracked, unattributed or no_event is moved",
			msgs: []store.MessageAttribution{
				msg("a", store.AttributionUntracked, "PROJ-1", "PROJ-1", "10001"),
				msg("b", store.AttributionUnattributed, "", "PROJ-1", "10001"),
				msg("c", store.AttributionNoEvent, "", "PROJ-1", "10001"),
			},
			want: []attribution.TicketState{{
				IssueKey: "PROJ-1",
				Moved: map[string][]store.MessageAttribution{"10001": {
					msg("a", store.AttributionUntracked, "PROJ-1", "PROJ-1", "10001"),
					msg("b", store.AttributionUnattributed, "", "PROJ-1", "10001"),
					msg("c", store.AttributionNoEvent, "", "PROJ-1", "10001"),
				}},
			}},
		},
		{
			name: "unmarked non-tracked messages produce no state",
			msgs: []store.MessageAttribution{
				msg("a", store.AttributionUntracked, "PROJ-1", "", ""),
				msg("b", store.AttributionUnattributed, "", "", ""),
				msg("c", store.AttributionNoEvent, "", "", ""),
			},
		},
		{
			name:    "actions pass through unchanged, states sorted by key",
			msgs:    []store.MessageAttribution{msg("b", tracked, "PROJ-2", "", ""), msg("a", tracked, "PROJ-1", "", "")},
			actions: map[string][]store.ActionRow{"PROJ-1": {act}},
			want: []attribution.TicketState{
				{
					IssueKey: "PROJ-1", Unlogged: []store.MessageAttribution{msg("a", tracked, "PROJ-1", "", "")},
					Actions: []store.ActionRow{act},
				},
				{IssueKey: "PROJ-2", Unlogged: []store.MessageAttribution{msg("b", tracked, "PROJ-2", "", "")}},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := attribution.BuildTicketStates(tc.msgs, tc.actions)
			assert.Empty(t, cmp.Diff(tc.want, got), "-want +got")
		})
	}
}

func TestTotals(t *testing.T) {
	pa := config.ModelPrice{Input: new(float64(1)), Output: new(float64(2)), CacheWrite: new(float64(3)), CacheRead: new(float64(4))}
	pb := config.ModelPrice{Input: new(float64(10)), Output: new(float64(20)), CacheWrite: new(float64(30)), CacheRead: new(float64(40))}
	pricing := config.TokenPricing{"a": pa, "b": pb}
	u := func(model string, in, out, cw, cr int64) store.MessageAttribution {
		return store.MessageAttribution{MessageUsage: store.MessageUsage{
			Model:       model,
			InputTokens: in, OutputTokens: out, CacheWriteTokens: cw, CacheReadTokens: cr,
		}}
	}

	got, err := attribution.Totals([]store.MessageAttribution{
		u("a", 100, 200, 300, 400), u("a", 1, 2, 3, 4), u("b", 1000, 0, 0, 1000),
	}, pricing)
	require.NoError(t, err)
	want := map[string]attribution.ModelTotal{
		"a": {
			Input: 101, Output: 202, CacheWrite: 303, CacheRead: 404,
			Cost: (101*1 + 202*2 + 303*3 + 404*4) / 1e6, Price: pa,
		},
		"b": {Input: 1000, CacheRead: 1000, Cost: (1000*10 + 1000*40) / 1e6, Price: pb},
	}
	assert.Empty(t, cmp.Diff(want, got), "-want +got")

	_, err = attribution.Totals([]store.MessageAttribution{u("nope", 1, 0, 0, 0)}, pricing)
	_, lookupErr := pricing.Lookup("nope")
	require.EqualError(t, err, lookupErr.Error())
}
