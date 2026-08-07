package workflow_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/clients/jira"
	"github.com/jcogilvie/unjira/internal/workflow"
)

func buildGraph() *workflow.Graph {
	g := workflow.NewGraph()
	for i := 0; i < 5; i++ {
		g.Observe("To Do", "In Progress")
		g.Observe("In Progress", "In Review")
		g.Observe("In Review", "Done")
	}
	g.Observe("In Review", "In Progress") // rare bounce-back

	return g
}

func TestPath_BFSMultiHop(t *testing.T) {
	path := buildGraph().Path("To Do", "Done")

	assert.Equal(t, []string{"To Do", "In Progress", "In Review", "Done"}, path)
}

func TestPath_NoPathReturnsNil(t *testing.T) {
	path := buildGraph().Path("Done", "To Do")

	assert.Nil(t, path)
}

func TestPath_SameStatusIsTrivialPath(t *testing.T) {
	path := buildGraph().Path("Done", "Done")

	assert.Equal(t, []string{"Done"}, path)
}

func TestRareEdges_FlagsTheBounceBack(t *testing.T) {
	edges := buildGraph().RareEdges(1)

	assert.Equal(t, []workflow.Edge{{From: "In Review", To: "In Progress", Count: 1}}, edges)
}

func TestDictRoundtrip_PreservesCounts(t *testing.T) {
	g := buildGraph()
	g.AddStatus("To Do", "new")

	clone, err := workflow.GraphFromMap(g.ToMap())

	require.NoError(t, err)
	assert.Equal(t, g.Edges(), clone.Edges())
	assert.Equal(t, g.StatusCategories(), clone.StatusCategories())
	assert.Equal(t, g.Path("To Do", "Done"), clone.Path("To Do", "Done"))
}

func TestMineProject_ObservesStatusesAndTransitions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.URL.Path == "/rest/api/2/project/PROJ/statuses":
			json.NewEncoder(w).Encode([]map[string]any{
				{"statuses": []map[string]any{
					{"name": "To Do", "statusCategory": map[string]any{"key": "new"}},
					{"name": "Done", "statusCategory": map[string]any{"key": "done"}},
				}},
			})
		case r.URL.Path == "/rest/api/3/search/jql":
			json.NewEncoder(w).Encode(map[string]any{
				"issues": []map[string]any{{"key": "PROJ-1"}},
			})
		case r.URL.Path == "/rest/api/2/issue/PROJ-1/changelog":
			json.NewEncoder(w).Encode(map[string]any{
				"isLast": true,
				"values": []map[string]any{
					{"items": []map[string]any{
						{"field": "status", "fromString": "To Do", "toString": "Done"},
					}},
				},
			})
		default:
			t.Fatalf("unexpected request: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client, err := jira.New(server.URL, "e", "t")
	require.NoError(t, err)

	graph, err := workflow.MineProject(client, "PROJ", 50)

	require.NoError(t, err)
	assert.Equal(t, map[string]string{"To Do": "new", "Done": "done"}, graph.StatusCategories())
	assert.True(t, graph.HasEdge("To Do", "Done"))
}
