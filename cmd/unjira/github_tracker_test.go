package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/credentials"
)

// TestResolver_GitHubTrackerReadsUpstreamIssuesAndNeverWrites: an upstream issue key
// routes to the github tracker, reads through GitHub Issues with the credential for the
// endpoint's host, and has no writer however the config is shaped — no request that
// writes can be built.
func TestResolver_GitHubTrackerReadsUpstreamIssuesAndNeverWrites(t *testing.T) {
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		assert.Equal(t, "Bearer gh-token", r.Header.Get("Authorization"))
		assert.Equal(t, "/repos/crossplane/crossplane/issues/6812", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"number": 6812, "title": "Composition functions", "state": "open",
		}))
	}))
	t.Cleanup(srv.Close)

	host := strings.TrimPrefix(srv.URL, "http://")
	app := &appContext{
		config: config.Config{
			Connections: []config.Connection{{Name: "github", Kind: config.KindGitHub, Endpoint: srv.URL}},
			Trackers:    []config.Tracker{{Name: "upstream", Connection: "github", Scopes: []string{"crossplane/*"}}},
		},
		store:             openTestStore(t),
		githubCredentials: jsonSetFor(t, map[string]credentials.Credential{host: {Token: "gh-token"}}),
	}

	res, err := app.resolver().Resolve("crossplane/crossplane#6812")
	require.NoError(t, err)
	assert.Equal(t, "upstream", res.Tracker)
	assert.Nil(t, res.Writer, "a GitHub tracker is read-only")

	issue, err := app.routedTracker().GetIssue("crossplane/crossplane#6812")
	require.NoError(t, err)
	assert.Equal(t, "Composition functions", issue.Summary)

	require.Error(t, app.routedTracker().AddComment("crossplane/crossplane#6812", "x"))
	assert.Equal(t, []string{http.MethodGet}, methods, "only the read reached GitHub")

	require.NoError(t, app.resolver().CheckWriters(), "a read-only tracker needs no identity")
}
