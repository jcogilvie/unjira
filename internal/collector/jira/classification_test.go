package jira_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/events"
)

// TestCollect_MarksTrackerRecordsByConfiguredScope: an issue's events are a tracker's
// account of itself only when its project is a configured tracker's scope here (F28).
// The query scoping keeps that true for everything Jira returns, so behaviour is
// unchanged; the case here is an issue whose project no tracker lists — moved, say,
// between the search and the read — whose events are therefore work evidence.
func TestCollect_MarksTrackerRecordsByConfiguredScope(t *testing.T) {
	fake := &fakeJira{
		accountID: "acct-unjira",
		issues: []map[string]any{
			searchIssue("PROJ-1", "PROJ", "2026-08-20T16:00:00.000+0000"),
			searchIssue("OTHER-2", "OTHER", "2026-08-20T16:00:00.000+0000"),
		},
		changelogs: map[string][]map[string]any{
			"PROJ-1":  {changelogEntry("1", "2026-08-20T14:00:00.000+0000", "status", "To Do", "Done")},
			"OTHER-2": {changelogEntry("2", "2026-08-20T14:00:00.000+0000", "status", "To Do", "Done")},
		},
	}
	cc := testContext(t, fake.start(t), config.Tracker{
		Name: "corp", Scopes: []string{"PROJ"},
		Queries: []config.JiraQuery{{Name: "mine", JQL: "assignee = currentUser()"}},
	})

	got, err := collectAll(t, cc)
	require.NoError(t, err)
	require.Len(t, got, 2)

	byKey := map[string]events.Event{}
	for _, e := range got {
		key, _ := e.Artifacts[events.ArtifactIssueKey].(string)
		byKey[key] = e
	}

	assert.True(t, events.IsTrackerRecord(byKey["PROJ-1"]), "PROJ is this deployment's tracker scope")
	assert.False(t, events.IsTrackerRecord(byKey["OTHER-2"]),
		"OTHER is no tracker's scope here, so its events are evidence, not the tracker describing itself")
}
