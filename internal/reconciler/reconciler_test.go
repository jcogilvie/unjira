package reconciler

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/clients/jira"
	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/llm"
	"github.com/jcogilvie/unjira/internal/rules"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// fakeTracker records every call, including writes.
//
// Distinct from internal/correlator/match_test.go's fakeTracker, which stubs
// the write methods as silent no-ops because matching never writes. This slice
// proposes actions and must never apply them, so a test needs to be able to
// assert that no write happened — a silent no-op cannot express that.
type fakeTracker struct {
	issues      map[string]tasktracker.Issue
	getErr      map[string]error
	transitions map[string][]tasktracker.Transition

	getCalls   []string
	writeCalls []string // any mutating call, for the never-writes assertion
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

func (f *fakeTracker) AvailableTransitions(key string) ([]tasktracker.Transition, error) {
	if t, ok := f.transitions[key]; ok {
		return t, nil
	}

	return nil, nil
}

func (f *fakeTracker) SearchIssues(string, int) ([]tasktracker.Issue, error) { return nil, nil }

func (f *fakeTracker) AddComment(key, _ string) error {
	f.writeCalls = append(f.writeCalls, "AddComment:"+key)

	return nil
}

func (f *fakeTracker) SetStatus(key, _ string) error {
	f.writeCalls = append(f.writeCalls, "SetStatus:"+key)

	return nil
}

func (f *fakeTracker) CreateIssue(project, _, _, _ string, _ []string) (string, error) {
	f.writeCalls = append(f.writeCalls, "CreateIssue:"+project)

	return "NEW-1", nil
}

var _ tasktracker.TaskTracker = (*fakeTracker)(nil)

// The reconciler is handed only a reader. This assertion is the structural
// guarantee that it cannot apply an action: if a future change widens the
// parameter to TaskTracker, TestReconcileNeverWritesToTheTracker is the
// runtime backstop, but this is what makes a write fail to compile.
var _ tasktracker.TaskReader = (*fakeTracker)(nil)

// fakeLLM satisfies llm.Client without making any real call, mirroring
// internal/correlator/correlator_test.go's fakeLLM. responses is consumed in
// call order; a test that only cares about one canned response can set a
// single-element slice. usagePerCall is returned from every call, so a test
// can assert Stats aggregates correctly.
type fakeLLM struct {
	responses []string
	prompts   []string // captured user prompts, in call order, for assertions
	// systemPrompts mirrors prompts for the system half.
	systemPrompts []string
	err           error
	usagePerCall  llm.Usage
}

func (f *fakeLLM) Complete(_ context.Context, systemPrompt, userPrompt string) (string, llm.Usage, error) {
	f.prompts = append(f.prompts, userPrompt)
	f.systemPrompts = append(f.systemPrompts, systemPrompt)
	if f.err != nil {
		return "", llm.Usage{}, f.err
	}

	idx := len(f.prompts) - 1
	if idx >= len(f.responses) {
		idx = len(f.responses) - 1
	}
	if idx < 0 {
		return "", f.usagePerCall, nil
	}

	return f.responses[idx], f.usagePerCall, nil
}

// testConfig returns a valid config.ReconcilerConfig for tests that don't
// care about tuning it further.
func testConfig() config.ReconcilerConfig {
	return config.ReconcilerConfig{MaxNarrativesPerPass: 10, MinConfidenceToPropose: 0.5}
}

// reconcileStore opens a fresh temp-file store.
func reconcileStore(t *testing.T) *store.Store {
	t.Helper()

	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	return s
}

// seedLinkedNarrative inserts a narrative, links evts to it, and attaches one
// link of the given role. Pass an empty issueKey to leave the narrative
// entirely unlinked.
func seedLinkedNarrative(t *testing.T, s *store.Store, issueKey string, role store.Role, evts ...events.Event) int64 {
	t.Helper()

	base := time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)
	id, err := s.InsertNarrative(base, base.Add(time.Hour), "did the work", "summary of the work")
	require.NoError(t, err)

	ids := make([]int64, 0, len(evts))
	for _, e := range evts {
		_, err := s.InsertEvent(e)
		require.NoError(t, err)
		eid, err := s.EventIDByExternalID(e.Source, e.ExternalID)
		require.NoError(t, err)
		ids = append(ids, eid)
	}
	require.NoError(t, s.AddNarrativeEvents(id, ids))

	if issueKey != "" {
		require.NoError(t, s.WithTx(func(tx *store.Tx) error {
			return tx.AddNarrativeIssues(id, []store.NarrativeIssue{{
				IssueKey:   issueKey,
				Role:       role,
				Provenance: "branch",
				Confidence: 0.9,
				Connection: "dev",
			}})
		}))
	}

	return id
}

