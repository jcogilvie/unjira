package jira_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/clients/jira"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

func TestTracker_GetIssue_NormalizesFields(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{
			"key": "P-1",
			"fields": map[string]any{
				"summary": "Fix the bug",
				"status": map[string]any{
					"name":           "In Progress",
					"statusCategory": map[string]any{"key": "indeterminate"},
				},
				"labels": []string{"bug", "urgent"},
			},
		})
	})
	tr := jira.NewTracker(client)

	issue, err := tr.GetIssue("P-1")

	require.NoError(t, err)
	assert.Equal(t, tasktracker.Issue{
		Key:            "P-1",
		Summary:        "Fix the bug",
		StatusCategory: tasktracker.StatusInProgress,
		StatusName:     "In Progress",
		Labels:         []string{"bug", "urgent"},
	}, issue)
}

func TestTracker_GetIssue_MapsStatusCategoriesToNormalizedBuckets(t *testing.T) {
	tests := []struct {
		jiraCategory string
		want         tasktracker.StatusCategory
	}{
		{"new", tasktracker.StatusTodo},
		{"indeterminate", tasktracker.StatusInProgress},
		{"done", tasktracker.StatusDone},
		{"unknown", tasktracker.StatusTodo},
	}

	for _, tt := range tests {
		t.Run(tt.jiraCategory, func(t *testing.T) {
			client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, http.StatusOK, map[string]any{
					"key": "P-1",
					"fields": map[string]any{
						"summary": "x",
						"status": map[string]any{
							"name":           "x",
							"statusCategory": map[string]any{"key": tt.jiraCategory},
						},
					},
				})
			})
			tr := jira.NewTracker(client)

			issue, err := tr.GetIssue("P-1")

			require.NoError(t, err)
			assert.Equal(t, tt.want, issue.StatusCategory)
		})
	}
}

func TestTracker_SearchIssues_NormalizesEachHit(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{
			"issues": []map[string]any{
				{
					"key": "P-1",
					"fields": map[string]any{
						"summary": "First",
						"status": map[string]any{
							"name":           "To Do",
							"statusCategory": map[string]any{"key": "new"},
						},
					},
				},
				{
					"key": "P-2",
					"fields": map[string]any{
						"summary": "Second",
						"status": map[string]any{
							"name":           "Done",
							"statusCategory": map[string]any{"key": "done"},
						},
					},
				},
			},
		})
	})
	tr := jira.NewTracker(client)

	issues, err := tr.SearchIssues("project = P", 10)

	require.NoError(t, err)
	require.Len(t, issues, 2)
	assert.Equal(t, "P-1", issues[0].Key)
	assert.Equal(t, tasktracker.StatusTodo, issues[0].StatusCategory)
	assert.Equal(t, "P-2", issues[1].Key)
	assert.Equal(t, tasktracker.StatusDone, issues[1].StatusCategory)
}

func TestTracker_AddComment_DelegatesToClient(t *testing.T) {
	var gotPath string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		writeJSON(t, w, http.StatusCreated, map[string]any{"id": "1"})
	})
	tr := jira.NewTracker(client)

	err := tr.AddComment("P-1", "a comment")

	require.NoError(t, err)
	assert.Contains(t, gotPath, "/issue/P-1/comment")
}

func TestTracker_CreateIssue_DelegatesToClient(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, json.NewDecoder(r.Body).Decode(new(map[string]any))) // handler goroutine: assert, not require
		writeJSON(t, w, http.StatusCreated, map[string]any{"key": "P-1"})
	})
	tr := jira.NewTracker(client)

	key, err := tr.CreateIssue("P", "New issue", "Task", "desc", []string{"bug"})

	require.NoError(t, err)
	assert.Equal(t, "P-1", key)
}

func TestTracker_SetStatus_PicksTransitionMatchingCategory(t *testing.T) {
	var gotPath string
	var gotBody map[string]any

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(t, w, http.StatusOK, map[string]any{
				"transitions": []map[string]any{
					{"id": "11", "to": map[string]any{"statusCategory": map[string]any{"key": "new"}}},
					{"id": "31", "to": map[string]any{"statusCategory": map[string]any{"key": "indeterminate"}}},
					{"id": "41", "to": map[string]any{"statusCategory": map[string]any{"key": "done"}}},
				},
			})
			return
		}

		gotPath = r.URL.Path
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		w.WriteHeader(http.StatusNoContent)
	})
	tr := jira.NewTracker(client)

	err := tr.SetStatus("P-1", tasktracker.StatusInProgress)

	require.NoError(t, err)
	assert.Contains(t, gotPath, "/issue/P-1/transitions")
	transition, ok := gotBody["transition"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "31", transition["id"])
}

func TestTracker_SetStatus_NoMatchingTransitionReturnsError(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{
			"transitions": []map[string]any{
				{"id": "11", "to": map[string]any{"statusCategory": map[string]any{"key": "new"}}},
			},
		})
	})
	tr := jira.NewTracker(client)

	err := tr.SetStatus("P-1", tasktracker.StatusDone)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "P-1")
}

func TestTracker_WorkflowGraph_DelegatesToMineProject(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rest/api/2/project/PROJ/statuses":
			writeJSON(t, w, http.StatusOK, []map[string]any{
				{"statuses": []map[string]any{
					{"name": "To Do", "statusCategory": map[string]any{"key": "new"}},
					{"name": "Done", "statusCategory": map[string]any{"key": "done"}},
				}},
			})
		case "/rest/api/3/search/jql":
			writeJSON(t, w, http.StatusOK, map[string]any{
				"issues": []map[string]any{{"key": "PROJ-1"}},
			})
		case "/rest/api/2/issue/PROJ-1/changelog":
			writeJSON(t, w, http.StatusOK, map[string]any{
				"isLast": true,
				"values": []map[string]any{
					{"items": []map[string]any{
						{"field": "status", "fromString": "To Do", "toString": "Done"},
					}},
				},
			})
		default:
			assert.Fail(t, "unexpected request", r.URL.Path)
		}
	})
	tr := jira.NewTracker(client)

	graph, err := tr.WorkflowGraph("PROJ")

	require.NoError(t, err)
	assert.Equal(t, map[string]string{"To Do": "new", "Done": "done"}, graph.StatusCategories())
	assert.True(t, graph.HasEdge("To Do", "Done"))
}
