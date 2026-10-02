//go:build live

package live

// cleanup_test.go is how this tier removes the seed issues it creates, and how it says so
// when it cannot (finding F38).
//
// The client's retry transport deliberately does not retry DELETE, so one transient 5xx
// leaks the issue into the sandbox project. A discarded error made that invisible: the
// project would fill with unjira-seed issues, one per affected test per CI run, and since
// Jira never reuses keys, nothing about a run would look different.
//
// A failed cleanup is REPORTED, never a test failure. A test's verdict is about the code
// under test, and every assertion has already run by the time cleanup does; failing on a
// cleanup hiccup would turn a sandbox-hygiene fact into this tier's own new flake. So the
// failure goes where a human will see it: stderr, and under GitHub Actions a ::warning
// annotation on the checks page.

import (
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/jcogilvie/unjira/internal/clients/jira"
)

// deleteIssueOnCleanup registers a cleanup that deletes key, reporting a failed delete
// rather than discarding it or failing the test.
//
// Every per-test seed issue goes through this, so the reporting cannot drift between seven
// hand-written copies of the same closure.
func deleteIssueOnCleanup(t *testing.T, client *jira.Client, key string) {
	t.Helper()

	name := t.Name()
	t.Cleanup(func() {
		reportCleanupFailure(os.Stderr, os.Stdout, inGitHubActions(),
			"the seed issue created by "+name, key, client.DeleteIssue(key))
	})
}

// reportCleanupFailure announces a failed delete of key, described by what, on stderr,
// and as a GitHub Actions ::warning on stdout when githubActions is set. A nil err reports
// nothing.
//
// The writers are parameters so the reporting is testable without Jira. The annotation
// goes to stdout because workflow commands are read from the start of a stdout line;
// t.Log would indent it and the runner would not see it.
func reportCleanupFailure(stderr, stdout io.Writer, githubActions bool, what, key string, err error) {
	if err == nil {
		return
	}

	msg := fmt.Sprintf("could not delete %s %s: %v — delete it by hand", what, key, err)
	_, _ = fmt.Fprintln(stderr, msg)

	if githubActions {
		_, _ = fmt.Fprintln(stdout, "::warning title=Live Jira cleanup::"+escapeWorkflowData(msg))
	}
}

// inGitHubActions reports whether this run is under GitHub Actions, where workflow-command
// annotations are read.
func inGitHubActions() bool { return os.Getenv("GITHUB_ACTIONS") == "true" }

// escapeWorkflowData escapes a GitHub Actions workflow command's data part, per the
// runner's rules (% CR LF). A raw newline would end the command and drop the rest.
func escapeWorkflowData(s string) string {
	return strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A").Replace(s)
}
