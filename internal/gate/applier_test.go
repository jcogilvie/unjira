package gate_test

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/gate"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// fakeWriter records every call made to it, so a test can assert the
// applier made EXACTLY the expected tracker call and no others — the same
// reasoning as internal/reconciler/reconciler_test.go's fakeTracker, which
// exists because a silent no-op fake cannot express "nothing was called."
//
// It implements only tasktracker.TaskWriter, never TaskReader/TaskTracker:
// that asymmetry is deliberate and is itself part of what the compile-time
// assertion below proves.
type fakeWriter struct {
	calls []string
	// errs maps a call signature prefix (e.g. "AddComment:PROJ-1") to an
	// error that call should return, for write-failure tests.
	errs map[string]error
}

func (f *fakeWriter) AddComment(key, text string) error {
	call := fmt.Sprintf("AddComment:%s:%s", key, text)
	f.calls = append(f.calls, call)

	return f.errs[call]
}

func (f *fakeWriter) SetStatus(key string, target tasktracker.StatusCategory) error {
	call := fmt.Sprintf("SetStatus:%s:%s", key, target)
	f.calls = append(f.calls, call)

	return f.errs[call]
}

func (f *fakeWriter) CreateIssue(projectOrRepo, summary, issueType, description string, _ []string) (string, error) {
	call := fmt.Sprintf("CreateIssue:%s:%s:%s:%s", projectOrRepo, summary, issueType, description)
	f.calls = append(f.calls, call)

	if err, ok := f.errs[call]; ok {
		return "", err
	}

	return "NEW-1", nil
}

// var _ tasktracker.TaskWriter = (*fakeWriter)(nil) is the structural
// assertion that fakeWriter satisfies exactly the surface Applier is allowed
// to hold. It deliberately does NOT also assert tasktracker.TaskTracker —
// unlike reconciler_test.go's fakeTracker, which stands in for a full
// backend. Adding a GetIssue method here would defeat the point of this
// test double.
var _ tasktracker.TaskWriter = (*fakeWriter)(nil)

// applierStore opens a fresh temp-file store, matching
// internal/reconciler/reconciler_test.go's reconcileStore — UpdateActionStatus
// is a real DB write, worth exercising against real SQLite rather than a
// mock.
func applierStore(t *testing.T) *store.Store {
	t.Helper()

	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	return s
}

// insertAction seeds one narrative and one action row, returning the action
// so tests have a real, valid ID to apply against.
func insertAction(t *testing.T, s *store.Store, a store.ActionRow) store.ActionRow {
	t.Helper()

	base := time.Date(2026, 8, 26, 9, 0, 0, 0, time.UTC)
	nid, err := s.InsertNarrative(base, base.Add(time.Hour), "t", "s")
	require.NoError(t, err)

	a.NarrativeID = nid
	if a.Status == "" {
		a.Status = "proposed"
	}

	id, err := s.InsertAction(a)
	require.NoError(t, err)
	a.ID = id

	return a
}

func TestApplier_Comment_CallsAddCommentAndMarksApplied(t *testing.T) {
	s := applierStore(t)
	w := &fakeWriter{}
	action := insertAction(t, s, store.ActionRow{
		Type: "comment", IssueKey: "PROJ-1", Payload: `{"body":"the work landed"}`, Confidence: 0.9,
	})

	applier := gate.NewApplier(s, w, "PROJ")
	err := applier.Apply(action)

	require.NoError(t, err)
	assert.Equal(t, []string{"AddComment:PROJ-1:the work landed"}, w.calls,
		"exactly the expected call, and no others")

	got, err := s.ActionsForNarrative(action.NarrativeID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "applied", got[0].Status)
	require.NotNil(t, got[0].ExecutedAt, "applying sets executed_at")
}

func TestApplier_Transition_CallsSetStatusAndMarksApplied(t *testing.T) {
	s := applierStore(t)
	w := &fakeWriter{}
	action := insertAction(t, s, store.ActionRow{
		Type: "transition", IssueKey: "PROJ-1", Payload: `{"target_status":"done"}`, Confidence: 0.9,
	})

	applier := gate.NewApplier(s, w, "PROJ")
	err := applier.Apply(action)

	require.NoError(t, err)
	assert.Equal(t, []string{"SetStatus:PROJ-1:done"}, w.calls)

	got, err := s.ActionsForNarrative(action.NarrativeID)
	require.NoError(t, err)
	assert.Equal(t, "applied", got[0].Status)
}

