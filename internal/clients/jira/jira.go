// Package jira is a thin facade over go-jira.
//
// The facade is the seam. Everything above it (collectors, workflow mining,
// devtools, the future applier) sees this stable, minimal surface; the
// community library underneath absorbs Jira Cloud API churn. If upstream
// ever rots, reimplementing this surface directly is deliberately small.
//
// Design rules that survive the library swap:
//   - Read methods are safe everywhere. Write methods exist as client
//     capability; authorization to call them lives in the pipeline (review
//     queue, autonomy graduation), never here.
//   - Per-issue transitions are the ground truth for legal moves at
//     execution time; the mined workflow graph is only for planning.
package jira

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	jiracloud "github.com/andygrunwald/go-jira/v2/cloud"
)

// SeedLabel marks issues created by unjira's dev seed tooling.
const SeedLabel = "unjira-seed"

// Error wraps a Jira API error with its HTTP status code.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("Jira API %d: %s", e.Status, e.Message)
}

// Client is a facade over go-jira, exposing only the surface unjira needs.
type Client struct {
	upstream *jiracloud.Client
}

// New constructs a Client for the given site, authenticating with email and
// an API token.
func New(site, email, token string) (*Client, error) {
	if site == "" {
		return nil, fmt.Errorf("jira site URL is required")
	}

	transport := jiracloud.BasicAuthTransport{Username: email, APIToken: token}
	upstream, err := jiracloud.NewClient(site, transport.Client())
	if err != nil {
		return nil, fmt.Errorf("constructing jira client for %s: %w", site, err)
	}

	return &Client{upstream: upstream}, nil
}

// Myself returns the authenticated user.
func (c *Client) Myself() (map[string]any, error) {
	var result map[string]any
	if err := c.do(http.MethodGet, "rest/api/2/myself", nil, &result); err != nil {
		return nil, err
	}

	return result, nil
}

// SearchProjects returns every project visible to the authenticated user.
func (c *Client) SearchProjects() ([]map[string]any, error) {
	var result []map[string]any
	if err := c.do(http.MethodGet, "rest/api/2/project", nil, &result); err != nil {
		return nil, err
	}

	return result, nil
}

// ProjectStatuses returns status name -> status category key, across all
// issue types in the project.
func (c *Client) ProjectStatuses(projectKey string) (map[string]string, error) {
	var issueTypes []struct {
		Statuses []struct {
			Name           string `json:"name"`
			StatusCategory struct {
				Key string `json:"key"`
			} `json:"statusCategory"`
		} `json:"statuses"`
	}

	path := fmt.Sprintf("rest/api/2/project/%s/statuses", projectKey)
	if err := c.do(http.MethodGet, path, nil, &issueTypes); err != nil {
		return nil, err
	}

	statuses := make(map[string]string)
	for _, issueType := range issueTypes {
		for _, status := range issueType.Statuses {
			category := status.StatusCategory.Key
			if category == "" {
				category = "unknown"
			}
			statuses[status.Name] = category
		}
	}

	return statuses, nil
}

// searchPage is the raw shape of one /search/jql response page.
type searchPage struct {
	Issues        []map[string]any `json:"issues"`
	NextPageToken string           `json:"nextPageToken"`
}

// SearchIssues walks every page of a JQL search (via /search/jql), calling
// visit for each issue, up to limit results.
func (c *Client) SearchIssues(jql string, fields []string, limit int, visit func(map[string]any)) error {
	fieldParam := "*all"
	if len(fields) > 0 {
		fieldParam = joinComma(fields)
	}

	var token string
	yielded := 0

	for {
		pageSize := limit - yielded
		if pageSize > 100 {
			pageSize = 100
		}

		path := fmt.Sprintf(
			"rest/api/3/search/jql?jql=%s&fields=%s&maxResults=%d",
			urlQueryEscape(jql), urlQueryEscape(fieldParam), pageSize,
		)
		if token != "" {
			path += "&nextPageToken=" + urlQueryEscape(token)
		}

		var page searchPage
		if err := c.do(http.MethodGet, path, nil, &page); err != nil {
			return err
		}

		for _, issue := range page.Issues {
			visit(issue)
			yielded++
			if yielded >= limit {
				return nil
			}
		}

		if page.NextPageToken == "" {
			return nil
		}
		token = page.NextPageToken
	}
}

// GetIssue fetches a single issue by key.
func (c *Client) GetIssue(key, expand string) (map[string]any, error) {
	path := fmt.Sprintf("rest/api/2/issue/%s", key)
	if expand != "" {
		path += "?expand=" + urlQueryEscape(expand)
	}

	var result map[string]any
	if err := c.do(http.MethodGet, path, nil, &result); err != nil {
		return nil, err
	}

	return result, nil
}

// changelogPage is the raw shape of one changelog response page.
type changelogPage struct {
	Values     []map[string]any `json:"values"`
	IsLast     bool             `json:"isLast"`
	MaxResults int              `json:"maxResults"`
}

