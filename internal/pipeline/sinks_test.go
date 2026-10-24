package pipeline_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/attribution"
	"github.com/jcogilvie/unjira/internal/attribution/worklog"
	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/gate"
	"github.com/jcogilvie/unjira/internal/pipeline"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/store/storetest"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// recordingSink records every call and drafts nothing.
type recordingSink struct {
	name  string
	calls []attribution.TicketState
	ctxs  []attribution.DraftContext
}

func (r *recordingSink) Name() string { return r.name }

func (r *recordingSink) Draft(dc attribution.DraftContext, st attribution.TicketState) ([]attribution.Draft, error) {
	r.ctxs = append(r.ctxs, dc)
	r.calls = append(r.calls, st)

	return nil, nil
}

// sinkWriter is a TaskWriter that logs work, recording calls.
type sinkWriter struct {
	calls []string
	next  int
}

func (w *sinkWriter) AddComment(key, _ string) error {
	w.calls = append(w.calls, "AddComment:"+key)
	return nil
}

func (w *sinkWriter) SetStatus(key, _ string) error {
	w.calls = append(w.calls, "SetStatus:"+key)
	return nil
}

func (w *sinkWriter) CreateIssue(string, string, string, string, []string) (string, error) {
	return "", fmt.Errorf("not supported")
}

func (w *sinkWriter) FindWorklog(string, string, []string) (string, bool, error) {
	return "", false, nil
}

func (w *sinkWriter) AddWorklog(key string, _ tasktracker.WorklogEntry) (string, error) {
	w.next++
	w.calls = append(w.calls, "AddWorklog:"+key)

	return fmt.Sprintf("w%d", w.next), nil
}

func (w *sinkWriter) UpdateWorklog(key, id string, _ tasktracker.WorklogEntry) error {
	w.calls = append(w.calls, "UpdateWorklog:"+key+":"+id)

	return nil
}

func (w *sinkWriter) DeleteWorklog(key, id string) error {
	w.calls = append(w.calls, "DeleteWorklog:"+key+":"+id)

	return nil
}

var _ tasktracker.WorklogWriter = (*sinkWriter)(nil)

var sinkPricing = config.TokenPricing{
	"claude-opus": {Input: new(1.0), Output: new(1.0), CacheWrite: new(1.0), CacheRead: new(1.0)},
}

func sinkConfig(sinks *[]string) config.Config {
	return config.Config{
		Connections: []config.Connection{{Name: "corp", Kind: config.KindJira}},
		Trackers: []config.Tracker{
			{Name: "corp", Connection: "corp", Scopes: []string{"PROJ", "OTHER"}, WritableScopes: []string{"PROJ"}, AttributionSinks: sinks},
		},
		Match:        config.MatchConfig{ConfidenceFloor: 0.8},
		TokenPricing: sinkPricing,
	}
}

// sinkStart is before every seeded message, so the enable floor admits them all.
var sinkStart = storetest.UTC(0, 0, 0)

// seedTracked links one event per issue, each in its own narrative with that primary,
// and returns the narrative ids by issue.
func seedTracked(t *testing.T, s *store.Store, issues ...string) storetest.IDs {
	t.Helper()
	evs := make([]events.Event, 0, len(issues))
	nars := make([]storetest.NarrativeSpec, 0, len(issues))
	for _, k := range issues {
		evs = append(evs, storetest.Event("ev-"+k).Build())
		nars = append(nars, storetest.Narrative("N-"+k).Members("ev-"+k).Build())
	}
	ids := storetest.Seed(t, s, storetest.Fixture{Events: evs, Narratives: nars})
	for _, k := range issues {
		require.NoError(t, s.AddNarrativeIssues(ids["N-"+k], []store.NarrativeIssue{{
			IssueKey: k, Role: store.RolePrimary, Provenance: "branch", Confidence: 0.9, Connection: "corp",
		}}))
	}

	return ids
}

func addMessages(t *testing.T, s *store.Store, issue string, at time.Time, ids ...string) {
	t.Helper()
	rows := make([]store.MessageUsage, len(ids))
	for i, id := range ids {
		rows[i] = storetest.MessageUsage(id).Covering("ev-"+issue).At(at.Add(time.Duration(i)*time.Minute)).Tokens(1, 1, 0, 0).Build()
	}
	require.NoError(t, s.UpsertMessageUsage(rows))
}

func worklogSinks(cfg config.Config) map[string]attribution.Sink {
	return map[string]attribution.Sink{config.SinkWorklog: worklog.New(cfg.Worklog, cfg.TokenPricing)}
}

