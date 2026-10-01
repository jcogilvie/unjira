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
