package github_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/clients/github"
)

func TestParseRepoRef_ShortFormElidesToGithubCom(t *testing.T) {
	ref, err := github.ParseRepoRef("yourorg/yourrepo")

	require.NoError(t, err)
	assert.Equal(t, "github.com", ref.Host)
	assert.Equal(t, "yourorg", ref.Owner)
	assert.Equal(t, "yourrepo", ref.Repo)
}

func TestParseRepoRef_HostQualifiedFormUsesExplicitHost(t *testing.T) {
	ref, err := github.ParseRepoRef("github.acme.corp/platform/infra")

	require.NoError(t, err)
	assert.Equal(t, "github.acme.corp", ref.Host)
	assert.Equal(t, "platform", ref.Owner)
	assert.Equal(t, "infra", ref.Repo)
}

// TestParseRepoRef_ThreeSegmentsWithNoDotIsAmbiguousAndErrors pins the design's own
// parsing rule: a three-segment string whose first segment has no dot is ambiguous,
// and guessing is how a repo gets silently collected from the wrong instance.
func TestParseRepoRef_ThreeSegmentsWithNoDotIsAmbiguousAndErrors(t *testing.T) {
	_, err := github.ParseRepoRef("someorg/platform/infra")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "someorg/platform/infra")
}

func TestParseRepoRef_SingleSegmentErrors(t *testing.T) {
	_, err := github.ParseRepoRef("justonesegment")

	require.Error(t, err)
}

func TestParseRepoRef_EmptyStringErrors(t *testing.T) {
	_, err := github.ParseRepoRef("")

	require.Error(t, err)
}

func TestRepoRef_StringIsHostOwnerRepo(t *testing.T) {
	ref, err := github.ParseRepoRef("yourorg/yourrepo")
	require.NoError(t, err)

	assert.Equal(t, "github.com/yourorg/yourrepo", ref.String())
}

func TestRepoRef_OwnerRepoOmitsHost(t *testing.T) {
	ref, err := github.ParseRepoRef("yourorg/yourrepo")
	require.NoError(t, err)

	assert.Equal(t, "yourorg/yourrepo", ref.OwnerRepo())
}

func TestBaseURL_GithubComUsesTheWellKnownAPIHost(t *testing.T) {
	assert.Equal(t, "https://api.github.com", github.BaseURL("github.com"))
}

// TestBaseURL_GHESUsesTheDocumentedRESTPrefix: GHES's REST API lives under
// /api/v3 on the instance's own host, not api.<host>.
func TestBaseURL_GHESUsesTheDocumentedRESTPrefix(t *testing.T) {
	assert.Equal(t, "https://github.acme.corp/api/v3", github.BaseURL("github.acme.corp"))
}
