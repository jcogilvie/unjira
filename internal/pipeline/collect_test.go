package pipeline_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/pipeline"
	"github.com/jcogilvie/unjira/internal/store"
)

// fakeCollector is a minimal test double satisfying pipeline.Collector.
type fakeCollector struct {
	name   string
	events []events.Event
	err    error
}

func (f *fakeCollector) Name() string { return f.name }

func (f *fakeCollector) Collect(_ *store.Store, _ map[string]any, visit func(events.Event)) error {
	if f.err != nil {
		return f.err
	}
	for _, e := range f.events {
		visit(e)
	}
	return nil
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()

	s, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	return s
}

func makeEvent(externalID string) events.Event {
	return events.NewEvent("fake", externalID, time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC), "test event")
}

func TestRunCollect_InsertsEventsFromEnabledCollectors(t *testing.T) {
	s := openTestStore(t)
	registry := map[string]func() pipeline.Collector{
		"fake": func() pipeline.Collector {
			return &fakeCollector{name: "fake", events: []events.Event{makeEvent("1"), makeEvent("2")}}
		},
	}
	cfg := config.Config{Collectors: map[string]map[string]any{
		"fake": {"enabled": true},
	}}

	results, err := pipeline.RunCollect(cfg, s, registry, nil)

	require.NoError(t, err)
	assert.Equal(t, map[string]int{"fake": 2}, results)
}

func TestRunCollect_SkipsDisabledCollectors(t *testing.T) {
	s := openTestStore(t)
	registry := map[string]func() pipeline.Collector{
		"fake": func() pipeline.Collector {
			return &fakeCollector{name: "fake", events: []events.Event{makeEvent("1")}}
		},
	}
	cfg := config.Config{Collectors: map[string]map[string]any{
		"fake": {"enabled": false},
	}}

	results, err := pipeline.RunCollect(cfg, s, registry, nil)

	require.NoError(t, err)
	assert.Empty(t, results)
}

func TestRunCollect_ReportsUnregisteredCollectorAsNegativeOne(t *testing.T) {
	s := openTestStore(t)
	cfg := config.Config{Collectors: map[string]map[string]any{
		"ghost": {"enabled": true},
	}}

	results, err := pipeline.RunCollect(cfg, s, map[string]func() pipeline.Collector{}, nil)

	require.NoError(t, err)
	assert.Equal(t, map[string]int{"ghost": -1}, results)
}

func TestRunCollect_DedupesOnInsert(t *testing.T) {
	s := openTestStore(t)
	registry := map[string]func() pipeline.Collector{
		"fake": func() pipeline.Collector {
			return &fakeCollector{name: "fake", events: []events.Event{makeEvent("1"), makeEvent("1")}}
		},
	}
	cfg := config.Config{Collectors: map[string]map[string]any{
		"fake": {"enabled": true},
	}}

	results, err := pipeline.RunCollect(cfg, s, registry, nil)

	require.NoError(t, err)
	assert.Equal(t, map[string]int{"fake": 1}, results) // second insert is a dedupe no-op
}

func TestRunCollect_ExcludedTicketKeysPersistedWithoutMutatingTicketKeys(t *testing.T) {
	s := openTestStore(t)
	e := makeEvent("1")
	e.Artifacts["ticket_keys"] = []any{"PROJ-42", "PROJ-0"}
	registry := map[string]func() pipeline.Collector{
		"fake": func() pipeline.Collector {
			return &fakeCollector{name: "fake", events: []events.Event{e}}
		},
	}
	cfg := config.Config{Collectors: map[string]map[string]any{
		"fake": {"enabled": true},
	}}
	compiled, err := events.CompileLinkExclusionPatterns([]string{"-0$"})
	require.NoError(t, err)

	_, err = pipeline.RunCollect(cfg, s, registry, compiled)
	require.NoError(t, err)

	rows, err := s.EventsOn(e.OccurredAt)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, []any{"PROJ-42", "PROJ-0"}, rows[0].Artifacts["ticket_keys"])
	assert.Equal(t, []any{"PROJ-0"}, rows[0].Artifacts["excluded_ticket_keys"])
}

func TestRunCollect_NoExclusionMatchLeavesArtifactAbsent(t *testing.T) {
	s := openTestStore(t)
	e := makeEvent("1")
	e.Artifacts["ticket_keys"] = []any{"PROJ-42"}
	registry := map[string]func() pipeline.Collector{
		"fake": func() pipeline.Collector {
			return &fakeCollector{name: "fake", events: []events.Event{e}}
		},
	}
	cfg := config.Config{Collectors: map[string]map[string]any{
		"fake": {"enabled": true},
	}}
	compiled, err := events.CompileLinkExclusionPatterns([]string{"-0$"})
	require.NoError(t, err)

	_, err = pipeline.RunCollect(cfg, s, registry, compiled)
	require.NoError(t, err)

	rows, err := s.EventsOn(e.OccurredAt)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.NotContains(t, rows[0].Artifacts, "excluded_ticket_keys")
}

func TestRunCollect_NoConfiguredPatternsNeverAddsExclusionArtifact(t *testing.T) {
	s := openTestStore(t)
	e := makeEvent("1")
	e.Artifacts["ticket_keys"] = []any{"PROJ-0"}
	registry := map[string]func() pipeline.Collector{
		"fake": func() pipeline.Collector {
			return &fakeCollector{name: "fake", events: []events.Event{e}}
		},
	}
	cfg := config.Config{Collectors: map[string]map[string]any{
		"fake": {"enabled": true},
	}}

	_, err := pipeline.RunCollect(cfg, s, registry, nil)
	require.NoError(t, err)

	rows, err := s.EventsOn(e.OccurredAt)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.NotContains(t, rows[0].Artifacts, "excluded_ticket_keys")
}
