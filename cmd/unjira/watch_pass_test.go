package main

// watch_pass_test.go exercises appContext.runWatchPass — the real
// collect->narrate->match->reconcile->auto-commit composition — against a
// real temp-file store, the local tasktracker backend (no network), and a
// fake llm.Client. watch_loop_test.go covers the LOOP's mechanics (lease,
// --once, surviving a pass failure) against a fake pass instead, so the two
// files are deliberately independent per runWatchPass's own doc comment.
//
// Every test here seeds its narrative(s) directly (InsertNarrative +
// AddNarrativeEvents + AddNarrativeIssues) rather than driving narrate/match
// through a real clustering pass: with no unlinked events in the collect
// window, RunNarrate short-circuits before any LLM call (nothing to
// cluster), and with no ticket-key-shaped artifacts on the seeded events,
// RunMatch's candidate gathering finds nothing and also short-circuits
// without touching the tracker or the pre-seeded narrative_issues rows. That
// leaves RunReconcile (and, on a clean pass, auto-commit) as the only real
// work runWatchPass does in these tests — which is exactly what's under
// test.
//
// The single most important test here is
// TestRunWatchPass_PartialReconcileAutoCommitsNothing: it is the property
// the whole slice exists to protect, and it is the test most likely to be
// gotten wrong (RunReconcile can return a non-nil error alongside a
// perfectly usable, non-empty Persisted slice — that is the exact trap).

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/clients/jira"
	"github.com/jcogilvie/unjira/internal/clients/local"
	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/gate"
	"github.com/jcogilvie/unjira/internal/llm"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// watchLLM is a fake llm.Client returning canned responses in call order,
// falling back to an empty-array response once exhausted — mirroring
// internal/pipeline's own pipelineFakeLLM (unexported there, so not
// reusable from this package).
type watchLLM struct {
	responses []string
	prompts   []string
}

func (f *watchLLM) Complete(_ context.Context, _, userPrompt string) (string, llm.Usage, error) {
	f.prompts = append(f.prompts, userPrompt)

	idx := len(f.prompts) - 1
	if idx >= len(f.responses) {
		return "[]", llm.Usage{}, nil
	}

	return f.responses[idx], llm.Usage{}, nil
}

var _ llm.Client = (*watchLLM)(nil)

// watchPassStore opens a fresh temp-file store, matching every other
// package's *_test.go precedent for exercising real SQLite rather than a
// mock.
func watchPassStore(t *testing.T) *store.Store {
	t.Helper()

	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	return s
}

// watchPassConfig returns a valid config.Config for a runWatchPass test:
// every stage's Validate must pass before the pass itself runs. AutoCommit
// is left nil (the untouched default) unless a test overrides it.
func watchPassConfig() config.Config {
	return config.Config{
		LLM:        config.LLMConfig{Model: "test-model", ContextWindowTokens: 128000},
		Correlator: config.CorrelatorConfig{TailSummarizeThresholdTokens: 1_000_000, RecentEventsKept: 20},
		Reconciler: config.ReconcilerConfig{MaxNarrativesPerPass: 10, MinConfidenceToPropose: 0.5},
	}
}

// watchPassWritableConnections is the test-default write scope for this
// file's tests, all of which write against the local "PROJ" project.
func watchPassWritableConnections() []config.JiraConnection {
	return []config.JiraConnection{
		{Name: "test", ProjectKeys: []string{"PROJ"}, WritableProjectKeys: []string{"PROJ"}},
	}
}

