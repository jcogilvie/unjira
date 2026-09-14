package store

// issueactivity.go answers one question for the correlator: which issue keys has
// this store actually seen tracker activity for, and how recently?
//
// It exists for finding F9. gatherCandidates ranks prose-mentioned issue keys and
// truncates to a cap, and its tiebreak was alphabetical — which on a monotonic
// issue counter systematically favours OLDER tickets, making the tiebreak
// anti-correlated with relevance rather than merely unrelated to it. Recent
// tracker activity is the deterministic, pre-model discriminator that replaces it.
//
// The correlator cannot query this itself: gatherCandidates is a pure function
// with no store, network, or tracker access, and that is an architecture
// invariant (see CLAUDE.md). So Match loads this map once per pass and hands it
// down.

import (
	"fmt"
	"time"

	"github.com/jcogilvie/unjira/internal/events"
)

// IssueActivity returns the most recent occurred_at per issue key across every
// tracker-sourced event in the store.
//
// Deliberately NOT filtered by source name. The obvious implementation adds
// `WHERE source = 'jira'`, and it would be wrong the moment a second tracker
// backend lands: a GitHub Issues collector's events are equally good evidence
// that an issue moved, and a source filter would make this query silently blind
// to them — the failure mode being a ranking that quietly stops working for one
// backend while still returning plausible numbers. What makes an event count here
// is that it names an issue, which is exactly what the predicate tests.
//
// Unbounded by time, for the same reason. F9's write-up proposed gating
// corroboration on a bounded recency window; measured against real data that
// window is only correct in a narrow ~21-30 day band (too short and the genuinely
// relevant ticket falls outside it, too long and the pool exceeds the candidate
// cap so the alphabetical tiebreak decides again). Ordering by recency gets the
// same benefit without a knob whose correct value nobody can calibrate, so the
// caller receives every key and sorts.
//
// Rows whose occurred_at will not parse are SKIPPED rather than failing the call.
// This map is a soft ranking signal, not a correctness gate: one malformed
// timestamp should cost that key its promotion, not cost the whole pass its
// ranking. The skip is silent because a parse failure here is a collector bug
// that belongs in the collector's own validation, and logging per row would emit
// one line per event on a corrupted store.
func (s *Store) IssueActivity() (map[string]time.Time, error) {
	// The artifact key is interpolated from internal/events' declared contract
	// rather than written as a literal, so this query and events.StatusChangeOf
	// cannot drift apart — the same treatment LatestStatusEvent uses.
	query := fmt.Sprintf(
		`SELECT json_extract(artifacts, '$.%[1]s') AS issue_key, MAX(occurred_at)
		   FROM events
		  WHERE json_extract(artifacts, '$.%[1]s') IS NOT NULL
		    AND json_extract(artifacts, '$.%[1]s') != ''
		  GROUP BY issue_key`,
		events.ArtifactIssueKey,
	)

	rows, err := s.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("reading issue activity: %w", err)
	}
	defer func() { _ = rows.Close() }()

	activity := make(map[string]time.Time)
	for rows.Next() {
		var (
			issueKey   string
			occurredAt string
		)
		if err := rows.Scan(&issueKey, &occurredAt); err != nil {
			return nil, fmt.Errorf("scanning issue activity: %w", err)
		}

		at, parseErr := time.Parse(time.RFC3339, occurredAt)
		if parseErr != nil {
			continue
		}

		activity[issueKey] = at
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating issue activity: %w", err)
	}

	return activity, nil
}
