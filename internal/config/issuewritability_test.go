package config_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/jcogilvie/unjira/internal/config"
)

// TestIssueWritability_ReadsTheScopeOffTheKeyBySyntax: a project key routes by its
// prefix and a GitHub key by owner/repo. Cutting at the first "-" — the old shape —
// would read "crossplane" off crossplane-contrib/provider-aws#3.
func TestIssueWritability_ReadsTheScopeOffTheKeyBySyntax(t *testing.T) {
	cfg := config.Config{Trackers: []config.Tracker{
		{Name: "dev", Scopes: []string{"PAAS", "DEVSBX"}, WritableScopes: []string{"DEVSBX"}},
		{Name: "upstream", Scopes: []string{"crossplane-contrib/*"}},
	}}

	assert.True(t, cfg.IssueWritability("DEVSBX-1").Writable)

	paas := cfg.IssueWritability("PAAS-1")
	assert.False(t, paas.Writable)
	assert.Equal(t, "dev", paas.Tracker)

	upstream := cfg.IssueWritability("crossplane-contrib/provider-aws#3")
	assert.False(t, upstream.Writable)
	assert.False(t, upstream.Untracked, "the repository is tracked, read-only")
	assert.Equal(t, "upstream", upstream.Tracker)
}

func TestIssueWritability_MalformedKeyIsUntracked(t *testing.T) {
	got := config.Config{}.IssueWritability("not a key")

	assert.False(t, got.Writable)
	assert.True(t, got.Untracked)
	assert.Contains(t, got.Reason, "not a key")
}