// seedReconcilableNarrative inserts a narrative with one linked event (a
// non-empty delta for Reconcile to work from) and a `primary`
// narrative_issues link to issueKey — the minimal shape Reconcile drafts
// against. Its event carries no ticket-key artifacts, so RunMatch's
// candidate gathering finds nothing for it and never touches the tracker or
// this pre-seeded link. windowStart orders narratives for
// NarrativesWithActionableLinks's `ORDER BY window_start, id`.
func seedReconcilableNarrative(
	t *testing.T, s *store.Store, extID, issueKey string, windowStart time.Time,
) int64 {
	t.Helper()

	nid, err := s.InsertNarrative(windowStart, windowStart.Add(time.Hour), "did the work", "summary of the work")
	require.NoError(t, err)

	_, err = s.InsertEvent(events.NewEvent("claude_code", extID, windowStart, "did some work"))
	require.NoError(t, err)
	eid, err := s.EventIDByExternalID("claude_code", extID)
	require.NoError(t, err)
	require.NoError(t, s.AddNarrativeEvents(nid, []int64{eid}))

	require.NoError(t, s.WithTx(func(tx *store.Tx) error {
		return tx.AddNarrativeIssues(nid, []store.NarrativeIssue{{
			IssueKey: issueKey, Role: "primary", Provenance: "branch", Confidence: 0.9, Connection: "local",
		}})
	}))

	return nid
}

// transportFailingTracker wraps local.Tracker, failing GetIssue for exactly
// one key with a transport-shaped error (mirroring jira.Error{Status: 503},
// which correlator.IsTransportError treats as transport) — so a test can
// force reconciler.Reconcile's per-narrative transport-error path without a
// real Jira connection. Every other call passes through to the embedded
// local.Tracker unchanged.
type transportFailingTracker struct {
	*local.Tracker

	failKey string
}

func (p *transportFailingTracker) GetIssue(key string) (tasktracker.Issue, error) {
	if key == p.failKey {
		return tasktracker.Issue{}, &jira.Error{Status: 503, Message: "service unavailable"}
	}

	return p.Tracker.GetIssue(key)
}

var _ tasktracker.TaskTracker = (*transportFailingTracker)(nil)

func TestRunWatchPass_HappyPathAppliesAGraduatedHighConfidenceComment(t *testing.T) {
	s := watchPassStore(t)
	tracker := local.New(s)

	issueKey, err := s.InsertLocalIssue("PROJ", "the ticket", "Task", "", nil)
	require.NoError(t, err)

	now := time.Now().UTC()
	seedReconcilableNarrative(t, s, "e1", issueKey, now.Add(-time.Hour))

	client := &watchLLM{responses: []string{
		fmt.Sprintf(`[{"issue_key":%q,"type":"comment","body":"the work landed","confidence":0.9,"rationale":"delta shows it"}]`, issueKey),
	}}

	cfg := watchPassConfig()
	cfg.AutoCommit = map[string]config.AutoCommitRule{"comment": {ConfidenceFloor: 0.5, Graduated: true}}
	app := &appContext{config: cfg, store: s}
	applier := gate.NewApplier(s, tracker, "PROJ", watchPassWritableConnections())

	err = app.runWatchPass(t.Context(), client, tracker, applier, nil, nil, correlator.SingleTracker(tracker), 24*time.Hour, false)
	require.NoError(t, err)

	comments, err := s.LocalIssueComments(issueKey)
	require.NoError(t, err)
	require.Equal(t, []string{"the work landed"}, comments,
		"a graduated, above-floor action must be applied to the real tracker")

	actions, err := s.ActionsForNarrative(1)
	require.NoError(t, err)
	require.Len(t, actions, 1)
	assert.Equal(t, "applied", actions[0].Status)
}