// codeEvent builds a non-Jira event (so authored_by_unjira is absent).
func codeEvent(externalID, summary string) events.Event {
	e := events.NewEvent("claude_code", externalID,
		time.Date(2026, 8, 25, 9, 30, 0, 0, time.UTC), summary)

	return e
}

// unjiraComment builds a Jira event unjira itself authored.
func unjiraComment(externalID string) events.Event {
	e := events.NewEvent("jira", externalID,
		time.Date(2026, 8, 25, 9, 45, 0, 0, time.UTC), "a comment unjira posted")
	e.Artifacts["authored_by_unjira"] = true
	e.Artifacts["issue_key"] = "PROJ-1"

	return e
}

func TestReconcileProposesNothingForAMentionedOnlyNarrative(t *testing.T) {
	// A narrative the reconciler must not act on still yields a result row:
	// no tracker reads, no LLM spend. `mentioned` is a citation, not
	// something the reconciler may comment on or transition.
	s := reconcileStore(t)
	tracker := &fakeTracker{}
	llmClient := &fakeLLM{}

	seedLinkedNarrative(t, s, "PROJ-9", store.Role("mentioned"), codeEvent("e1", "wrote some code"))

	results, _, err := Reconcile(t.Context(), s, tracker, llmClient, testConfig())
	require.NoError(t, err)

	require.Len(t, results, 1)
	assert.Empty(t, results[0].Proposed,
		"a mentioned-only link is a citation, not something the reconciler may act on")
	assert.Empty(t, tracker.getCalls, "no actionable links means no tracker reads")
	assert.Empty(t, llmClient.prompts, "and no LLM spend")
}

func TestReconcileSkipsANarrativeWithNoLinks(t *testing.T) {
	// A genuinely link-free narrative is matching's backlog, not the
	// reconciler's: it must simply not appear in results at all, since
	// NarrativesWithActionableLinks never selects it in the first place.
	s := reconcileStore(t)
	tracker := &fakeTracker{}
	llmClient := &fakeLLM{}

	seedLinkedNarrative(t, s, "", "", codeEvent("e1", "wrote some code"))

	results, _, err := Reconcile(t.Context(), s, tracker, llmClient, testConfig())
	require.NoError(t, err)

	assert.Empty(t, results,
		"an unlinked narrative is matching's concern, not the reconciler's, and must not be selected")
	assert.Empty(t, tracker.getCalls, "no links means no tracker reads")
	assert.Empty(t, llmClient.prompts, "and no LLM spend")
}

func TestReconcileSkipsWhenTheDeltaIsEmpty(t *testing.T) {
	s := reconcileStore(t)
	tracker := &fakeTracker{
		issues: map[string]tasktracker.Issue{
			"PROJ-1": {Key: "PROJ-1", Summary: "the ticket", StatusCategory: tasktracker.StatusInProgress},
		},
	}

	nid := seedLinkedNarrative(t, s, "PROJ-1", store.Role("primary"), codeEvent("e1", "wrote some code"))

	// A prior action already covers every linked event.
	_, err := s.InsertAction(store.ActionRow{
		NarrativeID: nid, Type: "comment", IssueKey: "PROJ-1",
		Payload: `{"body":"already said this"}`, Status: "proposed",
	})
	require.NoError(t, err)

	llmClient := &fakeLLM{}
	results, _, err := Reconcile(t.Context(), s, tracker, llmClient, testConfig())
	require.NoError(t, err)

	require.Len(t, results, 1)
	assert.True(t, results[0].SkippedNoDelta)
	assert.Empty(t, results[0].Proposed)
	assert.Empty(t, llmClient.prompts,
		"an empty delta must short-circuit BEFORE the LLM call — this is what stops a "+
			"re-run proposing the same comment twice")
}

