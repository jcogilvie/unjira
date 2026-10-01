package claudecode_test

// subagent_test.go covers subagent transcripts, which the collector never read.
//
// The glob was `<root>/*/*.jsonl`. Subagent transcripts live one level deeper, at
// `<slug>/<session>/subagents/agent-<agentId>.jsonl`, and in subagent-driven
// development that is where most implementation happens: measured across
// ~/.claude/projects, 991 subagent transcripts against 187 root sessions, all of them
// invisible.
//
// Shapes these fixtures are modeled on, all measured on real files:
//   - every line carries isSidechain: true, agentId, and the PARENT's sessionId
//     (985 of 991 files; the other 6 are resume copies carrying two);
//   - gitBranch is the PARENT session's branch, not the subagent worktree's: of 31
//     metas naming a worktreeBranch, 27 never show it in gitBranch;
//   - the sibling .meta.json is absent for 175 of 991, and spawnDepth reaches 3;
//   - <session>/tool-results/*.jsonl exists alongside subagents/ and is NOT a
//     transcript.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/events"
)

const (
	parentSession = "c68b4c96-f3be-42c3-b1d2-b37bee2fa1c9"
	slug          = "-w-unjira"
)

// segmentEvents drops PR-creation anchors, leaving one event per transcript segment.
func segmentEvents(evts []events.Event) []events.Event {
	var out []events.Event
	for _, e := range evts {
		if _, isAnchor := e.Artifacts["anchor_kind"]; !isAnchor {
			out = append(out, e)
		}
	}

	return out
}

func eventsWithArtifact(evts []events.Event, key string, want any) []events.Event {
	var out []events.Event
	for _, e := range evts {
		if e.Artifacts[key] == want {
			out = append(out, e)
		}
	}

	return out
}

func subagentTranscript(t *testing.T, agentID string) *transcriptBuilder {
	t.Helper()

	return newTranscript(t).AsSubagent(parentSession, agentID).OnBranch("main").
		User("2026-09-20T10:00:00Z", "Implement the F16 bound for PROJ-77")
}

func TestCollect_SubagentTranscriptIsCollected(t *testing.T) {
	root := t.TempDir()
	newTranscript(t).User("2026-09-20T09:00:00Z", "dispatch the work").WriteRoot(root, slug, parentSession)
	path := subagentTranscript(t, "a2fc2fb353c169993").WriteSubagent(root, slug, parentSession, "a2fc2fb353c169993")
	writeMeta(t, path, map[string]any{
		"agentType":   "general-purpose",
		"description": "Bound hydrated context narratives",
		"toolUseId":   "toolu_01Dispatch",
		"spawnDepth":  1,
	})

	_, got := collect(t, root, nil)

	subs := eventsWithArtifact(segmentEvents(got), "agent_id", "a2fc2fb353c169993")
	require.Len(t, subs, 1, "the subagent transcript must yield its own segment event")
	sub := subs[0]
	assert.Equal(t, parentSession, sub.Artifacts["session_id"],
		"session_id is the conversation the transcript belongs to: the parent's, for a subagent")
	assert.Equal(t, parentSession, sub.Artifacts["parent_session_id"])
	assert.Equal(t, "toolu_01Dispatch", sub.Artifacts["dispatch_tool_use_id"])
	assert.Equal(t, "Bound hydrated context narratives", sub.Artifacts["subagent_description"])
	assert.Equal(t, 1, sub.Artifacts["spawn_depth"])
	assert.Equal(t, "present", sub.Artifacts["subagent_meta"])
	assert.Equal(t, path, sub.RawRef)
	assert.Contains(t, sub.Summary, "subagent",
		"the opening line is the dispatching agent's prompt, so the summary must not present it as the human's ask")
	assert.Contains(t, events.TicketKeysOf(sub), "PROJ-77")
}

