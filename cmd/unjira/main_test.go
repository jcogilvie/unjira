package main

import (
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

func TestJiraClientForProject_ResolvesCredentialsByConnectionName(t *testing.T) {
	app := &appContext{
		config: config.Config{
			Jira: []config.JiraConnection{
				{Name: "corp", Site: "https://corp.atlassian.net", ProjectKeys: []string{"SUMO"}},
				{Name: "paas", Site: "https://paas.atlassian.net", ProjectKeys: []string{"PAAS"}},
			},
		},
		jiraCredentials: JiraCredentials{set: credentials.NewSet(map[string]credentials.Credential{
			"corp": {Email: "corp@example.com", Token: "corp-token"},
			"paas": {Email: "paas@example.com", Token: "paas-token"},
		})},
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
		jiraCredentials: JiraCredentials{set: credentials.NewSet(map[string]credentials.Credential{})},
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

func TestJiraCredentials_UnmarshalsFromJSON(t *testing.T) {
	var creds JiraCredentials

	err := creds.UnmarshalJSON([]byte(`{"corp":{"email":"a@x.com","token":"tok1"},"paas":{"email":"b@x.com","token":"tok2"}}`))

	require.NoError(t, err)

	corp, ok := creds.Set().For("corp")
	require.True(t, ok)
	assert.Equal(t, "a@x.com", corp.Email)
	assert.Equal(t, "tok1", corp.Token)

	paas, ok := creds.Set().For("paas")
	require.True(t, ok)
	assert.Equal(t, "b@x.com", paas.Email)
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
		jiraCredentials: JiraCredentials{set: credentials.NewSet(map[string]credentials.Credential{
			"default": {Email: "e", Token: "t"},
		})},
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
