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

// Config is unjira's top-level configuration.
type Config struct {
	Jira       []JiraConnection          `json:"jira"`
	Collectors map[string]map[string]any `json:"collectors"`
	// ExcludeFromLinking is a list of regex patterns; a ticket-key-shaped
	// match against any of them is excluded from consideration as a real
	// Jira link (see internal/events.CompileLinkExclusionPatterns). Empty by
	// default — unjira makes no assumption about any workflow's own
	// placeholder-ticket conventions.
	ExcludeFromLinking []string `json:"exclude_from_linking"`
	DBPath             string   `json:"db_path"`
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
