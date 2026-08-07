// Package pipeline runs the collect pass and renders the phase-0 digest.
package pipeline

import (
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
func RunCollect(cfg config.Config, s *store.Store, registry map[string]func() Collector) (map[string]int, error) {
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
