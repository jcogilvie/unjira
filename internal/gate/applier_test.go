package gate_test

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/gate"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// writableConnections is the test-default write scope: one jira connection
// that both reads and writes project. Every existing test in this file
// predates write scope and assumed an implicit "PROJ is always writable" —
// this is that assumption made explicit and named, so a reader sees exactly
// which project each test authorizes for writes.
func writableConnections(project string) []config.JiraConnection {
	return []config.JiraConnection{
		{Name: "test", ProjectKeys: []string{project}, WritableProjectKeys: []string{project}},
	}
}

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

	applier := gate.NewApplier(s, w, "PROJ", writableConnections("PROJ"))
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

	applier := gate.NewApplier(s, w, "PROJ", writableConnections("PROJ"))
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

	applier := gate.NewApplier(s, w, "PROJ", writableConnections("PROJ"))
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

	applier := gate.NewApplier(s, w, "", writableConnections("PROJ"))
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

	applier := gate.NewApplier(s, w, "PROJ", writableConnections("PROJ"))
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

	applier := gate.NewApplier(s, w, "PROJ", writableConnections("PROJ"))
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

	applier := gate.NewApplier(s, w, "PROJ", writableConnections("PROJ"))
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

	applier := gate.NewApplier(s, w, "PROJ", writableConnections("PROJ"))
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

	applier := gate.NewApplier(s, w, "PROJ", writableConnections("PROJ"))
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

	applier := gate.NewApplier(s, w, "PROJ", writableConnections("PROJ"))
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

	applier := gate.NewApplier(s, w, "PROJ", writableConnections("PROJ"))
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

	applier := gate.NewApplier(s, w, "PROJ", writableConnections("PROJ"))
	err := applier.Apply(action)

	require.Error(t, err)
	assert.Empty(t, w.calls)
}

// TestApplier_WriteScope_RefusesWriteOutsideWritableSet is test 1 from
// docs/superpowers/specs/2026-08-27-write-scope-design.md's testing section:
// IssueKey "PAAS-1" with a writable set of {DEVSBX} must be refused — the
// fake sees NO calls (asserted on the recording fake, not merely "no error"),
// status=failed, and the persisted error names both "PAAS" and
// "writable_project_keys" so an operator reading `actions list --status
// failed` tomorrow can find the config key to fix.
func TestApplier_WriteScope_RefusesWriteOutsideWritableSet(t *testing.T) {
	s := applierStore(t)
	w := &fakeWriter{}
	action := insertAction(t, s, store.ActionRow{
		Type: "comment", IssueKey: "PAAS-1", Payload: `{"body":"the work landed"}`, Confidence: 0.9,
	})

	conns := []config.JiraConnection{
		{Name: "dev", ProjectKeys: []string{"PAAS", "DEVSBX"}, WritableProjectKeys: []string{"DEVSBX"}},
	}
	applier := gate.NewApplier(s, w, "DEVSBX", conns)
	err := applier.Apply(action)

	require.Error(t, err)
	assert.Empty(t, w.calls, "PAAS is not writable: the tracker must never be called")

	got, err := s.GetAction(action.ID)
	require.NoError(t, err)
	assert.Equal(t, "failed", got.Status)
	assert.Contains(t, got.Error, "PAAS", "the persisted reason must name the refused project")
	assert.Contains(t, got.Error, "writable_project_keys", "the persisted reason must name the config key to fix")
}

// TestApplier_WriteScope_EmptyWritableSetRefusesEvenAReadableProject is test 2:
// an empty/absent WritableProjectKeys must refuse EVERYTHING, including a
// project the connection can read via ProjectKeys — deny-by-default, not
// "readable implies writable".
func TestApplier_WriteScope_EmptyWritableSetRefusesEvenAReadableProject(t *testing.T) {
	s := applierStore(t)
	w := &fakeWriter{}
	action := insertAction(t, s, store.ActionRow{
		Type: "comment", IssueKey: "PROJ-1", Payload: `{"body":"the work landed"}`, Confidence: 0.9,
	})

	conns := []config.JiraConnection{
		{Name: "dev", ProjectKeys: []string{"PROJ"}}, // WritableProjectKeys deliberately unset
	}
	applier := gate.NewApplier(s, w, "PROJ", conns)
	err := applier.Apply(action)

	require.Error(t, err)
	assert.Empty(t, w.calls, "PROJ is readable but not writable, so the tracker must never be called")

	got, err := s.GetAction(action.ID)
	require.NoError(t, err)
	assert.Equal(t, "failed", got.Status)
}

