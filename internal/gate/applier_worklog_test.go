package gate_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/clients/jira"
	"github.com/jcogilvie/unjira/internal/gate"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/store/storetest"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// worklogWriter is a fakeWriter that can also log work, recording every worklog call.
type worklogWriter struct {
	fakeWriter

	found   map[string]string // idempotency key -> existing worklog id
	addID   string
	addErr  error
	entries []tasktracker.WorklogEntry
}

var _ tasktracker.WorklogWriter = (*worklogWriter)(nil)

func (w *worklogWriter) FindWorklog(key, idem string, _ []string) (string, bool, error) {
	w.calls = append(w.calls, fmt.Sprintf("FindWorklog:%s:%s", key, idem))
	id, ok := w.found[idem]

	return id, ok, nil
}

func (w *worklogWriter) AddWorklog(key string, e tasktracker.WorklogEntry) (string, error) {
	w.calls = append(w.calls, "AddWorklog:"+key)
	w.entries = append(w.entries, e)
	if w.addErr != nil {
		return "", w.addErr
	}

	return w.addID, nil
}

func (w *worklogWriter) UpdateWorklog(key, id string, e tasktracker.WorklogEntry) error {
	w.calls = append(w.calls, fmt.Sprintf("UpdateWorklog:%s:%s", key, id))
	w.entries = append(w.entries, e)

	return nil
}

func (w *worklogWriter) DeleteWorklog(key, id string) error {
	w.calls = append(w.calls, fmt.Sprintf("DeleteWorklog:%s:%s", key, id))

	return nil
}

var worklogStarted = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func seedLedger(t *testing.T, s *store.Store, ids ...string) {
	t.Helper()
	rows := make([]store.MessageUsage, len(ids))
	for i, id := range ids {
		rows[i] = storetest.MessageUsage(id).Build()
	}
	require.NoError(t, s.UpsertMessageUsage(rows))
}

func worklogAction(t *testing.T, s *store.Store, issue string, p store.WorklogPayload) store.ActionRow {
	t.Helper()
	b, err := json.Marshal(p)
	require.NoError(t, err)
	a := store.ActionRow{Type: store.ActionTypeWorklog, IssueKey: issue, Payload: string(b), Confidence: 1, Status: "proposed"}
	id, err := s.InsertAction(a)
	require.NoError(t, err)
	a.ID = id

	return a
}

func addPayload(ids ...string) store.WorklogPayload {
	return store.WorklogPayload{
		Op: store.WorklogOpAdd, Started: worklogStarted, TimeSpentSeconds: 120, Comment: "c",
		MessageIDs: ids, IdempotencyKey: "idem", Property: json.RawMessage(`{"version":1}`),
	}
}

func logged(t *testing.T, s *store.Store, ids ...string) map[string]string {
	t.Helper()
	got, err := s.LoggedAmong(ids)
	require.NoError(t, err)

	return got
}

func actionRow(t *testing.T, s *store.Store, id int64) store.ActionRow {
	t.Helper()
	got, err := s.GetAction(id)
	require.NoError(t, err)

	return got
}

func TestApplier_WorklogAdd_SetsTheMarker(t *testing.T) {
	s := applierStore(t)
	seedLedger(t, s, "m1", "m2")
	w := &worklogWriter{addID: "10001"}
	a := worklogAction(t, s, "PROJ-1", addPayload("m1", "m2"))

	require.NoError(t, gate.NewApplier(s, w, "PROJ", writableTrackers("PROJ")).Apply(a))

	assert.Equal(t, []string{"FindWorklog:PROJ-1:idem", "AddWorklog:PROJ-1"}, w.calls)
	require.Len(t, w.entries, 1)
	assert.Equal(t, tasktracker.WorklogEntry{
		Started: worklogStarted, TimeSpentSeconds: 120, Comment: "c", Property: json.RawMessage(`{"version":1}`),
	}, w.entries[0])
	assert.Equal(t, store.StatusApplied, actionRow(t, s, a.ID).Status)
	assert.Equal(t, map[string]string{"m1": "PROJ-1/10001", "m2": "PROJ-1/10001"}, logged(t, s, "m1", "m2"))
}

