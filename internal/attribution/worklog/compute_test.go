package worklog

import (
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/attribution"
	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/store/storetest"
)

func at(agent string, h, m, s int) store.MessageAttribution {
	return store.MessageAttribution{MessageUsage: store.MessageUsage{
		MessageID: "msg_" + agent, TranscriptID: "tr-1", AgentID: agent,
		OccurredAt: storetest.UTC(h, m, s),
	}}
}

func TestActiveSeconds(t *testing.T) {
	tests := []struct {
		name string
		msgs []store.MessageAttribution
		want int
	}{
		{"contiguous", []store.MessageAttribution{at("", 9, 0, 0), at("", 9, 5, 0), at("", 9, 10, 0)}, 600},
		{"idle gap excluded", []store.MessageAttribution{at("", 9, 0, 0), at("", 9, 10, 0), at("", 9, 40, 0), at("", 9, 45, 0)}, 900},
		{"subagent adds none", []store.MessageAttribution{at("", 9, 0, 0), at("a1", 9, 5, 0), at("", 9, 10, 0)}, 600},
		{"exactly idle gap counts", []store.MessageAttribution{at("", 9, 0, 0), at("", 9, 15, 0)}, 900},
		{"over idle gap does not", []store.MessageAttribution{at("", 9, 0, 0), at("", 9, 15, 1)}, 0},
		{"unsorted", []store.MessageAttribution{at("", 9, 45, 0), at("", 9, 0, 0), at("", 9, 40, 0), at("", 9, 10, 0)}, 900},
		{"one parent", []store.MessageAttribution{at("", 9, 0, 0), at("a1", 9, 5, 0)}, 0},
		{"none", nil, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, activeSeconds(tt.msgs, DefaultIdleGap))
		})
	}
}

func TestWorklogSeconds(t *testing.T) {
	assert.Equal(t, minWorklogSeconds, worklogSeconds(nil, DefaultIdleGap))
	assert.Equal(t, 60, minWorklogSeconds)
	assert.Equal(t, 600, worklogSeconds([]store.MessageAttribution{at("", 9, 0, 0), at("", 9, 10, 0)}, DefaultIdleGap))
	assert.Equal(t, 60, worklogSeconds([]store.MessageAttribution{at("", 9, 0, 0), at("", 9, 0, 30)}, DefaultIdleGap))
}

func TestStarted(t *testing.T) {
	msgs := []store.MessageAttribution{at("", 9, 10, 0), at("a1", 8, 59, 0), at("", 9, 0, 0)}
	assert.Equal(t, storetest.UTC(8, 59, 0), started(msgs))
}

func TestHumanCount(t *testing.T) {
	for n, want := range map[int64]string{
		0: "0", 999: "999", 1000: "1k", 1234: "1.2k", 45000: "45k",
		999949: "999.9k", 999950: "1M", 1200000: "1.2M", 3000000: "3M",
	} {
		assert.Equal(t, want, humanCount(n), "n=%d", n)
	}
}

func twoModels() map[string]attribution.ModelTotal {
	return map[string]attribution.ModelTotal{
		"claude-sonnet-5-5": {
			Input: 100000, Output: 5000, CacheWrite: 0, CacheRead: 0, Cost: 0.31,
			Price: config.ModelPrice{Input: new(3.0), Output: new(15.0), CacheWrite: new(3.75), CacheRead: new(0.3)},
		},
		"claude-opus-5-5": {
			Input: 600000, Output: 40000, CacheWrite: 200000, CacheRead: 300000, Cost: 3.10,
			Price: config.ModelPrice{Input: new(5.0), Output: new(25.0), CacheWrite: new(6.25), CacheRead: new(0.5)},
		},
	}
}

func TestComment(t *testing.T) {
	got := comment(twoModels())
	assert.Equal(t, "unjira: 1.2M in / 45k out, $3.41 (claude-opus-5-5 $3.10, claude-sonnet-5-5 $0.31)", got)

	tie := map[string]attribution.ModelTotal{"b": {Output: 1, Cost: 1}, "a": {Output: 1, Cost: 1}}
	assert.Equal(t, "unjira: 0 in / 2 out, $2.00 (a $1.00, b $1.00)", comment(tie))
	for _, s := range []string{"msg_", "tr-1"} {
		assert.NotContains(t, got, s)
	}
}

func TestProperty(t *testing.T) {
	orig := unjiraVersion
	unjiraVersion = "v1.2.3"
	t.Cleanup(func() { unjiraVersion = orig })

	raw, err := property("unjira-abc", []string{"m1", "m2"}, twoModels())
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &got))
	want := map[string]any{
		"version":         1.0,
		"idempotency_key": "unjira-abc",
		"message_ids":     []any{"m1", "m2"},
		"unjira_version":  "v1.2.3",
		"total_cost_usd":  3.41,
		"models": map[string]any{
			"claude-opus-5-5": map[string]any{
				"input": 600000.0, "output": 40000.0, "cache_write": 200000.0, "cache_read": 300000.0,
				"cost_usd": 3.10,
				"rates":    map[string]any{"input": 5.0, "output": 25.0, "cache_write": 6.25, "cache_read": 0.5},
			},
			"claude-sonnet-5-5": map[string]any{
				"input": 100000.0, "output": 5000.0, "cache_write": 0.0, "cache_read": 0.0,
				"cost_usd": 0.31,
				"rates":    map[string]any{"input": 3.0, "output": 15.0, "cache_write": 3.75, "cache_read": 0.3},
			},
		},
	}
	assert.Empty(t, cmp.Diff(want, got, cmpopts.EquateApprox(0, 1e-9)))
	assert.NotContains(t, raw, "agent")
}

func TestIdempotencyKey(t *testing.T) {
	base := idempotencyKey("add", "PROJ-1", "", []string{"a", "b"})
	assert.Regexp(t, `^unjira-[0-9a-f]{64}$`, base)
	assert.Equal(t, base, idempotencyKey("add", "PROJ-1", "", []string{"b", "a"}))
	for _, other := range []string{
		idempotencyKey("update", "PROJ-1", "", []string{"a", "b"}),
		idempotencyKey("add", "PROJ-1", "10", []string{"a", "b"}),
		idempotencyKey("add", "PROJ-2", "", []string{"a", "b"}),
		idempotencyKey("add", "PROJ-1", "", []string{"a"}),
		idempotencyKey("add", "PROJ-1", "", []string{"a", "c"}),
	} {
		assert.NotEqual(t, base, other)
	}
}
