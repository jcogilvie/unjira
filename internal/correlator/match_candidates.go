package correlator

import (
	"regexp"
	"sort"
	"time"

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
// Three provenance tiers come directly out of today's artifacts:
//
//   - ProvenanceJiraEvent from events.ArtifactIssueKey on a jira-source event —
//     the event is already about that issue, so this is direct, not inferred.
//   - ProvenanceBranch from re-running events.ExtractTicketKeys over
//     events.ArtifactGitBranch. This is re-derived rather than trusted from
//     ArtifactTicketKeys because the claudecode collector flattens
//     branch-derived and prose-derived keys into that one artifact, losing
//     exactly the branch-vs-prose distinction that matters most for
//     attribution — the branch is an explicit human act of naming the ticket
//     for this work, prose is not.
//   - ProvenanceProseFirst / ProvenanceProseLater from walking
//     events.TicketKeysOf in order: index 0 is "first mentioned", everything
//     after is "later mentioned". No finer split exists because the collector
//     already deduped mentions into one order-preserving slice — "which of
//     the later mentions" is information that was discarded before this
//     function ever saw it.
//
// The same key can surface at multiple tiers across events (mentioned in
// prose in one session, then named in a branch in another); only the
// strongest surviving provenance is kept, and a Connection recorded by a
// stronger mention is never overwritten by a weaker mention that lacks one.
//
// A fourth tier, ProvenanceCorroborated, sits between JiraEvent and ProseFirst:
// a prose-only key whose issue appears in jiraActivity, meaning this store has
// already collected Jira events for it. Keys in that tier are ordered by
// most-recent activity first; every other tier keeps its alphabetical order.
//
// jiraActivity is passed in rather than queried because this function is pure —
// no store, no network, no tracker (see CLAUDE.md's correlator invariant). Match
// loads it once per pass and hands it down. A nil or empty map is "we know
// nothing", not "nothing is relevant": ranking then degrades to exactly the
// pre-F9 behaviour rather than to a worse one, which matters because most keys in
// a fresh store have no collected Jira activity at all — the jira collector only
// ever saw what its JQL scoped.
//
// Both halves of that tier are load-bearing. The tier alone does not fix F9: on
// the measured case 30 of 73 keys were corroborated, still 3x the cap, so an
// alphabetical sort inside the new tier re-decides the same way. See
// match_corroboration_test.go for the numbers, and for why the finding's
// originally-proposed recency WINDOW was rejected (it is only correct in a narrow
// 21-30 day band, so its config knob would be a latent bug).
//
// Truncation keeps the head (the strongest candidates) because the limit
// exists only to bound downstream cost (GetIssue fan-out, prompt size) — a
// tail-keeping truncation would routinely throw away the branch candidate in
// favor of prose noise, discarding the answer to save a few bytes.
func gatherCandidates(
	evts []Event,
	linkExclusions []*regexp.Regexp,
	limit int,
	jiraActivity map[string]time.Time,
) []Candidate {
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
		if issueKey, ok := e.Artifacts[events.ArtifactIssueKey].(string); ok && issueKey != "" {
			connection, _ := e.Artifacts[events.ArtifactConnection].(string)
			upsert(issueKey, ProvenanceJiraEvent, connection)
		}

		if branch, ok := e.Artifacts[events.ArtifactGitBranch].(string); ok && branch != "" {
			for _, key := range events.ExtractTicketKeys(branch) {
				upsert(key, ProvenanceBranch, "")
			}
		}

		for i, key := range events.TicketKeysOf(e) {
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

	promoteCorroborated(out, jiraActivity)
	rankCandidates(out, jiraActivity)

	out = dropExcluded(out, linkExclusions)

	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}

	return out
}

// promoteCorroborated relabels prose-tier candidates whose issue has collected
// Jira activity, in place.
//
// Runs AFTER the gather loop rather than inside upsert so it sees each key's FINAL
// provenance. Promoting during the gather would race the tiers: a key mentioned in
// prose and later named in a branch would be promoted on its prose mention and
// then need un-promoting, and upsert only ever moves provenance stronger.
//
// Only the prose tiers are eligible. ProvenanceCorroborated is WEAKER than both
// branch and jira_event, so "promoting" one of those would be a demotion dressed
// as a promotion — the easy mistake here, because the corroborated key has more
// evidence attached and reads like it ought to win.
func promoteCorroborated(candidates []Candidate, jiraActivity map[string]time.Time) {
	for i, c := range candidates {
		if _, active := jiraActivity[c.IssueKey]; !active {
			continue
		}
		if c.Provenance != ProvenanceProseFirst && c.Provenance != ProvenanceProseLater {
			continue
		}

		candidates[i].Provenance = ProvenanceCorroborated
	}
}

// rankCandidates sorts strongest-provenance-first, in place.
//
// Within the corroborated tier ONLY, most-recent Jira activity wins; every other
// tier stays alphabetical. That narrow scope is the half of F9's fix that actually
// rescues its cited example — the corroborated pool is routinely larger than the
// candidate cap (30 of 73 on the measured narrative), so without recency an
// alphabetical sort inside the new tier re-decides exactly as before.
func rankCandidates(candidates []Candidate, jiraActivity map[string]time.Time) {
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Provenance.Rank() != candidates[j].Provenance.Rank() {
			return candidates[i].Provenance.Rank() < candidates[j].Provenance.Rank()
		}

		if candidates[i].Provenance == ProvenanceCorroborated {
			iAt, jAt := jiraActivity[candidates[i].IssueKey], jiraActivity[candidates[j].IssueKey]
			if !iAt.Equal(jAt) {
				return iAt.After(jAt)
			}
			// Identical timestamps fall through to the key comparison rather than
			// leaving order to sort.Slice's instability. Determinism is not cosmetic:
			// an unstable candidate list changes the matching prompt between two
			// otherwise-identical passes.
		}

		return candidates[i].IssueKey < candidates[j].IssueKey
	})
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
		if issueKey, ok := e.Artifacts[events.ArtifactIssueKey].(string); ok && issueKey != "" && !seen[issueKey] {
			seen[issueKey] = true
			keys = append(keys, issueKey)
		}

		if branch, ok := e.Artifacts[events.ArtifactGitBranch].(string); ok && branch != "" {
			for _, key := range events.ExtractTicketKeys(branch) {
				if !seen[key] {
					seen[key] = true
					keys = append(keys, key)
				}
			}
		}

		for _, key := range events.TicketKeysOf(e) {
			if !seen[key] {
				seen[key] = true
				keys = append(keys, key)
			}
		}
	}

	_, excluded := events.PartitionExcludedKeys(keys, linkExclusions)

	return excluded
}
