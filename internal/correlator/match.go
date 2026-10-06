package correlator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/llm"
	"github.com/jcogilvie/unjira/internal/logging"
	"github.com/jcogilvie/unjira/internal/rules"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// MatchResult is what one narrative's resolution produced, for rendering and
// for tests. Links is empty when no candidate survived verification.
type MatchResult struct {
	NarrativeID int64
	Links       []store.NarrativeIssue
	// Primary is the promoted key, or "" when there was none, or when the
	// winning candidate's confidence fell below cfg.ConfidenceFloor — see
	// AddNarrativeIssues/SetNarrativeIssueLink below for why the row is
	// still written even when Primary stays empty.
	Primary string
	// Excluded and Unresolved carry the candidates that were dropped, and
	// why, so a narrative that looks untracked can be explained without
	// re-running: Excluded is what exclude_from_linking removed before
	// verification ever ran; Unresolved is what verification against the
	// live tracker rejected as not-found.
	Excluded   []string
	Unresolved []string
	// Rationale is the model's stated reason for its primary pick, when an
	// LLM call was made; empty on the deterministic zero- and
	// one-candidate paths, which never ask the model anything.
	Rationale string
}

// matchOptions holds Match's optional configuration, threaded through
// MatchOption so Match's positional signature doesn't grow every time a new
// knob is needed.
type matchOptions struct {
	linkExclusions []*regexp.Regexp
	rules          []rules.Rule
	log            *slog.Logger
}

// MatchOption configures an optional Match behaviour.
type MatchOption func(*matchOptions)

// WithMatchLogger supplies the logger Match reports on. Nil is silent, so no existing
// caller or test changes. Named apart from WithLogger (which configures Cluster) because
// the two option types are distinct and a shared name would not compile at one of them.
func WithMatchLogger(log *slog.Logger) MatchOption {
	return func(o *matchOptions) {
		o.log = log
	}
}

// WithLinkExclusions supplies the compiled exclude_from_linking patterns
// (see config.Config.CompiledLinkExclusions) so Match can drop placeholder
// keys from candidacy while still recording them as excluded rather than
// silently vanishing.
func WithLinkExclusions(compiled []*regexp.Regexp) MatchOption {
	return func(o *matchOptions) {
		o.linkExclusions = compiled
	}
}

// WithRules supplies rules/ entries already filtered to
// rules.ScopeCorrelator (see rules.ForScope) so Match can append them to its
// classification system prompt. Like WithLinkExclusions, Match only ever
// sees what the caller (internal/pipeline) already read from config and
// resolved — it does not load or filter rules itself.
func WithRules(learnedRules []rules.Rule) MatchOption {
	return func(o *matchOptions) {
		o.rules = learnedRules
	}
}

// verifiedCandidate is a Candidate whose issue key has been confirmed to
// exist against the live tracker (rules/verify-correlations.md's
// requirement — no candidate is trusted on provenance strength alone),
// carrying the tasktracker.Issue read during that verification so
// classifyCandidates never needs a second GetIssue call for the same key.
type verifiedCandidate struct {
	Candidate

	Issue tasktracker.Issue
}