func TestReconcileDropsAnUnverifiableLinkWithoutFailingTheNarrative(t *testing.T) {
	s := reconcileStore(t)
	// PROJ-404 resolves nowhere: fakeTracker returns a 404 for unknown keys.
	tracker := &fakeTracker{issues: map[string]tasktracker.Issue{}}

	seedLinkedNarrative(t, s, "PROJ-404", store.Role("primary"), codeEvent("e1", "wrote some code"))

	llmClient := &fakeLLM{}
	results, _, err := Reconcile(t.Context(), s, tracker, llmClient, testConfig())

	require.NoError(t, err, "a not-found link is recorded, not an error")
	require.Len(t, results, 1)
	assert.Equal(t, []string{"PROJ-404"}, results[0].Unverified)
	assert.Empty(t, results[0].Proposed)
	assert.Empty(t, llmClient.prompts,
		"verification runs BEFORE drafting: nothing verified means nothing to draft against")
}

func TestReconcileFailsTheNarrativeOnATransportError(t *testing.T) {
	s := reconcileStore(t)
	tracker := &fakeTracker{
		getErr: map[string]error{
			"PROJ-1": &jira.Error{Status: 503, Message: "service unavailable"},
		},
	}

	seedLinkedNarrative(t, s, "PROJ-1", store.Role("primary"), codeEvent("e1", "wrote some code"))

	llmClient := &fakeLLM{}
	_, _, err := Reconcile(t.Context(), s, tracker, llmClient, testConfig())

	require.Error(t, err,
		"an unreachable tracker must fail this narrative for retry, not record a "+
			"conclusion drawn from an unreachable tracker")
	assert.Empty(t, llmClient.prompts)
}

// TestReconcileRecordsButDoesNotDropALowConfidenceProposal pins
// MinConfidenceToPropose's semantics. Dropping the action would make a weak
// proposal indistinguishable from "nothing to do", and slice 6's triage needs
// to see it in order to judge it.
func TestReconcileRecordsButDoesNotDropALowConfidenceProposal(t *testing.T) {
	s := reconcileStore(t)
	tracker := &fakeTracker{
		issues: map[string]tasktracker.Issue{
			"PROJ-1": {Key: "PROJ-1", Summary: "the ticket"},
		},
	}

	seedLinkedNarrative(t, s, "PROJ-1", store.Role("primary"), codeEvent("e1", "did some work"))

	llmClient := &fakeLLM{responses: []string{
		`[{"issue_key":"PROJ-1","type":"comment","body":"maybe related","confidence":0.2,"rationale":"weak signal"}]`,
	}}

	cfg := config.ReconcilerConfig{MaxNarrativesPerPass: 10, MinConfidenceToPropose: 0.5}
	results, _, err := Reconcile(t.Context(), s, tracker, llmClient, cfg)
	require.NoError(t, err)

	require.Len(t, results, 1)
	require.Len(t, results[0].Proposed, 1,
		"a below-threshold action is still proposed and still persisted; the threshold "+
			"governs what unjira asserts, not what it records")
	assert.InDelta(t, 0.2, results[0].Proposed[0].Confidence, 0.0001)
	require.Len(t, results[0].LowConfidence, 1,
		"and it must be flagged, or a weak proposal reads as a confident one")
	assert.Contains(t, results[0].LowConfidence[0], "min_confidence_to_propose")
}

func TestReconcileNeverWritesToTheTracker(t *testing.T) {
	s := reconcileStore(t)
	tracker := &fakeTracker{
		issues: map[string]tasktracker.Issue{
			"PROJ-1": {Key: "PROJ-1", Summary: "the ticket", StatusCategory: tasktracker.StatusInProgress},
		},
	}

	seedLinkedNarrative(t, s, "PROJ-1", store.Role("primary"), codeEvent("e1", "wrote some code"))

	llmClient := &fakeLLM{responses: []string{
		`[{"issue_key":"PROJ-1","type":"comment","body":"the work landed","confidence":0.9,"rationale":"delta shows it"}]`,
	}}

	_, _, err := Reconcile(t.Context(), s, tracker, llmClient, testConfig())
	require.NoError(t, err)

	assert.Empty(t, tracker.writeCalls,
		"this slice proposes and never applies; any write here is an architecture violation")
}

