package correlator_test

// This file exercises Match against a hand-written fake
// tasktracker.TaskTracker and the existing fakeLLM, plus a real temp-file
// SQLite store — matching's own logic (candidate verification, the
// deterministic single-survivor path, LLM classification, floor gating,
// exclusion recording, per-narrative failure isolation, idempotency) is
// what's under test here, never a real Jira call.

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/clients/jira"
	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/rules"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// primaryOf reports the narrative's primary issue key as STORED, or "" when it has
// none.
//
// Reads the narrative_issues link table, which is the single source of truth for
// attribution. These assertions previously read narratives.issue_key, a
// denormalized column that finding F11 removed: the confidence floor decided
// whether to write it while the link row was written regardless, so the two
// disagreed and matching's backlog — which selected on the column — re-matched the
// narrative forever.
func primaryOf(t *testing.T, s *store.Store, narrativeID int64) string {
	t.Helper()

	links, err := s.NarrativeIssues(narrativeID)
	require.NoError(t, err)

	for _, l := range links {
		if l.Role == store.RolePrimary {
			return l.IssueKey
		}
	}

	return ""
}

// fakeTracker satisfies tasktracker.TaskReader without a network call.
//
// issues is the set of keys that resolve; anything else returns a not-found
// error, simulating a hallucinated or stale key. getErr forces a transport
// failure for one key, to exercise per-narrative isolation.
//
// Reader-only on purpose: matching verifies candidates and never mutates a
// tracker, so a fake that cannot write makes an accidental write a compile
// error here rather than a surprise against real Jira.
type fakeTracker struct {
	issues   map[string]tasktracker.Issue
	getErr   map[string]error
	getCalls []string
}

func (f *fakeTracker) GetIssue(key string) (tasktracker.Issue, error) {
	f.getCalls = append(f.getCalls, key)

	if err, ok := f.getErr[key]; ok {
		return tasktracker.Issue{}, err
	}

	issue, ok := f.issues[key]
	if !ok {
		return tasktracker.Issue{}, &jira.Error{Status: 404, Message: "issue does not exist"}
	}

	return issue, nil
}

func (f *fakeTracker) SearchIssues(string, int) ([]tasktracker.Issue, error) { return nil, nil }

// Matching never proposes transitions, so legality is never consulted.
func (f *fakeTracker) AvailableTransitions(string) ([]tasktracker.Transition, error) {
	return nil, nil
}

var _ tasktracker.TaskReader = (*fakeTracker)(nil)

// matchStore opens a fresh temp-file store for a Match test.
func matchStore(t *testing.T) *store.Store {
	t.Helper()

	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	return s
}

// seedNarrative inserts a narrative with the given title/summary and links
// evts to it (each first inserted into the events table), returning the
// narrative's row id.
func seedNarrative(t *testing.T, s *store.Store, title, summary string, evts ...events.Event) int64 {
	t.Helper()

	base := time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)
	id, err := s.InsertNarrative(base, base.Add(time.Hour), title, summary)
	require.NoError(t, err)

	eventIDs := make([]int64, 0, len(evts))
	for _, e := range evts {
		_, err := s.InsertEvent(e)
		require.NoError(t, err)
		eid, err := s.EventIDByExternalID(e.Source, e.ExternalID)
		require.NoError(t, err)
		eventIDs = append(eventIDs, eid)
	}

	require.NoError(t, s.AddNarrativeEvents(id, eventIDs))

	return id
}

func matchCfg() config.MatchConfig {
	return config.MatchConfig{ConfidenceFloor: 0.5}
}

func TestMatch_SingleCandidateNeedsNoLLM(t *testing.T) {
	s := matchStore(t)
	id := seedNarrative(t, s, "Implement feature", "did the feature work",
		claudeEvent(t, "s1", "feature/PROJ-42"))

	tracker := &fakeTracker{issues: map[string]tasktracker.Issue{
		"PROJ-42": {Key: "PROJ-42", Summary: "Feature work", StatusName: "In Progress"},
	}}
	llmFake := &fakeLLM{}

	results, stats, err := correlator.Match(t.Context(), s, correlator.SingleTracker(tracker), llmFake, matchCfg())
	require.NoError(t, err)
	require.Len(t, results, 1)

	assert.Equal(t, "PROJ-42", results[0].Primary)
	assert.Equal(t, 0, stats.Calls)
	assert.Empty(t, llmFake.prompts, "one survivor is not a judgment call; no LLM spend")

	assert.Equal(t, "PROJ-42", primaryOf(t, s, id))
}