func TestApplier_Create_CallsCreateIssueAndMarksApplied(t *testing.T) {
	s := applierStore(t)
	w := &fakeWriter{}
	action := insertAction(t, s, store.ActionRow{
		Type: "create", Payload: `{"summary":"New work","description":"a description"}`, Confidence: 0.9,
	})

	applier := gate.NewApplier(s, w, "PROJ")
	err := applier.Apply(action)

	require.NoError(t, err)
	assert.Equal(t, []string{"CreateIssue:PROJ:New work:Task:a description"}, w.calls)

	got, err := s.ActionsForNarrative(action.NarrativeID)
	require.NoError(t, err)
	assert.Equal(t, "applied", got[0].Status)
}

func TestApplier_Create_WithNoDefaultProjectErrorsRatherThanGuessing(t *testing.T) {
	s := applierStore(t)
	w := &fakeWriter{}
	action := insertAction(t, s, store.ActionRow{
		Type: "create", Payload: `{"summary":"New work"}`, Confidence: 0.9,
	})

	applier := gate.NewApplier(s, w, "")
	err := applier.Apply(action)

	require.Error(t, err)
	assert.Empty(t, w.calls, "must not call CreateIssue with a guessed project")

	got, err := s.ActionsForNarrative(action.NarrativeID)
	require.NoError(t, err)
	assert.Equal(t, "failed", got[0].Status)
}

// TestApplier_WriteFailure_MarksFailedAndKeepsTheRow is THE test the task
// most cares about: a write error must set status=failed, never leave the row
// at status=proposed (silently dropped from view) and never delete it.
func TestApplier_WriteFailure_MarksFailedAndKeepsTheRow(t *testing.T) {
	s := applierStore(t)
	writeErr := fmt.Errorf("jira: 503 service unavailable")
	w := &fakeWriter{errs: map[string]error{
		"AddComment:PROJ-1:the work landed": writeErr,
	}}
	action := insertAction(t, s, store.ActionRow{
		Type: "comment", IssueKey: "PROJ-1", Payload: `{"body":"the work landed"}`, Confidence: 0.9,
	})

	applier := gate.NewApplier(s, w, "PROJ")
	err := applier.Apply(action)

	require.Error(t, err)
	require.ErrorIs(t, err, writeErr)

	got, err := s.ActionsForNarrative(action.NarrativeID)
	require.NoError(t, err)
	require.Len(t, got, 1, "the row must still be in the table — never dropped")
	assert.Equal(t, "failed", got[0].Status)
	require.NotNil(t, got[0].ExecutedAt, "a failed write still stamps executed_at: the write was attempted")
}

// TestApplier_WriteFailure_PersistsTheReasonOnTheRow is the design doc's
// headline assertion: a failed apply must persist WHY, not just THAT, on the
// row itself — asserted via a fresh GetAction, not on the error Apply
// returns, since the return path already worked before this change and
// proves nothing new about persistence.
func TestApplier_WriteFailure_PersistsTheReasonOnTheRow(t *testing.T) {
	s := applierStore(t)
	writeErr := fmt.Errorf("jira: 403 Forbidden")
	w := &fakeWriter{errs: map[string]error{
		"AddComment:PROJ-1:the work landed": writeErr,
	}}
	action := insertAction(t, s, store.ActionRow{
		Type: "comment", IssueKey: "PROJ-1", Payload: `{"body":"the work landed"}`, Confidence: 0.9,
	})

	applier := gate.NewApplier(s, w, "PROJ")
	err := applier.Apply(action)
	require.Error(t, err)

	got, err := s.GetAction(action.ID)
	require.NoError(t, err)
	assert.Equal(t, "failed", got.Status)
	assert.Contains(t, got.Error, "403 Forbidden", "the persisted row must carry the tracker's message, not just the fact of failure")
}

// TestApplier_Success_LeavesErrorEmpty is Apply's success half of the
// design's contract: an applied action's error column must be empty, not
// merely unset by omission — proven against a real row that started with no
// error, so this only guards against a future change accidentally writing a
// non-empty placeholder on the happy path.
func TestApplier_Success_LeavesErrorEmpty(t *testing.T) {
	s := applierStore(t)
	w := &fakeWriter{}
	action := insertAction(t, s, store.ActionRow{
		Type: "comment", IssueKey: "PROJ-1", Payload: `{"body":"the work landed"}`, Confidence: 0.9,
	})

	applier := gate.NewApplier(s, w, "PROJ")
	require.NoError(t, applier.Apply(action))

	got, err := s.GetAction(action.ID)
	require.NoError(t, err)
	assert.Equal(t, "applied", got.Status)
	assert.Empty(t, got.Error)
}

