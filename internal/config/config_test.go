package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

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
			{
				Name: "default", Site: "https://yourorg.atlassian.net",
				ProjectKeys: []string{"PROJ"}, WritableProjectKeys: []string{"PROJ"},
			},
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
		{
			// Zero is "send no cap", not invalid — some gateways reject a cap
			// above their own ceiling, so unjira must be able to send none.
			name: "zero max output tokens is valid",
			cfg: config.LLMConfig{
				Model: "gpt-5-2", ContextWindowTokens: 128000, MaxOutputTokens: 0,
			},
		},
		{
			name: "explicit max output tokens is valid",
			cfg: config.LLMConfig{
				Model: "gpt-5-2", ContextWindowTokens: 128000, MaxOutputTokens: 32000,
			},
		},
		{
			// Negative most likely means someone reaching for "unlimited",
			// which this does not offer.
			name: "rejects negative max output tokens",
			cfg: config.LLMConfig{
				Model: "gpt-5-2", ContextWindowTokens: 128000, MaxOutputTokens: -1,
			},
			wantErrText: "max_output_tokens",
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
		{
			name: "max_context_narratives zero (unlimited) is valid",
			cfg: config.CorrelatorConfig{
				TailSummarizeThresholdTokens: 6000, RecentEventsKept: 20, MaxContextNarratives: 0,
			},
		},
		{
			name: "max_context_narratives positive is valid",
			cfg: config.CorrelatorConfig{
				TailSummarizeThresholdTokens: 6000, RecentEventsKept: 20, MaxContextNarratives: 40,
			},
		},
		{
			name: "max_context_narratives negative is always a mistake",
			cfg: config.CorrelatorConfig{
				TailSummarizeThresholdTokens: 6000, RecentEventsKept: 20, MaxContextNarratives: -1,
			},
			wantErrText: "max_context_narratives",
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
	body := `{"correlator": {"tail_summarize_threshold_tokens": 6000, "recent_events_kept": 20, ` +
		`"max_context_narratives": 40}}`
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	cfg, err := config.Load(path)

	require.NoError(t, err)
	assert.Equal(t, 6000, cfg.Correlator.TailSummarizeThresholdTokens)
	assert.Equal(t, 20, cfg.Correlator.RecentEventsKept)
	assert.Equal(t, 40, cfg.Correlator.MaxContextNarratives)
}

func TestLoad_MaxContextNarrativesDefaultsToZeroUnlimited(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unjira.config.json")
	body := `{"correlator": {"tail_summarize_threshold_tokens": 6000, "recent_events_kept": 20}}`
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	cfg, err := config.Load(path)

	require.NoError(t, err)
	assert.Zero(t, cfg.Correlator.MaxContextNarratives,
		"the bound ships inert until an operator opts in, matching MaxEventSummaryChars/MaxOutputTokens")
}

func TestMatchConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     config.MatchConfig
		wantErr string // substring; "" means no error expected
	}{
		{name: "zero max means default", cfg: config.MatchConfig{ConfidenceFloor: 0.7}},
		{name: "explicit max", cfg: config.MatchConfig{MaxCandidatesPerNarrative: 5, ConfidenceFloor: 0.7}},
		{name: "floor zero is valid", cfg: config.MatchConfig{ConfidenceFloor: 0}},
		{name: "floor one is valid", cfg: config.MatchConfig{ConfidenceFloor: 1}},
		{
			name:    "negative max",
			cfg:     config.MatchConfig{MaxCandidatesPerNarrative: -1},
			wantErr: "max_candidates_per_narrative",
		},
		{
			name:    "floor above one never promotes",
			cfg:     config.MatchConfig{ConfidenceFloor: 1.5},
			wantErr: "confidence_floor",
		},
		{
			name:    "negative floor",
			cfg:     config.MatchConfig{ConfidenceFloor: -0.1},
			wantErr: "confidence_floor",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()

			if tt.wantErr == "" {
				require.NoError(t, err)

				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr,
				"the message must name the JSON key so an operator can find it")
		})
	}
}

func TestMatchConfig_CandidateLimitDefaults(t *testing.T) {
	assert.Equal(t, config.DefaultMaxCandidatesPerNarrative,
		config.MatchConfig{}.CandidateLimit())
	assert.Equal(t, 5, config.MatchConfig{MaxCandidatesPerNarrative: 5}.CandidateLimit())
}

