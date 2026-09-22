// Package github_test exercises the facade against a real HTTP server
// (httptest), mirroring clients/jira's own testing idiom: one pattern for
// every remote system, never a second one for a subprocess.
package github_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/clients/github"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) *github.Client {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	client, err := github.New(server.URL, "test-token")
	require.NoError(t, err)

	return client
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, body any) {
	t.Helper()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	assert.NoError(t, json.NewEncoder(w).Encode(body))
}

func TestNew_RequiresBaseURL(t *testing.T) {
	_, err := github.New("", "token")

	require.Error(t, err)
}

func TestListPullRequests_RequestsExplicitSortAndDirection(t *testing.T) {
	var gotQuery string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		writeJSON(t, w, http.StatusOK, []map[string]any{})
	})

	_, err := client.ListPullRequests(mustRef(t, "o/r"), time.Time{})

	require.NoError(t, err)
	assert.Contains(t, gotQuery, "sort=updated")
	assert.Contains(t, gotQuery, "direction=desc")
	assert.Contains(t, gotQuery, "state=all")
}

func TestListPullRequests_SendsBearerToken(t *testing.T) {
	var gotAuth string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		writeJSON(t, w, http.StatusOK, []map[string]any{})
	})

	_, err := client.ListPullRequests(mustRef(t, "o/r"), time.Time{})

	require.NoError(t, err)
	assert.Equal(t, "Bearer test-token", gotAuth)
}

func TestListPullRequests_StopsAtTheFirstPROlderThanSince(t *testing.T) {
	since := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, []map[string]any{
			{"number": 3, "updated_at": "2026-09-05T00:00:00Z"},
			{"number": 2, "updated_at": "2026-09-03T00:00:00Z"},
			// Older than since: the scan must stop here and not include it.
			{"number": 1, "updated_at": "2026-08-01T00:00:00Z"},
		})
	})

	prs, err := client.ListPullRequests(mustRef(t, "o/r"), since)

	require.NoError(t, err)
	require.Len(t, prs, 2)
	assert.Equal(t, 3, prs[0].Number)
	assert.Equal(t, 2, prs[1].Number)
}

func TestListPullRequests_PagesUntilAShortPage(t *testing.T) {
	var pages []string
	callCount := 0
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		pages = append(pages, r.URL.Query().Get("page"))
		callCount++
		if callCount == 1 {
			// A full page (== perPage) means "there may be more" and must
			// trigger a second request.
			full := make([]map[string]any, 100)
			for i := range full {
				full[i] = map[string]any{"number": 200 - i, "updated_at": "2026-09-10T00:00:00Z"}
			}
			writeJSON(t, w, http.StatusOK, full)
			return
		}
		writeJSON(t, w, http.StatusOK, []map[string]any{
			{"number": 1, "updated_at": "2026-09-01T00:00:00Z"},
		})
	})

	prs, err := client.ListPullRequests(mustRef(t, "o/r"), time.Time{})

	require.NoError(t, err)
	assert.Len(t, prs, 101)
	require.Len(t, pages, 2)
	assert.Equal(t, "1", pages[0])
	assert.Equal(t, "2", pages[1])
}

func TestListPullRequests_DecodesPRFields(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, []map[string]any{
			{
				"number":     42,
				"title":      "Fix the thing",
				"body":       "Closes PROJ-1",
				"state":      "closed",
				"html_url":   "https://github.com/o/r/pull/42",
				"created_at": "2026-09-01T10:00:00Z",
				"updated_at": "2026-09-02T10:00:00Z",
				"merged_at":  "2026-09-02T10:00:00Z",
				"closed_at":  "2026-09-02T10:00:00Z",
				"user":       map[string]any{"login": "alice"},
				"head":       map[string]any{"ref": "feature/PROJ-1"},
			},
		})
	})

	prs, err := client.ListPullRequests(mustRef(t, "o/r"), time.Time{})

	require.NoError(t, err)
	require.Len(t, prs, 1)
	pr := prs[0]
	assert.Equal(t, 42, pr.Number)
	assert.Equal(t, "Fix the thing", pr.Title)
	assert.Equal(t, "Closes PROJ-1", pr.Body)
	assert.True(t, pr.IsClosed())
	assert.Equal(t, "https://github.com/o/r/pull/42", pr.HTMLURL)
	assert.Equal(t, "alice", pr.User.Login)
	assert.Equal(t, "feature/PROJ-1", pr.Head.Ref)
	require.NotNil(t, pr.MergedAt)
	require.NotNil(t, pr.ClosedAt)
}

func TestListPullRequests_OpenPRHasNilMergedAndClosed(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, []map[string]any{
			{
				"number": 1, "state": "open",
				"created_at": "2026-09-01T10:00:00Z", "updated_at": "2026-09-01T10:00:00Z",
				"merged_at": nil, "closed_at": nil,
			},
		})
	})

	prs, err := client.ListPullRequests(mustRef(t, "o/r"), time.Time{})

	require.NoError(t, err)
	require.Len(t, prs, 1)
	assert.False(t, prs[0].IsClosed())
	assert.Nil(t, prs[0].MergedAt)
	assert.Nil(t, prs[0].ClosedAt)
}

func TestListPullRequests_NonOKStatusTranslatesToError(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusUnauthorized, map[string]any{"message": "Bad credentials"})
	})

	_, err := client.ListPullRequests(mustRef(t, "o/r"), time.Time{})

	require.Error(t, err)
	var ghErr *github.Error
	require.ErrorAs(t, err, &ghErr)
	assert.Equal(t, 401, ghErr.Status)
	assert.Contains(t, ghErr.Message, "Bad credentials")
}

func TestListIssueEvents_DecodesTimelineShape(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, []map[string]any{
			{"id": 31498552227, "event": "closed", "created_at": "2026-09-21T01:13:11Z"},
			{"id": 31498554963, "event": "reopened", "created_at": "2026-09-21T01:13:16Z"},
		})
	})

	events, err := client.ListIssueEvents(mustRef(t, "o/r"), 7535)

	require.NoError(t, err)
	require.Len(t, events, 2)
	assert.Equal(t, int64(31498552227), events[0].ID)
	assert.Equal(t, "closed", events[0].Event)
	assert.Equal(t, int64(31498554963), events[1].ID)
	assert.Equal(t, "reopened", events[1].Event)
}

func TestListIssueEvents_PagesUntilAShortPage(t *testing.T) {
	callCount := 0
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		callCount++
		if callCount == 1 {
			full := make([]map[string]any, 100)
			for i := range full {
				full[i] = map[string]any{"id": i, "event": "labeled", "created_at": "2026-09-01T00:00:00Z"}
			}
			writeJSON(t, w, http.StatusOK, full)
			return
		}
		writeJSON(t, w, http.StatusOK, []map[string]any{
			{"id": 999, "event": "merged", "created_at": "2026-09-02T00:00:00Z"},
		})
	})

	events, err := client.ListIssueEvents(mustRef(t, "o/r"), 1)

	require.NoError(t, err)
	assert.Len(t, events, 101)
	assert.Equal(t, 2, callCount)
}

func mustRef(t *testing.T, raw string) github.RepoRef {
	t.Helper()

	ref, err := github.ParseRepoRef(raw)
	require.NoError(t, err)

	return ref
}
