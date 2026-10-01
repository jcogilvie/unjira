package github_test

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ghclient "github.com/jcogilvie/unjira/internal/clients/github"
	collectorgithub "github.com/jcogilvie/unjira/internal/collector/github"
	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/credentials"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/pipeline"
	"github.com/jcogilvie/unjira/internal/store"
)

// fakeAPI is a canned implementation of collectorgithub.API — no subprocess,
// no network, per the design's own testing instruction (§9): "a fake
// github.Client (canned []PullRequest per call, no subprocess)".
type fakeAPI struct {
	pulls         []ghclient.PullRequest
	timelines     map[int][]ghclient.IssueEvent
	pullsErr      error
	timelineErr   error
	seenSince     []time.Time
	requestedRefs []ghclient.RepoRef
}

func (f *fakeAPI) ListPullRequests(ref ghclient.RepoRef, since time.Time) ([]ghclient.PullRequest, error) {
	f.seenSince = append(f.seenSince, since)
	f.requestedRefs = append(f.requestedRefs, ref)
	if f.pullsErr != nil {
		return nil, f.pullsErr
	}

	return f.pulls, nil
}

func (f *fakeAPI) ListIssueEvents(_ ghclient.RepoRef, number int) ([]ghclient.IssueEvent, error) {
	if f.timelineErr != nil {
		return nil, f.timelineErr
	}

	return f.timelines[number], nil
}

func factoryFor(apis map[string]*fakeAPI) collectorgithub.ClientFactory {
	return func(base, _ string) (collectorgithub.API, error) {
		api, ok := apis[base]
		if !ok {
			return nil, fmt.Errorf("no fake registered for base %q", base)
		}

		return api, nil
	}
}

func pr(number int, state string, updatedAt time.Time) ghclient.PullRequest {
	p := ghclient.PullRequest{
		Number: number, Title: "Fix PROJ-1", State: state,
		HTMLURL:   fmt.Sprintf("https://github.com/o/r/pull/%d", number),
		CreatedAt: updatedAt, UpdatedAt: updatedAt,
	}
	p.User.Login = "alice"
	p.Head.Ref = "feature/PROJ-1"

	return p
}

func testStore(t *testing.T) *store.Store {
	t.Helper()

	s, err := store.Open(filepath.Join(t.TempDir(), "unjira.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	return s
}

func testContext(s *store.Store, repos []string, host string) pipeline.CollectContext {
	return pipeline.CollectContext{
		Store:   s,
		Options: map[string]any{"repos": repos},
		GitHubCredentials: credentials.NewSet(map[string]credentials.Credential{
			host: {Token: "tok"},
		}),
	}
}

func collectAll(t *testing.T, collector *collectorgithub.Collector, cc pipeline.CollectContext) ([]events.Event, error) {
	t.Helper()

	var got []events.Event
	err := collector.Collect(cc, func(e events.Event) { got = append(got, e) })

	return got, err
}

func TestCollect_EmitsOpenedEventForANewPR(t *testing.T) {
	api := &fakeAPI{pulls: []ghclient.PullRequest{
		pr(42, "open", time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)),
	}}
	collector := collectorgithub.NewWithClientFactory(factoryFor(map[string]*fakeAPI{
		"https://api.github.com": api,
	}))
	s := testStore(t)
	cc := testContext(s, []string{"o/r"}, "github.com")

	got, err := collectAll(t, collector, cc)

	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "o/r#42:opened", got[0].ExternalID)
}

// TestCollect_MissingCredentialErrorsNamingHostAndEnvVar guards a missing
// credential failing loudly, naming both the host and the env var — the
// design's §8: "a missing or empty credential fails loudly ... naming the
// env var and the HOST it was looked up under".
func TestCollect_MissingCredentialErrorsNamingHostAndEnvVar(t *testing.T) {
	collector := collectorgithub.New()
	s := testStore(t)
	cc := pipeline.CollectContext{
		Store:             s,
		Options:           map[string]any{"repos": []string{"unconfigured.example.com/o/r"}},
		GitHubCredentials: credentials.Set{},
	}

	_, err := collectAll(t, collector, cc)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "unconfigured.example.com")
	assert.Contains(t, err.Error(), "UNJIRA_GITHUB_CREDENTIALS")
}

// TestCollect_MalformedRepoEntryErrorsWithoutStoppingOtherRepos is the
// per-repo failure isolation the design's §"Failure is per repo" calls for:
// one bad config entry must not block a sibling valid repo's collection.
func TestCollect_MalformedRepoEntryErrorsWithoutStoppingOtherRepos(t *testing.T) {
	api := &fakeAPI{pulls: []ghclient.PullRequest{
		pr(1, "open", time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)),
	}}
	collector := collectorgithub.NewWithClientFactory(factoryFor(map[string]*fakeAPI{
		"https://api.github.com": api,
	}))
	s := testStore(t)
	cc := testContext(s, []string{"someorg/platform/infra", "o/r"}, "github.com")

	got, err := collectAll(t, collector, cc)

	require.Error(t, err, "the malformed entry must surface as a failure")
	require.Len(t, got, 1, "the sibling valid repo must still be collected")
	assert.Equal(t, "o/r#1:opened", got[0].ExternalID)
}

