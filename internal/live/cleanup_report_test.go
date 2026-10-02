//go:build live

package live

// cleanup_report_test.go covers reportCleanupFailure without Jira, so the one part of
// F38's fix that decides what a human sees is checked on every live-tagged run, not only
// on the rare run where a delete actually fails.

import (
	"bytes"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestReportCleanupFailure(t *testing.T) {
	deleteErr := errors.New("jira: 503 Service Unavailable\nretry later")

	tests := []struct {
		name          string
		err           error
		githubActions bool
		wantStderr    string
		wantStdout    string
	}{
		{
			name: "a successful delete reports nothing",
		},
		{
			name: "a failed delete is announced on stderr",
			err:  deleteErr,
			wantStderr: "could not delete the seed issue created by TestX SCRUM-7: " +
				"jira: 503 Service Unavailable\nretry later — delete it by hand\n",
		},
		{
			name:          "and under GitHub Actions also as an escaped ::warning",
			err:           deleteErr,
			githubActions: true,
			wantStderr: "could not delete the seed issue created by TestX SCRUM-7: " +
				"jira: 503 Service Unavailable\nretry later — delete it by hand\n",
			wantStdout: "::warning title=Live Jira cleanup::could not delete the seed issue " +
				"created by TestX SCRUM-7: jira: 503 Service Unavailable%0Aretry later — delete it by hand\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stderr, stdout bytes.Buffer

			reportCleanupFailure(&stderr, &stdout, tc.githubActions,
				"the seed issue created by TestX", "SCRUM-7", tc.err)

			assert.Equal(t, tc.wantStderr, stderr.String())
			assert.Equal(t, tc.wantStdout, stdout.String())
		})
	}
}

func TestEscapeWorkflowData(t *testing.T) {
	assert.Equal(t, "100%25 lost%0D%0Are-run", escapeWorkflowData("100% lost\r\nre-run"))
}
