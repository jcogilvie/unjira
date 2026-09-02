package main

// dev_workflow_test.go exercises devWorkflowCmd against a real httptest Jira
// server (the same fixture shape as internal/workflow's own
// TestMineProject_ObservesStatusesAndTransitions), proving the cache wiring
// end to end: a first run with no cache mines and says so, an immediate
// second run is served from disk instead of hitting the tracker again, and
// --refresh bypasses a fresh cache on demand. The cache's default path
// (workflow.DefaultCacheDir, "data/workflow-cache", relative to the working
// directory) is exercised for real via t.Chdir(t.TempDir()) rather than
// wired around — matching internal/store's own TestOpen_RelativePath
// convention for proving a real relative default path works without
// touching the repo tree.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/credentials"
)

// workflowMiningServer serves the three endpoints workflow.MineProject
// needs (project statuses, a search hit, and that issue's changelog), so
// devWorkflowCmd mines a real, small, known graph rather than a stub.
func workflowMiningServer(t *testing.T) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		// assert, not require/t.Fatalf: this runs in a handler goroutine,
		// where FailNow (runtime.Goexit) would not fail the test itself —
		// matching internal/workflow's own httptest fixture.
		switch r.URL.Path {
		case "/rest/api/2/project/PROJ/statuses":
			assert.NoError(t, json.NewEncoder(w).Encode([]map[string]any{
				{"statuses": []map[string]any{
					{"name": "To Do", "statusCategory": map[string]any{"key": "new"}},
					{"name": "Done", "statusCategory": map[string]any{"key": "done"}},
				}},
			}))
		case "/rest/api/3/search/jql":
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{
				"issues": []map[string]any{{"key": "PROJ-1"}},
			}))
		case "/rest/api/2/issue/PROJ-1/changelog":
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{
				"isLast": true,
				"values": []map[string]any{
					{"items": []map[string]any{
						{"field": "status", "fromString": "To Do", "toString": "Done"},
					}},
				},
			}))
		default:
			assert.Fail(t, "unexpected request", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	return server
}

func devWorkflowTestApp(t *testing.T, siteURL string) *appContext {
	t.Helper()

	return &appContext{
		config: config.Config{
			Jira: []config.JiraConnection{
				{Name: "default", Site: siteURL, ProjectKeys: []string{"PROJ"}},
			},
		},
		jiraCredentials: jsonSetFor(t, map[string]credentials.Credential{
			"default": {Email: "e", Token: "t"},
		}),
	}
}

// TestDevWorkflow_FirstRunMinesThenSecondRunIsCached is the behavior this
// PR exists to add: devWorkflowCmd's own doc comment used to describe a
// design that had zero callers (workflow.Save/Load) — this proves the cache
// is actually load-bearing from the CLI's own entry point, not just from
// workflow's unit tests.
func TestDevWorkflow_FirstRunMinesThenSecondRunIsCached(t *testing.T) {
	t.Chdir(t.TempDir())
	server := workflowMiningServer(t)
	app := devWorkflowTestApp(t, server.URL)

	firstOut := captureStdout(t, func() {
		require.NoError(t, (&devWorkflowCmd{Project: "PROJ"}).Run(app))
	})
	assert.Contains(t, firstOut, "freshly mined", "the first run has no cache yet and must say so")

	secondOut := captureStdout(t, func() {
		require.NoError(t, (&devWorkflowCmd{Project: "PROJ"}).Run(app))
	})
	assert.Contains(t, secondOut, "cached",
		"an immediate second run must be served from disk, not hit the tracker again")
}

// TestDevWorkflow_RefreshForcesReMineEvenWithAFreshCache proves --refresh is
// wired end to end: without it, the case above is served from cache, but
// with it the operator gets a fresh mine on demand — the design spec's
// "explicit flag" invalidation trigger.
func TestDevWorkflow_RefreshForcesReMineEvenWithAFreshCache(t *testing.T) {
	t.Chdir(t.TempDir())
	server := workflowMiningServer(t)
	app := devWorkflowTestApp(t, server.URL)

	require.NoError(t, (&devWorkflowCmd{Project: "PROJ"}).Run(app))

	out := captureStdout(t, func() {
		require.NoError(t, (&devWorkflowCmd{Project: "PROJ", Refresh: true}).Run(app))
	})

	assert.Contains(t, out, "freshly mined", "--refresh must bypass a fresh, well-within-TTL cache")
}