// TestCollect_WatermarkAdvancesOnlyAfterASuccessfulRepoPass mirrors
// collector/jira's own watermark discipline: a repo whose fetch fails must
// not advance its cursor, so the next pass retries the same range.
func TestCollect_WatermarkAdvancesOnlyAfterASuccessfulRepoPass(t *testing.T) {
	api := &fakeAPI{pullsErr: fmt.Errorf("boom")}
	collector := collectorgithub.NewWithClientFactory(factoryFor(map[string]*fakeAPI{
		"https://api.github.com": api,
	}))
	s := testStore(t)
	cc := testContext(s, []string{"o/r"}, "github.com")

	_, err := collectAll(t, collector, cc)
	require.Error(t, err)

	position, err := s.GetCursor(collectorgithub.Name, "github.com/o/r")
	require.NoError(t, err)
	assert.Empty(t, position, "a failed pass must not persist any watermark")
}

// TestCollect_WatermarkAdvancesToMaxUpdatedAtOnSuccess confirms the watermark
// is written, and to the max UpdatedAt observed — the value the next pass's
// backfill decision (DecodePosition) depends on.
func TestCollect_WatermarkAdvancesToMaxUpdatedAtOnSuccess(t *testing.T) {
	newer := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	older := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	api := &fakeAPI{pulls: []ghclient.PullRequest{
		pr(2, "open", newer),
		pr(1, "open", older),
	}}
	collector := collectorgithub.NewWithClientFactory(factoryFor(map[string]*fakeAPI{
		"https://api.github.com": api,
	}))
	s := testStore(t)
	cc := testContext(s, []string{"o/r"}, "github.com")

	_, err := collectAll(t, collector, cc)
	require.NoError(t, err)

	position, err := s.GetCursor(collectorgithub.Name, "github.com/o/r")
	require.NoError(t, err)
	got, ok := collectorgithub.DecodePosition(position)
	require.True(t, ok)
	assert.True(t, got.Equal(newer), "watermark must be the MAX updated_at seen, not the last one processed")
}

// TestCollect_FirstPassWithNoCursorBackfillsFromDefaultDays proves a repo new
// to config (no existing cursor) uses the configured/default backfill
// window rather than since=zero (which would mean "collect everything").
func TestCollect_FirstPassWithNoCursorBackfillsFromDefaultDays(t *testing.T) {
	api := &fakeAPI{}
	collector := collectorgithub.NewWithClientFactory(factoryFor(map[string]*fakeAPI{
		"https://api.github.com": api,
	}))
	s := testStore(t)
	cc := testContext(s, []string{"o/r"}, "github.com")

	before := time.Now().UTC().AddDate(0, 0, -collectorgithub.DefaultBackfillDays)
	_, err := collectAll(t, collector, cc)
	after := time.Now().UTC().AddDate(0, 0, -collectorgithub.DefaultBackfillDays)

	require.NoError(t, err)
	require.Len(t, api.seenSince, 1)
	assert.True(t, !api.seenSince[0].Before(before) && !api.seenSince[0].After(after),
		"first pass must backfill from DefaultBackfillDays, got %v", api.seenSince[0])
}

// TestCollect_CustomBackfillDaysOptionIsHonored: the design's §8
// backfill_days config key must actually reach the collector.
func TestCollect_CustomBackfillDaysOptionIsHonored(t *testing.T) {
	api := &fakeAPI{}
	collector := collectorgithub.NewWithClientFactory(factoryFor(map[string]*fakeAPI{
		"https://api.github.com": api,
	}))
	s := testStore(t)
	cc := pipeline.CollectContext{
		Store:   s,
		Options: map[string]any{"repos": []string{"o/r"}, "backfill_days": 7},
		GitHubCredentials: credentials.NewSet(map[string]credentials.Credential{
			"github.com": {Token: "tok"},
		}),
	}

	before := time.Now().UTC().AddDate(0, 0, -7)
	_, err := collectAll(t, collector, cc)
	after := time.Now().UTC().AddDate(0, 0, -7)

	require.NoError(t, err)
	require.Len(t, api.seenSince, 1)
	assert.True(t, !api.seenSince[0].Before(before) && !api.seenSince[0].After(after))
}

// TestCollect_SecondPassAppliesStoredWatermark: once a cursor exists, the
// next pass's ListPullRequests call receives it as `since`, not the
// backfill horizon.
func TestCollect_SecondPassAppliesStoredWatermark(t *testing.T) {
	watermark := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
	api := &fakeAPI{pulls: []ghclient.PullRequest{pr(1, "open", watermark)}}
	collector := collectorgithub.NewWithClientFactory(factoryFor(map[string]*fakeAPI{
		"https://api.github.com": api,
	}))
	s := testStore(t)
	cc := testContext(s, []string{"o/r"}, "github.com")

	_, err := collectAll(t, collector, cc)
	require.NoError(t, err)
	_, err = collectAll(t, collector, cc)
	require.NoError(t, err)

	require.Len(t, api.seenSince, 2)
	assert.True(t, api.seenSince[1].Equal(watermark),
		"the second pass must be bounded by the watermark the first pass wrote")
}

