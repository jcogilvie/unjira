package jira_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/clients/jira"
)

func worklogInput() jira.WorklogInput {
	return jira.WorklogInput{
		Started:          time.Date(2026, 10, 14, 9, 30, 0, 0, time.UTC),
		TimeSpentSeconds: 600,
		Comment:          "unjira: 10m of agent time",
		Properties: []jira.EntityProperty{
			{Key: "unjira.attribution", Value: json.RawMessage(`{"version":1}`)},
		},
	}
}

func TestWorklogWrites_SendNotifyAndEstimateFlags(t *testing.T) {
	var records []string

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		records = append(records, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		switch r.Method {
		case http.MethodPost:
			writeJSON(t, w, http.StatusCreated, map[string]any{"id": "10001"})
		case http.MethodPut:
			writeJSON(t, w, http.StatusOK, map[string]any{"id": "10001"})
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})

	_, err := client.AddWorklog("PROJ-1", worklogInput())
	require.NoError(t, err)
	require.NoError(t, client.UpdateWorklog("PROJ-1", "10001", worklogInput()))
	require.NoError(t, client.DeleteWorklog("PROJ-1", "10001"))

	require.Len(t, records, 4)
	wantPaths := []string{
		"POST /rest/api/2/issue/PROJ-1/worklog",
		"PUT /rest/api/2/issue/PROJ-1/worklog/10001",
		"PUT /rest/api/2/issue/PROJ-1/worklog/10001/properties/unjira.attribution",
		"DELETE /rest/api/2/issue/PROJ-1/worklog/10001",
	}
	for i, rec := range records {
		path, rawQuery, ok := strings.Cut(rec, "?")
		require.True(t, ok, rec)
		assert.Equal(t, wantPaths[i], path)
		q, err := url.ParseQuery(rawQuery)
		require.NoError(t, err)
		assert.Equal(t, url.Values{"notifyUsers": {"false"}, "adjustEstimate": {"leave"}}, q, rec)
	}
}

func TestAddWorklog_PostsBodyWithProperties(t *testing.T) {
	var body map[string]any

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		writeJSON(t, w, http.StatusCreated, map[string]any{"id": "10042"})
	})

	id, err := client.AddWorklog("PROJ-1", worklogInput())

	require.NoError(t, err)
	assert.Equal(t, "10042", id)
	assert.Equal(t, map[string]any{
		"started":          "2026-10-14T09:30:00.000+0000",
		"timeSpentSeconds": float64(600),
		"comment":          "unjira: 10m of agent time",
		"properties": []any{
			map[string]any{"key": "unjira.attribution", "value": map[string]any{"version": float64(1)}},
		},
	}, body)
}

// The worklog PUT ignores inline properties (measured on DEVSBX), so an update
// must set each property through its own endpoint, with the raw value as body.
func TestUpdateWorklog_SetsPropertiesThroughPropertyEndpoint(t *testing.T) {
	bodies := map[string]string{}

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPut, r.Method)
		b, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		bodies[r.URL.Path] = string(b)
		w.WriteHeader(http.StatusOK)
	})

	require.NoError(t, client.UpdateWorklog("PROJ-1", "10001", worklogInput()))

	require.Len(t, bodies, 2)
	assert.Contains(t, bodies, "/rest/api/2/issue/PROJ-1/worklog/10001")
	assert.JSONEq(t, `{"version":1}`, bodies["/rest/api/2/issue/PROJ-1/worklog/10001/properties/unjira.attribution"])
}

func TestListWorklogs_WalksEveryPage(t *testing.T) {
	var starts []string

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/rest/api/2/issue/PROJ-1/worklog", r.URL.Path)
		assert.Equal(t, "properties", r.URL.Query().Get("expand"))
		start := r.URL.Query().Get("startAt")
		starts = append(starts, start)
		n, _ := strconv.Atoi(start)
		// The server returns fewer than maxResults per page; advancing by
		// maxResults would skip the second worklog.
		page := []map[string]any{{
			"id":         strconv.Itoa(10001 + n),
			"properties": []map[string]any{{"key": "unjira.attribution", "value": map[string]any{"version": 1}}},
		}}
		if n >= 2 {
			page = nil
		}
		writeJSON(t, w, http.StatusOK, map[string]any{
			"startAt": n, "maxResults": 100, "total": 2, "worklogs": page,
		})
	})

	got, err := client.ListWorklogs("PROJ-1")

	require.NoError(t, err)
	assert.Equal(t, []string{"0", "1"}, starts)
	require.Len(t, got, 2)
	assert.Equal(t, "10001", got[0].ID)
	assert.Equal(t, "10002", got[1].ID)
	for _, wl := range got {
		require.Len(t, wl.Properties, 1)
		assert.Equal(t, "unjira.attribution", wl.Properties[0].Key)
		assert.JSONEq(t, `{"version":1}`, string(wl.Properties[0].Value))
	}
}

func TestAddWorklog_ErrorKeepsJiraStatus(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusBadRequest, map[string]any{"errorMessages": []string{"Worklog must not be null."}})
	})

	_, err := client.AddWorklog("PROJ-1", worklogInput())

	var jiraErr *jira.Error
	require.ErrorAs(t, err, &jiraErr)
	assert.Equal(t, 400, jiraErr.Status)
	assert.Contains(t, jiraErr.Message, "Worklog must not be null.")
}
