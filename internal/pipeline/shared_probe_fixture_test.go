package pipeline

// shared_probe_fixture_test.go tests the acceptance instrument offline, against stores
// whose answers are known. Treatment-only: it seeds links with LinkMembers and
// LinkContext, which the baseline arm does not have.

import (
	"database/sql"
	"fmt"
	"maps"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/store"
)

func probeFixtureEvent(t *testing.T, s *store.Store, source, ext string, at time.Time, artifacts map[string]any) int64 {
	t.Helper()

	e := events.NewEvent(source, ext, at, ext)
	maps.Copy(e.Artifacts, artifacts)
	_, err := s.InsertEvent(e)
	require.NoError(t, err)
	id, err := s.EventIDByExternalID(source, ext)
	require.NoError(t, err)

	return id
}

// TestMeasureAcceptance_ReadsTheStoreNotTheReport: a fixture with known answers. PR 1's
// narrative holds its anchor, its :opened event and a context link to a root segment
// that is PR 2's narrative's member; PR 2's merge event sits in a third narrative.
func TestMeasureAcceptance_ReadsTheStoreNotTheReport(t *testing.T) {
	path := t.TempDir() + "/probe.db"
	s, err := store.Open(path)
	require.NoError(t, err)
	base := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	window := correlator.TimeRange{Start: base, End: base.Add(24 * time.Hour)}

	opened1 := probeFixtureEvent(t, s, "github", "o/r#1:opened", base, map[string]any{events.ArtifactPullRequest: "o/r#1"})
	anchor1 := probeFixtureEvent(t, s, "claude_code", "pr_create:1", base, map[string]any{
		"anchor_kind": "pr_create", "pr_create_outcome": "created", events.ArtifactPullRequest: "o/r#1",
	})
	segment := probeFixtureEvent(t, s, "claude_code", "root:1", base, map[string]any{"session_id": "S1"})
	opened2 := probeFixtureEvent(t, s, "github", "o/r#2:opened", base, map[string]any{events.ArtifactPullRequest: "o/r#2"})
	merged2 := probeFixtureEvent(t, s, "github", "o/r#2:merged", base, map[string]any{events.ArtifactPullRequest: "o/r#2"})

	n1, err := s.InsertNarrative(base, base, "PR 1", "s")
	require.NoError(t, err)
	n2, err := s.InsertNarrative(base, base, "PR 2", "s")
	require.NoError(t, err)
	n3, err := s.InsertNarrative(base, base, "stray merge", "s")
	require.NoError(t, err)
	require.NoError(t, s.LinkMembers(n1, []int64{opened1, anchor1}, 0.9))
	require.NoError(t, s.LinkMembers(n2, []int64{opened2, segment}, 0.6))
	require.NoError(t, s.LinkMembers(n3, []int64{merged2}, 0.3))
	require.NoError(t, s.LinkContext(n1, []int64{segment}))
	empty, err := s.InsertNarrative(base, base, "emptied by a reshuffle", "s")
	require.NoError(t, err)
	require.NoError(t, s.Close())

	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	links, err := loadProbeLinks(db)
	require.NoError(t, err)
	workless, err := loadWorklessNarratives(db)
	require.NoError(t, err)

	m := measureAcceptance(links, workless, window)

	assert.Equal(t, 3, m.PRNarratives, "every narrative holding a PR event is a PR narrative")
	assert.Equal(t, 1, m.EvidenceByMember, "only PR 2's narrative holds a segment as a member")
	assert.Equal(t, 2, m.EvidenceByMemberOrContext, "PR 1's narrative holds it as context; the anchor never counts")
	assert.Equal(t, 1, m.TranscriptsByMemberOrContext)
	assert.Equal(t, 1, m.Anchors)
	assert.Equal(t, 1, m.AnchorsCoalesced)
	assert.Equal(t, 2, m.PRs)
	assert.Equal(t, 1, m.PRsIntact)
	require.Len(t, m.PRProblems, 1)
	assert.Contains(t, m.PRProblems[0], "o/r#2: split across narratives")
	assert.Equal(t, 1, m.ContextLinks)
	assert.Equal(t, 1, m.SharedEvents)
	assert.Equal(t, 1, m.MaxFanOut)
	assert.Equal(t, []float64{0.3, 0.6, 0.6, 0.9, 0.9}, m.MemberConfidences)
	assert.Equal(t, []string{fmt.Sprintf("open narrative %d holds no member event (F37's shape)", empty)}, m.Violations,
		"a narrative with no links at all is invisible to the link rows, so it needs its own query")
	assert.Contains(t, renderAcceptance(m), "M1  PR narratives with transcript evidence: member-only 1/3, member-or-context 2/3")
}

// TestMeasureAttraction_ScreensPassTwoPlacementsAgainstPassOneStreams: after pass 1,
// B holds A's investigation as background. In pass 2, an event on A's branch joins B:
// screened, and attracted, because B's background came from A.
func TestMeasureAttraction_ScreensPassTwoPlacementsAgainstPassOneStreams(t *testing.T) {
	links := []probeLink{
		{seq: 1, narrative: 1, eventID: 10, kind: "member", artifacts: map[string]any{events.ArtifactGitBranch: "fix/cache"}},
		{seq: 2, narrative: 2, eventID: 20, kind: "member", artifacts: map[string]any{events.ArtifactGitBranch: "fix/logs"}},
		{seq: 3, narrative: 2, eventID: 10, kind: "context", artifacts: map[string]any{events.ArtifactGitBranch: "fix/cache"}},
		// pass 2
		{
			seq: 4, narrative: 2, eventID: 30, kind: "member", source: "claude_code", externalID: "later-cache",
			artifacts: map[string]any{events.ArtifactGitBranch: "fix/cache"},
		},
		{
			seq: 5, narrative: 1, eventID: 40, kind: "member", source: "claude_code", externalID: "more-cache",
			artifacts: map[string]any{events.ArtifactGitBranch: "fix/cache"},
		},
	}

	a := measureAttraction(links, 3)

	assert.Equal(t, []string{"claude_code/later-cache joined narrative 2; its evidence points at [1]"}, a.Screened)
	assert.Equal(t, a.Screened, a.Attracted)
}
