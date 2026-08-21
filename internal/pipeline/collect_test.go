package pipeline_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/credentials"
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

func (f *fakeCollector) Collect(_ pipeline.CollectContext, visit func(events.Event)) error {
	if f.err != nil {
		return f.err
	}
	for _, e := range f.events {
		visit(e)
	}
	return nil
}

// contextCapturingCollector records the CollectContext it was handed, so a
// test can assert what RunCollect actually threads through.
type contextCapturingCollector struct {
	seen *pipeline.CollectContext
}

func (c *contextCapturingCollector) Name() string { return "capture" }

func (c *contextCapturingCollector) Collect(cc pipeline.CollectContext, _ func(events.Event)) error {
	*c.seen = cc

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

	results, err := pipeline.RunCollect(cfg, s, registry, nil, credentials.Set{})

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

	results, err := pipeline.RunCollect(cfg, s, registry, nil, credentials.Set{})

	require.NoError(t, err)
	assert.Empty(t, results)
}

func TestRunCollect_ReportsUnregisteredCollectorAsNegativeOne(t *testing.T) {
	s := openTestStore(t)
	cfg := config.Config{Collectors: map[string]map[string]any{
		"ghost": {"enabled": true},
	}}

	results, err := pipeline.RunCollect(cfg, s, map[string]func() pipeline.Collector{}, nil, credentials.Set{})

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

	results, err := pipeline.RunCollect(cfg, s, registry, nil, credentials.Set{})

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

	_, err = pipeline.RunCollect(cfg, s, registry, compiled, credentials.Set{})
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

	_, err = pipeline.RunCollect(cfg, s, registry, compiled, credentials.Set{})
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

	_, err := pipeline.RunCollect(cfg, s, registry, nil, credentials.Set{})
	require.NoError(t, err)

	rows, err := s.EventsOn(e.OccurredAt)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.NotContains(t, rows[0].Artifacts, "excluded_ticket_keys")
}

func TestRunCollect_PassesConfigAndCredentialsToCollector(t *testing.T) {
	s := openTestStore(t)

	var gotContext pipeline.CollectContext
	capturing := &contextCapturingCollector{seen: &gotContext}

	cfg := config.Config{
		Collectors: map[string]map[string]any{
			"capture": {"enabled": true, "some_option": "value"},
		},
		Jira: []config.JiraConnection{
			{Name: "corp", Site: "https://corp.atlassian.net", ProjectKeys: []string{"PROJ"}},
		},
	}
	creds := credentials.NewSet(map[string]credentials.Credential{
		"corp": {Email: "me@corp.example", Token: "t1"},
	})

	_, err := pipeline.RunCollect(cfg, s, map[string]func() pipeline.Collector{
		"capture": func() pipeline.Collector { return capturing },
	}, nil, creds)

	require.NoError(t, err)
	assert.Same(t, s, gotContext.Store, "the collector gets the same store RunCollect was given")
	assert.Equal(t, "value", gotContext.Options["some_option"],
		"the collector's own options block still reaches it")
	require.Len(t, gotContext.Config.Jira, 1)
	assert.Equal(t, "corp", gotContext.Config.Jira[0].Name,
		"a remote collector needs connection config, which is why this seam exists")
	gotCred, ok := gotContext.Credentials.For("corp")
	require.True(t, ok, "credentials must reach the collector or no remote source can authenticate")
	assert.Equal(t, "t1", gotCred.Token)
}
