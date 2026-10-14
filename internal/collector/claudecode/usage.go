package claudecode

// usage.go extracts the token ledger: what each assistant message.id cost, from the
// lines a transcript owns. Extraction only. Which ticket a message is charged to is
// joined at read time in the store, never decided here.

import (
	"fmt"
	"math"
	"time"

	"github.com/jcogilvie/unjira/internal/store"
)

// syntheticModel is the message.model Claude Code writes on assistant lines it generates
// itself, not from a model call. Measured over every transcript under ~/.claude/projects
// on 2026-10-14: 513 message ids carried it, and every one of their token counts was zero.
// They are skipped rather than ledgered, because an unpriced model is an error when the
// ledger is reported, and no user should have to price a model that does not exist.
const syntheticModel = "<synthetic>"

// segmentCover says what the ledger does with segment i's messages: externalID is the
// event the segment emits ("" when it emits none), and ledgered is false for a segment
// whose messages get no rows at all.
type segmentCover func(i int) (externalID string, ledgered bool)

// messageUsages returns one ledger row per assistant message.id in segs, in order of
// first appearance, plus the number of assistant lines that had no message.id or no
// usage and so yielded no row.
//
// A message's lines do not all carry the same usage: measured, 23% of ids differ across
// their lines, and the last line's counts are always the largest. So each counter is the
// MAX over the id's lines, and the store's upsert takes the MAX again across snapshots.
//
// A message is charged to the segment holding its first line, and only that segment's
// cover decides its covering event and whether it is ledgered. Measured, no id has lines
// in two segments, so this rule is a fallback that costs nothing. One row per id also
// means the store sees each id once per transcript, so a later segment can never
// overwrite the covering id.
//
// mtime dates a message whose lines carry no parseable timestamp, as sessionEvents
// dates such a segment.
//
// Only lines inside a segment are read. A line before any run opens that has neither a
// timestamp nor text never reaches a segment (rawRuns drops it), so its usage would be
// neither ledgered nor counted. Every Claude Code line carries a timestamp, so this
// assumes none such exists.
//
// A malformed counter is an error, not a zero: the cursor moves past these lines once the
// pass succeeds, so a counter read as 0 would stay 0 in the ledger for good.
func messageUsages(
	t transcript, segs []segment, cover segmentCover, mtime time.Time,
) (rows []store.MessageUsage, unledgered int, err error) {
	index := map[string]int{} // message id -> its row in rows, or -1 when not ledgered

	for i, seg := range segs {
		for _, line := range seg.factLines {
			if line["type"] != "assistant" {
				continue
			}
			message, _ := line["message"].(map[string]any)
			id, _ := message["id"].(string)
			usage, _ := message["usage"].(map[string]any)
			if id == "" || usage == nil {
				unledgered++

				continue
			}
			model, _ := message["model"].(string)
			if model == syntheticModel {
				continue
			}

			occurredAt := lineTime(line, mtime)

			at, seen := index[id]
			if !seen {
				externalID, ledgered := cover(i)
				if !ledgered {
					index[id] = -1

					continue
				}
				index[id] = len(rows)
				rows = append(rows, store.MessageUsage{
					MessageID:          id,
					TranscriptID:       t.id,
					AgentID:            agentID(t),
					OccurredAt:         occurredAt,
					Model:              model,
					CoveringExternalID: externalID,
				})
				at = len(rows) - 1
			}
			if at < 0 {
				continue
			}

			r := &rows[at]
			if occurredAt.Before(r.OccurredAt) {
				r.OccurredAt = occurredAt
			}
			if err := raiseCounters(r, usage); err != nil {
				return nil, 0, fmt.Errorf("message %s: %w", id, err)
			}
		}
	}

	return rows, unledgered, nil
}

// lineTime is line's timestamp, or fallback when it has none that parses.
func lineTime(line map[string]any, fallback time.Time) time.Time {
	if parsed, ok := parseTimestamp(stringField(line, "timestamp")); ok {
		return parsed
	}

	return fallback
}

// raiseCounters raises each of r's counters to usage's, if larger.
func raiseCounters(r *store.MessageUsage, usage map[string]any) error {
	for _, c := range []struct {
		key string
		dst *int64
	}{
		{"input_tokens", &r.InputTokens},
		{"output_tokens", &r.OutputTokens},
		{"cache_creation_input_tokens", &r.CacheWriteTokens},
		{"cache_read_input_tokens", &r.CacheReadTokens},
	} {
		n, err := usageCount(usage, c.key)
		if err != nil {
			return err
		}
		*c.dst = max(*c.dst, n)
	}

	return nil
}

// usageCount reads one usage counter. An absent or JSON null counter is zero: usage
// objects omit the cache counters when there was no caching. A non-number, a
// non-integral number or a negative one is an error. A negative one cannot be left to
// the store's CHECK: the MAX against a row's zero start would clamp it to 0 first.
func usageCount(usage map[string]any, key string) (int64, error) {
	v, present := usage[key]
	if !present || v == nil {
		return 0, nil
	}
	n, ok := v.(float64)
	if !ok || n != math.Trunc(n) || n < 0 {
		return 0, fmt.Errorf("usage counter %q is %v, not a non-negative integer", key, v)
	}

	return int64(n), nil
}

func stringField(line map[string]any, key string) string {
	s, _ := line[key].(string)

	return s
}

// agentID is a subagent transcript's agent id, or "" for a root session.
func agentID(t transcript) string {
	if t.sub == nil {
		return ""
	}

	return t.sub.agentID
}
