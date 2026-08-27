package pipeline_test

// autocommit_test.go exercises RunAutoCommit against a real temp-file store
// and a recording tasktracker.TaskWriter fake — the orchestration (deciding
// per action via gate.Decide, calling gate.Applier.Apply on DecisionApply,
// and isolating one action's failure from the rest) is what's under test
// here, not gate.Decide's own table of confidence/graduation cases, which
// internal/gate/decide_test.go covers, nor Applier's own payload-decoding
// behavior, which internal/gate/applier_test.go covers.

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/gate"
	"github.com/jcogilvie/unjira/internal/pipeline"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// autoCommitFixedTime is an arbitrary fixed instant for seeding a narrative's
// window — RunAutoCommit never reads it, only InsertNarrative requires one.
var autoCommitFixedTime = time.Date(2026, 8, 26, 9, 0, 0, 0, time.UTC)

// autoCommitFakeWriter records every call made to it, mirroring
// internal/gate/applier_test.go's fakeWriter (unexported there, so not
// reusable from this package). Implements only tasktracker.TaskWriter,
// matching gate.NewApplier's own parameter — a fake with a GetIssue method
// would defeat the point of asserting RunAutoCommit never reads.
type autoCommitFakeWriter struct {
	calls []string
	// errs maps a call signature to an error that call should return, for
	// write-failure tests.
	errs map[string]error
}

func (f *autoCommitFakeWriter) AddComment(key, text string) error {
	call := fmt.Sprintf("AddComment:%s:%s", key, text)
	f.calls = append(f.calls, call)

	return f.errs[call]
}

func (f *autoCommitFakeWriter) SetStatus(key string, target tasktracker.StatusCategory) error {
	call := fmt.Sprintf("SetStatus:%s:%s", key, target)
	f.calls = append(f.calls, call)

	return f.errs[call]
}

func (f *autoCommitFakeWriter) CreateIssue(projectOrRepo, summary, issueType, description string, _ []string) (string, error) {
	call := fmt.Sprintf("CreateIssue:%s:%s:%s:%s", projectOrRepo, summary, issueType, description)
	f.calls = append(f.calls, call)

	if err, ok := f.errs[call]; ok {
		return "", err
	}

	return "NEW-1", nil
}

var _ tasktracker.TaskWriter = (*autoCommitFakeWriter)(nil)

// autoCommitStore opens a fresh temp-file store, matching
// internal/gate/applier_test.go's applierStore — Applier.Apply performs a
// real store.UpdateActionStatus write, worth exercising against real SQLite.
func autoCommitStore(t *testing.T) *store.Store {
	t.Helper()

	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	return s
}

// insertProposedAction seeds one narrative and one status=proposed action
// row, returning the action with its assigned id — the shape
// reconciler.Persist actually hands watch (ReconcileRunResult.Persisted),
// so RunAutoCommit is exercised against real rows rather than zero-value
// ActionRow literals with no backing narrative.
func insertProposedAction(t *testing.T, s *store.Store, a store.ActionRow) store.ActionRow {
	t.Helper()

	nid, err := s.InsertNarrative(
		autoCommitFixedTime, autoCommitFixedTime.Add(time.Hour), "t", "s",
	)
	require.NoError(t, err)

	a.NarrativeID = nid
	a.Status = "proposed"

	id, err := s.InsertAction(a)
	require.NoError(t, err)
	a.ID = id

	return a
}

func TestRunAutoCommit_AppliesWhenGraduatedAndAboveFloor(t *testing.T) {
	s := autoCommitStore(t)
	writer := &autoCommitFakeWriter{}
	applier := gate.NewApplier(s, writer, "PROJ")

	action := insertProposedAction(t, s, store.ActionRow{
		Type: "comment", IssueKey: "PROJ-1", Payload: `{"body":"the work landed"}`, Confidence: 0.9,
	})

	result, err := pipeline.RunAutoCommit([]store.ActionRow{action}, pipeline.AutoCommitOptions{
		Rules:   map[string]config.AutoCommitRule{"comment": {ConfidenceFloor: 0.8, Graduated: true}},
		Applier: applier,
	})

	require.NoError(t, err)
	require.Len(t, result.Applied, 1)
	assert.Equal(t, action.ID, result.Applied[0].ID)
	assert.Empty(t, result.Queued)
	assert.Empty(t, result.Failed)
	assert.Equal(t, []string{"AddComment:PROJ-1:the work landed"}, writer.calls)

	got, err := s.ActionsForNarrative(action.NarrativeID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "applied", got[0].Status)
}