func TestLoad_ParsesMatchBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unjira.config.json")
	body := `{"match":{"max_candidates_per_narrative":7,"confidence_floor":0.8}}`
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	cfg, err := config.Load(path)

	require.NoError(t, err)
	assert.Equal(t, 7, cfg.Match.MaxCandidatesPerNarrative)
	assert.InDelta(t, 0.8, cfg.Match.ConfidenceFloor, 1e-9)
}

func TestLLMConfig_ResolvedAPIKeyHelper(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)

	t.Run("empty stays empty", func(t *testing.T) {
		got, err := config.LLMConfig{}.ResolvedAPIKeyHelper()
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("absolute path passes through", func(t *testing.T) {
		got, err := config.LLMConfig{APIKeyHelper: "/usr/local/bin/get-key"}.ResolvedAPIKeyHelper()
		require.NoError(t, err)
		assert.Equal(t, "/usr/local/bin/get-key", got)
	})

	t.Run("leading tilde expands", func(t *testing.T) {
		// The conventional way to write such a path; leaving it literal would
		// make sh report only "no such file" and send the operator hunting
		// their helper rather than their config.
		got, err := config.LLMConfig{APIKeyHelper: "~/.local/bin/get-key"}.ResolvedAPIKeyHelper()
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(home, ".local/bin/get-key"), got)
	})

	t.Run("bare tilde is not expanded", func(t *testing.T) {
		// Only the "~/" prefix is a home reference. A path merely containing a
		// tilde ("~backup/bin/x", a username-style form) must be left alone
		// rather than mangled into something that silently resolves elsewhere.
		got, err := config.LLMConfig{APIKeyHelper: "~backup/bin/get-key"}.ResolvedAPIKeyHelper()
		require.NoError(t, err)
		assert.Equal(t, "~backup/bin/get-key", got)
	})
}

func TestLoad_ParsesAPIKeyHelper(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unjira.config.json")
	body := `{"llm":{"model":"m","context_window_tokens":1,"api_key_helper":"~/bin/k"}}`
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	cfg, err := config.Load(path)

	require.NoError(t, err)
	assert.Equal(t, "~/bin/k", cfg.LLM.APIKeyHelper, "stored verbatim; expansion is a read-time concern")
}

func TestReconcilerConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     config.ReconcilerConfig
		wantErr string // substring; "" means no error expected
	}{
		{name: "zero max means default", cfg: config.ReconcilerConfig{MinConfidenceToPropose: 0.5}},
		{
			name: "explicit max",
			cfg:  config.ReconcilerConfig{MaxNarrativesPerPass: 5, MinConfidenceToPropose: 0.5},
		},
		{name: "threshold zero is valid", cfg: config.ReconcilerConfig{MinConfidenceToPropose: 0}},
		{name: "threshold one is valid", cfg: config.ReconcilerConfig{MinConfidenceToPropose: 1}},
		{
			name:    "negative max",
			cfg:     config.ReconcilerConfig{MaxNarrativesPerPass: -1, MinConfidenceToPropose: 0.5},
			wantErr: "max_narratives_per_pass",
		},
		{
			name:    "threshold above one suppresses every proposal",
			cfg:     config.ReconcilerConfig{MaxNarrativesPerPass: 10, MinConfidenceToPropose: 1.5},
			wantErr: "min_confidence_to_propose",
		},
		{
			name:    "negative threshold",
			cfg:     config.ReconcilerConfig{MaxNarrativesPerPass: 10, MinConfidenceToPropose: -0.1},
			wantErr: "min_confidence_to_propose",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()

			if tt.wantErr == "" {
				require.NoError(t, err)

				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr,
				"the message must name the JSON key so an operator can find it")
		})
	}
}

func TestReconcilerConfig_NarrativeLimitDefaults(t *testing.T) {
	assert.Equal(t, config.DefaultMaxNarrativesPerPass,
		config.ReconcilerConfig{}.NarrativeLimit())
	assert.Equal(t, 5, config.ReconcilerConfig{MaxNarrativesPerPass: 5}.NarrativeLimit())
}

func TestLoad_ParsesReconcilerBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unjira.config.json")
	body := `{"reconciler":{"max_narratives_per_pass":7,"min_confidence_to_propose":0.8}}`
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	cfg, err := config.Load(path)

	require.NoError(t, err)
	assert.Equal(t, 7, cfg.Reconciler.MaxNarrativesPerPass)
	assert.InDelta(t, 0.8, cfg.Reconciler.MinConfidenceToPropose, 1e-9)
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

