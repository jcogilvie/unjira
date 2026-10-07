package events_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/jcogilvie/unjira/internal/events"
)

// TestExtractGitHubIssueRefs: qualified references and issue URLs are candidates, in
// order of appearance, lower-cased and deduplicated. A bare #N needs repository context
// a key does not carry, and a pull-request URL is not an issue.
func TestExtractGitHubIssueRefs(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []string
	}{
		{"qualified reference", "see crossplane/crossplane#6812 for context", []string{"crossplane/crossplane#6812"}},
		{"fixes in a PR body", "Fixes crossplane-contrib/provider-aws#12\n\nDetails.", []string{"crossplane-contrib/provider-aws#12"}},
		{"issue URL", "https://github.com/crossplane/crossplane/issues/6812", []string{"crossplane/crossplane#6812"}},
		{"case folded and deduplicated", "Crossplane/Crossplane#1 and crossplane/crossplane#1", []string{"crossplane/crossplane#1"}},
		{
			"order of appearance across both forms",
			"https://github.com/o/r/issues/2 then o/r#1",
			[]string{"o/r#2", "o/r#1"},
		},
		{"bare #N is not a reference", "Fixes #12", nil},
		{"pull request URL is not an issue", "https://github.com/o/r/pull/3", nil},
		{"a path, not a reference", "see docs/a/b#c and src/x#y", nil},
		{"a deeper path ending in #N", "see docs/a/b#1", nil},
		{"a Jira key is not a GitHub reference", "PAAS-123", nil},
		{"dotted repository names", "kubernetes-sigs/cluster-api.v2#4", []string{"kubernetes-sigs/cluster-api.v2#4"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, events.ExtractGitHubIssueRefs(tt.text))
		})
	}
}

// TestExtractGitHubClosingRefs: a reference after one of GitHub's closing keywords,
// one keyword per reference, as GitHub itself reads them. A reference with no keyword
// is a citation and is not returned.
func TestExtractGitHubClosingRefs(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []string
	}{
		{"fixes", "Fixes o/r#1", []string{"o/r#1"}},
		{"any case, with a colon", "RESOLVED: o/r#2", []string{"o/r#2"}},
		{
			"every keyword form", "close o/r#1, closes o/r#2, closed o/r#3, fix o/r#4, fixed o/r#5, resolve o/r#6",
			[]string{"o/r#1", "o/r#2", "o/r#3", "o/r#4", "o/r#5", "o/r#6"},
		},
		{"an issue URL", "Closes https://github.com/o/r/issues/9.", []string{"o/r#9"}},
		{"a citation is not a closing reference", "see o/r#1, which hit the same bug", nil},
		{"one keyword covers one reference", "Fixes o/r#1 and o/r#2", []string{"o/r#1"}},
		{"a keyword inside a word is not a keyword", "prefixes o/r#1", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, events.ExtractGitHubClosingRefs(tt.text))
		})
	}
}
