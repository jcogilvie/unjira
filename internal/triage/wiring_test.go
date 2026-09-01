package triage

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/llm"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// wireTracker is a TaskReader that records reads and can be told an issue does
// not exist. It implements ONLY the reader half deliberately: StoreHandler holds
// a TaskReader, so a write from the handler would not compile, and this fake
// cannot accidentally provide one.
type wireTracker struct {
	issues   map[string]tasktracker.Issue
	getCalls []string
}

func (w *wireTracker) GetIssue(key string) (tasktracker.Issue, error) {
	w.getCalls = append(w.getCalls, key)

	issue, ok := w.issues[key]
	if !ok {
		return tasktracker.Issue{}, &notFoundError{key: key}
	}

	return issue, nil
}

func (w *wireTracker) AvailableStatusCategories(string) ([]tasktracker.StatusCategory, error) {
	return nil, nil
}

func (w *wireTracker) SearchIssues(string, int) ([]tasktracker.Issue, error) { return nil, nil }

var _ tasktracker.TaskReader = (*wireTracker)(nil)

type notFoundError struct{ key string }

func (e *notFoundError) Error() string { return "issue " + e.key + " does not exist" }

// wireLLM returns canned responses and captures the prompts it was given.
type wireLLM struct {
	responses []string
	prompts   []string
	// systemPrompts mirrors prompts for the system half, so split's tests can
	// assert the reviewer's instruction actually reached the model rather than
	// only that a call happened.
	systemPrompts []string
}

func (c *wireLLM) Complete(_ context.Context, systemPrompt, userPrompt string) (string, llm.Usage, error) {
	c.prompts = append(c.prompts, userPrompt)
	c.systemPrompts = append(c.systemPrompts, systemPrompt)

	idx := len(c.prompts) - 1
	if idx >= len(c.responses) {
		idx = len(c.responses) - 1
	}
	if idx < 0 {
		return "", llm.Usage{}, nil
	}

	return c.responses[idx], llm.Usage{}, nil
}

