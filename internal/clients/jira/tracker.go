package jira

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

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

// SelfIdentity is the authenticated account's accountId, which the Jira collector
// compares an author against to tag unjira's own writes (tasktracker.SelfIdentifier).
// An account that cannot read itself is an error: an empty identity would tag nothing.
func (t *Tracker) SelfIdentity() (string, error) {
	me, err := t.client.Myself()
	if err != nil {
		return "", fmt.Errorf("reading own jira account: %w", err)
	}

	id, _ := me["accountId"].(string)
	if id == "" {
		return "", fmt.Errorf("reading own jira account: the response has no accountId")
	}

	return id, nil
}

// NewTracker returns a Tracker wrapping client.
func NewTracker(client *Client) *Tracker {
	return &Tracker{client: client}
}

var (
	_ tasktracker.TaskTracker   = (*Tracker)(nil)
	_ workflow.GraphProvider    = (*Tracker)(nil)
	_ tasktracker.WorklogWriter = (*Tracker)(nil)
)

// worklogPropertyKey is the worklog entity property unjira's attribution lives under.
const worklogPropertyKey = "unjira.attribution"

// FindWorklog finds the worklog on key whose attribution property carries
// idempotencyKey. Another unjira worklog whose property lists any of messageIDs is an
// error: Jira already holds that time under a different key (an earlier add whose
// marking failed), and a second POST would log it twice. A property value that is not a
// JSON object, or carries no idempotency key, is skipped: another tool may write the key.
func (t *Tracker) FindWorklog(key, idempotencyKey string, messageIDs []string) (string, bool, error) {
	worklogs, err := t.client.ListWorklogs(key)
	if err != nil {
		return "", false, fmt.Errorf("listing worklogs on jira issue %s: %w", key, err)
	}

	var foundID string
	var overlaps []string
	for _, wl := range worklogs {
		for _, p := range wl.Properties {
			if p.Key != worklogPropertyKey {
				continue
			}

			var v struct {
				IdempotencyKey string   `json:"idempotency_key"`
				MessageIDs     []string `json:"message_ids"`
			}
			if json.Unmarshal(p.Value, &v) != nil || v.IdempotencyKey == "" {
				continue
			}

			if v.IdempotencyKey == idempotencyKey {
				foundID = wl.ID

				continue
			}
			if slices.ContainsFunc(v.MessageIDs, func(id string) bool { return slices.Contains(messageIDs, id) }) {
				overlaps = append(overlaps, wl.ID)
			}
		}
	}

	if len(overlaps) > 0 {
		return "", false, fmt.Errorf("worklog %s on jira issue %s already logs some of these messages under another key; "+
			"record its messages or delete it before re-proposing", strings.Join(overlaps, ", "), key)
	}

	return foundID, foundID != "", nil
}

func worklogInput(e tasktracker.WorklogEntry) WorklogInput {
	in := WorklogInput{Started: e.Started, TimeSpentSeconds: e.TimeSpentSeconds, Comment: e.Comment}
	if len(e.Property) > 0 {
		in.Properties = []EntityProperty{{Key: worklogPropertyKey, Value: e.Property}}
	}

	return in
}

// AddWorklog adds a worklog carrying unjira's attribution property.
func (t *Tracker) AddWorklog(key string, e tasktracker.WorklogEntry) (string, error) {
	id, err := t.client.AddWorklog(key, worklogInput(e))
	if err != nil {
		return "", fmt.Errorf("adding worklog to jira issue %s: %w", key, err)
	}

	return id, nil
}

// UpdateWorklog replaces a worklog's fields and its attribution property.
func (t *Tracker) UpdateWorklog(key, id string, e tasktracker.WorklogEntry) error {
	if err := t.client.UpdateWorklog(key, id, worklogInput(e)); err != nil {
		return fmt.Errorf("updating worklog %s on jira issue %s: %w", id, key, err)
	}

	return nil
}

// DeleteWorklog deletes a worklog, and returns nil once it is absent. A 404 is
// checked against the issue's worklogs: it counts as done only if the issue lists
// and the id is not on it, so a re-applied delete succeeds and a wrong id does not.
func (t *Tracker) DeleteWorklog(key, id string) error {
	err := t.client.DeleteWorklog(key, id)
	if err == nil {
		return nil
	}

	if jerr, ok := errors.AsType[*Error](err); !ok || jerr.Status != http.StatusNotFound {
		return fmt.Errorf("deleting worklog %s on jira issue %s: %w", id, key, err)
	}

	worklogs, lerr := t.client.ListWorklogs(key)
	if lerr != nil {
		return fmt.Errorf("deleting worklog %s on jira issue %s: delete answered 404 and listing worklogs failed: %w", id, key, lerr)
	}

	if slices.ContainsFunc(worklogs, func(wl Worklog) bool { return wl.ID == id }) {
		return fmt.Errorf("deleting worklog %s on jira issue %s: delete answered 404 but the issue still lists it: %w", id, key, err)
	}

	return nil
}

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
// whether key was recognized. Callers must check ok rather than fold an
// unrecognized key into some bucket: StatusTodo would read as a verified claim
// about a status Jira described with a category this package has never seen.
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

	// Flattened rather than type-asserted: Jira Cloud v3 returns ADF here, and a bare
	// `.(string)` yielded "" for every such issue — silently. Issue.Description feeds
	// the matching prompt (correlator/match.go), and it exists because a one-line
	// summary often cannot distinguish the ticket a narrative implements from one it
	// merely mentions, so emptying it degraded the comparison it was added for.
	issue.Description = adfText(fields["description"])

	if status, ok := fields["status"].(map[string]any); ok {
		issue.StatusName, _ = status["name"].(string)
		if category, ok := status["statusCategory"].(map[string]any); ok {
			categoryKey, _ := category["key"].(string)
			issue.StatusCategory = mustNormalizeStatusCategory(categoryKey)
		}
	}

	issue.Resolved = issueTime(fields["resolutiondate"])
	issue.Updated = issueTime(fields["updated"])

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

