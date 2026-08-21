package jira_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// fakeJira serves canned Jira responses. Requests are recorded so tests can
// assert on the JQL actually sent, which is where the watermark and the project
// scope become observable.
type fakeJira struct {
	mu         sync.Mutex
	searchJQLs []string
	issues     []map[string]any
	changelogs map[string][]map[string]any
	comments   map[string][]map[string]any
	// failChangelogFor makes GetChangelog return 500 for one issue key.
	failChangelogFor string
	accountID        string
}

func (f *fakeJira) start(t *testing.T) string {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/myself"):
			f.writeJSON(t, w, map[string]any{"accountId": f.accountID, "displayName": "unjira"})

		case strings.Contains(path, "/search/jql"):
			f.searchJQLs = append(f.searchJQLs, r.URL.Query().Get("jql"))
			f.writeJSON(t, w, map[string]any{"issues": f.issues})

		case strings.HasSuffix(path, "/changelog"):
			key := issueKeyFromPath(path)
			if key == f.failChangelogFor {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"errorMessages":["boom"]}`))

				return
			}
			f.writeJSON(t, w, map[string]any{
				"values": f.changelogs[key], "isLast": true, "maxResults": 100,
			})

		case strings.HasSuffix(path, "/comment"):
			key := issueKeyFromPath(path)
			list := f.comments[key]
			f.writeJSON(t, w, map[string]any{
				"comments": list, "startAt": 0, "maxResults": 100, "total": len(list),
			})

		default:
			t.Errorf("unexpected request path %q", path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	return srv.URL
}

func (f *fakeJira) writeJSON(t *testing.T, w http.ResponseWriter, body map[string]any) {
	t.Helper()

	w.Header().Set("Content-Type", "application/json")
	// assert, not require: this runs inside an httptest handler goroutine,
	// where require's FailNow (runtime.Goexit) would not fail the test itself.
	// internal/clients/jira/jira_test.go documents the same constraint.
	assert.NoError(t, json.NewEncoder(w).Encode(body))
}

// recordedJQLs returns the searches made so far, under the mutex. Tests must
// use this rather than touching f.searchJQLs: it is written from the httptest
// handler goroutine, so an unguarded read is a data race that -race will flag.
func (f *fakeJira) recordedJQLs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.searchJQLs...)
}

// issueKeyFromPath pulls PROJ-42 out of .../issue/PROJ-42/changelog.
func issueKeyFromPath(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i, p := range parts {
		if p == "issue" && i+1 < len(parts) {
			return parts[i+1]
		}
	}

	return ""
}

func searchIssue(key, project, updated string) map[string]any {
	return map[string]any{
		"key": key,
		"fields": map[string]any{
			"project": map[string]any{"key": project},
			"updated": updated,
		},
	}
}

func changelogEntry(id, created, field, from, to string) map[string]any {
	return map[string]any{
		"id":      id,
		"created": created,
		"author":  map[string]any{"accountId": "acct-alice", "displayName": "Alice"},
		"items": []any{
			map[string]any{"field": field, "fromString": from, "toString": to},
		},
	}
}

// testStore opens a real temp-file SQLite store. Cursor behaviour is worth
// testing against the real DB rather than a fake.
func testStore(t *testing.T) *store.Store {
	t.Helper()

	s, err := store.Open(filepath.Join(t.TempDir(), "unjira.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	return s
}

func testContext(t *testing.T, site string, conn config.JiraConnection) pipeline.CollectContext {
	t.Helper()

	conn.Site = site

	return pipeline.CollectContext{
		Store:  testStore(t),
		Config: config.Config{Jira: []config.JiraConnection{conn}},
		Credentials: credentials.NewSet(map[string]credentials.Credential{
			conn.Name: {Email: "dev@example.com", Token: "token"},
		}),
		Options: map[string]any{},
	}
}

func collectAll(t *testing.T, cc pipeline.CollectContext) ([]events.Event, error) {
	t.Helper()

	var got []events.Event
	err := collectorjira.New().Collect(cc, func(e events.Event) { got = append(got, e) })

	return got, err
}

func TestCollect_EmitsChangelogAndCommentEvents(t *testing.T) {
	fake := &fakeJira{
		accountID: "acct-unjira",
		issues:    []map[string]any{searchIssue("PROJ-42", "PROJ", "2026-08-20T16:00:00.000+0000")},
		changelogs: map[string][]map[string]any{
			"PROJ-42": {
				changelogEntry("10001", "2026-08-20T14:00:00.000+0000", "status", "To Do", "In Progress"),
				changelogEntry("10002", "2026-08-20T15:00:00.000+0000", "assignee", "", "Bob"),
			},
		},
		comments: map[string][]map[string]any{
			"PROJ-42": {{
				"id": "9001", "created": "2026-08-20T15:30:00.000+0000", "body": "looking at it",
				"author": map[string]any{"accountId": "acct-alice", "displayName": "Alice"},
			}},
		},
	}
	cc := testContext(t, fake.start(t), config.JiraConnection{
		Name: "corp", ProjectKeys: []string{"PROJ"},
		Queries: []config.JiraQuery{{Name: "mine", JQL: "assignee = currentUser()"}},
	})

	got, err := collectAll(t, cc)

	require.NoError(t, err)
	ids := make([]string, 0, len(got))
	for _, e := range got {
		ids = append(ids, e.ExternalID)
	}
	assert.ElementsMatch(t, []string{"PROJ-42:status:10001", "PROJ-42:comment:9001"}, ids,
		"the assignee change is not actionable in phase 1 and must not be emitted")
}

func TestCollect_ScopesJQLToProjectKeysAndAppliesNoWatermarkOnFirstPass(t *testing.T) {
	fake := &fakeJira{accountID: "acct-unjira"}
	cc := testContext(t, fake.start(t), config.JiraConnection{
		Name: "corp", ProjectKeys: []string{"PROJ", "OPS"},
		Queries: []config.JiraQuery{{Name: "mine", JQL: "assignee = currentUser()"}},
	})

	_, err := collectAll(t, cc)
	require.NoError(t, err)

	jqls := fake.recordedJQLs()
	require.Len(t, jqls, 1)
	assert.Contains(t, jqls[0], `project IN ("PROJ", "OPS")`,
		"collection must never exceed what the connection can write to")
	assert.NotContains(t, jqls[0], "updated >=", "the first pass has no watermark to apply")
}

func TestCollect_SecondPassAppliesStoredWatermark(t *testing.T) {
	fake := &fakeJira{
		accountID: "acct-unjira",
		issues:    []map[string]any{searchIssue("PROJ-42", "PROJ", "2026-08-20T16:00:00.000+0000")},
	}
	cc := testContext(t, fake.start(t), config.JiraConnection{
		Name: "corp", ProjectKeys: []string{"PROJ"},
		Queries: []config.JiraQuery{{Name: "mine", JQL: "assignee = currentUser()"}},
	})

	_, err := collectAll(t, cc)
	require.NoError(t, err)
	_, err = collectAll(t, cc)
	require.NoError(t, err)

	jqls := fake.recordedJQLs()
	require.Len(t, jqls, 2)
	assert.NotContains(t, jqls[0], "updated >=")
	assert.Contains(t, jqls[1], "updated >=",
		"the second pass must bound the search by what the first already covered")
}

func TestCollect_EmitsChangelogEntriesOlderThanTheWatermark(t *testing.T) {
	// The behaviour most likely to be broken and least likely to be noticed.
	// An issue reassigned to you today has updated=now, so it matches the
	// watermark — but its changelog runs back months, and every one of those
	// entries is new to us. The watermark selects *issues*, never entries.
	fake := &fakeJira{
		accountID: "acct-unjira",
		issues:    []map[string]any{searchIssue("PROJ-42", "PROJ", "2026-08-20T16:00:00.000+0000")},
		changelogs: map[string][]map[string]any{
			"PROJ-42": {
				changelogEntry("10001", "2026-01-04T09:00:00.000+0000", "status", "To Do", "In Progress"),
				changelogEntry("10002", "2026-08-20T15:00:00.000+0000", "status", "In Progress", "Done"),
			},
		},
	}
	cc := testContext(t, fake.start(t), config.JiraConnection{
		Name: "corp", ProjectKeys: []string{"PROJ"},
		Queries: []config.JiraQuery{{Name: "mine", JQL: "assignee = currentUser()"}},
	})

	// Seed a watermark well after the January entry by running a first pass.
	_, err := collectAll(t, cc)
	require.NoError(t, err)

	got, err := collectAll(t, cc)

	require.NoError(t, err)
	var sawJanuary bool
	for _, e := range got {
		if e.ExternalID == "PROJ-42:status:10001" {
			sawJanuary = true
		}
	}
	assert.True(t, sawJanuary,
		"entries predating the watermark are new to us and must be emitted; dedup makes it free")
}

func TestCollect_HittingTheIssueLimitIsLogged(t *testing.T) {
	// The spec requires this be "logged, never silent": a silent cap presents
	// as a clean pass while ignoring work. An untested log line is one refactor
	// away from being deleted, so capture the log output and assert on it.
	var logged bytes.Buffer
	log.SetOutput(&logged)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(log.LstdFlags)
	})

	fake := &fakeJira{
		accountID: "acct-unjira",
		issues: []map[string]any{
			searchIssue("PROJ-1", "PROJ", "2026-08-20T16:00:00.000+0000"),
			searchIssue("PROJ-2", "PROJ", "2026-08-20T16:00:00.000+0000"),
		},
	}
	cc := testContext(t, fake.start(t), config.JiraConnection{
		Name: "corp", ProjectKeys: []string{"PROJ"}, MaxIssuesPerQuery: 2,
		Queries: []config.JiraQuery{{Name: "mine", JQL: "assignee = currentUser()"}},
	})

	_, err := collectAll(t, cc)

	require.NoError(t, err)
	assert.Contains(t, logged.String(), "corp/mine",
		"the message must name the query that was truncated")
	assert.Contains(t, logged.String(), "limit")
}

func TestCollect_OneQueryFailingDoesNotStopTheOthers(t *testing.T) {
	fake := &fakeJira{
		accountID:        "acct-unjira",
		issues:           []map[string]any{searchIssue("PROJ-42", "PROJ", "2026-08-20T16:00:00.000+0000")},
		failChangelogFor: "PROJ-42",
	}
	cc := testContext(t, fake.start(t), config.JiraConnection{
		Name: "corp", ProjectKeys: []string{"PROJ"},
		Queries: []config.JiraQuery{
			{Name: "broken", JQL: "assignee = currentUser()"},
			{Name: "alsobroken", JQL: "watcher = currentUser()"},
		},
	})

	_, err := collectAll(t, cc)

	require.Error(t, err, "the caller must see that queries failed")
	assert.Contains(t, err.Error(), "broken")
	assert.Len(t, fake.recordedJQLs(), 2,
		"the second query must still have been attempted after the first failed")
}

func TestCollect_FailedQueryDoesNotAdvanceItsWatermark(t *testing.T) {
	fake := &fakeJira{
		accountID:        "acct-unjira",
		issues:           []map[string]any{searchIssue("PROJ-42", "PROJ", "2026-08-20T16:00:00.000+0000")},
		failChangelogFor: "PROJ-42",
	}
	cc := testContext(t, fake.start(t), config.JiraConnection{
		Name: "corp", ProjectKeys: []string{"PROJ"},
		Queries: []config.JiraQuery{{Name: "mine", JQL: "assignee = currentUser()"}},
	})

	_, err := collectAll(t, cc)
	require.Error(t, err)

	position, cursorErr := cc.Store.GetCursor("jira", "corp/mine")
	require.NoError(t, cursorErr)
	assert.Empty(t, position,
		"advancing past an issue we could not read would skip it permanently")
}

func TestCollect_WriteOnlyConnectionIsSkipped(t *testing.T) {
	fake := &fakeJira{accountID: "acct-unjira"}
	cc := testContext(t, fake.start(t), config.JiraConnection{
		Name: "corp", ProjectKeys: []string{"PROJ"},
		// No Queries: this connection routes project keys for writes only.
	})

	got, err := collectAll(t, cc)

	require.NoError(t, err)
	assert.Empty(t, got)
	assert.Empty(t, fake.recordedJQLs(), "a connection with no queries must make no requests")
}

func TestCollect_EmptyProjectKeysErrorsNamingTheConnection(t *testing.T) {
	fake := &fakeJira{accountID: "acct-unjira"}
	cc := testContext(t, fake.start(t), config.JiraConnection{
		Name:    "corp",
		Queries: []config.JiraQuery{{Name: "mine", JQL: "assignee = currentUser()"}},
	})

	_, err := collectAll(t, cc)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "corp")
	assert.Contains(t, err.Error(), "project_keys")
	assert.Empty(t, fake.recordedJQLs(), "an unscopeable query must not be sent")
}

func TestCollect_MissingCredentialErrorsNamingTheConnection(t *testing.T) {
	fake := &fakeJira{accountID: "acct-unjira"}
	cc := testContext(t, fake.start(t), config.JiraConnection{
		Name: "corp", ProjectKeys: []string{"PROJ"},
		Queries: []config.JiraQuery{{Name: "mine", JQL: "assignee = currentUser()"}},
	})
	cc.Credentials = credentials.Set{} // zero value: nothing configured

	_, err := collectAll(t, cc)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "corp")
}

func TestWatermarkClause_RendersInTheAccountTimezone(t *testing.T) {
	// The bug this guards, found only by a live run: JQL date literals are
	// interpreted in the account's configured timezone, not UTC. Sending UTC
	// pushes the bound hours into the future, so every incremental pass silently
	// under-collects — and Jira answers 200-with-zero-results rather than
	// erroring, so nothing surfaces.
	//
	// The expectation is derived from the zone database rather than hardcoded:
	// my first draft asserted 12:02 (UTC-7), which was wrong — Indianapolis is
	// EDT (UTC-4) in August, and -0700 was the *server's* offset, not the
	// account's. Deriving it keeps the test honest across DST and tzdata updates.
	watermark := time.Date(2026, 8, 21, 19, 2, 24, 0, time.UTC)

	const zone = "America/Indiana/Indianapolis"

	loc, err := time.LoadLocation(zone)
	require.NoError(t, err)

	wantLocal := watermark.In(loc).Truncate(time.Minute).Format("2006-01-02 15:04")
	wantUTC := watermark.UTC().Truncate(time.Minute).Format("2006-01-02 15:04")
	require.NotEqual(t, wantLocal, wantUTC,
		"this test only discriminates while the zone differs from UTC at this instant")

	got := collectorjira.WatermarkClauseForTest(watermark, zone, "corp")

	assert.Equal(t, `updated >= "`+wantLocal+`"`, got,
		"the bound must be the account-local wall clock, not the UTC one")
	assert.NotContains(t, got, "19:02", "a UTC bound would skip issues updated in the offset window")
}

func TestWatermarkClause_FlooringNeverRoundsUp(t *testing.T) {
	// JQL has minute precision. Flooring re-examines a little (dedup makes that
	// free); rounding up would step over an issue permanently.
	watermark := time.Date(2026, 8, 21, 12, 2, 59, 999_000_000, time.UTC)

	got := collectorjira.WatermarkClauseForTest(watermark, "UTC", "corp")

	assert.Equal(t, `updated >= "2026-08-21 12:02"`, got,
		"12:02:59.999 must floor to 12:02, never advance to 12:03")
}

func TestWatermarkClause_UnknownZoneWidensRatherThanSkips(t *testing.T) {
	tests := []struct {
		name string
		zone string
	}{
		{name: "absent", zone: ""},
		{name: "unloadable", zone: "Not/AZone"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Suppress the expected operator warning.
			log.SetOutput(io.Discard)
			t.Cleanup(func() { log.SetOutput(os.Stderr) })

			watermark := time.Date(2026, 8, 21, 19, 2, 0, 0, time.UTC)
			want := watermark.
				Add(-collectorjira.WatermarkZoneFallbackMarginForTest).
				UTC().Truncate(time.Minute).Format("2006-01-02 15:04")

			got := collectorjira.WatermarkClauseForTest(watermark, tt.zone, "corp")

			assert.Equal(t, `updated >= "`+want+`"`, got,
				"an unknown zone must widen the window; too-early re-examines, too-late skips forever")
		})
	}
}

func TestWatermarkClause_UnknownZoneIsLogged(t *testing.T) {
	var logged bytes.Buffer
	log.SetOutput(&logged)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(log.LstdFlags)
	})

	collectorjira.WatermarkClauseForTest(time.Now(), "", "corp")

	assert.Contains(t, logged.String(), "corp", "the operator must be able to see which connection")
	assert.Contains(t, logged.String(), "timezone")
}
