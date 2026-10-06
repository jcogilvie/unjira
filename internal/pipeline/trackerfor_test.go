package pipeline_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/pipeline"
)

// TestCollectContext_TrackerFor: whether an artifact is a tracker record depends on
// which systems are trackers HERE, so a collector asks the config rather than assuming
// its own system is one (F28). The same scope on a different kind is not a match.
func TestCollectContext_TrackerFor(t *testing.T) {
	cc := pipeline.CollectContext{Config: config.Config{
		Connections: []config.Connection{
			{Name: "j", Kind: config.KindJira, Endpoint: "https://x.atlassian.net"},
			{Name: "gh", Kind: config.KindGitHub, Endpoint: "https://api.github.com"},
			{Name: "l", Kind: config.KindLocal},
		},
		Trackers: []config.Tracker{
			{Name: "work", Connection: "j", Scopes: []string{"PAAS"}},
			{Name: "upstream", Connection: "gh", Scopes: []string{"crossplane-contrib/*"}},
			{Name: "sandbox", Connection: "l", Scopes: []string{"PROJ"}},
		},
	}}

	work, ok := cc.TrackerFor(config.KindJira, "PAAS")
	require.True(t, ok)
	assert.Equal(t, "work", work.Name)

	upstream, ok := cc.TrackerFor(config.KindGitHub, "Crossplane-Contrib/provider-aws")
	require.True(t, ok)
	assert.Equal(t, "upstream", upstream.Name)

	_, ok = cc.TrackerFor(config.KindJira, "OPS")
	assert.False(t, ok, "a project no tracker lists is not a tracker scope")

	_, ok = cc.TrackerFor(config.KindJira, "PROJ")
	assert.False(t, ok, "PROJ is a tracker scope, but on the local backend, not on Jira")

	_, ok = cc.TrackerFor(config.KindGitHub, "crossplane/crossplane")
	assert.False(t, ok, "a repository no tracker lists is work evidence, not a tracker record")
}
