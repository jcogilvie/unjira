// Package events defines the normalized event schema — the contract every
// collector emits. An Event is an observation about reality, not an
// instruction. Collectors are deterministic; anything requiring judgment
// (clustering, matching, drafting) happens downstream in the
// correlator/reconciler.
package events

import (
	"fmt"
	"regexp"
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
// Placeholder keys some workflows substitute for a real ticket (to satisfy a
// commit-message linter, for example) are matched too — see
// CompileLinkExclusionPatterns/PartitionExcludedKeys for how those are told
// apart from a real link downstream.
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

// CompileLinkExclusionPatterns compiles the configured exclude_from_linking
// regex patterns once, up front, so a bad pattern fails loudly at config-load
// time rather than being silently skipped (or panicking) during
// classification. An empty/nil patterns list is a no-op, not an error.
func CompileLinkExclusionPatterns(patterns []string) ([]*regexp.Regexp, error) {
	if len(patterns) == 0 {
		return nil, nil
	}

	compiled := make([]*regexp.Regexp, len(patterns))
	for i, pattern := range patterns {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("compiling exclude_from_linking pattern %q: %w", pattern, err)
		}
		compiled[i] = re
	}

	return compiled, nil
}

// PartitionExcludedKeys splits keys into kept (no configured pattern
// matched) and excluded (at least one did), preserving order within each.
// A nil/empty compiled list keeps every key.
func PartitionExcludedKeys(keys []string, compiled []*regexp.Regexp) (kept, excluded []string) {
	for _, key := range keys {
		if matchesAny(key, compiled) {
			excluded = append(excluded, key)
		} else {
			kept = append(kept, key)
		}
	}

	return kept, excluded
}

func matchesAny(key string, compiled []*regexp.Regexp) bool {
	for _, re := range compiled {
		if re.MatchString(key) {
			return true
		}
	}

	return false
}
