package correlator_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/events"
)

func summaryEvent(t *testing.T, source, externalID, summary string) events.Event {
	t.Helper()

	return events.NewEvent(source, externalID, time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC), summary)
}

// TestGatherCandidates_QualifiedGitHubReferenceInProseIsACandidate: an upstream issue
// named in a session is a candidate like a Jira key, verified later against its own
// tracker.
func TestGatherCandidates_QualifiedGitHubReferenceInProseIsACandidate(t *testing.T) {
	evts := []events.Event{
		summaryEvent(t, "claude_code", "s1", "investigating crossplane/crossplane#6812 and o/r#2"),
	}

	got := correlator.GatherCandidatesForTest(evts, nil, 10, nil)

	require.Len(t, got, 2)
	byKey := map[string]correlator.Candidate{}
	for _, c := range got {
		byKey[c.IssueKey] = c
	}
	assert.Equal(t, correlator.ProvenanceProseFirst, byKey["crossplane/crossplane#6812"].Provenance,
		"the first reference in an event naming no Jira key is its first mention")
	assert.Equal(t, correlator.ProvenanceProseLater, byKey["o/r#2"].Provenance)
}

// TestGatherCandidates_FixesInAPullRequestBodyIsAnAuthoringCandidate: "Fixes
// owner/repo#N" in a pull request's body is GitHub's own closing keyword, which links
// the pull request to the issue, so it ranks with SCM authoring commands — and the pull
// request's own reference, which heads its summary, is not a candidate: a pull request
// is not an issue.
func TestGatherCandidates_FixesInAPullRequestBodyIsAnAuthoringCandidate(t *testing.T) {
	pr := summaryEvent(t, "github", "o/r#7:opened", "o/r#7: Fix the thing\n\nFixes upstream/proj#99")
	pr.Artifacts[events.ArtifactPullRequest] = events.PullRequestRef("github.com", "o", "r", 7)

	got := correlator.GatherCandidatesForTest([]events.Event{pr}, nil, 10, nil)

	require.Len(t, got, 1)
	assert.Equal(t, "upstream/proj#99", got[0].IssueKey)
	assert.Equal(t, correlator.ProvenanceSCMCommand, got[0].Provenance)
}

// TestGatherCandidates_JiraKeysStillOutrankGitHubReferencesInATier: within a tier the
// order is alphabetical, and lower-cased references sort after uppercase project keys,
// so a narrative full of upstream references does not push its Jira keys past the cap.
func TestGatherCandidates_JiraKeysStillOutrankGitHubReferencesInATier(t *testing.T) {
	e := claudeEvent(t, "s1", "", "PROJ-1")
	e.Summary = "a/b#1 a/b#2 a/b#3"

	got := correlator.GatherCandidatesForTest([]events.Event{e}, nil, 2, nil)

	require.Len(t, got, 2)
	assert.Equal(t, "PROJ-1", got[0].IssueKey)
}

// TestGatherCandidates_APullRequestBodyCitesWhereItsTitleNames: a reference in a pull
// request's title names the issue, like a commit subject. One in its body with no
// closing keyword is a citation, so it is prose and the model judges it. One quoted as
// code in the body is data, not a candidate. The same split the github collector makes
// for Jira keys.
func TestGatherCandidates_APullRequestBodyCitesWhereItsTitleNames(t *testing.T) {
	pr := summaryEvent(t, "github", "o/r#7:opened",
		"o/r#7: Port the fix for up/a#1\n\nSee also up/b#2, which hit the same thing.\n\n"+
			"Fixes: up/c#3\n\n```\nerror: up/d#4 still open\n```\n")
	pr.Artifacts[events.ArtifactPullRequest] = events.PullRequestRef("github.com", "o", "r", 7)

	got := correlator.GatherCandidatesForTest([]events.Event{pr}, nil, 10, nil)

	byKey := map[string]correlator.Provenance{}
	for _, c := range got {
		byKey[c.IssueKey] = c.Provenance
	}

	assert.Equal(t, map[string]correlator.Provenance{
		"up/a#1": correlator.ProvenanceSCMCommand,
		"up/b#2": correlator.ProvenanceProseFirst,
		"up/c#3": correlator.ProvenanceSCMCommand,
	}, byKey)
}
