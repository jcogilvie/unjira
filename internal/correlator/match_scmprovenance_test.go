package correlator_test

// match_scmprovenance_test.go proves ProvenanceSCMCommand actually fires — the half
// of finding F20 that lives in the correlator.
//
// This file exists because F14 shipped INERT: its event constructor was written and
// tested while the field list it needed was never requested, so the feature was
// complete, green, and did nothing. "Wire it, then prove it fires" is now in
// go-conventions.md because of it. A new provenance tier that no candidate ever
// carries is the same defect, so these tests assert the tier reaches a Candidate from
// a realistic collector-shaped event, not merely that Rank() orders it correctly.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/events"
)

// scmEvent builds a claude_code event shaped the way the collector emits one, with
// the SCM keys in their own artifact.
func scmEvent(branch string, proseKeys, scmKeys []string) correlator.Event {
	e := events.NewEvent("claude_code", "sess:1", time.Now(), "a session")
	if branch != "" {
		e.Artifacts[events.ArtifactGitBranch] = branch
	}
	events.SetTicketKeys(&e, proseKeys)
	events.SetSCMKeys(&e, scmKeys)

	return e
}

// TestGatherCandidates_SCMKeyBecomesACandidate is the inert-shipping guard. A session
// whose prose names nothing must still produce the candidate it committed under —
// which is the measured case, 10 of 20 sessions.
func TestGatherCandidates_SCMKeyBecomesACandidate(t *testing.T) {
	got := correlator.GatherCandidatesForTest(
		[]correlator.Event{scmEvent("feature-no-key", nil, []string{"PAAS-3669"})}, nil, 0, nil)

	require.Len(t, got, 1,
		"a key named only in a commit must reach matching, or F20's fix is inert")
	assert.Equal(t, "PAAS-3669", got[0].IssueKey)
	assert.Equal(t, correlator.ProvenanceSCMCommand, got[0].Provenance)
}

// TestGatherCandidates_SCMOutranksProse pins the ordering that matters in practice: a
// session mentions many tickets in passing and commits against one.
func TestGatherCandidates_SCMOutranksProse(t *testing.T) {
	got := correlator.GatherCandidatesForTest(
		[]correlator.Event{scmEvent("", []string{"PAAS-1", "PAAS-2"}, []string{"PAAS-2"})}, nil, 0, nil)

	require.Len(t, got, 2)
	assert.Equal(t, "PAAS-2", got[0].IssueKey,
		"the ticket committed against ranks ahead of the ones merely discussed")
	assert.Equal(t, correlator.ProvenanceSCMCommand, got[0].Provenance)
}

// TestGatherCandidates_BranchOutranksSCM keeps the existing top tier intact. A branch
// names the work for its whole life; a commit names one change within it.
func TestGatherCandidates_BranchOutranksSCM(t *testing.T) {
	got := correlator.GatherCandidatesForTest(
		[]correlator.Event{scmEvent("PAAS-1-the-work", nil, []string{"PAAS-2"})}, nil, 0, nil)

	require.Len(t, got, 2)
	assert.Equal(t, "PAAS-1", got[0].IssueKey)
	assert.Equal(t, correlator.ProvenanceBranch, got[0].Provenance)
	assert.Equal(t, correlator.ProvenanceSCMCommand, got[1].Provenance)
}

// TestProvenanceRanks_AreStrictlyOrdered guards the whole ladder, because inserting a
// tier mid-list means renumbering every rank below it — an edit where a duplicated or
// skipped number silently breaks a tiebreak rather than failing.
func TestProvenanceRanks_AreStrictlyOrdered(t *testing.T) {
	ordered := []correlator.Provenance{
		correlator.ProvenanceReviewer,
		correlator.ProvenanceBranch,
		correlator.ProvenanceSCMCommand,
		correlator.ProvenanceJiraEvent,
		correlator.ProvenanceCorroborated,
		correlator.ProvenanceProseFirst,
		correlator.ProvenanceProseLater,
	}

	for i := 1; i < len(ordered); i++ {
		assert.Less(t, ordered[i-1].Rank(), ordered[i].Rank(),
			"%s must rank strictly stronger than %s", ordered[i-1], ordered[i])
	}

	assert.Greater(t, correlator.Provenance("something-new").Rank(),
		correlator.ProvenanceProseLater.Rank(),
		"an unrecognized provenance still ranks weakest, so a future tier degrades rather than "+
			"winning a tiebreak by accident")
}

// TestGatherCandidates_SCMKeysRespectLinkExclusions confirms the new path is not a
// hole in the exclusion filter. Placeholder keys a commit-message linter demands
// (`PROJ-0`, `NOJIRA-1`) reach ExtractTicketKeys by design and are told apart here.
func TestGatherCandidates_SCMKeysRespectLinkExclusions(t *testing.T) {
	patterns, err := events.CompileLinkExclusionPatterns([]string{`^PAAS-0$`})
	require.NoError(t, err)

	got := correlator.GatherCandidatesForTest(
		[]correlator.Event{scmEvent("", nil, []string{"PAAS-0", "PAAS-3669"})}, patterns, 0, nil)

	require.Len(t, got, 1,
		"an excluded placeholder must not become a candidate just because it arrived via SCM")
	assert.Equal(t, "PAAS-3669", got[0].IssueKey)
}