func TestMatch_ZeroCandidatesTouchesNothing(t *testing.T) {
	s := matchStore(t)
	id := seedNarrative(t, s, "Untracked work", "just some work",
		claudeEvent(t, "s1", "main"))

	tracker := &fakeTracker{issues: map[string]tasktracker.Issue{}}
	llmFake := &fakeLLM{}

	results, stats, err := correlator.Match(t.Context(), s, correlator.SingleTracker(tracker), llmFake, matchCfg())
	require.NoError(t, err)
	require.Len(t, results, 1)

	assert.Empty(t, results[0].Primary)
	assert.Empty(t, results[0].Links)
	assert.Empty(t, tracker.getCalls, "untracked work is the default path, not a special case")
	assert.Equal(t, 0, stats.Calls)

	assert.Empty(t, primaryOf(t, s, id))
}

func TestMatch_VerifiesEveryCandidateAndDropsUnresolvable(t *testing.T) {
	s := matchStore(t)
	seedNarrative(t, s, "Implement feature", "did the feature work",
		claudeEvent(t, "s1", "feature/GONE-1", "PROJ-42"))

	tracker := &fakeTracker{issues: map[string]tasktracker.Issue{
		"PROJ-42": {Key: "PROJ-42", Summary: "Feature work", StatusName: "In Progress"},
	}}
	llmFake := &fakeLLM{}

	results, _, err := correlator.Match(t.Context(), s, correlator.SingleTracker(tracker), llmFake, matchCfg())
	require.NoError(t, err)
	require.Len(t, results, 1)

	assert.Contains(t, tracker.getCalls, "GONE-1", "the strong-provenance candidate is still verified")
	assert.Equal(t, []string{"GONE-1"}, results[0].Unresolved)
	assert.Equal(t, "PROJ-42", results[0].Primary)
}

func TestMatch_MultipleCandidatesClassifiedByLLM(t *testing.T) {
	s := matchStore(t)
	id := seedNarrative(t, s, "Feature + change task", "engineering plus change management",
		claudeEvent(t, "s1", "feature/PAAS-1", "SUMO-2", "NOPE-3"))

	tracker := &fakeTracker{issues: map[string]tasktracker.Issue{
		"PAAS-1": {Key: "PAAS-1", Summary: "Feature work", StatusName: "In Progress"},
		"SUMO-2": {Key: "SUMO-2", Summary: "Change task", StatusName: "To Do"},
		"NOPE-3": {Key: "NOPE-3", Summary: "Unrelated ticket", StatusName: "Done"},
	}}
	llmFake := &fakeLLM{responses: []string{
		`[{"issue_key":"PAAS-1","role":"primary","confidence":0.9,"rationale":"branch names it"},` +
			`{"issue_key":"SUMO-2","role":"same_work","confidence":0.8,"rationale":"paired change task"},` +
			`{"issue_key":"NOPE-3","role":"mentioned","confidence":0.6,"rationale":"just referenced"}]`,
	}}

	results, stats, err := correlator.Match(t.Context(), s, correlator.SingleTracker(tracker), llmFake, matchCfg())
	require.NoError(t, err)
	require.Len(t, results, 1)

	assert.Equal(t, 1, stats.Calls)
	assert.Equal(t, "PAAS-1", results[0].Primary)

	links, err := s.NarrativeIssues(id)
	require.NoError(t, err)
	require.Len(t, links, 3)

	byKey := make(map[string]store.NarrativeIssue, len(links))
	for _, l := range links {
		byKey[l.IssueKey] = l
	}

	require.Contains(t, byKey, "PAAS-1")
	assert.Equal(t, store.Role("primary"), byKey["PAAS-1"].Role)
	assert.Equal(t, "branch", byKey["PAAS-1"].Provenance,
		"provenance must come from the verified candidate, not the model")

	require.Contains(t, byKey, "SUMO-2")
	assert.Equal(t, store.Role("same_work"), byKey["SUMO-2"].Role)

	require.Contains(t, byKey, "NOPE-3")
	assert.Equal(t, store.Role("mentioned"), byKey["NOPE-3"].Role)
}

