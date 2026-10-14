package claudecode

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// usageLines decodes JSONL the way jsonlLines does, so numbers arrive as float64.
func usageLines(t *testing.T, raw ...string) []map[string]any {
	t.Helper()

	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		var l map[string]any
		require.NoError(t, json.Unmarshal([]byte(r), &l))
		out = append(out, l)
	}

	return out
}

func assistantLine(branch, ts, id string, output int) string {
	return fmt.Sprintf(`{"type":"assistant","gitBranch":%q,"timestamp":%q,"message":{"id":%q,"model":"m",`+
		`"usage":{"input_tokens":1,"output_tokens":%d}}}`, branch, ts, id, output)
}

func coverEach(i int) (string, bool) { return fmt.Sprintf("seg-%d", i), true }

func TestMessageUsages_ChargesASplitMessageToItsFirstSegment(t *testing.T) {
	segs := segments(usageLines(t,
		assistantLine("a", "2026-09-01T10:00:00Z", "msg_1", 3),
		assistantLine("b", "2026-09-01T10:01:00Z", "msg_1", 120),
	), 1)
	require.Len(t, segs, 2, "the two lines sit in two segments")

	rows, unledgered, err := messageUsages(transcript{id: "s1"}, segs, coverEach, time.Time{})
	require.NoError(t, err)

	require.Len(t, rows, 1)
	assert.Equal(t, "seg-0", rows[0].CoveringExternalID)
	assert.Equal(t, int64(120), rows[0].OutputTokens, "counters still take the MAX over every line")
	assert.True(t, rows[0].OccurredAt.Equal(time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)))
	assert.Zero(t, unledgered)
}

func TestMessageUsages_ASplitMessageWhoseFirstSegmentIsNotLedgeredGetsNoRow(t *testing.T) {
	segs := segments(usageLines(t,
		assistantLine("a", "2026-09-01T10:00:00Z", "msg_1", 3),
		assistantLine("b", "2026-09-01T10:01:00Z", "msg_1", 120),
	), 1)
	firstExcluded := func(i int) (string, bool) { return "seg", i != 0 }

	rows, _, err := messageUsages(transcript{id: "s1"}, segs, firstExcluded, time.Time{})
	require.NoError(t, err)

	assert.Empty(t, rows)
}

func TestMessageUsages_CountsAssistantLinesWithoutIDOrUsage(t *testing.T) {
	segs := segments(usageLines(t,
		`{"type":"assistant","timestamp":"2026-09-01T10:00:00Z","message":{"model":"m","usage":{"output_tokens":1}}}`,
		`{"type":"assistant","timestamp":"2026-09-01T10:00:01Z","message":{"id":"msg_1","model":"m"}}`,
		`{"type":"user","timestamp":"2026-09-01T10:00:02Z","message":{"content":"not an assistant line"}}`,
	), 1)

	rows, unledgered, err := messageUsages(transcript{id: "s1"}, segs, coverEach, time.Time{})
	require.NoError(t, err)

	assert.Empty(t, rows)
	assert.Equal(t, 2, unledgered)
}

func TestMessageUsages_MalformedCounters(t *testing.T) {
	for name, tc := range map[string]struct {
		usage   string
		wantErr string
		want    int64
	}{
		"absent is zero":         {usage: `{"input_tokens":1}`},
		"null is zero":           {usage: `{"input_tokens":1,"output_tokens":null}`},
		"a string errors":        {usage: `{"input_tokens":1,"output_tokens":"7"}`, wantErr: `"output_tokens"`},
		"an object errors":       {usage: `{"input_tokens":1,"output_tokens":{"n":7}}`, wantErr: `"output_tokens"`},
		"a fraction errors":      {usage: `{"input_tokens":1,"output_tokens":7.5}`, wantErr: `"output_tokens"`},
		"a negative errors":      {usage: `{"input_tokens":1,"output_tokens":-7}`, wantErr: `"output_tokens"`},
		"an integral float is n": {usage: `{"input_tokens":1,"output_tokens":7}`, want: 7},
	} {
		t.Run(name, func(t *testing.T) {
			segs := segments(usageLines(t, `{"type":"assistant","timestamp":"2026-09-01T10:00:00Z",`+
				`"message":{"id":"msg_1","model":"m","usage":`+tc.usage+`}}`), 1)

			rows, _, err := messageUsages(transcript{id: "s1"}, segs, coverEach, time.Time{})

			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				assert.ErrorContains(t, err, "msg_1")

				return
			}
			require.NoError(t, err)
			require.Len(t, rows, 1)
			assert.Equal(t, tc.want, rows[0].OutputTokens)
		})
	}
}
