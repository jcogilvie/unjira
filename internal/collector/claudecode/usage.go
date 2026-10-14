package claudecode

// usage.go extracts the token ledger: what each assistant message.id cost, from the
// lines a transcript owns. Extraction only. Which ticket a message is charged to is
// joined at read time in the store, never decided here.

import (
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
func messageUsages(
	t transcript, segs []segment, cover segmentCover, mtime time.Time,
) (rows []store.MessageUsage, unledgered int) {
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

			occurredAt := mtime
			if parsed, ok := parseTimestamp(stringField(line, "timestamp")); ok {
				occurredAt = parsed
			}

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
			r.InputTokens = max(r.InputTokens, usageCount(usage, "input_tokens"))
			r.OutputTokens = max(r.OutputTokens, usageCount(usage, "output_tokens"))
			r.CacheWriteTokens = max(r.CacheWriteTokens, usageCount(usage, "cache_creation_input_tokens"))
			r.CacheReadTokens = max(r.CacheReadTokens, usageCount(usage, "cache_read_input_tokens"))
		}
	}

	return rows, unledgered
}

// usageCount reads one usage counter. An absent counter is zero: usage objects omit the
// cache counters when there was no caching. A negative one is passed through, so the
// store's CHECK refuses it loudly rather than this clamping it.
func usageCount(usage map[string]any, key string) int64 {
	n, _ := usage[key].(float64)

	return int64(n)
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
