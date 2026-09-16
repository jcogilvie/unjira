package events_test

// partition_test.go covers PartitionByTrackerRecord, the entrance-side split this
// codebase previously compensated for at the exit.
//
// The measurement that motivated it: 82% of the clustering prompt's characters were
// tracker bookkeeping (96 jira events, 210,350 chars, versus 229 session events at
// 45,550), and 29 of 68 narratives contained nothing else — 26 of those linked to
// the single ticket they were derived FROM. Every one was correctly suppressed
// downstream by AnyWorkEvidence, so nothing wrong was written; the whole cycle was
// simply spent to arrive there.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/events"
)

// evt builds a minimal event, marked as a tracker record or not.
func evt(id string, trackerRecord bool) events.Event {
	e := events.NewEvent("jira", id, time.Now(), "summary for "+id)
	if trackerRecord {
		events.SetTrackerRecord(&e)
	}

	return e
}

func TestPartitionByTrackerRecord_SplitsBothWays(t *testing.T) {
	in := []events.Event{
		evt("work-1", false),
		evt("record-1", true),
		evt("work-2", false),
		evt("record-2", true),
	}

	work, records := events.PartitionByTrackerRecord(in)

	require.Len(t, work, 2)
	require.Len(t, records, 2)
	assert.Equal(t, "work-1", work[0].ExternalID)
	assert.Equal(t, "work-2", work[1].ExternalID)
	assert.Equal(t, "record-1", records[0].ExternalID)
	assert.Equal(t, "record-2", records[1].ExternalID)
}

// TestPartitionByTrackerRecord_ReturnsBothHalves is the never-silently-drop-data
// invariant. A filter that returned only the work half would make the excluded
// count unreportable, and an operator cannot tune what they cannot see.
func TestPartitionByTrackerRecord_ReturnsBothHalves(t *testing.T) {
	in := []events.Event{evt("a", false), evt("b", true), evt("c", true)}

	work, records := events.PartitionByTrackerRecord(in)

	assert.Len(t, work, 1)
	assert.Len(t, records, 2,
		"the excluded half must be returned, not dropped: RunNarrate reports the count, and "+
			"a silent exclusion reads as 'nothing was left out'")
	assert.Len(t, in, 3, "and the input must not be mutated")
}

// TestPartitionByTrackerRecord_PreservesOrder matters because clustering numbers its
// candidates positionally and gatherCandidates reads TicketKeysOf in order to tell a
// first mention from a later one. A partition that reordered would silently change
// provenance.
func TestPartitionByTrackerRecord_PreservesOrder(t *testing.T) {
	in := []events.Event{
		evt("w1", false), evt("r1", true), evt("w2", false), evt("r2", true), evt("w3", false),
	}

	work, records := events.PartitionByTrackerRecord(in)

	assert.Equal(t, []string{"w1", "w2", "w3"}, ids(work))
	assert.Equal(t, []string{"r1", "r2"}, ids(records))
}

// TestPartitionByTrackerRecord_WronglyTypedArtifactReadsAsWork pins agreement with
// IsTrackerRecord, whose doc comment states the direction deliberately: Artifacts
// round-trip through JSON in the store, so any value can arrive as any type, and an
// unmarked event counts as work evidence. That fails toward an over-eager proposal
// reaching a human rather than toward silence.
func TestPartitionByTrackerRecord_WronglyTypedArtifactReadsAsWork(t *testing.T) {
	e := events.NewEvent("jira", "bad-type", time.Now(), "s")
	e.Artifacts[events.ArtifactTrackerRecord] = "true" // a string, not a bool

	work, records := events.PartitionByTrackerRecord([]events.Event{e})

	assert.Len(t, work, 1,
		"a wrongly-typed marker must read as absence, matching IsTrackerRecord")
	assert.Empty(t, records)
}

// TestPartitionByTrackerRecord_DegenerateInputs keeps callers from needing a
// special case before calling.
func TestPartitionByTrackerRecord_DegenerateInputs(t *testing.T) {
	work, records := events.PartitionByTrackerRecord(nil)
	assert.Empty(t, work)
	assert.Empty(t, records)

	work, records = events.PartitionByTrackerRecord([]events.Event{})
	assert.Empty(t, work)
	assert.Empty(t, records)
}

// TestPartitionByTrackerRecord_AgreesWithAnyWorkEvidence ties the entrance filter to
// the exit filter it makes redundant. If these two ever disagree, one of them is
// wrong — and the exit one has shipped behaviour depending on it.
func TestPartitionByTrackerRecord_AgreesWithAnyWorkEvidence(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []events.Event
	}{
		{"all work", []events.Event{evt("a", false), evt("b", false)}},
		{"all records", []events.Event{evt("a", true), evt("b", true)}},
		{"mixed", []events.Event{evt("a", true), evt("b", false)}},
		{"empty", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			work, _ := events.PartitionByTrackerRecord(tc.in)

			assert.Equal(t, events.AnyWorkEvidence(tc.in), len(work) > 0,
				"a non-empty work half must mean exactly what AnyWorkEvidence means")
		})
	}
}

// ids extracts ExternalIDs for order assertions.
func ids(evts []events.Event) []string {
	out := make([]string, 0, len(evts))
	for _, e := range evts {
		out = append(out, e.ExternalID)
	}

	return out
}