// wireStore opens a store and seeds one linked narrative with one proposed
// comment action, returning both ids.
func wireStore(t *testing.T) (*store.Store, int64, store.ActionRow) {
	t.Helper()

	s, err := store.Open(filepath.Join(t.TempDir(), "wire.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	base := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)
	nid, err := s.InsertNarrative(base, base.Add(time.Hour), "shipped retries", "summary")
	require.NoError(t, err)

	e := events.NewEvent("claude_code", "wire:1", base, "added the retry loop")
	_, err = s.InsertEvent(e)
	require.NoError(t, err)
	eid, err := s.EventIDByExternalID("claude_code", "wire:1")
	require.NoError(t, err)
	require.NoError(t, s.AddNarrativeEvents(nid, []int64{eid}))

	require.NoError(t, s.WithTx(func(tx *store.Tx) error {
		return tx.AddNarrativeIssues(nid, []store.NarrativeIssue{{
			IssueKey: "DEVSBX-9", Role: store.Role("primary"),
			Provenance: "branch", Confidence: 0.9, Connection: "dev",
		}})
	}))

	aid, err := s.InsertAction(store.ActionRow{
		NarrativeID: nid, Type: "comment", IssueKey: "DEVSBX-9",
		Payload: `{"body":"first draft"}`, Confidence: 0.9, Status: "proposed",
	})
	require.NoError(t, err)

	action, err := s.GetAction(aid)
	require.NoError(t, err)

	return s, nid, action
}

// TestStoreHandlerRedraft_PersistsTheReplacementSoItCanBeApplied is the defect
// this wiring had to avoid. An in-memory replacement has ID 0, and gate.Applier
// calls UpdateActionStatusAndError(action.ID, ...) — which matches no rows. The
// failure would land at APPLY time, after the reviewer approved the new text.
func TestStoreHandlerRedraft_PersistsTheReplacementSoItCanBeApplied(t *testing.T) {
	s, _, action := wireStore(t)

	tracker := &wireTracker{issues: map[string]tasktracker.Issue{
		"DEVSBX-9": {Key: "DEVSBX-9", StatusName: "In Progress"},
	}}
	client := &wireLLM{responses: []string{
		`[{"issue_key":"DEVSBX-9","type":"comment","body":"Shorter.","confidence":0.9,"rationale":"r"}]`,
	}}

	h := NewStoreHandler(s, tracker, client, nil, testCorrelatorConfig(), 100000)

	got, err := h.Redraft(context.Background(), action, "make it shorter")

	require.NoError(t, err)
	require.NotZero(t, got.ID, "an unpersisted replacement cannot be applied: gate.Applier needs its id")

	fresh, err := s.GetAction(got.ID)
	require.NoError(t, err)
	assert.JSONEq(t, `{"body":"Shorter."}`, fresh.Payload)
	assert.Equal(t, "proposed", fresh.Status)
}

// TestStoreHandlerRedraft_RulesOnTheSupersededRow: without this the original sits
// at proposed forever, so the reviewer sees BOTH the old and new text next
// session — and unjira could post both.
func TestStoreHandlerRedraft_RulesOnTheSupersededRow(t *testing.T) {
	s, _, action := wireStore(t)

	tracker := &wireTracker{issues: map[string]tasktracker.Issue{
		"DEVSBX-9": {Key: "DEVSBX-9", StatusName: "In Progress"},
	}}
	client := &wireLLM{responses: []string{
		`[{"issue_key":"DEVSBX-9","type":"comment","body":"Shorter.","confidence":0.9,"rationale":"r"}]`,
	}}

	h := NewStoreHandler(s, tracker, client, nil, testCorrelatorConfig(), 100000)

	_, err := h.Redraft(context.Background(), action, "make it shorter")
	require.NoError(t, err)

	old, err := s.GetAction(action.ID)
	require.NoError(t, err)
	assert.Equal(t, "edited", old.Status)
	assert.Equal(t, "make it shorter", old.Feedback,
		"slice 7's rules.Distill reads actions.feedback")

	live, err := s.ActionsByStatus("proposed")
	require.NoError(t, err)
	assert.Len(t, live, 1, "exactly one live proposal, or unjira could post both texts")
}

// TestStoreHandlerRedraft_PayloadIsReadableByTheApplier: gate.Applier decodes
// with typed structs mirroring reconciler's encoding. A second encoder in triage
// would be a silent divergence — a payload only discovered unreadable when a
// reviewer approved it.
func TestStoreHandlerRedraft_PayloadIsReadableByTheApplier(t *testing.T) {
	s, _, action := wireStore(t)

	tracker := &wireTracker{issues: map[string]tasktracker.Issue{
		"DEVSBX-9": {Key: "DEVSBX-9", StatusName: "In Progress"},
	}}
	// A body containing quotes and a newline: the case string concatenation
	// would corrupt.
	client := &wireLLM{responses: []string{
		`[{"issue_key":"DEVSBX-9","type":"comment","body":"He said \"ship it\".\nThen we did.","confidence":0.9,"rationale":"r"}]`,
	}}

	h := NewStoreHandler(s, tracker, client, nil, testCorrelatorConfig(), 100000)

	got, err := h.Redraft(context.Background(), action, "quote him")
	require.NoError(t, err)

	var decoded struct {
		Body string `json:"body"`
	}
	require.NoError(t, json.Unmarshal([]byte(got.Payload), &decoded))
	assert.Equal(t, "He said \"ship it\".\nThen we did.", decoded.Body,
		"the payload must round-trip through the same shape gate.Applier decodes")
}

// TestStoreHandlerRedraft_RefusesWhenTheRedraftSkipsTheEditedIssue: a redraft
// covers the whole narrative, so a same_work pair yields two actions while the
// reviewer edited one. Substituting a sibling would post text about a different
// audience's ticket.
func TestStoreHandlerRedraft_RefusesWhenTheRedraftSkipsTheEditedIssue(t *testing.T) {
	s, nid, action := wireStore(t)

	// Add a same_work link so the model can legitimately name the other issue.
	require.NoError(t, s.WithTx(func(tx *store.Tx) error {
		return tx.AddNarrativeIssues(nid, []store.NarrativeIssue{{
			IssueKey: "DEVSBX-77", Role: store.Role("same_work"),
			Provenance: "prose_first", Confidence: 0.8, Connection: "dev",
		}})
	}))

	tracker := &wireTracker{issues: map[string]tasktracker.Issue{
		"DEVSBX-9":  {Key: "DEVSBX-9", StatusName: "In Progress"},
		"DEVSBX-77": {Key: "DEVSBX-77", StatusName: "In Progress"},
	}}
	// The model answers only for the OTHER issue.
	client := &wireLLM{responses: []string{
		`[{"issue_key":"DEVSBX-77","type":"comment","body":"for the other audience","confidence":0.9,"rationale":"r"}]`,
	}}

	h := NewStoreHandler(s, tracker, client, nil, testCorrelatorConfig(), 100000)

	_, err := h.Redraft(context.Background(), action, "reword")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "DEVSBX-9")
	assert.Contains(t, err.Error(), "nothing was changed")

	old, err := s.GetAction(action.ID)
	require.NoError(t, err)
	assert.Equal(t, "proposed", old.Status,
		"a refused redraft must leave the original untouched")
}

// TestStoreHandlerRedraft_ReportsUnavailableWithoutDependencies: the handler is
// constructible without a tracker or client (a caller that could not resolve a
// default project still gets approve/reject/skip), so the verbs that need them
// must say so rather than panicking on a nil.
func TestStoreHandlerRedraft_ReportsUnavailableWithoutDependencies(t *testing.T) {
	s, _, action := wireStore(t)

	h := NewStoreHandler(s, nil, nil, nil, testCorrelatorConfig(), 100000)

	_, err := h.Redraft(context.Background(), action, "reword")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "unavailable")
}

