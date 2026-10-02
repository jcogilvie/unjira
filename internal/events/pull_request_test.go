package events_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/jcogilvie/unjira/internal/events"
)

// TestPullRequestRef_IsHostQualified is F43's host half: owner/repo#N alone cannot tell
// acme/infra#12 on github.com from acme/infra#12 on a GHES instance, so the join key
// must carry the host.
func TestPullRequestRef_IsHostQualified(t *testing.T) {
	assert.Equal(t, "github.com/o/r#7", events.PullRequestRef("github.com", "o", "r", 7))
	assert.NotEqual(t,
		events.PullRequestRef("github.com", "acme", "infra", 12),
		events.PullRequestRef("ghes.corp.example", "acme", "infra", 12),
		"the same owner/repo#N on two hosts is two pull requests")
}

// TestPullRequestRef_FoldsCase: GitHub resolves hosts, owners and repository names
// case-insensitively, so a config entry typed "sanyaku/helm-charts" and gh's printed
// "https://github.com/Sanyaku/helm-charts/pull/445" name one pull request and must
// produce one key, or the two writers silently disagree.
func TestPullRequestRef_FoldsCase(t *testing.T) {
	assert.Equal(t,
		events.PullRequestRef("GitHub.com", "Sanyaku", "Helm-Charts", 445),
		events.PullRequestRef("github.com", "sanyaku", "helm-charts", 445))
}

func TestPullRequestOf(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
		want  string
	}{
		{"a host-qualified ref", "github.com/o/r#7", "github.com/o/r#7"},
		{"absent", nil, ""},
		{"the wrong type", 7, ""},
		{"the pre-F43 host-less form, which cannot tell two hosts apart", "o/r#7", ""},
		{"no number", "github.com/o/r#", ""},
		{"a non-numeric number", "github.com/o/r#x", ""},
		{"a zero number", "github.com/o/r#0", ""},
		{"a number with a leading zero, which PullRequestRef never writes", "github.com/o/r#07", ""},
		{"an extra path segment", "github.com/a/o/r#7", ""},
		{"an empty segment", "github.com//r#7", ""},
		{"no separator", "github.com/o/r", ""},
		{"upper case, which PullRequestRef never writes", "github.com/O/r#7", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := events.NewEvent("github", "x", eventTime, "s")
			if tc.value != nil {
				e.Artifacts[events.ArtifactPullRequest] = tc.value
			}

			assert.Equal(t, tc.want, events.PullRequestOf(e))
		})
	}
}
