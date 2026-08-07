package pipeline

import (
	"fmt"
	"strings"
	"time"

	"github.com/jcogilvie/unjira/internal/events"
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
		var keys []string
		if raw, ok := row.Artifacts["ticket_keys"].([]any); ok {
			for _, k := range raw {
				key, _ := k.(string)
				if key != "" && !events.IsSentinelKey(key) {
					keys = append(keys, key)
				}
			}
		}

		stamp := "--:--"
		formatted := row.OccurredAt.Format("15:04")
		if formatted != "" {
			stamp = formatted
		}

		entry := fmt.Sprintf("- %s [%s] %s", stamp, row.Source, row.Summary)
		if len(keys) > 0 {
			linked = append(linked, fmt.Sprintf("%s  (%s)", entry, strings.Join(keys, ", ")))
		} else {
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
