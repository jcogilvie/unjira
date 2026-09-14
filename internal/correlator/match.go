package correlator

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/jcogilvie/unjira/internal/clients/jira"
	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/llm"
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
}

// MatchOption configures an optional Match behaviour.
type MatchOption func(*matchOptions)

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

// IsTransportError reports whether err means the tracker itself was
// unreachable — a 5xx, a 429, or no HTTP response at all (Status == 0) —
// rather than that the requested key genuinely does not exist (a 404, or
// store.ErrLocalIssueNotFound from the local backend). Match uses this to
// decide, per verified candidate, whether to drop the key as Unresolved and
// keep going, or to fail the whole narrative for a retry on the next pass.
//
// Getting this backwards in either direction is a real failure mode, not a
// theoretical one: misclassifying a transient outage as "not found" drops
// the candidate permanently if a sibling candidate still lets the narrative
// acquire a primary and leave the NarrativesWithoutIssueKey backlog (it will
// never be reconsidered again, so the falsely-dropped candidate never gets
// a second chance); misclassifying a genuinely deleted ticket as transport
// fails the narrative every single pass forever, since the same 404 recurs
// each time gatherCandidates re-derives the same key from the same events.
//
// An error of neither recognized shape defaults to "transport" (true): a
// spurious retry costs a deferred pass, while a spurious drop costs a
// candidate that then never comes back — the two outcomes above are not
// symmetric in cost, so the default leans toward the cheaper mistake.
//
// Exported, not merely correlator-internal: Task 10's live tier
// (internal/live) needs the identical classification and, being a separate
// package, cannot reach an unexported identifier here or this package's
// export_test.go shim (that shim only widens correlator's surface for the
// correlator_test package). It was deliberately not moved to
// internal/tasktracker, despite that package being the natural home for a
// tracker-contract concept: tasktracker's own doc comment states it has no
// imports of internal/clients or internal/store, and jira.Error carries no
// interface method to duck-type against (it is a plain struct, and changing
// its shape is outside this task's scope) — so classifying it there would
// require tasktracker to import internal/clients/jira directly. That
// package already imports internal/tasktracker (so jira.Tracker satisfies
// tasktracker.TaskTracker), which would make the reverse import an
// immediate cycle. internal/correlator already depends on neither
// direction of that pair, making it the cycle-free home; internal/live can
// import internal/correlator with no such constraint.
func IsTransportError(err error) bool {
	if err == nil {
		return false
	}

	if jerr, ok := errors.AsType[*jira.Error](err); ok {
		// Status == 0: the request never got an HTTP response at all
		// (connection refused, DNS failure, timeout) — always transport.
		// 429/5xx: the tracker rejected or failed the request, which says
		// nothing about whether the issue exists. Anything else (404 chief
		// among them) is a real "not found."
		return jerr.Status == 0 || jerr.Status == 429 || jerr.Status >= 500
	}

	if errors.Is(err, store.ErrLocalIssueNotFound) {
		return false
	}

	return true
}

// TrackerResolver returns the tracker to verify a candidate against, given the
// connection its provenance recorded (Candidate.Connection).
//
// A function rather than a map or a widened interface because the correlator must
// not learn what a "connection" is: for the jira backend it selects which
// configured site to talk to, for the local backend it means nothing at all, and
// for a future GitHub backend it would mean something different again. Resolution
// is the caller's business (cmd/unjira knows the config); this package only knows
// that different candidates can need different trackers.
//
// An empty connection is the common case, not an error: a branch- or prose-derived
// candidate carries no connection at all, since only jira-source events record one
// (see Candidate.Connection). A resolver is expected to answer that with a default.
//
// Returning an error is how a resolver says "this connection is not configured" —
// renamed, removed, or never present. verifyCandidates records that per candidate
// rather than failing the narrative, since a config gap does not fix itself between
// passes and failing forever would be worse than reporting it once per pass.
type TrackerResolver func(connection string) (tasktracker.TaskReader, error)