func payload(t *testing.T, a store.ActionRow) store.WorklogPayload {
	t.Helper()
	var p store.WorklogPayload
	require.NoError(t, json.Unmarshal([]byte(a.Payload), &p))

	return p
}

func withStatus(t *testing.T, s *store.Store, issue, status string) []store.ActionRow {
	t.Helper()
	all, err := s.ActionsForIssue(issue)
	require.NoError(t, err)
	var out []store.ActionRow
	for _, a := range all {
		if a.Status == status {
			out = append(out, a)
		}
	}

	return out
}

func TestRunSinks_BothSinksSeeTheSameState(t *testing.T) {
	s := openTokenStore(t)
	seedTracked(t, s, "PROJ-1")
	addMessages(t, s, "PROJ-1", storetest.UTC(12, 0, 0), "m1", "m2")

	a := &recordingSink{name: "a"}
	_, err := pipeline.RunSinks(s, sinkConfig(&[]string{config.SinkWorklog}),
		pipeline.SinkOptions{Sinks: map[string]attribution.Sink{config.SinkWorklog: a}, Now: sinkStart})
	require.NoError(t, err)
	require.Len(t, a.calls, 1)

	// Two names, bypassing Load's name validation.
	a2, b := &recordingSink{name: "a"}, &recordingSink{name: "b"}
	_, err = pipeline.RunSinks(s, sinkConfig(&[]string{"a", "b"}),
		pipeline.SinkOptions{Sinks: map[string]attribution.Sink{"a": a2, "b": b}, Now: sinkStart})
	require.NoError(t, err)
	require.Len(t, a2.calls, 1)
	require.Len(t, b.calls, 1)
	assert.True(t, cmp.Equal(a2.calls[0], b.calls[0]), cmp.Diff(a2.calls[0], b.calls[0]))
	assert.True(t, cmp.Equal(a.calls[0], b.calls[0]), cmp.Diff(a.calls[0], b.calls[0]))
	assert.Equal(t, "PROJ-1", b.calls[0].IssueKey)
	assert.Equal(t, "corp", b.ctxs[0].Tracker.Name)
}

func TestRunSinks_OnlyWritableScopesAndConfiguredTrackers(t *testing.T) {
	s := openTokenStore(t)
	seedTracked(t, s, "OTHER-1", "PROJ-1")
	addMessages(t, s, "OTHER-1", storetest.UTC(12, 0, 0), "m1")
	addMessages(t, s, "PROJ-1", storetest.UTC(12, 0, 0), "m2")

	a := &recordingSink{name: "a"}
	sinks := map[string]attribution.Sink{config.SinkWorklog: a}
	_, err := pipeline.RunSinks(s, sinkConfig(nil), pipeline.SinkOptions{Sinks: sinks, Now: sinkStart})
	require.NoError(t, err)
	require.Len(t, a.calls, 1, "the default runs on PROJ-1 only: OTHER-1 is read-only")
	assert.Equal(t, "PROJ-1", a.calls[0].IssueKey)

	a.calls = nil
	_, err = pipeline.RunSinks(s, sinkConfig(&[]string{}), pipeline.SinkOptions{Sinks: sinks, Now: sinkStart})
	require.NoError(t, err)
	assert.Empty(t, a.calls, "attribution_sinks: [] disables every sink")
}

func TestRunSinks_PersistsAndSupersedes(t *testing.T) {
	s := openTokenStore(t)
	seedTracked(t, s, "PROJ-1")
	addMessages(t, s, "PROJ-1", storetest.UTC(12, 0, 0), "m1")
	cfg := sinkConfig(nil)
	opts := pipeline.SinkOptions{Sinks: worklogSinks(cfg), Now: sinkStart}

	first, err := pipeline.RunSinks(s, cfg, opts)
	require.NoError(t, err)
	require.Len(t, first.Persisted, 1)
	assert.NotZero(t, first.Persisted[0].ID)

	addMessages(t, s, "PROJ-1", storetest.UTC(13, 0, 0), "m2")
	second, err := pipeline.RunSinks(s, cfg, opts)
	require.NoError(t, err)
	require.Len(t, second.Persisted, 1)
	assert.Equal(t, 1, second.Superseded)
	assert.NotZero(t, second.Persisted[0].ID)

	proposed := withStatus(t, s, "PROJ-1", store.StatusProposed)
	require.Len(t, proposed, 1)
	assert.Equal(t, second.Persisted[0].ID, proposed[0].ID)
	assert.Equal(t, []string{"m1", "m2"}, payload(t, proposed[0]).MessageIDs)
	suppressed := withStatus(t, s, "PROJ-1", store.StatusSuppressed)
	require.Len(t, suppressed, 1)
	assert.Equal(t, first.Persisted[0].ID, suppressed[0].ID)

	got, _, err := s.CorrectionsSince(store.CorrectionsCursor{})
	require.NoError(t, err)
	assert.Empty(t, got, "a supersede is not a human ruling (R8)")
	assert.Equal(t, "sinks: 1 drafted, 1 superseded, 0 retired\n", pipeline.RenderSinkResult(second))
}

