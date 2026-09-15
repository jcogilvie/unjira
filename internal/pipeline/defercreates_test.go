package pipeline_test

// defercreates_test.go covers finding F13: ProposeCreates reached narratives
// matching had skipped by cap, and proposed tickets for work already tracked.
//
// Both stages select oldest-first with the same cap over DIFFERENT populations —
// Match takes narratives with no PRIMARY link, ProposeCreates takes narratives with
// no link AT ALL. Create's population is a subset, and a subset's members sit at
// LOWER positions, so the narratives create reaches first are precisely the ones
// matching just skipped. Subsetting inverts the protection it appears to give.
//
// Observed: matching linked narratives 1-20 (exactly its cap) and stopped;
// ProposeCreates then ran with a pool of [6, 21, 22, 23, 24, 25, ...] and proposed a
// ticket for narrative 24 — whose eleven linked events each carried
// issue_key=PAAS-3898, and which a later pass matched at confidence 1.0. The ticket
// existed and was already Done.
//
// The fix is a precondition, not a filter on evidence: "no link" only means
// "untracked" once matching has examined everything. While matching is behind, it
// means "not looked at yet", and those are different facts. MatchRunResult.Remaining
// (added for F10) already answers this at no cost, in scope immediately before
// RunReconcile.
//
// All-or-nothing rather than per-narrative deliberately. Per-narrative deferral
// needs a record of which narratives matching examined, and matching writes nothing
// when it finds no candidates ("untracked work is the default path" —
// correlator/match.go). Since a later matching pass is the very thing that creates
// the link, deferring costs latency while proposing costs a duplicate ticket, and a
// create is the highest-blast-radius action unjira proposes.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/pipeline"
	"github.com/jcogilvie/unjira/internal/store"
)

// TestRenderReconcileResult_SaysWhenCreatesWereDeferred is what stops the fix
// becoming a silent stall. A permanently-behind matching stage would otherwise make
// unjira quietly stop proposing creates forever, which is F10's failure mode
// exactly: a pass that skipped work looking like a pass with none.
func TestRenderReconcileResult_SaysWhenCreatesWereDeferred(t *testing.T) {
	out := pipeline.RenderReconcileResult(pipeline.ReconcileRunResult{
		CreatesDeferred: 7,
	})

	assert.Contains(t, out, "7",
		"the count of unmatched narratives must appear: it is what tells the operator how far "+
			"matching has to get before creates resume")
	assert.Contains(t, strings.ToLower(out), "defer",
		"and it must name what was skipped, not merely report a number")
}

// TestRenderReconcileResult_SilentWhenNothingDeferred keeps it from becoming noise.
// A caught-up pass must say nothing, for the same reason the remainder line is
// silent at zero: a message on every ordinary pass trains an operator to skip it,
// which is how the original stderr cap warning got ignored.
func TestRenderReconcileResult_SilentWhenNothingDeferred(t *testing.T) {
	out := pipeline.RenderReconcileResult(pipeline.ReconcileRunResult{})

	assert.NotContains(t, strings.ToLower(out), "defer",
		"a pass that proposed creates normally must be silent about deferral")
}

// TestRunReconcile_DefersCreatesWhileMatchingIsBehind is the guard itself, asserted
// through the real pipeline entry point rather than only on the renderer.
//
// Written after a drill exposed the gap: disabling the guard entirely left every
// test in this file passing, because they all covered the RENDERING of a deferral
// and none covered the DECIDING. A test that cannot fail when the feature is removed
// is not testing the feature.
//
// The fixture is the F13 shape in miniature: untracked work that a create would be
// proposed for, and a matching stage that has not caught up.
func TestRunReconcile_DefersCreatesWhileMatchingIsBehind(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "defer.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	seedUntrackedForPipeline(t, s)

	// Would say "worth tracking" if it were ever asked.
	client := &pipelineFakeLLM{responses: []string{`{"worth_tracking":true,` +
		`"summary":"Rework the ingest retry path","description":"d",` +
		`"confidence":0.9,"rationale":"substantial"}`}}

	cfg := config.Config{Reconciler: config.ReconcilerConfig{
		MaxNarrativesPerPass: 10, MinConfidenceToPropose: 0.5,
	}}

	got, err := pipeline.RunReconcile(context.Background(), s, nil, client, cfg,
		pipeline.ReconcileOptions{UnmatchedNarratives: 3})
	require.NoError(t, err)

	assert.Empty(t, got.Persisted,
		"no create may be proposed while matching is behind: this narrative has no link because "+
			"matching has not LOOKED at it, which is not the same fact as the work being untracked")
	assert.Equal(t, 3, got.CreatesDeferred,
		"and the pass must report the deferral, or a permanently-behind matching stage silently "+
			"stops proposing creates forever")
	assert.Empty(t, client.prompts,
		"the model must not be consulted either: it would be asked a question whose premise "+
			"(\"no tracker issue exists for any of it\") is not yet known to be true")
}

// TestRunReconcile_ProposesCreatesOnceMatchingIsCaughtUp is the other half. The
// guard must not become a permanent off switch — deferral is "not yet", and a drained
// matching stage is what makes "no link" mean "untracked".
func TestRunReconcile_ProposesCreatesOnceMatchingIsCaughtUp(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "caught-up.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	seedUntrackedForPipeline(t, s)

	client := &pipelineFakeLLM{responses: []string{`{"worth_tracking":true,` +
		`"summary":"Rework the ingest retry path","description":"d",` +
		`"confidence":0.9,"rationale":"substantial"}`}}

	cfg := config.Config{Reconciler: config.ReconcilerConfig{
		MaxNarrativesPerPass: 10, MinConfidenceToPropose: 0.5,
	}}

	got, err := pipeline.RunReconcile(context.Background(), s, nil, client, cfg,
		pipeline.ReconcileOptions{UnmatchedNarratives: 0})
	require.NoError(t, err)

	require.Len(t, got.Persisted, 1, "a drained matching stage means creates resume")
	assert.Equal(t, "create", got.Persisted[0].Type)
	assert.Zero(t, got.CreatesDeferred)
}