// TestRunWatchPass_WriteScopeRefusesAnUnwritableProject is the auto-commit
// half of test 3 from
// docs/superpowers/specs/2026-08-27-write-scope-design.md's testing section
// ("the hole test") — a graduated, above-floor action against a project that
// is readable but NOT in writable_project_keys must still be refused, through
// runWatchPass's real composition. See
// TestActionsDecide_ApproveRefusesAnUnwritableProject in actions_test.go for
// the paired --approve-route half.
func TestRunWatchPass_WriteScopeRefusesAnUnwritableProject(t *testing.T) {
	s := watchPassStore(t)
	tracker := local.New(s)

	issueKey, err := s.InsertLocalIssue("PROJ", "the ticket", "Task", "", nil)
	require.NoError(t, err)

	now := time.Now().UTC()
	seedReconcilableNarrative(t, s, "e1", issueKey, now.Add(-time.Hour))

	client := &watchLLM{responses: []string{
		fmt.Sprintf(`[{"issue_key":%q,"type":"comment","body":"the work landed","confidence":0.9,"rationale":"delta shows it"}]`, issueKey),
	}}

	cfg := watchPassConfig()
	cfg.AutoCommit = map[string]config.AutoCommitRule{"comment": {ConfidenceFloor: 0.5, Graduated: true}}
	app := &appContext{config: cfg, store: s}
	// PROJ is readable (in ProjectKeys, for JiraConnectionForProject to
	// resolve) but deliberately not in WritableProjectKeys.
	unwritable := []config.JiraConnection{{Name: "test", ProjectKeys: []string{"PROJ"}}}
	applier := gate.NewApplier(s, tracker, "PROJ", unwritable)

	err = app.runWatchPass(t.Context(), client, tracker, applier, nil, nil, correlator.SingleTracker(tracker), 24*time.Hour, false)
	require.Error(t, err, "PROJ is not writable: the pass must surface a refusal")

	comments, err := s.LocalIssueComments(issueKey)
	require.NoError(t, err)
	assert.Empty(t, comments, "the tracker must never be called for an unwritable project")

	actions, err := s.ActionsForNarrative(1)
	require.NoError(t, err)
	require.Len(t, actions, 1)
	assert.Equal(t, "failed", actions[0].Status)
	assert.Contains(t, actions[0].Error, "PROJ")
}

// TestRunWatchPass_DefaultConfigAutoCommitsNothing is the safety property
// the whole gate rests on, exercised through runWatchPass's real
// composition rather than only at gate.Decide's unit level: a.config with
// no AutoCommit block at all (the zero value, what an untouched config
// produces) must leave a high-confidence action at status=proposed, and the
// tracker's write methods must never be called.
func TestRunWatchPass_DefaultConfigAutoCommitsNothing(t *testing.T) {
	s := watchPassStore(t)
	tracker := local.New(s)

	issueKey, err := s.InsertLocalIssue("PROJ", "the ticket", "Task", "", nil)
	require.NoError(t, err)

	now := time.Now().UTC()
	seedReconcilableNarrative(t, s, "e1", issueKey, now.Add(-time.Hour))

	client := &watchLLM{responses: []string{
		fmt.Sprintf(`[{"issue_key":%q,"type":"comment","body":"the work landed","confidence":1.0,"rationale":"delta shows it"}]`, issueKey),
	}}

	cfg := watchPassConfig() // AutoCommit is nil: the untouched default.
	app := &appContext{config: cfg, store: s}
	applier := gate.NewApplier(s, tracker, "PROJ", watchPassWritableConnections())

	err = app.runWatchPass(t.Context(), client, tracker, applier, nil, nil, correlator.SingleTracker(tracker), 24*time.Hour, false)
	require.NoError(t, err)

	comments, err := s.LocalIssueComments(issueKey)
	require.NoError(t, err)
	assert.Empty(t, comments, "default config (no auto_commit block) must never write to the tracker")

	actions, err := s.ActionsForNarrative(1)
	require.NoError(t, err)
	require.Len(t, actions, 1)
	assert.Equal(t, "proposed", actions[0].Status)
}

