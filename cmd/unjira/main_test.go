package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/clients/jira"
	"github.com/jcogilvie/unjira/internal/clients/local"
	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/credentials"
	"github.com/jcogilvie/unjira/internal/logging"
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

// jiraSites builds a config with one jira connection and one tracker per site, each
// tracker named after its connection.
func jiraSites(sites ...jiraSite) config.Config {
	var cfg config.Config

	for _, site := range sites {
		cfg.Connections = append(cfg.Connections,
			config.Connection{Name: site.name, Kind: config.KindJira, Endpoint: site.endpoint})
		cfg.Trackers = append(cfg.Trackers,
			config.Tracker{Name: site.name, Connection: site.name, Scopes: site.scopes})
	}

	return cfg
}

type jiraSite struct {
	name, endpoint string
	scopes         []string
}

// localTracker is a config whose one tracker sits on the local backend.
func localTracker(scopes ...string) config.Config {
	return config.Config{
		Connections: []config.Connection{{Name: "local", Kind: config.KindLocal}},
		Trackers:    []config.Tracker{{Name: "local", Connection: "local", Scopes: scopes}},
	}
}

func TestJiraClientForProject_ResolvesCredentialsByConnectionName(t *testing.T) {
	app := &appContext{
		config: jiraSites(
			jiraSite{"corp", "https://corp.atlassian.net", []string{"SUMO"}},
			jiraSite{"paas", "https://paas.atlassian.net", []string{"PAAS"}},
		),
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
		config:          jiraSites(jiraSite{"corp", "https://corp.atlassian.net", []string{"SUMO"}}),
		jiraCredentials: jsonSetFor(t, map[string]credentials.Credential{}),
	}

	_, err := app.jiraClientForProject("SUMO")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "corp")
	assert.Contains(t, err.Error(), "UNJIRA_JIRA_CREDENTIALS")
}

