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

// The local backend's three canonical status names — one per normalized
// category, and the same names its static WorkflowGraph observes.
//
// Names rather than category strings ("To Do", not "todo") because a status is a
// name on every real backend, and a double whose statuses were spelled like
// categories would let a bug that confuses the two pass offline and fail live.
// LocalStatusTodo matches the local_issues.status schema default, so a freshly
// created issue is already at a status this list contains.
const (
	LocalStatusTodo       = "To Do"
	LocalStatusInProgress = "In Progress"
	LocalStatusDone       = "Done"
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

// SetStatus records the named target status verbatim.
//
// Accepts any name, including one AvailableTransitions did not offer. The local
// backend has no workflow to violate, and refusing an unlisted name would make
// this stricter than the thing it doubles: on a real backend, legality is
// whatever the live per-issue read says, and a test that could not set an
// arbitrary status could not exercise the reconciler against a workflow that
// does not resemble this one.
func (t *Tracker) SetStatus(key, targetStatus string) error {
	if err := t.store.SetLocalIssueStatus(key, targetStatus); err != nil {
		return fmt.Errorf("setting status for local issue %s: %w", key, err)
	}

	return nil
}

// AvailableTransitions reports the three canonical status names, one per
// category, matching the static graph WorkflowGraph returns.
//
// The local backend has no workflow restrictions, so a complete answer is the
// honest one rather than a stub. It is deliberately NOT the union of every
// status any real tracker might have: these three are the ones this backend's
// own graph observes, so its two methods agree with each other. Since SetStatus
// accepts any name regardless, a test needing PAAS-shaped statuses sets them
// directly rather than being constrained by this list.
func (t *Tracker) AvailableTransitions(_ string) ([]tasktracker.Transition, error) {
	return []tasktracker.Transition{
		{ToStatus: LocalStatusTodo, ToCategory: tasktracker.StatusTodo},
		{ToStatus: LocalStatusInProgress, ToCategory: tasktracker.StatusInProgress},
		{ToStatus: LocalStatusDone, ToCategory: tasktracker.StatusDone},
	}, nil
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
	g.AddStatus(LocalStatusTodo, string(tasktracker.StatusTodo))
	g.AddStatus(LocalStatusInProgress, string(tasktracker.StatusInProgress))
	g.AddStatus(LocalStatusDone, string(tasktracker.StatusDone))
	g.Observe(LocalStatusTodo, LocalStatusInProgress)
	g.Observe(LocalStatusInProgress, LocalStatusDone)

	return g, nil
}

func toIssue(issue store.LocalIssue) tasktracker.Issue {
	return tasktracker.Issue{
		Key:            issue.Key,
		Summary:        issue.Summary,
		StatusCategory: localStatusCategory(issue.Status),
		StatusName:     issue.Status,
		Labels:         issue.Labels,
		Description:    issue.Description,
	}
}

// localStatusCategory buckets one of this backend's three canonical status
// names.
//
// An unrecognized name yields the empty category rather than a guess. SetStatus
// accepts any name, so an arbitrary status genuinely has no category here — and
// StatusTodo would be a claim about direction that nothing established. The
// empty value weakens only the coarse direction check, which is advisory;
// StatusName still carries the truth.
func localStatusCategory(name string) tasktracker.StatusCategory {
	switch name {
	case LocalStatusTodo:
		return tasktracker.StatusTodo
	case LocalStatusInProgress:
		return tasktracker.StatusInProgress
	case LocalStatusDone:
		return tasktracker.StatusDone
	default:
		return ""
	}
}