// issueTimeFormat is Jira's timestamp format, as GET /issue returns resolutiondate
// and updated: 2025-03-04T10:15:30.000+0000.
const issueTimeFormat = "2006-01-02T15:04:05.000-0700"

// issueTime reads one of an issue's timestamp fields. Null (an unresolved issue's
// resolutiondate), absent, or unparseable is the zero time, which tasktracker.Issue
// defines as "the backend did not say": like normalizeIssue's other fields, a
// best-effort snapshot that degrades to unknown rather than failing a read or
// guessing a date.
func issueTime(raw any) time.Time {
	s, ok := raw.(string)
	if !ok {
		return time.Time{}
	}

	at, err := time.Parse(issueTimeFormat, s)
	if err != nil {
		return time.Time{}
	}

	return at
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

// SetStatus finds the transition landing on the named target status and
// executes it. Errors loudly rather than guessing when nothing lands there.
//
// Matched on the destination's NAME, not its category. Jira routinely offers
// several transitions whose destinations share a category — in the PAAS project,
// In Progress, In Review, Blocked, and In Test are all indeterminate — so a
// category match would execute whichever of them the API happened to list first.
// That is a wrong write, not a near miss: "move to In Review" would sometimes
// block the ticket.
//
// Comparison is case-insensitive on a trimmed name, since the target travels
// through a persisted JSON payload and a model's response before arriving here,
// and "in review" versus "In Review" is not a distinction worth refusing a
// legitimate transition over. It is not fuzzy beyond that: no prefix matching,
// no closest-match. An unresolvable name means the workflow changed or the
// status was never real, and both deserve the error.
func (t *Tracker) SetStatus(key, targetStatus string) error {
	transitions, err := t.client.GetTransitions(key)
	if err != nil {
		return fmt.Errorf("fetching transitions for jira issue %s: %w", key, err)
	}

	want := normalizeStatusName(targetStatus)

	available := make([]string, 0, len(transitions))

	for _, transition := range transitions {
		to, ok := transition["to"].(map[string]any)
		if !ok {
			continue
		}
		name, _ := to["name"].(string)
		if name == "" {
			continue
		}
		available = append(available, name)

		if normalizeStatusName(name) != want {
			continue
		}

		transitionID, _ := transition["id"].(string)
		if err := t.client.TransitionIssue(key, transitionID, nil); err != nil {
			return fmt.Errorf("transitioning jira issue %s to %q: %w", key, name, err)
		}

		return nil
	}

	// Naming what WAS available turns an opaque refusal into a diagnosis: a
	// typo, a workflow edit, and someone else having already moved the issue
	// look identical without it.
	return fmt.Errorf(
		"no available transition for jira issue %s lands on status %q (available: %s)",
		key, targetStatus, strings.Join(available, ", "),
	)
}

// AvailableTransitions reports every destination the issue can currently move
// to, by name, with its normalized category alongside.
//
// Deduplicated by name, not by category: two transitions to the same named
// status are one destination, but two transitions to differently-named statuses
// are two destinations even when their categories match. Deduplicating by
// category is what the previous StatusCategory-based version did, and it
// collapsed PAAS's four indeterminate destinations into one.
//
// A destination whose category Jira reports but this package doesn't recognize
// is still returned, with an empty ToCategory. That is deliberate and is a
// change from the category-only version, which dropped it: the name is what
// authorizes a transition, and it was verified. Dropping the destination made
// three real PAAS statuses (Open, Paused, To Do — all `unknown` category)
// invisible to the reconciler, which is a false NEGATIVE suppressing a legal
// move. An empty category only weakens the coarse direction check, which is
// advisory.
func (t *Tracker) AvailableTransitions(key string) ([]tasktracker.Transition, error) {
	transitions, err := t.client.GetTransitions(key)
	if err != nil {
		return nil, fmt.Errorf("fetching transitions for jira issue %s: %w", key, err)
	}

	seen := make(map[string]bool, len(transitions))
	out := make([]tasktracker.Transition, 0, len(transitions))

	for _, transition := range transitions {
		to, ok := transition["to"].(map[string]any)
		if !ok {
			continue
		}

		name, _ := to["name"].(string)
		if name == "" || seen[normalizeStatusName(name)] {
			continue
		}
		seen[normalizeStatusName(name)] = true

		// An unrecognized or absent category leaves ToCategory empty rather
		// than defaulting: StatusTodo would read as a real claim about
		// direction that nothing verified.
		var normalized tasktracker.StatusCategory
		if category, ok := to["statusCategory"].(map[string]any); ok {
			categoryKey, _ := category["key"].(string)
			normalized, _ = normalizedStatusCategory(categoryKey)
		}

		out = append(out, tasktracker.Transition{ToStatus: name, ToCategory: normalized})
	}

	return out, nil
}

// normalizeStatusName folds a status name for comparison: trimmed and
// case-insensitive. Jira treats status names as display strings and does not
// guarantee the casing a caller saw earlier is the casing it will report later.
func normalizeStatusName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// CreateIssue creates an issue and returns its key.
func (t *Tracker) CreateIssue(projectOrRepo, summary, issueType, description string, labels []string) (string, error) {
	key, err := t.client.CreateIssue(projectOrRepo, summary, issueType, description, labels)
	if err != nil {
		return "", fmt.Errorf("creating jira issue in %s: %w", projectOrRepo, err)
	}

	return key, nil
}
