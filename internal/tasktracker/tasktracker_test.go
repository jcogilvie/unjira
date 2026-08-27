package tasktracker_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// TestProjectFromIssueKey_ExtractsTheProjectPrefix covers the ordinary case:
// every backend that mints keys via store.InsertLocalIssue or a real Jira
// site uses the <PROJECT>-<NUMBER> shape (confirmed against
// internal/store.InsertLocalIssue's own key-minting code), so this is the
// common path gate.Applier's writable-project check depends on.
func TestProjectFromIssueKey_ExtractsTheProjectPrefix(t *testing.T) {
	tests := []struct {
		key  string
		want string
	}{
		{"PROJ-1", "PROJ"},
		{"PAAS-4036", "PAAS"},
		{"DEVSBX-1", "DEVSBX"},
		{"A1-2", "A1"}, // a project key may itself contain a digit
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			got, err := tasktracker.ProjectFromIssueKey(tt.key)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestProjectFromIssueKey_MalformedKeyErrorsRatherThanGuessing is the loud-
// failure half: per CLAUDE.md's "don't guess" convention and this package's
// own precedent (a decode error rather than a zero-value guess), a key that
// does not match <PROJECT>-<NUMBER> must error, naming the offending key,
// rather than silently returning "" or some truncated prefix.
func TestProjectFromIssueKey_MalformedKeyErrorsRatherThanGuessing(t *testing.T) {
	tests := []string{
		"",
		"PROJ",     // no separator, no number
		"PROJ-",    // no number
		"-1",       // no project
		"PROJ-abc", // non-numeric suffix
		"PROJ-1-2", // extra segment
		"proj-1",   // lowercase: real project keys are uppercase
	}

	for _, key := range tests {
		t.Run(key, func(t *testing.T) {
			_, err := tasktracker.ProjectFromIssueKey(key)
			require.Error(t, err)
			assert.Contains(t, err.Error(), key)
		})
	}
}
