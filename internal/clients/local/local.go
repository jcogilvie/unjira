// Package local implements tasktracker.TaskTracker against unjira's own
// SQLite store, for running the correlator/reconciler with no real tracker
// reachable (e.g. a hosted control plane with no Jira auth configured)
// while still deriving a local readout of proposed/applied state.
package local

import (
	"fmt"

	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
	"github.com/jcogilvie/unjira/internal/workflow"
)

// Tracker implements tasktracker.TaskTracker against a *store.Store.
type Tracker struct {
	store *store.Store
}

var (
	_ tasktracker.TaskTracker = (*Tracker)(nil)
	_ workflow.GraphProvider  = (*Tracker)(nil)
)

// New returns a Tracker backed by s.
func New(s *store.Store) *Tracker {
	return &Tracker{store: s}
}

// CreateIssue creates a local issue and returns its assigned key.
func (t *Tracker) CreateIssue(projectOrRepo, summary, issueType, description string, labels []string) (string, error) {
	key, err := t.store.InsertLocalIssue(projectOrRepo, summary, issueType, description, labels)
	if err != nil {
		return "", fmt.Errorf("creating local issue in %s: %w", projectOrRepo, err)
	}

	return key, nil
}

// AddComment posts a comment against the issue.
func (t *Tracker) AddComment(key, text string) error {
	if err := t.store.InsertLocalIssueComment(key, text); err != nil {
		return fmt.Errorf("adding comment to local issue %s: %w", key, err)
	}

	return nil
}

// SetStatus updates the issue's status category.
func (t *Tracker) SetStatus(key string, target tasktracker.StatusCategory) error {
	if err := t.store.SetLocalIssueStatus(key, string(target)); err != nil {
		return fmt.Errorf("setting status for local issue %s: %w", key, err)
	}

	return nil
}

// GetIssue resolves key to its current normalized state.
func (t *Tracker) GetIssue(key string) (tasktracker.Issue, error) {
	issue, err := t.store.GetLocalIssue(key)
	if err != nil {
		return tasktracker.Issue{}, fmt.Errorf("getting local issue %s: %w", key, err)
	}

	return toIssue(issue), nil
}

// SearchIssues treats query as an optional case-insensitive substring match
// against an issue's summary — a deliberate simplification, not portable
// query-language parity with JQL/GitHub search syntax.
func (t *Tracker) SearchIssues(query string, limit int) ([]tasktracker.Issue, error) {
	issues, err := t.store.SearchLocalIssues(query, limit)
	if err != nil {
		return nil, fmt.Errorf("searching local issues for %q: %w", query, err)
	}

	out := make([]tasktracker.Issue, len(issues))
	for i, issue := range issues {
		out[i] = toIssue(issue)
	}

	return out, nil
}

// WorkflowGraph returns a hardcoded, static todo -> in_progress -> done
// graph — no store lookup, no mining. The local backend has no admin-
// configurable workflow to mine; this is the same static-graph disposition
// GitHub Issues' open/closed model would take.
func (t *Tracker) WorkflowGraph(_ string) (*workflow.Graph, error) {
	g := workflow.NewGraph()
	g.AddStatus("To Do", string(tasktracker.StatusTodo))
	g.AddStatus("In Progress", string(tasktracker.StatusInProgress))
	g.AddStatus("Done", string(tasktracker.StatusDone))
	g.Observe("To Do", "In Progress")
	g.Observe("In Progress", "Done")

	return g, nil
}

func toIssue(issue store.LocalIssue) tasktracker.Issue {
	return tasktracker.Issue{
		Key:            issue.Key,
		Summary:        issue.Summary,
		StatusCategory: tasktracker.StatusCategory(issue.StatusCategory),
		StatusName:     issue.StatusCategory,
		Labels:         issue.Labels,
	}
}
