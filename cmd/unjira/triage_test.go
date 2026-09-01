package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/gate"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
	"github.com/jcogilvie/unjira/internal/triage"
)

func TestParseDecision(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		wantVerb triage.Verb
		wantText string
		wantPos  []int
		wantErr  string
	}{
		{name: "approve", input: "a", wantVerb: triage.VerbApprove},
		{name: "reject with reason", input: "r not worth posting", wantVerb: triage.VerbReject, wantText: "not worth posting"},
		{name: "edit with correction", input: "e name the actual PR", wantVerb: triage.VerbEdit, wantText: "name the actual PR"},
		{name: "skip", input: "k", wantVerb: triage.VerbSkip},
		{name: "quit", input: "q", wantVerb: triage.VerbQuit},
		{name: "merge two positions", input: "m 1 3", wantVerb: triage.VerbMerge, wantPos: []int{1, 3}},
		{name: "split one position", input: "s 4", wantVerb: triage.VerbSplit, wantPos: []int{4}},
		{name: "target an issue key", input: "t PAAS-4010", wantVerb: triage.VerbTarget, wantText: "PAAS-4010"},
		{name: "whitespace tolerated", input: "  a  ", wantVerb: triage.VerbApprove},
		{name: "unknown verb", input: "z", wantErr: "unknown"},
		{name: "empty line", input: "", wantErr: "unknown"},
		{
			name:    "edit with no text is an error, not an empty correction",
			input:   "e",
			wantErr: "requires text",
		},
		{
			name:    "merge with one position is an error",
			input:   "m 1",
			wantErr: "two positions",
		},
		{
			name:    "merge with a non-numeric position",
			input:   "m 1 x",
			wantErr: "position",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseDecision(tc.input)

			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.wantVerb, got.Verb)
			assert.Equal(t, tc.wantText, got.Text)
			assert.Equal(t, tc.wantPos, got.Positions)
		})
	}
}

// triageRecordingWriter records which tracker calls actually happened.
type triageRecordingWriter struct {
	calls []string
}

func (w *triageRecordingWriter) AddComment(key, _ string) error {
	w.calls = append(w.calls, "AddComment:"+key)

	return nil
}

func (w *triageRecordingWriter) SetStatus(key string, target tasktracker.StatusCategory) error {
	w.calls = append(w.calls, "SetStatus:"+key+":"+string(target))

	return nil
}

func (w *triageRecordingWriter) CreateIssue(project, _, _, _ string, _ []string) (string, error) {
	w.calls = append(w.calls, "CreateIssue:"+project)

	return project + "-1", nil
}

// TestTriage_AutoApproveRespectsWriteScope pins what --auto-approve actually
// protects, and deliberately does NOT claim more.
//
// gate.Applier enforces exactly one gate: jira[].writable_project_keys.
// Graduated and ConfidenceFloor live in gate.Decide, which the approve path
// never calls — by design, since the auto-commit gate governs UNATTENDED writes
// and a human passing this flag is attending. An earlier draft of the design doc
// claimed all three gates held here and asked for a test proving it; that test
// would have encoded a safety property that does not exist.
func TestTriage_AutoApproveRespectsWriteScope(t *testing.T) {
	s := triageTestStore(t)
	paas := seedTriageAction(t, s, "PAAS-1")
	devsbx := seedTriageAction(t, s, "DEVSBX-1")

	writer := &triageRecordingWriter{}
	applier := gate.NewApplier(s, writer, "DEVSBX", []config.JiraConnection{{
		Name:                "dev",
		ProjectKeys:         []string{"PAAS", "DEVSBX"},
		WritableProjectKeys: []string{"DEVSBX"},
	}})

	require.Error(t, applier.Apply(paas), "PAAS is not writable")
	require.NoError(t, applier.Apply(devsbx), "DEVSBX is writable")

	assert.Equal(t, []string{"AddComment:DEVSBX-1"}, writer.calls,
		"only the writable-project action reached the tracker")

	failed, err := s.GetAction(paas.ID)
	require.NoError(t, err)
	assert.Equal(t, "failed", failed.Status)
	assert.Contains(t, failed.Error, "writable_project_keys",
		"the refusal reason must name the config key, so an operator knows what to change")
}

// TestTriage_ApproveAllMarksEveryAction: --auto-approve skips the PROMPT, so
// every action must come back approved without a Prompter ever being consulted.
func TestTriage_ApproveAllMarksEveryAction(t *testing.T) {
	s := triageTestStore(t)
	a := seedTriageAction(t, s, "DEVSBX-1")
	b := seedTriageAction(t, s, "DEVSBX-2")

	session := triage.NewSession(context.Background(), []store.ActionRow{a, b}, nil, nil)
	session.ApproveAll()

	approved := session.Approved()
	require.Len(t, approved, 2, "every action is approved without prompting")
	assert.Equal(t, a.ID, approved[0].ID)
	assert.Equal(t, b.ID, approved[1].ID)
}

func triageTestStore(t *testing.T) *store.Store {
	t.Helper()

	s, err := store.Open(filepath.Join(t.TempDir(), "triage.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	return s
}

func seedTriageAction(t *testing.T, s *store.Store, issueKey string) store.ActionRow {
	t.Helper()

	now := time.Now().UTC()
	nid, err := s.InsertNarrative(now.Add(-time.Hour), now, "t", "s")
	require.NoError(t, err)

	row := store.ActionRow{
		NarrativeID: nid, Type: "comment", IssueKey: issueKey,
		Payload: `{"body":"drafted"}`, Confidence: 0.9, Status: "proposed",
	}

	id, err := s.InsertAction(row)
	require.NoError(t, err)

	got, err := s.GetAction(id)
	require.NoError(t, err)

	return got
}
