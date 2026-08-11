// Package config loads unjira's configuration. Copy
// config/unjira.example.json to ./unjira.config.json.
//
// Credentials never live in config files. They come from the environment:
// UNJIRA_JIRA_EMAIL / UNJIRA_JIRA_TOKEN, loadable from a gitignored .env. Real
// env vars win over .env. In CI, UNJIRA_JIRA_TOKEN is mapped from the
// UNJIRA_CI_TOKEN repository secret.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"

	"github.com/jcogilvie/unjira/internal/events"
)

// DefaultConfigPath is where Load looks when no path is given.
const DefaultConfigPath = "unjira.config.json"

// JiraConnection describes one Jira Cloud site and the projects on it.
// Multiple connections let a project set span more than one Jira instance —
// e.g. after a migration or an acquisition merges two orgs' Jiras — without
// unjira assuming a single global site. Name identifies the connection for
// credential lookup (see cmd/unjira's UNJIRA_JIRA_CREDENTIALS).
type JiraConnection struct {
	Name        string   `json:"name"`
	Site        string   `json:"site"`
	ProjectKeys []string `json:"project_keys"`
}

// TrackerConfig selects the phase-1+ apply-target backend and, separately,
// where a brand-new issue lands when a proposed action has no existing
// issue link to anchor it to. Site/project info for existing issues comes
// from Config.Jira + the project key at call time — TrackerConfig only
// carries what's specific to backend selection and new-issue routing.
type TrackerConfig struct {
	Backend string `json:"backend"` // "" (defaults to "jira") | "jira" | "local"
	// DefaultProject is where a new issue lands with no other routing
	// signal (smart routing from repo/component/collector is a later,
	// reconciler-level concern — this is only the configured floor).
	DefaultProject string `json:"default_project"`
}

// LLMConfig configures the OpenAI-Chat-Completions-compatible endpoint the
// phase-1 correlator/reconciler/rules packages call. Model and
// ContextWindowTokens are both required, validated by Validate — not
// defaulted or looked up. Different models have different context-window
// sizes, and there's no reliable way to query that generically across every
// possible OpenAI-compatible gateway (litellm, Azure OpenAI, OpenRouter,
// Ollama, ...); a maintained model-name->context-window lookup table would
// need constant upkeep and fail *silently wrong* for any model not yet
// added. Requiring it explicitly makes a misconfigured model a loud
// config-validation error, not a silent context-overflow risk at run time.
// BaseURL and the API key (UNJIRA_LLM_API_KEY, read in cmd/unjira, not
// here) are unjira's own explicit config, never read from ambient
// OPENAI_*/ANTHROPIC_* env vars — same precedent as UNJIRA_JIRA_CREDENTIALS.
type LLMConfig struct {
	Model               string `json:"model"`
	BaseURL             string `json:"base_url"`
	ContextWindowTokens int    `json:"context_window_tokens"`
}

// Validate reports whether Model and ContextWindowTokens are both set to
// usable values.
func (c LLMConfig) Validate() error {
	if c.Model == "" {
		return fmt.Errorf("llm.model is required")
	}
	if c.ContextWindowTokens <= 0 {
		return fmt.Errorf("llm.context_window_tokens must be a positive number of tokens")
	}

	return nil
}

// Config is unjira's top-level configuration.
type Config struct {
	Jira       []JiraConnection          `json:"jira"`
	Collectors map[string]map[string]any `json:"collectors"`
	// ExcludeFromLinking is a list of regex patterns; a ticket-key-shaped
	// match against any of them is excluded from consideration as a real
	// Jira link (see internal/events.CompileLinkExclusionPatterns). Empty by
	// default — unjira makes no assumption about any workflow's own
	// placeholder-ticket conventions.
	ExcludeFromLinking []string      `json:"exclude_from_linking"`
	Tracker            TrackerConfig `json:"tracker"`
	LLM                LLMConfig     `json:"llm"`
	DBPath             string        `json:"db_path"`
}

// JiraConnectionForProject finds the connection whose ProjectKeys contains
// projectKey. Returns false if no configured connection covers it.
func (c Config) JiraConnectionForProject(projectKey string) (JiraConnection, bool) {
	for _, conn := range c.Jira {
		if slices.Contains(conn.ProjectKeys, projectKey) {
			return conn, true
		}
	}

	return JiraConnection{}, false
}

// TrackerBackend returns the configured tracker backend, defaulting to
// "jira" when unset — backward compatible with phase-0's Jira-only
// assumption.
func (c Config) TrackerBackend() string {
	if c.Tracker.Backend == "" {
		return "jira"
	}

	return c.Tracker.Backend
}

// DefaultProjectConnection resolves Tracker.DefaultProject via
// JiraConnectionForProject, erroring loudly if unset or unresolvable —
// exactly the case a phase-2 create-action would hit with no routing logic
// upstream of it yet.
func (c Config) DefaultProjectConnection() (JiraConnection, error) {
	if c.Tracker.DefaultProject == "" {
		return JiraConnection{}, fmt.Errorf("no tracker.default_project configured for new-issue creation")
	}

	conn, ok := c.JiraConnectionForProject(c.Tracker.DefaultProject)
	if !ok {
		return JiraConnection{}, fmt.Errorf(
			"tracker.default_project %q is not covered by any configured jira connection",
			c.Tracker.DefaultProject,
		)
	}

	return conn, nil
}

// Default returns the configuration used when no config file is present:
// the claude_code collector enabled, everything else default/empty.
func Default() Config {
	return Config{
		Collectors: map[string]map[string]any{
			"claude_code": {"enabled": true},
		},
		DBPath: "data/unjira.db",
	}
}

// CompiledLinkExclusions compiles ExcludeFromLinking, failing loudly (naming
// the offending pattern) rather than silently ignoring a bad one.
func (c Config) CompiledLinkExclusions() ([]*regexp.Regexp, error) {
	return events.CompileLinkExclusionPatterns(c.ExcludeFromLinking)
}

// EnabledCollectors returns only the collector options with "enabled": true.
func (c Config) EnabledCollectors() map[string]map[string]any {
	enabled := make(map[string]map[string]any)
	for name, opts := range c.Collectors {
		if isEnabled, _ := opts["enabled"].(bool); isEnabled {
			enabled[name] = opts
		}
	}

	return enabled
}

// Load reads a config file, falling back to Default when path does not
// exist. An empty path uses DefaultConfigPath.
func Load(path string) (Config, error) {
	if path == "" {
		path = DefaultConfigPath
	}

	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Default(), nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("reading config %s: %w", path, err)
	}

	var cfg Config
	if err := json.Unmarshal(body, &cfg); err != nil {
		return Config{}, fmt.Errorf("parsing config %s: %w", path, err)
	}

	return cfg, nil
}
