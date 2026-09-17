package pipeline

// context_narratives_test.go is the pure-function unit suite for F16's
// context-narrative bound (task #7): candidateIssueKeys and
// selectContextNarratives are tested here, in the white-box package, because
// both are unexported — like f16_probe_test.go, which reaches
// hydrateContextNarratives the same way.
//
// Deliberately does NOT attempt an end-to-end token measurement. .env is
// gitignored, so a worktree has no UNJIRA_JIRA_CREDENTIALS and dev narrate dies
// before clustering — design-notes #37 records what happens when a harness trusts
// a number out of a run like that. These tests are pure functions on constructed
// narratives; no store, no model, no credentials.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/store"
)

func TestCandidateIssueKeys_ReadsEveryProvenanceSource(t *testing.T) {
	jiraEvt := events.NewEvent("jira", "PROJ-7:status:1", time.Now(), "status change")
	jiraEvt.Artifacts[events.ArtifactIssueKey] = "PROJ-7"

	branchEvt := events.NewEvent("claude_code", "s1", time.Now(), "session")
	branchEvt.Artifacts[events.ArtifactGitBranch] = "feature/PROJ-9"

	scmEvt := events.NewEvent("claude_code", "s2", time.Now(), "session")
	events.SetSCMKeys(&scmEvt, []string{"PROJ-11"})

	proseEvt := events.NewEvent("claude_code", "s3", time.Now(), "session")
	events.SetTicketKeys(&proseEvt, []string{"PROJ-13"})

	got := candidateIssueKeys([]events.Event{jiraEvt, branchEvt, scmEvt, proseEvt})

	assert.Equal(t, map[string]bool{
		"PROJ-7": true, "PROJ-9": true, "PROJ-11": true, "PROJ-13": true,
	}, got)
}

func TestCandidateIssueKeys_NoEventsIsEmptyNotNil(t *testing.T) {
	got := candidateIssueKeys(nil)

	assert.NotNil(t, got, "an empty set, not a nil one a caller might mis-range over expecting an error")
	assert.Empty(t, got)
}

// narrativeRow is a tiny builder so each selectContextNarratives test states only
// what it cares about (id and window_end), matching go-conventions.md's fluent-
// builder guidance for nontrivial fixtures.
func narrativeRow(id int64, windowEnd time.Time) store.NarrativeRow {
	return store.NarrativeRow{ID: id, WindowStart: windowEnd.Add(-time.Hour), WindowEnd: windowEnd}
}

func TestSelectContextNarratives_ZeroBoundIncludesEverything(t *testing.T) {
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	rows := []store.NarrativeRow{
		narrativeRow(1, base), narrativeRow(2, base.Add(time.Hour)), narrativeRow(3, base.Add(2*time.Hour)),
	}

	kept, excluded := selectContextNarratives(rows, nil, nil, 0)

	assert.Equal(t, rows, kept, "zero means unlimited: rows pass through unchanged, in their original order")
	assert.Zero(t, excluded)
}

func TestSelectContextNarratives_BoundAtOrAbovePopulationIsANoOp(t *testing.T) {
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	rows := []store.NarrativeRow{narrativeRow(1, base), narrativeRow(2, base.Add(time.Hour))}

	kept, excluded := selectContextNarratives(rows, nil, nil, 5)

	assert.Equal(t, rows, kept, "nothing excluded means nothing re-ordered either")
	assert.Zero(t, excluded)
}

// TestSelectContextNarratives_RecencyBeatsInsertionOrder pins the ordering this
// bound exists to fix: NarrativesOverlapping orders (window_start, id) ascending,
// so a bare LIMIT keeps the OLDEST rows — close to the worst choice, since the
// newest overlapping narrative is the likeliest to be extended by a new event
// (finding F16). Three rows, oldest-to-newest by id, bound to 2: the oldest must
// be the one dropped.
func TestSelectContextNarratives_RecencyBeatsInsertionOrder(t *testing.T) {
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	oldest := narrativeRow(1, base)
	middle := narrativeRow(2, base.Add(time.Hour))
	newest := narrativeRow(3, base.Add(2*time.Hour))
	rows := []store.NarrativeRow{oldest, middle, newest} // NarrativesOverlapping's own order

	kept, excluded := selectContextNarratives(rows, nil, nil, 2)

	require.Len(t, kept, 2)
	assert.ElementsMatch(t, []int64{newest.ID, middle.ID}, idsOf(kept),
		"the two most recently active narratives survive, not the first two by insertion")
	assert.Equal(t, 1, excluded)
}

// TestSelectContextNarratives_SharedIssueKeyOutranksRecency is the load-bearing
// case: an OLD narrative that this window's own candidate events also name must
// survive over a newer one that shares nothing, because dropping it would make
// the model unable to see the story an event belongs to and it would open a
// spurious "new" cluster fragmenting a narrative that already exists.
func TestSelectContextNarratives_SharedIssueKeyOutranksRecency(t *testing.T) {
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	oldButNamed := narrativeRow(1, base) // oldest, but linked to PROJ-9
	newer := narrativeRow(2, base.Add(time.Hour))
	newest := narrativeRow(3, base.Add(2*time.Hour))
	rows := []store.NarrativeRow{oldButNamed, newer, newest}
	linkedKeys := map[int64][]string{oldButNamed.ID: {"PROJ-9"}}
	candidateKeys := map[string]bool{"PROJ-9": true}

	kept, excluded := selectContextNarratives(rows, linkedKeys, candidateKeys, 2)

	require.Len(t, kept, 2)
	assert.ElementsMatch(t, []int64{oldButNamed.ID, newest.ID}, idsOf(kept),
		"the keyed narrative keeps its slot even though it is the oldest; "+
			"recency decides only among the rest")
	assert.Equal(t, 1, excluded)
}

// TestSelectContextNarratives_UnlinkedNarrativeFallsBackToRecency proves the
// shared-key tier does not error or panic when linkedKeys has no entry for a row
// — the ordinary case for a narrative Match has not examined yet.
func TestSelectContextNarratives_UnlinkedNarrativeFallsBackToRecency(t *testing.T) {
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	rows := []store.NarrativeRow{narrativeRow(1, base), narrativeRow(2, base.Add(time.Hour))}

	kept, excluded := selectContextNarratives(rows, map[int64][]string{}, map[string]bool{"PROJ-1": true}, 1)

	require.Len(t, kept, 1)
	assert.Equal(t, int64(2), kept[0].ID, "no row is linked to PROJ-1, so recency alone decides")
	assert.Equal(t, 1, excluded)
}

// TestSelectContextNarratives_TiesBreakOnIDNotSortInstability: two rows sharing a
// window_end must resolve identically across repeated calls, or an otherwise-
// identical pass could select a different context set from one run to the next
// (mirrors match_candidates.go's rankCandidates determinism note).
func TestSelectContextNarratives_TiesBreakOnIDNotSortInstability(t *testing.T) {
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	rows := []store.NarrativeRow{narrativeRow(5, base), narrativeRow(2, base), narrativeRow(9, base)}

	kept, excluded := selectContextNarratives(rows, nil, nil, 2)

	require.Len(t, kept, 2)
	assert.Equal(t, []int64{2, 5}, idsOf(kept), "identical window_end: lowest id wins, deterministically")
	assert.Equal(t, 1, excluded)
}

func idsOf(rows []store.NarrativeRow) []int64 {
	out := make([]int64, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}

	return out
}
