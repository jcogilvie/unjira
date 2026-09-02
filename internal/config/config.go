// Package config loads unjira's configuration. Copy
// config/unjira.example.json to ./unjira.config.json.
//
// Credentials never live in config files. They come from the environment, as
// UNJIRA_JIRA_CREDENTIALS: one JSON object mapping each Jira connection's name
// to its {email, token} pair, so the variable count does not grow with the
// number of configured connections. internal/envfile loads a gitignored .env
// from the repository root, and real environment variables win over it.
//
// In CI, UNJIRA_JIRA_CREDENTIALS is composed from the UNJIRA_CI_EMAIL variable
// and the UNJIRA_CI_TOKEN secret — see .github/workflows/ci.yml.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/jcogilvie/unjira/internal/events"
)

// DefaultConfigPath is where Load looks when no path is given.
const DefaultConfigPath = "unjira.config.json"

// DefaultMaxIssuesPerQuery bounds how many issues one collector query examines
// per pass. A limit is required rather than optional: the Jira collector makes
// two API calls per issue (changelog and comments, since the search endpoint
// cannot expand either), so an unbounded query is unbounded API cost.
const DefaultMaxIssuesPerQuery = 200

// JiraQuery is one named JQL view of a connection's issues. Named rather than a
// single string per connection because one Jira site legitimately has several
// views worth collecting, and duplicating site plus credentials to get them
// would be the wrong shape.
//
// The name is also the cursor key (see internal/collector/jira), so renaming a
// query resets only that query's watermark.
type JiraQuery struct {
	Name string `json:"name"`
	JQL  string `json:"jql"`
}

// JiraConnection describes one Jira Cloud site and the projects on it.
// Multiple connections let a project set span more than one Jira instance —
// e.g. after a migration or an acquisition merges two orgs' Jiras — without
// unjira assuming a single global site. Name identifies the connection for
// credential lookup (see cmd/unjira's UNJIRA_JIRA_CREDENTIALS). ProjectKeys
// bounds what its Queries may collect (for reads; see EffectiveJQL) and which
// projects JiraConnectionForProject resolves this connection's site/credential
// for. WritableProjectKeys is the separate, narrower write authorization —
// see its own doc comment.
type JiraConnection struct {
	Name        string   `json:"name"`
	Site        string   `json:"site"`
	ProjectKeys []string `json:"project_keys"`
	// WritableProjectKeys is which of this connection's ProjectKeys unjira may
	// actually WRITE to — the choke point gate.Applier consults before any
	// AddComment/SetStatus/CreateIssue call. Declared independently of
	// ProjectKeys, which only answers "read scope, and which connection's
	// site/credential to use."
	//
	// Absent or empty means NOTHING on this connection is writable — deny by
	// default, mirroring AutoCommitRule.Graduated's own zero-value safety
	// property. Deliberately NOT "defaults to ProjectKeys when unset": that
	// would mean every project unjira reads is armed for writes the moment a
	// connection is configured at all, which is the exact bug write scope
	// exists to fix rather than a safe default to fall back to. See
	// docs/superpowers/specs/2026-08-27-write-scope-design.md.
	//
	// Must be a subset of ProjectKeys — ValidateWriteScope checks this at
	// startup, since a writable project this connection cannot even read
	// would leave JiraConnectionForProject unable to resolve a site for it at
	// all.
	WritableProjectKeys []string `json:"writable_project_keys"`
	// Queries are the named JQL views the Jira collector reads. Empty means
	// this connection is write-only: it routes project keys but collects
	// nothing.
	Queries []JiraQuery `json:"queries"`
	// MaxIssuesPerQuery bounds one query's issue count per pass. Zero means
	// DefaultMaxIssuesPerQuery.
	MaxIssuesPerQuery int `json:"max_issues_per_query"`
}

// IsProjectWritable reports whether projectKey is in WritableProjectKeys —
// the single question gate.Applier asks before every tracker write. An empty
// or absent WritableProjectKeys answers false for every project, including
// one this connection reads via ProjectKeys: see WritableProjectKeys' own doc
// comment for why that is the deliberate default rather than a gap.
func (c JiraConnection) IsProjectWritable(projectKey string) bool {
	return slices.Contains(c.WritableProjectKeys, projectKey)
}

