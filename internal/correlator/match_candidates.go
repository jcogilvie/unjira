package correlator

import (
	"regexp"
	"sort"

	"github.com/jcogilvie/unjira/internal/events"
)

// Candidate is one issue key a narrative might be attributed to, with how it
// was found. Gathering is deliberately dumb: it records every mention it can
// find and ranks them by provenance, but never decides which one (if any)
// actually owns the work — that judgment belongs to Match, once verification
// against live Jira has pruned candidates that no longer exist or are already
// closed (see rules/intent-not-outcome.md).
type Candidate struct {
	IssueKey   string
	Provenance Provenance
	// Connection is the configured Jira connection name, when the key came from
	// a jira-source event that recorded one. Empty otherwise.
	Connection string
}

// gatherCandidates walks evts and returns every non-excluded issue key
// mentioned, ranked strongest-provenance-first and truncated to limit
// (limit <= 0 means no truncation).
//
// Three provenance tiers come out of today's artifacts:
//
//   - ProvenanceJiraEvent from artifacts["issue_key"] on a jira-source event —
//     the event is already about that issue, so this is direct, not inferred.
//   - ProvenanceBranch from re-running events.ExtractTicketKeys over
//     artifacts["git_branch"]. This is re-derived rather than trusted from
//     ticket_keys because the claudecode collector flattens branch-derived and
//     prose-derived keys into that one artifact, losing exactly the
//     branch-vs-prose distinction that matters most for attribution — the
//     branch is an explicit human act of naming the ticket for this work,
//     prose is not.
//   - ProvenanceProseFirst / ProvenanceProseLater from walking ticket_keys in
//     order: index 0 is "first mentioned", everything after is "later
//     mentioned". No finer split exists because the collector already
//     deduped mentions into one order-preserving slice — "which of the later
//     mentions" is information that was discarded before this function ever
//     saw it.
//
// The same key can surface at multiple tiers across events (mentioned in
// prose in one session, then named in a branch in another); only the
// strongest surviving provenance is kept, and a Connection recorded by a
// stronger mention is never overwritten by a weaker mention that lacks one.
//
// Truncation keeps the head (the strongest candidates) because the limit
// exists only to bound downstream cost (GetIssue fan-out, prompt size) — a
// tail-keeping truncation would routinely throw away the branch candidate in
// favor of prose noise, discarding the answer to save a few bytes.
func gatherCandidates(evts []Event, linkExclusions []*regexp.Regexp, limit int) []Candidate {
	best := make(map[string]Candidate)

	upsert := func(issueKey string, provenance Provenance, connection string) {
		existing, ok := best[issueKey]
		if !ok {
			best[issueKey] = Candidate{IssueKey: issueKey, Provenance: provenance, Connection: connection}
			return
		}

		switch {
		case provenance.Rank() < existing.Provenance.Rank():
			// Stronger provenance wins outright, but don't lose a Connection the
			// existing (weaker) entry already had if the new, stronger mention
			// doesn't carry one.
			if connection == "" {
				connection = existing.Connection
			}
			best[issueKey] = Candidate{IssueKey: issueKey, Provenance: provenance, Connection: connection}
		case existing.Connection == "" && connection != "":
			// Same or weaker provenance, but it fills in a Connection the
			// existing entry was missing.
			existing.Connection = connection
			best[issueKey] = existing
		}
	}

	for _, e := range evts {
		if issueKey, ok := e.Artifacts["issue_key"].(string); ok && issueKey != "" {
			connection, _ := e.Artifacts["connection"].(string)
			upsert(issueKey, ProvenanceJiraEvent, connection)
		}

		if branch, ok := e.Artifacts["git_branch"].(string); ok && branch != "" {
			for _, key := range events.ExtractTicketKeys(branch) {
				upsert(key, ProvenanceBranch, "")
			}
		}

		for i, key := range ticketKeys(e) {
			provenance := ProvenanceProseLater
			if i == 0 {
				provenance = ProvenanceProseFirst
			}
			upsert(key, provenance, "")
		}
	}

	out := make([]Candidate, 0, len(best))
	for _, c := range best {
		out = append(out, c)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Provenance.Rank() != out[j].Provenance.Rank() {
			return out[i].Provenance.Rank() < out[j].Provenance.Rank()
		}
		return out[i].IssueKey < out[j].IssueKey
	})

	out = dropExcluded(out, linkExclusions)

	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}

	return out
}

// dropExcluded filters candidates whose issue key matches a configured
// exclude_from_linking pattern (a placeholder key satisfying a commit
// linter, not a real link). A nil/empty linkExclusions is a no-op.
func dropExcluded(candidates []Candidate, linkExclusions []*regexp.Regexp) []Candidate {
	if len(linkExclusions) == 0 {
		return candidates
	}

	keys := make([]string, 0, len(candidates))
	for _, c := range candidates {
		keys = append(keys, c.IssueKey)
	}

	kept, _ := events.PartitionExcludedKeys(keys, linkExclusions)
	keptSet := make(map[string]bool, len(kept))
	for _, k := range kept {
		keptSet[k] = true
	}

	out := make([]Candidate, 0, len(kept))
	for _, c := range candidates {
		if keptSet[c.IssueKey] {
			out = append(out, c)
		}
	}

	return out
}

// excludedCandidates reports which issue keys mentioned across evts were
// removed by linkExclusions, so a narrative that looks untracked can be
// explained by the placeholder that was dropped rather than appearing to
// have had no candidates at all. Nil when there are no patterns.
func excludedCandidates(evts []Event, linkExclusions []*regexp.Regexp) []string {
	if len(linkExclusions) == 0 {
		return nil
	}

	seen := make(map[string]bool)
	var keys []string

	for _, e := range evts {
		if issueKey, ok := e.Artifacts["issue_key"].(string); ok && issueKey != "" && !seen[issueKey] {
			seen[issueKey] = true
			keys = append(keys, issueKey)
		}

		if branch, ok := e.Artifacts["git_branch"].(string); ok && branch != "" {
			for _, key := range events.ExtractTicketKeys(branch) {
				if !seen[key] {
					seen[key] = true
					keys = append(keys, key)
				}
			}
		}

		for _, key := range ticketKeys(e) {
			if !seen[key] {
				seen[key] = true
				keys = append(keys, key)
			}
		}
	}

	_, excluded := events.PartitionExcludedKeys(keys, linkExclusions)

	return excluded
}

// ticketKeys reads e's ticket_keys artifact, which is always []any (the
// claudecode collector converts before assigning, and the shape survives a
// JSON round-trip through SQLite the same way) — never []string. Keeping only
// non-empty strings tolerates absence and any malformed element without
// erroring, since a corrupt artifact here should degrade to "no prose
// candidates", not fail the whole gather.
func ticketKeys(e Event) []string {
	raw, ok := e.Artifacts["ticket_keys"].([]any)
	if !ok {
		return nil
	}

	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok && s != "" {
			out = append(out, s)
		}
	}

	return out
}
