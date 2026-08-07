package pipeline_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/pipeline"
	"github.com/jcogilvie/unjira/internal/store"
)

func TestRenderDigest_NoEventsReportsNone(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	out, err := pipeline.RenderDigest(s, time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC))

	require.NoError(t, err)
	assert.Contains(t, out, "No events observed.")
}

func TestRenderDigest_SplitsLinkedAndUnlinked(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	day := time.Date(2026, 7, 11, 14, 30, 0, 0, time.UTC)

	linked := events.NewEvent("claude_code", "s1:1", day, "Worked on the linked thing")
	linked.Artifacts["ticket_keys"] = []any{"PROJ-42"}
	_, err = s.InsertEvent(linked)
	require.NoError(t, err)

	unlinked := events.NewEvent("claude_code", "s2:1", day, "Worked on the unlinked thing")
	_, err = s.InsertEvent(unlinked)
	require.NoError(t, err)

	// A sentinel key ($PROJECT-0) must be treated as unlinked, not a real link.
	sentinel := events.NewEvent("claude_code", "s3:1", day, "Placated the commit checker")
	sentinel.Artifacts["ticket_keys"] = []any{"PROJ-0"}
	_, err = s.InsertEvent(sentinel)
	require.NoError(t, err)

	out, err := pipeline.RenderDigest(s, time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC))

	require.NoError(t, err)
	assert.Contains(t, out, "## Linked to tickets")
	assert.Contains(t, out, "PROJ-42")
	assert.Contains(t, out, "## Untracked work")
	assert.Contains(t, out, "Worked on the unlinked thing")
	assert.Contains(t, out, "Placated the commit checker")
	assert.NotContains(t, out, "PROJ-0")
}
