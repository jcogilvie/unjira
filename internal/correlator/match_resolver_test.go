package correlator_test

// This file is the regression suite for the cross-connection matching bug:
// correlator.Match used to take ONE tasktracker.TaskReader for an entire pass,
// resolved once by the caller from a single default project. On a multi-site Jira
// setup, a candidate owned by a different site was verified against the wrong one —
// GetIssue(key) would 404 against a site that never had the issue, or (worse, if the
// key collided with something unrelated) resolve to a different ticket entirely.
//
// Match now reads through tasktracker.Routed over a tasktracker.Resolver, so every
// candidate routes by its own key to the tracker whose scopes own it. These tests give
// each tracker only its own keys, so a wrong-tracker GetIssue fails loudly rather than
// passing by accident because both fakes happened to answer the same way.

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// connectionJiraEvent is jiraEvent (match_candidates_test.go) parameterized on
// connection and project, so a test can put candidates from two different trackers
// into one narrative.
func connectionJiraEvent(t *testing.T, connection, key string) events.Event {
	t.Helper()

	e := events.NewEvent("jira", connection+":"+key+":status:1",
		time.Date(2026, 8, 24, 10, 30, 0, 0, time.UTC), key+" status: To Do → Done")
	e.Artifacts["issue_key"] = key
	e.Artifacts["project_key"] = "PROJ"
	e.Artifacts["connection"] = connection

	return e
}

// singleTracker resolves exactly the keys in issues and records every key it was asked
// about, so a multi-tracker test can tell which backend a candidate reached.
type singleTracker struct {
	issues   map[string]tasktracker.Issue
	getCalls []string
}

func (f *singleTracker) GetIssue(key string) (tasktracker.Issue, error) {
	f.getCalls = append(f.getCalls, key)

	issue, ok := f.issues[key]
	if !ok {
		return tasktracker.Issue{}, fmt.Errorf("issue %s: %w", key, tasktracker.ErrNotFound)
	}

	return issue, nil
}

func (f *singleTracker) SearchIssues(string, int) ([]tasktracker.Issue, error) {
	return nil, nil
}

func (f *singleTracker) AvailableTransitions(string) ([]tasktracker.Transition, error) {
	return nil, nil
}

var _ tasktracker.TaskReader = (*singleTracker)(nil)

// routeTo is a read-only Route over tracker for scopes.
func routeTo(name string, tracker tasktracker.TaskReader, scopes ...string) tasktracker.Route {
	return tasktracker.Route{
		Tracker: name, Scopes: scopes,
		OpenReader: func() (tasktracker.TaskReader, error) { return tracker, nil },
	}
}

// TestMatch_ResolvesEachCandidateAgainstItsOwnTracker is the bug, reproduced: one
// narrative names a PAAS candidate and a SUMO candidate, owned by different trackers.
// Each fake only knows its own key, so a wrong-tracker call fails.
func TestMatch_ResolvesEachCandidateAgainstItsOwnTracker(t *testing.T) {
	s := matchStore(t)
	seedNarrative(t, s, "Two-tracker work", "engineering plus change management",
		connectionJiraEvent(t, "corp", "PAAS-1"),
		connectionJiraEvent(t, "paas", "SUMO-2"),
	)

	corpTracker := &singleTracker{issues: map[string]tasktracker.Issue{
		"PAAS-1": {Key: "PAAS-1", Summary: "Feature work", StatusName: "In Progress"},
	}}
	paasTracker := &singleTracker{issues: map[string]tasktracker.Issue{
		"SUMO-2": {Key: "SUMO-2", Summary: "Change task", StatusName: "To Do"},
	}}
	routed := tasktracker.Routed(tasktracker.NewResolver(
		routeTo("corp", corpTracker, "PAAS"),
		routeTo("paas", paasTracker, "SUMO"),
	))

	llmFake := &fakeLLM{responses: []string{
		`[{"issue_key":"PAAS-1","role":"primary","confidence":0.9,"rationale":"r"},` +
			`{"issue_key":"SUMO-2","role":"same_work","confidence":0.8,"rationale":"r"}]`,
	}}

	results, _, err := correlator.Match(t.Context(), s, routed, llmFake, matchCfg())
	require.NoError(t, err)
	require.Len(t, results, 1)

	assert.Equal(t, []string{"PAAS-1"}, corpTracker.getCalls,
		"the PAAS candidate must be verified against the tracker owning PAAS, never the other")
	assert.Equal(t, []string{"SUMO-2"}, paasTracker.getCalls,
		"the SUMO candidate must be verified against the tracker owning SUMO, never the other")
	assert.Equal(t, "PAAS-1", results[0].Primary)
}

