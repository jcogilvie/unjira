package main

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/clients/jira"
	"github.com/jcogilvie/unjira/internal/clients/local"
	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/credentials"
	"github.com/jcogilvie/unjira/internal/store"
)

// jsonSetFor builds a credentials.JSONSet for tests. JSONSet's only
// constructor is its UnmarshalJSON (the whole point: Kong drives it, and
// internal/credentials keeps a single decode path), so tests go through the
// same JSON round-trip a real UNJIRA_JIRA_CREDENTIALS value would.
func jsonSetFor(t *testing.T, byName map[string]credentials.Credential) credentials.JSONSet {
	t.Helper()

	data, err := json.Marshal(byName)
	require.NoError(t, err)

	var set credentials.JSONSet
	require.NoError(t, set.UnmarshalJSON(data))

	return set
}

func TestJiraClientForProject_ResolvesCredentialsByConnectionName(t *testing.T) {
	app := &appContext{
		config: config.Config{
			Jira: []config.JiraConnection{
				{Name: "corp", Site: "https://corp.atlassian.net", ProjectKeys: []string{"SUMO"}},
				{Name: "paas", Site: "https://paas.atlassian.net", ProjectKeys: []string{"PAAS"}},
			},
		},
		jiraCredentials: jsonSetFor(t, map[string]credentials.Credential{
			"corp": {Email: "corp@example.com", Token: "corp-token"},
			"paas": {Email: "paas@example.com", Token: "paas-token"},
		}),
	}

	client, err := app.jiraClientForProject("PAAS")

	require.NoError(t, err)
	require.NotNil(t, client)
}

func TestJiraClientForProject_MissingCredentialsErrorsWithConnectionName(t *testing.T) {
	app := &appContext{
		config: config.Config{
			Jira: []config.JiraConnection{
				{Name: "corp", Site: "https://corp.atlassian.net", ProjectKeys: []string{"SUMO"}},
			},
		},
		jiraCredentials: jsonSetFor(t, map[string]credentials.Credential{}),
	}

	_, err := app.jiraClientForProject("SUMO")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "corp")
	assert.Contains(t, err.Error(), "UNJIRA_JIRA_CREDENTIALS")
}

func TestJiraClientForProject_UnknownProjectErrors(t *testing.T) {
	app := &appContext{
		config: config.Config{
			Jira: []config.JiraConnection{
				{Name: "corp", Site: "https://corp.atlassian.net", ProjectKeys: []string{"SUMO"}},
			},
		},
	}

	_, err := app.jiraClientForProject("GHOST")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "GHOST")
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()

	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	return s
}

func TestTaskTracker_JiraBackendReturnsJiraTracker(t *testing.T) {
	app := &appContext{
		config: config.Config{
			Tracker: config.TrackerConfig{Backend: "jira"},
			Jira: []config.JiraConnection{
				{Name: "default", Site: "https://yourorg.atlassian.net", ProjectKeys: []string{"PROJ"}},
			},
		},
		store: openTestStore(t),
		jiraCredentials: jsonSetFor(t, map[string]credentials.Credential{
			"default": {Email: "e", Token: "t"},
		}),
	}

	tracker, err := app.taskTracker("PROJ")

	require.NoError(t, err)
	assert.IsType(t, &jira.Tracker{}, tracker)
}

func TestTaskTracker_LocalBackendReturnsLocalTracker(t *testing.T) {
	app := &appContext{
		config: config.Config{Tracker: config.TrackerConfig{Backend: "local"}},
		store:  openTestStore(t),
	}

	tracker, err := app.taskTracker("PROJ")

	require.NoError(t, err)
	assert.IsType(t, &local.Tracker{}, tracker)
}

func TestTaskTracker_UnknownBackendErrors(t *testing.T) {
	app := &appContext{
		config: config.Config{Tracker: config.TrackerConfig{Backend: "trello"}},
		store:  openTestStore(t),
	}

	_, err := app.taskTracker("PROJ")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "trello")
}
