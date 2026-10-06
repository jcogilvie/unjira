//go:build live

package live

// github_reader_test.go drives the read-only GitHub Issues tracker against the fixture
// repo jcogilvie/unjira-sandbox. READ-ONLY, like github_test.go: the reader has no write
// method, so nothing here can alter the sandbox.

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ghclient "github.com/jcogilvie/unjira/internal/clients/github"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

func liveGitHubReader(t *testing.T) *ghclient.Reader {
	t.Helper()

	cred, ok := githubLiveCredentials(t).For(ghclient.DefaultHost)
	require.True(t, ok, "githubLiveCredentials guarantees a github.com credential")

	client, err := ghclient.New(ghclient.BaseURL(ghclient.DefaultHost), cred.Token)
	require.NoError(t, err)

	return ghclient.NewReader(client)
}

// TestLiveGitHubReader_APullRequestIsNotAnIssue: GitHub really does serve pull requests
// from the issues endpoint, and the reader must read every sandbox PR as not found —
// a pull request is work evidence, never the issue work is tracked in.
func TestLiveGitHubReader_APullRequestIsNotAnIssue(t *testing.T) {
	reader := liveGitHubReader(t)

	for _, pr := range sandboxPRs {
		key := fmt.Sprintf("%s#%d", sandboxRepo, pr.number)

		_, err := reader.GetIssue(key)

		require.ErrorIs(t, err, tasktracker.ErrNotFound, "%s is a pull request", key)
		assert.False(t, tasktracker.IsTransportError(err), key)
	}
}

// TestLiveGitHubReader_AMissingIssueIsNotTransport: a real 404 must classify as "does
// not exist", or matching would retry a nonexistent upstream key every pass forever.
func TestLiveGitHubReader_AMissingIssueIsNotTransport(t *testing.T) {
	_, err := liveGitHubReader(t).GetIssue(sandboxRepo + "#999999")

	require.Error(t, err)
	assert.False(t, tasktracker.IsTransportError(err), "a 404 from GitHub: %v", err)
}
