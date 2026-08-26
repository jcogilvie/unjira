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
	// ProvenanceProseFirst is a key first mentioned in free-text prose
	// (commit messages, session transcripts).
	ProvenanceProseFirst Provenance = "prose_first"
	// ProvenanceProseLater is a key mentioned in prose after the first
	// mention — see the package doc comment for why no finer-grained prose
	// tier exists.
	ProvenanceProseLater Provenance = "prose_later"
)

// Rank returns p's tiebreaker strength: lower is stronger. Used only to
// order otherwise-equal candidates and as a prior in the matching prompt,
// never in place of verification. An unrecognized Provenance ranks weakest
// (4) rather than erroring, because Rank is a soft ordering signal, not a
// closed-set gate like parseRole — an unexpected value here should degrade
// gracefully, not fail a matching pass.
func (p Provenance) Rank() int {
	switch p {
	case ProvenanceBranch:
		return 0
	case ProvenanceJiraEvent:
		return 1
	case ProvenanceProseFirst:
		return 2
	case ProvenanceProseLater:
		return 3
	default:
		return 4
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
// raw is run through llm.StripJSONFence first — see that function's doc
// comment for why every parser in this package does this.
func parseMatchResponse(raw string) ([]matchVerdict, error) {
	var items []rawVerdict
	if err := json.Unmarshal([]byte(llm.StripJSONFence(raw)), &items); err != nil {
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