func TestRunSinks_MarkerThenDelta(t *testing.T) {
	s := openTokenStore(t)
	seedTracked(t, s, "PROJ-1")
	addMessages(t, s, "PROJ-1", storetest.UTC(12, 0, 0), "m1")
	cfg := sinkConfig(nil)
	opts := pipeline.SinkOptions{Sinks: worklogSinks(cfg), Now: sinkStart}
	applier := gate.NewApplier(s, &sinkWriter{}, "PROJ", cfg.Trackers)

	r, err := pipeline.RunSinks(s, cfg, opts)
	require.NoError(t, err)
	require.Len(t, r.Persisted, 1)
	require.NoError(t, applier.Apply(r.Persisted[0]))

	addMessages(t, s, "PROJ-1", storetest.UTC(13, 0, 0), "m2")
	r, err = pipeline.RunSinks(s, cfg, opts)
	require.NoError(t, err)
	require.Len(t, r.Persisted, 1)
	assert.Equal(t, []string{"m2"}, payload(t, r.Persisted[0]).MessageIDs)
	assert.Zero(t, r.Superseded)
}

func TestRunSinks_CorrectionThenNewTicket(t *testing.T) {
	s := openTokenStore(t)
	ids := seedTracked(t, s, "PROJ-1")
	addMessages(t, s, "PROJ-1", storetest.UTC(12, 0, 0), "m1")
	cfg := sinkConfig(nil)
	opts := pipeline.SinkOptions{Sinks: worklogSinks(cfg), Now: sinkStart}
	w := &sinkWriter{}
	applier := gate.NewApplier(s, w, "PROJ", cfg.Trackers)

	r, err := pipeline.RunSinks(s, cfg, opts)
	require.NoError(t, err)
	require.Len(t, r.Persisted, 1)
	require.NoError(t, applier.Apply(r.Persisted[0]))

	nid := ids["N-PROJ-1"]
	require.NoError(t, s.RemoveNarrativeIssue(nid, "PROJ-1"))
	require.NoError(t, s.AddNarrativeIssues(nid, []store.NarrativeIssue{{
		IssueKey: "PROJ-2", Role: store.RolePrimary, Provenance: "branch", Confidence: 0.9, Connection: "corp",
	}}))

	r, err = pipeline.RunSinks(s, cfg, opts)
	require.NoError(t, err)
	require.Len(t, r.Persisted, 1)
	assert.Equal(t, "PROJ-1", r.Persisted[0].IssueKey)
	assert.Equal(t, store.WorklogOpDelete, payload(t, r.Persisted[0]).Op)
	assert.Empty(t, withStatus(t, s, "PROJ-2", store.StatusProposed), "m1 joins PROJ-2 only after PROJ-1's correction applies (R7)")

	require.NoError(t, applier.Apply(r.Persisted[0]))
	r, err = pipeline.RunSinks(s, cfg, opts)
	require.NoError(t, err)
	require.Len(t, r.Persisted, 1)
	assert.Equal(t, "PROJ-2", r.Persisted[0].IssueKey)
	p := payload(t, r.Persisted[0])
	assert.Equal(t, store.WorklogOpAdd, p.Op)
	assert.Equal(t, []string{"m1"}, p.MessageIDs)
}

func TestRunSinks_EnabledAtFloor(t *testing.T) {
	s := openTokenStore(t)
	seedTracked(t, s, "PROJ-1")
	addMessages(t, s, "PROJ-1", storetest.UTC(11, 0, 0), "old")
	cfg := sinkConfig(nil)
	sinks := worklogSinks(cfg)

	r, err := pipeline.RunSinks(s, cfg, pipeline.SinkOptions{Sinks: sinks, Now: storetest.UTC(12, 0, 0)})
	require.NoError(t, err)
	assert.Empty(t, r.Persisted, "a message before the first run is never backfilled")

	addMessages(t, s, "PROJ-1", storetest.UTC(13, 0, 0), "new")
	r, err = pipeline.RunSinks(s, cfg, pipeline.SinkOptions{Sinks: sinks, Now: storetest.UTC(14, 0, 0)})
	require.NoError(t, err)
	require.Len(t, r.Persisted, 1)
	assert.Equal(t, []string{"new"}, payload(t, r.Persisted[0]).MessageIDs)
}

