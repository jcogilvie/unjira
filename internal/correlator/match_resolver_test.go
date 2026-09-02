package correlator_test

// This file is the regression suite for the cross-connection matching bug:
// correlator.Match used to take ONE tasktracker.TaskReader for an entire
// pass, resolved once by the caller from a single default project. On a
// multi-connection Jira setup, a candidate whose Candidate.Connection names a
// DIFFERENT configured connection than that default was verified against the
// wrong site's tracker — GetIssue(key) would either 404 against a site that
// never had the issue, or (worse, if key collides with something unrelated on
// the wrong site) resolve to a different ticket entirely. Either way the
// candidate was silently reported unresolved, exactly the "does not exist"
// outcome IsTransportError's own doc comment warns is expensive to get wrong.
//
// Match now takes a correlator.TrackerResolver — a function resolving the
// tracker for a given Candidate.Connection — and calls it once PER CANDIDATE
// (see verifyCandidates), not once per pass. These tests build a fake
// resolver that panics/errors if asked for a tracker on the wrong connection,
// so a regression back to "one tracker for everything" fails loudly rather
// than passing by accident because both fakes happened to answer the same
// way.

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
// connection, so a test can put candidates from two different connections into
// one narrative — the shape that never existed before this change, since the
// existing jiraEvent hardcodes "corp".
func connectionJiraEvent(t *testing.T, connection, key string) events.Event {
	t.Helper()

	e := events.NewEvent("jira", connection+":"+key+":status:1",
		time.Date(2026, 8, 24, 10, 30, 0, 0, time.UTC), key+" status: To Do → Done")
	e.Artifacts["issue_key"] = key
	e.Artifacts["project_key"] = "PROJ"
	e.Artifacts["connection"] = connection

	return e
}

// singleConnectionTracker resolves exactly the keys in issues and records
// every key it was asked about, mirroring fakeTracker but scoped to one
// connection so a multi-connection test can tell which backend a candidate
// actually reached.
type singleConnectionTracker struct {
	issues   map[string]tasktracker.Issue
	getCalls []string
}

func (f *singleConnectionTracker) GetIssue(key string) (tasktracker.Issue, error) {
	f.getCalls = append(f.getCalls, key)

	issue, ok := f.issues[key]
	if !ok {
		return tasktracker.Issue{}, fmt.Errorf("issue %s does not exist on this connection", key)
	}

	return issue, nil
}

func (f *singleConnectionTracker) SearchIssues(string, int) ([]tasktracker.Issue, error) {
	return nil, nil
}

func (f *singleConnectionTracker) AvailableTransitions(string) ([]tasktracker.Transition, error) {
	return nil, nil
}

var _ tasktracker.TaskReader = (*singleConnectionTracker)(nil)

// resolverFor builds a correlator.TrackerResolver over a
// connection-name->tracker map, plus a default used for "" (the common case
// for a branch/prose-derived candidate, which carries no connection at all —
// see Candidate.Connection's own doc comment). Asking for a connection absent
// from byConnection is exactly the "cannot be resolved" case this change must
// not silently drop — it returns a named error rather than panicking, so
// verifyCandidates' handling of it is what's under test, not a crash.
func resolverFor(
	byConnection map[string]tasktracker.TaskReader, def tasktracker.TaskReader,
) correlator.TrackerResolver {
	return func(connection string) (tasktracker.TaskReader, error) {
		if connection == "" {
			return def, nil
		}
		if tr, ok := byConnection[connection]; ok {
			return tr, nil
		}

		return nil, fmt.Errorf("no configured jira connection named %q", connection)
	}
}

// TestMatch_ResolvesEachCandidateAgainstItsOwnConnection is the bug, reproduced:
// one narrative names a candidate on "corp" and a candidate on "paas". Before
// this change Match took a single tracker and would have verified BOTH
// candidates against whichever one tracker the caller resolved up front —
// silently reporting the other site's candidate unresolved (or, worse,
// resolving it against an unrelated issue that happens to share a key). Each
// fake tracker here only knows its own key, so a wrong-site GetIssue call
// fails LOUDLY (a "does not exist" error the fake never populated) rather
// than coincidentally succeeding, which is what would let this regress
// unnoticed if the fakes were less strict.
func TestMatch_ResolvesEachCandidateAgainstItsOwnConnection(t *testing.T) {
	s := matchStore(t)
	seedNarrative(t, s, "Two-connection work", "engineering plus change management",
		connectionJiraEvent(t, "corp", "PAAS-1"),
		connectionJiraEvent(t, "paas", "SUMO-2"),
	)

	corpTracker := &singleConnectionTracker{issues: map[string]tasktracker.Issue{
		"PAAS-1": {Key: "PAAS-1", Summary: "Feature work", StatusName: "In Progress"},
	}}
	paasTracker := &singleConnectionTracker{issues: map[string]tasktracker.Issue{
		"SUMO-2": {Key: "SUMO-2", Summary: "Change task", StatusName: "To Do"},
	}}
	resolve := resolverFor(map[string]tasktracker.TaskReader{
		"corp": corpTracker,
		"paas": paasTracker,
	}, corpTracker)

	llmFake := &fakeLLM{responses: []string{
		`[{"issue_key":"PAAS-1","role":"primary","confidence":0.9,"rationale":"r"},` +
			`{"issue_key":"SUMO-2","role":"same_work","confidence":0.8,"rationale":"r"}]`,
	}}

	results, _, err := correlator.Match(t.Context(), s, resolve, llmFake, matchCfg())
	require.NoError(t, err)
	require.Len(t, results, 1)

	assert.Equal(t, []string{"PAAS-1"}, corpTracker.getCalls,
		"the corp candidate must be verified against the corp tracker, never the paas one")
	assert.Equal(t, []string{"SUMO-2"}, paasTracker.getCalls,
		"the paas candidate must be verified against the paas tracker, never the corp one")
	assert.Equal(t, "PAAS-1", results[0].Primary)
}