func TestReconcileWithNoOptionsIsValid(t *testing.T) {
	// Mirrors correlator.Match/Cluster's "zero options is valid" guarantee:
	// Reconcile must not require WithRules (or any future option) to be
	// called.
	s := reconcileStore(t)
	tracker := &fakeTracker{
		issues: map[string]tasktracker.Issue{
			"PROJ-1": {Key: "PROJ-1", Summary: "the ticket", StatusCategory: tasktracker.StatusInProgress},
		},
	}

	seedLinkedNarrative(t, s, "PROJ-1", store.Role("primary"), codeEvent("e1", "wrote some code"))

	llmClient := &fakeLLM{responses: []string{
		`[{"issue_key":"PROJ-1","type":"comment","body":"the work landed","confidence":0.9,"rationale":"delta shows it"}]`,
	}}

	results, _, err := Reconcile(t.Context(), s, tracker, llmClient, testConfig())
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Len(t, results[0].Proposed, 1)
}

// TestReconcileWithRulesAppendsThemToTheDraftingSystemPrompt is the
// end-to-end proof that a scope:reconciler rule reaches the LLM call
// Reconcile ultimately makes via draft, through WithRules — not merely
// through draft's own unit tests in draft_test.go.
func TestReconcileWithRulesAppendsThemToTheDraftingSystemPrompt(t *testing.T) {
	s := reconcileStore(t)
	tracker := &fakeTracker{
		issues: map[string]tasktracker.Issue{
			"PROJ-1": {Key: "PROJ-1", Summary: "the ticket", StatusCategory: tasktracker.StatusInProgress},
		},
	}

	seedLinkedNarrative(t, s, "PROJ-1", store.Role("primary"), codeEvent("e1", "wrote some code"))

	llmClient := &fakeLLM{responses: []string{
		`[{"issue_key":"PROJ-1","type":"comment","body":"the work landed","confidence":0.9,"rationale":"delta shows it"}]`,
	}}
	learnedRules := []rules.Rule{
		{
			Name: "bot-pr-noise", Scope: rules.ScopeReconciler, Confidence: rules.ConfidenceHigh,
			Body: "Sentinel reconciler rule body.",
		},
	}

	_, _, err := Reconcile(t.Context(), s, tracker, llmClient, testConfig(), WithRules(learnedRules))
	require.NoError(t, err)

	require.Len(t, llmClient.systemPrompts, 1)
	assert.Contains(t, llmClient.systemPrompts[0], "Sentinel reconciler rule body.")
}

func TestReconcileDropsSelfAuthoredEventsBeforeComputingTheDelta(t *testing.T) {
	// Without dropSelfAuthored, a Jira comment unjira itself posted would
	// count as new delta forever, and the reconciler would propose
	// commenting about its own comment every pass.
	s := reconcileStore(t)
	tracker := &fakeTracker{
		issues: map[string]tasktracker.Issue{
			"PROJ-1": {Key: "PROJ-1", Summary: "the ticket"},
		},
	}

	nid := seedLinkedNarrative(t, s, "PROJ-1", store.Role("primary"))

	// Only a self-authored event is linked: after filtering, the delta is
	// empty.
	unjiraEvt := unjiraComment("c1")
	_, err := s.InsertEvent(unjiraEvt)
	require.NoError(t, err)
	eid, err := s.EventIDByExternalID(unjiraEvt.Source, unjiraEvt.ExternalID)
	require.NoError(t, err)
	require.NoError(t, s.AddNarrativeEvents(nid, []int64{eid}))

	llmClient := &fakeLLM{}
	results, _, err := Reconcile(t.Context(), s, tracker, llmClient, testConfig())
	require.NoError(t, err)

	require.Len(t, results, 1)
	assert.True(t, results[0].SkippedNoDelta,
		"a delta consisting only of self-authored events must be treated as empty")
	assert.Empty(t, tracker.getCalls, "verification never runs when the delta is empty")
	assert.Empty(t, llmClient.prompts)
}