// ValidateWriteScope rejects a WritableProjectKeys entry this connection
// cannot even read — the second of write scope's two layers (the first is
// gate.Applier's per-action runtime check), catching a statically-checkable
// misconfiguration at startup rather than leaving a writable-but-unreachable
// project to fail in some stranger way whenever a write is actually
// attempted. An empty or absent WritableProjectKeys is valid: it is the safe
// default, not a misconfiguration to flag.
func (c JiraConnection) ValidateWriteScope() error {
	for _, writable := range c.WritableProjectKeys {
		if !slices.Contains(c.ProjectKeys, writable) {
			return fmt.Errorf(
				"jira connection %q: writable_project_keys includes %q, which is not in "+
					"project_keys — a project must be readable by this connection before it can "+
					"be declared writable",
				c.Name, writable,
			)
		}
	}

	return nil
}

// EffectiveJQL returns query's JQL scoped to this connection's ProjectKeys.
//
// The scope is added rather than left to the operator because the two lists
// answer different questions that must not disagree: ProjectKeys says which
// projects this connection can write to, and an unscoped JQL (assignee =
// currentUser(), say) spans a whole site. Collecting an issue from a project no
// connection covers would narrate work the reconciler can never act on —
// JiraConnectionForProject would return false when it came time to write.
//
// Empty ProjectKeys is an error rather than "collect everything", for the same
// reason.
func (c JiraConnection) EffectiveJQL(query JiraQuery) (string, error) {
	if len(c.ProjectKeys) == 0 {
		return "", fmt.Errorf(
			"jira connection %q has no project_keys: cannot scope collector query %q, and an "+
				"unscoped query would collect issues no connection can write to",
			c.Name, query.Name,
		)
	}

	quoted := make([]string, 0, len(c.ProjectKeys))
	for _, key := range c.ProjectKeys {
		quoted = append(quoted, fmt.Sprintf("%q", key))
	}

	return fmt.Sprintf("(%s) AND project IN (%s)", query.JQL, strings.Join(quoted, ", ")), nil
}

// IssueLimit returns the per-query issue cap, defaulting when unset. A negative
// value is a configuration error rather than silently coerced: it most likely
// means someone intended "no limit", which this collector deliberately does not
// offer.
func (c JiraConnection) IssueLimit() (int, error) {
	switch {
	case c.MaxIssuesPerQuery < 0:
		return 0, fmt.Errorf(
			"jira connection %q has max_issues_per_query %d: must be positive, or omitted for the default of %d",
			c.Name, c.MaxIssuesPerQuery, DefaultMaxIssuesPerQuery,
		)
	case c.MaxIssuesPerQuery == 0:
		return DefaultMaxIssuesPerQuery, nil
	default:
		return c.MaxIssuesPerQuery, nil
	}
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
	// MaxOutputTokens caps each response's length. Zero sends no cap and lets
	// the gateway impose its own, which is a documented hazard rather than a
	// neutral default: a litellm-fronted claude-sonnet-5 silently capped output
	// at 4096 while advertising max_output_tokens=128000, truncating a
	// clustering reply mid-JSON. Every phase-1 prompt asks for JSON, and a
	// truncated reply that happens to parse would silently drop narratives,
	// matches, or proposed actions.
	//
	// Not defaulted, for the same reason ContextWindowTokens is not: the right
	// ceiling is model- and gateway-specific, and a stale built-in table would
	// fail silently wrong. Left optional rather than required only because some
	// gateways reject a cap above their own ceiling, so unjira must be able to
	// send none. openai.Complete errors loudly if a response is truncated, so
	// an unset cap degrades to a clear failure rather than silent data loss.
	MaxOutputTokens int `json:"max_output_tokens"`
	// APIKeyHelper is a command whose stdout is the LLM credential. When set it
	// takes precedence over UNJIRA_LLM_API_KEY, and is run again whenever the
	// credential needs refreshing.
	//
	// A path belongs in config even though a credential never does: this is not
	// itself a secret, any more than BaseURL is. The rule it must not break is
	// that the *credential* never appears in a config file — and it does not,
	// only the means of obtaining one. Same shape as Claude Code's own
	// apiKeyHelper.
	//
	// Exists because a static key cannot survive a long pass against a gateway
	// issuing short-lived tokens: slice 4's verification died at its final stage
	// on a 401, having already paid for every earlier stage. See
	// docs/superpowers/specs/2026-08-26-llm-credential-helper-design.md.
	//
	// A leading ~ expands to the user's home directory, since that is how such a
	// path is conventionally written and silently failing on it would be a poor
	// surprise.
	APIKeyHelper string `json:"api_key_helper"`
}

