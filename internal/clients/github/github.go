// Package github is a thin facade over GitHub's REST API, exposing only the
// surface unjira needs: listing pull requests and reading one PR's issue-events
// timeline for the collector, and reading issues for the read-only tracker
// (Reader). It sends GET requests and nothing else.
//
// No SDK. This slice needs one listing call plus one per-changed-PR follow-up
// call, both plain REST-over-JSON — encoding/json needs no library for a
// surface this small, unlike clients/jira's wrap of go-jira, which earns its
// keep against a large, churning API. The facade exists precisely so swapping
// in google/go-github later, if real pagination or secondary-rate-limit
// behaviour outgrows this, does not ripple past this package.
//
// Design: docs/superpowers/specs/2026-09-17-github-collector-design.md.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// perPage is the page size requested from GitHub's list endpoints. 100 is the
// documented maximum, minimizing the number of round trips per pass.
const perPage = 100

// Error wraps a GitHub API error with its HTTP status code, mirroring
// clients/jira.Error's shape: a status code and a typed body, not merely an
// exit code and a line of text — the whole reason this transport was chosen
// over shelling out to `gh` (see the design's §1).
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("github API %d: %s", e.Status, e.Message)
}

// IsTransport classifies this error for tasktracker.IsTransportError: true when GitHub
// was unreachable, failed, or throttled the request, false when it answered about the
// issue (404 Not Found, 410 Gone for a deleted or transferred one, or a 403 that is a
// permission answer).
//
// Status 0 means no HTTP response at all. GitHub throttles with 429 and, for its primary
// rate limit, with 403 and a "rate limit" message, so a 403 is transport only when it
// says so: a throttle says nothing about whether the issue exists.
func (e *Error) IsTransport() bool {
	switch {
	case e.Status == 0, e.Status == http.StatusTooManyRequests, e.Status >= http.StatusInternalServerError:
		return true
	case e.Status == http.StatusForbidden:
		return strings.Contains(strings.ToLower(e.Message), "rate limit")
	default:
		return false
	}
}

// Client is a facade over GitHub's REST API.
type Client struct {
	http  *http.Client
	base  string // e.g. https://api.github.com, or https://<ghes-host>/api/v3
	token string
}

// New constructs a Client against base (see BaseURL), authenticating with
// token via a Bearer Authorization header on every request.
//
// The retry transport is composed here, internally, the same way jira.New
// composes clients/jira's retryTransport around its auth transport: one
// decorator, installed once, covering every method this facade gains later.
// token may be empty at this layer — an absent or empty credential is the
// COLLECTOR's error to raise (naming the env var and the host it was looked
// up under), not this constructor's; a thin facade should not duplicate that
// check.
func New(base, token string) (*Client, error) {
	if base == "" {
		return nil, fmt.Errorf("github api base url is required")
	}

	httpClient := &http.Client{Timeout: clientTimeout}
	httpClient.Transport = newRetryTransport(http.DefaultTransport, nil)

	return &Client{
		http:  httpClient,
		base:  strings.TrimSuffix(base, "/"),
		token: token,
	}, nil
}

// PullRequest is the subset of GitHub's pull-request JSON shape this
// collector reads.
type PullRequest struct {
	Number    int        `json:"number"`
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	State     string     `json:"state"` // "open" | "closed"
	HTMLURL   string     `json:"html_url"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	MergedAt  *time.Time `json:"merged_at"`
	ClosedAt  *time.Time `json:"closed_at"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
	Head struct {
		Ref string `json:"ref"`
	} `json:"head"`
}

// IsClosed reports whether pr has left the open state (closed, whether or not
// it was merged).
func (pr PullRequest) IsClosed() bool {
	return pr.State == "closed"
}