// TestCollect_ParentAndSubagentNeverCollideOnExternalID is the trap: a subagent's lines
// carry the PARENT's sessionId, so an identity read from that field would give both
// transcripts the same prefix — and with equal size and segment index, INSERT OR
// IGNORE would drop the second silently and permanently (F21). The two files here are
// byte-identical, so size and index coincide by construction.
func TestCollect_ParentAndSubagentNeverCollideOnExternalID(t *testing.T) {
	root := t.TempDir()
	b := subagentTranscript(t, "a2fc2fb353c169993")
	b.WriteRoot(root, slug, parentSession)
	b.WriteSubagent(root, slug, parentSession, "a2fc2fb353c169993")

	s, got := collect(t, root, nil)
	segs := segmentEvents(got)
	require.Len(t, segs, 2)
	assert.NotEqual(t, segs[0].ExternalID, segs[1].ExternalID)

	for _, e := range segs {
		inserted, err := s.InsertEvent(e)
		require.NoError(t, err)
		assert.True(t, inserted, "%s must insert, not be ignored as a duplicate", e.ExternalID)
	}
}

func TestCollect_SubagentWithoutMetaIsStillCollected(t *testing.T) {
	for _, tc := range []struct {
		name, meta, want string
	}{
		{"absent", "", "absent"},
		{"malformed", `{"agentType": `, "malformed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := subagentTranscript(t, "a1").WriteSubagent(root, slug, parentSession, "a1")
			if tc.meta != "" {
				require.NoError(t, os.WriteFile(strings.TrimSuffix(path, ".jsonl")+".meta.json", []byte(tc.meta), 0o644))
			}

			_, got := collect(t, root, nil)

			subs := segmentEvents(got)
			require.Len(t, subs, 1, "a missing or unreadable meta must not drop the transcript")
			assert.Equal(t, tc.want, subs[0].Artifacts["subagent_meta"])
			assert.Equal(t, "a1", subs[0].Artifacts["agent_id"])
			for _, key := range []string{"dispatch_tool_use_id", "subagent_description", "spawn_depth", "worktree_branch"} {
				assert.NotContains(t, subs[0].Artifacts, key, "a meta field must be omitted, never guessed")
			}
		})
	}
}

// TestCollect_SubagentGitBranchIsNotEmitted pins that the parent's branch never reaches
// the provenance ladder's top tier through a subagent. Measured: subagent gitBranch is
// the parent's (`main`, `pr-27998`) even when the subagent ran in its own worktree.
func TestCollect_SubagentGitBranchIsNotEmitted(t *testing.T) {
	root := t.TempDir()
	path := newTranscript(t).AsSubagent(parentSession, "a1").OnBranch("main").
		User("2026-09-20T10:00:00Z", "one").
		User("2026-09-20T10:01:00Z", "two").
		User("2026-09-20T10:02:00Z", "three").
		OnBranch("PROJ-9-parent-moved").
		User("2026-09-20T10:03:00Z", "four").
		User("2026-09-20T10:04:00Z", "five").
		User("2026-09-20T10:05:00Z", "six").
		WriteSubagent(root, slug, parentSession, "a1")
	writeMeta(t, path, map[string]any{"worktreeBranch": "worktree-agent-a1", "spawnDepth": 1})

	_, got := collect(t, root, nil)

	subs := segmentEvents(got)
	require.Len(t, subs, 1,
		"the parent's branch changing is not a boundary in the subagent's work, so it must not split it")
	assert.NotContains(t, subs[0].Artifacts, events.ArtifactGitBranch)
	assert.NotEmpty(t, subs[0].Artifacts["git_branch_omitted"], "the omission must carry its reason")
	assert.NotContains(t, subs[0].Summary, "on branch")
	assert.Equal(t, "worktree-agent-a1", subs[0].Artifacts["worktree_branch"])
}

