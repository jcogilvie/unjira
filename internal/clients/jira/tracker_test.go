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

// paasTransitions is the response shape a real PAAS issue mid-flight returns:
// four legal destinations, ALL in the indeterminate category. Mined 2026-09-01
// via `dev workflow --project PAAS`.
func paasTransitions() map[string]any {
	return map[string]any{
		"transitions": []map[string]any{
			{"id": "31", "to": map[string]any{
				"name": "In Progress", "statusCategory": map[string]any{"key": "indeterminate"},
			}},
			{"id": "41", "to": map[string]any{
				"name": "In Review", "statusCategory": map[string]any{"key": "indeterminate"},
			}},
			{"id": "51", "to": map[string]any{
				"name": "Blocked", "statusCategory": map[string]any{"key": "indeterminate"},
			}},
			{"id": "61", "to": map[string]any{
				"name": "In Test", "statusCategory": map[string]any{"key": "indeterminate"},
			}},
		},
	}
}

// TestTracker_SetStatus_PicksTheTransitionMatchingTheName is the wrong-write
// regression, and the reason this whole change exists.
//
// All four destinations share the indeterminate category. The previous
// category-matching implementation executed the FIRST transition whose category
// matched, so asking for In Review would have posted transition 31 and moved the
// ticket to In Progress; asking for Blocked would have done the same. Every
// transition request in a real project would have hit the wrong status — not
// occasionally, but by construction.
func TestTracker_SetStatus_PicksTheTransitionMatchingTheName(t *testing.T) {
	var gotBody map[string]any

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(t, w, http.StatusOK, paasTransitions())

			return
		}

		assert.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		w.WriteHeader(http.StatusNoContent)
	})
	tr := jira.NewTracker(client)

	require.NoError(t, tr.SetStatus("P-1", "In Review"))

	transition, ok := gotBody["transition"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "41", transition["id"],
		"41 is In Review; 31 is In Progress and shares its category")
}

// TestTracker_SetStatus_IsCaseInsensitive: the target name survives a round trip
// through a model response and a persisted JSON payload before arriving here.
// Refusing a legitimate transition over "in review" vs "In Review" would be a
// self-inflicted failure, and Jira treats status names as display strings with
// no casing guarantee.
func TestTracker_SetStatus_IsCaseInsensitive(t *testing.T) {
	var posted bool

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(t, w, http.StatusOK, paasTransitions())

			return
		}

		posted = true
		w.WriteHeader(http.StatusNoContent)
	})
	tr := jira.NewTracker(client)

	require.NoError(t, tr.SetStatus("P-1", "  in review  "))
	assert.True(t, posted)
}

// TestTracker_SetStatus_NoMatchingNameReturnsErrorNamingWhatWasAvailable: an
// opaque refusal makes a typo, a workflow edit, and someone else having already
// moved the issue indistinguishable. The available list is the diagnosis.
func TestTracker_SetStatus_NoMatchingNameReturnsErrorNamingWhatWasAvailable(t *testing.T) {
	var posted bool

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(t, w, http.StatusOK, paasTransitions())

			return
		}

		posted = true
		w.WriteHeader(http.StatusNoContent)
	})
	tr := jira.NewTracker(client)

	err := tr.SetStatus("P-1", "Shipped")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "P-1")
	assert.Contains(t, err.Error(), "Shipped")
	assert.Contains(t, err.Error(), "In Review", "the error must name what WAS available")
	assert.False(t, posted, "an unresolvable name must never execute some other transition")
}

// TestTracker_SetStatus_DoesNotPrefixMatch: "In" is a prefix of three real
// destinations. Matching loosely would pick one arbitrarily and apply a
// transition nobody approved — worse than refusing, because it looks like it
// worked.
func TestTracker_SetStatus_DoesNotPrefixMatch(t *testing.T) {
	var posted bool

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(t, w, http.StatusOK, paasTransitions())

			return
		}

		posted = true
		w.WriteHeader(http.StatusNoContent)
	})
	tr := jira.NewTracker(client)

	require.Error(t, tr.SetStatus("P-1", "In"))
	assert.False(t, posted)
}

