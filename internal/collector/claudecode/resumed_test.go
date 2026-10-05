package claudecode_test

// resumed_test.go covers F33: a resumed Claude Code session writes a new transcript that
// begins with a copy of earlier history. Measured: 1,841 tool_use ids appear in more than
// one root transcript, across 7 groups of files, and every copied line keeps its uuid and
// timestamp (only sessionId is rewritten). Segment ExternalIDs are per file, so the copy
// became a second set of segment events, the same work reaching clustering twice.

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/events"
)

func userMessages(evts []events.Event) int {
	n := 0
	for _, e := range segmentEvents(evts) {
		switch c := e.Artifacts["user_message_count"].(type) {
		case int:
			n += c
		case float64:
			n += int(c)
		}
	}

	return n
}

func originalSession(t *testing.T) *transcriptBuilder {
	t.Helper()

	return newTranscript(t).InCwd("/w/u").WithUUIDs("orig").
		User("2026-09-01T10:00:00Z", "the original ask").
		User("2026-09-01T10:01:00Z", "two").
		User("2026-09-01T10:02:00Z", "three")
}

func TestCollect_AResumedSessionCountsCopiedHistoryOnce(t *testing.T) {
	root := t.TempDir()
	orig := originalSession(t)
	orig.WriteRoot(root, slug, "orig")
	orig.Resumed("resumed").
		User("2026-09-02T09:00:00Z", "the resumed ask").
		User("2026-09-02T09:01:00Z", "five").
		User("2026-09-02T09:02:00Z", "six").
		WriteRoot(root, slug, "resumed")

	_, got := collect(t, root, nil)

	assert.Equal(t, 6, userMessages(got), "three original messages and three new ones, each once")
	segs := segmentEvents(got)
	openings := make([]string, 0, len(segs))
	for _, e := range segs {
		openings = append(openings, e.Summary)
	}
	require.Len(t, openings, 2)
	assert.Contains(t, openings[0]+openings[1], `Opened with: "the resumed ask"`,
		"the resumed file's segment starts at its own new work, not at the copied history")
}

func TestCollect_APureCopyEmitsNoSegment(t *testing.T) {
	root := t.TempDir()
	orig := originalSession(t)
	orig.WriteRoot(root, slug, "orig")
	orig.Resumed("unused").WriteRoot(root, slug, "copy")

	_, got := collect(t, root, nil)

	assert.Len(t, segmentEvents(got), 1)
	assert.Equal(t, 3, userMessages(got))
}

// TestCollect_TheFileThatEndedFirstOwnsSharedHistory: the original stops when it is
// resumed, so its last line is the earlier one. Ownership follows that, not file names,
// which are random session ids: here the copy's name sorts first.
func TestCollect_TheFileThatEndedFirstOwnsSharedHistory(t *testing.T) {
	root := t.TempDir()
	orig := originalSession(t)
	orig.WriteRoot(root, slug, "zzz-original")
	orig.Resumed("resumed").
		User("2026-09-02T09:00:00Z", "the resumed ask").
		User("2026-09-02T09:01:00Z", "five").
		User("2026-09-02T09:02:00Z", "six").
		WriteRoot(root, slug, "aaa-resumed")

	_, got := collect(t, root, nil)

	for _, e := range segmentEvents(got) {
		if e.Artifacts["session_id"] == "zzz-original" {
			assert.Contains(t, e.Summary, `Opened with: "the original ask"`)
		} else {
			assert.Contains(t, e.Summary, `Opened with: "the resumed ask"`)
		}
	}
}

// TestCollect_AnOwnerOutsideTheBackfillWindowStillOwns: an original too old to collect
// still owns its lines, so its copy does not bring them back in. They are older than the
// window, which is what the window is for.
func TestCollect_AnOwnerOutsideTheBackfillWindowStillOwns(t *testing.T) {
	root := t.TempDir()
	orig := originalSession(t)
	origPath := orig.WriteRoot(root, slug, "orig")
	old := time.Now().AddDate(0, 0, -400)
	require.NoError(t, os.Chtimes(origPath, old, old))
	orig.Resumed("resumed").
		User("2026-09-02T09:00:00Z", "the resumed ask").
		User("2026-09-02T09:01:00Z", "five").
		User("2026-09-02T09:02:00Z", "six").
		WriteRoot(root, slug, "resumed")

	_, got := collect(t, root, map[string]any{"backfill_days": 30})

	assert.Equal(t, 3, userMessages(got))
	require.Len(t, segmentEvents(got), 1)
	assert.Equal(t, "resumed", segmentEvents(got)[0].Artifacts["session_id"])
}

// TestCollect_ACopiedPRAnchorIsEmittedOnce: anchors key on the tool_use id alone, so a
// copied PR-creating call already dedupes, and ownership does not filter them: a call in
// the original whose result landed in the copy must still resolve.
func TestCollect_ACopiedPRAnchorIsEmittedOnce(t *testing.T) {
	root := t.TempDir()
	orig := originalSession(t).
		Bash("2026-09-01T10:03:00Z", "toolu_01PR", `gh pr create --fill`).
		ToolResult("2026-09-01T10:03:05Z", "toolu_01PR", "https://github.com/o/r/pull/7\n", false)
	orig.WriteRoot(root, slug, "orig")
	orig.Resumed("resumed").User("2026-09-02T09:00:00Z", "more").WriteRoot(root, slug, "resumed")

	s, got := collect(t, root, nil)

	assert.Equal(t, 1, countInserted(t, s, anchors(got)), "one PR, one anchor")
}

// TestCollect_AnAnchorWhoseResultLandedInTheCopyStillResolves: the original ended
// mid-call, so it holds the PR-creating call and no result; the resumed copy holds both.
// Filtering anchors by line ownership would leave the call with its owner, which has no
// result, and the copy's result with no call: the PR would get no anchor at all.
func TestCollect_AnAnchorWhoseResultLandedInTheCopyStillResolves(t *testing.T) {
	root := t.TempDir()
	orig := originalSession(t).Bash("2026-09-01T10:03:00Z", "toolu_01Mid", `gh pr create --fill`)
	orig.WriteRoot(root, slug, "orig")
	orig.Resumed("resumed").
		ToolResult("2026-09-02T09:00:00Z", "toolu_01Mid", "https://github.com/o/r/pull/8\n", false).
		User("2026-09-02T09:01:00Z", "carry on").
		WriteRoot(root, slug, "resumed")

	_, got := collect(t, root, nil)

	a := onlyAnchor(t, got)
	assert.Equal(t, "created", a.Artifacts["pr_create_outcome"])
	assert.Equal(t, "github.com/o/r#8", a.Artifacts[events.ArtifactPullRequest])
}