// Match resolves narratives that have no primary link (per
// store.NarrativesWithoutPrimaryLink) against tracker, recording a
// narrative_issues row per judged candidate.
//
// tracker is read per candidate key. In production it is tasktracker.Routed
// over the one Resolver, so each candidate is verified against the tracker
// whose scopes own its key, never against a default chosen for the pass —
// that was the cross-connection bug, where a candidate on one Jira site was
// checked against another and reported missing.
// A primary is recorded whatever its confidence; match.confidence_floor decides
// only whether the primary is ASSERTED in the pass result (see persistLinks).
// See docs/superpowers/specs/2026-08-24-narrative-issue-matching-design.md
// for the design this implements.
//
// Match acquires no lease; the caller holds one, matching RunNarrate's
// existing convention for Cluster/Persist. Failures are per narrative, not
// per pass: one narrative's tracker error accumulates via errors.Join and
// the rest still get a chance, since an unmatched narrative is a valid
// resting state (it simply stays in the backlog for the next pass) while a
// pass that gave up entirely would cost every narrative a retry over one
// narrative's bad luck.
func Match(
	ctx context.Context,
	s *store.Store,
	tracker tasktracker.TaskReader,
	client llm.Client,
	cfg config.MatchConfig,
	opts ...MatchOption,
) ([]MatchResult, Stats, error) {
	var o matchOptions
	for _, opt := range opts {
		opt(&o)
	}

	// TWO limits, deliberately named apart. A single `limit` served both roles
	// here, so a config setting max_candidates_per_narrative=10 silently examined
	// only 10 narratives per pass — on a 30-narrative backlog that left 20
	// unmatched, and those then reached the create path as untracked work and drew
	// proposals for tickets duplicating issues they already named. The linked
	// narratives came out as a contiguous id block, which is what gave it away.
	candidateLimit := cfg.CandidateLimit()
	narrativeLimit := cfg.NarrativeLimit()

	narratives, err := s.NarrativesWithoutPrimaryLink(narrativeLimit + 1)
	if err != nil {
		return nil, Stats{}, fmt.Errorf("listing narratives without an issue key: %w", err)
	}

	// Reaching the cap is logged, never silent — the same treatment Reconcile
	// gives its own cap, and its absence here is why 20 unexamined narratives read
	// as a matching failure rather than a batch limit. Fetching limit+1 above is
	// how this knows the difference between "exactly full" and "more waiting".
	if len(narratives) > narrativeLimit {
		logging.For(o.log, "correlator").WarnContext(ctx, "narrative cap reached",
			"unmatched_at_least", len(narratives), "examining", narrativeLimit,
			"config_key", "match.max_narratives_per_pass")
		narratives = narratives[:narrativeLimit]
	}

	// Loaded ONCE per pass, not per narrative: it is a whole-store aggregate that
	// does not vary between narratives, and matchOne runs in a loop.
	//
	// A failure here does not fail the pass. An empty map degrades ranking to
	// exactly the pre-F9 alphabetical behaviour, which is worse but not wrong —
	// whereas refusing to match anything because one COUNT-style query failed
	// would turn a ranking regression into an outage.
	jiraActivity, err := s.IssueActivity()
	if err != nil {
		logging.For(o.log, "correlator").WarnContext(ctx, "could not load issue activity",
			"err", err,
			"consequence", "candidate ranking falls back to provenance and issue key alone, so a "+
				"recently-active ticket mentioned in passing may be truncated away")

		jiraActivity = nil
	}

	var (
		results []MatchResult
		stats   Stats
		errs    error
	)

	for _, n := range narratives {
		result, oneStats, err := matchOne(
			ctx, s, tracker, client, n, o.linkExclusions, candidateLimit, cfg, o.rules, jiraActivity,
			o.log)
		stats.Add(oneStats)
		results = append(results, result)
		if err != nil {
			errs = errors.Join(errs, err)
		}
	}

	return results, stats, errs
}

