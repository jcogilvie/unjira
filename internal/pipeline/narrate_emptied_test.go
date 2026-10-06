package pipeline_test

// narrate_emptied_test.go is finding F40 end to end: a context narrative's eligible
// members are numbered in the clustering prompt, so the model may place every one of
// them in other clusters. The narrative they left holds no work, and must not stay
// open and ride into every later prompt as context.

import (
	"database/sql"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/pipeline"
	"github.com/jcogilvie/unjira/internal/store"
)

// TestRunNarrate_AReshuffleThatEmptiesAContextNarrativeMarksItSplit: narrative five
// holds one eligible member, "prior", and one context link, to "bg" (home's work). The
// model moves "prior" into a new narrative with the in-window event. five is left with
// no member, so it is marked split, its context link goes with it, and the next
// pass's context query no longer returns it.
func TestRunNarrate_AReshuffleThatEmptiesAContextNarrativeMarksItSplit(t *testing.T) {
	s := narrateStore(t)
	base := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	seedNarrateEvent(t, s, "prior", "earlier cache work", base.Add(-time.Hour))
	seedNarrateEvent(t, s, "bg", "the investigation behind it", base.Add(-2*time.Hour))
	home, err := s.InsertNarrative(base.Add(-3*time.Hour), base.Add(-2*time.Hour), "Investigation", "i")
	require.NoError(t, err)
	require.NoError(t, s.LinkMembers(home, []int64{narrateEventID(t, s, "bg")}, 0.9))
	five, err := s.InsertNarrative(base.Add(-time.Hour), base, "Cache rework", "cache so far")
	require.NoError(t, err)
	require.NoError(t, s.LinkMembers(five, []int64{narrateEventID(t, s, "prior")}, 0.9))
	require.NoError(t, s.LinkContext(five, []int64{narrateEventID(t, s, "bg")}))
	seedNarrateEvent(t, s, "E", "more cache rework", base.Add(time.Minute))

	client := &narrateLLM{responses: []string{
		// Numbered: 0=E (in-window), 1=prior (five's eligible member).
		`[{"kind":"new","title":"Cache rework, take two","summary":"s","confidence":0.8,"event_indices":[0,1]}]`,
	}}
	window := correlator.TimeRange{Start: base, End: base.Add(time.Hour)}

	got, err := pipeline.RunNarrate(t.Context(), s, client, narrateConfig(), window, pipeline.NarrateOptions{})

	require.NoError(t, err)
	row, err := s.GetNarrative(five)
	require.NoError(t, err)
	assert.Equal(t, store.StatusSplit, row.Status, "every member moved away, so five holds no work")

	overlapping, err := s.NarrativesOverlapping(window.Start, window.End)
	require.NoError(t, err)
	for _, n := range overlapping {
		assert.NotEqual(t, five, n.ID, "an emptied narrative must not be the next pass's context")
	}

	_, err = s.NarrativeEventLink(five, narrateEventID(t, s, "bg"))
	require.ErrorIs(t, err, sql.ErrNoRows, "an emptied narrative's context links go with it")
	l, err := s.NarrativeEventLink(home, narrateEventID(t, s, "bg"))
	require.NoError(t, err)
	assert.Equal(t, store.LinkMember, l.Kind, "the context event keeps its member home")

	// Reported, because five is in no cluster of this pass and nothing else shows it.
	assert.Equal(t, []correlator.EmptiedNarrative{{NarrativeID: five, Title: "Cache rework", ContextLinksDeleted: 1}}, got.Stats.Emptied)
	assert.Contains(t, pipeline.RenderNarrateResult(got),
		"emptied  narrative "+strconv.FormatInt(five, 10)+" \"Cache rework\": every member moved to another "+
			"narrative; marked split, 1 context link(s) deleted")
}
