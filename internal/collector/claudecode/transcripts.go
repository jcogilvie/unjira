package claudecode

// transcripts.go finds every transcript under the root: root sessions AND the subagent
// transcripts the collector used to miss.
//
// The glob was `<root>/*/*.jsonl`, one directory too shallow. Subagent transcripts live
// at `<slug>/<session>/subagents/agent-<agentId>.jsonl` and were never read — measured
// across ~/.claude/projects, 991 of them against 187 root sessions. In subagent-driven
// development that is where most implementation happens, so the loss was the bulk of
// the work and nothing reported it.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Collector-private artifact keys describing a subagent's place in its conversation.
// None is read outside this package (docs/go-conventions.md keeps only cross-package
// keys in events/artifact_keys.go). Their intended reader is the later step that
// attaches a root session's shared context to the narratives its subagents produced.
const (
	// artifactSessionID is the Claude Code conversation a transcript belongs to: a
	// root session's own id, or for a subagent its parent's. That is Claude Code's
	// own meaning — every line of a subagent transcript carries the parent's
	// sessionId — and it is the key that groups a root with its subagents. For root
	// sessions it is the same value it always was.
	artifactSessionID = "session_id"
	// artifactParentSessionID is set only on subagent transcripts. It equals
	// artifactSessionID by construction; its presence is what marks a child.
	artifactParentSessionID = "parent_session_id"
	// artifactAgentID is the subagent's own id, the filename's `agent-<agentId>`.
	artifactAgentID = "agent_id"
	// artifactParentAgentID is the dispatching subagent, for spawnDepth >= 2.
	artifactParentAgentID = "parent_agent_id"
	// artifactDispatchToolUseID is the parent's Agent tool call that spawned this
	// subagent (meta `toolUseId`) — the link back into the parent transcript.
	artifactDispatchToolUseID = "dispatch_tool_use_id"
	// artifactSubagentDescription is the dispatcher's short label for the task.
	artifactSubagentDescription = "subagent_description"
	// artifactSpawnDepth is the meta's spawnDepth: 1 for a subagent of a root session.
	artifactSpawnDepth = "spawn_depth"
	// artifactWorktreeBranch is the branch the subagent's worktree was CREATED on,
	// from the meta. Every value measured is tool-generated (`worktree-agent-<id>`)
	// and the user renames such branches afterwards, so it names neither the work
	// nor necessarily any branch that exists today. That is why it is not
	// events.ArtifactGitBranch: that key feeds the provenance ladder's top tier,
	// which means "a human named the work this".
	artifactWorktreeBranch = "worktree_branch"
	// artifactSubagentMeta records whether the sibling .meta.json was present,
	// absent (175 of 991 measured, older transcripts predate it) or malformed. The
	// transcript is collected in every case; only the meta-derived artifacts go.
	artifactSubagentMeta = "subagent_meta"
	// artifactGitBranchOmitted carries the reason events.ArtifactGitBranch is not
	// set, so an absent branch reads as a decision rather than as missing data.
	artifactGitBranchOmitted = "git_branch_omitted"
)

const (
	metaPresent     = "present"
	metaAbsent      = "absent"
	metaMalformed   = "malformed"
	subagentsDir    = "subagents"
	agentFilePrefix = "agent-"

	// subagentBranchOmittedReason is recorded on every subagent event. Measured: of
	// 31 metas naming a worktreeBranch, 27 never show it in gitBranch, which instead
	// holds the parent's branch (`pr-27998`, `main`) — the branch of the process, not
	// of the worktree the subagent worked in.
	subagentBranchOmittedReason = "subagent transcript: gitBranch records the parent session's " +
		"branch, not the subagent's own checkout"
)

// transcript is one JSONL file and its place in a conversation.
type transcript struct {
	path string
	// id prefixes the transcript's segment ExternalIDs: the root session's
	// filename stem, or `agent-<agentId>` for a subagent. Never the JSONL
	// sessionId field, which a subagent shares with its parent — keying on it would
	// collide the two and INSERT OR IGNORE would drop one silently and for good.
	id string
	// sessionID is the conversation the transcript belongs to; see artifactSessionID.
	sessionID string
	// slug is the project directory under the root, the fallback project name.
	slug string
	// sub is nil for a root session.
	sub *subagent
}

// subagent is what is known about a subagent transcript beyond its lines.
type subagent struct {
	agentID    string
	metaStatus string
	meta       subagentMeta
}

