package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/config"
)

func TestLoad_MissingFileReturnsDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.json")

	cfg, err := config.Load(path)

	require.NoError(t, err)
	assert.Empty(t, cfg.Jira)
	assert.Equal(t, "data/unjira.db", cfg.DBPath)
	assert.Empty(t, cfg.ExcludeFromLinking)

	enabled := cfg.EnabledCollectors()
	require.Contains(t, enabled, "claude_code")
	assert.Equal(t, true, enabled["claude_code"]["enabled"])
}

func TestLoad_ParsesExampleShapedConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unjira.config.json")
	body := `{
		"jira": [{"name": "default", "site": "https://yourorg.atlassian.net", "project_keys": ["PROJ"]}],
		"collectors": {
			"claude_code": {"enabled": true, "backfill_days": 14},
			"github": {"enabled": false, "repos": ["yourorg/yourrepo"]},
			"slack": {"enabled": false, "channels": ["#my-team"]},
			"jira": {"enabled": false}
		},
		"exclude_from_linking": ["-0$", "-1$"],
		"db_path": "data/unjira.db"
	}`
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	cfg, err := config.Load(path)

	require.NoError(t, err)
	require.Len(t, cfg.Jira, 1)
	assert.Equal(t, "default", cfg.Jira[0].Name)
	assert.Equal(t, "https://yourorg.atlassian.net", cfg.Jira[0].Site)
	assert.Equal(t, []string{"PROJ"}, cfg.Jira[0].ProjectKeys)
	assert.Equal(t, "data/unjira.db", cfg.DBPath)
	assert.Equal(t, []string{"-0$", "-1$"}, cfg.ExcludeFromLinking)
}

func TestJiraConnectionForProject_FindsByProjectKey(t *testing.T) {
	cfg := config.Config{
		Jira: []config.JiraConnection{
			{Name: "default", Site: "https://yourorg.atlassian.net", ProjectKeys: []string{"PROJ"}},
		},
	}

	conn, ok := cfg.JiraConnectionForProject("PROJ")

	require.True(t, ok)
	assert.Equal(t, "default", conn.Name)
	assert.Equal(t, "https://yourorg.atlassian.net", conn.Site)
}

func TestJiraConnectionForProject_UnknownProjectNotFound(t *testing.T) {
	cfg := config.Config{
		Jira: []config.JiraConnection{
			{Name: "default", Site: "https://yourorg.atlassian.net", ProjectKeys: []string{"PROJ"}},
		},
	}

	_, ok := cfg.JiraConnectionForProject("GHOST")

	assert.False(t, ok)
}

func TestJiraConnectionForProject_MultipleConnectionsDisambiguateByProject(t *testing.T) {
	cfg := config.Config{
		Jira: []config.JiraConnection{
			{Name: "corp", Site: "https://corp.atlassian.net", ProjectKeys: []string{"SUMO"}},
			{Name: "paas", Site: "https://paas.atlassian.net", ProjectKeys: []string{"PAAS"}},
		},
	}

	corp, ok := cfg.JiraConnectionForProject("SUMO")
	require.True(t, ok)
	assert.Equal(t, "corp", corp.Name)

	paas, ok := cfg.JiraConnectionForProject("PAAS")
	require.True(t, ok)
	assert.Equal(t, "paas", paas.Name)
}

func TestEnabledCollectors_FiltersToOnlyEnabled(t *testing.T) {
	cfg := config.Config{
		Collectors: map[string]map[string]any{
			"claude_code": {"enabled": true, "backfill_days": float64(14)},
			"github":      {"enabled": false},
			"slack":       {}, // no "enabled" key at all: must not be treated as enabled
		},
	}

	enabled := cfg.EnabledCollectors()

	assert.Equal(t, map[string]map[string]any{
		"claude_code": {"enabled": true, "backfill_days": float64(14)},
	}, enabled)
}

func TestCompiledLinkExclusions_CompilesConfiguredPatterns(t *testing.T) {
	cfg := config.Config{ExcludeFromLinking: []string{"-0$"}}

	compiled, err := cfg.CompiledLinkExclusions()

	require.NoError(t, err)
	require.Len(t, compiled, 1)
	assert.True(t, compiled[0].MatchString("PROJ-0"))
}

func TestCompiledLinkExclusions_BadPatternErrorsWithPatternNamed(t *testing.T) {
	cfg := config.Config{ExcludeFromLinking: []string{"("}}

	_, err := cfg.CompiledLinkExclusions()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "(")
}

func TestTrackerBackend_DefaultsToJira(t *testing.T) {
	cfg := config.Config{}

	assert.Equal(t, "jira", cfg.TrackerBackend())
}

func TestTrackerBackend_ExplicitBackendWins(t *testing.T) {
	cfg := config.Config{Tracker: config.TrackerConfig{Backend: "local"}}

	assert.Equal(t, "local", cfg.TrackerBackend())
}

func TestLoad_ParsesTrackerBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unjira.config.json")
	body := `{"tracker": {"backend": "local", "default_project": "PROJ"}}`
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	cfg, err := config.Load(path)

	require.NoError(t, err)
	assert.Equal(t, "local", cfg.TrackerBackend())
	assert.Equal(t, "PROJ", cfg.Tracker.DefaultProject)
}

func TestDefaultProjectConnection_UnsetErrors(t *testing.T) {
	cfg := config.Config{}

	_, err := cfg.DefaultProjectConnection()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "default_project")
}

func TestDefaultProjectConnection_SetButUncoveredErrors(t *testing.T) {
	cfg := config.Config{
		Tracker: config.TrackerConfig{DefaultProject: "GHOST"},
		Jira: []config.JiraConnection{
			{Name: "default", Site: "https://yourorg.atlassian.net", ProjectKeys: []string{"PROJ"}},
		},
	}

	_, err := cfg.DefaultProjectConnection()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "GHOST")
}

func TestDefaultProjectConnection_SetAndCoveredReturnsConnection(t *testing.T) {
	cfg := config.Config{
		Tracker: config.TrackerConfig{DefaultProject: "PROJ"},
		Jira: []config.JiraConnection{
			{Name: "default", Site: "https://yourorg.atlassian.net", ProjectKeys: []string{"PROJ"}},
		},
	}

	conn, err := cfg.DefaultProjectConnection()

	require.NoError(t, err)
	assert.Equal(t, "default", conn.Name)
}

func TestDefaultConfig_HasClaudeCodeEnabledByDefault(t *testing.T) {
	cfg := config.Default()

	enabled := cfg.EnabledCollectors()

	require.Contains(t, enabled, "claude_code")
	assert.Equal(t, "data/unjira.db", cfg.DBPath)
}
