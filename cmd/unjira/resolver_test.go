package main

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/logging"
	"github.com/jcogilvie/unjira/internal/store"
)

// twoLocalTrackers puts two trackers on one local connection: PROJ writable, OPS read
// only.
func twoLocalTrackers() config.Config {
	return config.Config{
		Connections: []config.Connection{{Name: "local", Kind: config.KindLocal}},
		Trackers: []config.Tracker{
			{Name: "dev", Connection: "local", Scopes: []string{"PROJ"}, WritableScopes: []string{"PROJ"}},
			{Name: "ops", Connection: "local", Scopes: []string{"OPS"}},
		},
	}
}

// TestResolver_RoutesEachKeyToItsTrackerAndWritesOnlyWritableScopes is the wiring
// half of routing: the resolver cmd builds from config hands out a writer for a
// writable scope only, whichever tracker shares the connection.
func TestResolver_RoutesEachKeyToItsTrackerAndWritesOnlyWritableScopes(t *testing.T) {
	s := openTestStore(t)
	app := &appContext{config: twoLocalTrackers(), store: s}

	proj, err := app.resolver().Resolve("PROJ-1")
	require.NoError(t, err)
	assert.Equal(t, "dev", proj.Tracker)
	assert.NotNil(t, proj.Writer)

	ops, err := app.resolver().Resolve("OPS-1")
	require.NoError(t, err)
	assert.Equal(t, "ops", ops.Tracker)
	assert.Nil(t, ops.Writer, "OPS is readable, not writable")

	opsKey, err := s.InsertLocalIssue("OPS", "theirs", "Task", "", nil)
	require.NoError(t, err)
	require.Error(t, app.routedTracker().AddComment(opsKey, "x"),
		"the routed writer refuses a read-only scope even when the gate is bypassed")
}

// TestResolver_FallbackIsTheFirstProjectScopesTracker: an unlisted project is read
// through the tracker every candidate used to be verified against, and never written.
func TestResolver_FallbackIsTheFirstProjectScopesTracker(t *testing.T) {
	app := &appContext{config: twoLocalTrackers(), store: openTestStore(t)}

	res, err := app.resolver().Resolve("SUMO-1")

	require.NoError(t, err)
	assert.True(t, res.Fallback)
	assert.Equal(t, "dev", res.Tracker)
	assert.Nil(t, res.Writer)
}

// TestWarnUnroutedStoredKeys_NamesEachStrandedKey: a config change that drops a scope
// is reported at startup, key by key, and the keys stay stored.
func TestWarnUnroutedStoredKeys_NamesEachStrandedKey(t *testing.T) {
	var logged strings.Builder
	testLog, err := logging.New(logging.Options{Format: "text", Level: "info", Out: &logged})
	require.NoError(t, err)

	s := openTestStore(t)
	app := &appContext{config: twoLocalTrackers(), store: s, log: testLog}

	id, err := s.InsertNarrative(time.Time{}, time.Time{}, "work", "s")
	require.NoError(t, err)
	require.NoError(t, s.WithTx(func(tx *store.Tx) error {
		return tx.AddNarrativeIssues(id, []store.NarrativeIssue{
			{IssueKey: "PROJ-1", Role: "primary", Provenance: "branch", Confidence: 0.9},
			{IssueKey: "GONE-4", Role: "mentioned", Provenance: "prose_later", Confidence: 0.1},
		})
	}))

	app.warnUnroutedStoredKeys()

	assert.Contains(t, logged.String(), "GONE-4")
	assert.NotContains(t, logged.String(), "PROJ-1", "a routed key is not reported")
}
