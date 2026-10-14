package pipeline_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/pipeline"
	"github.com/jcogilvie/unjira/internal/reconciler"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// seedUntrackedForPipeline inserts a narrative with one event and NO links.
func seedUntrackedForPipeline(t *testing.T, s *store.Store) int64 {
	t.Helper()

	base := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)
	nid, err := s.InsertNarrative(base, base.Add(time.Hour),
		"rewrote the ingest retry path", "replaced fixed retries with backoff")
	require.NoError(t, err)

	e := events.NewEvent("claude_code", "pw:1", base, "replaced fixed retries with backoff+jitter")
	_, err = s.InsertEvent(e)
	require.NoError(t, err)
	eid, err := s.EventIDByExternalID("claude_code", "pw:1")
	require.NoError(t, err)
	require.NoError(t, s.LinkMembers(nid, []int64{eid}, 1))

	return nid
}

// withDefaultTicketIn gives cfg a writable local tracker as its default destination: with
// destinations, untracked work outside every scope goes where default_ticket_in says,
// and nowhere when it says nothing.
func withDefaultTicketIn(cfg config.Config) config.Config {
	cfg.Connections = []config.Connection{{Name: "local", Kind: config.KindLocal}}
	cfg.Trackers = []config.Tracker{{
		Name: "dev", Connection: "local", Scopes: []string{"PROJ"},
		WritableScopes: []string{"PROJ"}, DefaultScope: "PROJ",
	}}
	cfg.DefaultTicketIn = []string{"dev"}

	return cfg
}

// TestRunReconcile_UntrackedWorkReachesTheQueue is #168's fix, asserted through
// the real pipeline entry point and all the way to a persisted row.
//
// No opt-in flag is needed, and none exists: gate.Decide refuses to auto-apply a
// create without explicit human graduation, so the review queue is the gate. A
// proposal is a queue entry, not a mutation.
func TestRunReconcile_UntrackedWorkReachesTheQueue(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "on.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	nid := seedUntrackedForPipeline(t, s)

	client := &pipelineFakeLLM{responses: []string{`{"worth_tracking":true,` +
		`"summary":"Rework the ingest retry path",` +
		`"description":"Replaced fixed retries with exponential backoff and jitter.",` +
		`"confidence":0.8,"rationale":"substantial, lasting outcome"}`}}

	cfg := withDefaultTicketIn(config.Config{Reconciler: config.ReconcilerConfig{
		MaxNarrativesPerPass: 10, MinConfidenceToPropose: 0.5,
	}})

	got, err := pipeline.RunReconcile(context.Background(), s, nil, client, cfg, pipeline.ReconcileOptions{})

	require.NoError(t, err)
	require.Len(t, got.Persisted, 1,
		"untracked work must reach the review queue, not vanish")

	row := got.Persisted[0]
	assert.Equal(t, "create", row.Type)
	assert.Equal(t, nid, row.NarrativeID)
	assert.Empty(t, row.IssueKey, "a create names no issue; the tracker assigns the key")
	assert.JSONEq(t,
		`{"summary":"Rework the ingest retry path",`+
			`"description":"Replaced fixed retries with exponential backoff and jitter.","scope":"PROJ"}`,
		row.Payload,
		"the payload must be the shape gate.Applier's createPayload decodes")

	queued, err := s.ActionsByStatus("proposed")
	require.NoError(t, err)
	require.Len(t, queued, 1, "and it must be reviewable, i.e. actually in the queue")
}

// TestRunReconcile_AnEmptyDestinationSetIsRecordedAsASuppression: with no
// default_ticket_in, untracked work outside every scope is ticketed nowhere. Nothing is
// asked of the model, and the pass records why, like the other suppressions.
func TestRunReconcile_AnEmptyDestinationSetIsRecordedAsASuppression(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "empty.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	nid := seedUntrackedForPipeline(t, s)
	client := &pipelineFakeLLM{responses: []string{`{"worth_tracking":true,"summary":"x","description":"y"}`}}
	cfg := withDefaultTicketIn(config.Config{Reconciler: config.ReconcilerConfig{
		MaxNarrativesPerPass: 10, MinConfidenceToPropose: 0.5,
	}})
	cfg.DefaultTicketIn = nil

	got, err := pipeline.RunReconcile(context.Background(), s, nil, client, cfg, pipeline.ReconcileOptions{})

	require.NoError(t, err)
	assert.Empty(t, got.Persisted, "no destination, no proposal")
	assert.Empty(t, client.prompts)

	rows, err := s.ActionsForNarrative(nid)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, store.StatusSuppressed, rows[0].Status)
	assert.Contains(t, rows[0].Rationale, "default_ticket_in")
}

// TestRunReconcile_WorkWithNoConfidentPrimaryIsProposedAsACreateNamingItsLinks: RunReconcile
// hands match.confidence_floor to both halves, so a primary the model doubted is drafted
// onto by nobody and the work reaches the queue as a create whose payload names the
// doubted ticket, read live, for the reviewer to link instead (findings F58, F64).
func TestRunReconcile_WorkWithNoConfidentPrimaryIsProposedAsACreateNamingItsLinks(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "doubted.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	nid := seedUntrackedForPipeline(t, s)
	require.NoError(t, s.AddNarrativeIssues(nid, []store.NarrativeIssue{
		{IssueKey: "PROJ-1", Role: store.RolePrimary, Provenance: "prose_first", Confidence: 0.3},
	}))

	client := &pipelineFakeLLM{responses: []string{`{"worth_tracking":true,` +
		`"summary":"Rework the ingest retry path","description":"d","confidence":0.8,` +
		`"rationale":"its own work; PROJ-1 is only the old retry bug"}`}}
	tracker := &pipelineFakeTracker{issues: map[string]tasktracker.Issue{
		"PROJ-1": {Key: "PROJ-1", Summary: "Retries hammer the API", StatusName: "Done"},
	}}

	cfg := withDefaultTicketIn(config.Config{
		Reconciler: config.ReconcilerConfig{MaxNarrativesPerPass: 10, MinConfidenceToPropose: 0.5},
		Match:      config.MatchConfig{ConfidenceFloor: 0.7},
	})

	got, err := pipeline.RunReconcile(context.Background(), s, tracker, client, cfg, pipeline.ReconcileOptions{})

	require.NoError(t, err)
	require.Len(t, client.prompts, 1, "one create call, and no drafting call onto the doubted primary")
	require.Len(t, got.Persisted, 1)

	row := got.Persisted[0]
	assert.Equal(t, "create", row.Type)
	assert.Equal(t, nid, row.NarrativeID)
	assert.Equal(t, []reconciler.CreateCandidate{{
		IssueKey: "PROJ-1", Role: store.RolePrimary, Confidence: 0.3, Provenance: "prose_first",
		Summary: "Retries hammer the API", Status: "Done",
	}}, reconciler.CreateCandidatesOf(row.Payload))
	assert.Zero(t, got.Remaining, "the reconciler recorded its examination")
	assert.Zero(t, got.CreatesRemaining, "and the create backlog is counted under the same floor")

	rendered := pipeline.RenderReconcileResult(got)
	assert.Contains(t, rendered, "candidate PROJ-1 (primary, confidence 0.30")
	assert.Contains(t, rendered, "Retries hammer the API")
	assert.Contains(t, rendered, "below match.confidence_floor 0.70")
}