// subagentMeta is the sibling agent-<agentId>.meta.json. Pointer-free strings read as
// "absent" when empty; SpawnDepth is a pointer because 317 of 815 measured metas omit
// it and zero is not a depth anybody recorded.
type subagentMeta struct {
	AgentType      string `json:"agentType"`
	Description    string `json:"description"`
	ToolUseID      string `json:"toolUseId"`
	SpawnDepth     *int   `json:"spawnDepth"`
	ParentAgentID  string `json:"parentAgentId"`
	WorktreeBranch string `json:"worktreeBranch"`
}

// discoverTranscripts returns every root session and subagent transcript under root,
// sorted by path.
//
// Subagent directories are walked recursively, so a nested layout is collected rather
// than skipped; today every one measured is flat, with depth 2 and 3 subagents filed
// beside depth 1 in the root session's subagents/. The walk is confined to
// subagents/ because a session directory also holds tool-results/*.jsonl, which is
// spilled tool output, not a transcript.
func discoverTranscripts(root string) ([]transcript, error) {
	roots, err := filepath.Glob(filepath.Join(root, "*", "*.jsonl"))
	if err != nil {
		return nil, fmt.Errorf("globbing root transcripts under %s: %w", root, err)
	}

	out := make([]transcript, 0, len(roots))
	for _, path := range roots {
		out = append(out, transcript{
			path:      path,
			id:        strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)),
			sessionID: strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)),
			slug:      filepath.Base(filepath.Dir(path)),
		})
	}

	dirs, err := filepath.Glob(filepath.Join(root, "*", "*", subagentsDir))
	if err != nil {
		return nil, fmt.Errorf("globbing subagent directories under %s: %w", root, err)
	}

	for _, dir := range dirs {
		sessionDir := filepath.Dir(dir)
		walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || filepath.Ext(path) != ".jsonl" {
				return nil
			}

			out = append(out, subagentTranscript(path, sessionDir))

			return nil
		})
		if walkErr != nil {
			return nil, fmt.Errorf("walking subagent transcripts under %s: %w", dir, walkErr)
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })

	return out, nil
}

// subagentTranscript describes the subagent transcript at path, filed under the root
// session directory sessionDir.
//
// The agent id comes from the filename, which equals every line's agentId field in
// all 991 files measured, and is readable without parsing the transcript.
func subagentTranscript(path, sessionDir string) transcript {
	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	agentID := strings.TrimPrefix(stem, agentFilePrefix)

	sub := &subagent{agentID: agentID}
	sub.meta, sub.metaStatus = readSubagentMeta(strings.TrimSuffix(path, ".jsonl") + ".meta.json")

	return transcript{
		path:      path,
		id:        agentFilePrefix + agentID,
		sessionID: filepath.Base(sessionDir),
		slug:      filepath.Base(filepath.Dir(sessionDir)),
		sub:       sub,
	}
}

// readSubagentMeta reads a subagent's meta file, reporting whether it was present,
// absent, or unusable. It never errors: a missing or broken meta costs its own fields,
// not the transcript.
func readSubagentMeta(path string) (subagentMeta, string) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return subagentMeta{}, metaAbsent
	}
	if err != nil {
		return subagentMeta{}, metaMalformed
	}

	var meta subagentMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return subagentMeta{}, metaMalformed
	}

	return meta, metaPresent
}

// setTranscriptArtifacts records which transcript an event came from and, for a
// subagent, its relationship to the conversation. Every meta-derived field is omitted
// when the meta did not supply it.
func setTranscriptArtifacts(artifacts map[string]any, t transcript) {
	artifacts[artifactSessionID] = t.sessionID
	if t.sub == nil {
		return
	}

	artifacts[artifactParentSessionID] = t.sessionID
	artifacts[artifactAgentID] = t.sub.agentID
	artifacts[artifactSubagentMeta] = t.sub.metaStatus

	meta := t.sub.meta
	setIfNonEmpty(artifacts, artifactDispatchToolUseID, meta.ToolUseID)
	setIfNonEmpty(artifacts, artifactSubagentDescription, meta.Description)
	setIfNonEmpty(artifacts, artifactParentAgentID, meta.ParentAgentID)
	setIfNonEmpty(artifacts, artifactWorktreeBranch, meta.WorktreeBranch)
	if meta.SpawnDepth != nil {
		artifacts[artifactSpawnDepth] = *meta.SpawnDepth
	}
}

func setIfNonEmpty(artifacts map[string]any, key, value string) {
	if value != "" {
		artifacts[key] = value
	}
}