// TestStoreHandlerRetarget_VerifiesTheNewIssueBeforeLinkingIt: the old link was
// verified by the pass that drafted this action; the new one never was. A typo'd
// key would otherwise become a stored link pointing at nothing
// (rules/verify-correlations.md).
func TestStoreHandlerRetarget_VerifiesTheNewIssueBeforeLinkingIt(t *testing.T) {
	s, nid, action := wireStore(t)

	// DEVSBX-404 is absent from issues, so GetIssue fails.
	tracker := &wireTracker{issues: map[string]tasktracker.Issue{
		"DEVSBX-9": {Key: "DEVSBX-9", StatusName: "In Progress"},
	}}
	client := &wireLLM{}

	h := NewStoreHandler(s, tracker, client, nil, testCorrelatorConfig(), 100000)

	_, err := h.Retarget(context.Background(), action, "DEVSBX-404")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "DEVSBX-404")
	assert.Empty(t, client.prompts, "no LLM call should be spent on an unverifiable key")

	links, err := s.NarrativeIssues(nid)
	require.NoError(t, err)
	require.Len(t, links, 1, "the bad key must not have been linked")
	assert.Equal(t, "DEVSBX-9", links[0].IssueKey, "the original link must survive")
}

// TestStoreHandlerRetarget_ReplacesThePrimaryLink: the partial unique index
// one_primary_per_narrative rejects a second primary row, so the old link must be
// removed rather than merely added alongside.
func TestStoreHandlerRetarget_ReplacesThePrimaryLink(t *testing.T) {
	s, nid, action := wireStore(t)

	tracker := &wireTracker{issues: map[string]tasktracker.Issue{
		"DEVSBX-9":  {Key: "DEVSBX-9", StatusName: "In Progress"},
		"DEVSBX-42": {Key: "DEVSBX-42", StatusName: "To Do", Summary: "the right ticket"},
	}}
	client := &wireLLM{responses: []string{
		`[{"issue_key":"DEVSBX-42","type":"comment","body":"on the right ticket","confidence":0.9,"rationale":"r"}]`,
	}}

	h := NewStoreHandler(s, tracker, client, nil, testCorrelatorConfig(), 100000)

	got, err := h.Retarget(context.Background(), action, "DEVSBX-42")

	require.NoError(t, err)
	assert.Equal(t, "DEVSBX-42", got.IssueKey)
	require.NotZero(t, got.ID)

	links, err := s.NarrativeIssues(nid)
	require.NoError(t, err)
	require.Len(t, links, 1, "exactly one primary, or the unique index would have rejected it")
	assert.Equal(t, "DEVSBX-42", links[0].IssueKey)
}