// GetChangelog returns every changelog entry for an issue, oldest first.
func (c *Client) GetChangelog(key string) ([]map[string]any, error) {
	var entries []map[string]any
	start := 0

	for {
		path := fmt.Sprintf("rest/api/2/issue/%s/changelog?startAt=%d&maxResults=100", key, start)

		var page changelogPage
		if err := c.do(http.MethodGet, path, nil, &page); err != nil {
			return nil, err
		}

		entries = append(entries, page.Values...)
		if page.IsLast {
			return entries, nil
		}

		maxResults := page.MaxResults
		if maxResults == 0 {
			maxResults = 100
		}
		start += maxResults
	}
}

// StatusChange is a single (from, to) status transition observed in an
// issue's changelog.
type StatusChange struct {
	From string
	To   string
}

// StatusChanges returns the (from, to) status pairs from an issue's
// changelog, oldest first.
func (c *Client) StatusChanges(key string) ([]StatusChange, error) {
	entries, err := c.GetChangelog(key)
	if err != nil {
		return nil, err
	}

	var changes []StatusChange
	for _, entry := range entries {
		items, _ := entry["items"].([]any)
		for _, rawItem := range items {
			item, ok := rawItem.(map[string]any)
			if !ok || item["field"] != "status" {
				continue
			}
			changes = append(changes, StatusChange{
				From: stringOr(item["fromString"], "?"),
				To:   stringOr(item["toString"], "?"),
			})
		}
	}

	return changes, nil
}

// GetTransitions returns the raw available transitions for an issue (API
// shape: id, name, to.name, to.statusCategory, ...).
func (c *Client) GetTransitions(key string) ([]map[string]any, error) {
	var result struct {
		Transitions []map[string]any `json:"transitions"`
	}

	path := fmt.Sprintf("rest/api/2/issue/%s/transitions", key)
	if err := c.do(http.MethodGet, path, nil, &result); err != nil {
		return nil, err
	}

	return result.Transitions, nil
}

// -- writes (pipeline gates authorization; see package docstring) ----------

// CreateIssue creates an issue and returns its key.
func (c *Client) CreateIssue(projectKey, summary, issueType, description string, labels []string) (string, error) {
	fields := map[string]any{
		"project":   map[string]string{"key": projectKey},
		"summary":   summary,
		"issuetype": map[string]string{"name": issueType},
	}
	if description != "" {
		fields["description"] = description
	}
	if len(labels) > 0 {
		fields["labels"] = labels
	}

	var result struct {
		Key string `json:"key"`
	}
	if err := c.do(http.MethodPost, "rest/api/2/issue", map[string]any{"fields": fields}, &result); err != nil {
		return "", err
	}

	return result.Key, nil
}

// TransitionIssue executes a transition on an issue, optionally setting
// fields as part of the transition.
func (c *Client) TransitionIssue(key, transitionID string, fields map[string]any) error {
	payload := map[string]any{
		"transition": map[string]string{"id": transitionID},
	}
	if fields != nil {
		payload["fields"] = fields
	}

	path := fmt.Sprintf("rest/api/2/issue/%s/transitions", key)
	return c.do(http.MethodPost, path, payload, nil)
}

// AddComment adds a comment to an issue.
func (c *Client) AddComment(key, text string) (map[string]any, error) {
	var result map[string]any
	path := fmt.Sprintf("rest/api/2/issue/%s/comment", key)
	if err := c.do(http.MethodPost, path, map[string]any{"body": text}, &result); err != nil {
		return nil, err
	}

	return result, nil
}

// DeleteIssue deletes an issue by key.
func (c *Client) DeleteIssue(key string) error {
	path := fmt.Sprintf("rest/api/2/issue/%s", key)
	return c.do(http.MethodDelete, path, nil, nil)
}

// do performs a request against the upstream client, translating HTTP
// errors into *Error.
func (c *Client) do(method, path string, body, result any) error {
	req, err := c.upstream.NewRequest(context.Background(), method, path, body)
	if err != nil {
		return fmt.Errorf("building %s %s: %w", method, path, err)
	}

	resp, err := c.upstream.Do(req, result)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}

		return &Error{Status: status, Message: errorMessage(resp, err)}
	}

	return nil
}

// errorMessage extracts the Jira API's structured error messages from the
// response body. go-jira's Do() does not do this automatically — it must be
// constructed explicitly via NewJiraError, which re-reads the response body
// even after Do() has consumed it.
func errorMessage(resp *jiracloud.Response, err error) string {
	if resp == nil {
		return err.Error()
	}

	jerr := jiracloud.NewJiraError(resp, err)
	type jiraErrorLike interface {
		LongError() string
	}
	if withLong, ok := jerr.(jiraErrorLike); ok {
		return withLong.LongError()
	}

	return jerr.Error()
}

func stringOr(v any, fallback string) string {
	if s, ok := v.(string); ok {
		return s
	}

	return fallback
}

func joinComma(ss []string) string {
	out := ss[0]
	for _, s := range ss[1:] {
		out += "," + s
	}

	return out
}

func urlQueryEscape(s string) string {
	return url.QueryEscape(s)
}
