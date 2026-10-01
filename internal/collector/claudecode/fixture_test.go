package claudecode_test

// fixture_test.go is a fluent builder for transcript JSONL, modeled on the real shapes
// under ~/.claude/projects:
//
//   - a root session at <slug>/<session>.jsonl;
//   - a subagent at <slug>/<session>/subagents/agent-<agentId>.jsonl, every line of
//     which carries isSidechain: true, agentId, and the PARENT's sessionId;
//   - an optional sibling agent-<agentId>.meta.json.
//
// A builder rather than literal maps so each test states only the fields its point
// depends on (docs/go-conventions.md, "Test fixture builders").

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type transcriptBuilder struct {
	t      *testing.T
	cwd    string
	branch string
	// sidechain fields, set by AsSubagent: a subagent's lines carry the parent's
	// sessionId, which is exactly the field that must NOT become its identity.
	sessionID string
	agentID   string
	lines     []map[string]any
}

func newTranscript(t *testing.T) *transcriptBuilder {
	t.Helper()

	return &transcriptBuilder{t: t, cwd: "/w/other", branch: "feature/PROJ-1"}
}

func (b *transcriptBuilder) InCwd(cwd string) *transcriptBuilder {
	b.cwd = cwd

	return b
}

func (b *transcriptBuilder) OnBranch(branch string) *transcriptBuilder {
	b.branch = branch

	return b
}

// AsSubagent marks every subsequently added line as a sidechain of parentSession.
func (b *transcriptBuilder) AsSubagent(parentSession, agentID string) *transcriptBuilder {
	b.sessionID = parentSession
	b.agentID = agentID

	return b
}

func (b *transcriptBuilder) base(ts, kind string) map[string]any {
	l := map[string]any{"timestamp": ts, "type": kind}
	if b.cwd != "" {
		l["cwd"] = b.cwd
	}
	if b.branch != "" {
		l["gitBranch"] = b.branch
	}
	if b.agentID != "" {
		l["isSidechain"] = true
		l["agentId"] = b.agentID
		l["sessionId"] = b.sessionID
	}

	return l
}

func (b *transcriptBuilder) User(ts, text string) *transcriptBuilder {
	l := b.base(ts, "user")
	l["message"] = map[string]any{"role": "user", "content": text}
	b.lines = append(b.lines, l)

	return b
}

func (b *transcriptBuilder) ToolUse(ts, id, name string, input map[string]any) *transcriptBuilder {
	l := b.base(ts, "assistant")
	l["message"] = map[string]any{
		"role": "assistant",
		"content": []any{
			map[string]any{"type": "tool_use", "id": id, "name": name, "input": input},
		},
	}
	b.lines = append(b.lines, l)

	return b
}

func (b *transcriptBuilder) Bash(ts, id, command string) *transcriptBuilder {
	return b.ToolUse(ts, id, "Bash", map[string]any{"command": command})
}

// ToolResult appends the user-typed line Claude Code writes for a tool's output.
// content is a string (Bash) or a []any of text blocks (MCP).
func (b *transcriptBuilder) ToolResult(ts, id string, content any, isError bool) *transcriptBuilder {
	l := b.base(ts, "user")
	block := map[string]any{"type": "tool_result", "tool_use_id": id, "content": content}
	if isError {
		block["is_error"] = true
	}
	l["message"] = map[string]any{"role": "user", "content": []any{block}}
	b.lines = append(b.lines, l)

	return b
}

func (b *transcriptBuilder) bytes() []byte {
	b.t.Helper()

	var sb strings.Builder
	for _, l := range b.lines {
		raw, err := json.Marshal(l)
		require.NoError(b.t, err)
		sb.Write(raw)
		sb.WriteByte('\n')
	}

	return []byte(sb.String())
}

func (b *transcriptBuilder) writeTo(path string) string {
	b.t.Helper()

	require.NoError(b.t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(b.t, os.WriteFile(path, b.bytes(), 0o644))

	return path
}

// WriteRoot writes root/<slug>/<session>.jsonl.
func (b *transcriptBuilder) WriteRoot(root, slug, session string) string {
	return b.writeTo(filepath.Join(root, slug, session+".jsonl"))
}

// WriteSubagent writes root/<slug>/<session>/subagents/agent-<agentID>.jsonl.
func (b *transcriptBuilder) WriteSubagent(root, slug, session, agentID string) string {
	return b.writeTo(subagentPath(root, slug, session, agentID))
}

func subagentPath(root, slug, session, agentID string) string {
	return filepath.Join(root, slug, session, "subagents", "agent-"+agentID+".jsonl")
}

// writeMeta writes the sibling agent-<agentID>.meta.json for a subagent transcript.
func writeMeta(t *testing.T, transcriptPath string, meta map[string]any) {
	t.Helper()

	raw, err := json.Marshal(meta)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(strings.TrimSuffix(transcriptPath, ".jsonl")+".meta.json", raw, 0o644))
}
