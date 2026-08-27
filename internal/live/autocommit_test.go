//go:build live

package live

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/clients/jira"
	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/gate"
	"github.com/jcogilvie/unjira/internal/pipeline"
	"github.com/jcogilvie/unjira/internal/store"
)

// This file closes the gap slice 5's design note stated plainly: "no action has
// ever actually been applied." Every existing test of the apply path uses a
// recording fake, so what was proven was that gate.Applier calls the method it
// says it calls — not that a graduated action reaches a real tracker and
// changes it.
//
// Everything here writes to a throwaway issue this file creates and deletes,
// never to a pre-existing one. That is deliberate and load-bearing: the
// obvious cheaper test — comment on DEVSBX-1, the seeded probe issue — would
// leave residue on an issue other tests read, and "the auto-commit test
// polluted the collector fixture" is a bad way to learn that lesson. The
// project is DEVSBX via UNJIRA_LIVE_PROJECT, a sandbox owned outright.
//
// Note what these tests do NOT do: touch unjira's own config or its real
// data/unjira.db. The rules map is built here, in-process, per test. Real
// config still has no auto_commit block at all, so a fresh clone (and the
// operator's own machine) keeps the safe default — nothing this file does can
// arm the gate outside its own t.TempDir().

// liveAutoCommitStore opens a scratch store for one test, isolated in a temp
// dir so nothing here can reach the real database.
func liveAutoCommitStore(t *testing.T) *store.Store {
	t.Helper()

	s, err := store.Open(filepath.Join(t.TempDir(), "unjira.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	return s
}

// liveThrowawayIssue creates an issue in the live project and registers its
// deletion, returning the key. Labelled with jira.SeedLabel so a leaked issue
// is identifiable as test residue rather than someone's real work.
func liveThrowawayIssue(t *testing.T, client *jira.Client, purpose string) string {
	t.Helper()

	key, err := client.CreateIssue(
		testProject(),
		"[seed] auto-commit live test: "+purpose,
		"Task",
		"Created by internal/live/autocommit_test.go; deleted by this test's cleanup.",
		[]string{jira.SeedLabel},
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.DeleteIssue(key) })

	return key
}

// seedProposedComment inserts a narrative and one proposed comment action
// against issueKey, returning the persisted row.
func seedProposedComment(t *testing.T, s *store.Store, issueKey, body string, confidence float64) store.ActionRow {
	t.Helper()

	now := time.Now().UTC()
	nid, err := s.InsertNarrative(now.Add(-time.Hour), now, "live auto-commit test", "summary")
	require.NoError(t, err)

	row := store.ActionRow{
		NarrativeID: nid,
		Type:        "comment",
		IssueKey:    issueKey,
		Payload:     fmt.Sprintf(`{"body":%q}`, body),
		Confidence:  confidence,
		Rationale:   "seeded by internal/live/autocommit_test.go",
		Status:      "proposed",
	}

	id, err := s.InsertAction(row)
	require.NoError(t, err)

	got, err := s.GetAction(id)
	require.NoError(t, err)

	return got
}

// commentBodies returns every comment body on issueKey as plain text, so a
// test can assert on what a human would actually see on the issue.
func commentBodies(t *testing.T, client *jira.Client, issueKey string) []string {
	t.Helper()

	raw, err := client.GetComments(issueKey)
	require.NoError(t, err)

	out := make([]string, 0, len(raw))

	for _, c := range raw {
		// Jira Cloud returns ADF; the rendered plain text is not guaranteed to
		// be present, so walk the document for text nodes rather than assuming
		// a shape. Anything unparseable is included as its raw fmt so a
		// mismatch shows up as a visible diff instead of a silent skip.
		out = append(out, adfPlainText(c["body"]))
	}

	return out
}

// adfPlainText flattens an Atlassian Document Format value to its
// concatenated text nodes. Deliberately tolerant: this exists to let a test
// assert "the body I sent is on the issue", not to be a faithful ADF renderer.
func adfPlainText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case map[string]any:
		if text, ok := t["text"].(string); ok {
			return text
		}

		return adfPlainText(t["content"])
	case []any:
		var b string
		for _, child := range t {
			b += adfPlainText(child)
		}

		return b
	default:
		return fmt.Sprintf("%v", v)
	}
}

// TestLiveAutoCommit_UngraduatedActionIsNotWrittenToJira is the safety
// assertion, and it runs FIRST on purpose: before proving unjira can write,
// prove it does not write when it was not told to.
//
// This is the property slice 5 verified against real data at the pipeline
// level ("applied: 0 queued: 2 failed: 0", Jira timestamps unchanged). Here it
// is verified at the tracker level against an issue whose entire comment
// history is known — zero — so "nothing was written" is provable rather than
// inferred from an unchanged updated timestamp.
func TestLiveAutoCommit_UngraduatedActionIsNotWrittenToJira(t *testing.T) {
	client := testClient(t)
	s := liveAutoCommitStore(t)

	key := liveThrowawayIssue(t, client, "ungraduated must not write")
	action := seedProposedComment(t, s, key, "THIS BODY MUST NEVER APPEAR ON THE ISSUE", 0.99)

	applier := gate.NewApplier(s, jira.NewTracker(client), testProject())

	// Graduated false at maximum confidence: the confidence floor is satisfied
	// and the gate must still refuse. An empty rules map (the shipped default,
	// since config/unjira.example.json has no auto_commit block) must behave
	// identically — both are checked.
	for name, rules := range map[string]map[string]config.AutoCommitRule{
		"explicit rule, not graduated": {"comment": {ConfidenceFloor: 0.1, Graduated: false}},
		"no rules at all":              {},
		"nil rules":                    nil,
	} {
		t.Run(name, func(t *testing.T) {
			result, err := pipeline.RunAutoCommit([]store.ActionRow{action}, pipeline.AutoCommitOptions{
				Rules:   rules,
				Applier: applier,
			})
			require.NoError(t, err)
			assert.Empty(t, result.Applied, "an ungraduated action must not be applied")
			assert.Empty(t, result.Failed)
			require.Len(t, result.Queued, 1, "it must be queued for triage instead")
		})
	}

	// The real assertion: the live issue has no comments at all.
	bodies := commentBodies(t, client, key)
	assert.Empty(t, bodies, "the gate must not have written anything to %s", key)

	// And the row is untouched — still awaiting a human.
	got, err := s.GetAction(action.ID)
	require.NoError(t, err)
	assert.Equal(t, "proposed", got.Status)
	assert.Nil(t, got.ExecutedAt, "nothing was executed")
	assert.Empty(t, got.Error)
}

