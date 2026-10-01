package correlator_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ghclient "github.com/jcogilvie/unjira/internal/clients/github"
	"github.com/jcogilvie/unjira/internal/collector/claudecode"
	collectorgithub "github.com/jcogilvie/unjira/internal/collector/github"
	collectorjira "github.com/jcogilvie/unjira/internal/collector/jira"
	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/pipeline"
	"github.com/jcogilvie/unjira/internal/store"
)

// TestArtifactKeyContract_JiraCollectorToCorrelator_ConnectionAndIssueKey is
// the end-to-end guard task #182/F2 exists to make possible: it drives the
// REAL jira collector emitter, not a hand-built fixture map, into the REAL
// correlator gatherer.
//
// A per-package unit test on each side can pass while the two sides disagree
// about the artifact's string key — that is exactly F2's failure mode
// (docs/design-notes.md incident 21): two packages independently spelling the
// same literal, one typo or one refactor away from silently no longer
// agreeing. Declaring events.ArtifactConnection/ArtifactIssueKey as shared
// constants only closes that gap if both sides actually import them; this
// test would fail if either the collector reverted to writing a literal or
// the correlator reverted to reading one, even though each package's own
// tests construct events by hand and would stay green.
func TestArtifactKeyContract_JiraCollectorToCorrelator_ConnectionAndIssueKey(t *testing.T) {
	ic := collectorjira.IssueContext{
		Key: "PROJ-42", ProjectKey: "PROJ", Connection: "corp", Site: "https://corp.atlassian.net",
	}
	entry := map[string]any{
		"id":      "10001",
		"created": "2026-08-20T14:30:00.000+0000",
		"author":  map[string]any{"accountId": "acct-alice", "displayName": "Alice"},
		"items": []any{
			map[string]any{"field": "status", "fromString": "To Do", "toString": "In Progress"},
		},
	}

	evts, err := collectorjira.EventsFromChangelogEntry(ic, entry)
	require.NoError(t, err)
	require.Len(t, evts, 1)

	got := correlator.GatherCandidatesForTest(evts, nil, 10, nil)

	require.Len(t, got, 1)
	assert.Equal(t, "PROJ-42", got[0].IssueKey)
	assert.Equal(t, correlator.ProvenanceJiraEvent, got[0].Provenance)
	assert.Equal(t, "corp", got[0].Connection,
		"the correlator must resolve the same connection the collector recorded, "+
			"or verifyCandidates would check the wrong tracker (or none)")
}

// TestArtifactKeyContract_ClaudeCodeCollectorToCorrelator_BranchAndTicketKeys
// is the same end-to-end guard for the claude_code collector's two artifacts,
// both of which the correlator reads with a different access pattern
// (events.ArtifactGitBranch via a bare type assertion, events.ArtifactTicketKeys
// via the events.TicketKeysOf accessor because its wire type is []any, not
// []string).
//
// Drives the REAL collector — writes a transcript file and runs
// claudecode.Collector.Collect over it — rather than a hand-built events.Event.
// A hand-built event with the artifacts set inline would pass even if the
// collector's own writer diverged from the correlator's reader, because a
// hand-built fixture is a third spelling that agrees with neither; the drill
// for this test (breaking the collector's write to events.ArtifactGitBranch)
// caught exactly that gap in an earlier draft of this file.
func TestArtifactKeyContract_ClaudeCodeCollectorToCorrelator_BranchAndTicketKeys(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "proj-a")
	require.NoError(t, os.MkdirAll(dir, 0o755))

	line, err := json.Marshal(map[string]any{
		"type":      "user",
		"gitBranch": "feature/PROJ-42",
		"timestamp": "2026-08-24T10:00:00Z",
		"message":   map[string]any{"content": "Also touches PROJ-100 and PROJ-205"},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "s1.jsonl"), line, 0o644))

	s, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	var evts []events.Event
	require.NoError(t, claudecode.New().Collect(
		pipeline.CollectContext{Store: s, Options: map[string]any{"transcript_root": root}},
		func(e events.Event) { evts = append(evts, e) },
	))
	require.Len(t, evts, 1)

	got := correlator.GatherCandidatesForTest(evts, nil, 10, nil)

	require.NotEmpty(t, got)
	assert.Equal(t, "PROJ-42", got[0].IssueKey,
		"the branch candidate, re-derived from events.ArtifactGitBranch, must outrank prose")
	assert.Equal(t, correlator.ProvenanceBranch, got[0].Provenance)

	keys := make([]string, 0, len(got))
	for _, c := range got {
		keys = append(keys, c.IssueKey)
	}
	assert.Contains(t, keys, "PROJ-100", "prose candidates read via events.TicketKeysOf must still surface")
	assert.Contains(t, keys, "PROJ-205")
}

