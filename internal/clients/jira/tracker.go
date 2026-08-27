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
// normalized StatusCategory. Jira Cloud defines only these three keys today,
// but team-managed projects, marketplace apps, or a future API version could
// introduce more — callers must decide for themselves how to treat a key
// this map doesn't recognize (see normalizedStatusCategory and
// mustNormalizeStatusCategory below).
var jiraStatusCategories = map[string]tasktracker.StatusCategory{
	"new":           tasktracker.StatusTodo,
	"indeterminate": tasktracker.StatusInProgress,
	"done":          tasktracker.StatusDone,
}

// normalizedStatusCategory maps key to its normalized bucket, reporting
// whether key was recognized. Legality-facing callers (SetStatus,
// AvailableStatusCategories) must check ok themselves rather than fold an
// unrecognized key into some bucket: doing so would let unjira report a
// category reachable — or, in SetStatus, actually execute a transition into
// one — that Jira never actually offered.
func normalizedStatusCategory(key string) (tasktracker.StatusCategory, bool) {
	category, ok := jiraStatusCategories[key]

	return category, ok
}

// mustNormalizeStatusCategory is normalizedStatusCategory for the read path
// (normalizeIssue), where an unrecognized key (including "unknown", the
// Client's own fallback for a missing category) deliberately degrades to
// StatusTodo, the least presumptive bucket, rather than erroring — Issue is
// a best-effort snapshot, not a gate on a write.
func mustNormalizeStatusCategory(key string) tasktracker.StatusCategory {
	if category, ok := normalizedStatusCategory(key); ok {
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

	// A failed assertion leaves this empty, which is the intended degradation
	// for an ADF object (Jira Cloud v3) rather than a string.
	issue.Description, _ = fields["description"].(string)

	if status, ok := fields["status"].(map[string]any); ok {
		issue.StatusName, _ = status["name"].(string)
		if category, ok := status["statusCategory"].(map[string]any); ok {
			categoryKey, _ := category["key"].(string)
			issue.StatusCategory = mustNormalizeStatusCategory(categoryKey)
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
// transition lands in the target category, rather than guessing. A
// transition into a status category Jira reports but this package doesn't
// recognize never matches any target, StatusTodo included — an unrecognized
// category must never be treated as equivalent to a caller's request, since
// that would execute a transition unjira never actually verified.
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

		normalized, ok := normalizedStatusCategory(categoryKey)
		if !ok || normalized != target {
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

// AvailableStatusCategories maps the issue's currently-legal transitions to
// normalized categories, deduplicated (several named transitions routinely
// land in the same category). A transition into a status category this
// package doesn't recognize is dropped rather than folded into StatusTodo:
// this method's whole purpose is telling the reconciler which categories
// are actually reachable, and reporting one that was never verified would
// be a false positive licensing an unverified write.
func (t *Tracker) AvailableStatusCategories(key string) ([]tasktracker.StatusCategory, error) {
	transitions, err := t.client.GetTransitions(key)
	if err != nil {
		return nil, fmt.Errorf("fetching transitions for jira issue %s: %w", key, err)
	}

	seen := make(map[tasktracker.StatusCategory]bool, len(transitions))
	var out []tasktracker.StatusCategory

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

		normalized, ok := normalizedStatusCategory(categoryKey)
		if !ok || seen[normalized] {
			continue
		}
		seen[normalized] = true
		out = append(out, normalized)
	}

	return out, nil
}

// CreateIssue creates an issue and returns its key.
func (t *Tracker) CreateIssue(projectOrRepo, summary, issueType, description string, labels []string) (string, error) {
	key, err := t.client.CreateIssue(projectOrRepo, summary, issueType, description, labels)
	if err != nil {
		return "", fmt.Errorf("creating jira issue in %s: %w", projectOrRepo, err)
	}

	return key, nil
}