// TestCollect_ClosedPRFetchesTimelineAndEmitsCompletionEvents is the "closed
// PR" full-flow proof: opened once, plus both timeline-derived completion
// events for a merge — matching the design's live-measured 93ms-apart pair.
func TestCollect_ClosedPRFetchesTimelineAndEmitsCompletionEvents(t *testing.T) {
	mergedAt := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	closedAt := mergedAt.Add(93 * time.Millisecond)
	merged := pr(69, "closed", mergedAt)
	merged.MergedAt = &mergedAt
	merged.ClosedAt = &closedAt

	api := &fakeAPI{
		pulls: []ghclient.PullRequest{merged},
		timelines: map[int][]ghclient.IssueEvent{
			69: {
				{ID: 31343430711, Event: "merged", CreatedAt: mergedAt},
				{ID: 31343430804, Event: "closed", CreatedAt: closedAt},
			},
		},
	}
	collector := collectorgithub.NewWithClientFactory(factoryFor(map[string]*fakeAPI{
		"https://api.github.com": api,
	}))
	s := testStore(t)
	cc := testContext(s, []string{"o/r"}, "github.com")

	got, err := collectAll(t, collector, cc)

	require.NoError(t, err)
	ids := make([]string, 0, len(got))
	for _, e := range got {
		ids = append(ids, e.ExternalID)
	}
	assert.ElementsMatch(t, []string{
		"o/r#69:opened", "o/r#69:merged:31343430711", "o/r#69:closed:31343430804",
	}, ids)
}

// TestCollect_OpenPRDoesNotFetchTimeline: an open PR has no completion facts
// to fetch, so this collector must not spend the extra API call on it.
func TestCollect_OpenPRDoesNotFetchTimeline(t *testing.T) {
	api := &fakeAPI{pulls: []ghclient.PullRequest{
		pr(1, "open", time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)),
	}, timelineErr: fmt.Errorf("must not be called")}
	collector := collectorgithub.NewWithClientFactory(factoryFor(map[string]*fakeAPI{
		"https://api.github.com": api,
	}))
	s := testStore(t)
	cc := testContext(s, []string{"o/r"}, "github.com")

	got, err := collectAll(t, collector, cc)

	require.NoError(t, err, "an open PR must never trigger the timeline call")
	require.Len(t, got, 1)
}

// TestCollect_GHESHostResolvesToItsOwnAPIBaseAndCredential exercises the
// host-qualified config form end to end: a GHES repo entry must resolve to
// the GHES base URL (not api.github.com) and look up its credential by that
// host.
func TestCollect_GHESHostResolvesToItsOwnAPIBaseAndCredential(t *testing.T) {
	api := &fakeAPI{pulls: []ghclient.PullRequest{
		pr(9, "open", time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)),
	}}
	collector := collectorgithub.NewWithClientFactory(factoryFor(map[string]*fakeAPI{
		"https://github.acme.corp/api/v3": api,
	}))
	s := testStore(t)
	cc := testContext(s, []string{"github.acme.corp/platform/infra"}, "github.acme.corp")

	got, err := collectAll(t, collector, cc)

	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "platform/infra#9:opened", got[0].ExternalID)
}

// TestCollect_RunCollectDedupesAcrossPasses is the full pipeline.RunCollect
// integration test the design's §9 calls for, matching collector/jira's own
// TestRunCollect_JiraCollectorDedupesOnSecondPass shape.
func TestCollect_RunCollectDedupesAcrossPasses(t *testing.T) {
	api := &fakeAPI{pulls: []ghclient.PullRequest{
		pr(42, "open", time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)),
	}}
	s := testStore(t)
	reg := map[string]func() pipeline.Collector{
		collectorgithub.Name: func() pipeline.Collector {
			return collectorgithub.NewWithClientFactory(factoryFor(map[string]*fakeAPI{
				"https://api.github.com": api,
			}))
		},
	}
	cfg := config.Config{Collectors: map[string]map[string]any{
		collectorgithub.Name: {"enabled": true, "repos": []string{"o/r"}},
	}}
	creds := credentials.NewSet(map[string]credentials.Credential{"github.com": {Token: "tok"}})

	first, err := pipeline.RunCollect(cfg, s, reg, nil, credentials.Set{}, creds, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, first[collectorgithub.Name])

	second, err := pipeline.RunCollect(cfg, s, reg, nil, credentials.Set{}, creds, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, second[collectorgithub.Name],
		"(source, external_id) dedup makes re-running the collector safe")
}
