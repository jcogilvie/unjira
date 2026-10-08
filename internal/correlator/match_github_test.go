package correlator_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// TestMatch_AnUpstreamIssueIsVerifiedOnItsOwnTracker: work that names an upstream issue
// matches it, verified on the GitHub tracker that owns the repository rather than on
// Jira, so upstream work no longer looks untracked.
func TestMatch_AnUpstreamIssueIsVerifiedOnItsOwnTracker(t *testing.T) {
	s := matchStore(t)
	seedNarrative(t, s, "Upstream fix", "worked on the composition bug",
		summaryEvent(t, "claude_code", "s1", "working on crossplane/crossplane#6812"))

	jira := &singleTracker{issues: map[string]tasktracker.Issue{}}
	upstream := &singleTracker{issues: map[string]tasktracker.Issue{
		"crossplane/crossplane#6812": {Key: "crossplane/crossplane#6812", Summary: "Composition bug", StatusName: "open"},
	}}
	routed := tasktracker.Routed(tasktracker.NewResolver(
		routeTo("work", jira, "PAAS"),
		routeTo("upstream", upstream, "crossplane/*"),
	).WithReadFallback("work"))

	// A lone prose mention is judged by the model (Provenance.NamesTheWork), which here
	// says it is the work.
	llmFake := &fakeLLM{responses: []string{
		`[{"issue_key":"crossplane/crossplane#6812","role":"primary","confidence":0.9,"rationale":"the work"}]`,
	}}

	results, _, err := correlator.Match(t.Context(), s, routed, llmFake, matchCfg())
	require.NoError(t, err)
	require.Len(t, results, 1)

	assert.Equal(t, "crossplane/crossplane#6812", results[0].Primary)
	assert.Equal(t, []string{"crossplane/crossplane#6812"}, upstream.getCalls)
	assert.Empty(t, jira.getCalls, "the Jira fallback never serves a repository key")
}