// ResolvedAPIKeyHelper returns APIKeyHelper with a leading ~ expanded to the
// user's home directory, and "" when no helper is configured.
//
// Expansion happens here rather than at the exec site so every caller agrees on
// what the configured string means. An unexpandable ~ is an error rather than a
// silent pass-through: `sh` would fail to find a literal "~/..." path and report
// only "no such file", which sends the operator looking at their helper instead
// of at their config.
func (c LLMConfig) ResolvedAPIKeyHelper() (string, error) {
	if c.APIKeyHelper == "" {
		return "", nil
	}

	if !strings.HasPrefix(c.APIKeyHelper, "~/") {
		return c.APIKeyHelper, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf(
			"expanding ~ in llm.api_key_helper %q: %w", c.APIKeyHelper, err,
		)
	}

	return filepath.Join(home, strings.TrimPrefix(c.APIKeyHelper, "~/")), nil
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
	// Negative is rejected while zero is allowed: zero means "send no cap"
	// (see MaxOutputTokens), but a negative value is always a mistake, most
	// likely someone reaching for "unlimited" — which this does not offer.
	if c.MaxOutputTokens < 0 {
		return fmt.Errorf(
			"llm.max_output_tokens is %d: must be positive, or omitted to send no cap",
			c.MaxOutputTokens,
		)
	}

	return nil
}

// CorrelatorConfig configures narrative persistence and compaction. Both
// fields are required (positive), validated by Validate. See
// docs/superpowers/specs/2026-08-12-correlator-persist-design.md.
type CorrelatorConfig struct {
	// TailSummarizeThresholdTokens: once a narrative's post-boundary history
	// (recap + raw tail) estimates above this, Persist compacts the old tail
	// into the summary's recap prefix via one LLM call.
	TailSummarizeThresholdTokens int `json:"tail_summarize_threshold_tokens"`
	// RecentEventsKept: how many of the newest events stay raw after a
	// compaction — a count, so it's well-defined regardless of event density.
	RecentEventsKept int `json:"recent_events_kept"`
}

// Validate reports whether both correlator limits are set to usable values.
func (c CorrelatorConfig) Validate() error {
	if c.TailSummarizeThresholdTokens <= 0 {
		return fmt.Errorf("correlator.tail_summarize_threshold_tokens must be a positive number of tokens")
	}
	if c.RecentEventsKept <= 0 {
		return fmt.Errorf("correlator.recent_events_kept must be a positive count")
	}

	return nil
}

// DefaultMaxCandidatesPerNarrative bounds how many candidate issue keys one
// narrative's matching pass examines. A limit is required rather than optional
// because each candidate costs a GetIssue call and a slot in the LLM prompt, so
// a narrative that mentions thirty tickets would otherwise be unboundedly
// expensive.
const DefaultMaxCandidatesPerNarrative = 10

// DefaultMaxNarrativesPerMatchPass bounds how many narratives one matching pass
// examines, mirroring DefaultMaxNarrativesPerPass for the reconciler.
//
// A separate constant rather than reusing the reconciler's: the two stages have
// different per-narrative costs. Matching spends one GetIssue per candidate and
// an LLM call only when 2+ candidates survive verification — a lone candidate is
// resolved deterministically for free — while the reconciler spends a GetIssue
// per link plus a drafting call for every narrative it examines. Tying them to
// one number would mean tuning the cheaper stage by the expensive one's budget.
const DefaultMaxNarrativesPerMatchPass = 20