// TestCollect_RootGitBranchIsStillEmitted guards the other side: the subagent rule must
// not leak onto root sessions, whose gitBranch is the strongest signal unjira has.
func TestCollect_RootGitBranchIsStillEmitted(t *testing.T) {
	root := t.TempDir()
	newTranscript(t).OnBranch("feature/PROJ-42").User("2026-09-20T10:00:00Z", "go").WriteRoot(root, slug, parentSession)

	_, got := collect(t, root, nil)

	require.Len(t, got, 1)
	assert.Equal(t, "feature/PROJ-42", got[0].Artifacts[events.ArtifactGitBranch])
	assert.NotContains(t, got[0].Artifacts, "agent_id")
}

// TestCollect_ExcludeCwdsCoversSubagentWorktree pins that a subagent is excluded by its
// OWN cwd, the same prefix rule root sessions get: a worktree under an excluded repo
// (`<repo>/.claude/worktrees/agent-x`, the measured shape) is excluded with it.
func TestCollect_ExcludeCwdsCoversSubagentWorktree(t *testing.T) {
	root := t.TempDir()
	repo := "/Users/j/workspace/unjira"
	newTranscript(t).AsSubagent(parentSession, "a1").InCwd(repo+"/.claude/worktrees/agent-a1").
		User("2026-09-20T10:00:00Z", "work").
		WriteSubagent(root, slug, parentSession, "a1")
	newTranscript(t).AsSubagent(parentSession, "a2").InCwd("/Users/j/workspace/other").
		User("2026-09-20T10:00:00Z", "work").
		WriteSubagent(root, slug, parentSession, "a2")

	_, got := collect(t, root, map[string]any{"exclude_cwds": []string{repo}})

	require.Len(t, got, 1)
	assert.Equal(t, "a2", got[0].Artifacts["agent_id"])
}

// TestCollect_NestedSubagentsAreCollected covers spawnDepth > 1, which is real (26
// metas at depth 2, 5 at depth 3). Today they sit flat in the root session's
// subagents/ directory; a nested layout is collected too, so no depth is hardcoded.
func TestCollect_NestedSubagentsAreCollected(t *testing.T) {
	root := t.TempDir()
	path := subagentTranscript(t, "a2").WriteSubagent(root, slug, parentSession, "a2")
	writeMeta(t, path, map[string]any{"spawnDepth": 2, "parentAgentId": "a1", "toolUseId": "toolu_02"})
	subagentTranscript(t, "a3").writeTo(
		filepath.Join(root, slug, parentSession, "subagents", "nested", "agent-a3.jsonl"))

	_, got := collect(t, root, nil)

	require.Len(t, got, 2)
	byAgent := map[any]events.Event{}
	for _, e := range got {
		byAgent[e.Artifacts["agent_id"]] = e
	}
	assert.Equal(t, 2, byAgent["a2"].Artifacts["spawn_depth"])
	assert.Equal(t, "a1", byAgent["a2"].Artifacts["parent_agent_id"])
	assert.Equal(t, parentSession, byAgent["a3"].Artifacts["session_id"])
}

// TestCollect_ToolResultsJSONLIsNotATranscript pins the measured sibling directory: a
// recursive walk of the whole session directory would ingest these as sessions.
func TestCollect_ToolResultsJSONLIsNotATranscript(t *testing.T) {
	root := t.TempDir()
	newTranscript(t).User("2026-09-20T10:00:00Z", "spilled output").
		writeTo(filepath.Join(root, slug, parentSession, "tool-results", "prose.jsonl"))

	_, got := collect(t, root, nil)

	assert.Empty(t, got)
}

// TestCollect_SubagentAdvancesItsOwnCursor keeps the cursor per file: an unchanged
// subagent transcript is not re-read.
func TestCollect_SubagentAdvancesItsOwnCursor(t *testing.T) {
	root := t.TempDir()
	path := subagentTranscript(t, "a1").WriteSubagent(root, slug, parentSession, "a1")

	s, got := collect(t, root, nil)
	require.Len(t, got, 1)

	cursor, err := s.GetCursor("claude_code", path)
	require.NoError(t, err)
	assert.NotEmpty(t, cursor)
}