// TestRunWatchPass_PartialReconcileAutoCommitsNothing is the test the whole
// slice exists to protect: Reconcile isolates one narrative's transport
// failure per narrative, so it returns BOTH a usable partial result (the
// OTHER narrative's action, cleanly drafted and persisted as
// status=proposed) AND a non-nil error. The all-or-nothing property means
// auto-commit must not act on ANY of this pass's freshly-proposed actions
// when that happens — not because the good action was somehow invalid, but
// because the pass as a WHOLE did not finish cleanly. Getting this wrong
// (checking len(Persisted) > 0 instead of err == nil) is exactly the trap
// named in the task brief and in RunReconcile's own doc comment.
func TestRunWatchPass_PartialReconcileAutoCommitsNothing(t *testing.T) {
	s := watchPassStore(t)
	tracker := local.New(s)

	healthyKey, err := s.InsertLocalIssue("PROJ", "the healthy ticket", "Task", "", nil)
	require.NoError(t, err)
	const failingKey = "FAIL-1" // never resolves against the local backend either way

	now := time.Now().UTC()
	seedReconcilableNarrative(t, s, "e1", healthyKey, now.Add(-2*time.Hour))
	seedReconcilableNarrative(t, s, "e2", failingKey, now.Add(-time.Hour))

	client := &watchLLM{responses: []string{
		fmt.Sprintf(`[{"issue_key":%q,"type":"comment","body":"the work landed","confidence":0.9,"rationale":"delta shows it"}]`, healthyKey),
	}}

	failing := &transportFailingTracker{Tracker: tracker, failKey: failingKey}

	cfg := watchPassConfig()
	cfg.AutoCommit = map[string]config.AutoCommitRule{"comment": {ConfidenceFloor: 0.5, Graduated: true}}
	app := &appContext{config: cfg, store: s}
	applier := gate.NewApplier(s, failing, "PROJ", watchPassWritableConnections())

	err = app.runWatchPass(t.Context(), client, failing, applier, nil, nil, correlator.SingleTracker(failing), 24*time.Hour, false)
	require.Error(t, err, "the unreachable narrative's transport error must surface")

	// The healthy narrative's action WAS persisted (Reconcile/Persist's own
	// asymmetry: a partial pass still persists what drafted cleanly — see
	// RunReconcile's doc comment) but must remain status=proposed: this is
	// the auto-commit decision the all-or-nothing property withholds.
	healthyActions, err := s.ActionsForNarrative(1)
	require.NoError(t, err)
	require.Len(t, healthyActions, 1, "the healthy narrative's action was drafted and persisted")
	assert.Equal(t, "proposed", healthyActions[0].Status,
		"auto-commit must not run when the pass did not reconcile cleanly, even though "+
			"this narrative's own action drafted fine")

	comments, err := s.LocalIssueComments(healthyKey)
	require.NoError(t, err)
	assert.Empty(t, comments, "zero tracker writes: the all-or-nothing property")
}

// writeFailingTracker wraps local.Tracker, failing AddComment for exactly
// one issue key while every other call (including every other TaskWriter
// method) passes through unchanged. Used to force Applier.Apply's error
// path for one action without a hand-rolled fake reimplementing the whole
// TaskTracker surface.
type writeFailingTracker struct {
	*local.Tracker

	failKey string
}

func (w *writeFailingTracker) AddComment(key, text string) error {
	if key == w.failKey {
		return fmt.Errorf("jira: 503 service unavailable")
	}

	return w.Tracker.AddComment(key, text)
}

var _ tasktracker.TaskTracker = (*writeFailingTracker)(nil)