// TestMatch_EmptyConnectionUsesTheDefaultTracker: a branch- or
// prose-derived candidate never carries a Connection (the collector only sets
// one for jira-source events — see Candidate's own doc comment). The
// resolver must treat "" as "use the caller's default tracker", not as an
// unresolvable connection — this is the COMMON case (every candidate the
// local backend and every single-connection setup produces), so getting this
// wrong breaks every existing test's shape, not an edge case.
func TestMatch_EmptyConnectionUsesTheDefaultTracker(t *testing.T) {
	s := matchStore(t)
	seedNarrative(t, s, "Implement feature", "did the feature work",
		claudeEvent(t, "s1", "feature/PROJ-42"))

	def := &singleConnectionTracker{issues: map[string]tasktracker.Issue{
		"PROJ-42": {Key: "PROJ-42", Summary: "Feature work", StatusName: "In Progress"},
	}}
	// byConnection is deliberately empty: nothing but the "" -> def fallback
	// should ever be consulted for a branch-derived candidate.
	resolve := resolverFor(map[string]tasktracker.TaskReader{}, def)

	results, _, err := correlator.Match(t.Context(), s, resolve, &fakeLLM{}, matchCfg())
	require.NoError(t, err)
	require.Len(t, results, 1)

	assert.Equal(t, []string{"PROJ-42"}, def.getCalls)
	assert.Equal(t, "PROJ-42", results[0].Primary)
}

// TestMatch_UnresolvableConnectionIsRecordedNotDropped covers the "never
// silently drop data" invariant (CLAUDE.md) for the new failure mode this
// change introduces: a candidate whose Connection names a jira connection
// that is no longer in config (renamed, removed) or was never configured at
// all. If verifyCandidates treated a resolver error as fatal (aborting the
// whole narrative, correlator.IsTransportError's "transport" branch), a
// narrative with even one candidate on a since-removed connection would fail
// EVERY pass forever with no way to make progress, since the config problem
// never resolves itself between passes — the exact "genuinely deleted ticket
// misclassified as transport" failure mode IsTransportError's own doc comment
// warns about. Instead it must be recorded in Unresolved, with the resolver's
// error message included so a human can act on it (add the connection to
// config, or fix the typo), and matching must continue for any other
// candidate.
func TestMatch_UnresolvableConnectionIsRecordedNotDropped(t *testing.T) {
	s := matchStore(t)
	seedNarrative(t, s, "Orphaned connection", "candidate on a connection nobody configured",
		connectionJiraEvent(t, "decommissioned", "OLD-1"))

	def := &singleConnectionTracker{issues: map[string]tasktracker.Issue{}}
	// byConnection has no entry for "decommissioned".
	resolve := resolverFor(map[string]tasktracker.TaskReader{}, def)

	results, _, err := correlator.Match(t.Context(), s, resolve, &fakeLLM{}, matchCfg())
	require.NoError(t, err, "an unresolvable connection must not fail the whole narrative")
	require.Len(t, results, 1)

	require.Len(t, results[0].Unresolved, 1)
	assert.Contains(t, results[0].Unresolved[0], "OLD-1")
	assert.Contains(t, results[0].Unresolved[0], "decommissioned",
		"the reason must name the unresolvable connection so a human can act on it")
	assert.Empty(t, results[0].Primary)
	assert.Empty(t, def.getCalls, "GetIssue must never be called when the connection itself could not be resolved")
}

// TestMatch_UnresolvableConnectionCandidateDoesNotBlockASibling: the
// unresolvable-connection candidate above must not cost a SIBLING candidate
// its chance — the same per-candidate isolation verifyCandidates already
// gives a live 404 (TestMatch_VerifiesEveryCandidateAndDropsUnresolvable).
// Two candidates on the same narrative: one on a connection that resolves and
// exists, one on a connection nobody configured. The narrative must still
// resolve to the good candidate as primary.
func TestMatch_UnresolvableConnectionCandidateDoesNotBlockASibling(t *testing.T) {
	s := matchStore(t)
	seedNarrative(t, s, "One good, one orphaned", "summary",
		connectionJiraEvent(t, "corp", "PAAS-1"),
		connectionJiraEvent(t, "decommissioned", "OLD-1"),
	)

	corpTracker := &singleConnectionTracker{issues: map[string]tasktracker.Issue{
		"PAAS-1": {Key: "PAAS-1", Summary: "Feature work", StatusName: "In Progress"},
	}}
	resolve := resolverFor(map[string]tasktracker.TaskReader{"corp": corpTracker}, corpTracker)

	results, _, err := correlator.Match(t.Context(), s, resolve, &fakeLLM{}, matchCfg())
	require.NoError(t, err)
	require.Len(t, results, 1)

	assert.Equal(t, "PAAS-1", results[0].Primary,
		"the resolvable candidate must still win primary deterministically")
	require.Len(t, results[0].Unresolved, 1)
	assert.Contains(t, results[0].Unresolved[0], "OLD-1")
}
