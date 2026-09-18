package store_test

// narrativeissuekeys_test.go covers NarrativeIssueKeysByNarrative, added for F16's
// context-narrative bound (task #7): SelectContextNarratives needs each candidate
// narrative's linked issue keys, cheaply and for many narratives at once, to rank a
// narrative sharing a key with the incoming window above one that does not.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/store"
)

func TestNarrativeIssueKeysByNarrative_GroupsKeysPerNarrative(t *testing.T) {
	s := openStore(t)
	first := insertNarrativeForTest(t, s, "first")
	second := insertNarrativeForTest(t, s, "second")
	third := insertNarrativeForTest(t, s, "third — no links at all")

	require.NoError(t, s.WithTx(func(tx *store.Tx) error {
		if err := tx.AddNarrativeIssues(first, []store.NarrativeIssue{
			{IssueKey: "PAAS-1", Role: "primary", Provenance: "branch", Confidence: 0.9},
			{IssueKey: "SUMO-2", Role: "same_work", Provenance: "prose_first", Confidence: 0.7},
		}); err != nil {
			return err
		}

		return tx.AddNarrativeIssues(second, []store.NarrativeIssue{
			{IssueKey: "OTHER-9", Role: "mentioned", Provenance: "prose_later", Confidence: 0.1},
		})
	}))

	got, err := s.NarrativeIssueKeysByNarrative([]int64{first, second, third})

	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"PAAS-1", "SUMO-2"}, got[first])
	assert.ElementsMatch(t, []string{"OTHER-9"}, got[second])
	_, hasThird := got[third]
	assert.False(t, hasThird, "a narrative with no links has no entry, not an empty slice")
}

func TestNarrativeIssueKeysByNarrative_EveryRoleCounts(t *testing.T) {
	// Ranking cares whether a story is ABOUT a key at all, not which role won —
	// that judgment (primary vs. same_work vs. mentioned) is Match's, already made
	// and irrelevant to "is this the same story".
	s := openStore(t)
	id := insertNarrativeForTest(t, s, "work")
	require.NoError(t, s.WithTx(func(tx *store.Tx) error {
		return tx.AddNarrativeIssues(id, []store.NarrativeIssue{
			{IssueKey: "PAAS-1", Role: "mentioned", Provenance: "prose_later", Confidence: 0.1},
		})
	}))

	got, err := s.NarrativeIssueKeysByNarrative([]int64{id})

	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"PAAS-1"}, got[id])
}

func TestNarrativeIssueKeysByNarrative_EmptyInputReturnsEmptyMap(t *testing.T) {
	s := openStore(t)

	got, err := s.NarrativeIssueKeysByNarrative(nil)

	require.NoError(t, err)
	assert.Empty(t, got)
}
