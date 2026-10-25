package pipeline

import (
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/jcogilvie/unjira/internal/attribution"
	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/logging"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// SinkOptions are RunSinks' inputs beyond the store and config. Sinks maps each config
// sink name to its implementation; Now is the pass time, the enable floor of a sink's
// first run (D-19).
type SinkOptions struct {
	Sinks map[string]attribution.Sink
	Now   time.Time
	// Log is where the stage reports a skipped ticket. Nil is silent.
	Log *slog.Logger
}

// SinkRunResult is one sinks pass. Persisted are the new rows, ids set, for auto-commit;
// Superseded counts pending rows a new draft replaced, Retired those closed with none.
type SinkRunResult struct {
	Persisted           []store.ActionRow
	Superseded, Retired int
}

// RunSinks drafts every configured attribution sink's actions from the token ledger and
// persists them (R6-R8, R16). A ticket outside every writable scope is skipped; one
// ticket's error is joined and the others still run. A draft returned alongside an error
// still persists: the sink keeps it standing and the error asks a reviewer for the rest.
func RunSinks(s *store.Store, cfg config.Config, opts SinkOptions) (SinkRunResult, error) {
	var result SinkRunResult

	// The floor is when a sink was enabled, not when it first had a ticket to draft for:
	// record it for every writable tracker's sinks before reading anything (D-19).
	for _, tracker := range cfg.Trackers {
		if len(tracker.WritableScopes) == 0 {
			continue
		}
		for _, name := range cfg.SinksFor(tracker) {
			if _, err := s.SinkEnabledAt(tracker.Name, name, opts.Now); err != nil {
				return result, err
			}
		}
	}

	msgs, err := s.MessageAttributions(cfg.Match.ConfidenceFloor)
	if err != nil {
		return result, fmt.Errorf("reading attributions: %w", err)
	}

	keys := map[string]bool{}
	for _, m := range msgs {
		if m.State == store.AttributionTracked {
			keys[m.IssueKey] = true
		}
		if m.WorklogIssueKey != "" {
			keys[m.WorklogIssueKey] = true
		}
	}
	actions := map[string][]store.ActionRow{}
	for _, key := range slices.Sorted(maps.Keys(keys)) {
		rows, err := s.ActionsForIssue(key)
		if err != nil {
			return result, fmt.Errorf("reading actions on %s: %w", key, err)
		}
		actions[key] = rows
	}

	log := logging.For(opts.Log, "sinks")
	var errs []error
	for _, st := range attribution.BuildTicketStates(msgs, actions) {
		ref, err := tasktracker.ParseIssueKey(st.IssueKey)
		if err != nil {
			log.Warn("skipping ticket with an unparseable key", "issue", st.IssueKey, "err", err)

			continue
		}
		tracker, ok := cfg.TrackerForScope(ref.Scope)
		if !ok || !tracker.IsScopeWritable(ref.Scope) {
			// Logged or moved time on a ticket no sink may write is stranded: its
			// corrections are never drafted, and moved messages never reach their new
			// ticket's delta (R7). Say so; a plain unlogged read-only ticket is routine.
			if len(st.Logged) > 0 || len(st.Moved) > 0 {
				log.Warn("skipping ticket outside every writable scope that holds logged time; its corrections are not drafted",
					"issue", st.IssueKey, "worklogs", len(st.Logged), "moved_from", len(st.Moved))
			}

			continue
		}
		errs = append(errs, runTicketSinks(s, cfg.SinksFor(tracker), tracker, st, opts, &result)...)
	}

	return result, errors.Join(errs...)
}

// runTicketSinks runs each named sink on one ticket, persisting its drafts.
func runTicketSinks(s *store.Store, names []string, tracker config.Tracker, st attribution.TicketState,
	opts SinkOptions, result *SinkRunResult,
) []error {
	var errs []error
	for _, name := range names {
		sink, ok := opts.Sinks[name]
		if !ok {
			errs = append(errs, fmt.Errorf("%s: no attribution sink registered as %q", st.IssueKey, name))

			continue
		}
		enabledAt, err := s.SinkEnabledAt(tracker.Name, name, opts.Now)
		if err != nil {
			errs = append(errs, err)

			continue
		}
		drafts, draftErr := sink.Draft(attribution.DraftContext{Tracker: tracker, EnabledAt: enabledAt}, st)
		if draftErr != nil {
			errs = append(errs, fmt.Errorf("sink %s: %w", name, draftErr))
		}
		for _, d := range drafts {
			if err := persistDraft(s, d, result); err != nil {
				errs = append(errs, fmt.Errorf("sink %s on %s: %w", name, st.IssueKey, err))
			}
		}
	}

	return errs
}

// persistDraft writes one draft: a replacement supersedes its pending row in one
// transaction, a nil Row retires it, and anything else is a fresh insert.
func persistDraft(s *store.Store, d attribution.Draft, result *SinkRunResult) error {
	switch {
	case d.Row == nil && d.Supersedes == 0:
		return nil
	case d.Row == nil:
		if err := s.UpdateActionStatus(d.Supersedes, store.StatusSuppressed); err != nil {
			return fmt.Errorf("retiring action %d: %w", d.Supersedes, err)
		}
		result.Retired++

		return nil
	}

	row := *d.Row
	var err error
	if d.Supersedes != 0 {
		row.ID, err = s.SupersedeAction(d.Supersedes, store.StatusSuppressed, "", row)
		if err == nil {
			result.Superseded++
		}
	} else {
		row.ID, err = s.InsertAction(row)
	}
	if err != nil {
		return err
	}
	result.Persisted = append(result.Persisted, row)

	return nil
}

// RenderSinkResult formats one sinks pass for a terminal.
func RenderSinkResult(r SinkRunResult) string {
	return fmt.Sprintf("sinks: %d drafted, %d superseded, %d retired\n", len(r.Persisted), r.Superseded, r.Retired)
}