// TestTracker_AvailableTransitions_KeepsDestinationsSharingACategory is the
// read-side half of the same regression. The old category-keyed version
// deduplicated these four PAAS destinations down to ONE entry, so the reconciler
// could not tell that In Review was reachable and Blocked was too.
func TestTracker_AvailableTransitions_KeepsDestinationsSharingACategory(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, paasTransitions())
	})
	tr := jira.NewTracker(client)

	got, err := tr.AvailableTransitions("PROJ-1")
	require.NoError(t, err)

	assert.Equal(t, []tasktracker.Transition{
		{ToStatus: "In Progress", ToCategory: tasktracker.StatusInProgress},
		{ToStatus: "In Review", ToCategory: tasktracker.StatusInProgress},
		{ToStatus: "Blocked", ToCategory: tasktracker.StatusInProgress},
		{ToStatus: "In Test", ToCategory: tasktracker.StatusInProgress},
	}, got, "four distinct destinations, not one category")
}

// TestTracker_AvailableTransitions_KeepsAnUnrecognizedCategoryAsANamedTarget is
// a deliberate REVERSAL of the old behavior, which dropped such a transition
// entirely.
//
// Dropping it was defensible when the category WAS the target — reporting a
// category nothing verified would license a wrong write. Now the name is the
// target, and the name was verified: it came from the live endpoint. Dropping it
// suppressed three real PAAS statuses (Open, Paused, To Do, all `unknown`
// category), which is a false negative refusing a legal move. An empty category
// weakens only the advisory direction check.
func TestTracker_AvailableTransitions_KeepsAnUnrecognizedCategoryAsANamedTarget(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{
			"transitions": []map[string]any{
				{"id": "31", "to": map[string]any{
					"name": "In Progress", "statusCategory": map[string]any{"key": "indeterminate"},
				}},
				// Jira reports "Paused" in PAAS with a category this package
				// does not map.
				{"id": "91", "to": map[string]any{
					"name": "Paused", "statusCategory": map[string]any{"key": "cosmic"},
				}},
			},
		})
	})
	tr := jira.NewTracker(client)

	got, err := tr.AvailableTransitions("PROJ-1")
	require.NoError(t, err)

	assert.Equal(t, []tasktracker.Transition{
		{ToStatus: "In Progress", ToCategory: tasktracker.StatusInProgress},
		{ToStatus: "Paused", ToCategory: ""},
	}, got)
	assert.NotEqual(t, tasktracker.StatusTodo, got[1].ToCategory,
		"an unrecognized category must stay empty, not default to Todo")
}

// TestTracker_AvailableTransitions_DeduplicatesByName: two transitions to the
// same named status are one destination. A reviewer offered the same move twice
// would reasonably think they were different.
func TestTracker_AvailableTransitions_DeduplicatesByName(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{
			"transitions": []map[string]any{
				{"id": "41", "to": map[string]any{
					"name": "Done", "statusCategory": map[string]any{"key": "done"},
				}},
				// A second workflow path to the same status — routine in Jira.
				{"id": "51", "to": map[string]any{
					"name": "Done", "statusCategory": map[string]any{"key": "done"},
				}},
			},
		})
	})
	tr := jira.NewTracker(client)

	got, err := tr.AvailableTransitions("PROJ-1")
	require.NoError(t, err)

	assert.Equal(t, []tasktracker.Transition{
		{ToStatus: "Done", ToCategory: tasktracker.StatusDone},
	}, got)
}

// TestTracker_AvailableTransitions_SkipsANamelessDestination: a destination with
// no name cannot be targeted, since the name IS the target. Returning it with an
// empty ToStatus would put an unselectable entry in front of the reconciler.
func TestTracker_AvailableTransitions_SkipsANamelessDestination(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{
			"transitions": []map[string]any{
				{"id": "11", "to": map[string]any{
					"statusCategory": map[string]any{"key": "new"},
				}},
				{"id": "31", "to": map[string]any{
					"name": "In Progress", "statusCategory": map[string]any{"key": "indeterminate"},
				}},
			},
		})
	})
	tr := jira.NewTracker(client)

	got, err := tr.AvailableTransitions("PROJ-1")
	require.NoError(t, err)

	assert.Equal(t, []tasktracker.Transition{
		{ToStatus: "In Progress", ToCategory: tasktracker.StatusInProgress},
	}, got)
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
