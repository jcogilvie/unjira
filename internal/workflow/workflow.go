// Package workflow mines observed workflow graphs from issue changelogs.
//
// Three-tier workflow knowledge: admin API when available, this observed
// graph for planning, and per-issue GET /transitions as ground truth at
// execution time. The graph carries edge frequencies: the happy path falls
// out statistically, and rare edges are a proxy for "unusual move, gate it" —
// a sharper rerouting guardrail than status-category direction alone.
//
// Staleness: cache the graph per project; when the live transitions endpoint
// returns an edge the graph doesn't predict, mark it dirty and re-mine.
package workflow

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
)

// edgeKey identifies a directed transition between two statuses.
type edgeKey struct {
	From, To string
}

// StatusChange is a single (from, to) status transition observed in an
// issue's changelog. Lives here (not in a backend-specific client package)
// because it's a workflow-mining concept, not a Jira-specific one, and it
// lets projectMiner below stay free of any clients/ import — keeping the
// dependency direction strictly clients/jira -> workflow, never the
// reverse (jira.Tracker.WorkflowGraph needs *workflow.Graph).
type StatusChange struct {
	From string
	To   string
}

// Graph is an observed workflow graph: statuses with categories, and
// directed status transitions with observed frequencies.
type Graph struct {
	statusCategories map[string]string
	edges            map[edgeKey]int
}

// NewGraph returns an empty Graph.
func NewGraph() *Graph {
	return &Graph{
		statusCategories: make(map[string]string),
		edges:            make(map[edgeKey]int),
	}
}

// AddStatus records a status name with its category. If the status is
// already known, its category is left unchanged (first write wins, matching
// Python's dict.setdefault).
func (g *Graph) AddStatus(name, category string) {
	if _, exists := g.statusCategories[name]; !exists {
		g.statusCategories[name] = category
	}
}

// Observe records one transition from fromStatus to toStatus, adding both
// statuses (with an "unknown" category if not already known).
func (g *Graph) Observe(fromStatus, toStatus string) {
	g.addStatusUnknown(fromStatus)
	g.addStatusUnknown(toStatus)
	g.edges[edgeKey{fromStatus, toStatus}]++
}

func (g *Graph) addStatusUnknown(name string) {
	if _, exists := g.statusCategories[name]; !exists {
		g.statusCategories[name] = "unknown"
	}
}

// Neighbors returns every status reachable from status via one observed
// edge.
func (g *Graph) Neighbors(status string) []string {
	var out []string
	for edge := range g.edges {
		if edge.From == status {
			out = append(out, edge.To)
		}
	}

	return out
}

// HasEdge reports whether fromStatus -> toStatus has been observed.
func (g *Graph) HasEdge(fromStatus, toStatus string) bool {
	_, ok := g.edges[edgeKey{fromStatus, toStatus}]
	return ok
}

// Path returns the shortest observed transition path from fromStatus to
// toStatus, inclusive of both endpoints, or nil if none exists.
func (g *Graph) Path(fromStatus, toStatus string) []string {
	if fromStatus == toStatus {
		return []string{fromStatus}
	}

	queue := [][]string{{fromStatus}}
	seen := map[string]bool{fromStatus: true}

	for len(queue) > 0 {
		trail := queue[0]
		queue = queue[1:]

		for _, next := range g.Neighbors(trail[len(trail)-1]) {
			if seen[next] {
				continue
			}
			if next == toStatus {
				return append(append([]string{}, trail...), next)
			}
			seen[next] = true
			queue = append(queue, append(append([]string{}, trail...), next))
		}
	}

	return nil
}

// Edge is one observed transition and how many times it was seen.
type Edge struct {
	From, To string
	Count    int
}

// RareEdges returns edges taken at most maxCount times, sorted, as
// candidates for gating.
func (g *Graph) RareEdges(maxCount int) []Edge {
	var out []Edge
	for edge, count := range g.edges {
		if count <= maxCount {
			out = append(out, Edge{From: edge.From, To: edge.To, Count: count})
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].From != out[j].From {
			return out[i].From < out[j].From
		}
		return out[i].To < out[j].To
	})

	return out
}

// StatusCategories returns a copy of the known status -> category map.
func (g *Graph) StatusCategories() map[string]string {
	out := make(map[string]string, len(g.statusCategories))
	maps.Copy(out, g.statusCategories)

	return out
}

// Edges returns a copy of the observed edge -> count map, keyed by
// "from\x00to" so it remains comparable across Graph instances without
// exposing the unexported edgeKey type.
func (g *Graph) Edges() map[[2]string]int {
	out := make(map[[2]string]int, len(g.edges))
	for k, v := range g.edges {
		out[[2]string{k.From, k.To}] = v
	}

	return out
}

// -- persistence ---------------------------------------------------------

type graphEdgeJSON struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Count int    `json:"count"`
}

