// Package worklog is the attribution sink that drafts Jira worklogs from a ticket's
// attributed token ledger messages.
package worklog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jcogilvie/unjira/internal/attribution"
	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/store"
)

// DefaultIdleGap is worklog.idle_gap when unset: a longer gap between parent-session
// messages is idle time and logs nothing (D-23).
const DefaultIdleGap = 15 * time.Minute

// minWorklogSeconds is Jira's minimum accepted duration, measured on DEVSBX 2026-10-23:
// 1s and 30s were rejected with a 400, 60s was accepted.
const minWorklogSeconds = 60

// unjiraVersion is the build's module version, recorded in the worklog property. A
// var so tests can pin it.
var unjiraVersion = buildVersion()

func buildVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "unknown"
}

// activeSeconds sums the gaps of at most idleGap between consecutive parent-session
// messages (AgentID empty). Subagent messages add tokens, not time (D-6).
func activeSeconds(msgs []store.MessageAttribution, idleGap time.Duration) int {
	var times []time.Time
	for _, m := range msgs {
		if m.AgentID == "" {
			times = append(times, m.OccurredAt)
		}
	}
	slices.SortFunc(times, time.Time.Compare)
	var active time.Duration
	for i := 1; i < len(times); i++ {
		if gap := times[i].Sub(times[i-1]); gap <= idleGap {
			active += gap
		}
	}
	return int(active / time.Second)
}

// worklogSeconds is the time spent to log: active time, floored at Jira's minimum.
func worklogSeconds(msgs []store.MessageAttribution, idleGap time.Duration) int {
	return max(activeSeconds(msgs, idleGap), minWorklogSeconds)
}

// started is the earliest covered message, subagent messages included.
func started(msgs []store.MessageAttribution) time.Time {
	var first time.Time
	for _, m := range msgs {
		if first.IsZero() || m.OccurredAt.Before(first) {
			first = m.OccurredAt
		}
	}
	return first
}

// humanCount renders n with at most one decimal and a k or M suffix.
func humanCount(n int64) string {
	if n < 1000 {
		return strconv.FormatInt(n, 10)
	}
	tenths := func(div float64) string {
		return strings.TrimSuffix(strconv.FormatFloat(math.Round(float64(n)/div)/10, 'f', 1, 64), ".0")
	}
	if math.Round(float64(n)/100) < 10000 {
		return tenths(100) + "k"
	}
	return tenths(1e5) + "M"
}

// byCost orders model names by cost descending, then by name.
func byCost(totals map[string]attribution.ModelTotal) []string {
	return slices.SortedFunc(maps.Keys(totals), func(a, b string) int {
		if c := totals[b].Cost - totals[a].Cost; c != 0 {
			if c > 0 {
				return 1
			}
			return -1
		}
		return strings.Compare(a, b)
	})
}

// comment is the worklog's visible text (D-27): a deterministic template over the
// totals, with no model call and no transcript text.
func comment(totals map[string]attribution.ModelTotal) string {
	var in, out int64
	var cost float64
	parts := make([]string, 0, len(totals))
	for _, model := range byCost(totals) {
		t := totals[model]
		in += t.Input + t.CacheWrite + t.CacheRead
		out += t.Output
		cost += t.Cost
		parts = append(parts, fmt.Sprintf("%s $%.2f", model, t.Cost))
	}
	return fmt.Sprintf("unjira: %s in / %s out, $%.2f (%s)",
		humanCount(in), humanCount(out), cost, strings.Join(parts, ", "))
}

type propertyRates struct {
	Input      *float64 `json:"input"`
	Output     *float64 `json:"output"`
	CacheWrite *float64 `json:"cache_write"`
	CacheRead  *float64 `json:"cache_read"`
}

type propertyModel struct {
	Input      int64         `json:"input"`
	Output     int64         `json:"output"`
	CacheWrite int64         `json:"cache_write"`
	CacheRead  int64         `json:"cache_read"`
	CostUSD    float64       `json:"cost_usd"`
	Rates      propertyRates `json:"rates"`
}

type propertyValue struct {
	Version        int                      `json:"version"`
	IdempotencyKey string                   `json:"idempotency_key"`
	MessageIDs     []string                 `json:"message_ids"`
	UnjiraVersion  string                   `json:"unjira_version"`
	Models         map[string]propertyModel `json:"models"`
	TotalCostUSD   float64                  `json:"total_cost_usd"`
}

// property is the JSON value of the worklog's unjira.attribution property (D-24, D-25).
// It lists the covered message ids so an add can find time Jira already holds under
// another key (FindWorklog).
func property(key string, ids []string, totals map[string]attribution.ModelTotal) (string, error) {
	v := propertyValue{
		Version: 1, IdempotencyKey: key, MessageIDs: ids, UnjiraVersion: unjiraVersion,
		Models: make(map[string]propertyModel, len(totals)),
	}
	for model, t := range totals {
		v.Models[model] = propertyModel{
			Input: t.Input, Output: t.Output, CacheWrite: t.CacheWrite, CacheRead: t.CacheRead,
			CostUSD: t.Cost, Rates: rates(t.Price),
		}
		v.TotalCostUSD += t.Cost
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("marshal worklog property: %w", err)
	}
	return string(b), nil
}

func rates(p config.ModelPrice) propertyRates {
	return propertyRates{Input: p.Input, Output: p.Output, CacheWrite: p.CacheWrite, CacheRead: p.CacheRead}
}

// idempotencyKey identifies one write: sha256 over op, issue, worklog id and the sorted
// message ids, each terminated by NUL.
func idempotencyKey(op, issue, worklogID string, ids []string) string {
	h := sha256.New()
	for _, s := range append([]string{op, issue, worklogID}, slices.Sorted(slices.Values(ids))...) {
		h.Write([]byte(s + "\x00"))
	}
	return "unjira-" + hex.EncodeToString(h.Sum(nil))
}