// SingleTracker adapts one tracker to a TrackerResolver, for a caller with nothing
// to resolve — the local backend, which ignores connections entirely, and every
// single-connection jira setup.
//
// Exists so those callers do not each write the same closure, and so the
// single-tracker case stays a special case of the general one rather than a
// separate code path through Match.
func SingleTracker(tracker tasktracker.TaskReader) TrackerResolver {
	return func(string) (tasktracker.TaskReader, error) { return tracker, nil }
}

// Match resolves narratives lacking an issue_key (per
// store.NarrativesWithoutIssueKey) against the tracker each candidate's
// connection resolves to, promoting a primary
// into narratives.issue_key when one is found with sufficient confidence.
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
	resolve TrackerResolver,
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

	narratives, err := s.NarrativesWithoutIssueKey(narrativeLimit + 1)
	if err != nil {
		return nil, Stats{}, fmt.Errorf("listing narratives without an issue key: %w", err)
	}

	// Reaching the cap is logged, never silent — the same treatment Reconcile
	// gives its own cap, and its absence here is why 20 unexamined narratives read
	// as a matching failure rather than a batch limit. Fetching limit+1 above is
	// how this knows the difference between "exactly full" and "more waiting".
	if len(narratives) > narrativeLimit {
		log.Printf(
			"correlator: %d or more narratives are unmatched but this pass examines %d "+
				"(match.max_narratives_per_pass); the remainder wait for the next pass",
			len(narratives), narrativeLimit,
		)
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
		log.Printf("correlator: could not load issue activity (%v); candidate ranking falls back "+
			"to provenance and issue key alone, so a recently-active ticket mentioned in passing "+
			"may be truncated away", err)

		jiraActivity = nil
	}

	var (
		results []MatchResult
		stats   Stats
		errs    error
	)

	for _, n := range narratives {
		result, oneStats, err := matchOne(
			ctx, s, resolve, client, n, o.linkExclusions, candidateLimit, cfg, o.rules, jiraActivity)
		stats.Add(oneStats)
		results = append(results, result)
		if err != nil {
			errs = errors.Join(errs, err)
		}
	}

	return results, stats, errs
}

// matchOne resolves a single narrative: gather candidates (recording
// exclusions regardless of outcome), verify every survivor against its own
// connection's tracker,
// then either promote a lone survivor deterministically or ask the model to
// classify 2+ of them, and persist whatever was decided in one transaction.
func matchOne(
	ctx context.Context,
	s *store.Store,
	resolve TrackerResolver,
	client llm.Client,
	narrative store.NarrativeRow,
	linkExclusions []*regexp.Regexp,
	limit int,
	cfg config.MatchConfig,
	learnedRules []rules.Rule,
	jiraActivity map[string]time.Time,
) (MatchResult, Stats, error) {
	result := MatchResult{NarrativeID: narrative.ID}

	// AllNarrativeEvents, not NarrativeEventsForContext: candidate keys live
	// in event artifacts, and the git_branch artifact carrying the
	// strongest provenance sits on a narrative's oldest events — exactly
	// the ones a compaction boundary hides from context-only readers.
	evts, err := s.AllNarrativeEvents(narrative.ID)
	if err != nil {
		return result, Stats{}, fmt.Errorf("loading events for narrative %d: %w", narrative.ID, err)
	}

	result.Excluded = excludedCandidates(evts, linkExclusions)

	candidates := gatherCandidates(evts, linkExclusions, limit, jiraActivity)
	if len(candidates) == 0 {
		// Untracked work is the default path, not a special case: no
		// tracker call, no LLM call, nothing written.
		return result, Stats{}, nil
	}

	verified, unresolved, err := verifyCandidates(resolve, narrative.ID, candidates)
	result.Unresolved = unresolved
	if err != nil {
		return result, Stats{}, err
	}

	if len(verified) == 0 {
		return result, Stats{}, nil
	}

	links, primaryKey, primaryConfidence, rationale, stats, err := resolveVerified(ctx, client, narrative, evts, verified, learnedRules)
	if err != nil {
		return result, stats, fmt.Errorf("classifying candidates for narrative %d: %w", narrative.ID, err)
	}

	result.Links = links
	result.Rationale = rationale

	promoted, err := persistLinks(s, narrative.ID, links, primaryKey, primaryConfidence, cfg.ConfidenceFloor)
	if err != nil {
		return result, stats, fmt.Errorf("persisting issue links for narrative %d: %w", narrative.ID, err)
	}
	result.Primary = promoted

	return result, stats, nil
}