func TestConfig_RulesDirDefaultsWhenUnset(t *testing.T) {
	cfg := config.Config{}

	assert.Equal(t, "rules", cfg.RulesDir())
}

func TestConfig_RulesDirHonorsConfigured(t *testing.T) {
	cfg := config.Config{Rules: config.RulesConfig{Dir: "/etc/unjira/rules"}}

	assert.Equal(t, "/etc/unjira/rules", cfg.RulesDir())
}

func TestLoad_ParsesRulesBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unjira.config.json")
	body := `{"rules": {"dir": "/etc/unjira/rules"}}`
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	cfg, err := config.Load(path)

	require.NoError(t, err)
	assert.Equal(t, "/etc/unjira/rules", cfg.Rules.Dir)
}

// TestLoad_ParsesWorkflowCacheTTL asserts workflow.cache_ttl round-trips
// through config.Span the same way every other duration-ish key does
// (config.Span already covers day/week units and positivity in
// internal/config/span_test.go) — this only needs to prove the new field is
// actually wired into Config's JSON tags, not re-prove Span's own parsing.
func TestLoad_ParsesWorkflowCacheTTL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unjira.config.json")
	body := `{"workflow": {"cache_ttl": "48h"}}`
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	cfg, err := config.Load(path)

	require.NoError(t, err)
	assert.Equal(t, 48*time.Hour, cfg.Workflow.CacheTTL.Duration())
}

// TestLoad_WorkflowCacheTTLDefaultsToZeroWhenUnset documents the "0 means
// use the package default" contract explicitly: internal/config
// deliberately does not duplicate workflow.DefaultCacheTTL as its own
// fallback value (that constant lives in internal/workflow, and config must
// not import it — see workflow.StatusChange's own doc comment on why the
// dependency direction stays clients/jira -> workflow, never the reverse,
// which extends to config here too). A caller wires
// cfg.Workflow.CacheTTL.Duration() straight into workflow.CacheOptions.TTL,
// and workflow.Cached treats <= 0 as "use my own default".
func TestLoad_WorkflowCacheTTLDefaultsToZeroWhenUnset(t *testing.T) {
	cfg, err := config.Load(filepath.Join(t.TempDir(), "missing.json"))

	require.NoError(t, err)
	assert.Zero(t, cfg.Workflow.CacheTTL.Duration())
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

// TestConfig_AutoCommitDefaultsToQueueEverything is the safety property the
// whole auto-commit gate rests on: a zero-value Config — no auto_commit block
// at all, exactly what an untouched unjira.config.json produces — must queue
// every action type rather than apply any of them, no matter how high its
// confidence. Go's zero value for AutoCommitRule.Graduated (false) gives this
// for free, but the property is worth asserting explicitly because a future
// "sensible defaults" refactor could silently break it.
func TestConfig_AutoCommitDefaultsToQueueEverything(t *testing.T) {
	var cfg config.Config

	for _, actionType := range []string{"comment", "transition", "create"} {
		rule := cfg.AutoCommit[actionType]
		assert.False(t, rule.Graduated,
			"action type %q must default to Graduated=false: an unconfigured "+
				"action type must never auto-apply", actionType)
	}
}

// TestConfig_AutoCommitUnknownActionTypeDefaultsToZeroRule covers the same
// property for a type that isn't even in the closed comment/transition/create
// set — a map lookup miss returns the zero AutoCommitRule regardless of key,
// so this is really the same guarantee as the test above from a different
// angle: absence from config, by any name, never grants apply authority.
func TestConfig_AutoCommitUnknownActionTypeDefaultsToZeroRule(t *testing.T) {
	cfg := config.Config{
		AutoCommit: map[string]config.AutoCommitRule{
			"comment": {ConfidenceFloor: 0.5, Graduated: true},
		},
	}

	rule := cfg.AutoCommit["estimate"]

	assert.False(t, rule.Graduated)
	assert.Zero(t, rule.ConfidenceFloor)
}

func TestAutoCommitRule_Validate(t *testing.T) {
	tests := []struct {
		name    string
		rule    config.AutoCommitRule
		wantErr string // substring; "" means no error expected
	}{
		{name: "floor zero is valid", rule: config.AutoCommitRule{ConfidenceFloor: 0}},
		{name: "floor one is valid", rule: config.AutoCommitRule{ConfidenceFloor: 1}},
		{
			name:    "floor above one",
			rule:    config.AutoCommitRule{ConfidenceFloor: 1.5},
			wantErr: "confidence_floor",
		},
		{
			name:    "negative floor",
			rule:    config.AutoCommitRule{ConfidenceFloor: -0.1},
			wantErr: "confidence_floor",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.rule.Validate()

			if tt.wantErr == "" {
				require.NoError(t, err)

				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr,
				"the message must name the JSON key so an operator can find it")
		})
	}
}