func TestApplier_WorklogAdd_RetryFindsTheExistingWorklog(t *testing.T) {
	s := applierStore(t)
	seedLedger(t, s, "m1")
	w := &worklogWriter{found: map[string]string{"idem": "10001"}}
	a := worklogAction(t, s, "PROJ-1", addPayload("m1"))

	require.NoError(t, gate.NewApplier(s, w, "PROJ", writableTrackers("PROJ")).Apply(a))

	assert.Equal(t, []string{"FindWorklog:PROJ-1:idem"}, w.calls, "no re-POST")
	assert.Equal(t, store.StatusApplied, actionRow(t, s, a.ID).Status)
	assert.Equal(t, map[string]string{"m1": "PROJ-1/10001"}, logged(t, s, "m1"))
}

func TestApplier_WorklogAdd_ClosedIssueFailsAndStaysUnlogged(t *testing.T) {
	s := applierStore(t)
	seedLedger(t, s, "m1")
	w := &worklogWriter{addErr: &jira.Error{Status: 400, Message: "issue is not editable"}}
	a := worklogAction(t, s, "PROJ-1", addPayload("m1"))

	require.Error(t, gate.NewApplier(s, w, "PROJ", writableTrackers("PROJ")).Apply(a))

	got := actionRow(t, s, a.ID)
	assert.Equal(t, store.StatusFailed, got.Status)
	assert.Contains(t, got.Error, "issue is not editable")
	assert.Empty(t, logged(t, s, "m1"))
}

func TestApplier_WorklogAdd_StaleDraftRefused(t *testing.T) {
	s := applierStore(t)
	seedLedger(t, s, "m1", "m2")
	require.NoError(t, s.MarkWorklogged("PROJ-1", "10002", []string{"m1"}))
	w := &worklogWriter{addID: "10001"}
	a := worklogAction(t, s, "PROJ-1", addPayload("m1", "m2"))

	require.Error(t, gate.NewApplier(s, w, "PROJ", writableTrackers("PROJ")).Apply(a))

	assert.Empty(t, w.calls)
	got := actionRow(t, s, a.ID)
	assert.Equal(t, store.StatusFailed, got.Status)
	assert.Contains(t, got.Error, "message m1 is already logged in PROJ-1/10002: this draft is stale")
}

func TestApplier_WorklogAdd_MarkerFailureNamesTheWorklog(t *testing.T) {
	s := applierStore(t)
	seedLedger(t, s, "m1")
	w := &worklogWriter{addID: "10001"}
	a := worklogAction(t, s, "PROJ-1", addPayload("m1", "ghost"))

	require.Error(t, gate.NewApplier(s, w, "PROJ", writableTrackers("PROJ")).Apply(a))

	assert.Equal(t, []string{"FindWorklog:PROJ-1:idem", "AddWorklog:PROJ-1"}, w.calls)
	got := actionRow(t, s, a.ID)
	assert.Equal(t, store.StatusFailed, got.Status)
	assert.Contains(t, got.Error, "10001")
	assert.Contains(t, got.Error, "re-approve")
}

func TestApplier_WorklogUpdate_ClearsMovedMarkers(t *testing.T) {
	s := applierStore(t)
	seedLedger(t, s, "m1", "m2", "m3")
	require.NoError(t, s.MarkWorklogged("PROJ-1", "10001", []string{"m1", "m2"}))
	require.NoError(t, s.MarkWorklogged("PROJ-1", "10009", []string{"m3"}))
	w := &worklogWriter{}
	p := addPayload("m1")
	p.Op, p.WorklogID = store.WorklogOpUpdate, "10001"
	a := worklogAction(t, s, "PROJ-1", p)

	require.NoError(t, gate.NewApplier(s, w, "PROJ", writableTrackers("PROJ")).Apply(a))

	assert.Equal(t, []string{"UpdateWorklog:PROJ-1:10001"}, w.calls)
	assert.Equal(t, map[string]string{"m1": "PROJ-1/10001", "m3": "PROJ-1/10009"}, logged(t, s, "m1", "m2", "m3"))
	assert.Equal(t, store.StatusApplied, actionRow(t, s, a.ID).Status)
}