// TestApplier_RetryAfterFailure_ClearsTheStaleReason is the stale-reason test
// at the Applier level (internal/store's own test covers the store method in
// isolation; this proves Apply actually calls it with a clearing pointer on
// the success path). A human fixes the underlying cause and re-approves via
// `actions decide --approve` (cmd/unjira/actions.go's allowed failed->approve
// transition) — the second Apply call must not leave the first failure's
// reason sitting on the row now that it succeeded.
func TestApplier_RetryAfterFailure_ClearsTheStaleReason(t *testing.T) {
	s := applierStore(t)
	writeErr := fmt.Errorf("jira: 503 service unavailable")
	w := &fakeWriter{errs: map[string]error{
		"AddComment:PROJ-1:the work landed": writeErr,
	}}
	action := insertAction(t, s, store.ActionRow{
		Type: "comment", IssueKey: "PROJ-1", Payload: `{"body":"the work landed"}`, Confidence: 0.9,
	})

	applier := gate.NewApplier(s, w, "PROJ")
	require.Error(t, applier.Apply(action))

	got, err := s.GetAction(action.ID)
	require.NoError(t, err)
	require.NotEmpty(t, got.Error, "sanity: the first attempt's reason landed")

	// The underlying cause is now fixed (the fake no longer errors on retry).
	delete(w.errs, "AddComment:PROJ-1:the work landed")
	require.NoError(t, applier.Apply(got))

	got, err = s.GetAction(action.ID)
	require.NoError(t, err)
	assert.Equal(t, "applied", got.Status)
	assert.Empty(t, got.Error, "a subsequent success must clear the stale reason from the earlier failure")
}

// TestApplier_MalformedPayload_ErrorsWithoutCallingTheTracker proves a
// payload that does not match its declared type is a loud error, never a
// guess at what the writer should do.
func TestApplier_MalformedPayload_ErrorsWithoutCallingTheTracker(t *testing.T) {
	s := applierStore(t)
	w := &fakeWriter{}
	action := insertAction(t, s, store.ActionRow{
		Type: "comment", IssueKey: "PROJ-1", Payload: `not json at all`, Confidence: 0.9,
	})

	applier := gate.NewApplier(s, w, "PROJ")
	err := applier.Apply(action)

	require.Error(t, err)
	assert.Empty(t, w.calls, "a payload that fails to decode must never reach the tracker")

	got, err := s.ActionsForNarrative(action.NarrativeID)
	require.NoError(t, err)
	assert.Equal(t, "failed", got[0].Status)
}

func TestApplier_UnrecognizedTargetStatus_ErrorsWithoutCallingTheTracker(t *testing.T) {
	s := applierStore(t)
	w := &fakeWriter{}
	action := insertAction(t, s, store.ActionRow{
		Type: "transition", IssueKey: "PROJ-1", Payload: `{"target_status":"blocked"}`, Confidence: 0.9,
	})

	applier := gate.NewApplier(s, w, "PROJ")
	err := applier.Apply(action)

	require.Error(t, err)
	assert.Empty(t, w.calls)
}

// TestApplier_UnknownActionType_ErrorsRatherThanGuessing covers "estimate" —
// a real actions.type value with no TaskWriter method — and any future type
// this package doesn't yet know how to enact.
func TestApplier_UnknownActionType_ErrorsRatherThanGuessing(t *testing.T) {
	s := applierStore(t)
	w := &fakeWriter{}
	action := insertAction(t, s, store.ActionRow{
		Type: "estimate", Payload: `{}`, Confidence: 0.9,
	})

	applier := gate.NewApplier(s, w, "PROJ")
	err := applier.Apply(action)

	require.Error(t, err)
	assert.Empty(t, w.calls)

	got, err := s.ActionsForNarrative(action.NarrativeID)
	require.NoError(t, err)
	assert.Equal(t, "failed", got[0].Status)
}

func TestApplier_MissingIssueKeyOnComment_Errors(t *testing.T) {
	s := applierStore(t)
	w := &fakeWriter{}
	action := insertAction(t, s, store.ActionRow{
		Type: "comment", Payload: `{"body":"x"}`, Confidence: 0.9,
	})

	applier := gate.NewApplier(s, w, "PROJ")
	err := applier.Apply(action)

	require.Error(t, err)
	assert.Empty(t, w.calls)
}
