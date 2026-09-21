package github_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ghclient "github.com/jcogilvie/unjira/internal/clients/github"
	collectorgithub "github.com/jcogilvie/unjira/internal/collector/github"
)

func TestCursorResource_IsHostQualifiedOwnerRepo(t *testing.T) {
	ref, err := ghclient.ParseRepoRef("o/r")
	require.NoError(t, err)

	assert.Equal(t, "github.com/o/r", collectorgithub.CursorResource(ref))
}

func TestEncodeDecodePosition_RoundTrips(t *testing.T) {
	watermark := time.Date(2026, 9, 21, 1, 13, 11, 123000000, time.UTC)

	position := collectorgithub.EncodePosition(watermark)
	got, ok := collectorgithub.DecodePosition(position)

	require.True(t, ok)
	assert.True(t, got.Equal(watermark), "want %v, got %v", watermark, got)
}

// TestDecodePosition_UnreadableValueIsUnusableNotAnError mirrors
// collector/jira's DecodePosition: false means "rescan from the horizon,"
// which is always safe because (source, external_id) dedup makes re-emission
// free. Wrongly TRUSTING an unreadable watermark would be the unsafe
// direction — this is a corrupted local cursor row, not remote data, so
// there is nothing to lose by rescanning.
func TestDecodePosition_UnreadableValueIsUnusableNotAnError(t *testing.T) {
	_, ok := collectorgithub.DecodePosition("not-a-timestamp")

	assert.False(t, ok)
}