// matchOne resolves a single narrative: gather candidates (recording
// exclusions regardless of outcome), verify every survivor against the
// tracker that owns its key, then either promote a lone survivor deterministically or ask the model to
// classify 2+ of them, and persist whatever was decided in one transaction.
func matchOne(
	ctx context.Context,
	s *store.Store,
	tracker tasktracker.TaskReader,
	client llm.Client,
	narrative store.NarrativeRow,
	linkExclusions []*regexp.Regexp,
	limit int,
	cfg config.MatchConfig,
	learnedRules []rules.Rule,
	jiraActivity map[string]time.Time,
	log *slog.Logger,
) (MatchResult, Stats, error) {
	result := MatchResult{NarrativeID: narrative.ID}

	// AllMemberEvents, not MemberEventsAfterBoundary: candidate keys live
	// in event artifacts, and the git_branch artifact carrying the
	// strongest provenance sits on a narrative's oldest events — exactly
	// the ones a compaction boundary hides from context-only readers.
	//
	// MEMBER events only, at every tier (shared-context spec §3). A shared root segment
	// routinely carries 17+ prose keys and up to 37 scm_command keys; fed in as context,
	// every narrative it supports would inherit them, and a lone verified one would
	// become primary at confidence 1.0 with no model call (resolveVerified).
	evts, err := s.AllMemberEvents(narrative.ID)
	if err != nil {
		return result, Stats{}, fmt.Errorf("loading events for narrative %d: %w", narrative.ID, err)
	}

	result.Excluded = excludedCandidates(evts, linkExclusions)

	candidates := gatherCandidates(evts, linkExclusions, limit, jiraActivity)
	if len(candidates) == 0 {
		// Untracked work is the default path, not a special case: no tracker call, no
		// LLM call, nothing to link.
		//
		// But it must leave a WATERMARK, or this is a livelock (finding F22).
		// NarrativesWithoutPrimaryLink orders by (window_start, id) — stable — so a
		// narrative that writes nothing is re-selected every pass, and the ones behind
		// it are never reached. Measured before this: eleven consecutive passes each
		// reported 49 unmatched, 33 of which had no candidate keys at all, while
		// creates stayed deferred forever because that count could not reach zero.
		//
		// Recorded here rather than by the caller because this is the branch that knows
		// WHY nothing happened, and design-notes #29's lesson is that an outcome and
		// its trace belong together.
		if err := s.RecordMatchExamined(
			narrative.ID, "no candidate issue keys in any event"); err != nil {
			return result, Stats{}, err
		}

		return result, Stats{}, nil
	}

	verified, unresolved, err := verifyCandidates(tracker, narrative.ID, candidates)
	result.Unresolved = unresolved
	if err != nil {
		return result, Stats{}, err
	}

	if len(verified) == 0 {
		// Same livelock, one step later: candidates existed but none survived
		// verification (the issues do not exist, or are on no configured connection).
		// Without a watermark this narrative is re-selected and re-verified — at
		// tracker-call cost, not just selector cost — every pass forever.
		if err := s.RecordMatchExamined(
			narrative.ID, "every candidate issue key failed verification"); err != nil {
			return result, Stats{}, err
		}

		return result, Stats{}, nil
	}

	links, primaryKey, primaryConfidence, rationale, stats, err := resolveVerified(ctx, client, narrative, evts, verified, learnedRules, log)
	if err != nil {
		return result, stats, fmt.Errorf("classifying candidates for narrative %d: %w", narrative.ID, err)
	}

	result.Links = links
	result.Rationale = rationale

	promoted, err := persistLinks(s, narrative.ID, links, primaryKey, primaryConfidence, cfg.ConfidenceFloor, log)
	if err != nil {
		return result, stats, fmt.Errorf("persisting issue links for narrative %d: %w", narrative.ID, err)
	}
	result.Primary = promoted

	return result, stats, nil
}

// verifyCandidates confirms every candidate exists, regardless of provenance
// strength — rules/verify-correlations.md's requirement, since even a
// branch-derived key can be a stale name. A not-found key is dropped into
// unresolved and matching continues; a transport error (see
// tasktracker.IsTransportError) aborts this narrative entirely so the next
// pass retries it rather than recording a wrong conclusion drawn from an
// unreachable tracker.
//
// A key no configured tracker covers (tasktracker.UnroutedError) is a config
// gap, not a tracker answer. It is recorded per candidate with its reason
// rather than failing the narrative, because unlike a transport error it does
// not fix itself between passes, and a bare key would tell a reviewer a
// ticket is missing when the truth is that unjira never looked.
func verifyCandidates(
	tracker tasktracker.TaskReader,
	narrativeID int64,
	candidates []Candidate,
) (verified []verifiedCandidate, unresolved []string, err error) {
	for _, c := range candidates {
		issue, getErr := tracker.GetIssue(c.IssueKey)
		if getErr != nil {
			if tasktracker.IsTransportError(getErr) {
				return nil, unresolved, fmt.Errorf(
					"verifying candidate %s for narrative %d: %w", c.IssueKey, narrativeID, getErr,
				)
			}

			if _, unrouted := errors.AsType[*tasktracker.UnroutedError](getErr); unrouted {
				unresolved = append(unresolved, fmt.Sprintf("%s: %v", c.IssueKey, getErr))

				continue
			}

			unresolved = append(unresolved, c.IssueKey)

			continue
		}

		verified = append(verified, verifiedCandidate{Candidate: c, Issue: issue})
	}

	return verified, unresolved, nil
}

