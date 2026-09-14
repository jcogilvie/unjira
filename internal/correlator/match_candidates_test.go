package correlator_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/events"
)

func claudeEvent(t *testing.T, externalID, branch string, ticketKeys ...string) events.Event {
	t.Helper()

	e := events.NewEvent("claude_code", externalID,
		time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC), "session summary")
	if branch != "" {
		e.Artifacts["git_branch"] = branch
	}
	if len(ticketKeys) > 0 {
		// []any deliberately: the collector stores it that way, and a []string
		// fixture would make every prose candidate silently vanish.
		anyKeys := make([]any, 0, len(ticketKeys))
		for _, k := range ticketKeys {
			anyKeys = append(anyKeys, k)
		}
		e.Artifacts["ticket_keys"] = anyKeys
	}

	return e
}

func jiraEvent(t *testing.T, key string) events.Event {
	t.Helper()

	e := events.NewEvent("jira", key+":status:1",
		time.Date(2026, 8, 24, 10, 30, 0, 0, time.UTC), key+" status: To Do → Done")
	e.Artifacts["issue_key"] = key
	e.Artifacts["project_key"] = "PROJ"
	e.Artifacts["connection"] = "corp"

	return e
}

func TestGatherCandidates_BranchOutranksProse(t *testing.T) {
	// The branch is the strongest attribution signal available today: a session
	// mentioning three keys but committing on feature/PROJ-42 is implementing
	// PROJ-42.
	evts := []events.Event{
		claudeEvent(t, "s1", "feature/PROJ-42", "PROJ-100", "PROJ-205", "PROJ-42"),
	}

	got := correlator.GatherCandidatesForTest(evts, nil, 10, nil)

	require.NotEmpty(t, got)
	assert.Equal(t, "PROJ-42", got[0].IssueKey)
	assert.Equal(t, correlator.ProvenanceBranch, got[0].Provenance)
}

func TestGatherCandidates_ProseOrderIsPreserved(t *testing.T) {
	evts := []events.Event{claudeEvent(t, "s1", "", "PROJ-100", "PROJ-205")}

	got := correlator.GatherCandidatesForTest(evts, nil, 10, nil)

	require.Len(t, got, 2)
	assert.Equal(t, "PROJ-100", got[0].IssueKey)
	assert.Equal(t, correlator.ProvenanceProseFirst, got[0].Provenance)
	assert.Equal(t, "PROJ-205", got[1].IssueKey)
	assert.Equal(t, correlator.ProvenanceProseLater, got[1].Provenance)
}

func TestGatherCandidates_JiraArtifactIsACandidate(t *testing.T) {
	evts := []events.Event{jiraEvent(t, "PROJ-7")}

	got := correlator.GatherCandidatesForTest(evts, nil, 10, nil)

	require.Len(t, got, 1)
	assert.Equal(t, "PROJ-7", got[0].IssueKey)
	assert.Equal(t, correlator.ProvenanceJiraEvent, got[0].Provenance)
	assert.Equal(t, "corp", got[0].Connection,
		"the connection artifact records which Jira site resolved this key")
}

func TestGatherCandidates_DedupesKeepingStrongestProvenance(t *testing.T) {
	// The same key can appear as prose in one event and as a branch in another.
	// It must appear once, at its strongest provenance.
	evts := []events.Event{
		claudeEvent(t, "s1", "", "PROJ-42"),
		claudeEvent(t, "s2", "feature/PROJ-42"),
	}

	got := correlator.GatherCandidatesForTest(evts, nil, 10, nil)

	require.Len(t, got, 1, "one key, one candidate")
	assert.Equal(t, correlator.ProvenanceBranch, got[0].Provenance)
}

func TestGatherCandidates_HonorsExcludeFromLinking(t *testing.T) {
	// A placeholder key satisfying a commit linter is not a real link. It must
	// be dropped from candidacy but reported, so a narrative whose only key was
	// excluded still shows up as untracked and annotated rather than vanishing.
	compiled, err := events.CompileLinkExclusionPatterns([]string{`^NOJIRA-\d+$`})
	require.NoError(t, err)
	evts := []events.Event{claudeEvent(t, "s1", "", "NOJIRA-1", "PROJ-42")}

	got := correlator.GatherCandidatesForTest(evts, compiled, 10, nil)

	require.Len(t, got, 1)
	assert.Equal(t, "PROJ-42", got[0].IssueKey)

	excluded := correlator.ExcludedCandidatesForTest(evts, compiled)
	assert.Equal(t, []string{"NOJIRA-1"}, excluded,
		"an excluded key must be recorded, not silently dropped")
}

func TestGatherCandidates_RespectsLimitStrongestFirst(t *testing.T) {
	// The cap bounds GetIssue fan-out and prompt size. Truncating must keep the
	// strongest candidates, or the limit would discard the answer.
	evts := []events.Event{
		claudeEvent(t, "s1", "feature/PROJ-9", "PROJ-1", "PROJ-2", "PROJ-3", "PROJ-4"),
	}

	got := correlator.GatherCandidatesForTest(evts, nil, 2, nil)

	require.Len(t, got, 2)
	assert.Equal(t, "PROJ-9", got[0].IssueKey, "the branch candidate must survive truncation")
	assert.Equal(t, correlator.ProvenanceBranch, got[0].Provenance)
}

func TestGatherCandidates_NoKeysIsEmptyNotError(t *testing.T) {
	evts := []events.Event{claudeEvent(t, "s1", "main")}

	got := correlator.GatherCandidatesForTest(evts, nil, 10, nil)

	assert.Empty(t, got, "untracked work is the default path, not an error")
}