// TestRunAutoCommit_DefaultRulesQueueEverything is the safety property this
// orchestration rests on, exercised end to end rather than only at
// gate.Decide's own unit level: a nil/empty Rules map — what an unconfigured
// watch produces — must queue a high-confidence action rather than apply it,
// and must never call the writer.
func TestRunAutoCommit_DefaultRulesQueueEverything(t *testing.T) {
	s := autoCommitStore(t)
	writer := &autoCommitFakeWriter{}
	applier := gate.NewApplier(s, writer, "PROJ")

	action := insertProposedAction(t, s, store.ActionRow{
		Type: "comment", IssueKey: "PROJ-1", Payload: `{"body":"the work landed"}`, Confidence: 1.0,
	})

	result, err := pipeline.RunAutoCommit([]store.ActionRow{action}, pipeline.AutoCommitOptions{
		Rules:   nil,
		Applier: applier,
	})

	require.NoError(t, err)
	assert.Empty(t, result.Applied)
	require.Len(t, result.Queued, 1)
	assert.Equal(t, action.ID, result.Queued[0].ID)
	assert.Empty(t, result.Failed)
	assert.Empty(t, writer.calls, "queued means the writer must never be called")

	got, err := s.ActionsForNarrative(action.NarrativeID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "proposed", got[0].Status, "a queued action stays proposed for triage")
}

// TestRunAutoCommit_OneFailureDoesNotBlockTheRest is the isolation property:
// each action is independent (its own narrative, its own issue), Applier
// already records a write failure as status=failed on that one row, and
// this mirrors reconciler.Reconcile's own per-item isolation via
// errors.Join rather than abandoning a whole batch over one bad write.
func TestRunAutoCommit_OneFailureDoesNotBlockTheRest(t *testing.T) {
	s := autoCommitStore(t)
	writeErr := fmt.Errorf("jira: 503 service unavailable")
	writer := &autoCommitFakeWriter{errs: map[string]error{
		"AddComment:PROJ-1:will fail": writeErr,
	}}
	applier := gate.NewApplier(s, writer, "PROJ")

	failing := insertProposedAction(t, s, store.ActionRow{
		Type: "comment", IssueKey: "PROJ-1", Payload: `{"body":"will fail"}`, Confidence: 0.9,
	})
	succeeding := insertProposedAction(t, s, store.ActionRow{
		Type: "comment", IssueKey: "PROJ-2", Payload: `{"body":"will succeed"}`, Confidence: 0.9,
	})

	rules := map[string]config.AutoCommitRule{"comment": {ConfidenceFloor: 0.8, Graduated: true}}

	result, err := pipeline.RunAutoCommit([]store.ActionRow{failing, succeeding}, pipeline.AutoCommitOptions{
		Rules:   rules,
		Applier: applier,
	})

	require.Error(t, err, "a failure is surfaced, not swallowed")
	require.ErrorIs(t, err, writeErr)

	require.Len(t, result.Applied, 1)
	assert.Equal(t, succeeding.ID, result.Applied[0].ID)
	require.Len(t, result.Failed, 1)
	assert.Equal(t, failing.ID, result.Failed[0].Action.ID)
	require.ErrorIs(t, result.Failed[0].Err, writeErr, "the failed entry must carry its OWN error, not just the row")
	assert.Empty(t, result.Queued)

	// Both writer calls happened: the failing action's write was attempted
	// (and failed), and the succeeding action's write still ran after it.
	assert.ElementsMatch(t, []string{"AddComment:PROJ-1:will fail", "AddComment:PROJ-2:will succeed"}, writer.calls)

	gotFailing, err := s.ActionsForNarrative(failing.NarrativeID)
	require.NoError(t, err)
	require.Len(t, gotFailing, 1)
	assert.Equal(t, "failed", gotFailing[0].Status)

	gotSucceeding, err := s.ActionsForNarrative(succeeding.NarrativeID)
	require.NoError(t, err)
	require.Len(t, gotSucceeding, 1)
	assert.Equal(t, "applied", gotSucceeding[0].Status)
}

