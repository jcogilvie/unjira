package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/config"
)

func TestJiraClientForProject_ResolvesCredentialsByConnectionName(t *testing.T) {
	app := &appContext{
		config: config.Config{
			Jira: []config.JiraConnection{
				{Name: "corp", Site: "https://corp.atlassian.net", ProjectKeys: []string{"SUMO"}},
				{Name: "paas", Site: "https://paas.atlassian.net", ProjectKeys: []string{"PAAS"}},
			},
		},
		jiraCredentials: JiraCredentials{byName: map[string]jiraCredential{
			"corp": {Email: "corp@example.com", Token: "corp-token"},
			"paas": {Email: "paas@example.com", Token: "paas-token"},
		}},
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
		jiraCredentials: JiraCredentials{byName: map[string]jiraCredential{}},
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
	assert.Equal(t, "a@x.com", creds.byName["corp"].Email)
	assert.Equal(t, "tok1", creds.byName["corp"].Token)
	assert.Equal(t, "b@x.com", creds.byName["paas"].Email)
}
