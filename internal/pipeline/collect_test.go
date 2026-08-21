package pipeline_test

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	collectorjira "github.com/jcogilvie/unjira/internal/collector/jira"
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

func TestRunCollect_JiraCollectorDedupesOnSecondPass(t *testing.T) {
	// The collector re-emits changelog entries older than its watermark on
	// every pass by design, relying on (source, external_id) dedup to make that
	// free. Its own unit tests call Collect directly and never reach
	// InsertEvent, so this is the only layer where that property is observable.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/myself"):
			_, _ = w.Write([]byte(`{"accountId":"acct-unjira","displayName":"unjira"}`))
		case strings.Contains(r.URL.Path, "/search/jql"):
			_, _ = w.Write([]byte(`{"issues":[{"key":"PROJ-42","fields":{` +
				`"project":{"key":"PROJ"},"updated":"2026-08-20T16:00:00.000+0000"}}]}`))
		case strings.HasSuffix(r.URL.Path, "/changelog"):
			_, _ = w.Write([]byte(`{"values":[{"id":"10001",` +
				`"created":"2026-08-20T14:00:00.000+0000",` +
				`"author":{"accountId":"acct-alice","displayName":"Alice"},` +
				`"items":[{"field":"status","fromString":"To Do","toString":"Done"}]}],` +
				`"isLast":true,"maxResults":100}`))
		case strings.HasSuffix(r.URL.Path, "/comment"):
			_, _ = w.Write([]byte(`{"comments":[],"startAt":0,"maxResults":100,"total":0}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	s, err := store.Open(filepath.Join(t.TempDir(), "unjira.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()

	cfg := config.Config{
		Collectors: map[string]map[string]any{"jira": {"enabled": true}},
		Jira: []config.JiraConnection{{
			Name: "corp", Site: srv.URL, ProjectKeys: []string{"PROJ"},
			Queries: []config.JiraQuery{{Name: "mine", JQL: "assignee = currentUser()"}},
		}},
	}
	reg := map[string]func() pipeline.Collector{
		"jira": func() pipeline.Collector { return collectorjira.New() },
	}
	creds := credentials.NewSet(map[string]credentials.Credential{
		"corp": {Email: "dev@example.com", Token: "token"},
	})

	first, err := pipeline.RunCollect(cfg, s, reg, nil, creds)
	require.NoError(t, err)
	assert.Equal(t, 1, first["jira"], "the status change must be inserted on the first pass")

	second, err := pipeline.RunCollect(cfg, s, reg, nil, creds)

	require.NoError(t, err)
	assert.Equal(t, 0, second["jira"],
		"(source, external_id) dedup makes re-running a collector safe; a nonzero count means it isn't")
}

func TestRunCollect_JiraEnabledButUnregisteredReportsSentinel(t *testing.T) {
	// Guards the wiring itself: if someone enables the jira collector in config
	// but the registry entry is missing, RunCollect reports -1 rather than
	// silently collecting nothing. This is what Step 2 below observes.
	s, err := store.Open(filepath.Join(t.TempDir(), "unjira.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()

	cfg := config.Config{Collectors: map[string]map[string]any{"jira": {"enabled": true}}}

	results, err := pipeline.RunCollect(cfg, s, map[string]func() pipeline.Collector{}, nil, credentials.NewSet(nil))

	require.NoError(t, err)
	assert.Equal(t, -1, results["jira"])
}
