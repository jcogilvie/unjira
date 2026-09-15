package correlator_test

// announce_test.go covers the F17 fix in the correlator: the pipeline's slowest step now
// says what it is about to do, before it does it.
//
// The concrete failure it answers, from a real session: a 90-day window built a ~147k
// token prompt, ran ~18 minutes, and died on a 504 having printed nothing and persisted
// nothing. Asked "has something changed?", neither reviewer nor agent could tell running
// from hung without querying SQLite. Two durations were then reported from feel and both
// were wrong, because nothing printed a stage boundary to count.
//
// The assertion that matters is ORDERING — after the call, this information is already in
// the stage summary; its whole value is being readable while the call is still running.

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/llm"
	"github.com/jcogilvie/unjira/internal/logging"
)

// announceWindow is the window every test here clusters over, and announceEvents dates
// its events inside it. Cluster filters to the window before building the prompt, so
// events outside it produce an empty assignable set and a parser error about index range
// rather than anything to do with logging.
func announceWindow() correlator.TimeRange {
	end := time.Now()

	return correlator.TimeRange{Start: end.Add(-time.Hour), End: end}
}

// announceEvents builds n distinct unlinked events inside announceWindow.
func announceEvents(n int) []correlator.Event {
	base := time.Now().Add(-30 * time.Minute)

	out := make([]correlator.Event, 0, n)
	for i := range n {
		e := events.NewEvent("claude_code",
			"ann:"+string(rune('a'+i)), base.Add(time.Duration(i)*time.Second), "did some work")
		out = append(out, e)
	}

	return out
}

// announceLLM records the buffer contents at the moment Complete is entered, which is how
// the ordering assertion is made without depending on log timestamps.
type announceLLM struct {
	response  string
	buf       *bytes.Buffer
	atCall    string
	callCount int
}

func (f *announceLLM) Complete(_ context.Context, _, _ string) (string, llm.Usage, error) {
	f.callCount++
	if f.buf != nil && f.atCall == "" {
		f.atCall = f.buf.String()
	}

	return f.response, llm.Usage{}, nil
}

func announceLogger(t *testing.T, buf *bytes.Buffer) *correlator.ClusterOption {
	t.Helper()

	log, err := logging.New(logging.Options{Format: "json", Level: "info", Out: buf})
	require.NoError(t, err)

	opt := correlator.WithLogger(log)

	return &opt
}

// TestCluster_AnnouncesBeforeTheCall is the finding. The line has to be emitted before
// client.Complete, because an operator reads it WHILE waiting.
func TestCluster_AnnouncesBeforeTheCall(t *testing.T) {
	var buf bytes.Buffer

	fake := &announceLLM{
		response: `[{"kind":"new","title":"t","summary":"s","event_indices":[0,1,2]}]`,
		buf:      &buf,
	}

	_, _, err := correlator.Cluster(
		t.Context(), announceEvents(3), nil, fake,
		announceWindow(),
		200000,
		*announceLogger(t, &buf),
	)
	require.NoError(t, err)

	require.Equal(t, 1, fake.callCount)
	require.NotEmpty(t, fake.atCall,
		"the log must already be written when Complete is entered; emitting it afterwards "+
			"duplicates the stage summary and helps nobody who is waiting")

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(fake.atCall), &got))

	assert.Equal(t, "clustering", got["msg"])
	assert.Equal(t, "correlator", got["component"])
	assert.InDelta(t, 3, got["unlinked_events"], 0,
		"the unlinked count is what the pass summary also reports, so the two must agree")
	assert.InDelta(t, 3, got["assignable_events"], 0,
		"and assignable is separate: with no context narratives it equals unlinked, but on a "+
			"mature store it is larger because context narratives' events are reshufflable")
	assert.Positive(t, got["est_tokens"],
		"the token estimate is the number that predicts the wait, and the only one a caller "+
			"cannot compute for itself — buildClusterPrompt is unexported")
}

// TestCluster_AnnouncesTheContextCount pins the number that explained the real slowdown.
// 16 candidates cost 104k prompt tokens because 52 narratives were hydrated as context;
// without that count the cost looks like a bug rather than the price of context.
func TestCluster_AnnouncesTheContextCount(t *testing.T) {
	var buf bytes.Buffer

	existing := []correlator.Narrative{
		{ID: 1, Title: "prior work", Summary: "s", WindowStart: time.Now().Add(-2 * time.Hour), WindowEnd: time.Now()},
		{ID: 2, Title: "more prior work", Summary: "s", WindowStart: time.Now().Add(-2 * time.Hour), WindowEnd: time.Now()},
	}

	_, _, err := correlator.Cluster(
		t.Context(), announceEvents(1), existing,
		&announceLLM{response: `[{"kind":"new","title":"t","summary":"s","event_indices":[0]}]`},
		announceWindow(),
		200000,
		*announceLogger(t, &buf),
	)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &got))

	assert.InDelta(t, 2, got["context_narratives"], 0,
		"context narratives dominate the prompt on a mature store, so omitting them makes a "+
			"104k-token call for 16 events unexplainable")
}

// TestCluster_WithoutALoggerStaysSilent keeps the seam optional. Every existing caller
// and test passes no logger, and none of them should need updating or start emitting.
func TestCluster_WithoutALoggerStaysSilent(t *testing.T) {
	_, _, err := correlator.Cluster(
		t.Context(), announceEvents(1), nil,
		&announceLLM{response: `[{"kind":"new","title":"t","summary":"s","event_indices":[0]}]`},
		announceWindow(),
		200000,
	)

	require.NoError(t, err, "a nil logger must not be a special case at the call site")
}
