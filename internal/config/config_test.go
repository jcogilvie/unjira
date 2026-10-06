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
	assert.Empty(t, cfg.Trackers)
	assert.Equal(t, "data/unjira.db", cfg.DBPath)
	assert.Empty(t, cfg.ExcludeFromLinking)

	enabled := cfg.EnabledCollectors()
	require.Contains(t, enabled, "claude_code")
	assert.Equal(t, true, enabled["claude_code"]["enabled"])
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

func TestLoad_MemberConfidenceFloorParsesAndDefaultsToZeroOff(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unjira.config.json")
	require.NoError(t, os.WriteFile(path, []byte(
		`{"correlator": {"tail_summarize_threshold_tokens": 6000, "recent_events_kept": 20}}`), 0o600))

	cfg, err := config.Load(path)
	require.NoError(t, err)
	assert.Zero(t, cfg.Correlator.MemberConfidenceFloor, "ships off: a model's stated confidence is uncalibrated")

	require.NoError(t, os.WriteFile(path, []byte(
		`{"correlator": {"tail_summarize_threshold_tokens": 6000, "recent_events_kept": 20, `+
			`"member_confidence_floor": 0.6}}`), 0o600))

	cfg, err = config.Load(path)
	require.NoError(t, err)
	assert.InDelta(t, 0.6, cfg.Correlator.MemberConfidenceFloor, 1e-9)
}

func TestCorrelatorConfig_ValidatesMemberConfidenceFloor(t *testing.T) {
	valid := config.CorrelatorConfig{TailSummarizeThresholdTokens: 6000, RecentEventsKept: 20}

	for _, floor := range []float64{0, 0.5, 1} {
		c := valid
		c.MemberConfidenceFloor = floor
		require.NoError(t, c.Validate(), "floor %v", floor)
	}

	for _, floor := range []float64{-0.1, 1.5} {
		c := valid
		c.MemberConfidenceFloor = floor
		require.ErrorContains(t, c.Validate(), "correlator.member_confidence_floor", "floor %v", floor)
	}
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
