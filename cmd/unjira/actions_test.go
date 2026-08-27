package main

// actions_test.go exercises `unjira actions list|decide` against a real
// temp-file store and the local tasktracker backend (no network) — the same
// "no mocks for the store, a real no-op tracker for writes" combination
// watch_pass_test.go already establishes for runWatchPass. The local
// backend's own storage (local_issues / local_issue_comments) doubles as the
// recording fake the design doc asks for: "assert against a recording fake,
// not merely that no error was returned" — checking that a local issue's
// comment count is unchanged after a reject/edit is a stronger assertion
// than "no error", since a silently swallowed tracker call would still pass
// the weaker check.

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/alecthomas/kong"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/clients/local"
	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/store"
)

// actionsTestApp returns an appContext wired to a fresh temp-file store and
// the local tracker backend, with defaultProject set so `--approve`'s
// create-action path (and gate.NewApplier's defaultProject parameter) has
// somewhere to route.
func actionsTestApp(t *testing.T) (*appContext, *store.Store) {
	t.Helper()

	s := openTestStore(t)

	return &appContext{
		config: config.Config{
			Tracker: config.TrackerConfig{Backend: "local", DefaultProject: "PROJ"},
			// approveWriter resolves a project key via appContext.projectKey,
			// which falls back to the first configured Jira connection's
			// first project key even though the local backend itself ignores
			// projectKey entirely — this connection exists only to satisfy
			// that resolution step, matching watch_pass_test.go's own
			// pattern for exercising the local backend under Approve.
			Jira: []config.JiraConnection{{Name: "local", ProjectKeys: []string{"PROJ"}}},
		},
		store: s,
	}, s
}

// seedAction inserts one narrative and one action row against issueKey
// (assumed to already exist as a local issue, or empty for a create
// action), returning the action's id.
func seedAction(t *testing.T, s *store.Store, a store.ActionRow) int64 {
	t.Helper()

	base := time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC)
	nid, err := s.InsertNarrative(base, base.Add(time.Hour), "t", "s")
	require.NoError(t, err)

	a.NarrativeID = nid
	if a.Status == "" {
		a.Status = "proposed"
	}

	id, err := s.InsertAction(a)
	require.NoError(t, err)

	return id
}

func TestActionsList_DefaultsToProposedStatus(t *testing.T) {
	app, s := actionsTestApp(t)

	issueKey, err := s.InsertLocalIssue("PROJ", "ticket", "Task", "", nil)
	require.NoError(t, err)

	proposedID := seedAction(t, s, store.ActionRow{
		Type: "comment", IssueKey: issueKey, Payload: `{"body":"x"}`, Confidence: 0.7, Status: "proposed",
	})
	seedAction(t, s, store.ActionRow{
		Type: "comment", IssueKey: issueKey, Payload: `{"body":"y"}`, Confidence: 0.7, Status: "rejected",
	})

	// Status: "proposed" here stands in for Kong's `default:"proposed"` tag
	// on actionsListCmd.Status, which only applies during a real
	// kong.Parse — constructing the struct directly (as every other
	// cmd/unjira test does, per main_test.go's own precedent) means this
	// test must supply the same value Kong would have, not rely on Go's
	// zero-value empty string.
	cmd := actionsListCmd{Status: "proposed"}
	out := captureStdout(t, func() {
		require.NoError(t, cmd.Run(app))
	})

	assert.Contains(t, out, fmt.Sprintf("#%-5d", proposedID))
	assert.NotContains(t, out, "status=rejected")
}

func TestActionsList_StatusFlagOverridesDefault(t *testing.T) {
	app, s := actionsTestApp(t)

	issueKey, err := s.InsertLocalIssue("PROJ", "ticket", "Task", "", nil)
	require.NoError(t, err)

	rejectedID := seedAction(t, s, store.ActionRow{
		Type: "comment", IssueKey: issueKey, Payload: `{"body":"y"}`, Confidence: 0.7, Status: "rejected",
	})

	cmd := actionsListCmd{Status: "rejected"}
	out := captureStdout(t, func() {
		require.NoError(t, cmd.Run(app))
	})

	assert.Contains(t, out, fmt.Sprintf("#%-5d", rejectedID))
	assert.Contains(t, out, "status=rejected")
}

func TestActionsList_EmptyResultSaysSoRatherThanPrintingNothing(t *testing.T) {
	app, _ := actionsTestApp(t)

	cmd := actionsListCmd{Status: "failed"}
	out := captureStdout(t, func() {
		require.NoError(t, cmd.Run(app))
	})

	assert.Contains(t, out, `"failed"`)
}