func TestMatch_RulesReachTheClassificationSystemPrompt(t *testing.T) {
	s := matchStore(t)
	seedNarrative(t, s, "Feature + change task", "engineering plus change management",
		claudeEvent(t, "s1", "feature/PAAS-1", "SUMO-2"))

	tracker := &fakeTracker{issues: map[string]tasktracker.Issue{
		"PAAS-1": {Key: "PAAS-1", Summary: "Feature work", StatusName: "In Progress"},
		"SUMO-2": {Key: "SUMO-2", Summary: "Change task", StatusName: "To Do"},
	}}
	llmFake := &fakeLLM{responses: []string{
		`[{"issue_key":"PAAS-1","role":"primary","confidence":0.9,"rationale":"r"},` +
			`{"issue_key":"SUMO-2","role":"same_work","confidence":0.8,"rationale":"r"}]`,
	}}
	learnedRules := []rules.Rule{
		{Name: "sumo-change-tasks", Scope: rules.ScopeCorrelator, Confidence: rules.ConfidenceHigh, Body: "Sentinel matching rule body."},
	}

	_, _, err := correlator.Match(t.Context(), s, correlator.SingleTracker(tracker), llmFake, matchCfg(), correlator.WithRules(learnedRules))
	require.NoError(t, err)

	require.Len(t, llmFake.systemPrompts, 1)
	assert.Contains(t, llmFake.systemPrompts[0], "Sentinel matching rule body.")
	assert.Contains(t, llmFake.systemPrompts[0], "sumo-change-tasks")
}

func TestMatch_NoRulesOptionLeavesClassificationSystemPromptUnchanged(t *testing.T) {
	s := matchStore(t)
	seedNarrative(t, s, "Feature + change task", "engineering plus change management",
		claudeEvent(t, "s1", "feature/PAAS-1", "SUMO-2"))

	tracker := &fakeTracker{issues: map[string]tasktracker.Issue{
		"PAAS-1": {Key: "PAAS-1", Summary: "Feature work", StatusName: "In Progress"},
		"SUMO-2": {Key: "SUMO-2", Summary: "Change task", StatusName: "To Do"},
	}}
	llmFake := &fakeLLM{responses: []string{
		`[{"issue_key":"PAAS-1","role":"primary","confidence":0.9,"rationale":"r"},` +
			`{"issue_key":"SUMO-2","role":"same_work","confidence":0.8,"rationale":"r"}]`,
	}}

	_, _, err := correlator.Match(t.Context(), s, correlator.SingleTracker(tracker), llmFake, matchCfg())
	require.NoError(t, err)

	require.Len(t, llmFake.systemPrompts, 1)
	assert.Equal(t, correlator.ClassifySystemPromptForTest(), llmFake.systemPrompts[0])
}

func TestMatch_PromptCarriesSummaryAndDescription(t *testing.T) {
	s := matchStore(t)
	seedNarrative(t, s, "Sentinel narrative title", "narrative summary text",
		claudeEvent(t, "s1", "feature/PAAS-1", "SUMO-2"))

	tracker := &fakeTracker{issues: map[string]tasktracker.Issue{
		"PAAS-1": {
			Key: "PAAS-1", Summary: "sentinel-summary-alpha",
			Description: "sentinel-description-alpha", StatusName: "In Progress",
		},
		"SUMO-2": {
			Key: "SUMO-2", Summary: "sentinel-summary-beta",
			Description: "sentinel-description-beta", StatusName: "To Do",
		},
	}}
	llmFake := &fakeLLM{responses: []string{
		`[{"issue_key":"PAAS-1","role":"primary","confidence":0.9,"rationale":"r"},` +
			`{"issue_key":"SUMO-2","role":"same_work","confidence":0.8,"rationale":"r"}]`,
	}}

	_, _, err := correlator.Match(t.Context(), s, correlator.SingleTracker(tracker), llmFake, matchCfg())
	require.NoError(t, err)

	require.Len(t, llmFake.prompts, 1)
	prompt := llmFake.prompts[0]

	assert.Contains(t, prompt, "Sentinel narrative title")
	assert.Contains(t, prompt, "sentinel-description-alpha")
	assert.Contains(t, prompt, "sentinel-description-beta")
	assert.Contains(t, prompt, "branch", "provenance must be stated as a prior in the prompt")
}

