package pipeline_test

// narrate_trackerrecords_test.go covers the entrance-side split: RunNarrate clusters
// work evidence and never clusters tracker records.
//
// The finding (F18): 96 Jira events carried 210,350 characters into the clustering
// prompt against the session events' 45,550 — 82% of the payload — and 29 of 68
// narratives held nothing but tracker bookkeeping. 26 of those contained exactly one
// issue key and linked to the ticket they were derived FROM. Every one was correctly
// refused downstream by AnyWorkEvidence, so nothing wrong was written; the whole
// cluster/name/persist/rehydrate/propose/suppress cycle was spent to reach "no".

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/pipeline"
	"github.com/jcogilvie/unjira/internal/store"
)

// seedTrackerRecord inserts a jira-sourced event marked as tracker bookkeeping —
// what EventsFromChangelogEntry and EventFromComment produce.
func seedTrackerRecord(t *testing.T, s *store.Store, extID, summary string, at time.Time) {
	t.Helper()

	e := events.NewEvent("jira", extID, at, summary)
	events.SetTrackerRecord(&e)
	_, err := s.InsertEvent(e)
	require.NoError(t, err)
}

// TestRunNarrate_DoesNotClusterTrackerRecords is the finding. A pass over a window
// holding both kinds must cluster only the work.
func TestRunNarrate_DoesNotClusterTrackerRecords(t *testing.T) {
	s := narrateStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	seedNarrateEvent(t, s, "session-1", "built the thing", base)
	seedTrackerRecord(t, s, "PAAS-1:status:1", "PAAS-1 status: Ready for Dev → In Progress", base.Add(time.Minute))
	seedTrackerRecord(t, s, "PAAS-1:status:2", "PAAS-1 status: In Progress → In Review", base.Add(2*time.Minute))

	client := &narrateLLM{
		responses: []string{`[{"kind":"new","title":"Built the thing","summary":"s","event_indices":[0]}]`},
	}
	window := correlator.TimeRange{Start: base, End: base.Add(time.Hour)}

	got, err := pipeline.RunNarrate(t.Context(), s, client, narrateConfig(), window,
		pipeline.NarrateOptions{})
	require.NoError(t, err)

	assert.Equal(t, 1, got.UnlinkedEvents,
		"only the work event is a clustering candidate")
	assert.Equal(t, 2, got.ExcludedTrackerRecords,
		"and the excluded count is reported, or an operator cannot see what was left out")

	require.Len(t, client.prompts, 1)
	assert.NotContains(t, client.prompts[0], "Ready for Dev",
		"a tracker record must not reach the prompt at all — this is the 82% of payload F18 measured")
	assert.Contains(t, client.prompts[0], "built the thing")
}

// TestRunNarrate_TrackerRecordsAloneCostNoModelCall is the cost half. A window
// holding ONLY bookkeeping produced a narrative, a match and a suppressed comment
// every pass; now it must spend nothing.
func TestRunNarrate_TrackerRecordsAloneCostNoModelCall(t *testing.T) {
	s := narrateStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	seedTrackerRecord(t, s, "PAAS-2:status:1", "PAAS-2 status: Discovery → In Progress", base)
	seedTrackerRecord(t, s, "PAAS-2:comment:1", "PAAS-2 comment by a human: looking at this", base.Add(time.Minute))

	client := &narrateLLM{responses: []string{`[]`}}
	window := correlator.TimeRange{Start: base, End: base.Add(time.Hour)}

	got, err := pipeline.RunNarrate(t.Context(), s, client, narrateConfig(), window,
		pipeline.NarrateOptions{})
	require.NoError(t, err)

	assert.Empty(t, client.prompts,
		"no work evidence means nothing to narrate: the early return must fire BEFORE the call, "+
			"which is the whole cost saving")
	assert.Zero(t, got.UnlinkedEvents)
	assert.Equal(t, 2, got.ExcludedTrackerRecords)
	assert.Empty(t, got.Narratives)
}

// TestRunNarrate_ExcludedRecordsAreNotLinkedAndThatIsFine pins the deliberate
// consequence, so nobody "fixes" it with a watermark it does not need.
//
// An excluded event has no narrative_events row, so UnlinkedEventsInRange offers it
// again next pass. That is NOT design-notes #29's livelock: that failure re-examined
// narratives at full MODEL cost. Here the events are filtered by a pure function
// before any call, so a repeat costs a map lookup.
func TestRunNarrate_ExcludedRecordsAreNotLinkedAndThatIsFine(t *testing.T) {
	s := narrateStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	seedNarrateEvent(t, s, "session-1", "built the thing", base)
	seedTrackerRecord(t, s, "PAAS-3:status:1", "PAAS-3 status: Discovery → In Progress", base.Add(time.Minute))

	client := &narrateLLM{
		responses: []string{`[{"kind":"new","title":"Built","summary":"s","event_indices":[0]}]`},
	}
	window := correlator.TimeRange{Start: base, End: base.Add(time.Hour)}

	first, err := pipeline.RunNarrate(t.Context(), s, client, narrateConfig(), window,
		pipeline.NarrateOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, first.ExcludedTrackerRecords)

	// Second pass: the work event is now linked, so only the tracker record remains
	// unlinked — and with no work evidence, the pass must not call the model again.
	callsAfterFirst := len(client.prompts)

	second, err := pipeline.RunNarrate(t.Context(), s, client, narrateConfig(), window,
		pipeline.NarrateOptions{})
	require.NoError(t, err)

	assert.Equal(t, 1, second.ExcludedTrackerRecords,
		"the record is offered again, which is expected and cheap")
	assert.Zero(t, second.UnlinkedEvents)
	assert.Len(t, client.prompts, callsAfterFirst,
		"but it costs NO further model call — that is what distinguishes this from a livelock")
}

// TestRunNarrate_UnmarkedJiraEventsStillCluster guards the direction of failure.
// IsTrackerRecord treats absence as work evidence on purpose, so a collector that
// forgets to mark makes unjira too talkative rather than silent — an over-eager
// proposal reaches a human, a suppression does not.
func TestRunNarrate_UnmarkedJiraEventsStillCluster(t *testing.T) {
	s := narrateStore(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	// Deliberately NOT marked, e.g. a future collector's event.
	_, err := s.InsertEvent(events.NewEvent("jira", "unmarked-1", base, "some jira-sourced observation"))
	require.NoError(t, err)

	client := &narrateLLM{
		responses: []string{`[{"kind":"new","title":"Observed","summary":"s","event_indices":[0]}]`},
	}
	window := correlator.TimeRange{Start: base, End: base.Add(time.Hour)}

	got, err := pipeline.RunNarrate(t.Context(), s, client, narrateConfig(), window,
		pipeline.NarrateOptions{})
	require.NoError(t, err)

	assert.Equal(t, 1, got.UnlinkedEvents,
		"an unmarked event is work evidence: the exclusion is opt-in by the producing collector")
	assert.Zero(t, got.ExcludedTrackerRecords)
}
