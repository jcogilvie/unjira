package pipeline_test

// match_test.go exercises RunMatch against a real temp-file store and a
// fake llm.Client/tasktracker.TaskTracker — the orchestration (validating
// config, compiling exclusions, threading them into correlator.Match, and
// returning partial results alongside an error) is what's under test here,
// not Match's own matching logic, which internal/correlator covers.
//
// RenderMatchResult's tests are pure and need neither a store nor an LLM.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/clients/jira"
	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/llm"
	"github.com/jcogilvie/unjira/internal/pipeline"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// pipelineFakeTracker satisfies tasktracker.TaskTracker without a network
// call. Named distinctly from internal/correlator's file-scoped fakeTracker,
// which is unexported and not visible from this package.
type pipelineFakeTracker struct {
	issues   map[string]tasktracker.Issue
	getCalls []string
}

func (f *pipelineFakeTracker) GetIssue(key string) (tasktracker.Issue, error) {
	f.getCalls = append(f.getCalls, key)

	issue, ok := f.issues[key]
	if !ok {
		return tasktracker.Issue{}, &jira.Error{Status: 404, Message: "issue does not exist"}
	}

	return issue, nil
}

func (f *pipelineFakeTracker) SearchIssues(string, int) ([]tasktracker.Issue, error) { return nil, nil }
func (f *pipelineFakeTracker) AddComment(string, string) error                       { return nil }
func (f *pipelineFakeTracker) SetStatus(string, tasktracker.StatusCategory) error    { return nil }
func (f *pipelineFakeTracker) CreateIssue(_, _, _, _ string, _ []string) (string, error) {
	return "", nil
}

var _ tasktracker.TaskTracker = (*pipelineFakeTracker)(nil)

// pipelineFakeLLM is a fake llm.Client returning canned responses in call
// order, falling back to an empty classification once exhausted. Named
// distinctly from internal/correlator's file-scoped fakeLLM, which is
// unexported and not visible from this package.
type pipelineFakeLLM struct {
	responses []string
	prompts   []string
	// systemPrompts mirrors prompts for the system half, so a test can
	// assert rules text loaded from config.Config.RulesDir reached the
	// actual classification system prompt Match sends, not merely that
	// Load succeeded.
	systemPrompts []string
}

func (f *pipelineFakeLLM) Complete(_ context.Context, systemPrompt, userPrompt string) (string, llm.Usage, error) {
	f.prompts = append(f.prompts, userPrompt)
	f.systemPrompts = append(f.systemPrompts, systemPrompt)
	if len(f.responses) == 0 {
		return "[]", llm.Usage{}, nil
	}
	out := f.responses[0]
	if len(f.responses) > 1 {
		f.responses = f.responses[1:]
	}

	return out, llm.Usage{}, nil
}

var _ llm.Client = (*pipelineFakeLLM)(nil)

// matchPipelineStore opens a fresh temp-file store for a RunMatch test.
func matchPipelineStore(t *testing.T) *store.Store {
	t.Helper()

	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	return s
}

// seedMatchNarrative inserts a narrative with the given title/summary and
// links evts to it (each first inserted into the events table), returning
// the narrative's row id.
func seedMatchNarrative(t *testing.T, s *store.Store, title, summary string, evts ...events.Event) int64 {
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

func TestRunMatch_ReturnsRenderableResult(t *testing.T) {
	s := matchPipelineStore(t)

	e := events.NewEvent("claude_code", "s1",
		time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC), "session summary")
	e.Artifacts["git_branch"] = "feature/PROJ-42"

	id := seedMatchNarrative(t, s, "Implement feature", "did the feature work", e)

	tracker := &pipelineFakeTracker{issues: map[string]tasktracker.Issue{
		"PROJ-42": {Key: "PROJ-42", Summary: "Feature work", StatusName: "In Progress"},
	}}
	llmFake := &pipelineFakeLLM{}
	cfg := config.Config{Match: config.MatchConfig{MaxCandidatesPerNarrative: 10, ConfidenceFloor: 0.5}}

	got, err := pipeline.RunMatch(t.Context(), s, tracker, llmFake, cfg)

	require.NoError(t, err)
	require.Len(t, got.Matched, 1)
	assert.Equal(t, id, got.Matched[0].NarrativeID)
	assert.Equal(t, "PROJ-42", got.Matched[0].Primary)
}

