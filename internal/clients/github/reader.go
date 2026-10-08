package github

import (
	"fmt"
	"strings"
	"time"

	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// Reader adapts a Client to tasktracker.TaskReader over GitHub Issues.
//
// It is a reader and nothing else. unjira never writes to a public tracker, and the way
// that is guaranteed is that no GitHub type implements tasktracker.TaskWriter: there is
// no write to gate, because there is no write. A GitHub writer is out of scope and would
// need its own design.
//
// Keys are <owner>/<repo>#<N> (tasktracker.ParseIssueKey). Issues only: GitHub serves
// pull requests from the issues endpoint too, and a pull request is work evidence, never
// the issue work is tracked in, so one reads as tasktracker.ErrNotFound.
type Reader struct {
	client *Client
}

// NewReader returns a Reader over client.
func NewReader(client *Client) *Reader {
	return &Reader{client: client}
}

// GetIssue resolves key to its current normalized state.
func (r *Reader) GetIssue(key string) (tasktracker.Issue, error) {
	ref, number, err := repoIssue(key)
	if err != nil {
		return tasktracker.Issue{}, err
	}

	issue, err := r.client.GetIssue(ref, number)
	if err != nil {
		return tasktracker.Issue{}, fmt.Errorf("getting github issue %s: %w", key, err)
	}

	if issue.IsPullRequest() {
		return tasktracker.Issue{}, fmt.Errorf(
			"github %s is a pull request, not an issue: %w", key, tasktracker.ErrNotFound)
	}

	return toIssue(key, issue), nil
}

// SearchIssues runs a GitHub search qualifier string, restricted to issues.
func (r *Reader) SearchIssues(query string, limit int) ([]tasktracker.Issue, error) {
	hits, err := r.client.SearchIssues(query+" is:issue", limit)
	if err != nil {
		return nil, fmt.Errorf("searching github issues for %q: %w", query, err)
	}

	out := make([]tasktracker.Issue, 0, len(hits))

	for _, hit := range hits {
		ownerRepo, err := hit.OwnerRepo()
		if err != nil {
			return nil, err
		}

		out = append(out, toIssue(fmt.Sprintf("%s#%d", ownerRepo, hit.Number), hit))
	}

	return out, nil
}

// AvailableTransitions offers nothing: unjira cannot move a GitHub issue, so no
// destination is legal for it, and the reconciler proposes no transition here.
func (r *Reader) AvailableTransitions(key string) ([]tasktracker.Transition, error) {
	if _, _, err := repoIssue(key); err != nil {
		return nil, err
	}

	return nil, nil
}

// repoIssue reads a GitHub key into the repository and number the API addresses.
func repoIssue(key string) (RepoRef, int, error) {
	parsed, err := tasktracker.ParseIssueKey(key)
	if err != nil {
		return RepoRef{}, 0, err
	}

	if parsed.Syntax != tasktracker.SyntaxRepo {
		return RepoRef{}, 0, fmt.Errorf("issue key %q is not a GitHub <owner>/<repo>#<N> key", key)
	}

	owner, repo, _ := strings.Cut(parsed.Scope, "/")

	return RepoRef{Host: DefaultHost, Owner: owner, Repo: repo}, parsed.Number, nil
}

// toIssue normalizes a GitHub issue. GitHub has two states and no in-progress one, so
// open is todo and closed is done; the state itself is the status name.
func toIssue(key string, issue Issue) tasktracker.Issue {
	category := tasktracker.StatusTodo
	if issue.State == "closed" {
		category = tasktracker.StatusDone
	}

	labels := make([]string, 0, len(issue.Labels))
	for _, l := range issue.Labels {
		labels = append(labels, l.Name)
	}

	var resolved time.Time
	if issue.ClosedAt != nil {
		resolved = *issue.ClosedAt
	}

	return tasktracker.Issue{
		Key:            key,
		Summary:        issue.Title,
		StatusCategory: category,
		StatusName:     issue.State,
		Labels:         labels,
		Description:    issue.Body,
		Resolved:       resolved,
		Updated:        issue.UpdatedAt,
	}
}
