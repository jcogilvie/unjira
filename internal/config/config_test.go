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

func TestLLMConfig_Validate(t *testing.T) {
	tests := []struct {
		name        string
		cfg         config.LLMConfig
		wantErrText string // empty means Validate must return nil
	}{
		{
			name:        "requires model",
			cfg:         config.LLMConfig{ContextWindowTokens: 128000},
			wantErrText: "model",
		},
		{
			name:        "requires context window tokens",
			cfg:         config.LLMConfig{Model: "gpt-5-2"},
			wantErrText: "context_window_tokens",
		},
		{
			name:        "requires positive context window tokens",
			cfg:         config.LLMConfig{Model: "gpt-5-2", ContextWindowTokens: 0},
			wantErrText: "context_window_tokens",
		},
		{
			name: "passes with model and context window",
			cfg:  config.LLMConfig{Model: "gpt-5-2", ContextWindowTokens: 128000},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()

			if tt.wantErrText == "" {
				require.NoError(t, err)
				return
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErrText)
		})
	}
}

func TestLoad_ParsesLLMBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unjira.config.json")
	body := `{"llm": {"model": "gpt-5-2", "base_url": "http://localhost:4000/v1", "context_window_tokens": 128000}}`
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	cfg, err := config.Load(path)

	require.NoError(t, err)
	assert.Equal(t, "gpt-5-2", cfg.LLM.Model)
	assert.Equal(t, "http://localhost:4000/v1", cfg.LLM.BaseURL)
	assert.Equal(t, 128000, cfg.LLM.ContextWindowTokens)
}

func TestCorrelatorConfig_Validate(t *testing.T) {
	tests := []struct {
		name        string
		cfg         config.CorrelatorConfig
		wantErrText string
	}{
		{
			name:        "requires positive threshold",
			cfg:         config.CorrelatorConfig{TailSummarizeThresholdTokens: 0, RecentEventsKept: 20},
			wantErrText: "tail_summarize_threshold_tokens",
		},
		{
			name:        "requires positive recent-events-kept",
			cfg:         config.CorrelatorConfig{TailSummarizeThresholdTokens: 6000, RecentEventsKept: 0},
			wantErrText: "recent_events_kept",
		},
		{
			name: "passes when both positive",
			cfg:  config.CorrelatorConfig{TailSummarizeThresholdTokens: 6000, RecentEventsKept: 20},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if tt.wantErrText == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErrText)
		})
	}
}

func TestLoad_ParsesCorrelatorBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unjira.config.json")
	body := `{"correlator": {"tail_summarize_threshold_tokens": 6000, "recent_events_kept": 20}}`
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	cfg, err := config.Load(path)

	require.NoError(t, err)
	assert.Equal(t, 6000, cfg.Correlator.TailSummarizeThresholdTokens)
	assert.Equal(t, 20, cfg.Correlator.RecentEventsKept)
}

func TestDefaultConfig_HasClaudeCodeEnabledByDefault(t *testing.T) {
	cfg := config.Default()

	enabled := cfg.EnabledCollectors()

	require.Contains(t, enabled, "claude_code")
	assert.Equal(t, "data/unjira.db", cfg.DBPath)
}

func TestJiraConnection_EffectiveJQLScopesToProjectKeys(t *testing.T) {
	conn := config.JiraConnection{
		Name:        "corp",
		Site:        "https://corp.atlassian.net",
		ProjectKeys: []string{"PROJ", "OPS"},
		Queries: []config.JiraQuery{
			{Name: "mine", JQL: "assignee = currentUser()"},
		},
	}

	got, err := conn.EffectiveJQL(conn.Queries[0])

	require.NoError(t, err)
	assert.Equal(t, `(assignee = currentUser()) AND project IN ("PROJ", "OPS")`, got,
		"collection must be bounded to projects the connection can also write to")
}

func TestJiraConnection_EffectiveJQLErrorsWithoutProjectKeys(t *testing.T) {
	conn := config.JiraConnection{
		Name:    "corp",
		Queries: []config.JiraQuery{{Name: "mine", JQL: "assignee = currentUser()"}},
	}

	_, err := conn.EffectiveJQL(conn.Queries[0])

	require.Error(t, err, "an unscoped query would collect issues no connection can write to")
	assert.Contains(t, err.Error(), "corp")
	assert.Contains(t, err.Error(), "project_keys")
}

func TestJiraConnection_MaxIssuesDefaultsAndRejectsNegative(t *testing.T) {
	tests := []struct {
		name       string
		configured int
		want       int
		wantErr    bool
	}{
		{name: "absent defaults", configured: 0, want: config.DefaultMaxIssuesPerQuery},
		{name: "explicit is honored", configured: 50, want: 50},
		{name: "negative is an error", configured: -1, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn := config.JiraConnection{Name: "corp", MaxIssuesPerQuery: tt.configured}

			got, err := conn.IssueLimit()

			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "max_issues_per_query")

				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestLoad_ParsesJiraQueries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unjira.config.json")
	body := `{"jira":[{"name":"corp","site":"https://corp.atlassian.net",
	  "project_keys":["PROJ"],"max_issues_per_query":50,
	  "queries":[{"name":"mine","jql":"assignee = currentUser()"}]}]}`
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	cfg, err := config.Load(path)

	require.NoError(t, err)
	require.Len(t, cfg.Jira, 1)
	require.Len(t, cfg.Jira[0].Queries, 1)
	assert.Equal(t, "mine", cfg.Jira[0].Queries[0].Name)
	assert.Equal(t, "assignee = currentUser()", cfg.Jira[0].Queries[0].JQL)
	assert.Equal(t, 50, cfg.Jira[0].MaxIssuesPerQuery)
}