func TestApplier_WorklogUpdate_StaleKeepSetRefused(t *testing.T) {
	s := applierStore(t)
	seedLedger(t, s, "m1", "m2")
	require.NoError(t, s.MarkWorklogged("PROJ-1", "10001", []string{"m1"}))
	w := &worklogWriter{}
	p := addPayload("m1", "m2")
	p.Op, p.WorklogID = store.WorklogOpUpdate, "10001"
	a := worklogAction(t, s, "PROJ-1", p)

	require.Error(t, gate.NewApplier(s, w, "PROJ", writableTrackers("PROJ")).Apply(a))

	assert.Empty(t, w.calls, "no PUT of a keep-set the ledger no longer holds")
	got := actionRow(t, s, a.ID)
	assert.Equal(t, store.StatusFailed, got.Status)
	assert.Contains(t, got.Error, "message m2 is no longer logged in PROJ-1/10001: this draft is stale")
	assert.Equal(t, map[string]string{"m1": "PROJ-1/10001"}, logged(t, s, "m1", "m2"))
}

func TestApplier_WorklogDelete_ClearsAllMarkers(t *testing.T) {
	s := applierStore(t)
	seedLedger(t, s, "m1", "m2")
	require.NoError(t, s.MarkWorklogged("PROJ-1", "10001", []string{"m1", "m2"}))
	w := &worklogWriter{}
	a := worklogAction(t, s, "PROJ-1", store.WorklogPayload{Op: store.WorklogOpDelete, WorklogID: "10001", IdempotencyKey: "idem"})

	require.NoError(t, gate.NewApplier(s, w, "PROJ", writableTrackers("PROJ")).Apply(a))

	assert.Equal(t, []string{"DeleteWorklog:PROJ-1:10001"}, w.calls)
	assert.Empty(t, logged(t, s, "m1", "m2"))
	assert.Equal(t, store.StatusApplied, actionRow(t, s, a.ID).Status)
}

func TestApplier_Worklog_UnwritableScopeRefused(t *testing.T) {
	s := applierStore(t)
	seedLedger(t, s, "m1")
	w := &worklogWriter{addID: "10001"}
	a := worklogAction(t, s, "OTHER-1", addPayload("m1"))

	require.Error(t, gate.NewApplier(s, w, "PROJ", writableTrackers("PROJ")).Apply(a))

	assert.Empty(t, w.calls)
	assert.Contains(t, actionRow(t, s, a.ID).Error, "OTHER")
}

func TestApplier_Worklog_WriterWithoutWorklogSupport(t *testing.T) {
	s := applierStore(t)
	seedLedger(t, s, "m1")
	w := &fakeWriter{}
	a := worklogAction(t, s, "PROJ-1", addPayload("m1"))

	require.Error(t, gate.NewApplier(s, w, "PROJ", writableTrackers("PROJ")).Apply(a))

	assert.Empty(t, w.calls)
	assert.Contains(t, actionRow(t, s, a.ID).Error,
		fmt.Sprintf("action %d: the configured tracker writer cannot write worklogs", a.ID))
}

func TestApplier_Worklog_MalformedPayload(t *testing.T) {
	update := addPayload("m1")
	update.Op = store.WorklogOpUpdate
	del := store.WorklogPayload{Op: store.WorklogOpDelete}
	noIDs := addPayload()
	zeroAdd := addPayload("m1")
	zeroAdd.TimeSpentSeconds = 0
	zeroUpdate := addPayload("m1")
	zeroUpdate.Op, zeroUpdate.WorklogID, zeroUpdate.TimeSpentSeconds = store.WorklogOpUpdate, "10001", 0
	unknown := addPayload("m1")
	unknown.Op = "merge"
	noKey := addPayload("m1")
	noKey.IdempotencyKey = ""

	for name, p := range map[string]store.WorklogPayload{
		"unknown op": unknown, "add without message ids": noIDs,
		"update without worklog_id": update, "delete without worklog_id": del,
		"add under one second": zeroAdd, "update under one second": zeroUpdate,
		"add without idempotency_key": noKey,
	} {
		t.Run(name, func(t *testing.T) {
			s := applierStore(t)
			seedLedger(t, s, "m1")
			w := &worklogWriter{addID: "10001"}
			a := worklogAction(t, s, "PROJ-1", p)

			require.Error(t, gate.NewApplier(s, w, "PROJ", writableTrackers("PROJ")).Apply(a))

			assert.Empty(t, w.calls)
			assert.Equal(t, store.StatusFailed, actionRow(t, s, a.ID).Status)
		})
	}
}
