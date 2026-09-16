package correlator

import (
	"encoding/json"
	"fmt"

	"github.com/jcogilvie/unjira/internal/llm"
	"github.com/jcogilvie/unjira/internal/store"
)

// Role is which relationship a narrative has to a candidate issue, as judged
// by the matching LLM call. An alias (not a definition) for store.Role, so a
// correlator.Role parsed here assigns straight into a store.NarrativeIssue
// with no conversion.
//
// Deliberately closed: an open string field rots. If the model were free to
// emit "relates", "related-to", "blocked-by", and "blocker" as synonyms for
// the same idea, every downstream consumer would have to pattern-match a
// vocabulary that grows without bound. A closed set lets parseRole reject an
// unknown role loudly instead, which this codebase prefers over accepting
// data it cannot interpret.
//
// Two roles were considered and rejected:
//   - "gating": the motivating case (a feature ticket whose prod deploy is
//     gated by a separate change-management ticket) turned out to be
//     co-representation of one body of work, not a dependency — RoleSameWork
//     covers it. A true blocking dependency belongs to a *different*
//     narrative's own work, not a role on this one.
//   - "caused_by" / "discovered_while": neither has a statable behavioural
//     difference from RoleMentioned — all three mean "do not attribute this
//     narrative's work to this issue" — so splitting them would add
//     vocabulary without adding information.
type Role = store.Role

const (
	// RolePrimary is the record the work is principally tracked in. Exactly
	// one per narrative — enforced both by parseMatchResponse (see the "two
	// primaries" case) and by a partial unique index at the store layer.
	RolePrimary Role = "primary"
	// RoleSameWork is a co-representation of the same body of work elsewhere
	// (see the package doc comment on Role for the motivating PAAS/SUMO
	// case). Not a dependency: one body of work recorded twice for two
	// audiences, not two bodies of work related to each other.
	RoleSameWork Role = "same_work"
	// RoleMentioned means the issue was referenced in the events but is not
	// itself the work — the correct role for a citation, a "caused by", or a
	// "discovered while" reference alike.
	RoleMentioned Role = "mentioned"
)

// parseRole validates raw against the closed Role set, returning an error
// naming the rejected value rather than coercing it. Exact match only — no
// case-folding, no trimming — because a model that emits "PRIMARY" or
// "primary " with stray whitespace is exhibiting exactly the kind of
// vocabulary drift this closed set exists to catch, not something to paper
// over silently.
func parseRole(raw string) (Role, error) {
	switch Role(raw) {
	case RolePrimary, RoleSameWork, RoleMentioned:
		return Role(raw), nil
	default:
		return "", fmt.Errorf("parsing match response: unrecognized role %q", raw)
	}
}

// Provenance is where a candidate issue key was found before verification —
// used only as a tiebreaker between otherwise-equal candidates and as a
// prior weight in the matching prompt, never as a substitute for verifying
// the issue actually exists and is live (see rules/intent-not-outcome.md).
//
// The tiers are limited to what today's artifacts support, not to what's
// theoretically orderable:
//   - There is no commit-trailer tier because nothing in this pipeline
//     records commit trailers today; adding one before a collector produces
//     that data would be a rank with no real inputs to occupy it.
//   - ProvenanceProseFirst and ProvenanceProseLater are the only prose tiers,
//     and prose keys beyond "first" and "later" are not separable: the
//     claudecode collector dedupes mentioned issue keys into one
//     order-preserving slice, so only first-mention-vs-later survives:
//     "which of the later mentions" is information the collector has
//     already discarded.
type Provenance string

const (
	// ProvenanceBranch is a key found in a branch name — the strongest
	// signal, because a branch name is an explicit human act of naming the
	// ticket for this work.
	ProvenanceBranch Provenance = "branch"
	// ProvenanceJiraEvent is a key found in a Jira-sourced event (e.g. the
	// issue the event is already about).
	ProvenanceJiraEvent Provenance = "jira_event"
	// ProvenanceCorroborated is a key found only in prose, but whose issue has
	// Jira activity this store has already collected. The mention is still an
	// inference — nobody named this ticket for this work — but it is an inference
	// about a ticket that demonstrably moved, which is strictly more than a bare
	// mention.
	//
	// It ranks below ProvenanceJiraEvent because that means the event IS about the
	// issue, and below ProvenanceBranch because a branch name is an explicit human
	// act of naming the ticket, which no amount of activity timing equals.
	//
	// Introduced for finding F9: prose ties broke alphabetically, and on a
	// monotonic issue counter that favours OLDER tickets — so the tiebreak was
	// anti-correlated with relevance, not merely unrelated to it. See
	// match_corroboration_test.go for the measurement, including why this tier
	// only works alongside the recency ordering in gatherCandidates and not on
	// its own.
	ProvenanceCorroborated Provenance = "corroborated"
	// ProvenanceSCMCommand is a key a claude_code session named while AUTHORING in
	// source control — a commit message, a branch creation, a PR title. Recovered
	// from tool inputs the collector previously discarded (finding F20).
	//
	// Ranks just below ProvenanceBranch and above ProvenanceJiraEvent, because it is
	// the same KIND of signal as a branch name — a human naming the ticket for this
	// work — but one step less committal: a branch names the work for its whole life,
	// while a commit names one change within it, and a session may author against
	// several tickets. Above ProvenanceJiraEvent because that means "a Jira event in
	// this cluster mentions the issue", which is the tracker talking about itself,
	// where this is the developer talking about their work.
	//
	// Only AUTHORING commands qualify, and the exclusion happens at the collector:
	// keys from reading commands (`git log --grep=`, `gh pr view`) are dropped there,
	// because 8 of the keys measured appeared only in those — an agent investigating a
	// ticket it may have nothing to do with. See internal/collector/claudecode/scm.go.
	ProvenanceSCMCommand Provenance = "scm_command"
	// ProvenanceProseFirst is a key first mentioned in free-text prose
	// (commit messages, session transcripts).
	ProvenanceProseFirst Provenance = "prose_first"
	// ProvenanceProseLater is a key mentioned in prose after the first
	// mention — see the package doc comment for why no finer-grained prose
	// tier exists.
	ProvenanceProseLater Provenance = "prose_later"
	// ProvenanceReviewer is a key a human asserted during triage's [t]arget.
	// Distinct from every tier above because those are INFERENCES from
	// artifacts, and this is a person stating where the work belongs. It ranks
	// strongest for that reason: a reviewer looking at the drafted text and the
	// ticket together has strictly more information than a branch-name parse.
	//
	// Recorded distinctly rather than reusing ProvenanceBranch so a later pass
	// can tell "someone decided this" from "we guessed this", which matters when
	// deciding whether to revisit a link.
	ProvenanceReviewer Provenance = "reviewer"
)