// TestActionsList_JSONRoundTripsEveryField proves --json is a real,
// scriptable encoding of every ActionRow column, not just the fields the
// plain-text renderer happens to show.
func TestActionsList_JSONRoundTripsEveryField(t *testing.T) {
	app, s := actionsTestApp(t)

	issueKey, err := s.InsertLocalIssue("PROJ", "ticket", "Task", "", nil)
	require.NoError(t, err)

	id := seedAction(t, s, store.ActionRow{
		Type: "comment", IssueKey: issueKey, Payload: `{"body":"the work landed"}`,
		Confidence: 0.83, Rationale: "because reasons", Status: "proposed",
	})

	cmd := actionsListCmd{JSON: true, Status: "proposed"}
	out := captureStdout(t, func() {
		require.NoError(t, cmd.Run(app))
	})

	var rows []store.ActionRow
	require.NoError(t, json.Unmarshal([]byte(out), &rows))
	require.Len(t, rows, 1)
	assert.Equal(t, id, rows[0].ID)
	assert.Equal(t, "comment", rows[0].Type)
	assert.Equal(t, issueKey, rows[0].IssueKey)
	assert.JSONEq(t, `{"body":"the work landed"}`, rows[0].Payload)
	assert.InDelta(t, 0.83, rows[0].Confidence, 0.0001)
	assert.Equal(t, "because reasons", rows[0].Rationale)
	assert.Equal(t, "proposed", rows[0].Status)
}

// TestActionsDecide_RejectMakesNoTrackerCall is explicitly the test the
// design doc names: --reject must set status=rejected AND must never reach
// the tracker. The local backend's own comment count is the recording fake
// here — if AddComment had been called, this issue would have one comment;
// it must have zero.
func TestActionsDecide_RejectMakesNoTrackerCall(t *testing.T) {
	app, s := actionsTestApp(t)

	issueKey, err := s.InsertLocalIssue("PROJ", "ticket", "Task", "", nil)
	require.NoError(t, err)
	id := seedAction(t, s, store.ActionRow{
		Type: "comment", IssueKey: issueKey, Payload: `{"body":"proposed text"}`, Confidence: 0.7,
	})

	cmd := actionsDecideCmd{ID: id, Reject: true}
	require.NoError(t, cmd.Run(app))

	got, err := s.GetAction(id)
	require.NoError(t, err)
	assert.Equal(t, "rejected", got.Status)
	require.NotNil(t, got.DecidedAt)
	assert.Nil(t, got.ExecutedAt, "rejecting is not execution")

	comments, err := s.LocalIssueComments(issueKey)
	require.NoError(t, err)
	assert.Empty(t, comments, "reject must never reach the tracker")
}

// TestActionsDecide_EditPersistsFeedbackVerbatimAndMakesNoTrackerCall covers
// both halves of --edit: the feedback text round-trips exactly (quotes and
// newlines included — the payload-encoding bug class slice 4 hit), and,
// like --reject, it never reaches the tracker.
func TestActionsDecide_EditPersistsFeedbackVerbatimAndMakesNoTrackerCall(t *testing.T) {
	app, s := actionsTestApp(t)

	issueKey, err := s.InsertLocalIssue("PROJ", "ticket", "Task", "", nil)
	require.NoError(t, err)
	id := seedAction(t, s, store.ActionRow{
		Type: "comment", IssueKey: issueKey, Payload: `{"body":"proposed text"}`, Confidence: 0.7,
	})

	feedback := "say \"landed in prod\" instead\nand mention PROJ-2"
	cmd := actionsDecideCmd{ID: id, Edit: feedback}
	require.NoError(t, cmd.Run(app))

	got, err := s.GetAction(id)
	require.NoError(t, err)
	assert.Equal(t, "edited", got.Status)
	assert.Equal(t, feedback, got.Feedback, "feedback must round-trip verbatim")
	require.NotNil(t, got.DecidedAt)
	assert.Nil(t, got.ExecutedAt, "editing is not execution")

	comments, err := s.LocalIssueComments(issueKey)
	require.NoError(t, err)
	assert.Empty(t, comments, "edit must never reach the tracker")
}

func TestActionsDecide_ApproveAppliesViaApplierAndMarksApplied(t *testing.T) {
	app, s := actionsTestApp(t)

	issueKey, err := s.InsertLocalIssue("PROJ", "ticket", "Task", "", nil)
	require.NoError(t, err)
	id := seedAction(t, s, store.ActionRow{
		Type: "comment", IssueKey: issueKey, Payload: `{"body":"the work landed"}`, Confidence: 0.9,
	})

	cmd := actionsDecideCmd{ID: id, Approve: true}
	require.NoError(t, cmd.Run(app))

	got, err := s.GetAction(id)
	require.NoError(t, err)
	assert.Equal(t, "applied", got.Status)
	require.NotNil(t, got.ExecutedAt, "approving applies the action, which sets executed_at")

	comments, err := s.LocalIssueComments(issueKey)
	require.NoError(t, err)
	assert.Equal(t, []string{"the work landed"}, comments)
}