// TestRunAutoCommit_TwoFailuresAttributeTheRightReasonToEachID is the design
// doc's per-id assertion: a single joined error string would pass a weaker
// test (two failures happened), but this proves RunAutoCommit's result
// attributes the CORRECT one of two distinct reasons to each id, not just
// that both ids appear somewhere in result.Failed and some error was
// returned.
func TestRunAutoCommit_TwoFailuresAttributeTheRightReasonToEachID(t *testing.T) {
	s := autoCommitStore(t)
	firstErr := fmt.Errorf("jira: 403 Forbidden")
	secondErr := fmt.Errorf("jira: 404 Not Found")
	writer := &autoCommitFakeWriter{errs: map[string]error{
		"AddComment:PROJ-1:first will fail":  firstErr,
		"AddComment:PROJ-2:second will fail": secondErr,
	}}
	applier := gate.NewApplier(s, writer, "PROJ")

	first := insertProposedAction(t, s, store.ActionRow{
		Type: "comment", IssueKey: "PROJ-1", Payload: `{"body":"first will fail"}`, Confidence: 0.9,
	})
	second := insertProposedAction(t, s, store.ActionRow{
		Type: "comment", IssueKey: "PROJ-2", Payload: `{"body":"second will fail"}`, Confidence: 0.9,
	})

	rules := map[string]config.AutoCommitRule{"comment": {ConfidenceFloor: 0.8, Graduated: true}}

	result, err := pipeline.RunAutoCommit([]store.ActionRow{first, second}, pipeline.AutoCommitOptions{
		Rules:   rules,
		Applier: applier,
	})

	require.Error(t, err)
	require.ErrorIs(t, err, firstErr)
	require.ErrorIs(t, err, secondErr)

	require.Len(t, result.Failed, 2)

	byID := map[int64]error{
		result.Failed[0].Action.ID: result.Failed[0].Err,
		result.Failed[1].Action.ID: result.Failed[1].Err,
	}

	require.Contains(t, byID, first.ID)
	require.Contains(t, byID, second.ID)
	require.ErrorIs(t, byID[first.ID], firstErr, "the first action's own reason, not the second's")
	require.ErrorIs(t, byID[second.ID], secondErr, "the second action's own reason, not the first's")
	require.NotErrorIs(t, byID[first.ID], secondErr, "the reasons must not be swapped or merged")
	require.NotErrorIs(t, byID[second.ID], firstErr, "the reasons must not be swapped or merged")
}

func TestRunAutoCommit_EmptyActionsIsANoOp(t *testing.T) {
	s := autoCommitStore(t)
	writer := &autoCommitFakeWriter{}
	applier := gate.NewApplier(s, writer, "PROJ")

	result, err := pipeline.RunAutoCommit(nil, pipeline.AutoCommitOptions{
		Rules:   map[string]config.AutoCommitRule{"comment": {ConfidenceFloor: 0, Graduated: true}},
		Applier: applier,
	})

	require.NoError(t, err)
	assert.Empty(t, result.Applied)
	assert.Empty(t, result.Queued)
	assert.Empty(t, result.Failed)
	assert.Empty(t, writer.calls)
}

func TestRenderAutoCommitResult_NamesEveryAction(t *testing.T) {
	out := pipeline.RenderAutoCommitResult(pipeline.AutoCommitRunResult{
		Applied: []store.ActionRow{{ID: 1, Type: "comment", IssueKey: "PROJ-1"}},
		Queued:  []store.ActionRow{{ID: 2, Type: "transition", IssueKey: "PROJ-2", Confidence: 0.4}},
		Failed: []pipeline.FailedAction{
			{Action: store.ActionRow{ID: 3, Type: "comment", IssueKey: "PROJ-3"}, Err: fmt.Errorf("jira: 403 Forbidden")},
		},
	})

	assert.Contains(t, out, "applied: 1")
	assert.Contains(t, out, "queued: 1")
	assert.Contains(t, out, "failed: 1")
	assert.Contains(t, out, "PROJ-1")
	assert.Contains(t, out, "PROJ-2")
	assert.Contains(t, out, "PROJ-3")
	assert.Contains(t, out, "403 Forbidden", "the renderer must show WHY an action failed, not just THAT it did")
}