// resolveVerified decides roles for verified candidates: a lone survivor is
// primary deterministically (Confidence 1.0, no LLM spend — one survivor is
// not a judgment call), while 2+ survivors go through classifyCandidates.
// Every returned store.NarrativeIssue carries Provenance/Connection from
// the verifiedCandidate that produced it, not from the model — provenance
// is a fact this package already determined and must not be something an
// LLM response can restate incorrectly.
func resolveVerified(
	ctx context.Context,
	client llm.Client,
	narrative store.NarrativeRow,
	evts []Event,
	verified []verifiedCandidate,
	learnedRules []rules.Rule,
	log *slog.Logger,
) (links []store.NarrativeIssue, primaryKey string, primaryConfidence float64, rationale string, stats Stats, err error) {
	if len(verified) == 1 {
		v := verified[0]
		links = []store.NarrativeIssue{{
			IssueKey:   v.IssueKey,
			Role:       RolePrimary,
			Provenance: string(v.Provenance),
			Confidence: 1.0,
			Connection: v.Connection,
		}}
		return links, v.IssueKey, 1.0, "", Stats{}, nil
	}

	// Events, not just title/summary. matchOne already loaded them above for
	// candidate gathering, and withholding them is what let a real narrative go
	// unlinked: its two Jira events naming PAAS-4038 were the evidence that
	// settled which of four candidates owned the work, and the classifier never
	// saw them. See match_events_test.go for the reproduction.
	n := Narrative{
		ID: narrative.ID, Title: narrative.Title, Summary: narrative.Summary, Events: evts,
	}

	verdicts, callStats, callErr := classifyCandidates(ctx, client, n, verified, learnedRules)
	if callErr != nil {
		return nil, "", 0, "", callStats, callErr
	}

	if len(verdicts) == 0 {
		// The classifier declined to attribute anything, which discards every
		// candidate below — including any whose provenance was a recorded fact
		// rather than an inference. Logged because silence is exactly how this
		// went unnoticed through a real pass: a narrative with four verified
		// candidates ended with zero links, no error, and no line anywhere saying
		// so, and the create path then proposed a duplicate ticket for work
		// already tracked.
		//
		// NOT an error, deliberately. classifySystemPrompt does say "Exactly one
		// candidate must receive this role", so an empty array violates a stated
		// requirement and is arguably malformed — but failing here would abort a
		// narrative that the next pass may classify fine, and the prompt now
		// carries the narrative's events, which may remove the case entirely.
		// Visibility first; escalate only if it recurs with the evidence present.
		logging.For(log, "correlator").WarnContext(ctx, "classifier returned no verdicts",
			"narrative_id", narrative.ID, "verified_candidates", len(verified),
			"consequence", "nothing linked, so this narrative still reads as untracked")
	}

	byKey := make(map[string]verifiedCandidate, len(verified))
	for _, v := range verified {
		byKey[v.IssueKey] = v
	}

	for _, verdict := range verdicts {
		v, ok := byKey[verdict.IssueKey]
		if !ok {
			// The model named a key that was never presented as a
			// candidate. It was not verified against the live tracker
			// under this pass, so — per verify-correlations.md — it is not
			// trusted regardless of stated confidence. Log loudly and skip
			// rather than writing an attribution this pass never verified.
			logging.For(log, "correlator").WarnContext(ctx, "classifier returned an unrecognized issue key",
				"narrative_id", narrative.ID, "issue_key", verdict.IssueKey, "action", "ignoring")
			continue
		}

		links = append(links, store.NarrativeIssue{
			IssueKey:   verdict.IssueKey,
			Role:       verdict.Role,
			Provenance: string(v.Provenance),
			Confidence: verdict.Confidence,
			Connection: v.Connection,
		})

		if verdict.Role == RolePrimary {
			primaryKey = verdict.IssueKey
			primaryConfidence = verdict.Confidence
			rationale = verdict.Rationale
		}
	}

	return links, primaryKey, primaryConfidence, rationale, callStats, nil
}

