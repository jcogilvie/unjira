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
	"github.com/jcogilvie/unjira/internal/store"
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
	require.NoError(t, s.AddNarrativeEvents(nid, []int64{eid}))

	return nid
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

	cfg := config.Config{Reconciler: config.ReconcilerConfig{
		MaxNarrativesPerPass: 10, MinConfidenceToPropose: 0.5,
	}}

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
			`"description":"Replaced fixed retries with exponential backoff and jitter."}`,
		row.Payload,
		"the payload must be the shape gate.Applier's createPayload decodes")

	queued, err := s.ActionsByStatus("proposed")
	require.NoError(t, err)
	require.Len(t, queued, 1, "and it must be reviewable, i.e. actually in the queue")
}