// TestLiveAutoCommit_GraduatedActionReachesJira is the first end-to-end proof
// that unjira can change a real tracker: graduate `comment` in an in-process
// rules map, run the gate, and read the comment back off the issue.
//
// The body carries a unique marker so the assertion cannot pass on some other
// comment that happened to be there — belt and braces, given the issue is
// created fresh by this test and starts empty.
func TestLiveAutoCommit_GraduatedActionReachesJira(t *testing.T) {
	client := testClient(t)
	s := liveAutoCommitStore(t)

	key := liveThrowawayIssue(t, client, "graduated must write")

	marker := fmt.Sprintf("unjira-live-autocommit-%d", time.Now().UnixNano())
	body := "Auto-commit live test. Marker: " + marker

	action := seedProposedComment(t, s, key, body, 0.9)

	// Confirm the pre-state rather than assuming a fresh issue is empty.
	require.Empty(t, commentBodies(t, client, key), "a freshly created issue must start with no comments")

	applier := gate.NewApplier(s, jira.NewTracker(client), testProject())

	result, err := pipeline.RunAutoCommit([]store.ActionRow{action}, pipeline.AutoCommitOptions{
		Rules: map[string]config.AutoCommitRule{
			"comment": {ConfidenceFloor: 0.5, Graduated: true},
		},
		Applier: applier,
	})
	require.NoError(t, err)
	require.Len(t, result.Applied, 1, "a graduated action above the floor must be applied")
	assert.Empty(t, result.Failed)
	assert.Empty(t, result.Queued)

	// THE assertion: the comment is on the real issue.
	bodies := commentBodies(t, client, key)
	require.Len(t, bodies, 1, "exactly one comment must have been posted, not zero and not two")
	assert.Contains(t, bodies[0], marker,
		"the comment on %s must be the one this action drafted", key)

	// And the row records the write.
	got, err := s.GetAction(action.ID)
	require.NoError(t, err)
	assert.Equal(t, "applied", got.Status)
	require.NotNil(t, got.ExecutedAt, "a write that reached the tracker sets executed_at")
	assert.Empty(t, got.Error, "a successful apply must leave no failure reason")
}

// TestLiveAutoCommit_RealFailureRecordsARealReason proves the failure-reason
// capture added in PR #21 against an actual Jira error rather than a fake's
// canned one.
//
// The failure is induced by deleting the issue before applying, so the tracker
// returns a genuine 404 for a key that is syntactically valid and was real
// moments ago. That is a closer analogue of the production failure this column
// exists for — "the issue went away between propose and apply" — than any
// error a fake could return, and it is the exact scenario Applier.Apply's doc
// comment cites as the reason not to retry.
func TestLiveAutoCommit_RealFailureRecordsARealReason(t *testing.T) {
	client := testClient(t)
	s := liveAutoCommitStore(t)

	// Not via liveThrowawayIssue: this one is deleted mid-test on purpose, and
	// registering a second delete in cleanup would just log a confusing 404.
	key, err := client.CreateIssue(
		testProject(),
		"[seed] auto-commit live test: failure reason",
		"Task",
		"Created and deliberately deleted by internal/live/autocommit_test.go.",
		[]string{jira.SeedLabel},
	)
	require.NoError(t, err)

	action := seedProposedComment(t, s, key, "this write is expected to fail", 0.9)

	require.NoError(t, client.DeleteIssue(key), "the issue must be gone before the apply")

	applier := gate.NewApplier(s, jira.NewTracker(client), testProject())

	result, err := pipeline.RunAutoCommit([]store.ActionRow{action}, pipeline.AutoCommitOptions{
		Rules: map[string]config.AutoCommitRule{
			"comment": {ConfidenceFloor: 0.5, Graduated: true},
		},
		Applier: applier,
	})

	require.Error(t, err, "a failed tracker write must surface as an error")
	assert.Empty(t, result.Applied)
	require.Len(t, result.Failed, 1)
	require.Error(t, result.Failed[0].Err, "the failed entry must carry its own reason")

	// The point of PR #21: the reason is PERSISTED, not merely returned. A
	// returned error scrolls past in watch's loop and is gone; this row is
	// what `unjira actions list --status failed` reads tomorrow.
	got, err := s.GetAction(action.ID)
	require.NoError(t, err)
	assert.Equal(t, "failed", got.Status)
	require.NotEmpty(t, got.Error, "a failed write must persist WHY it failed")
	assert.Contains(t, got.Error, key, "the reason should name the issue it could not write to")
	require.NotNil(t, got.ExecutedAt, "a failed attempt still attempted a write")

	t.Logf("persisted failure reason: %s", got.Error)
}
