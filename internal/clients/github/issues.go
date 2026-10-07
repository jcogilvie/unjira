package github

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Issue is the subset of GitHub's issue JSON shape the reader uses.
type Issue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	State  string `json:"state"` // "open" | "closed"
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	// PullRequest is present when the issues endpoint returned a pull request, which
	// it does: GitHub models every pull request as an issue too.
	PullRequest   *struct{} `json:"pull_request"`
	RepositoryURL string    `json:"repository_url"`
	// ClosedAt is nil for an open issue.
	ClosedAt  *time.Time `json:"closed_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// IsPullRequest reports whether this "issue" is a pull request.
func (i Issue) IsPullRequest() bool { return i.PullRequest != nil }

// GetIssue reads issue number in ref: GET /repos/{owner}/{repo}/issues/{n}.
func (c *Client) GetIssue(ref RepoRef, number int) (Issue, error) {
	var issue Issue
	if err := c.get(fmt.Sprintf("/repos/%s/issues/%d", ref.OwnerRepo(), number), &issue); err != nil {
		return Issue{}, err
	}

	return issue, nil
}

// SearchIssues runs query through GET /search/issues, returning at most limit hits from
// the first page (GitHub caps a page at 100).
func (c *Client) SearchIssues(query string, limit int) ([]Issue, error) {
	n := min(max(limit, 1), perPage)

	params := url.Values{"q": {query}, "per_page": {fmt.Sprint(n)}}

	var result struct {
		Items []Issue `json:"items"`
	}
	if err := c.get("/search/issues?"+params.Encode(), &result); err != nil {
		return nil, err
	}

	return result.Items, nil
}

// OwnerRepo reads owner/repo off the issue's repository_url
// (https://api.github.com/repos/<owner>/<repo>), which a search hit carries in place of
// the repository it was asked about.
func (i Issue) OwnerRepo() (string, error) {
	_, rest, ok := strings.Cut(i.RepositoryURL, "/repos/")
	if !ok || strings.Count(rest, "/") != 1 {
		return "", fmt.Errorf("issue #%d: repository_url %q does not name owner/repo", i.Number, i.RepositoryURL)
	}

	return rest, nil
}