// MatchConfig tunes narrative→issue matching. See
// docs/superpowers/specs/2026-08-24-narrative-issue-matching-design.md.
type MatchConfig struct {
	// MaxCandidatesPerNarrative caps candidates examined per narrative. Zero
	// means DefaultMaxCandidatesPerNarrative.
	MaxCandidatesPerNarrative int `json:"max_candidates_per_narrative"`
	// MaxNarrativesPerPass caps how many unmatched narratives one pass
	// examines. Zero means DefaultMaxNarrativesPerMatchPass.
	//
	// Distinct from MaxCandidatesPerNarrative, and that distinction is why this
	// field exists: correlator.Match used the candidate cap as the narrative cap,
	// so a config setting max_candidates_per_narrative=10 silently examined only
	// 10 narratives per pass. On a 30-narrative backlog that left 20 unmatched,
	// which then reached the create path as untracked work and drew proposals for
	// new tickets duplicating issues those narratives already named.
	MaxNarrativesPerPass int `json:"max_narratives_per_pass"`
	// ConfidenceFloor governs only whether a primary is promoted into the
	// denormalized narratives.issue_key. Below it, every narrative_issues row
	// is still written — including the primary — but narratives.issue_key stays
	// NULL. The floor governs what unjira asserts, not what it records:
	// dropping the rows would make a low-confidence match indistinguishable
	// from finding nothing at all.
	ConfidenceFloor float64 `json:"confidence_floor"`
}

// CandidateLimit returns the effective per-narrative candidate cap.
func (c MatchConfig) CandidateLimit() int {
	if c.MaxCandidatesPerNarrative == 0 {
		return DefaultMaxCandidatesPerNarrative
	}

	return c.MaxCandidatesPerNarrative
}

// NarrativeLimit returns the effective per-pass narrative cap.
//
// Named to match ReconcilerConfig.NarrativeLimit so the two stages read the same
// way at their call sites — the previous code's single `limit` variable serving
// both roles is exactly how the two got conflated.
func (c MatchConfig) NarrativeLimit() int {
	if c.MaxNarrativesPerPass == 0 {
		return DefaultMaxNarrativesPerMatchPass
	}

	return c.MaxNarrativesPerPass
}

// Validate rejects configurations that would fail silently at runtime.
//
// A ConfidenceFloor above 1 is the important case: confidence is a 0..1 score,
// so a floor of, say, 1.5 would never promote any primary, and matching would
// present as broken rather than as misconfigured.
func (c MatchConfig) Validate() error {
	if c.MaxCandidatesPerNarrative < 0 {
		return fmt.Errorf(
			"match.max_candidates_per_narrative is %d: must be positive, or omitted for the default of %d",
			c.MaxCandidatesPerNarrative, DefaultMaxCandidatesPerNarrative,
		)
	}

	if c.MaxNarrativesPerPass < 0 {
		return fmt.Errorf(
			"match.max_narratives_per_pass is %d: must be positive, or omitted for the default of %d",
			c.MaxNarrativesPerPass, DefaultMaxNarrativesPerMatchPass,
		)
	}

	if c.ConfidenceFloor < 0 || c.ConfidenceFloor > 1 {
		return fmt.Errorf(
			"match.confidence_floor is %v: must be within [0, 1] — confidence is a 0..1 score, so a "+
				"floor above 1 would never promote a primary and matching would look broken",
			c.ConfidenceFloor,
		)
	}

	return nil
}

// DefaultMaxNarrativesPerPass bounds how many narratives one reconcile pass
// examines. A limit is required rather than optional: each narrative costs at
// least one GetIssue per link plus one LLM drafting call, so an unbounded pass
// is unbounded spend.
const DefaultMaxNarrativesPerPass = 20

// ReconcilerConfig tunes proposal drafting. See
// docs/superpowers/specs/2026-08-25-reconciler-design.md.
type ReconcilerConfig struct {
	// MaxNarrativesPerPass caps narratives examined per pass. Zero means
	// DefaultMaxNarrativesPerPass. Reaching the cap is logged, never silent —
	// a silent cap presents as a clean pass that quietly ignored work.
	MaxNarrativesPerPass int `json:"max_narratives_per_pass"`
	// MinConfidenceToPropose governs what unjira *asserts*, not what it
	// *records*: below it the action is still written, carrying its low score.
	// Slice 6's triage must be able to see weak proposals in order to judge
	// them, and a dropped proposal is indistinguishable from "nothing to do."
	// Same reasoning as MatchConfig.ConfidenceFloor.
	MinConfidenceToPropose float64 `json:"min_confidence_to_propose"`
}

