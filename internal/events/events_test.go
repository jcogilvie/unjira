package events_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

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

func TestIsSentinelKey_PrefixAgnostic(t *testing.T) {
	tests := []struct {
		name string
		key  string
		want bool
	}{
		{"zero suffix, short prefix", "XYZ-0", true},
		{"zero suffix, common prefix", "PROJ-0", true},
		{"one suffix, long prefix", "LONGERKEY-1", true},
		{"non-sentinel numeric suffix", "XYZ-10", false},
		{"ordinary issue key", "PROJ-123", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, events.IsSentinelKey(tt.key))
		})
	}
}
