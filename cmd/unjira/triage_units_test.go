package main

// triage_units_test.go drives approval units (D-18) through `unjira triage`'s
// session construction end to end, over a real store and the token ledger.

import (
	"bufio"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/clients/local"
	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/gate"
	"github.com/jcogilvie/unjira/internal/store"
)

// unitFixture is narrative nid on a local PROJ-1 with ledger rows m1 and m2 on
// its event e1, a comment of that narrative, and a proposed worklog add over m1, m2.
type unitFixture struct {
	s         *store.Store
	cfg       config.Config
	app       *appContext
	nid       int64
	commentID int64
	worklogID int64
}

func newUnitFixture(t *testing.T, commentStatus string) unitFixture {
	t.Helper()
	s := triageTestStore(t)
	issueKey, err := s.InsertLocalIssue("PROJ", "the ticket", "Task", "", nil)
	require.NoError(t, err)
	now := time.Now().UTC()
	nid := seedReconcilableNarrative(t, s, "e1", issueKey, now.Add(-time.Hour))
	require.NoError(t, s.UpsertMessageUsage([]store.MessageUsage{
		{MessageID: "m1", TranscriptID: "t", OccurredAt: now.Add(-50 * time.Minute), Model: "claude-opus", InputTokens: 1, CoveringExternalID: "e1"},
		{MessageID: "m2", TranscriptID: "t", OccurredAt: now.Add(-45 * time.Minute), Model: "claude-opus", InputTokens: 1, CoveringExternalID: "e1"},
	}))

	commentID, err := s.InsertAction(store.ActionRow{
		NarrativeID: nid, Type: "comment", IssueKey: issueKey,
		Payload: `{"body":"the work landed"}`, Confidence: 0.9, Status: commentStatus,
	})
	require.NoError(t, err)

	payload, err := json.Marshal(store.WorklogPayload{
		Op: store.WorklogOpAdd, Started: now.Add(-50 * time.Minute), TimeSpentSeconds: 300,
		Comment: "worked on it", MessageIDs: []string{"m1", "m2"}, IdempotencyKey: "k1",
	})
	require.NoError(t, err)
	worklogID, err := s.InsertAction(store.ActionRow{
		Type: store.ActionTypeWorklog, IssueKey: issueKey,
		Payload: string(payload), Confidence: 0.9, Status: store.StatusProposed,
	})
	require.NoError(t, err)

	cfg := watchPassConfig()
	cfg.Connections = []config.Connection{{Name: "corp", Kind: config.KindJira}}
	cfg.Trackers = []config.Tracker{{Name: "test", Connection: "corp", Scopes: []string{"PROJ"}, WritableScopes: []string{"PROJ"}}}
	cfg.Match.ConfidenceFloor = 0.5

	return unitFixture{
		s: s, cfg: cfg, app: &appContext{config: cfg, store: s},
		nid: nid, commentID: commentID, worklogID: worklogID,
	}
}

func (f unitFixture) proposed(t *testing.T) []store.ActionRow {
	t.Helper()
	batch, err := f.s.ActionsByStatus(store.StatusProposed)
	require.NoError(t, err)

	return batch
}

func scriptedPrompter(input string) *terminalPrompter {
	return &terminalPrompter{in: bufio.NewScanner(strings.NewReader(input))}
}

func TestTriage_AWorklogAndItsCommentAreOneUnit(t *testing.T) {
	f := newUnitFixture(t, store.StatusProposed)

	out := captureStdout(t, func() {
		session := f.app.newTriageSession(f.proposed(t), scriptedPrompter("k\n"), nil)
		require.NoError(t, session.Run())
	})

	assert.Contains(t, out, "approval unit")
	assert.Equal(t, 1, strings.Count(out, "s[k]ip"), "one prompt for the whole unit")
}

func TestTriage_DropThenApproveAppliesTheRestAndRulesOnTheDropped(t *testing.T) {
	f := newUnitFixture(t, store.StatusProposed)

	captureStdout(t, func() {
		session := f.app.newTriageSession(f.proposed(t), scriptedPrompter("d 1 wrong ticket\na\n"), nil)
		require.NoError(t, session.Run())
		require.NoError(t, f.app.recordRulings(session.Rulings()))
		applier := gate.NewApplier(f.s, &worklogLocalTracker{Tracker: local.New(f.s)}, "PROJ", f.cfg.Trackers)
		for _, a := range session.Approved() {
			require.NoError(t, applier.Apply(a))
		}
	})

	worklog, err := f.s.GetAction(f.worklogID)
	require.NoError(t, err)
	assert.Equal(t, store.StatusApplied, worklog.Status)

	comment, err := f.s.GetAction(f.commentID)
	require.NoError(t, err)
	assert.Equal(t, store.StatusRejected, comment.Status)
	assert.Equal(t, "wrong ticket", comment.Feedback)

	corrections, _, err := f.s.CorrectionsSince(store.CorrectionsCursor{})
	require.NoError(t, err)
	require.Len(t, corrections, 1)
	assert.Equal(t, f.commentID, corrections[0].ActionID)
	assert.Equal(t, "wrong ticket", corrections[0].Feedback)

	msgs, err := f.s.MessageAttributions(f.cfg.Match.ConfidenceFloor)
	require.NoError(t, err)
	got := map[string]string{}
	for _, m := range msgs {
		got[m.MessageID] = m.WorklogID
	}
	assert.Empty(t, cmp.Diff(map[string]string{"m1": "10001", "m2": "10001"}, got))
}

func TestTriage_AGraduatedMemberLeavesNoUnit(t *testing.T) {
	f := newUnitFixture(t, store.StatusApplied)
	batch := f.proposed(t)
	require.Len(t, batch, 1)
	require.Equal(t, f.worklogID, batch[0].ID)

	out := captureStdout(t, func() {
		session := f.app.newTriageSession(batch, scriptedPrompter("k\n"), nil)
		require.NoError(t, session.Run())
	})

	assert.NotContains(t, out, "approval unit")
	assert.NotContains(t, out, "[d]rop")
}

func TestTriage_AWorklogOverTwoNarrativesIsReviewedAlone(t *testing.T) {
	f := newUnitFixture(t, store.StatusProposed)
	now := time.Now().UTC()
	nid2 := seedReconcilableNarrative(t, f.s, "e2", "PROJ-1", now.Add(-30*time.Minute))
	_, err := f.s.InsertAction(store.ActionRow{
		NarrativeID: nid2, Type: "comment", IssueKey: "PROJ-1",
		Payload: `{"body":"more work"}`, Confidence: 0.9, Status: store.StatusProposed,
	})
	require.NoError(t, err)
	require.NoError(t, f.s.UpsertMessageUsage([]store.MessageUsage{
		{MessageID: "m2", TranscriptID: "t", OccurredAt: now.Add(-45 * time.Minute), Model: "claude-opus", InputTokens: 1, CoveringExternalID: "e2"},
	}))

	out := captureStdout(t, func() {
		// Two comments and the worklog, each reviewed alone: three skips.
		session := f.app.newTriageSession(f.proposed(t), scriptedPrompter(strings.Repeat("k\n", 3)), nil)
		require.NoError(t, session.Run())
	})

	assert.Contains(t, out, "covers work from narratives")
	assert.NotContains(t, out, "approval unit")
}
