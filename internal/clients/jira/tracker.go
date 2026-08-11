package jira

import (
	"fmt"

	"github.com/jcogilvie/unjira/internal/tasktracker"
	"github.com/jcogilvie/unjira/internal/workflow"
)

// Tracker adapts Client to tasktracker.TaskTracker, translating Jira's raw
// API shape into unjira's normalized types. Kept separate from Client
// (which stays a thin, no-business-logic facade per this package's own
// doc) because interpreting Jira's field shape into tasktracker.Issue is
// business logic.
type Tracker struct {
	client *Client
}

// NewTracker returns a Tracker wrapping client.
func NewTracker(client *Client) *Tracker {
	return &Tracker{client: client}
}

var (
	_ tasktracker.TaskTracker = (*Tracker)(nil)
	_ workflow.GraphProvider  = (*Tracker)(nil)
)

// defaultMaxIssues bounds how many issues WorkflowGraph mines changelogs
// from when no caller-specified limit exists yet (nothing above this layer
// calls it today).
const defaultMaxIssues = 200

// WorkflowGraph mines the observed transition graph for projectKey via the
// existing changelog-mining machinery (workflow.MineProject) — Jira's
// workflows are admin-configurable with no static answer, unlike GitHub
// Issues' fixed open/closed model or the local backend's static graph.
func (t *Tracker) WorkflowGraph(projectKey string) (*workflow.Graph, error) {
	return workflow.MineProject(t.client, projectKey, defaultMaxIssues)
}

// jiraStatusCategories map Jira's statusCategory.key values to unjira's
// normalized StatusCategory. Any unrecognized key (including "unknown", the
// Client's own fallback for a missing category) maps to StatusTodo, the
// least presumptive bucket.
var jiraStatusCategories = map[string]tasktracker.StatusCategory{
	"new":           tasktracker.StatusTodo,
	"indeterminate": tasktracker.StatusInProgress,
	"done":          tasktracker.StatusDone,
}

func normalizedStatusCategory(key string) tasktracker.StatusCategory {
	if category, ok := jiraStatusCategories[key]; ok {
		return category
	}

	return tasktracker.StatusTodo
}

// normalizeIssue extracts the fields tasktracker.Issue needs from a Jira
// issue's raw map[string]any shape (as returned by Client.GetIssue and
// Client.SearchIssues).
func normalizeIssue(raw map[string]any) tasktracker.Issue {
	key, _ := raw["key"].(string)
	fields, _ := raw["fields"].(map[string]any)

	issue := tasktracker.Issue{Key: key}

	if fields == nil {
		return issue
	}

	issue.Summary, _ = fields["summary"].(string)

	if status, ok := fields["status"].(map[string]any); ok {
		issue.StatusName, _ = status["name"].(string)
		if category, ok := status["statusCategory"].(map[string]any); ok {
			categoryKey, _ := category["key"].(string)
			issue.StatusCategory = normalizedStatusCategory(categoryKey)
		}
	}

	if rawLabels, ok := fields["labels"].([]string); ok {
		issue.Labels = rawLabels
	} else if rawLabels, ok := fields["labels"].([]any); ok {
		labels := make([]string, 0, len(rawLabels))
		for _, l := range rawLabels {
			if s, ok := l.(string); ok {
				labels = append(labels, s)
			}
		}
		issue.Labels = labels
	}

	return issue
}

// GetIssue resolves key to its current normalized state.
func (t *Tracker) GetIssue(key string) (tasktracker.Issue, error) {
	raw, err := t.client.GetIssue(key, "")
	if err != nil {
		return tasktracker.Issue{}, fmt.Errorf("getting jira issue %s: %w", key, err)
	}

	return normalizeIssue(raw), nil
}

// SearchIssues runs a JQL query and normalizes every hit, up to limit.
func (t *Tracker) SearchIssues(query string, limit int) ([]tasktracker.Issue, error) {
	var issues []tasktracker.Issue

	err := t.client.SearchIssues(query, nil, limit, func(raw map[string]any) {
		issues = append(issues, normalizeIssue(raw))
	})
	if err != nil {
		return nil, fmt.Errorf("searching jira issues for %q: %w", query, err)
	}

	return issues, nil
}

// AddComment posts a comment to the issue.
func (t *Tracker) AddComment(key, text string) error {
	if _, err := t.client.AddComment(key, text); err != nil {
		return fmt.Errorf("adding comment to jira issue %s: %w", key, err)
	}

	return nil
}

// SetStatus resolves target to a legal transition (one landing in that
// status category) and executes it. Errors loudly if no available
// transition lands in the target category, rather than guessing.
func (t *Tracker) SetStatus(key string, target tasktracker.StatusCategory) error {
	transitions, err := t.client.GetTransitions(key)
	if err != nil {
		return fmt.Errorf("fetching transitions for jira issue %s: %w", key, err)
	}

	for _, transition := range transitions {
		to, ok := transition["to"].(map[string]any)
		if !ok {
			continue
		}
		category, ok := to["statusCategory"].(map[string]any)
		if !ok {
			continue
		}
		categoryKey, _ := category["key"].(string)
		if normalizedStatusCategory(categoryKey) != target {
			continue
		}

		transitionID, _ := transition["id"].(string)
		if err := t.client.TransitionIssue(key, transitionID, nil); err != nil {
			return fmt.Errorf("transitioning jira issue %s: %w", key, err)
		}

		return nil
	}

	return fmt.Errorf("no available transition for jira issue %s lands in status category %q", key, target)
}

// CreateIssue creates an issue and returns its key.
func (t *Tracker) CreateIssue(projectOrRepo, summary, issueType, description string, labels []string) (string, error) {
	key, err := t.client.CreateIssue(projectOrRepo, summary, issueType, description, labels)
	if err != nil {
		return "", fmt.Errorf("creating jira issue in %s: %w", projectOrRepo, err)
	}

	return key, nil
}