// persistLinks writes links in one transaction, matching Persist's convention of
// never holding a transaction open across an LLM round-trip (classifyCandidates
// has already returned by the time this runs).
//
// It returns primaryKey when primaryConfidence clears floor, and "" otherwise.
// That return value is REPORTING ONLY — it becomes MatchResult.Primary, which the
// renderer prints (or, when empty, prints "no primary promoted — below confidence
// floor"). The floor no longer gates any write.
//
// It used to. `narratives.issue_key` denormalized the primary, and the floor
// decided whether to set it, so a sub-floor match wrote the primary LINK ROW and
// left the COLUMN null — inside one transaction, so it committed atomically into a
// state where the two disagreed forever. Matching's backlog selected on the
// column, so that narrative was re-matched every pass until a differing primary
// tripped one_primary_per_narrative and aborted the whole pass. That was finding
// F11; the column is gone and the backlog asks the link table now
// (store.NarrativesWithoutPrimaryLink).
//
// The floor still gates nothing about RECORDING, which was always the intent:
// every link is written regardless of confidence, because a dropped row would make
// a low-confidence match indistinguishable from finding nothing at all
// (config.MatchConfig.ConfidenceFloor's own doc comment — "the floor governs what
// unjira asserts, not what it records").
func persistLinks(
	s *store.Store,
	narrativeID int64,
	links []store.NarrativeIssue,
	primaryKey string,
	primaryConfidence float64,
	confidenceFloor float64,
	log *slog.Logger,
) (string, error) {
	promoted := ""

	err := s.WithTx(func(tx *store.Tx) error {
		promoted = ""

		if len(links) > 0 {
			if err := tx.AddNarrativeIssues(narrativeID, links); err != nil {
				return err
			}
		}

		if primaryKey == "" {
			return nil
		}

		if primaryConfidence < confidenceFloor {
			// Logged, naming exactly what was withheld and why: an unreported
			// sub-floor match is indistinguishable from finding nothing at all.
			// The link row above is already written — this only withholds the
			// assertion, which is the whole distinction the floor draws.
			logging.For(log, "correlator").Info("primary below the confidence floor",
				"narrative_id", narrativeID, "issue_key", primaryKey,
				"confidence", primaryConfidence, "floor", confidenceFloor,
				"action", "recorded but not asserted")

			return nil
		}

		promoted = primaryKey

		return nil
	})
	if err != nil {
		return "", err
	}

	return promoted, nil
}

// classifySystemPrompt instructs the model to assign each verified
// candidate one of the three closed roles defined in match_types.go —
// worded to match that file's doc comments so the two never drift apart.
const classifySystemPrompt = `You are attributing one work narrative to the tracker issues it might belong to. Every candidate shown already exists — verified against the live tracker — and each is shown with its provenance (where its key was found) as a stated PRIOR, not a verdict: even a key found in a branch name can be a stale reference, so judge from each candidate's summary, description, and status, not from provenance strength alone.

Assign each candidate exactly one role:
- "primary": the record the work is principally tracked in. Exactly one candidate must receive this role.
- "same_work": a co-representation of the same body of work in another tracking context — not a dependency. One body of work recorded twice for two audiences (for example, an engineering ticket and a paired change-management ticket for the same deploy), not two separate bodies of work related to each other.
- "mentioned": the issue was referenced in the narrative's events but is not itself the work — the correct role for a citation, a "caused by", or a "discovered while" reference alike.

Return ONLY a bare JSON array, no prose, no markdown fences:
[{"issue_key":"...","role":"primary"|"same_work"|"mentioned","confidence":0.0-1.0,"rationale":"..."}]`

