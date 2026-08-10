package pipeline

import (
	"fmt"
	"strings"
	"time"

	"github.com/jcogilvie/unjira/internal/store"
)

// RenderDigest renders the phase-0 daily digest: what happened on day, and
// what looks untracked.
//
// Deterministic for now — it groups events and flags the unlinked ones. The
// LLM narrative pass (clustering events into work stories, matching against
// open issues, drift analysis) replaces the grouping here in phase 1; the
// rendering contract stays the same.
func RenderDigest(s *store.Store, day time.Time) (string, error) {
	rows, err := s.EventsOn(day)
	if err != nil {
		return "", fmt.Errorf("fetching events on %s: %w", day.Format("2006-01-02"), err)
	}

	var lines []string
	lines = append(lines, fmt.Sprintf("# unjira digest — %s", day.Format("2006-01-02")), "")

	if len(rows) == 0 {
		lines = append(lines, "No events observed.")
		return strings.Join(lines, "\n"), nil
	}

	var linked, unlinked []string
	for _, row := range rows {
		keys := stringArtifact(row.Artifacts, "ticket_keys")
		excluded := stringArtifact(row.Artifacts, "excluded_ticket_keys")
		realKeys := subtractKeys(keys, excluded)

		stamp := "--:--"
		formatted := row.OccurredAt.Format("15:04")
		if formatted != "" {
			stamp = formatted
		}

		entry := fmt.Sprintf("- %s [%s] %s", stamp, row.Source, row.Summary)
		switch {
		case len(realKeys) > 0:
			linked = append(linked, fmt.Sprintf("%s  (%s)", entry, strings.Join(realKeys, ", ")))
		case len(excluded) > 0:
			unlinked = append(unlinked, fmt.Sprintf("%s  [excluded from linking: %s]", entry, strings.Join(excluded, ", ")))
		default:
			unlinked = append(unlinked, entry)
		}
	}

	if len(linked) > 0 {
		lines = append(lines, "## Linked to tickets")
		lines = append(lines, linked...)
		lines = append(lines, "")
	}
	if len(unlinked) > 0 {
		lines = append(lines,
			"## Untracked work (no confident ticket link)",
		)
		lines = append(lines, unlinked...)
		lines = append(lines,
			"",
			"_These are candidates for a new ticket or a missed link — the main thing",
			"phase 0 is measuring. Corrections here become learned rules._",
		)
	}

	return strings.Join(lines, "\n"), nil
}

// stringArtifact reads a []any-of-strings artifact, tolerating absence.
func stringArtifact(artifacts map[string]any, key string) []string {
	raw, ok := artifacts[key].([]any)
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

// subtractKeys returns keys with every member of excluded removed, order
// preserved.
func subtractKeys(keys, excluded []string) []string {
	if len(excluded) == 0 {
		return keys
	}

	excludedSet := make(map[string]bool, len(excluded))
	for _, k := range excluded {
		excludedSet[k] = true
	}

	var out []string
	for _, k := range keys {
		if !excludedSet[k] {
			out = append(out, k)
		}
	}

	return out
}
