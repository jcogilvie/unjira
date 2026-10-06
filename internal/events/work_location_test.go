package events_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/events"
)

var testTime = time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)

// TestRepoArtifacts_SurviveAStoreRoundTrip: the artifacts are []any on disk, so a writer
// storing []string would be read back as nothing.
func TestRepoArtifacts_SurviveAStoreRoundTrip(t *testing.T) {
	evt := events.NewEvent("claude_code", "x", testTime, "s")
	events.SetRepos(&evt, events.ArtifactWorkRepos, []string{"github.com/o/r"})
	events.SetRepos(&evt, events.ArtifactCwdRemotes, []string{"github.com/a/b", "github.com/c/d"})

	raw, err := json.Marshal(evt)
	require.NoError(t, err)

	var back events.Event
	require.NoError(t, json.Unmarshal(raw, &back))

	assert.Equal(t, []string{"github.com/o/r"}, events.ReposOf(back, events.ArtifactWorkRepos))
	assert.Equal(t, []string{"github.com/a/b", "github.com/c/d"}, events.ReposOf(back, events.ArtifactCwdRemotes))
	assert.Nil(t, events.ReposOf(events.NewEvent("x", "y", testTime, ""), events.ArtifactWorkRepos))
}

func TestNormalizeRepo(t *testing.T) {
	tests := []struct {
		raw, want string
		ok        bool
	}{
		{"git@github.com:Crossplane/Crossplane.git", "github.com/crossplane/crossplane", true},
		{"https://github.com/crossplane/crossplane", "github.com/crossplane/crossplane", true},
		{"https://github.com/crossplane/crossplane.git", "github.com/crossplane/crossplane", true},
		{"ssh://git@github.com/o/r.git", "github.com/o/r", true},
		{"https://user:pw@ghe.example.com/o/r.git", "ghe.example.com/o/r", true},
		{"github.com:o/r.git", "github.com/o/r", true},
		{"https://github.com/o/r/", "github.com/o/r", true},
		{"/srv/git/r.git", "", false},
		{"/srv/git:o/r.git", "", false},
		{"./mirror:o/r", "", false},
		{"C:/o/r", "", false},
		{"git@github-work:o/r.git", "github-work/o/r", true},
		{"file:///srv/git/o/r.git", "", false},
		{"https://github.com/o", "", false},
		{"", "", false},
	}

	for _, tt := range tests {
		got, ok := events.NormalizeRepo(tt.raw)

		assert.Equal(t, tt.ok, ok, tt.raw)
		assert.Equal(t, tt.want, got, tt.raw)
	}
}