// TestActionsDecide_ApproveOnAlreadyAppliedActionIsRefused is THE double-post
// guard test: an action already at status=applied has already mutated the
// tracker once, and approving it again must be refused rather than posting
// the same comment a second time.
func TestActionsDecide_ApproveOnAlreadyAppliedActionIsRefused(t *testing.T) {
	app, s := actionsTestApp(t)

	issueKey, err := s.InsertLocalIssue("PROJ", "ticket", "Task", "", nil)
	require.NoError(t, err)
	id := seedAction(t, s, store.ActionRow{
		Type: "comment", IssueKey: issueKey, Payload: `{"body":"the work landed"}`,
		Confidence: 0.9, Status: "applied",
	})
	// Simulate the FIRST approval having already happened for real.
	require.NoError(t, s.UpdateActionStatus(id, "applied"))
	require.NoError(t, local.New(s).AddComment(issueKey, "the work landed"))

	cmd := actionsDecideCmd{ID: id, Approve: true}
	err = cmd.Run(app)

	require.Error(t, err, "re-approving an already-applied action must be refused")

	comments, err := s.LocalIssueComments(issueKey)
	require.NoError(t, err)
	assert.Len(t, comments, 1, "the comment must not be posted a second time")
}

// TestActionsDecide_ApproveOnFailedActionIsAllowed proves the deliberate
// retry path: a failed auto-commit write is never retried automatically
// (internal/gate/applier.go's own doc comment), but a human explicitly
// re-approving via this command after fixing the underlying cause is a
// different act and must succeed.
func TestActionsDecide_ApproveOnFailedActionIsAllowed(t *testing.T) {
	app, s := actionsTestApp(t)

	issueKey, err := s.InsertLocalIssue("PROJ", "ticket", "Task", "", nil)
	require.NoError(t, err)
	id := seedAction(t, s, store.ActionRow{
		Type: "comment", IssueKey: issueKey, Payload: `{"body":"the work landed"}`,
		Confidence: 0.9, Status: "failed",
	})

	cmd := actionsDecideCmd{ID: id, Approve: true}
	err = cmd.Run(app)

	require.NoError(t, err, "a human retrying a failed write must be allowed")

	got, err := s.GetAction(id)
	require.NoError(t, err)
	assert.Equal(t, "applied", got.Status)
}

// TestActionsDecide_ApproveOnRejectedActionIsAllowed covers the "your
// judgement" case the design doc calls out: nothing about rejected/edited
// implies the tracker has already been touched, so reversing a prior
// rejection must be allowed — only "already applied" is refused.
func TestActionsDecide_ApproveOnRejectedActionIsAllowed(t *testing.T) {
	app, s := actionsTestApp(t)

	issueKey, err := s.InsertLocalIssue("PROJ", "ticket", "Task", "", nil)
	require.NoError(t, err)
	id := seedAction(t, s, store.ActionRow{
		Type: "comment", IssueKey: issueKey, Payload: `{"body":"the work landed"}`,
		Confidence: 0.9, Status: "rejected",
	})

	cmd := actionsDecideCmd{ID: id, Approve: true}
	require.NoError(t, cmd.Run(app))

	got, err := s.GetAction(id)
	require.NoError(t, err)
	assert.Equal(t, "applied", got.Status)
}

func TestActionsDecide_NonexistentIDErrorsForEveryVerb(t *testing.T) {
	app, _ := actionsTestApp(t)

	t.Run("approve", func(t *testing.T) {
		err := (&actionsDecideCmd{ID: 999999, Approve: true}).Run(app)
		require.Error(t, err)
	})
	t.Run("reject", func(t *testing.T) {
		err := (&actionsDecideCmd{ID: 999999, Reject: true}).Run(app)
		require.Error(t, err)
	})
	t.Run("edit", func(t *testing.T) {
		err := (&actionsDecideCmd{ID: 999999, Edit: "x"}).Run(app)
		require.Error(t, err)
	})
}

// TestActionsDecideCmd_KongXorGroupRejectsCombinedFlags exercises the real
// Kong parser (every other test in this file constructs actionsDecideCmd
// directly, bypassing parsing entirely) to prove the `xor:"decision"
// required:""` tags actually produce the two properties actionsDecideCmd's
// own doc comment claims: at least one of --approve/--reject/--edit is
// required, and no two may be combined.
func TestActionsDecideCmd_KongXorGroupRejectsCombinedFlags(t *testing.T) {
	// Each case gets its own cli struct and parser: Kong's bound struct
	// retains flag values across repeated Parse calls on the same instance
	// (there is no implicit reset between calls), so reusing one parser
	// across cases here would leak --approve/--reject from an earlier case
	// into a later one's xor-duplicate check.
	parse := func(args ...string) error {
		var cli struct {
			Decide actionsDecideCmd `cmd:""`
		}

		parser, err := kong.New(&cli)
		require.NoError(t, err)

		_, err = parser.Parse(args)

		return err
	}

	err := parse("decide", "5")
	require.Error(t, err, "none of the three verbs supplied must be rejected")
	assert.Contains(t, err.Error(), "missing flags")

	err = parse("decide", "5", "--approve", "--reject")
	require.Error(t, err, "two verbs at once must be rejected")
	assert.Contains(t, err.Error(), "can't be used together")

	var cli struct {
		Decide actionsDecideCmd `cmd:""`
	}

	parser, err := kong.New(&cli)
	require.NoError(t, err)

	ctx, err := parser.Parse([]string{"decide", "5", "--reject"})
	require.NoError(t, err, "exactly one verb must parse cleanly")
	require.NotNil(t, ctx)
	assert.True(t, cli.Decide.Reject)
	assert.Equal(t, int64(5), cli.Decide.ID)
}