// verifyCandidates confirms every candidate exists, each against the tracker its
// own Connection resolves to,
// regardless of provenance strength — rules/verify-correlations.md's
// requirement, since even a branch-derived key can be a stale name. A
// not-found key is dropped into unresolved and matching continues; a
// transport error (see IsTransportError) aborts this narrative entirely so
// the next pass retries it rather than recording a wrong conclusion drawn
// from an unreachable tracker.
func verifyCandidates(
	resolve TrackerResolver,
	narrativeID int64,
	candidates []Candidate,
) (verified []verifiedCandidate, unresolved []string, err error) {
	for _, c := range candidates {
		// Per candidate, not per pass. A candidate's Connection names which
		// configured backend actually holds it, and verifying it against a
		// different one silently reports a real ticket as nonexistent — or, worse,
		// resolves a colliding key to an unrelated issue on the wrong site.
		tracker, resolveErr := resolve(c.Connection)
		if resolveErr != nil {
			// A config gap, not a tracker answer: the connection is renamed,
			// removed, or was never configured. Recorded per candidate rather
			// than failing the narrative, because unlike a transport error this
			// does not fix itself between passes — aborting would fail this
			// narrative on every pass forever with no way to make progress.
			//
			// Carries the reason, unlike the not-found case below: a bare key
			// would tell a reviewer a ticket is missing when the truth is that
			// unjira never looked.
			unresolved = append(unresolved, fmt.Sprintf(
				"%s: %v", c.IssueKey, resolveErr,
			))

			continue
		}

		issue, getErr := tracker.GetIssue(c.IssueKey)
		if getErr != nil {
			if IsTransportError(getErr) {
				return nil, unresolved, fmt.Errorf(
					"verifying candidate %s for narrative %d: %w", c.IssueKey, narrativeID, getErr,
				)
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
		log.Printf(
			"correlator: narrative %d classifier returned no verdicts for %d verified candidate(s); "+
				"nothing linked, so this narrative still reads as untracked",
			narrative.ID, len(verified),
		)
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
			log.Printf(
				"correlator: narrative %d classifier returned unrecognized issue_key %q, ignoring",
				narrative.ID, verdict.IssueKey,
			)
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

// persistLinks writes links and, only when primaryKey is set and
// primaryConfidence clears floor, promotes it into the denormalized
// narratives.issue_key — both inside one transaction, matching Persist's
// convention of never holding a transaction open across an LLM round-trip
// (classifyCandidates has already returned by the time this runs).
//
// The floor gates promotion, never recording: every link is written
// regardless, since a dropped row would make a low-confidence match
// indistinguishable from finding nothing at all (config.MatchConfig's own
// doc comment). Returns the promoted key, or "" when nothing was promoted.
func persistLinks(
	s *store.Store,
	narrativeID int64,
	links []store.NarrativeIssue,
	primaryKey string,
	primaryConfidence float64,
	confidenceFloor float64,
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
			// A silently-unpromoted match is indistinguishable from
			// finding nothing at all, so this is logged, naming exactly
			// what was blocked and why.
			log.Printf(
				"correlator: narrative %d match %s at confidence %.2f below floor %.2f, not promoting",
				narrativeID, primaryKey, primaryConfidence, confidenceFloor,
			)
			return nil
		}

		if err := tx.SetNarrativeIssueLink(narrativeID, primaryKey, primaryConfidence); err != nil {
			return err
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
	if err != nil {
		return nil, stats, err
	}

	return verdicts, stats, nil
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
