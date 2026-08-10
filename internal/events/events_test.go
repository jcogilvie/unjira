package events_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/events"
)

func TestExtractTicketKeys_DedupesInOrder(t *testing.T) {
	text := "PROJ-123 fixes AUTH-9; see PROJ-123 and proj-99 (lowercase ignored)"

	got := events.ExtractTicketKeys(text)

	assert.Equal(t, []string{"PROJ-123", "AUTH-9"}, got)
}

func TestNewEvent_ArtifactsDefaultsToEmptyMap(t *testing.T) {
	occurredAt := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

	e := events.NewEvent("claude_code", "session-1", occurredAt, "did some work")

	assert.NotNil(t, e.Artifacts)
	assert.Empty(t, e.Artifacts)

	// Must be safely writable, unlike Go's zero-value nil map.
	e.Artifacts["key"] = "value"
	assert.Equal(t, "value", e.Artifacts["key"])
}

func TestCompileLinkExclusionPatterns_CompilesEachPattern(t *testing.T) {
	compiled, err := events.CompileLinkExclusionPatterns([]string{"-0$", "-1$"})

	require.NoError(t, err)
	require.Len(t, compiled, 2)
	assert.True(t, compiled[0].MatchString("PROJ-0"))
	assert.True(t, compiled[1].MatchString("PROJ-1"))
}

func TestCompileLinkExclusionPatterns_InvalidPatternNamesItInError(t *testing.T) {
	_, err := events.CompileLinkExclusionPatterns([]string{"("})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "(")
}

func TestCompileLinkExclusionPatterns_EmptyIsANoOp(t *testing.T) {
	compiled, err := events.CompileLinkExclusionPatterns(nil)

	require.NoError(t, err)
	assert.Nil(t, compiled)
}

func TestPartitionExcludedKeys_SplitsOnMatch(t *testing.T) {
	compiled, err := events.CompileLinkExclusionPatterns([]string{"-0$"})
	require.NoError(t, err)

	kept, excluded := events.PartitionExcludedKeys([]string{"PROJ-42", "PROJ-0"}, compiled)

	assert.Equal(t, []string{"PROJ-42"}, kept)
	assert.Equal(t, []string{"PROJ-0"}, excluded)
}

func TestPartitionExcludedKeys_NoMatchKeepsOrderAndReportsNoExclusions(t *testing.T) {
	compiled, err := events.CompileLinkExclusionPatterns([]string{"-0$"})
	require.NoError(t, err)

	kept, excluded := events.PartitionExcludedKeys([]string{"PROJ-42"}, compiled)

	assert.Equal(t, []string{"PROJ-42"}, kept)
	assert.Nil(t, excluded)
}

func TestPartitionExcludedKeys_NoPatternsKeepsEverything(t *testing.T) {
	keys := []string{"PROJ-42", "PROJ-0"}

	kept, excluded := events.PartitionExcludedKeys(keys, nil)

	assert.Equal(t, keys, kept)
	assert.Nil(t, excluded)
}
