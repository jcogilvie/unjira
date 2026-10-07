package github_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/clients/github"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// The GitHub backend is a reader and nothing else: it implements no TaskWriter method,
// so a write to a public tracker is a compile error, not a gate.
var _ tasktracker.TaskReader = (*github.Reader)(nil)

func TestReader_GetIssue_NormalizesAnIssue(t *testing.T) {
	var gotPath string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		writeJSON(t, w, http.StatusOK, map[string]any{
			"number": 6812, "title": "Composition functions", "body": "the body", "state": "open",
			"labels": []map[string]any{{"name": "enhancement"}},
		})
	})

	issue, err := github.NewReader(client).GetIssue("Crossplane/Crossplane#6812")

	require.NoError(t, err)
	assert.Equal(t, "/repos/crossplane/crossplane/issues/6812", gotPath)
	assert.Equal(t, tasktracker.Issue{
		Key:            "Crossplane/Crossplane#6812",
		Summary:        "Composition functions",
		StatusCategory: tasktracker.StatusTodo,
		StatusName:     "open",
		Labels:         []string{"enhancement"},
		Description:    "the body",
	}, issue)
}

func TestReader_GetIssue_ClosedIsDone(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{"number": 1, "title": "t", "state": "closed"})
	})

	issue, err := github.NewReader(client).GetIssue("o/r#1")

	require.NoError(t, err)
	assert.Equal(t, tasktracker.StatusDone, issue.StatusCategory)
	assert.Equal(t, "closed", issue.StatusName)
}

// TestReader_GetIssue_CarriesClosedAndUpdatedDates: closed_at is GitHub's resolution
// date, and updated_at its last change. Matching sets both against a narrative's
// window. An open issue has no closed_at.
func TestReader_GetIssue_CarriesClosedAndUpdatedDates(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{
			"number": 1, "title": "t", "state": "closed",
			"closed_at": "2025-03-04T10:15:30Z", "updated_at": "2025-03-05T08:00:00Z",
		})
	})

	issue, err := github.NewReader(client).GetIssue("o/r#1")

	require.NoError(t, err)
	assert.True(t, issue.Resolved.Equal(time.Date(2025, 3, 4, 10, 15, 30, 0, time.UTC)), "resolved: %v", issue.Resolved)
	assert.True(t, issue.Updated.Equal(time.Date(2025, 3, 5, 8, 0, 0, 0, time.UTC)), "updated: %v", issue.Updated)
}

// TestReader_GetIssue_APullRequestIsNotAnIssue: GitHub serves pull requests from the
// issues endpoint too. The reader resolves issues only — a pull request is work
// evidence — so one reads as not found, never as an issue a narrative is tracked in.
func TestReader_GetIssue_APullRequestIsNotAnIssue(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{
			"number": 7, "title": "a PR", "state": "open",
			"pull_request": map[string]any{"url": "https://api.github.com/repos/o/r/pulls/7"},
		})
	})

	_, err := github.NewReader(client).GetIssue("o/r#7")

	require.ErrorIs(t, err, tasktracker.ErrNotFound)
	assert.Contains(t, err.Error(), "pull request")
	assert.False(t, tasktracker.IsTransportError(err))
}

func TestReader_GetIssue_404IsNotFoundNotTransport(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusNotFound, map[string]any{"message": "Not Found"})
	})

	_, err := github.NewReader(client).GetIssue("o/r#404")

	require.Error(t, err)
	assert.False(t, tasktracker.IsTransportError(err), "a missing issue must drop, not retry forever")
}

func TestReader_GetIssue_RefusesAKeyThatIsNotARepositoryIssue(t *testing.T) {
	client := newTestClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("no request may be made for a key this reader cannot own")
	})

	_, err := github.NewReader(client).GetIssue("PAAS-1")

	require.ErrorContains(t, err, "PAAS-1")
}

// TestError_IsTransport classifies GitHub's answers for tasktracker.IsTransportError:
// an answer about the issue is not transport; an outage or a rate limit is.
func TestError_IsTransport(t *testing.T) {
	tests := []struct {
		err  github.Error
		want bool
	}{
		{github.Error{Status: 0}, true},
		{github.Error{Status: 404, Message: "Not Found"}, false},
		{github.Error{Status: 410, Message: "Gone"}, false},
		{github.Error{Status: 429}, true},
		{github.Error{Status: 403, Message: "API rate limit exceeded for user"}, true},
		{github.Error{Status: 403, Message: "Resource not accessible by integration"}, false},
		{github.Error{Status: 502}, true},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, tt.err.IsTransport(), "%d %q", tt.err.Status, tt.err.Message)
	}
}

// TestReader_AvailableTransitions_OffersNone: unjira never writes to GitHub, so there
// is no destination it could move an issue to, and the reconciler proposes none.
func TestReader_AvailableTransitions_OffersNone(t *testing.T) {
	client := newTestClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("no request is needed to know a read-only backend offers nothing")
	})

	got, err := github.NewReader(client).AvailableTransitions("o/r#1")

	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestReader_SearchIssues_SearchesIssuesOnly(t *testing.T) {
	var gotQuery string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/search/issues", r.URL.Path)
		gotQuery = r.URL.Query().Get("q")
		writeJSON(t, w, http.StatusOK, map[string]any{"items": []map[string]any{
			{
				"number": 3, "title": "found", "state": "open",
				"repository_url": "https://api.github.com/repos/o/r",
			},
		}})
	})

	got, err := github.NewReader(client).SearchIssues("repo:o/r label:bug", 10)

	require.NoError(t, err)
	assert.Equal(t, "repo:o/r label:bug is:issue", gotQuery)
	require.Len(t, got, 1)
	assert.Equal(t, "o/r#3", got[0].Key)
}