// TestApplier_WriteScope_CreateHonoursWritableSetNotJustProjectKeys is test 4:
// a `create` action's target must be checked against writable_project_keys,
// not merely resolved as covered by SOME connection's project_keys.
func TestApplier_WriteScope_CreateHonoursWritableSetNotJustProjectKeys(t *testing.T) {
	s := applierStore(t)
	w := &fakeWriter{}
	action := insertAction(t, s, store.ActionRow{
		Type: "create", Payload: `{"summary":"New work"}`, Confidence: 0.9,
	})

	conns := []config.JiraConnection{
		// PAAS is readable (in project_keys) but not writable.
		{Name: "dev", ProjectKeys: []string{"PAAS", "DEVSBX"}, WritableProjectKeys: []string{"DEVSBX"}},
	}
	applier := gate.NewApplier(s, w, "PAAS", conns)
	err := applier.Apply(action)

	require.Error(t, err)
	assert.Empty(t, w.calls, "PAAS is not writable: CreateIssue must never be called")

	got, err := s.GetAction(action.ID)
	require.NoError(t, err)
	assert.Equal(t, "failed", got.Status)
	assert.Contains(t, got.Error, "PAAS")
}

// TestApplier_Create_LinksTheNewIssueToItsNarrative is the duplicate-ticket
// guard. applyCreate used to discard CreateIssue's returned key
// (`if _, err := ...`), so the narrative kept zero narrative_issues rows and
// still read as untracked work — and the next pass proposed a create for it
// again. Probed before the fix:
//
//	CreateIssue called 1 time(s), returned DEVSBX-100
//	narrative_issues rows for narrative 1: 0
//
// Of the three mutation types this is the least recoverable: unjira cannot
// un-create an issue, and each duplicate is an object other people reference.
func TestApplier_Create_LinksTheNewIssueToItsNarrative(t *testing.T) {
	s := applierStore(t)
	w := &fakeWriter{}
	action := insertAction(t, s, store.ActionRow{
		Type:    "create",
		Payload: `{"summary":"Do the thing","description":"the body"}`,
	})

	applier := gate.NewApplier(s, w, "PROJ", writableConnections("PROJ"))

	require.NoError(t, applier.Apply(action))

	links, err := s.NarrativeIssues(action.NarrativeID)
	require.NoError(t, err)
	require.Len(t, links, 1,
		"without a link the narrative still looks untracked and the next pass creates a duplicate")
	assert.Equal(t, "NEW-1", links[0].IssueKey,
		"the link must name the key the tracker actually returned")
}

// TestApplier_Create_RecordsUnjiraCreatedProvenance: this is not an inference
// about where work belongs — unjira put it there. A later pass should be able to
// tell that apart from a branch-name guess.
func TestApplier_Create_RecordsUnjiraCreatedProvenance(t *testing.T) {
	s := applierStore(t)
	w := &fakeWriter{}
	action := insertAction(t, s, store.ActionRow{
		Type:    "create",
		Payload: `{"summary":"s","description":"d"}`,
	})

	applier := gate.NewApplier(s, w, "PROJ", writableConnections("PROJ"))
	require.NoError(t, applier.Apply(action))

	links, err := s.NarrativeIssues(action.NarrativeID)
	require.NoError(t, err)
	require.Len(t, links, 1)
	assert.Equal(t, "unjira_created", links[0].Provenance)
	assert.Equal(t, store.Role("primary"), links[0].Role,
		"a created issue IS the record of this work, so it is the primary")
}

// TestApplier_Create_ASecondPassFindsTheNarrativeTracked closes the loop the way
// it actually manifests: the point of linking is that the narrative stops being
// selected as untracked. Asserted through the selector matching drafts, not just
// on the link row.
func TestApplier_Create_ASecondPassFindsTheNarrativeTracked(t *testing.T) {
	s := applierStore(t)
	w := &fakeWriter{}
	action := insertAction(t, s, store.ActionRow{
		Type:    "create",
		Payload: `{"summary":"s","description":"d"}`,
	})

	before, err := s.NarrativesWithoutIssueKey(10)
	require.NoError(t, err)
	require.Len(t, before, 1, "precondition: the narrative starts untracked")

	applier := gate.NewApplier(s, w, "PROJ", writableConnections("PROJ"))
	require.NoError(t, applier.Apply(action))

	tracked, err := s.NarrativesWithActionableLinks(10, []store.Role{store.Role("primary")})
	require.NoError(t, err)
	assert.Len(t, tracked, 1,
		"the narrative must now be the reconciler's business, not matching's backlog")
}

// TestApplier_Create_ReportsAnOrphanWhenTheTrackerReturnsNoKey: an issue that
// exists but cannot be linked is the duplicate-ticket condition. It must be loud,
// because a human has to find the orphan before the next pass runs.
func TestApplier_Create_ReportsAnOrphanWhenTheTrackerReturnsNoKey(t *testing.T) {
	s := applierStore(t)
	w := &keylessWriter{}
	action := insertAction(t, s, store.ActionRow{
		Type:    "create",
		Payload: `{"summary":"s","description":"d"}`,
	})

	applier := gate.NewApplier(s, w, "PROJ", writableConnections("PROJ"))

	err := applier.Apply(action)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "returned no key")
	assert.Contains(t, err.Error(), "link it by hand")

	stored, err := s.GetAction(action.ID)
	require.NoError(t, err)
	assert.Equal(t, "failed", stored.Status,
		"an unlinkable create is a failure a human must see, not a silent success")
}

