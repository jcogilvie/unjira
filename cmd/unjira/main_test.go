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

// watchTestLLMConfig is the minimal config.LLMConfig that satisfies
// appContext.llmClient's own validation (Model, ContextWindowTokens, BaseURL,
// and a credential) so a watchCmd.Run test can reach the write-scope
// validation block that follows it without a real LLM backend.
func watchTestLLMConfig() config.LLMConfig {
	return config.LLMConfig{
		Model:               "test-model",
		ContextWindowTokens: 128000,
		BaseURL:             "http://localhost:4000/v1",
	}
}

// TestWatchCmd_UnwritableDefaultProjectFailsFast is test 6 from
// docs/superpowers/specs/2026-08-27-write-scope-design.md's testing section:
// tracker.default_project must be validated for writability at STARTUP
// (before any pass, any lease, any tracker construction), not merely
// discovered the first time a `create` action tries to fire. This exercises
// the real watchCmd.Run — the actual wiring in cmd/unjira/main.go — not just
// config.DefaultProjectConnection in isolation (internal/config/config_test.go
// already covers that method directly).
func TestWatchCmd_UnwritableDefaultProjectFailsFast(t *testing.T) {
	app := &appContext{
		config: config.Config{
			LLM:        watchTestLLMConfig(),
			Correlator: config.CorrelatorConfig{TailSummarizeThresholdTokens: 1_000_000, RecentEventsKept: 20},
			Reconciler: config.ReconcilerConfig{MaxNarrativesPerPass: 10, MinConfidenceToPropose: 0.5},
			Tracker:    config.TrackerConfig{DefaultProject: "PAAS"},
			Jira: []config.JiraConnection{
				// PAAS is readable but deliberately not in WritableProjectKeys.
				{Name: "dev", ProjectKeys: []string{"PAAS", "DEVSBX"}, WritableProjectKeys: []string{"DEVSBX"}},
			},
		},
		llmAPIKey: "test-key",
		// store is deliberately left nil: this test's whole point is that the
		// error surfaces BEFORE anything reaches app.store (a lease
		// acquisition, a tracker construction) — a nil-pointer panic here
		// would itself be evidence the check runs too late.
	}

	err := (&watchCmd{}).Run(app)

	require.Error(t, err, "an unwritable default_project must fail fast, before the first pass")
	assert.Contains(t, err.Error(), "PAAS")
	assert.Contains(t, err.Error(), "writable_project_keys")
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
		config: config.Config{
			Tracker:    config.TrackerConfig{Backend: "jira"},
			Collectors: map[string]map[string]any{"claude_code": {"enabled": true}},
		},
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
		config: config.Config{
			Tracker:    config.TrackerConfig{Backend: "jira"},
			Collectors: map[string]map[string]any{"jira": {"enabled": true}},
		},
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
		config: config.Config{
			Tracker:    config.TrackerConfig{Backend: "local"},
			Collectors: map[string]map[string]any{"claude_code": {"enabled": true}},
		},
		log: testLog,
	}

	app.warnIfNoStatusHistorySource()

	assert.Empty(t, logged.String(), "the local backend has no external tracker to diverge from")
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