// TestStoreHandlerRetarget_RecordsReviewerProvenance: a human's assertion is not
// the same kind of claim as a model's inference from a branch name, and a later
// pass should be able to tell them apart.
func TestStoreHandlerRetarget_RecordsReviewerProvenance(t *testing.T) {
	s, nid, action := wireStore(t)

	tracker := &wireTracker{issues: map[string]tasktracker.Issue{
		"DEVSBX-9":  {Key: "DEVSBX-9", StatusName: "In Progress"},
		"DEVSBX-42": {Key: "DEVSBX-42", StatusName: "To Do"},
	}}
	client := &wireLLM{responses: []string{
		`[{"issue_key":"DEVSBX-42","type":"comment","body":"b","confidence":0.9,"rationale":"r"}]`,
	}}

	h := NewStoreHandler(s, tracker, client, nil, testCorrelatorConfig(), 100000)

	_, err := h.Retarget(context.Background(), action, "DEVSBX-42")
	require.NoError(t, err)

	links, err := s.NarrativeIssues(nid)
	require.NoError(t, err)
	require.Len(t, links, 1)
	assert.Equal(t, "reviewer", links[0].Provenance,
		"a reviewer's decision must be distinguishable from a model's guess")
}

// TestStoreHandlerRetarget_RulesRejectedNotEdited: the old action named the wrong
// issue, so it was not reworded — it was ruled against. Slice 7 should learn from
// that distinction rather than seeing every correction as a wording tweak.
func TestStoreHandlerRetarget_RulesRejectedNotEdited(t *testing.T) {
	s, _, action := wireStore(t)

	tracker := &wireTracker{issues: map[string]tasktracker.Issue{
		"DEVSBX-9":  {Key: "DEVSBX-9", StatusName: "In Progress"},
		"DEVSBX-42": {Key: "DEVSBX-42", StatusName: "To Do"},
	}}
	client := &wireLLM{responses: []string{
		`[{"issue_key":"DEVSBX-42","type":"comment","body":"b","confidence":0.9,"rationale":"r"}]`,
	}}

	h := NewStoreHandler(s, tracker, client, nil, testCorrelatorConfig(), 100000)

	_, err := h.Retarget(context.Background(), action, "DEVSBX-42")
	require.NoError(t, err)

	old, err := s.GetAction(action.ID)
	require.NoError(t, err)
	assert.Equal(t, "rejected", old.Status)
	assert.Contains(t, old.Feedback, "DEVSBX-9")
	assert.Contains(t, old.Feedback, "DEVSBX-42")
}

// TestStoreHandlerRetarget_RefusesANoOp: retargeting to the key already targeted
// would spend an LLM call and rewrite the row for no change.
func TestStoreHandlerRetarget_RefusesANoOp(t *testing.T) {
	s, _, action := wireStore(t)

	tracker := &wireTracker{issues: map[string]tasktracker.Issue{
		"DEVSBX-9": {Key: "DEVSBX-9", StatusName: "In Progress"},
	}}
	client := &wireLLM{}

	h := NewStoreHandler(s, tracker, client, nil, testCorrelatorConfig(), 100000)

	_, err := h.Retarget(context.Background(), action, "DEVSBX-9")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "already targets")
	assert.Empty(t, client.prompts)
}

// testCorrelatorConfig is a valid CorrelatorConfig for handler tests. Thresholds
// high enough that compaction never triggers: these tests are about split's own
// behaviour, not about Persist's tail summarization.
func testCorrelatorConfig() config.CorrelatorConfig {
	return config.CorrelatorConfig{
		TailSummarizeThresholdTokens: 1000000,
		RecentEventsKept:             50,
	}
}