// NarrativeLimit returns the effective per-pass narrative cap.
func (c ReconcilerConfig) NarrativeLimit() int {
	if c.MaxNarrativesPerPass == 0 {
		return DefaultMaxNarrativesPerPass
	}

	return c.MaxNarrativesPerPass
}

// Validate rejects configurations that would fail silently at runtime.
//
// A MinConfidenceToPropose above 1 is the important case: confidence is a 0..1
// score, so every proposal would land below the threshold and the reconciler
// would look broken rather than misconfigured.
func (c ReconcilerConfig) Validate() error {
	if c.MaxNarrativesPerPass < 0 {
		return fmt.Errorf(
			"reconciler.max_narratives_per_pass is %d: must be positive, or omitted for the default of %d",
			c.MaxNarrativesPerPass, DefaultMaxNarrativesPerPass,
		)
	}

	if c.MinConfidenceToPropose < 0 || c.MinConfidenceToPropose > 1 {
		return fmt.Errorf(
			"reconciler.min_confidence_to_propose is %v: must be within [0, 1] — confidence is a "+
				"0..1 score, so a threshold above 1 would suppress every proposal",
			c.MinConfidenceToPropose,
		)
	}

	return nil
}

// AutoCommitRule governs whether watch's auto-commit gate applies one action
// type immediately or leaves it for triage. See internal/gate.Decide, which
// is this rule's only reader: `Confidence >= ConfidenceFloor && Graduated`.
//
// Per the phase-1 spec (docs/superpowers/specs/2026-08-11-phase1-correlator-design.md,
// "Auto-commit gate"): the floor comparison is inclusive (>=), so a proposal
// scoring exactly at the floor applies.
type AutoCommitRule struct {
	ConfidenceFloor float64 `json:"confidence_floor"`
	// Graduated is set only by explicit human action — a config edit, or
	// (once triage exists) answering a graduation prompt it surfaces. Nothing
	// in unjira may ever write this field: there is no setter, no
	// with-Graduated-flipped copy constructor, nothing. The obvious future
	// feature this forbids is "auto-graduate an action type after N clean
	// approvals" — the phase-1 spec is explicit that this must never happen
	// "even once approval history looks clean," because the moment unjira can
	// decide for itself that it has earned more write authority, a human is
	// no longer the one deciding it has.
	//
	// Defaults to false, including for every action type absent from
	// config entirely: Config.AutoCommit is a map, and Go's zero value for a
	// missing key is a zero-valued AutoCommitRule, so an unconfigured action
	// type is structurally unable to auto-apply without unjira doing
	// anything to guarantee it. See
	// TestConfig_AutoCommitDefaultsToQueueEverything, which asserts this
	// property directly because everything else in the gate rests on it.
	Graduated bool `json:"graduated"`
}

// Validate rejects a ConfidenceFloor outside [0, 1], following
// MatchConfig.Validate's reasoning: confidence is a 0..1 score, so a floor
// outside that range would either never gate anything (above 1) or always let
// everything through once Graduated (below 0) — either way, the gate would
// look broken rather than misconfigured.
func (r AutoCommitRule) Validate() error {
	if r.ConfidenceFloor < 0 || r.ConfidenceFloor > 1 {
		return fmt.Errorf(
			"auto_commit confidence_floor is %v: must be within [0, 1] — confidence is a 0..1 "+
				"score, so a floor outside that range would make the gate look broken rather than "+
				"misconfigured",
			r.ConfidenceFloor,
		)
	}

	return nil
}

