package store_test

// matchingbacklog_test.go covers finding F11: matching's backlog asked the wrong
// question.
//
// The accessor (then named `NarrativesWithoutIssueKey`) selected on the
// denormalized `narratives.issue_key` column. `match.confidence_floor` only promotes a primary INTO that column —
// below the floor, `persistLinks` still writes every `narrative_issues` row
// (deliberately: config.go's ConfidenceFloor doc says "the floor governs what
// unjira asserts, not what it records", because dropping the rows would make a
// low-confidence match indistinguishable from finding nothing at all).
//
// So a narrative with a REAL but low-confidence primary had link rows AND a NULL
// column, and the column-based query kept handing it back forever. Every pass
// re-matched it; when a re-match picked a DIFFERENT primary, the partial unique
// index `one_primary_per_narrative` rejected the insert and aborted the whole
// pass. That is not hypothetical — it crashed a drain on narrative 15
// (primary=PAAS-4002 at 0.55 against a floor of 0.70).
//
// docs/design-notes.md had already named the fix when the CREATE path hit the
// same trap: "The correct question is NOT EXISTS (SELECT 1 FROM
// narrative_issues ...)". Two of the three accessors applied that lesson
// (`NarrativesWithNoIssueLink`, `NarrativesWithActionableLinks`); matching was
// never revisited. This is that revisit.
//
// The tests below are written against the LINK TABLE only. They must keep
// passing after `narratives.issue_key` is deleted, which is the point: a test
// that asserted on the column would be pinning the thing being removed.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/store"
)

// TestNarrativesWithoutPrimaryLink_ExcludesASubFloorPrimary is the finding itself. A
// primary link at 0.55 is a match — just not one unjira will assert — so matching
// must stop reconsidering it.
func TestNarrativesWithoutPrimaryLink_ExcludesASubFloorPrimary(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 9, 14, 9, 0, 0, 0, time.UTC)

	subFloor, err := s.InsertNarrative(base, base.Add(time.Hour), "matched, below the floor", "s")
	require.NoError(t, err)

	// Exactly what persistLinks writes below the floor: the link row, and NO
	// SetNarrativeIssueLink call. Constructed this way rather than by calling
	// persistLinks so the test states the STORED STATE it cares about.
	require.NoError(t, s.AddNarrativeIssues(subFloor, []store.NarrativeIssue{
		{IssueKey: "DEVSBX-1", Role: "primary", Provenance: "prose_first", Confidence: 0.55},
	}))

	backlog, err := s.NarrativesWithoutPrimaryLink(10)
	require.NoError(t, err)

	for _, n := range backlog {
		assert.NotEqual(t, subFloor, n.ID,
			"a narrative with a primary link is attributed and must leave matching's backlog, "+
				"whatever its confidence; leaving it in means re-matching it every pass until a "+
				"differing primary trips one_primary_per_narrative and aborts the pass")
	}
}

// TestNarrativesWithoutPrimaryLink_KeepsANarrativeWithNoPrimary is the other half:
// the fix must not empty the backlog. Unmatched work is the whole input.
func TestNarrativesWithoutPrimaryLink_KeepsANarrativeWithNoPrimary(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 9, 14, 9, 0, 0, 0, time.UTC)

	unmatched, err := s.InsertNarrative(base, base.Add(time.Hour), "never matched", "s")
	require.NoError(t, err)

	backlog, err := s.NarrativesWithoutPrimaryLink(10)
	require.NoError(t, err)

	ids := make([]int64, 0, len(backlog))
	for _, n := range backlog {
		ids = append(ids, n.ID)
	}
	assert.Contains(t, ids, unmatched, "a narrative with no links at all is matching's job")
}

// TestNarrativesWithoutPrimaryLink_KeepsANarrativeWithOnlyNonPrimaryLinks is the
// case a naive `NOT EXISTS (any link)` would get wrong, and it is a real state:
// Match records `mentioned` for a cited-but-unrelated ticket while promoting
// nothing. That narrative is still unattributed and still matching's work.
//
// Note this is deliberately NOT the same predicate as NarrativesWithNoIssueLink,
// which asks "any link at all" because the CREATE path must not open a duplicate
// ticket for work that names any issue. Two questions, two predicates.
func TestNarrativesWithoutPrimaryLink_KeepsANarrativeWithOnlyNonPrimaryLinks(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 9, 14, 9, 0, 0, 0, time.UTC)

	mentionedOnly, err := s.InsertNarrative(base, base.Add(time.Hour), "cites a ticket", "s")
	require.NoError(t, err)
	require.NoError(t, s.AddNarrativeIssues(mentionedOnly, []store.NarrativeIssue{
		{IssueKey: "DEVSBX-2", Role: "mentioned", Provenance: "prose_later", Confidence: 0.9},
	}))

	backlog, err := s.NarrativesWithoutPrimaryLink(10)
	require.NoError(t, err)

	ids := make([]int64, 0, len(backlog))
	for _, n := range backlog {
		ids = append(ids, n.ID)
	}
	assert.Contains(t, ids, mentionedOnly,
		"a `mentioned` link is not an attribution; only a primary takes a narrative out of "+
			"matching's backlog")
}

// TestCountNarrativesWithoutPrimaryLink_MatchesTheFixedSelection re-pins the F10
// count against the corrected predicate. The count duplicates the selector's
// WHERE clause, so changing one and not the other would make the operator-facing
// remainder describe a different population than the pass examined.
func TestCountNarrativesWithoutPrimaryLink_MatchesTheFixedSelection(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 9, 14, 9, 0, 0, 0, time.UTC)

	// One of each shape the predicate must distinguish.
	subFloor, err := s.InsertNarrative(base, base.Add(time.Hour), "sub-floor primary", "s")
	require.NoError(t, err)
	require.NoError(t, s.AddNarrativeIssues(subFloor, []store.NarrativeIssue{
		{IssueKey: "DEVSBX-3", Role: "primary", Provenance: "prose_first", Confidence: 0.55},
	}))

	mentionedOnly, err := s.InsertNarrative(base, base.Add(time.Hour), "mentioned only", "s")
	require.NoError(t, err)
	require.NoError(t, s.AddNarrativeIssues(mentionedOnly, []store.NarrativeIssue{
		{IssueKey: "DEVSBX-4", Role: "mentioned", Provenance: "prose_later", Confidence: 0.9},
	}))

	_, err = s.InsertNarrative(base, base.Add(time.Hour), "no links", "s")
	require.NoError(t, err)

	selected, err := s.NarrativesWithoutPrimaryLink(bigEnoughToHoldEverything)
	require.NoError(t, err)

	counted, err := s.CountNarrativesWithoutPrimaryLink()
	require.NoError(t, err)

	assert.Equal(t, len(selected), counted,
		"the count must mirror the selection's predicate exactly")
	assert.Equal(t, 2, counted,
		"the mentioned-only and the unlinked narrative remain; the sub-floor primary does not")
}