func TestRunSinks_OneTicketsErrorDoesNotStopOthers(t *testing.T) {
	s := openTokenStore(t)
	seedTracked(t, s, "PROJ-1", "PROJ-2")
	addMessages(t, s, "PROJ-2", storetest.UTC(12, 0, 0), "m2")
	require.NoError(t, s.UpsertMessageUsage([]store.MessageUsage{
		storetest.MessageUsage("m1").Covering("ev-PROJ-1").Model("mystery").Build(),
	}))
	cfg := sinkConfig(nil)

	r, err := pipeline.RunSinks(s, cfg, pipeline.SinkOptions{Sinks: worklogSinks(cfg), Now: sinkStart})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PROJ-1")
	assert.Contains(t, err.Error(), "mystery")
	require.Len(t, r.Persisted, 1)
	assert.Equal(t, "PROJ-2", r.Persisted[0].IssueKey)
}

func TestRunSinks_TwoPendingAddsPersistsTheDraftAndReports(t *testing.T) {
	s := openTokenStore(t)
	seedTracked(t, s, "PROJ-1")
	addMessages(t, s, "PROJ-1", storetest.UTC(12, 0, 0), "m1")
	pending := make([]int64, 0, 2)
	for _, key := range []string{"k1", "k2"} {
		b, err := json.Marshal(store.WorklogPayload{Op: store.WorklogOpAdd, MessageIDs: []string{"x"}, IdempotencyKey: key})
		require.NoError(t, err)
		id, err := s.InsertAction(store.ActionRow{
			Type: store.ActionTypeWorklog, IssueKey: "PROJ-1",
			Payload: string(b), Confidence: 1, Status: store.StatusProposed,
		})
		require.NoError(t, err)
		pending = append(pending, id)
	}
	cfg := sinkConfig(nil)

	r, err := pipeline.RunSinks(s, cfg, pipeline.SinkOptions{Sinks: worklogSinks(cfg), Now: sinkStart})
	require.Error(t, err)
	assert.Contains(t, err.Error(), fmt.Sprintf("action %d", pending[0]))
	require.Len(t, r.Persisted, 1, "the superseding draft persists despite the error")
	assert.Equal(t, 1, r.Superseded)
	got, err := s.GetAction(pending[1])
	require.NoError(t, err)
	assert.Equal(t, store.StatusSuppressed, got.Status)
}

func TestRunSinks_EnableFloorIsTheFirstPassEvenWithNoTickets(t *testing.T) {
	s := openTokenStore(t)
	cfg := sinkConfig(nil)
	sinks := worklogSinks(cfg)

	r, err := pipeline.RunSinks(s, cfg, pipeline.SinkOptions{Sinks: sinks, Now: storetest.UTC(10, 0, 0)})
	require.NoError(t, err)
	assert.Empty(t, r.Persisted, "nothing is tracked yet")

	seedTracked(t, s, "PROJ-1")
	addMessages(t, s, "PROJ-1", storetest.UTC(11, 0, 0), "m1")
	r, err = pipeline.RunSinks(s, cfg, pipeline.SinkOptions{Sinks: sinks, Now: storetest.UTC(12, 0, 0)})
	require.NoError(t, err)
	require.Len(t, r.Persisted, 1, "m1 is after the first pass, though before the first pass with a ticket")
	assert.Equal(t, []string{"m1"}, payload(t, r.Persisted[0]).MessageIDs)
}

func TestRunSinks_WarnsOnAnUnwritableTicketHoldingLoggedTime(t *testing.T) {
	s := openTokenStore(t)
	seedTracked(t, s, "PROJ-1")
	addMessages(t, s, "PROJ-1", storetest.UTC(12, 0, 0), "m1")
	require.NoError(t, s.MarkWorklogged("PROJ-1", "10001", []string{"m1"}))
	cfg := sinkConfig(nil)
	cfg.Trackers[0].WritableScopes = []string{"OTHER"}
	var buf bytes.Buffer

	_, err := pipeline.RunSinks(s, cfg, pipeline.SinkOptions{
		Sinks: worklogSinks(cfg), Now: sinkStart, Log: slog.New(slog.NewTextHandler(&buf, nil)),
	})
	require.NoError(t, err)
	assert.Contains(t, buf.String(), "skipping ticket outside every writable scope that holds logged time")
	assert.Contains(t, buf.String(), "issue=PROJ-1")
}