func TestJiraClientForProject_UnknownProjectErrors(t *testing.T) {
	app := &appContext{
		config: jiraSites(jiraSite{"corp", "https://corp.atlassian.net", []string{"SUMO"}}),
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

func TestTrackerReader_JiraBackendReturnsJiraTracker(t *testing.T) {
	app := &appContext{
		config: jiraSites(jiraSite{"default", "https://yourorg.atlassian.net", []string{"PROJ"}}),
		store:  openTestStore(t),
		jiraCredentials: jsonSetFor(t, map[string]credentials.Credential{
			"default": {Email: "e", Token: "t"},
		}),
	}

	tracker, err := app.trackerReader("PROJ")

	require.NoError(t, err)
	assert.IsType(t, &jira.Tracker{}, tracker)
}

func TestTrackerReader_LocalBackendReturnsLocalTracker(t *testing.T) {
	app := &appContext{
		config: localTracker("PROJ"),
		store:  openTestStore(t),
	}

	tracker, err := app.trackerReader("PROJ")

	require.NoError(t, err)
	assert.IsType(t, &local.Tracker{}, tracker)
}

// TestWarnIfNoStatusHistorySource_LogsOnceWhenNothingSuppliesHistory is
// task #174's structural-gap half: a config where the tracker backend is
// jira (transition-capable) but nothing enabled can ever supply status
// history (registry has no matching entry here — the same shape as a real
// deployment that enables claude_code only). If this regressed to silent,
// the operator would have no way to learn that EVERY future transition on
// this deployment is unguarded, short of noticing it in per-narrative
// output one issue at a time.
func TestWarnIfNoStatusHistorySource_LogsOnceWhenNothingSuppliesHistory(t *testing.T) {
	var logged strings.Builder
	testLog, err := logging.New(logging.Options{Format: "text", Level: "info", Out: &logged})
	require.NoError(t, err)

	app := &appContext{
		config: withCollectors(jiraSites(jiraSite{"default", "https://x.atlassian.net", []string{"PROJ"}}),
			map[string]map[string]any{"claude_code": {"enabled": true}}),
		log: testLog,
	}

	app.warnIfNoStatusHistorySource()

	assert.Contains(t, logged.String(), "staleness guard",
		"the operator must be told the guard can never run, not merely see nothing happen")
}

// TestWarnIfNoStatusHistorySource_SilentWhenTheJiraCollectorIsEnabled is the
// case that must NOT warn: the jira collector is enabled and registered, so
// the staleness guard can run once history is collected. A false positive
// here would train operators to ignore the warning.
func TestWarnIfNoStatusHistorySource_SilentWhenTheJiraCollectorIsEnabled(t *testing.T) {
	var logged strings.Builder
	testLog, err := logging.New(logging.Options{Format: "text", Level: "info", Out: &logged})
	require.NoError(t, err)

	app := &appContext{
		config: withCollectors(jiraSites(jiraSite{"default", "https://x.atlassian.net", []string{"PROJ"}}),
			map[string]map[string]any{"jira": {"enabled": true}}),
		log: testLog,
	}

	app.warnIfNoStatusHistorySource()

	assert.Empty(t, logged.String(), "the real jira collector is enabled: nothing is missing")
}

// TestWarnIfNoStatusHistorySource_SilentOnTheLocalBackend: the local
// backend has no real tracker for anyone else to move behind unjira's back
// (internal/clients/local's own package doc — "no real tracker reachable"),
// so the staleness guard's entire premise does not apply. This is also what
// keeps the offline test suite quiet: local is the backend every automated
// test uses, and warning on every one of them would be exactly the noise
// task #174's brief says this must not become.
func TestWarnIfNoStatusHistorySource_SilentOnTheLocalBackend(t *testing.T) {
	var logged strings.Builder
	testLog, err := logging.New(logging.Options{Format: "text", Level: "info", Out: &logged})
	require.NoError(t, err)

	app := &appContext{
		config: withCollectors(localTracker("PROJ"), map[string]map[string]any{"claude_code": {"enabled": true}}),
		log:    testLog,
	}

	app.warnIfNoStatusHistorySource()

	assert.Empty(t, logged.String(), "the local backend has no external tracker to diverge from")
}

// withCollectors sets cfg's collector block.
func withCollectors(cfg config.Config, collectors map[string]map[string]any) config.Config {
	cfg.Collectors = collectors

	return cfg
}

// TestWarnIfNoStatusHistorySource_SilentWithNoTrackers: nothing is reconciled, so there
// is no transition for the guard to protect.
func TestWarnIfNoStatusHistorySource_SilentWithNoTrackers(t *testing.T) {
	var logged strings.Builder
	testLog, err := logging.New(logging.Options{Format: "text", Level: "info", Out: &logged})
	require.NoError(t, err)

	app := &appContext{config: config.Config{}, log: testLog}

	app.warnIfNoStatusHistorySource()

	assert.Empty(t, logged.String())
}

func TestTrackerReader_UncoveredProjectErrors(t *testing.T) {
	app := &appContext{config: localTracker("PROJ"), store: openTestStore(t)}

	_, err := app.trackerReader("GHOST")

	require.ErrorContains(t, err, "GHOST")
}

// TestTaskTracker_GitHubTrackerHasNoBackendYet: a github tracker is configurable, but
// nothing reads or writes it in this build, so resolving one is a named error rather
// than a silent fallback to some other backend.
func TestTrackerReader_GitHubTrackerHasNoBackendYet(t *testing.T) {
	app := &appContext{
		config: config.Config{
			Connections: []config.Connection{{Name: "gh", Kind: config.KindGitHub, Endpoint: "https://api.github.com"}},
			Trackers:    []config.Tracker{{Name: "upstream", Connection: "gh", Scopes: []string{"o/r"}}},
		},
		store: openTestStore(t),
	}

	_, err := app.trackerReader("o/r")

	require.ErrorContains(t, err, "upstream")
}