// keylessWriter creates successfully but returns an empty key — a tracker that
// mutated reality and told us nothing usable about it.
type keylessWriter struct{ fakeWriter }

func (w *keylessWriter) CreateIssue(string, string, string, string, []string) (string, error) {
	return "", nil
}

// TestApplier_UntrackedProjectRefusalNamesTheRightRemedy is the distinction an
// earlier version of checkProjectWritable deliberately collapsed, arguing that
// "no connection says yes" and "a connection says no" are the same outcome. They
// are the same OUTCOME and different REMEDIES, which is what the message is for.
//
// The old message sent a reader to edit writable_project_keys for connection
// "none" — a connection that does not exist, so following the advice was
// impossible.
func TestApplier_UntrackedProjectRefusalNamesTheRightRemedy(t *testing.T) {
	s := applierStore(t)
	w := &fakeWriter{}
	action := insertAction(t, s, store.ActionRow{
		Type: "comment", IssueKey: "SUMO-287220", Payload: `{"body":"b"}`,
	})

	// PROJ is tracked and writable; SUMO is not tracked at all.
	applier := gate.NewApplier(s, w, "PROJ", writableConnections("PROJ"))

	err := applier.Apply(action)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not tracked by unjira",
		"an untracked project must not be reported as a writable_project_keys problem")
	assert.Contains(t, err.Error(), "project_keys",
		"the message must name the config key that would actually change this")
	assert.Contains(t, err.Error(), "retarget",
		"the likely remedy is retargeting, since unjira attributed work to a project it does not track")
	assert.NotContains(t, err.Error(), `connection "none"`,
		"the old message pointed at a connection that does not exist")

	assert.Empty(t, w.calls, "nothing may reach the tracker")
}

// TestApplier_ReadableButUnwritableRefusalNamesTheConnection: this one IS a scope
// decision someone made, so the message names the connection to edit.
func TestApplier_ReadableButUnwritableRefusalNamesTheConnection(t *testing.T) {
	s := applierStore(t)
	w := &fakeWriter{}
	action := insertAction(t, s, store.ActionRow{
		Type: "comment", IssueKey: "PAAS-4038", Payload: `{"body":"b"}`,
	})

	// PAAS is readable on connection "test" but absent from writable_project_keys.
	applier := gate.NewApplier(s, w, "DEVSBX", []config.JiraConnection{{
		Name:                "test",
		ProjectKeys:         []string{"PAAS", "DEVSBX"},
		WritableProjectKeys: []string{"DEVSBX"},
	}})

	err := applier.Apply(action)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "readable but not writable")
	assert.Contains(t, err.Error(), `connection "test"`,
		"a scope decision names where to change it")
	assert.Contains(t, err.Error(), "writable_project_keys")
	assert.NotContains(t, err.Error(), "not tracked",
		"a tracked-but-unwritable project must not read as untracked")

	assert.Empty(t, w.calls)
}

// TestApplier_BothRefusalsPersistTheirReasonForTriage: a reviewer sees these in
// `actions list --status failed`, which is the surface that has to answer WHY.
func TestApplier_BothRefusalsPersistTheirReasonForTriage(t *testing.T) {
	cases := []struct {
		name      string
		issueKey  string
		conns     []config.JiraConnection
		wantPhras string
	}{
		{
			name:      "untracked project",
			issueKey:  "SUMO-1",
			conns:     writableConnections("PROJ"),
			wantPhras: "not tracked by unjira",
		},
		{
			name:     "readable but unwritable",
			issueKey: "PAAS-1",
			conns: []config.JiraConnection{{
				Name: "test", ProjectKeys: []string{"PAAS"}, WritableProjectKeys: nil,
			}},
			wantPhras: "readable but not writable",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := applierStore(t)
			w := &fakeWriter{}
			action := insertAction(t, s, store.ActionRow{
				Type: "comment", IssueKey: tc.issueKey, Payload: `{"body":"b"}`,
			})

			applier := gate.NewApplier(s, w, "PROJ", tc.conns)
			require.Error(t, applier.Apply(action))

			stored, err := s.GetAction(action.ID)
			require.NoError(t, err)
			assert.Equal(t, "failed", stored.Status)
			assert.Contains(t, stored.Error, tc.wantPhras,
				"the persisted reason is what a reviewer reads later, so it must carry the distinction")
		})
	}
}