// classifyCandidates asks client to assign a role to each of candidates,
// following clusterSystemPrompt/buildClusterPrompt's style: a fixed system
// prompt (plus, when learnedRules is non-empty, a rendered rules section —
// see rules.Render) plus a rendered user prompt, parsed by the existing
// parseMatchResponse (which strips a markdown fence models add despite the
// system prompt forbidding it — see stripJSONFence's doc comment).
func classifyCandidates(
	ctx context.Context,
	client llm.Client,
	n Narrative,
	candidates []verifiedCandidate,
	learnedRules []rules.Rule,
) ([]matchVerdict, Stats, error) {
	userPrompt := buildMatchPrompt(n, candidates)

	systemPrompt := classifySystemPrompt
	if rendered := rules.Render(learnedRules); rendered != "" {
		systemPrompt += "\n\n" + rendered
	}

	var stats Stats

	raw, usage, err := client.Complete(ctx, systemPrompt, userPrompt)
	if err != nil {
		return nil, stats, fmt.Errorf("classifying candidates for narrative %d: %w", n.ID, err)
	}
	stats.AddUsage(usage)

	verdicts, err := parseMatchResponse(raw)
	if err == nil {
		return verdicts, stats, nil
	}

	// One re-ask, quoting the parser's reason (finding F44). Seen twice on one real
	// 30-day pass: prose where the array belonged, and a duplicated malformed key.
	// The parser is right to refuse both, since a best-effort reading is how a
	// narrative gets attributed to the wrong ticket, but the model nearly always
	// answers correctly when shown what was wrong. A matching call is one
	// narrative's prompt, so the re-ask is cheap. One is the budget, as for the
	// clustering re-asks: a second refusal is information, and fails loudly.
	stats.MatchReasks++

	raw2, usage, err2 := client.Complete(ctx, systemPrompt, buildMatchReaskPrompt(userPrompt, raw, err))
	if err2 != nil {
		return nil, stats, fmt.Errorf("re-asking for narrative %d after %w: %w", n.ID, err, err2)
	}
	stats.AddUsage(usage)

	verdicts, err2 = parseMatchResponse(raw2)
	if err2 != nil {
		return nil, stats, fmt.Errorf("match response refused twice: first: %w; re-ask: %w", err, err2)
	}

	return verdicts, stats, nil
}

// buildMatchReaskPrompt repeats the original user prompt verbatim, so the
// candidates mean what they meant the first time, then quotes the refused
// response and the parser's reason.
func buildMatchReaskPrompt(userPrompt, refused string, reason error) string {
	var b strings.Builder

	b.WriteString(userPrompt)
	b.WriteString("\n\n## Your previous response could not be used\n\n")
	fmt.Fprintf(&b, "It was refused because: %v\n\n", reason)
	b.WriteString("The refused response:\n\n")
	b.WriteString(refused)
	b.WriteString("\n\nAnswer again. Return ONLY the bare JSON array the system prompt specifies, " +
		"with one entry per candidate, no prose and no markdown fences.\n")

	return b.String()
}

// buildMatchPrompt renders the narrative's title/summary plus each
// candidate's key, provenance, status, summary, and description — the
// description is the reason tasktracker.Issue gained that field: a one-line
// summary frequently cannot distinguish the ticket a narrative implements
// from one it merely mentions in passing.
// buildMatchPrompt renders the narrative, its events, and every verified
// candidate.
//
// The EVENTS are the part that took a production failure to get right. Without
// them the model is asked which of several tickets owns this work while seeing
// only the keys and their Jira metadata — and the system prompt tells it to
// "judge from each candidate's summary, description, and status, not from
// provenance strength alone", which asks for evidence-based judgment while
// withholding the evidence. A narrative whose two Jira events were literally
// status changes on PAAS-4038 went unlinked because that fact reached the model
// only as the single word "jira_event" on a candidate line.
//
// Rendered in the same shape buildDraftPrompt and buildClusterPrompt use, so all
// three prompts describe an event identically.
func buildMatchPrompt(n Narrative, candidates []verifiedCandidate) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Narrative: title=%q\nsummary: %q\n", n.Title, n.Summary)

	if len(n.Events) > 0 {
		b.WriteString("\nEvents in this narrative:\n")
		for _, e := range n.Events {
			// %q on Summary for the same reason buildClusterPrompt quotes it:
			// event summaries come from arbitrary upstream text, and an embedded
			// newline could otherwise fabricate a line the model reads as
			// structure.
			fmt.Fprintf(&b, "- [%s] %q (occurred_at=%s)\n",
				e.Source, e.Summary, e.OccurredAt.Format(time.RFC3339))
		}
	}

	b.WriteString("\nCandidates:\n")

	for _, c := range candidates {
		fmt.Fprintf(&b, "- issue_key=%s provenance=%s\n", c.IssueKey, c.Provenance)
		fmt.Fprintf(&b, "  status: %s\n", c.Issue.StatusName)
		fmt.Fprintf(&b, "  summary: %q\n", c.Issue.Summary)
		fmt.Fprintf(&b, "  description: %q\n", c.Issue.Description)
	}

	return b.String()
}
