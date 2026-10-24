package config

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// SinkWorklog names the attribution sink that drafts Jira worklogs from the token ledger.
const SinkWorklog = "worklog"

// knownSinks is every attribution sink unjira implements.
var knownSinks = []string{SinkWorklog}

// KnownSinks returns every sink name attribution_sinks accepts.
func KnownSinks() []string { return slices.Clone(knownSinks) }

// WorklogConfig tunes the worklog attribution sink.
type WorklogConfig struct {
	// WorkSince is the backfill floor: no message timestamped before it is ever
	// proposed in a worklog. Absent means no backfill, and the floor is when the sink
	// was enabled. It is not --since: --since is the per-pass narration window on
	// `dev narrate` and `watch`, and neither defaults from the other.
	WorkSince Date `json:"work_since"`
	// IdleGap is the longest gap between consecutive parent-session messages still
	// counted as active time. Zero (the key absent) means worklog.DefaultIdleGap, which
	// that package owns, following WorkflowConfig.CacheTTL's precedent.
	IdleGap Span `json:"idle_gap"`
}

// Date is a calendar date, YYYY-MM-DD, read as local midnight. Pointer receivers
// throughout, as on Span, because UnmarshalText must have one.
type Date struct{ t time.Time }

// UnmarshalText parses YYYY-MM-DD in time.Local. A timestamp is refused rather than
// truncated, so a time of day the operator wrote is never silently dropped.
func (d *Date) UnmarshalText(text []byte) error {
	raw := strings.TrimSpace(string(text))

	t, err := time.ParseInLocation(time.DateOnly, raw, time.Local) //nolint:gosmopolitan // a calendar date is the operator's local day
	if err != nil {
		return fmt.Errorf("worklog.work_since %q: want a date, YYYY-MM-DD", raw)
	}

	d.t = t

	return nil
}

// Time returns the date's local midnight, in UTC.
func (d *Date) Time() time.Time {
	return d.t.UTC()
}

// IsZero reports whether no date was configured.
func (d *Date) IsZero() bool {
	return d.t.IsZero()
}

// SinksFor returns the attribution sinks tracker t runs. With attribution_sinks absent,
// a writable Jira tracker runs the worklog sink and every other tracker runs none; an
// explicit list, empty included, is returned as configured.
func (c Config) SinksFor(t Tracker) []string {
	if t.AttributionSinks != nil {
		return slices.Clone(*t.AttributionSinks)
	}

	conn, ok := c.ConnectionOf(t)
	if !ok || conn.Kind != KindJira || len(t.WritableScopes) == 0 {
		return nil
	}

	return []string{SinkWorklog}
}

// validateAttributionSinks checks an explicit attribution_sinks list on tracker t.
func validateAttributionSinks(t Tracker, kind ConnectionKind) []error {
	if t.AttributionSinks == nil {
		return nil
	}

	sinks := *t.AttributionSinks

	var errs []error

	if len(sinks) > 0 && kind != KindJira {
		errs = append(errs, fmt.Errorf(
			"tracker %q: attribution_sinks is set on a %s tracker: the only sink, worklog, writes Jira worklogs",
			t.Name, kind))
	}

	if len(sinks) > 0 && len(t.WritableScopes) == 0 {
		errs = append(errs, fmt.Errorf(
			"tracker %q: attribution_sinks is set, but writable_scopes is empty: a sink only drafts writes, "+
				"so it would draft nothing", t.Name))
	}

	seen := map[string]int{}

	for _, s := range sinks {
		seen[s]++

		switch {
		case !slices.Contains(knownSinks, s):
			errs = append(errs, fmt.Errorf("tracker %q: attribution_sinks names unknown sink %q (known: %s)",
				t.Name, s, strings.Join(knownSinks, ", ")))
		case seen[s] == 2: // report a repeat once, however many times it appears
			errs = append(errs, fmt.Errorf("tracker %q: attribution_sinks lists %q twice", t.Name, s))
		}
	}

	return errs
}