// Rank returns p's tiebreaker strength: lower is stronger. Used only to
// order otherwise-equal candidates and as a prior in the matching prompt,
// never in place of verification. An unrecognized Provenance ranks weakest
// (one past the weakest known tier) rather than erroring, because Rank is a soft
// ordering signal, not a closed-set gate like parseRole — an unexpected value
// here should degrade gracefully, not fail a matching pass.
func (p Provenance) Rank() int {
	switch p {
	case ProvenanceReviewer:
		// Strongest, ahead of every inferred tier: a human who looked at the
		// drafted text and the ticket together has more information than any
		// artifact parse. Without this case it would fall to default (weakest),
		// which inverts the intent — a reviewer's explicit correction losing a
		// tiebreak to a branch-name guess.
		return -1
	case ProvenanceBranch:
		return 0
	case ProvenanceSCMCommand:
		return 1
	case ProvenanceJiraEvent:
		return 2
	case ProvenanceCorroborated:
		return 3
	case ProvenanceProseFirst:
		return 4
	case ProvenanceProseLater:
		return 5
	default:
		return 6
	}
}

// matchVerdict is one candidate issue's judged relationship to a narrative,
// after parseMatchResponse has validated it against the closed Role set and
// the confidence range.
type matchVerdict struct {
	IssueKey   string
	Role       Role
	Confidence float64
	Rationale  string
}

// rawVerdict is the wire shape of one element in the matching model's JSON
// array response, before validation.
type rawVerdict struct {
	IssueKey   string  `json:"issue_key"`
	Role       string  `json:"role"`
	Confidence float64 `json:"confidence"`
	Rationale  string  `json:"rationale"`
}

// parseMatchResponse validates and converts raw (the matching model's
// response body) into matchVerdicts. Every rejection names the offending
// entry's index and issue key so a bad response is diagnosable without
// re-running the call: an empty issue_key, an unrecognized role, a
// confidence outside [0, 1], or more than one entry claiming RolePrimary.
//
// The "more than one primary" case matters beyond parser hygiene: the store
// enforces exactly one primary per narrative via a partial unique index, so
// a response with two would otherwise fail at write time with a constraint
// error naming a column, not the model's mistake. Catching it here produces
// a diagnosable message instead.
//
// raw is run through llm.JSONArrayPayload first, which strips a markdown fence
// and wraps a lone object into a one-element array — see that function for why
// each is treated as a property of the interface, not a prompt bug.
func parseMatchResponse(raw string) ([]matchVerdict, error) {
	var items []rawVerdict
	if err := json.Unmarshal([]byte(llm.JSONArrayPayload(raw)), &items); err != nil {
		return nil, fmt.Errorf("parsing match response %q: %w", raw, err)
	}

	verdicts := make([]matchVerdict, 0, len(items))
	primaryCount := 0

	for i, item := range items {
		if item.IssueKey == "" {
			return nil, fmt.Errorf("parsing match response: entry %d has empty issue_key", i)
		}

		role, err := parseRole(item.Role)
		if err != nil {
			return nil, fmt.Errorf("parsing match response: entry %d (issue_key %q): %w", i, item.IssueKey, err)
		}

		if item.Confidence < 0 || item.Confidence > 1 {
			return nil, fmt.Errorf(
				"parsing match response: entry %d (issue_key %q) has confidence %v outside [0, 1]",
				i, item.IssueKey, item.Confidence,
			)
		}

		if role == RolePrimary {
			primaryCount++
		}

		verdicts = append(verdicts, matchVerdict{
			IssueKey:   item.IssueKey,
			Role:       role,
			Confidence: item.Confidence,
			Rationale:  item.Rationale,
		})
	}

	if primaryCount > 1 {
		return nil, fmt.Errorf("parsing match response: %d entries claim role primary, want at most 1", primaryCount)
	}

	return verdicts, nil
}
