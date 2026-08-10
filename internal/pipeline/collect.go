// Package pipeline runs the collect pass and renders the phase-0 digest.
package pipeline

import (
	"regexp"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/store"
)

// Collector reads its source since the last cursor, emits normalized
// events via visit, and advances its cursor via the store. Collectors must
// be deterministic and idempotent: re-running one is always safe because
// (source, external_id) dedupes at insert.
type Collector interface {
	Name() string
	Collect(s *store.Store, options map[string]any, visit func(events.Event)) error
}

// RunCollect runs every enabled collector found in registry, persisting new
// events. Returns {collector_name: new_event_count}; a collector enabled in
// config but not present in registry reports -1.
//
// linkExclusions (from config.CompiledLinkExclusions, compiled once by the
// caller) is applied to every event's ticket_keys artifact before insert: a
// matching key is recorded in a new excluded_ticket_keys artifact, never
// removed from ticket_keys itself. A nil/empty linkExclusions is a no-op.
func RunCollect(
	cfg config.Config,
	s *store.Store,
	registry map[string]func() Collector,
	linkExclusions []*regexp.Regexp,
) (map[string]int, error) {
	results := make(map[string]int)

	for name, options := range cfg.EnabledCollectors() {
		factory, ok := registry[name]
		if !ok {
			results[name] = -1
			continue
		}

		collector := factory()
		inserted := 0
		var collectErr error

		err := collector.Collect(s, options, func(event events.Event) {
			if collectErr != nil {
				return
			}

			annotateExcludedTicketKeys(event, linkExclusions)

			ok, err := s.InsertEvent(event)
			if err != nil {
				collectErr = err
				return
			}
			if ok {
				inserted++
			}
		})
		if err != nil {
			return nil, err
		}
		if collectErr != nil {
			return nil, collectErr
		}

		results[name] = inserted
	}

	return results, nil
}

// annotateExcludedTicketKeys sets event.Artifacts["excluded_ticket_keys"]
// when any of its ticket_keys match a configured link-exclusion pattern.
// ticket_keys itself is left untouched — nothing is ever removed from it.
func annotateExcludedTicketKeys(event events.Event, linkExclusions []*regexp.Regexp) {
	if len(linkExclusions) == 0 {
		return
	}

	raw, ok := event.Artifacts["ticket_keys"].([]any)
	if !ok || len(raw) == 0 {
		return
	}

	keys := make([]string, 0, len(raw))
	for _, k := range raw {
		if s, ok := k.(string); ok && s != "" {
			keys = append(keys, s)
		}
	}

	_, excluded := events.PartitionExcludedKeys(keys, linkExclusions)
	if len(excluded) == 0 {
		return
	}

	excludedAny := make([]any, len(excluded))
	for i, k := range excluded {
		excludedAny[i] = k
	}
	event.Artifacts["excluded_ticket_keys"] = excludedAny
}