func TestLoad_ParsesAutoCommitBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unjira.config.json")
	body := `{"auto_commit":{"comment":{"confidence_floor":0.9,"graduated":true},
	  "transition":{"confidence_floor":0.95,"graduated":false}}}`
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	cfg, err := config.Load(path)

	require.NoError(t, err)
	require.Contains(t, cfg.AutoCommit, "comment")
	assert.InDelta(t, 0.9, cfg.AutoCommit["comment"].ConfidenceFloor, 1e-9)
	assert.True(t, cfg.AutoCommit["comment"].Graduated)
	require.Contains(t, cfg.AutoCommit, "transition")
	assert.InDelta(t, 0.95, cfg.AutoCommit["transition"].ConfidenceFloor, 1e-9)
	assert.False(t, cfg.AutoCommit["transition"].Graduated)
}

// TestLoad_MissingAutoCommitBlockDefaultsToNilMap exercises the exact shape a
// pre-slice-5 config file has: no auto_commit key at all. Load must not
// synthesize any entries — a nil map, matching the zero-value safety property
// above.
func TestLoad_MissingAutoCommitBlockDefaultsToNilMap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unjira.config.json")
	require.NoError(t, os.WriteFile(path, []byte(`{}`), 0o600))

	cfg, err := config.Load(path)

	require.NoError(t, err)
	assert.Nil(t, cfg.AutoCommit)
}

// TestJiraConnection_IsProjectWritable_EmptyWritableSetDeniesEverything is the
// write-scope safety property, at the same level TestConfig_AutoCommitDefaultsToQueueEverything
// asserts for AutoCommitRule.Graduated: absent/empty WritableProjectKeys must
// deny EVERY project, including one the connection can read (ProjectKeys) —
// per docs/superpowers/specs/2026-08-27-write-scope-design.md's rejection of
// "inherit project_keys when unset".
func TestJiraConnection_IsProjectWritable_EmptyWritableSetDeniesEverything(t *testing.T) {
	conn := config.JiraConnection{
		Name:        "dev",
		ProjectKeys: []string{"PAAS", "DEVSBX"},
		// WritableProjectKeys deliberately unset.
	}

	assert.False(t, conn.IsProjectWritable("PAAS"))
	assert.False(t, conn.IsProjectWritable("DEVSBX"))
}

// TestJiraConnection_IsProjectWritable_OnlyListedProjectsAreWritable proves
// the field is checked, not merely present: DEVSBX (listed) must be writable
// and PAAS (readable but not listed) must not be — this is the exact shape of
// the real unjira.config.json this design exists to fix.
func TestJiraConnection_IsProjectWritable_OnlyListedProjectsAreWritable(t *testing.T) {
	conn := config.JiraConnection{
		Name:                "dev",
		ProjectKeys:         []string{"PAAS", "DEVSBX"},
		WritableProjectKeys: []string{"DEVSBX"},
	}

	assert.True(t, conn.IsProjectWritable("DEVSBX"))
	assert.False(t, conn.IsProjectWritable("PAAS"))
	assert.False(t, conn.IsProjectWritable("GHOST"), "a project this connection doesn't even read must not be writable")
}

// TestJiraConnection_ValidateWriteScope_SubsetIsValid is the happy path for
// the startup check: every writable key also appears in project_keys.
func TestJiraConnection_ValidateWriteScope_SubsetIsValid(t *testing.T) {
	conn := config.JiraConnection{
		Name:                "dev",
		ProjectKeys:         []string{"PAAS", "DEVSBX"},
		WritableProjectKeys: []string{"DEVSBX"},
	}

	assert.NoError(t, conn.ValidateWriteScope())
}

