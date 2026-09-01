//go:build live

package live

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/clients/jira"
	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/gate"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/triage"
)

// This file covers what fakes cannot: that a triage-approved action reaches a
// real tracker through the same gate.Applier `watch` uses, and that the
// writable-project scope holds against a live instance.
//
// Everything writes to throwaway issues this file creates and deletes, never a
// pre-existing one — the same discipline autocommit_test.go follows, and for the
// same reason: leaving residue on an issue other live tests read would be a bad
// way to learn that lesson. Config is built IN-PROCESS, so the real
// unjira.config.json is never consulted and this cannot be affected by what an
// operator has configured.

// TestLiveTriage_ApprovedActionReachesJira is the end-to-end proof for the
// review surface: a batch the reviewer approved applies for real.
func TestLiveTriage_ApprovedActionReachesJira(t *testing.T) {
	client := testClient(t)
	s := liveTriageStore(t)

	key := liveThrowawayIssue(t, client, "triage approve reaches jira")

	marker := "unjira-live-triage-" + time.Now().UTC().Format("150405.000")
	action := seedLiveTriageAction(t, s, key, "Triage-approved comment. Marker: "+marker)

	require.Empty(t, commentBodies(t, client, key), "a freshly created issue starts with no comments")

	session := triage.NewSession(
		context.Background(), []store.ActionRow{action}, nil, nil)
	session.ApproveAll()

	approved := session.Approved()
	require.Len(t, approved, 1)

	applier := gate.NewApplier(s, jira.NewTracker(client), testProject(), liveTriageConnections())
	require.NoError(t, applier.Apply(approved[0]))

	bodies := commentBodies(t, client, key)
	require.Len(t, bodies, 1, "exactly one comment, not zero and not two")
	assert.Contains(t, bodies[0], marker, "the comment on %s must be the one triage approved", key)

	row, err := s.GetAction(action.ID)
	require.NoError(t, err)
	assert.Equal(t, "applied", row.Status)
	require.NotNil(t, row.ExecutedAt)
	assert.Empty(t, row.Error, "a successful apply leaves no failure reason")
}

// TestLiveTriage_UnwritableProjectIsRefusedAgainstLiveJira proves the scope gate
// against a real instance rather than a fake: the tracker is reachable and the
// issue exists, so a refusal here can only come from writable_project_keys.
func TestLiveTriage_UnwritableProjectIsRefusedAgainstLiveJira(t *testing.T) {
	client := testClient(t)
	s := liveTriageStore(t)

	key := liveThrowawayIssue(t, client, "triage scope refusal")
	action := seedLiveTriageAction(t, s, key, "this must never be posted")

	// The connection reads the live project but declares NOTHING writable.
	applier := gate.NewApplier(s, jira.NewTracker(client), testProject(), []config.JiraConnection{{
		Name:        "live",
		ProjectKeys: []string{testProject()},
		// WritableProjectKeys deliberately empty.
	}})

	require.Error(t, applier.Apply(action))

	assert.Empty(t, commentBodies(t, client, key),
		"nothing may be posted when no project is writable, even though the issue exists")

	row, err := s.GetAction(action.ID)
	require.NoError(t, err)
	assert.Equal(t, "failed", row.Status)
	assert.Contains(t, row.Error, "writable_project_keys")
}

func liveTriageStore(t *testing.T) *store.Store {
	t.Helper()

	s, err := store.Open(filepath.Join(t.TempDir(), "triage.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	return s
}

// liveTriageConnections declares the live project both readable and writable,
// built in-process so the real config file is never consulted.
func liveTriageConnections() []config.JiraConnection {
	return []config.JiraConnection{{
		Name:                "live",
		ProjectKeys:         []string{testProject()},
		WritableProjectKeys: []string{testProject()},
	}}
}

func seedLiveTriageAction(t *testing.T, s *store.Store, issueKey, body string) store.ActionRow {
	t.Helper()

	now := time.Now().UTC()
	nid, err := s.InsertNarrative(now.Add(-time.Hour), now, "live triage test", "summary")
	require.NoError(t, err)

	// Encode via encoding/json rather than string concatenation: the body is
	// prose and will contain quotes, and gate.Applier decodes this payload with
	// a typed struct, so a hand-built string that happens to be invalid JSON
	// would fail as a decode error rather than as the thing under test.
	payload, err := json.Marshal(map[string]string{"body": body})
	require.NoError(t, err)

	id, err := s.InsertAction(store.ActionRow{
		NarrativeID: nid, Type: "comment", IssueKey: issueKey,
		Payload:    string(payload),
		Confidence: 0.9, Rationale: "seeded by internal/live/triage_test.go", Status: "proposed",
	})
	require.NoError(t, err)

	got, err := s.GetAction(id)
	require.NoError(t, err)

	return got
}