func TestMatch_ConfidenceFloorWithholdsTheAssertionNotTheRecord(t *testing.T) {
	s := matchStore(t)
	id := seedNarrative(t, s, "Feature + change task", "engineering plus change management",
		claudeEvent(t, "s1", "feature/PAAS-1", "SUMO-2"))

	tracker := &fakeTracker{issues: map[string]tasktracker.Issue{
		"PAAS-1": {Key: "PAAS-1", Summary: "Feature work", StatusName: "In Progress"},
		"SUMO-2": {Key: "SUMO-2", Summary: "Change task", StatusName: "To Do"},
	}}
	llmFake := &fakeLLM{responses: []string{
		`[{"issue_key":"PAAS-1","role":"primary","confidence":0.20,"rationale":"weak"},` +
			`{"issue_key":"SUMO-2","role":"same_work","confidence":0.20,"rationale":"weak"}]`,
	}}

	cfg := config.MatchConfig{ConfidenceFloor: 0.5}
	results, _, err := correlator.Match(t.Context(), s, correlator.SingleTracker(tracker), llmFake, cfg)
	require.NoError(t, err)
	require.Len(t, results, 1)

	assert.Empty(t, results[0].Primary,
		"the floor withholds the ASSERTION: MatchResult.Primary is what the renderer prints, and "+
			"an empty one makes it say \"no primary promoted — below confidence floor\"")

	links, err := s.NarrativeIssues(id)
	require.NoError(t, err)
	assert.Len(t, links, 2, "every narrative_issues row must still be written, including the primary")
	assert.Equal(t, "PAAS-1", primaryOf(t, s, id),
		"...and RECORDING is unaffected, which is the distinction the floor draws: dropping the row "+
			"would make a low-confidence match indistinguishable from finding nothing at all")

	// The narrative must NOT come back. This assertion is inverted from what it
	// was, and the inversion is finding F11: it used to require the narrative
	// remain "selectable for a later pass", which sounds prudent and was the bug.
	// A re-match that picked a different primary tripped one_primary_per_narrative
	// and aborted the whole pass — it crashed a real drain. A primary link at any
	// confidence means attributed; the floor governs what unjira asserts, not
	// whether matching is done with it.
	backlog, err := s.NarrativesWithoutPrimaryLink(10)
	require.NoError(t, err)
	assert.Empty(t, backlog,
		"a recorded primary takes the narrative out of matching's backlog whatever its confidence; "+
			"re-matching it forever is what F11 fixed")
}

func TestMatch_ExcludedKeysAreRecordedNotSilentlyDropped(t *testing.T) {
	s := matchStore(t)
	seedNarrative(t, s, "Untracked-looking work", "summary",
		claudeEvent(t, "s1", "", "NOJIRA-1"))

	compiled, err := events.CompileLinkExclusionPatterns([]string{`^NOJIRA-\d+$`})
	require.NoError(t, err)

	tracker := &fakeTracker{issues: map[string]tasktracker.Issue{}}
	llmFake := &fakeLLM{}

	results, _, err := correlator.Match(
		t.Context(), s, correlator.SingleTracker(tracker), llmFake, matchCfg(), correlator.WithLinkExclusions(compiled),
	)
	require.NoError(t, err)
	require.Len(t, results, 1)

	assert.Empty(t, results[0].Primary)
	assert.Equal(t, []string{"NOJIRA-1"}, results[0].Excluded)
	assert.Empty(t, tracker.getCalls, "an excluded key is never verified")
}

func TestMatch_OneNarrativeFailingDoesNotStopTheOthers(t *testing.T) {
	s := matchStore(t)
	failingID := seedNarrative(t, s, "Failing narrative", "summary",
		claudeEvent(t, "s1", "feature/BOOM-1"))
	healthyID := seedNarrative(t, s, "Healthy narrative", "summary",
		claudeEvent(t, "s2", "feature/OK-1"))

	tracker := &fakeTracker{
		issues: map[string]tasktracker.Issue{
			"OK-1": {Key: "OK-1", Summary: "fine", StatusName: "To Do"},
		},
		getErr: map[string]error{
			"BOOM-1": &jira.Error{Status: 500, Message: "internal error"},
		},
	}
	llmFake := &fakeLLM{}

	_, _, err := correlator.Match(t.Context(), s, correlator.SingleTracker(tracker), llmFake, matchCfg())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "BOOM-1")

	assert.Equal(t, "OK-1", primaryOf(t, s, healthyID), "the healthy narrative must still resolve")
	assert.Empty(t, primaryOf(t, s, failingID), "the failing narrative must stay unmatched for a retry")
}

func TestMatch_IsIdempotent(t *testing.T) {
	s := matchStore(t)
	id := seedNarrative(t, s, "Implement feature", "did the feature work",
		claudeEvent(t, "s1", "feature/PROJ-42"))

	tracker := &fakeTracker{issues: map[string]tasktracker.Issue{
		"PROJ-42": {Key: "PROJ-42", Summary: "Feature work", StatusName: "In Progress"},
	}}
	llmFake := &fakeLLM{}

	_, _, err := correlator.Match(t.Context(), s, correlator.SingleTracker(tracker), llmFake, matchCfg())
	require.NoError(t, err)

	_, _, err = correlator.Match(t.Context(), s, correlator.SingleTracker(tracker), llmFake, matchCfg())
	require.NoError(t, err)

	links, err := s.NarrativeIssues(id)
	require.NoError(t, err)
	assert.Len(t, links, 1, "a second pass over an emptied backlog must be a no-op")
}
