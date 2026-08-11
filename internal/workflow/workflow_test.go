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
	for range 5 {
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

		// assert, not require/t.Fatalf: this is a handler goroutine, where
		// FailNow (runtime.Goexit) wouldn't fail the test itself.
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
	defer server.Close()

	client, err := jira.New(server.URL, "e", "t")
	require.NoError(t, err)

	graph, err := workflow.MineProject(client, "PROJ", 50)

	require.NoError(t, err)
	assert.Equal(t, map[string]string{"To Do": "new", "Done": "done"}, graph.StatusCategories())
	assert.True(t, graph.HasEdge("To Do", "Done"))
}

// fakeMiner satisfies workflow's projectMiner interface without being a
// *jira.Client, proving MineProject's signature actually decoupled from the
// concrete type rather than just compiling against it by coincidence.
type fakeMiner struct {
	statuses map[string]string
	issues   []map[string]any
	changes  map[string][]workflow.StatusChange
}

func (f *fakeMiner) ProjectStatuses(_ string) (map[string]string, error) {
	return f.statuses, nil
}

func (f *fakeMiner) SearchIssues(_ string, _ []string, _ int, visit func(map[string]any)) error {
	for _, issue := range f.issues {
		visit(issue)
	}
	return nil
}

func (f *fakeMiner) StatusChanges(key string) ([]workflow.StatusChange, error) {
	return f.changes[key], nil
}

func TestMineProject_AcceptsNonJiraMiner(t *testing.T) {
	miner := &fakeMiner{
		statuses: map[string]string{"To Do": "new", "Done": "done"},
		issues:   []map[string]any{{"key": "PROJ-1"}},
		changes: map[string][]workflow.StatusChange{
			"PROJ-1": {{From: "To Do", To: "Done"}},
		},
	}

	graph, err := workflow.MineProject(miner, "PROJ", 50)

	require.NoError(t, err)
	assert.Equal(t, map[string]string{"To Do": "new", "Done": "done"}, graph.StatusCategories())
	assert.True(t, graph.HasEdge("To Do", "Done"))
}
