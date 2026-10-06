package github_test

// author_test.go covers which PRs count as the user's work evidence. The collector
// ingested every PR in a configured repo: measured on a real 30-day pass over six repos,
// 365 of 3,845 GitHub events were the user's, and the rest (dependabot, Renovate,
// cherry-pick bots, other contributors) became 843 narratives. Another person's PR is not
// evidence of what this user did. (The user's reviews and comments on others' PRs are a
// separate, planned kind of evidence; see the README roadmap.)

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ghclient "github.com/jcogilvie/unjira/internal/clients/github"
	collectorgithub "github.com/jcogilvie/unjira/internal/collector/github"
)

func authored(number int, login string, at time.Time) ghclient.PullRequest {
	p := pr(number, "open", at)
	p.User.Login = login

	return p
}

func collectorFor(api *fakeAPI) *collectorgithub.Collector {
	return collectorgithub.NewWithClientFactory(factoryFor(map[string]*fakeAPI{"https://api.github.com": api}))
}

func TestCollect_SkipsPRsAuthoredBySomeoneElse(t *testing.T) {
	t0 := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	api := &fakeAPI{login: "jcogilvie", pulls: []ghclient.PullRequest{
		authored(1, "jcogilvie", t0),
		authored(2, "renovate[bot]", t0.Add(time.Hour)),
		authored(3, "crenshaw-dev", t0.Add(2*time.Hour)),
	}}
	s := testStore(t)
	cc := testContext(s, []string{"o/r"}, "github.com")

	got, err := collectAll(t, collectorFor(api), cc)

	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "o/r#1:opened", got[0].ExternalID)

	// The watermark still steps past the skipped PRs: they are out of scope, not
	// unread, and leaving them behind it would re-list them on every pass.
	position, err := s.GetCursor(collectorgithub.Name, collectorgithub.CursorResource(ghclient.RepoRef{Host: "github.com", Owner: "o", Repo: "r"}))
	require.NoError(t, err)
	since, ok := collectorgithub.DecodePosition(position)
	require.True(t, ok)
	assert.True(t, since.Equal(t0.Add(2*time.Hour)), "watermark at the newest PR listed, skipped ones included; got %s", since)
}

func TestCollect_AuthorMatchIgnoresCase(t *testing.T) {
	t0 := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	api := &fakeAPI{login: "JCOgilvie", pulls: []ghclient.PullRequest{authored(1, "jcogilvie", t0)}}
	s := testStore(t)

	got, err := collectAll(t, collectorFor(api), testContext(s, []string{"o/r"}, "github.com"))

	require.NoError(t, err)
	assert.Len(t, got, 1, "GitHub logins are case-insensitive")
}

func TestCollect_AuthorsOptionReplacesTheAuthenticatedUser(t *testing.T) {
	t0 := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	api := &fakeAPI{login: "jcogilvie", pulls: []ghclient.PullRequest{
		authored(1, "jcogilvie", t0),
		authored(2, "bot-i-run", t0.Add(time.Hour)),
	}}
	s := testStore(t)
	cc := testContext(s, []string{"o/r"}, "github.com")
	cc.Options["authors"] = []any{"bot-i-run"}

	got, err := collectAll(t, collectorFor(api), cc)

	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "o/r#2:opened", got[0].ExternalID)
	assert.Zero(t, api.loginCalls, "an explicit authors list needs no user lookup")
}

// TestCollect_AFailedUserLookupFailsTheRepo: guessing would collect everyone's PRs or
// nobody's, and neither is safe to do silently.
func TestCollect_AFailedUserLookupFailsTheRepo(t *testing.T) {
	api := &fakeAPI{
		loginErr: errors.New("401 bad credentials"),
		pulls:    []ghclient.PullRequest{authored(1, "jcogilvie", time.Now())},
	}
	s := testStore(t)

	got, err := collectAll(t, collectorFor(api), testContext(s, []string{"o/r"}, "github.com"))

	require.ErrorContains(t, err, "401 bad credentials")
	require.ErrorContains(t, err, "`authors` option", "the error names the fix")
	assert.Empty(t, got)
}

func TestCollect_LooksTheUserUpOncePerHostPerPass(t *testing.T) {
	t0 := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	api := &fakeAPI{login: "jcogilvie", pulls: []ghclient.PullRequest{authored(1, "jcogilvie", t0)}}
	s := testStore(t)

	_, err := collectAll(t, collectorFor(api), testContext(s, []string{"o/a", "o/b", "o/c"}, "github.com"))

	require.NoError(t, err)
	assert.Equal(t, 1, api.loginCalls)
}