// TestJiraConnection_ValidateWriteScope_EmptyIsValid covers the deny-by-
// default case explicitly: an absent WritableProjectKeys must not itself be a
// validation error — it is the safe, expected default, not a misconfiguration.
func TestJiraConnection_ValidateWriteScope_EmptyIsValid(t *testing.T) {
	conn := config.JiraConnection{Name: "dev", ProjectKeys: []string{"PAAS"}}

	assert.NoError(t, conn.ValidateWriteScope())
}

// TestJiraConnection_ValidateWriteScope_NotASubsetErrorsNamingBoth is test 5
// from the design doc's test list: a writable project the connection cannot
// even read is a loud, named config error at startup, not something that
// loads silently and fails (or, worse, half-succeeds) at write time.
func TestJiraConnection_ValidateWriteScope_NotASubsetErrorsNamingBoth(t *testing.T) {
	conn := config.JiraConnection{
		Name:                "dev",
		ProjectKeys:         []string{"DEVSBX"},
		WritableProjectKeys: []string{"DEVSBX", "PAAS"},
	}

	err := conn.ValidateWriteScope()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "PAAS", "must name the offending writable project")
	assert.Contains(t, err.Error(), "dev", "must name the connection")
	assert.Contains(t, err.Error(), "writable_project_keys")
	assert.Contains(t, err.Error(), "project_keys")
}

func TestLoad_ParsesWritableProjectKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unjira.config.json")
	body := `{"jira":[{"name":"dev","site":"https://x.atlassian.net",
	  "project_keys":["PAAS","DEVSBX"],"writable_project_keys":["DEVSBX"]}]}`
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	cfg, err := config.Load(path)

	require.NoError(t, err)
	require.Len(t, cfg.Jira, 1)
	assert.Equal(t, []string{"DEVSBX"}, cfg.Jira[0].WritableProjectKeys)
}

// TestLoad_MissingWritableProjectKeysDefaultsToNil is the JSON-shape half of
// the deny-by-default property: an omitted writable_project_keys key must
// decode to a nil slice, not an empty-but-present one — both behave the same
// under IsProjectWritable, but this pins the JSON round-trip explicitly since
// it is the shape every pre-existing config file (including the real
// unjira.config.json before this change) actually has.
func TestLoad_MissingWritableProjectKeysDefaultsToNil(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unjira.config.json")
	body := `{"jira":[{"name":"dev","site":"https://x.atlassian.net","project_keys":["PAAS"]}]}`
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	cfg, err := config.Load(path)

	require.NoError(t, err)
	require.Len(t, cfg.Jira, 1)
	assert.Nil(t, cfg.Jira[0].WritableProjectKeys)
	assert.False(t, cfg.Jira[0].IsProjectWritable("PAAS"),
		"a project_keys-covered but unlisted-for-write project must not be writable")
}

// TestDefaultProjectConnection_UnwritableDefaultProjectErrors is test 6 from
// the design doc: DefaultProjectConnection is where `create`'s target is
// statically validated (tracker.default_project is config, not a per-action
// runtime value), so an unwritable default project must be a startup error
// here — the same function TestDefaultProjectConnection_SetButUncoveredErrors
// already uses for the "uncovered" case, extended to also check writability.
func TestDefaultProjectConnection_UnwritableDefaultProjectErrors(t *testing.T) {
	cfg := config.Config{
		Tracker: config.TrackerConfig{DefaultProject: "PAAS"},
		Jira: []config.JiraConnection{
			{
				Name: "dev", Site: "https://x.atlassian.net",
				ProjectKeys:         []string{"PAAS", "DEVSBX"},
				WritableProjectKeys: []string{"DEVSBX"},
			},
		},
	}

	_, err := cfg.DefaultProjectConnection()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "PAAS")
	assert.Contains(t, err.Error(), "writable_project_keys")
}

// TestDefaultProjectConnection_WritableDefaultProjectSucceeds is the
// unwritable test's positive counterpart, proving the added check does not
// regress the existing happy path.
func TestDefaultProjectConnection_WritableDefaultProjectSucceeds(t *testing.T) {
	cfg := config.Config{
		Tracker: config.TrackerConfig{DefaultProject: "DEVSBX"},
		Jira: []config.JiraConnection{
			{
				Name: "dev", Site: "https://x.atlassian.net",
				ProjectKeys:         []string{"PAAS", "DEVSBX"},
				WritableProjectKeys: []string{"DEVSBX"},
			},
		},
	}

	conn, err := cfg.DefaultProjectConnection()

	require.NoError(t, err)
	assert.Equal(t, "dev", conn.Name)
}
