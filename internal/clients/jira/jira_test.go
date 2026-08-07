// Package jira_test exercises the facade against a real HTTP server
// (httptest), rather than stubbing go-jira internals — the library's own
// wire behavior is covered by its own tests; these tests cover the logic
// that is unjira's own: pagination loops, shape extraction, error
// translation, payload construction.
package jira_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/clients/jira"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) *jira.Client {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	client, err := jira.New(server.URL, "e", "t")
	require.NoError(t, err)

	return client
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, body any) {
	t.Helper()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	require.NoError(t, json.NewEncoder(w).Encode(body))
}

func TestErrorTranslatedToJiraError(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusUnauthorized, map[string]any{"errorMessages": []string{"nope"}})
	})

	_, err := client.Myself()

	require.Error(t, err)
	var jiraErr *jira.Error
	require.ErrorAs(t, err, &jiraErr)
	assert.Equal(t, 401, jiraErr.Status)
	assert.Contains(t, jiraErr.Message, "nope")
}

func TestSearchIssues_PagesWithNextPageToken(t *testing.T) {
	var tokens []string

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		tokens = append(tokens, r.URL.Query().Get("nextPageToken"))
		if r.URL.Query().Get("nextPageToken") == "" {
			writeJSON(t, w, http.StatusOK, map[string]any{
				"issues":        []map[string]any{{"key": "P-1"}, {"key": "P-2"}},
				"nextPageToken": "t2",
			})
			return
		}
		writeJSON(t, w, http.StatusOK, map[string]any{
			"issues": []map[string]any{{"key": "P-3"}},
		})
	})

	var keys []string
	err := client.SearchIssues("project = P", nil, 200, func(issue map[string]any) {
		keys = append(keys, issue["key"].(string))
	})

	require.NoError(t, err)
	assert.Equal(t, []string{"P-1", "P-2", "P-3"}, keys)
	require.Len(t, tokens, 2)
	assert.Equal(t, "t2", tokens[1])
}

func TestSearchIssues_RespectsLimit(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{
			"issues":        []map[string]any{{"key": "P-1"}, {"key": "P-2"}},
			"nextPageToken": "more",
		})
	})

	var keys []string
	err := client.SearchIssues("project = P", nil, 2, func(issue map[string]any) {
		keys = append(keys, issue["key"].(string))
	})

	require.NoError(t, err)
	assert.Equal(t, []string{"P-1", "P-2"}, keys)
}

func TestChangelog_PaginatesUntilIsLast(t *testing.T) {
	var calls int

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			writeJSON(t, w, http.StatusOK, map[string]any{
				"values": []map[string]any{
					{"items": []map[string]any{
						{"field": "status", "fromString": "To Do", "toString": "Doing"},
					}},
				},
				"isLast":     false,
				"maxResults": 100,
			})
			return
		}
		writeJSON(t, w, http.StatusOK, map[string]any{
			"values": []map[string]any{
				{"items": []map[string]any{
					{"field": "assignee", "fromString": nil, "toString": "jon"},
					{"field": "status", "fromString": "Doing", "toString": "Done"},
				}},
			},
			"isLast": true,
		})
	})

	changes, err := client.StatusChanges("P-1")

	require.NoError(t, err)
	assert.Equal(t, []jira.StatusChange{
		{From: "To Do", To: "Doing"},
		{From: "Doing", To: "Done"},
	}, changes)
}

func TestProjectStatuses_MergesIssueTypes(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, []map[string]any{
			{"statuses": []map[string]any{
				{"name": "To Do", "statusCategory": map[string]any{"key": "new"}},
			}},
			{"statuses": []map[string]any{
				{"name": "Done", "statusCategory": map[string]any{"key": "done"}},
				{"name": "To Do", "statusCategory": map[string]any{"key": "new"}},
			}},
		})
	})

	statuses, err := client.ProjectStatuses("P")

	require.NoError(t, err)
	assert.Equal(t, map[string]string{"To Do": "new", "Done": "done"}, statuses)
}

func TestGetTransitions_ReturnsRawAPIShape(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{
			"transitions": []map[string]any{
				{"id": "31", "name": "Start", "to": map[string]any{"name": "In Progress"}},
			},
		})
	})

	transitions, err := client.GetTransitions("P-1")

	require.NoError(t, err)
	require.Len(t, transitions, 1)
	to, ok := transitions[0]["to"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "In Progress", to["name"])
}

func TestTransitionIssue_WithFieldsPostsFullPayload(t *testing.T) {
	var gotPath string
	var gotBody map[string]any

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		w.WriteHeader(http.StatusNoContent)
	})

	err := client.TransitionIssue("P-1", "31", map[string]any{"resolution": map[string]any{"name": "Done"}})

	require.NoError(t, err)
	assert.Contains(t, gotPath, "/issue/P-1/transitions")
	assert.Equal(t, map[string]any{
		"transition": map[string]any{"id": "31"},
		"fields":     map[string]any{"resolution": map[string]any{"name": "Done"}},
	}, gotBody)
}

func TestAddComment_PostsToCommentEndpoint(t *testing.T) {
	var gotPath string

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		writeJSON(t, w, http.StatusCreated, map[string]any{"id": "1", "body": "hi"})
	})

	_, err := client.AddComment("P-1", "hi")

	require.NoError(t, err)
	assert.Contains(t, gotPath, "/issue/P-1/comment")
}

func TestCreateIssue_PostsProjectSummaryAndType(t *testing.T) {
	var gotBody map[string]any

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		writeJSON(t, w, http.StatusCreated, map[string]any{"key": "P-1", "id": "10000"})
	})

	key, err := client.CreateIssue("P", "New issue", "Task", "a description", []string{"unjira-seed"})

	require.NoError(t, err)
	assert.Equal(t, "P-1", key)

	fields, ok := gotBody["fields"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "New issue", fields["summary"])
	assert.Equal(t, "a description", fields["description"])
}

func TestDeleteIssue_CallsDeleteEndpoint(t *testing.T) {
	var gotMethod, gotPath string

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	})

	err := client.DeleteIssue("P-1")

	require.NoError(t, err)
	assert.Equal(t, http.MethodDelete, gotMethod)
	assert.Contains(t, gotPath, "/issue/P-1")
}

func TestSearchProjects_ReturnsProjectList(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, []map[string]any{
			{"key": "PROJ", "name": "Project One"},
			{"key": "SCRUM", "name": "Scrum Project"},
		})
	})

	projects, err := client.SearchProjects()

	require.NoError(t, err)
	require.Len(t, projects, 2)
	assert.Equal(t, "PROJ", projects[0]["key"])
	assert.Equal(t, "SCRUM", projects[1]["key"])
}

func TestNew_RequiresSite(t *testing.T) {
	_, err := jira.New("", "e", "t")

	assert.Error(t, err)
}