// TestArtifactKeyContract_GitHubCollectorToCorrelator_BranchAndSCMKeys is the
// same end-to-end guard for the github collector, driving its REAL
// OpenedEvent constructor (not a hand-built events.Event) into the REAL
// correlator gatherer — mirroring
// TestArtifactKeyContract_ClaudeCodeCollectorToCorrelator_BranchAndTicketKeys.
//
// This collector reuses claudecode's ArtifactGitBranch key verbatim (§2 of
// the design), so the branch candidate must rank as ProvenanceBranch exactly
// as it does for a claude_code-sourced event, with zero gatherCandidates
// changes. The title/body keys land on ArtifactSCMKeys (ProvenanceSCMCommand),
// not ArtifactTicketKeys — a different tier than claudecode's own prose keys,
// which is the collector's own deliberate §5 decision.
func TestArtifactKeyContract_GitHubCollectorToCorrelator_BranchAndSCMKeys(t *testing.T) {
	ref, err := ghclient.ParseRepoRef("o/r")
	require.NoError(t, err)

	pull := ghclient.PullRequest{
		Number: 7, Title: "Fix PROJ-42 crash", Body: "Also touches PROJ-100.",
		CreatedAt: time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC),
	}
	pull.Head.Ref = "feature/PROJ-42"

	evt := collectorgithub.OpenedEvent(ref, pull)

	got := correlator.GatherCandidatesForTest([]correlator.Event{evt}, nil, 10, nil)

	keys := make([]string, 0, len(got))
	byKey := make(map[string]correlator.Candidate, len(got))
	for _, c := range got {
		keys = append(keys, c.IssueKey)
		byKey[c.IssueKey] = c
	}

	require.Contains(t, keys, "PROJ-42")
	assert.Equal(t, correlator.ProvenanceBranch, byKey["PROJ-42"].Provenance,
		"the branch candidate, re-derived from events.ArtifactGitBranch, must outrank the SCM-command tier")

	require.Contains(t, keys, "PROJ-100")
	assert.Equal(t, correlator.ProvenanceSCMCommand, byKey["PROJ-100"].Provenance,
		"a title/body key read via events.SCMKeysOf must land on ProvenanceSCMCommand, not a prose tier")
}

// TestArtifactKeyContract_PRAnchorAndGitHubOpenedAgreeOnArtifactPullRequest guards the
// one artifact with two writers: a claude_code anchor for a `gh pr create` call and the
// github collector's :opened event for the PR it created must carry the SAME
// events.ArtifactPullRequest, or the join it exists for would silently match nothing.
//
// Both sides are the REAL producers — the claudecode collector reading a transcript
// file, and collectorgithub.OpenedEvent — for the reason the claude_code contract test
// above gives: a hand-built event is a third spelling that agrees with neither.
//
// It also pins that the anchor adds no matching candidates: anchors are new
// clusterable evidence, not a new provenance input, and this task changes no matching.
func TestArtifactKeyContract_PRAnchorAndGitHubOpenedAgreeOnArtifactPullRequest(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "proj-a")
	require.NoError(t, os.MkdirAll(dir, 0o755))

	var raw []byte
	for _, line := range []map[string]any{
		{
			"type": "user", "timestamp": "2026-09-20T10:00:00Z", "cwd": "/w/r",
			"message": map[string]any{"content": "open the PR for PROJ-42"},
		},
		{
			"type": "assistant", "timestamp": "2026-09-20T10:01:00Z", "cwd": "/w/r",
			"message": map[string]any{"content": []any{map[string]any{
				"type": "tool_use", "id": "toolu_01Contract", "name": "Bash",
				"input": map[string]any{"command": `gh pr create --title "Fix PROJ-42 crash"`},
			}}},
		},
		{
			"type": "user", "timestamp": "2026-09-20T10:01:05Z", "cwd": "/w/r",
			"message": map[string]any{"content": []any{map[string]any{
				"type": "tool_result", "tool_use_id": "toolu_01Contract",
				"content": "https://github.com/o/r/pull/7\n",
			}}},
		},
	} {
		b, err := json.Marshal(line)
		require.NoError(t, err)
		raw = append(append(raw, b...), '\n')
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "s1.jsonl"), raw, 0o644))

	s, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	var anchor *events.Event
	require.NoError(t, claudecode.New().Collect(
		pipeline.CollectContext{Store: s, Options: map[string]any{"transcript_root": root}},
		func(e events.Event) {
			if e.Artifacts["anchor_kind"] == "pr_create" {
				anchor = &e
			}
		},
	))
	require.NotNil(t, anchor, "the collector must emit an anchor for the gh pr create call")

	ref, err := ghclient.ParseRepoRef("o/r")
	require.NoError(t, err)
	opened := collectorgithub.OpenedEvent(ref, ghclient.PullRequest{
		Number: 7, Title: "Fix PROJ-42 crash", CreatedAt: time.Date(2026, 9, 20, 10, 1, 4, 0, time.UTC),
	})

	require.NotEmpty(t, opened.Artifacts[events.ArtifactPullRequest])
	assert.Equal(t, opened.Artifacts[events.ArtifactPullRequest], anchor.Artifacts[events.ArtifactPullRequest],
		"both sides of the same PR must spell its identifier identically")

	assert.Empty(t, correlator.GatherCandidatesForTest([]correlator.Event{*anchor}, nil, 10, nil),
		"an anchor must not change matching's inputs")
}
