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
	assert.Equal(t, config.JiraConfig{}, cfg.Jira)
	assert.Equal(t, "data/unjira.db", cfg.DBPath)

	enabled := cfg.EnabledCollectors()
	require.Contains(t, enabled, "claude_code")
	assert.Equal(t, true, enabled["claude_code"]["enabled"])
}

func TestLoad_ParsesExampleShapedConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unjira.config.json")
	body := `{
		"jira": {"site": "https://yourorg.atlassian.net", "project_keys": ["PROJ"]},
		"collectors": {
			"claude_code": {"enabled": true, "backfill_days": 14},
			"github": {"enabled": false, "repos": ["yourorg/yourrepo"]},
			"slack": {"enabled": false, "channels": ["#my-team"]},
			"jira": {"enabled": false}
		},
		"db_path": "data/unjira.db"
	}`
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	cfg, err := config.Load(path)

	require.NoError(t, err)
	assert.Equal(t, "https://yourorg.atlassian.net", cfg.Jira.Site)
	assert.Equal(t, []string{"PROJ"}, cfg.Jira.ProjectKeys)
	assert.Equal(t, "data/unjira.db", cfg.DBPath)
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

func TestDefaultConfig_HasClaudeCodeEnabledByDefault(t *testing.T) {
	cfg := config.Default()

	enabled := cfg.EnabledCollectors()

	require.Contains(t, enabled, "claude_code")
	assert.Equal(t, "data/unjira.db", cfg.DBPath)
}
