package jira_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	collectorjira "github.com/jcogilvie/unjira/internal/collector/jira"
)

func TestPosition_RoundTrips(t *testing.T) {
	watermark := time.Date(2026, 8, 20, 14, 30, 0, 0, time.UTC)
	encoded := collectorjira.EncodePosition("project = PROJ", watermark)

	got, ok := collectorjira.DecodePosition(encoded, "project = PROJ")

	require.True(t, ok, "the same JQL must accept its own watermark")
	assert.True(t, got.Equal(watermark), "want %v, got %v", watermark, got)
}

func TestDecodePosition_RejectsWatermarkFromDifferentJQL(t *testing.T) {
	encoded := collectorjira.EncodePosition("assignee = currentUser()",
		time.Date(2026, 8, 20, 14, 30, 0, 0, time.UTC))

	_, ok := collectorjira.DecodePosition(encoded, "project = PROJ")

	assert.False(t, ok,
		"a watermark from a narrower query would permanently hide issues untouched since it was stored")
}

func TestDecodePosition_HashCoversTheProjectScope(t *testing.T) {
	// EncodePosition is always given the *effective* JQL, so adding a project
	// key must invalidate too — the same bug one level up.
	narrow := `(assignee = currentUser()) AND project IN ("PROJ")`
	wide := `(assignee = currentUser()) AND project IN ("PROJ", "OPS")`
	encoded := collectorjira.EncodePosition(narrow, time.Date(2026, 8, 20, 14, 30, 0, 0, time.UTC))

	_, ok := collectorjira.DecodePosition(encoded, wide)

	assert.False(t, ok, "widening project_keys must invalidate the watermark")
}

func TestDecodePosition_MalformedInputsRescanRatherThanFail(t *testing.T) {
	tests := []struct {
		name     string
		position string
	}{
		{name: "empty (no cursor yet)", position: ""},
		{name: "no separator", position: "abc123"},
		{name: "unparseable watermark", position: "abc123:not-a-time"},
		{name: "empty watermark", position: "abc123:"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ok := collectorjira.DecodePosition(tt.position, "project = PROJ")

			assert.False(t, ok,
				"an unreadable cursor must fall back to a full rescan, which is safe; "+
					"treating it as valid would skip issues")
		})
	}
}

func TestEncodePosition_PreservesSubSecondPrecisionAndZone(t *testing.T) {
	// A watermark truncated to the second can re-fetch an issue (harmless) or,
	// if rounded up, skip one (not harmless). This project has already shipped
	// one bug from a timestamp format that silently dropped sub-second digits.
	watermark := time.Date(2026, 8, 20, 14, 30, 0, 123456789, time.FixedZone("+02:00", 2*60*60))
	encoded := collectorjira.EncodePosition("project = PROJ", watermark)

	got, ok := collectorjira.DecodePosition(encoded, "project = PROJ")

	require.True(t, ok)
	assert.True(t, got.Equal(watermark), "want %v, got %v", watermark, got)
}

func TestCursorResource_NamesConnectionAndQuery(t *testing.T) {
	// The query name is part of the cursors-table key, so renaming a query
	// resets only that query's watermark rather than every query's.
	assert.Equal(t, "corp/mine", collectorjira.CursorResource("corp", "mine"))
}
