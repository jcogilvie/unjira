package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/store"
)

// insertPREvent inserts a github-sourced event carrying pr as its
// events.ArtifactPullRequest, returning its id.
func insertPREvent(t *testing.T, s *store.Store, extID, pr string, at time.Time) int64 {
	t.Helper()

	e := events.NewEvent("github", extID, at, "pr event "+extID)
	if pr != "" {
		e.Artifacts[events.ArtifactPullRequest] = pr
	}
	_, err := s.InsertEvent(e)
	require.NoError(t, err)

	id, err := s.EventIDByExternalID("github", extID)
	require.NoError(t, err)

	return id
}

// TestPullRequestMemberHolders_ReportsEveryNarrativeHoldingAMember is the lookup the
// clustering pre-filter's PR-identity join stands on. It must answer the question the
// join asks — which narratives hold this PR as WORK — so a context link is not holding
// it, a narrative is reported once however many of the PR's events it holds, and every
// holder is reported, with its status, so the caller can refuse an ambiguous identity
// rather than pick one.
func TestPullRequestMemberHolders_ReportsEveryNarrativeHoldingAMember(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	const pr7, pr8, pr9 = "github.com/o/r#7", "github.com/o/r#8", "github.com/o/r#9"

	one := seedNarrative(t, s, "one holder", base)
	require.NoError(t, s.LinkMembers(one, []int64{
		insertPREvent(t, s, "o/r#7:opened", pr7, base),
		insertPREvent(t, s, "pr_create:a", pr7, base),
	}, 0.9))

	splitA := seedNarrative(t, s, "two holders a", base)
	splitB := seedNarrative(t, s, "two holders b", base)
	require.NoError(t, s.LinkMembers(splitA, []int64{insertPREvent(t, s, "o/r#8:opened", pr8, base)}, 0.9))
	require.NoError(t, s.LinkMembers(splitB, []int64{insertPREvent(t, s, "o/r#8:merged", pr8, base)}, 0.9))
	require.NoError(t, s.SetNarrativeStatus(splitB, store.StatusSplit))

	background := seedNarrative(t, s, "holds #7 only as context", base)
	require.NoError(t, s.LinkMembers(background, []int64{insertBareEvent(t, s, "own", base)}, 0.9))
	require.NoError(t, s.LinkContext(background, []int64{insertPREvent(t, s, "pr_create:b", pr7, base)}))
	require.NoError(t, s.LinkMembers(one, []int64{mustEventID(t, s, "github", "pr_create:b")}, 0.9))

	got, err := s.PullRequestMemberHolders([]string{pr7, pr8, pr9})

	require.NoError(t, err)
	assert.Equal(t, map[string][]store.PullRequestHolder{
		pr7: {{NarrativeID: one, Status: store.StatusOpen}},
		pr8: {{NarrativeID: splitA, Status: store.StatusOpen}, {NarrativeID: splitB, Status: store.StatusSplit}},
	}, got, "a PR no narrative holds is absent, not an empty entry")
}

// TestPullRequestMemberHolders_MatchesTheValueExactly: the join key is compared whole,
// so a pre-host-qualification "o/r#7" on a stored event holds nothing for
// "github.com/o/r#7", and a GHES PR with the same owner/repo#N is a different key.
func TestPullRequestMemberHolders_MatchesTheValueExactly(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	legacy := seedNarrative(t, s, "legacy", base)
	ghes := seedNarrative(t, s, "ghes", base)
	require.NoError(t, s.LinkMembers(legacy, []int64{insertPREvent(t, s, "legacy", "o/r#7", base)}, 0.9))
	require.NoError(t, s.LinkMembers(ghes, []int64{insertPREvent(t, s, "ghes", "ghes.corp.example/o/r#7", base)}, 0.9))

	got, err := s.PullRequestMemberHolders([]string{"github.com/o/r#7"})

	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestPullRequestMemberHolders_NoKeysAsksNothing(t *testing.T) {
	got, err := openStore(t).PullRequestMemberHolders(nil)

	require.NoError(t, err)
	assert.Empty(t, got)
}

func mustEventID(t *testing.T, s *store.Store, source, extID string) int64 {
	t.Helper()

	id, err := s.EventIDByExternalID(source, extID)
	require.NoError(t, err)

	return id
}
