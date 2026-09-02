package pipeline_test

// status_history_test.go exercises pipeline.HasEnabledStatusHistorySource,
// the config-only seam cmd/unjira uses to warn ONCE at startup when nothing
// enabled can ever supply the reconciler's staleness guard with collected
// status history — task #174's second half, distinct from
// reconciler.FindUnguardedTransitions' per-narrative reporting
// (internal/pipeline/reconcile_test.go). This must be answerable from
// config and the collector registry alone, before any narrative exists, or
// the warning could not run at startup at all.

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/pipeline"
)

// statusHistoryCollector is a fakeCollector (collect_test.go) that also
// declares StatusHistorySource, mirroring how the real jira collector
// implements it. Embeds *fakeCollector rather than fakeCollector: its
// Name/Collect methods have pointer receivers.
type statusHistoryCollector struct {
	*fakeCollector
}

func (statusHistoryCollector) SuppliesStatusHistory() {}

var _ pipeline.StatusHistorySource = statusHistoryCollector{}

// TestHasEnabledStatusHistorySource_TrueWhenAnEnabledCollectorSuppliesIt is
// the case that must silence the warning: a collector implementing
// StatusHistorySource, enabled in config, is present in the registry.
func TestHasEnabledStatusHistorySource_TrueWhenAnEnabledCollectorSuppliesIt(t *testing.T) {
	cfg := config.Config{Collectors: map[string]map[string]any{"jira": {"enabled": true}}}
	registry := map[string]func() pipeline.Collector{
		"jira": func() pipeline.Collector {
			return statusHistoryCollector{&fakeCollector{name: "jira"}}
		},
	}

	assert.True(t, pipeline.HasEnabledStatusHistorySource(cfg, registry))
}

// TestHasEnabledStatusHistorySource_FalseWhenNoEnabledCollectorQualifies is
// the gap task #174 names: claude_code (no status concept at all) enabled,
// jira not configured — nothing can ever supply status history, and this
// must be knowable without touching a store or running a pass. If this
// regressed to true, cmd/unjira's startup warning would never fire for a
// deployment with a real config gap.
func TestHasEnabledStatusHistorySource_FalseWhenNoEnabledCollectorQualifies(t *testing.T) {
	cfg := config.Config{Collectors: map[string]map[string]any{"claude_code": {"enabled": true}}}
	registry := map[string]func() pipeline.Collector{
		"claude_code": func() pipeline.Collector {
			return &fakeCollector{name: "claude_code"}
		},
	}

	assert.False(t, pipeline.HasEnabledStatusHistorySource(cfg, registry))
}

// TestHasEnabledStatusHistorySource_FalseWhenTheQualifyingCollectorIsDisabled:
// a collector merely being registered is not enough — it must be enabled in
// THIS config, matching cfg.EnabledCollectors' own semantics everywhere else
// in the pipeline.
func TestHasEnabledStatusHistorySource_FalseWhenTheQualifyingCollectorIsDisabled(t *testing.T) {
	cfg := config.Config{Collectors: map[string]map[string]any{"jira": {"enabled": false}}}
	registry := map[string]func() pipeline.Collector{
		"jira": func() pipeline.Collector {
			return statusHistoryCollector{&fakeCollector{name: "jira"}}
		},
	}

	assert.False(t, pipeline.HasEnabledStatusHistorySource(cfg, registry))
}

// TestHasEnabledStatusHistorySource_IgnoresAnEnabledButUnregisteredCollector
// mirrors RunCollect's own "-1" sentinel case: a name enabled in config with
// no matching registry entry must not panic or be treated as qualifying.
func TestHasEnabledStatusHistorySource_IgnoresAnEnabledButUnregisteredCollector(t *testing.T) {
	cfg := config.Config{Collectors: map[string]map[string]any{"ghost": {"enabled": true}}}

	assert.False(t, pipeline.HasEnabledStatusHistorySource(cfg, map[string]func() pipeline.Collector{}))
}

// TestHasEnabledStatusHistorySource_ADifferentCollectorTypeDoesNotQualify:
// implementing pipeline.Collector alone is not the signal — only
// StatusHistorySource is. A plain fakeCollector (claude_code's shape) must
// not be mistaken for one just because it emits events at all.
func TestHasEnabledStatusHistorySource_ADifferentCollectorTypeDoesNotQualify(t *testing.T) {
	cfg := config.Config{Collectors: map[string]map[string]any{"fake": {"enabled": true}}}
	registry := map[string]func() pipeline.Collector{
		"fake": func() pipeline.Collector {
			return &fakeCollector{name: "fake", events: []events.Event{makeEvent("1")}}
		},
	}

	assert.False(t, pipeline.HasEnabledStatusHistorySource(cfg, registry))
}
