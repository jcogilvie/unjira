package tasktracker_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// TestParseIssueKey_ParsesEachSyntax: an issue key stays in its tracker's native syntax,
// and the two syntaxes cannot be confused, so the scope is read off the key alone.
func TestParseIssueKey_ParsesEachSyntax(t *testing.T) {
	tests := []struct {
		key  string
		want tasktracker.IssueRef
	}{
		{"PROJ-1", tasktracker.IssueRef{Key: "PROJ-1", Syntax: tasktracker.SyntaxProject, Scope: "PROJ", Number: 1}},
		{"PAAS-4036", tasktracker.IssueRef{Key: "PAAS-4036", Syntax: tasktracker.SyntaxProject, Scope: "PAAS", Number: 4036}},
		{"A1-2", tasktracker.IssueRef{Key: "A1-2", Syntax: tasktracker.SyntaxProject, Scope: "A1", Number: 2}},
		{
			"crossplane/crossplane#6812",
			tasktracker.IssueRef{
				Key: "crossplane/crossplane#6812", Syntax: tasktracker.SyntaxRepo,
				Scope: "crossplane/crossplane", Number: 6812,
			},
		},
		{
			// Case-folded the way events.PullRequestRef folds it: GitHub names are
			// case-insensitive, so two spellings are one repository.
			"Crossplane-Contrib/Provider-AWS#12",
			tasktracker.IssueRef{
				Key: "Crossplane-Contrib/Provider-AWS#12", Syntax: tasktracker.SyntaxRepo,
				Scope: "crossplane-contrib/provider-aws", Number: 12,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			got, err := tasktracker.ParseIssueKey(tt.key)

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestParseIssueKey_MalformedKeyErrorsRatherThanGuessing: a key of neither shape errors,
// naming it, rather than returning a truncated guess a write-scope check would then act
// on.
func TestParseIssueKey_MalformedKeyErrorsRatherThanGuessing(t *testing.T) {
	tests := []string{
		"",
		"PROJ",       // no separator, no number
		"PROJ-",      // no number
		"-1",         // no project
		"PROJ-abc",   // non-numeric suffix
		"PROJ-1-2",   // extra segment
		"proj-1",     // lowercase: real project keys are uppercase
		"owner/repo", // a repository, not an issue
		"#12",        // bare #N needs repo context, which a key does not carry
		"a/b/c#1",    // too many segments
		"owner/repo#x",
	}

	for _, key := range tests {
		t.Run(key, func(t *testing.T) {
			_, err := tasktracker.ParseIssueKey(key)

			require.Error(t, err)
			assert.Contains(t, err.Error(), key)
		})
	}
}

func TestScopeMatches(t *testing.T) {
	tests := []struct {
		pattern, scope string
		want           bool
	}{
		{"PAAS", "PAAS", true},
		{"PAAS", "PAASX", false},
		{"PAAS", "paas", false}, // Jira project keys are exact
		{"crossplane/crossplane", "crossplane/crossplane", true},
		{"crossplane/crossplane", "Crossplane/CrossPlane", true},
		{"crossplane-contrib/*", "crossplane-contrib/provider-aws", true},
		{"crossplane-contrib/*", "CROSSPLANE-CONTRIB/x", true},
		{"crossplane-contrib/*", "crossplane/crossplane", false},
		{"crossplane-contrib/*", "crossplane-contrib", false}, // a glob matches one whole segment
		{"PAAS", "o/r", false},
		{"o/r", "PAAS", false},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, tasktracker.ScopeMatches(tt.pattern, tt.scope), "ScopeMatches(%q, %q)", tt.pattern, tt.scope)
	}
}