// TestRunWatchPass_OneApplyFailureDoesNotBlockTheOthers proves
// RunAutoCommit's isolation (each action independent, one write failure
// recorded as status=failed on that row alone) survives through
// runWatchPass's real wiring, not merely at RunAutoCommit's own unit level.
func TestRunWatchPass_OneApplyFailureDoesNotBlockTheOthers(t *testing.T) {
	s := watchPassStore(t)
	baseTracker := local.New(s)

	firstKey, err := s.InsertLocalIssue("PROJ", "first ticket", "Task", "", nil)
	require.NoError(t, err)
	secondKey, err := s.InsertLocalIssue("PROJ", "second ticket", "Task", "", nil)
	require.NoError(t, err)

	now := time.Now().UTC()
	seedReconcilableNarrative(t, s, "e1", firstKey, now.Add(-2*time.Hour))
	seedReconcilableNarrative(t, s, "e2", secondKey, now.Add(-time.Hour))

	client := &watchLLM{responses: []string{
		fmt.Sprintf(`[{"issue_key":%q,"type":"comment","body":"first landed","confidence":0.9,"rationale":"r"}]`, firstKey),
		fmt.Sprintf(`[{"issue_key":%q,"type":"comment","body":"second landed","confidence":0.9,"rationale":"r"}]`, secondKey),
	}}

	cfg := watchPassConfig()
	cfg.AutoCommit = map[string]config.AutoCommitRule{"comment": {ConfidenceFloor: 0.5, Graduated: true}}
	app := &appContext{config: cfg, store: s}

	// The reader half (narrate/match/reconcile) uses the healthy tracker;
	// only the applier's writer fails, and only for firstKey.
	failingWriter := &writeFailingTracker{Tracker: baseTracker, failKey: firstKey}
	applier := gate.NewApplier(s, failingWriter, "PROJ", watchPassWritableConnections())

	err = app.runWatchPass(t.Context(), client, baseTracker, applier, nil, nil, correlator.SingleTracker(baseTracker), 24*time.Hour, false)
	require.Error(t, err, "one action's write failure is surfaced by the pass, not swallowed")

	firstActions, err := s.ActionsForNarrative(1)
	require.NoError(t, err)
	require.Len(t, firstActions, 1)
	assert.Equal(t, "failed", firstActions[0].Status)

	secondActions, err := s.ActionsForNarrative(2)
	require.NoError(t, err)
	require.Len(t, secondActions, 1)
	assert.Equal(t, "applied", secondActions[0].Status,
		"the second action's own write must still have been attempted and succeeded, "+
			"despite the first one failing")

	secondComments, err := s.LocalIssueComments(secondKey)
	require.NoError(t, err)
	assert.Equal(t, []string{"second landed"}, secondComments)
}

// captureStdout redirects os.Stdout for the duration of fn and returns
// everything written to it. runWatchPass prints via fmt.Print/Println
// (matching devNarrateCmd.Run's existing convention), so this is the most
// direct way to assert on what an operator watching `watch`'s output would
// actually see.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	r, w, err := os.Pipe()
	require.NoError(t, err)

	original := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = original }()

	fn()

	require.NoError(t, w.Close())

	out, err := io.ReadAll(r)
	require.NoError(t, err)

	return string(out)
}

// TestRunWatchPass_DryRunSkipsMatchReconcileAndAutoCommit proves --dry-run's
// two halves of the same guarantee: nothing is written (store or tracker),
// and every stage it skips past narrate SAYS so rather than going quiet —
// the same discipline devNarrateCmd.Run already follows for
// matching/reconcile, extended here to auto-commit.
func TestRunWatchPass_DryRunSkipsMatchReconcileAndAutoCommit(t *testing.T) {
	s := watchPassStore(t)
	tracker := local.New(s)

	issueKey, err := s.InsertLocalIssue("PROJ", "the ticket", "Task", "", nil)
	require.NoError(t, err)

	now := time.Now().UTC()
	seedReconcilableNarrative(t, s, "e1", issueKey, now.Add(-time.Hour))

	client := &watchLLM{}
	cfg := watchPassConfig()
	cfg.AutoCommit = map[string]config.AutoCommitRule{"comment": {ConfidenceFloor: 0, Graduated: true}}
	app := &appContext{config: cfg, store: s}
	applier := gate.NewApplier(s, tracker, "PROJ", watchPassWritableConnections())

	var runErr error
	out := captureStdout(t, func() {
		runErr = app.runWatchPass(t.Context(), client, tracker, applier, nil, nil, correlator.SingleTracker(tracker), 24*time.Hour, true)
	})
	require.NoError(t, runErr)

	assert.Contains(t, out, "matching skipped (--dry-run)")
	assert.Contains(t, out, "reconcile skipped (--dry-run)")
	assert.Contains(t, out, "auto-commit skipped (--dry-run)")

	// The pre-seeded narrative's action count is unchanged: reconcile never
	// ran, so it drafted nothing new.
	actions, err := s.ActionsForNarrative(1)
	require.NoError(t, err)
	assert.Empty(t, actions, "--dry-run must not persist any action")

	comments, err := s.LocalIssueComments(issueKey)
	require.NoError(t, err)
	assert.Empty(t, comments, "--dry-run must never write to the tracker")

	assert.Empty(t, client.prompts, "narrate found nothing to cluster, so it made no LLM call either")
}
