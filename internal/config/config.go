// Package config loads unjira's configuration. Copy config/unjira.example.yaml
// to ./unjira.config.yaml.
//
// The file is YAML, read with sigs.k8s.io/yaml in strict mode, so an unknown key
// is an error. JSON is a subset of YAML and keeps working.
//
// Credentials never live in config files. They come from the environment:
// UNJIRA_JIRA_CREDENTIALS maps each jira connection's name to its {email, token}
// pair, and UNJIRA_GITHUB_CREDENTIALS maps each GitHub host to its {token}, so
// the variable count does not grow with the number of configured connections.
// internal/envfile loads a gitignored .env from the repository root, and real
// environment variables win over it.
//
// In CI, UNJIRA_JIRA_CREDENTIALS is composed from the UNJIRA_CI_EMAIL variable
// and the UNJIRA_CI_TOKEN secret — see .github/workflows/ci.yml.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/jcogilvie/unjira/internal/events"
)

// DefaultConfigNames are the file names Load looks for when no path is given, in
// the working directory. Exactly one may exist: see DefaultPathIn.
var DefaultConfigNames = []string{"unjira.config.yaml", "unjira.config.yml", "unjira.config.json"}

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
	//
	// Also the reply's reserve: clustering fits each prompt to ContextWindowTokens
	// less this (correlator.WithResponseReserve), since a server holds the prompt and
	// the reply's ceiling together. Unset reserves nothing, so Validate requires it
	// to be less than ContextWindowTokens only when set.
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
	// MaxReasks is this model's re-ask budget for every use that does not set its
	// own; MaxMatchReasks, MaxOmissionReasks and MaxDisputeReasks set one use's.
	// Pointers, because unset (inherit the tier below) and an explicit 0 (never
	// re-ask) must differ. Resolved by Config.ReaskBudgets, which documents the
	// tiers and what a re-ask is at each use.
	MaxReasks         *int `json:"max_reasks"`
	MaxMatchReasks    *int `json:"max_match_reasks"`
	MaxOmissionReasks *int `json:"max_omission_reasks"`
	MaxDisputeReasks  *int `json:"max_dispute_reasks"`
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
	// A server sizes the prompt and the reply's ceiling together, so clustering
	// fits its prompt to the window less this (correlator.WithResponseReserve). A
	// ceiling at or above the window would leave no prompt at all.
	if c.MaxOutputTokens >= c.ContextWindowTokens {
		return fmt.Errorf(
			"llm.max_output_tokens is %d: must be less than llm.context_window_tokens (%d), "+
				"which holds the prompt and the reply together",
			c.MaxOutputTokens, c.ContextWindowTokens)
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
	// MaxEventSummaryChars caps each context event's summary in the clustering
	// prompt. Zero means unlimited, and zero is the default so the knob ships
	// inert — nobody's behaviour shifts until they choose a value.
	//
	// Finding F16: 93.7% of a 140k-token prompt was the events hydrated under
	// context narratives, and 15 of 325 events held 52% of the characters, the
	// largest a 15,037-char Jira description. Measured, 2000 fits a 365-day
	// window in ONE call where 90 days previously bisected — and bisecting costs
	// 1.37x, because both halves re-hydrate the same context.
	//
	// A plain character count rather than tiers or a token estimate: tokens are
	// what the budget is denominated in, but characters are what a summary is
	// measured in, and what an operator can compare against the lengths the
	// truncation report gives back.
	//
	// Truncation is REPORTED with real pre-truncation lengths (see
	// correlator.TruncationReport). A silent cap reads as "nothing was left out",
	// which is F25's defect one layer down, and a bare count could not tell an
	// operator whether raising the cap by 100 or by 10,000 recovers what was cut.
	MaxEventSummaryChars int `json:"max_event_summary_chars"`
	// MaxContextNarratives caps how many EXISTING narratives one narration pass
	// hydrates as clustering context (pipeline.hydrateContextNarratives, fed by
	// store.NarrativesOverlapping). Zero means no cap, and zero is the default.
	//
	// It is a cap on top of the bound that always holds: every clustering call fits
	// its context to llm.context_window_tokens less llm.max_output_tokens, bisecting
	// first and leaving whole narratives out only where the window cannot be split
	// (correlator/context_fit.go). So no value is needed to keep a prompt from
	// overflowing; this exists for the response ceiling, below.
	//
	// Finding F16, re-measured after the per-event summary cap and the tracker-
	// record exclusion both landed: completion tokens track EXTENDS-cluster count,
	// and EXTENDS count equals the context-narrative count in every measured run —
	// nine runs, 32/32 every time. Capping the per-event summary or the window
	// width does not touch this: the cost is one cluster per pre-existing
	// narrative shown to the model, not the size of what each one carries.
	//
	// A plain count rather than a token budget, matching MaxEventSummaryChars'
	// own reasoning one level up: an operator chooses "how many stories can this
	// pass consider" the same way they chose "how many characters can one event
	// contribute", and a token estimate would hide the number that actually
	// drives completion cost (cluster count) behind one further conversion the
	// operator would have to invert to tune it.
	//
	// Bounding WHICH narratives survive, not just how many, is the load-bearing
	// half — see selectContextNarratives. A narrative is a story the model needs
	// in order to judge "does this new event extend that". NarrativesOverlapping
	// orders (window_start, id), so a bare LIMIT would keep the OLDEST narratives
	// — close to the worst choice, since the newest are likeliest to be extended.
	//
	// AN ESCAPE HATCH, NOT A TUNING KNOB, and that is measured rather than
	// cautionary. On post-f18.db over a 7-day window, 2 reps per arm:
	//
	//	bound  clusters   NEW    ctx  completion
	//	off      37, 37     6     31  6,968 / 10,465
	//	12       25, 26  13, 14   12  7,826 /  5,324
	//
	// Cluster count fell 37 -> 25, which is the win this knob was built for. It is
	// not a win. NEW clusters MORE THAN DOUBLED, and every extra one duplicates a
	// narrative that already exists but was dropped from context —
	// 'triage-shows-context', 'corroborated-candidate-tier',
	// 'finding-issue-key-drift' and 'finding-reconcile-remainder' each already had
	// a row in narratives. The model could not see the story, so it opened a new
	// one. That is data corruption, not overspend. Completion did not improve
	// either: the two arms' ranges overlap, well inside the 34% run-to-run noise
	// F16 records.
	//
	// So set this ONLY where the alternative is a pass that fails outright — the
	// 365-day window that dies on the response ceiling, where a fragmented
	// narrative beats no narrative. Do not set it to trim cost on a pass that
	// already completes.
	//
	// The shared-issue-key tier cannot rescue an UNMATCHED narrative, which is why
	// the damage lands where it does: the dropped narratives mostly had no issue
	// key yet, so they fell to the recency tier and off the end. A branch- or
	// repo-overlap tier is the untried idea; see F16.
	//
	// Excluded narratives are REPORTED (NarrateResult.ExcludedContextNarratives),
	// mirroring ExcludedTrackerRecords: an unreported exclusion reads as "nothing
	// was left out", and here the count is how an operator sees how much context
	// they traded away.
	MaxContextNarratives int `json:"max_context_narratives"`
	// MemberConfidenceFloor is the confidence below which a MEMBER link — the model's
	// claim that an event is a narrative's work — is surfaced in triage as an
	// attribution for a reviewer to confirm, and counted in the pass summary. Zero
	// means off, and zero is the default.
	//
	// Off by default because a model's stated confidence is not calibrated. The floor
	// is meant to be set from reviewer rulings (the labeled data `learn` already
	// reads), not guessed up front — the same pattern as match.confidence_floor and
	// auto_commit.<type>.confidence_floor, which gate on a model-stated confidence
	// too. Member links matter beyond filing: they are the unit of token attribution,
	// so a wrong one is wrong estimation data. See
	// docs/superpowers/specs/2026-10-02-shared-context-design.md §1.
	//
	// A plain 0..1 number, in the max_output_tokens mould: the confidence it compares
	// against is a 0..1 score, so anything above 1 would flag every attribution.
	MemberConfidenceFloor float64 `json:"member_confidence_floor"`
}