type graphJSON struct {
	StatusCategories map[string]string `json:"status_categories"`
	Edges            []graphEdgeJSON   `json:"edges"`
}

// ToMap returns a JSON-serializable representation of the graph.
func (g *Graph) ToMap() map[string]any {
	edges := make([]graphEdgeJSON, 0, len(g.edges))
	for k, v := range g.edges {
		edges = append(edges, graphEdgeJSON{From: k.From, To: k.To, Count: v})
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].From != edges[j].From {
			return edges[i].From < edges[j].From
		}
		return edges[i].To < edges[j].To
	})

	return map[string]any{
		"status_categories": g.StatusCategories(),
		"edges":             edges,
	}
}

// GraphFromMap reconstructs a Graph from the representation produced by
// ToMap (round-tripped through JSON, so map values decode as
// map[string]any/[]any rather than the concrete types ToMap produced).
func GraphFromMap(data map[string]any) (*Graph, error) {
	body, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("marshaling graph map: %w", err)
	}

	var parsed graphJSON
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("unmarshaling graph: %w", err)
	}

	g := NewGraph()
	maps.Copy(g.statusCategories, parsed.StatusCategories)
	for _, edge := range parsed.Edges {
		g.edges[edgeKey{edge.From, edge.To}] = edge.Count
	}

	return g, nil
}

// Save writes the graph as JSON to path, creating parent directories as
// needed.
func (g *Graph) Save(path string) error {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("creating directory for %s: %w", path, err)
		}
	}

	body, err := json.MarshalIndent(g.ToMap(), "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling graph: %w", err)
	}

	if err := os.WriteFile(path, body, 0o600); err != nil {
		return fmt.Errorf("writing graph to %s: %w", path, err)
	}

	return nil
}

// Load reads a graph previously written by Save.
func Load(path string) (*Graph, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading graph from %s: %w", path, err)
	}

	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("unmarshaling graph from %s: %w", path, err)
	}

	return GraphFromMap(data)
}

// GraphProvider is an optional TaskTracker capability: backends whose
// workflow model has a real transition graph to give (Jira's is genuinely
// mined from changelog history, since its workflows are admin-configurable
// with no static answer) implement this so callers can get a *Graph however
// that backend produces one — GitHub Issues would hardcode a static
// open/closed graph, the local backend hardcodes a static three-category
// graph (see internal/clients/local). Not part of tasktracker.TaskTracker
// itself: it lives here, next to Graph, and callers type-assert for it
// (`if p, ok := tracker.(workflow.GraphProvider); ok { ... }`) rather than
// every TaskTracker implementation being forced to have an opinion on
// workflow graphs.
type GraphProvider interface {
	WorkflowGraph(projectOrRepo string) (*Graph, error)
}

// projectMiner is the Jira-shaped mining capability MineProject needs — a
// consumer-owned interface (Go idiom) rather than a concrete *jira.Client,
// so a fake can exercise MineProject in tests without a real HTTP server.
// *jira.Client already satisfies this. Not a candidate for
// tasktracker.TaskTracker: no other backend (GitHub Issues, the local
// tracker) has a changelog to mine into this shape — see
// jira.Tracker.WorkflowGraph, which calls MineProject internally and is
// itself the tasktracker-facing capability.
type projectMiner interface {
	ProjectStatuses(projectKey string) (map[string]string, error)
	SearchIssues(jql string, fields []string, limit int, visit func(map[string]any)) error
	StatusChanges(key string) ([]StatusChange, error)
}

// MineProject reconstructs the used workflow subgraph from recent issue
// changelogs.
//
// The same changelog fetch later feeds estimation calibration — keep the two
// consumers on one code path when that lands.
func MineProject(client projectMiner, projectKey string, maxIssues int) (*Graph, error) {
	g := NewGraph()

	statuses, err := client.ProjectStatuses(projectKey)
	if err != nil {
		return nil, fmt.Errorf("fetching statuses for project %s: %w", projectKey, err)
	}
	for name, category := range statuses {
		g.AddStatus(name, category)
	}

	jql := fmt.Sprintf(`project = "%s" ORDER BY updated DESC`, projectKey)

	var firstErr error
	err = client.SearchIssues(jql, []string{"status"}, maxIssues, func(issue map[string]any) {
		if firstErr != nil {
			return
		}
		key, _ := issue["key"].(string)

		changes, err := client.StatusChanges(key)
		if err != nil {
			firstErr = fmt.Errorf("fetching status changes for %s: %w", key, err)
			return
		}
		for _, change := range changes {
			g.Observe(change.From, change.To)
		}
	})
	if err != nil {
		return nil, fmt.Errorf("searching issues in project %s: %w", projectKey, err)
	}
	if firstErr != nil {
		return nil, firstErr
	}

	return g, nil
}