// TestMatch_ACandidateWithNoConnectionRoutesByItsKey: a branch- or prose-derived
// candidate carries no connection, and needs none — its key names its scope.
func TestMatch_ACandidateWithNoConnectionRoutesByItsKey(t *testing.T) {
	s := matchStore(t)
	seedNarrative(t, s, "Implement feature", "did the feature work",
		claudeEvent(t, "s1", "feature/PROJ-42"))

	other := &singleTracker{issues: map[string]tasktracker.Issue{}}
	owner := &singleTracker{issues: map[string]tasktracker.Issue{
		"PROJ-42": {Key: "PROJ-42", Summary: "Feature work", StatusName: "In Progress"},
	}}
	routed := tasktracker.Routed(tasktracker.NewResolver(
		routeTo("other", other, "OPS"),
		routeTo("owner", owner, "PROJ"),
	))

	results, _, err := correlator.Match(t.Context(), s, routed, &fakeLLM{}, matchCfg())
	require.NoError(t, err)
	require.Len(t, results, 1)

	assert.Equal(t, []string{"PROJ-42"}, owner.getCalls)
	assert.Empty(t, other.getCalls)
	assert.Equal(t, "PROJ-42", results[0].Primary)
}

// TestMatch_UnroutedCandidateIsRecordedNotDropped covers "never silently drop data"
// for a key no tracker's scopes cover (a scope removed from config, or never added).
// Treating it as transport would fail the narrative every pass forever, since config
// does not change between passes. It is recorded in Unresolved with the reason, and
// no tracker is asked.
func TestMatch_UnroutedCandidateIsRecordedNotDropped(t *testing.T) {
	s := matchStore(t)
	seedNarrative(t, s, "Orphaned scope", "candidate on a project nobody configured",
		connectionJiraEvent(t, "decommissioned", "OLD-1"))

	def := &singleTracker{issues: map[string]tasktracker.Issue{}}
	routed := tasktracker.Routed(tasktracker.NewResolver(routeTo("work", def, "PROJ")))

	results, _, err := correlator.Match(t.Context(), s, routed, &fakeLLM{}, matchCfg())
	require.NoError(t, err, "an unrouted key must not fail the whole narrative")
	require.Len(t, results, 1)

	require.Len(t, results[0].Unresolved, 1)
	assert.Contains(t, results[0].Unresolved[0], "OLD-1")
	assert.Contains(t, results[0].Unresolved[0], "no configured tracker's scopes cover",
		"the reason must say no tracker owns it, so a human can act on it")
	assert.Empty(t, results[0].Primary)
	assert.Empty(t, def.getCalls, "GetIssue must never be called for a key no tracker owns")
}

// TestMatch_UnroutedCandidateDoesNotBlockASibling: the unrouted candidate above must
// not cost a sibling its chance.
func TestMatch_UnroutedCandidateDoesNotBlockASibling(t *testing.T) {
	s := matchStore(t)
	seedNarrative(t, s, "One good, one orphaned", "summary",
		connectionJiraEvent(t, "corp", "PAAS-1"),
		connectionJiraEvent(t, "decommissioned", "OLD-1"),
	)

	corpTracker := &singleTracker{issues: map[string]tasktracker.Issue{
		"PAAS-1": {Key: "PAAS-1", Summary: "Feature work", StatusName: "In Progress"},
	}}
	routed := tasktracker.Routed(tasktracker.NewResolver(routeTo("corp", corpTracker, "PAAS")))

	results, _, err := correlator.Match(t.Context(), s, routed, &fakeLLM{}, matchCfg())
	require.NoError(t, err)
	require.Len(t, results, 1)

	assert.Equal(t, "PAAS-1", results[0].Primary,
		"the routed candidate must still win primary deterministically")
	require.Len(t, results[0].Unresolved, 1)
	assert.Contains(t, results[0].Unresolved[0], "OLD-1")
}

// TestMatch_FallbackReadKeepsAnUnlistedProjectLinked: a ticket in a project no tracker
// lists is read through the resolver's fallback, as every candidate used to be, so the
// work on it stays linked rather than reaching the create path as untracked.
func TestMatch_FallbackReadKeepsAnUnlistedProjectLinked(t *testing.T) {
	s := matchStore(t)
	seedNarrative(t, s, "Work on another team's ticket", "summary",
		claudeEvent(t, "s1", "feature/SUMO-7"))

	def := &singleTracker{issues: map[string]tasktracker.Issue{
		"SUMO-7": {Key: "SUMO-7", Summary: "Their ticket", StatusName: "In Progress"},
	}}
	routed := tasktracker.Routed(tasktracker.NewResolver(routeTo("work", def, "PROJ")).WithReadFallback("work"))

	results, _, err := correlator.Match(t.Context(), s, routed, &fakeLLM{}, matchCfg())
	require.NoError(t, err)
	require.Len(t, results, 1)

	assert.Equal(t, "SUMO-7", results[0].Primary)
}
