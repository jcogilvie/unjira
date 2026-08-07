// Package events defines the normalized event schema — the contract every
// collector emits. An Event is an observation about reality, not an
// instruction. Collectors are deterministic; anything requiring judgment
// (clustering, matching, drafting) happens downstream in the
// correlator/reconciler.
package events

import (
	"regexp"
	"strings"
	"time"
)

// Event is a single normalized observation from one source stream.
type Event struct {
	Source string `json:"source"`
	// ExternalID is a stable id within the source; (Source, ExternalID) dedupes.
	ExternalID string         `json:"externalId"`
	OccurredAt time.Time      `json:"occurredAt"`
	Actor      string         `json:"actor,omitempty"`
	Summary    string         `json:"summary"`
	Artifacts  map[string]any `json:"artifacts"`
	// RawRef points back to the raw source material.
	RawRef string `json:"rawRef,omitempty"`
}

// NewEvent constructs an Event with Artifacts initialized to a non-nil,
// empty map — mirroring Pydantic's default_factory=dict, since Go's zero
// value for a map is nil and unsafe to write to.
func NewEvent(source, externalID string, occurredAt time.Time, summary string) Event {
	return Event{
		Source:     source,
		ExternalID: externalID,
		OccurredAt: occurredAt,
		Summary:    summary,
		Artifacts:  make(map[string]any),
	}
}

// TicketKeyRegexp matches PROJ-123 style keys for any project prefix.
// Sentinel keys — $PROJECT-0 / $PROJECT-1, subbed in by devs to placate
// commit checkers — are matched too; downstream treats them as an explicit
// "unlinked work" flag, not a real link.
var TicketKeyRegexp = regexp.MustCompile(`\b[A-Z][A-Z0-9]{1,9}-\d+\b`)

// ExtractTicketKeys returns all candidate Jira keys in text, order-preserving
// and deduplicated.
func ExtractTicketKeys(text string) []string {
	seen := make(map[string]bool)
	var keys []string

	for _, match := range TicketKeyRegexp.FindAllString(text, -1) {
		if !seen[match] {
			seen[match] = true
			keys = append(keys, match)
		}
	}

	return keys
}

// sentinelIssueNumbers are the numeric suffixes devs use to placate commit
// checkers without a real ticket. Any project prefix counts.
//
// Caveat: -0 is never a valid Jira issue, but $PROJECT-1 is a real issue (the
// project's first), so a -1 match is only probably a sentinel — the
// correlator should break the tie with a Jira existence check.
var sentinelIssueNumbers = map[string]bool{"0": true, "1": true}

// IsSentinelKey reports whether key is a $PROJECT-0 / $PROJECT-1 sentinel key.
func IsSentinelKey(key string) bool {
	suffix := key
	if idx := strings.LastIndex(key, "-"); idx >= 0 {
		suffix = key[idx+1:]
	}

	return sentinelIssueNumbers[suffix]
}