func TestRunMatch_LoadsRulesAndAppendsThemToClassificationSystemPrompt(t *testing.T) {
	s := matchPipelineStore(t)

	e := events.NewEvent("claude_code", "s1",
		time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC), "session summary")
	e.Artifacts["git_branch"] = "feature/PAAS-1"
	e.Artifacts["ticket_keys"] = []any{"SUMO-2"}

	seedMatchNarrative(t, s, "Feature + change task", "engineering plus change management", e)

	tracker := &pipelineFakeTracker{issues: map[string]tasktracker.Issue{
		"PAAS-1": {Key: "PAAS-1", Summary: "Feature work", StatusName: "In Progress"},
		"SUMO-2": {Key: "SUMO-2", Summary: "Change task", StatusName: "To Do"},
	}}
	llmFake := &pipelineFakeLLM{responses: []string{
		`[{"issue_key":"PAAS-1","role":"primary","confidence":0.9,"rationale":"r"},` +
			`{"issue_key":"SUMO-2","role":"same_work","confidence":0.8,"rationale":"r"}]`,
	}}

	rulesDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(rulesDir, "sentinel.md"), []byte(`---
scope: correlator
confidence: high
learned: 2026-07-16
source: test
---

Sentinel match-time rule body.
`), 0o600))

	cfg := config.Config{
		Match: config.MatchConfig{MaxCandidatesPerNarrative: 10, ConfidenceFloor: 0.5},
		Rules: config.RulesConfig{Dir: rulesDir},
	}

	_, err := pipeline.RunMatch(t.Context(), s, tracker, llmFake, cfg)

	require.NoError(t, err)
	require.NotEmpty(t, llmFake.systemPrompts)
	assert.Contains(t, llmFake.systemPrompts[0], "Sentinel match-time rule body.")
	assert.Contains(t, llmFake.systemPrompts[0], "sentinel")
}

func TestRunMatch_MissingRulesDirIsANoOpNotAnError(t *testing.T) {
	s := matchPipelineStore(t)

	e := events.NewEvent("claude_code", "s1",
		time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC), "session summary")
	e.Artifacts["git_branch"] = "feature/PROJ-42"
	seedMatchNarrative(t, s, "Implement feature", "did the feature work", e)

	tracker := &pipelineFakeTracker{issues: map[string]tasktracker.Issue{
		"PROJ-42": {Key: "PROJ-42", Summary: "Feature work", StatusName: "In Progress"},
	}}
	llmFake := &pipelineFakeLLM{}
	cfg := config.Config{
		Match: config.MatchConfig{MaxCandidatesPerNarrative: 10, ConfidenceFloor: 0.5},
		Rules: config.RulesConfig{Dir: filepath.Join(t.TempDir(), "does-not-exist")},
	}

	got, err := pipeline.RunMatch(t.Context(), s, tracker, llmFake, cfg)

	require.NoError(t, err)
	require.Len(t, got.Matched, 1)
	assert.Equal(t, "PROJ-42", got.Matched[0].Primary)
}

