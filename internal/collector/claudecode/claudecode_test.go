package claudecode_test

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/collector/claudecode"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/store"
)

// writeSession creates a minimal one-user-message transcript under
// root/<projectSlug>/<sessionID>.jsonl, mirroring the fixture in the
// original Python test.
func writeSession(t *testing.T, root, projectSlug, sessionID, cwd string) string {
	t.Helper()

	dir := filepath.Join(root, projectSlug)
	require.NoError(t, os.MkdirAll(dir, 0o755))

	line, err := json.Marshal(map[string]any{
		"type":      "user",
		"cwd":       cwd,
		"gitBranch": "main",
		"timestamp": "2026-07-16T12:00:00Z",
		"message":   map[string]any{"content": "Help me with PROJ-123 please"},
	})
	require.NoError(t, err)

	path := filepath.Join(dir, sessionID+".jsonl")
	require.NoError(t, os.WriteFile(path, line, 0o644))

	return path
}

func collect(t *testing.T, root string, options map[string]any) (*store.Store, []events.Event) {
	t.Helper()

	s, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	opts := map[string]any{"transcript_root": root}
	maps.Copy(opts, options)

	var out []events.Event
	err = claudecode.New().Collect(s, opts, func(e events.Event) { out = append(out, e) })
	require.NoError(t, err)

	return s, out
}

func TestCollect_SessionYieldsEventWithoutExclusion(t *testing.T) {
	root := t.TempDir()
	writeSession(t, root, "proj-a", "s1", "/Users/j/workspace/other")

	_, got := collect(t, root, nil)

	require.Len(t, got, 1)
	assert.Contains(t, got[0].Artifacts["ticket_keys"], "PROJ-123")
}

func TestCollect_OwnRepoSessionExcluded(t *testing.T) {
	root := t.TempDir()
	repo := "/Users/j/workspace/unjira"
	writeSession(t, root, "proj-unjira", "s1", repo)

	_, got := collect(t, root, map[string]any{"exclude_cwds": []string{repo}})

	assert.Empty(t, got)
}

func TestCollect_NestedCwdUnderExcludedRepoIsExcluded(t *testing.T) {
	root := t.TempDir()
	repo := "/Users/j/workspace/unjira"
	writeSession(t, root, "proj-unjira", "s1", repo+"/src/unjira")

	_, got := collect(t, root, map[string]any{"exclude_cwds": []string{repo}})

	assert.Empty(t, got)
}

func TestCollect_SiblingSharingPrefixIsNotExcluded(t *testing.T) {
	root := t.TempDir()
	repo := "/Users/j/workspace/unjira"
	writeSession(t, root, "proj-docs", "s1", repo+"-docs")

	_, got := collect(t, root, map[string]any{"exclude_cwds": []string{repo}})

	assert.Len(t, got, 1)
}

func TestCollect_ExcludedSessionStillAdvancesCursor(t *testing.T) {
	root := t.TempDir()
	repo := "/Users/j/workspace/unjira"
	path := writeSession(t, root, "proj-unjira", "s1", repo)

	s, got := collect(t, root, map[string]any{"exclude_cwds": []string{repo}})

	assert.Empty(t, got)
	cursor, err := s.GetCursor("claude_code", path)
	require.NoError(t, err)
	assert.NotEmpty(t, cursor)
}