// WorkflowConfig tunes internal/workflow's per-project graph cache. See
// docs/superpowers/specs/2026-09-01-named-status-transitions-design.md,
// "workflow.Graph becomes load-bearing, with a cache".
//
// Deliberately just this one field, and no Validate method: config.Span's
// own UnmarshalText already rejects an empty, unparseable, or non-positive
// value at decode time (internal/config/span_test.go), so by the time a
// *Config exists there is no further post-parse invariant left to check —
// unlike, say, MatchConfig.ConfidenceFloor, a bare float64 that needs its
// own range check because nothing upstream of it already validated.
type WorkflowConfig struct {
	// CacheTTL bounds how long workflow.Cached trusts a mined graph before
	// re-mining. Zero (the field's default when the key is absent from
	// config, which is expected — most operators never need to touch this)
	// means workflow.DefaultCacheTTL: internal/config must not import
	// internal/workflow (that would invert the dependency direction
	// workflow.StatusChange's own doc comment locks in — clients/jira ->
	// workflow, never the reverse — and config sits below workflow in that
	// same direction), so the actual default value is workflow's to own, not
	// duplicated here. A caller wires this straight into
	// workflow.CacheOptions.TTL, which already treats <= 0 as "use my
	// default".
	CacheTTL Span `json:"cache_ttl"`
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
	ExcludeFromLinking []string         `json:"exclude_from_linking"`
	Tracker            TrackerConfig    `json:"tracker"`
	LLM                LLMConfig        `json:"llm"`
	Correlator         CorrelatorConfig `json:"correlator"`
	Match              MatchConfig      `json:"match"`
	Reconciler         ReconcilerConfig `json:"reconciler"`
	Workflow           WorkflowConfig   `json:"workflow"`
	// AutoCommit keys the auto-commit gate's rules by actions.type
	// ("comment" | "transition" | "create"). Absent from config entirely
	// (nil map) is the common case and the safe default: every action type
	// queues for triage rather than auto-applying. See AutoCommitRule's doc
	// comment for the safety property this rests on.
	AutoCommit map[string]AutoCommitRule `json:"auto_commit"`
	Rules      RulesConfig               `json:"rules"`
	DBPath     string                    `json:"db_path"`
}

// DefaultRulesDir is where RulesDir looks when Rules.Dir is unset — matching
// how DefaultConfigPath is the default when Load's own path argument is
// empty: a working-directory-relative default rather than one baked into
// the binary as an absolute path.
const DefaultRulesDir = "rules"

// RulesConfig configures where internal/rules.Load reads human-curated
// markdown rule files from. A separate struct (rather than a bare top-level
// string field) for the same reason as TrackerConfig/LLMConfig: room to grow
// (e.g. a later per-scope override) without adding another top-level Config
// field alongside it.
type RulesConfig struct {
	// Dir is the rules directory, relative to the working directory unless
	// absolute. Empty means DefaultRulesDir.
	Dir string `json:"dir"`
}

// RulesDir returns the configured rules directory, defaulting to
// DefaultRulesDir when unset — the same working-directory-relative default
// DefaultConfigPath uses, so a fresh clone with no rules.dir configuration
// still resolves to the repo's seeded rules/ directory when run from the
// repo root.
func (c Config) RulesDir() string {
	if c.Rules.Dir == "" {
		return DefaultRulesDir
	}

	return c.Rules.Dir
}

// JiraConnectionByName finds a connection by its configured name, reporting
// whether one exists.
//
// Distinct from JiraConnectionForProject, which searches by project key: an
// event's provenance records the connection NAME it was collected through (see
// events.ArtifactIssueKey's neighbours and correlator.Candidate.Connection), and
// two connections can legitimately cover the same project key during a migration
// — which is exactly when looking one up by project would pick the wrong site.
func (c Config) JiraConnectionByName(name string) (JiraConnection, bool) {
	for _, conn := range c.Jira {
		if conn.Name == name {
			return conn, true
		}
	}

	return JiraConnection{}, false
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
// JiraConnectionForProject, erroring loudly if unset, unresolvable, or not
// writable — exactly the case a `create` action would hit with no routing
// logic upstream of it yet.
//
// The writability check belongs here rather than only at gate.Applier's
// per-action runtime check: tracker.default_project is static config, known
// entirely at startup, so a misconfigured (unwritable) default project is a
// statically-checkable error — the second of write scope's two layers. See
// docs/superpowers/specs/2026-08-27-write-scope-design.md, "startup
// validation is the second layer".
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

	if !conn.IsProjectWritable(c.Tracker.DefaultProject) {
		return JiraConnection{}, fmt.Errorf(
			"tracker.default_project %q is not writable: jira[].writable_project_keys does not "+
				"include it for connection %q",
			c.Tracker.DefaultProject, conn.Name,
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