// Validate reports whether both correlator limits are set to usable values.
func (c CorrelatorConfig) Validate() error {
	if c.TailSummarizeThresholdTokens <= 0 {
		return fmt.Errorf("correlator.tail_summarize_threshold_tokens must be a positive number of tokens")
	}
	if c.RecentEventsKept <= 0 {
		return fmt.Errorf("correlator.recent_events_kept must be a positive count")
	}
	// Negative is always a mistake; zero is the documented "unlimited" and must stay
	// permitted, since it is the default. Same shape as LLMConfig.MaxOutputTokens.
	if c.MaxEventSummaryChars < 0 {
		return fmt.Errorf(
			"correlator.max_event_summary_chars must be zero (unlimited) or a positive character count")
	}
	// Same shape again: zero is the documented "unlimited" default, negative is
	// always a mistake (most likely someone reaching for "unlimited", which this
	// does not spell that way).
	if c.MaxContextNarratives < 0 {
		return fmt.Errorf(
			"correlator.max_context_narratives must be zero (unlimited) or a positive count")
	}
	// Zero is the documented "off" default. Negative is a mistake, and above 1 would
	// surface every member link, since member confidence is a 0..1 score — the same
	// bound match.confidence_floor enforces.
	if c.MemberConfidenceFloor < 0 || c.MemberConfidenceFloor > 1 {
		return fmt.Errorf(
			"correlator.member_confidence_floor is %v: must be within [0, 1] — 0 is off, and member "+
				"confidence is a 0..1 score", c.MemberConfidenceFloor)
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
	// ConfidenceFloor governs only whether matching reports a primary as
	// promoted in its pass result (correlator.MatchResult.Primary). Below it,
	// every narrative_issues row is still written, including the primary. The
	// floor governs what unjira asserts, not what it records: dropping the rows
	// would make a low-confidence match indistinguishable from finding nothing
	// at all. Nothing downstream reads it, though, so the reconciler drafts onto
	// a primary at any confidence (finding F64).
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
	// Connections are the systems unjira talks to: kind and endpoint, nothing about
	// scope. See Connection.
	Connections []Connection `json:"connections"`
	// Trackers are the scopes unjira reconciles against, each on one connection, with
	// its own write authority. See Tracker.
	Trackers []Tracker `json:"trackers"`
	// DefaultTicketIn names the trackers untracked work outside every tracker's scope is
	// ticketed in (see UntrackedDestinations). Empty means nowhere: no create is proposed
	// for such work.
	DefaultTicketIn []string                  `json:"default_ticket_in"`
	Collectors      map[string]map[string]any `json:"collectors"`
	// ExcludeFromLinking is a list of regex patterns; a ticket-key-shaped
	// match against any of them is excluded from consideration as a real
	// Jira link (see internal/events.CompileLinkExclusionPatterns). Empty by
	// default — unjira makes no assumption about any workflow's own
	// placeholder-ticket conventions.
	ExcludeFromLinking []string  `json:"exclude_from_linking"`
	LLM                LLMConfig `json:"llm"`
	// LLMDefaults holds what applies across every model block. There is one model
	// block today; this tier exists so a multi-model config has somewhere to put a
	// default that no single model owns.
	LLMDefaults LLMDefaultsConfig `json:"llm_defaults"`
	Correlator  CorrelatorConfig  `json:"correlator"`
	Match       MatchConfig       `json:"match"`
	Reconciler  ReconcilerConfig  `json:"reconciler"`
	Workflow    WorkflowConfig    `json:"workflow"`
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
// how DefaultConfigNames are the default when Load's own path argument is
// empty: a working-directory-relative default rather than one baked into
// the binary as an absolute path.
const DefaultRulesDir = "rules"

// RulesConfig configures where internal/rules.Load reads human-curated
// markdown rule files from. A separate struct (rather than a bare top-level
// string field) for the same reason as LLMConfig: room to grow
// (e.g. a later per-scope override) without adding another top-level Config
// field alongside it.
type RulesConfig struct {
	// Dir is the rules directory, relative to the working directory unless
	// absolute. Empty means DefaultRulesDir.
	Dir string `json:"dir"`
}

// RulesDir returns the configured rules directory, defaulting to
// DefaultRulesDir when unset — the same working-directory-relative default
// DefaultConfigNames use, so a fresh clone with no rules.dir configuration
// still resolves to the repo's seeded rules/ directory when run from the
// repo root.
func (c Config) RulesDir() string {
	if c.Rules.Dir == "" {
		return DefaultRulesDir
	}

	return c.Rules.Dir
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

// DefaultPathIn returns the one default-named config file in dir, or "" when there
// is none. Two is an error rather than a preference: an operator editing the file
// unjira does not read would see their change silently ignored.
func DefaultPathIn(dir string) (string, error) {
	var found []string

	for _, name := range DefaultConfigNames {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err == nil {
			found = append(found, path)
		} else if !os.IsNotExist(err) {
			return "", fmt.Errorf("checking for config %s: %w", path, err)
		}
	}

	switch len(found) {
	case 0:
		return "", nil
	case 1:
		return found[0], nil
	default:
		return "", fmt.Errorf("found several config files (%s): keep one, so it is clear which unjira reads",
			strings.Join(found, ", "))
	}
}

// removedKeys are top-level keys of the previous config shape. Each is refused with
// the new shape named, never mapped: config is user-local and the store has no
// migrations, so a silent translation would be a second source of truth.
var removedKeys = []struct{ key, replacement string }{
	{
		key: "jira",
		replacement: `Jira sites are now "connections" entries (kind: jira, endpoint: <site>), and each ` +
			`site's project_keys, writable_project_keys, queries and max_issues_per_query move to a ` +
			`"trackers" entry on that connection as scopes, writable_scopes, queries and ` +
			`max_issues_per_query`,
	},
	{
		key: "tracker",
		replacement: `tracker.backend is now each connection's kind, and tracker.default_project is now ` +
			`"default_ticket_in" (a tracker name) plus that tracker's "default_scope"`,
	},
}

// refuseRemovedKeys reports every removed top-level key present in a config body.
func refuseRemovedKeys(path string, body []byte) error {
	asJSON, err := yaml.YAMLToJSON(body)
	if err != nil {
		return fmt.Errorf("parsing config %s: %w", path, err)
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(asJSON, &top); err != nil {
		return fmt.Errorf("parsing config %s: the top level must be a mapping: %w", path, err)
	}

	var errs []error

	for _, removed := range removedKeys {
		if _, ok := top[removed.key]; ok {
			errs = append(errs, fmt.Errorf("config %s: key %q was removed: %s. See config/unjira.example.yaml",
				path, removed.key, removed.replacement))
		}
	}

	return errors.Join(errs...)
}

// Load reads a config file, falling back to Default when none exists. An empty path
// looks in the working directory for one of DefaultConfigNames.
//
// The tracker model is validated here (ValidateTrackers), so a misconfigured tracker
// fails before any command does work rather than when a write is first attempted.
func Load(path string) (Config, error) {
	if path == "" {
		found, err := DefaultPathIn(".")
		if err != nil {
			return Config{}, err
		}

		if found == "" {
			return Default(), nil
		}

		path = found
	}

	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Default(), nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("reading config %s: %w", path, err)
	}

	if err := refuseRemovedKeys(path, body); err != nil {
		return Config{}, err
	}

	var cfg Config
	if err := yaml.UnmarshalStrict(body, &cfg); err != nil {
		return Config{}, fmt.Errorf("parsing config %s: %w", path, err)
	}

	if err := cfg.ValidateTrackers(); err != nil {
		return Config{}, fmt.Errorf("config %s: %w", path, err)
	}

	// Checked here, not only where the budgets are used: a negative budget is a typo
	// in a value no command can run correctly with, and every LLM-using command reads
	// it, so the earliest point it can fail is the right one.
	if _, err := cfg.ReaskBudgets(); err != nil {
		return Config{}, fmt.Errorf("config %s: %w", path, err)
	}

	return cfg, nil
}