func TestRunMatch_AppliesConfiguredLinkExclusions(t *testing.T) {
	s := matchPipelineStore(t)

	e := events.NewEvent("claude_code", "s1",
		time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC), "session summary")
	// []any deliberately: the collector stores it that way, and a []string
	// fixture would make the candidate silently vanish and this test would
	// pass for the wrong reason.
	e.Artifacts["ticket_keys"] = []any{"NOJIRA-1"}

	seedMatchNarrative(t, s, "Placeholder work", "used the placeholder ticket", e)

	tracker := &pipelineFakeTracker{issues: map[string]tasktracker.Issue{}}
	llmFake := &pipelineFakeLLM{}
	cfg := config.Config{
		ExcludeFromLinking: []string{`^NOJIRA-\d+$`},
		Match:              config.MatchConfig{MaxCandidatesPerNarrative: 10, ConfidenceFloor: 0.5},
	}

	got, err := pipeline.RunMatch(t.Context(), s, tracker, llmFake, cfg)

	require.NoError(t, err)
	require.Len(t, got.Matched, 1)
	assert.Equal(t, []string{"NOJIRA-1"}, got.Matched[0].Excluded)
	assert.Empty(t, tracker.getCalls, "an excluded placeholder must never be verified")
}

func TestRunMatch_RejectsInvalidMatchConfig(t *testing.T) {
	s := matchPipelineStore(t)

	e := events.NewEvent("claude_code", "s1",
		time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC), "session summary")
	e.Artifacts["git_branch"] = "feature/PROJ-42"
	seedMatchNarrative(t, s, "Implement feature", "did the feature work", e)

	tracker := &pipelineFakeTracker{issues: map[string]tasktracker.Issue{
		"PROJ-42": {Key: "PROJ-42"},
	}}
	llmFake := &pipelineFakeLLM{}
	cfg := config.Config{Match: config.MatchConfig{ConfidenceFloor: 1.5}}

	_, err := pipeline.RunMatch(t.Context(), s, tracker, llmFake, cfg)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "confidence_floor")
	assert.Empty(t, tracker.getCalls, "an invalid config must fail before any network call")
}

func TestRenderMatchResult_ShowsLinksAndReasons(t *testing.T) {
	result := pipeline.MatchRunResult{
		Matched: []correlator.MatchResult{
			{
				NarrativeID: 7,
				Primary:     "PAAS-1",
				Links: []store.NarrativeIssue{
					{IssueKey: "PAAS-1", Role: "primary", Provenance: "branch", Confidence: 0.93},
					{IssueKey: "SUMO-2", Role: "same_work", Provenance: "prose_first", Confidence: 0.85},
				},
				Rationale: "branch name matches the primary ticket; prose also references SUMO-2",
			},
			{
				NarrativeID: 8,
				Excluded:    []string{"NOJIRA-9"},
			},
		},
	}

	out := pipeline.RenderMatchResult(result)

	assert.Contains(t, out, "PAAS-1")
	assert.Contains(t, out, "primary")
	assert.Contains(t, out, "SUMO-2")
	assert.Contains(t, out, "same_work")
	assert.Contains(t, out, "branch")
	assert.Contains(t, out, "0.93")
	assert.Contains(t, out, "NOJIRA-9")
	assert.Contains(t, out, "unmatched")
}

func TestRenderMatchResult_DistinguishesWhyUnmatched(t *testing.T) {
	result := pipeline.MatchRunResult{
		Matched: []correlator.MatchResult{
			{NarrativeID: 1, Excluded: []string{"NOJIRA-1"}},
			{NarrativeID: 2, Unresolved: []string{"STALE-9"}},
			{NarrativeID: 3},
		},
	}

	out := pipeline.RenderMatchResult(result)

	assert.Contains(t, out, "excluded")
	assert.Contains(t, out, "unresolved")
	assert.Contains(t, out, "no candidate")
}

func TestRenderMatchResult_MarksAnUnpromotedPrimary(t *testing.T) {
	result := pipeline.MatchRunResult{
		Matched: []correlator.MatchResult{
			{
				NarrativeID: 4,
				Primary:     "",
				Links: []store.NarrativeIssue{
					{IssueKey: "PAAS-1", Role: "primary", Provenance: "branch", Confidence: 0.2},
				},
			},
		},
	}

	out := pipeline.RenderMatchResult(result)

	assert.Contains(t, out, "PAAS-1")
	assert.Contains(t, out, "no primary promoted")
}