// IssueEvent is one entry of a PR's issue-events timeline — the shape
// GET /repos/{owner}/{repo}/issues/{n}/events returns. Each state transition
// (opened, closed, reopened, merged, ...) carries its own immutable numeric
// ID, which is what lets a reopened-then-re-closed PR mint a second, distinct
// ExternalID rather than colliding with (and silently dropping via INSERT OR
// IGNORE) the first closure. Verified live against crossplane/crossplane#7535
// (closed id=31498552227, reopened id=31498554963 six seconds later) — see the
// design doc's decision 1.
type IssueEvent struct {
	ID        int64     `json:"id"`
	Event     string    `json:"event"` // "closed" | "merged" | "reopened" | ... (many more, ignored)
	CreatedAt time.Time `json:"created_at"`
}

// ListPullRequests returns every PR in ref whose UpdatedAt is at or after
// since, newest-updated first, paging until the first PR older than since (or
// the collection genuinely ends).
//
// Requests state=all&sort=updated&direction=desc explicitly: the endpoint's
// default order is by PR NUMBER, and an early-stop scan over unordered
// results would skip events silently — the same failure mode
// collector/jira's watermarkClause guards against for JQL's missing
// ORDER BY.
func (c *Client) ListPullRequests(ref RepoRef, since time.Time) ([]PullRequest, error) {
	var out []PullRequest

	for page := 1; ; page++ {
		path := fmt.Sprintf(
			"/repos/%s/pulls?state=all&sort=updated&direction=desc&per_page=%d&page=%d",
			ref.OwnerRepo(), perPage, page,
		)

		var batch []PullRequest
		if err := c.get(path, &batch); err != nil {
			return nil, err
		}

		stoppedEarly := false
		for _, pr := range batch {
			if pr.UpdatedAt.Before(since) {
				stoppedEarly = true
				break
			}
			out = append(out, pr)
		}

		if stoppedEarly || len(batch) < perPage {
			return out, nil
		}
	}
}

// ListIssueEvents returns every issue-event for PR number in ref, in the
// order GitHub returns them (oldest first, per GitHub's documented default).
func (c *Client) ListIssueEvents(ref RepoRef, number int) ([]IssueEvent, error) {
	var out []IssueEvent

	for page := 1; ; page++ {
		path := fmt.Sprintf(
			"/repos/%s/issues/%d/events?per_page=%d&page=%d",
			ref.OwnerRepo(), number, perPage, page,
		)

		var batch []IssueEvent
		if err := c.get(path, &batch); err != nil {
			return nil, err
		}

		out = append(out, batch...)
		if len(batch) < perPage {
			return out, nil
		}
	}
}

// AuthenticatedLogin returns the login of the user the client's token belongs to
// (GET /user), which the PR collector uses to decide whose PRs are this user's work.
func (c *Client) AuthenticatedLogin() (string, error) {
	var user struct {
		Login string `json:"login"`
	}
	if err := c.get("/user", &user); err != nil {
		return "", fmt.Errorf("looking up the authenticated user: %w", err)
	}
	if user.Login == "" {
		return "", fmt.Errorf("looking up the authenticated user: response has no login")
	}

	return user.Login, nil
}

// get performs one GET against base+path, decoding a JSON response body into
// result and translating a non-2xx status into *Error.
//
// GET only, by construction: unjira reads GitHub and never writes to it (see
// Reader), so this facade has no way to send any other method. A GitHub writer
// would need its own design, and would start by adding one here.
func (c *Client) get(path string, result any) error {
	const method = http.MethodGet

	req, err := http.NewRequestWithContext(context.Background(), method, c.base+path, nil)
	if err != nil {
		return fmt.Errorf("building %s %s: %w", method, path, err)
	}

	req.Header.Set("Accept", "application/vnd.github+json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("requesting %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response body for %s %s: %w", method, path, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &Error{Status: resp.StatusCode, Message: errorMessage(body)}
	}

	if result == nil || len(body) == 0 {
		return nil
	}

	if err := json.Unmarshal(body, result); err != nil {
		return fmt.Errorf("decoding response for %s %s: %w", method, path, err)
	}

	return nil
}

// errorMessage extracts GitHub's own {"message": "..."} error shape, falling
// back to the raw body when that shape is absent — an HTML error page from an
// intermediary proxy, say.
func errorMessage(body []byte) string {
	var decoded struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &decoded); err == nil && decoded.Message != "" {
		return decoded.Message
	}

	return string(body)
}
