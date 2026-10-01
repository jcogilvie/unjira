//go:build live

package live

// github_test.go drives the real GitHub collector against the fixture repo
// jcogilvie/unjira-sandbox — never jcogilvie/unjira (CLAUDE.md, "Fixture
// instances, not real ones"). Unlike this package's Jira tests it is READ-ONLY:
// the collector has no write path, so nothing here can alter the sandbox.
//
// The sandbox holds four PRs, one per lifecycle path (see the Status section of
// docs/superpowers/specs/2026-09-17-github-collector-design.md):
//
//	#1 DEVSBX-101-merged     merged                          :opened, :merged:<id>, :closed:<id>
//	#2 DEVSBX-102-abandoned  closed unmerged                 :opened, :closed:<id>
//	#3 DEVSBX-103-reopened   closed -> reopened -> re-closed :opened, :closed:<id> x2 (distinct ids)
//	#4 DEVSBX-104-open       open                            :opened
//
// Assertions are STRUCTURAL — counts, kinds, id distinctness, ExternalID shape —
// never pinned timeline ids, so recreating the sandbox (which mints new ids)
// does not break this file, while changing what the collector does with any of
// these paths does.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

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

// sandboxRepo is the only repo this file may collect from.
const sandboxRepo = "jcogilvie/unjira-sandbox"

// sandboxBackfillDays overrides the collector's 30-day DefaultBackfillDays. The
// fixture PRs were created 2026-09-22; under the default they would age out of
// the first-pass window a month later, and this test would start collecting
// zero events — a plausible-looking empty result rather than an error
// (docs/design-notes.md #37). A century is "all of the sandbox's history".
const sandboxBackfillDays = 36500

// githubCredentialExport is the setup line from CLAUDE.md, quoted verbatim in
// the failure message so the fix is copy-pasteable.
const githubCredentialExport = `export UNJIRA_GITHUB_CREDENTIALS="{\"github.com\":{\"token\":\"$(gh auth token)\"}}"`

// sandboxExternalID matches every ExternalID the collector may emit for the
// sandbox: a bare :opened, or :merged/:closed suffixed with GitHub's numeric
// timeline-event id. Anything else is a shape regression.
var sandboxExternalID = regexp.MustCompile(
	`^` + regexp.QuoteMeta(sandboxRepo) + `#(\d+):(opened|merged|closed)(?::(\d+))?$`,
)

// sandboxPR is one fixture PR's expected shape.
type sandboxPR struct {
	number int
	branch string
	key    string
	merged int // expected :merged:<id> events
	closed int // expected :closed:<id> events
}

var sandboxPRs = []sandboxPR{
	{number: 1, branch: "DEVSBX-101-merged", key: "DEVSBX-101", merged: 1, closed: 1},
	{number: 2, branch: "DEVSBX-102-abandoned", key: "DEVSBX-102", closed: 1},
	{number: 3, branch: "DEVSBX-103-reopened", key: "DEVSBX-103", closed: 2},
	{number: 4, branch: "DEVSBX-104-open", key: "DEVSBX-104"},
}

// sandboxEventTotal is the sum of every sandboxPR's events (one :opened each
// plus its completions). Measured manually on 2026-09-22: 9.
func sandboxEventTotal() int {
	total := 0
	for _, pr := range sandboxPRs {
		total += 1 + pr.merged + pr.closed
	}

	return total
}

// githubLiveCredentials gates this file and resolves its credential.
//
// UNJIRA_LIVE unset skips, matching testClient. But UNJIRA_LIVE=1 with no usable
// github.com credential FAILS rather than skipping: someone who asked for the
// live run and got a silent skip believes it passed, which is
// docs/design-notes.md #37 (a missing credential reads as a result, not an
// error) in test-runner form. A worktree never has .env, so this is the
// expected outcome there — and it must look like a failure.
func githubLiveCredentials(t *testing.T) credentials.Set {
	t.Helper()

	if os.Getenv("UNJIRA_LIVE") != "1" {
		t.Skip("set UNJIRA_LIVE=1 to run")
	}

	raw := os.Getenv(credentials.GitHubEnvVar)
	if raw == "" {
		t.Fatalf("UNJIRA_LIVE=1 but %s is unset (checked the environment and .env), so the "+
			"GitHub live tests cannot run. This is a FAILURE, not a skip: a skipped live run reads "+
			"as a passing one. Set it, e.g.:\n  %s",
			credentials.GitHubEnvVar, githubCredentialExport)
	}

	var decoded credentials.JSONSet
	// The decode error is reported, the raw value never is: it holds a token.
	if err := decoded.UnmarshalJSON([]byte(raw)); err != nil {
		t.Fatalf("%s is set but is not a valid credentials object: %v. Expected shape:\n  %s",
			credentials.GitHubEnvVar, err, githubCredentialExport)
	}

	set := decoded.Set()

	cred, ok := set.For(ghclient.DefaultHost)
	if !ok || cred.Token == "" {
		t.Fatalf("%s has no non-empty token for host %q, which %s lives on. Expected shape:\n  %s",
			credentials.GitHubEnvVar, ghclient.DefaultHost, sandboxRepo, githubCredentialExport)
	}

	return set
}

// recordingCollector wraps the real collector so a test can run it through the
// real pipeline.RunCollect (store insert and dedup included) and still see every
// event it emitted, not just RunCollect's inserted count.
type recordingCollector struct {
	inner    pipeline.Collector
	recorded []events.Event
}

func (r *recordingCollector) Name() string { return r.inner.Name() }

func (r *recordingCollector) Collect(cc pipeline.CollectContext, visit func(events.Event)) error {
	return r.inner.Collect(cc, func(evt events.Event) {
		r.recorded = append(r.recorded, evt)
		visit(evt)
	})
}

// liveGitHubStore is a fresh temp-file store, so no cursor from an earlier run
// can make the first pass incremental.
func liveGitHubStore(t *testing.T) *store.Store {
	t.Helper()

	s, err := store.Open(filepath.Join(t.TempDir(), "unjira.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	return s
}

// collectSandbox runs one real collection pass over the sandbox into s, through
// pipeline.RunCollect exactly as `unjira collect` does, returning every event the
// collector emitted and how many of them the store inserted as new.
func collectSandbox(t *testing.T, s *store.Store, creds credentials.Set) ([]events.Event, int) {
	t.Helper()

	recorder := &recordingCollector{inner: collectorgithub.New()}

	cfg := config.Config{Collectors: map[string]map[string]any{
		collectorgithub.Name: {
			"enabled":       true,
			"repos":         []any{sandboxRepo},
			"backfill_days": sandboxBackfillDays,
		},
	}}

	registry := map[string]func() pipeline.Collector{
		collectorgithub.Name: func() pipeline.Collector { return recorder },
	}

	results, err := pipeline.RunCollect(cfg, s, registry, nil, credentials.Set{}, creds, nil)
	require.NoError(t, err,
		"collecting %s failed. A 404 here usually means the token cannot see this PRIVATE repo "+
			"(wrong account, or a fine-grained token not granted it), not that the repo is gone",
		sandboxRepo)

	inserted, ok := results[collectorgithub.Name]
	require.True(t, ok, "RunCollect reported no result for the %q collector: %v", collectorgithub.Name, results)

	return recorder.recorded, inserted
}

// parsedEvent is one event with its ExternalID decomposed.
type parsedEvent struct {
	evt        events.Event
	number     int
	kind       string // opened | merged | closed
	timelineID string // empty for opened
}

// parseSandboxEvents decomposes every ExternalID, failing (with the offending
// id) on any that does not match the expected shape.
func parseSandboxEvents(t *testing.T, got []events.Event) []parsedEvent {
	t.Helper()

	out := make([]parsedEvent, 0, len(got))

	for _, evt := range got {
		match := sandboxExternalID.FindStringSubmatch(evt.ExternalID)
		if match == nil {
			t.Errorf("ExternalID %q does not match %s", evt.ExternalID, sandboxExternalID)
			continue
		}

		number, err := strconv.Atoi(match[1])
		require.NoError(t, err)

		parsed := parsedEvent{evt: evt, number: number, kind: match[2], timelineID: match[3]}

		switch {
		case parsed.kind == "opened" && parsed.timelineID != "":
			t.Errorf("ExternalID %q: :opened must have no timeline-id suffix", evt.ExternalID)
		case parsed.kind != "opened" && parsed.timelineID == "":
			t.Errorf("ExternalID %q: :%s must carry a :<timeline-id> suffix — without it a "+
				"reopened-then-reclosed PR collides on INSERT OR IGNORE", evt.ExternalID, parsed.kind)
		}

		out = append(out, parsed)
	}

	return out
}

// inventory renders every collected ExternalID, sorted, one per line — included
// in failure messages so a mismatch is diagnosable from the output alone, and
// logged on every run so a reviewer can paste it as verification evidence.
func inventory(got []events.Event) string {
	ids := make([]string, 0, len(got))
	for _, evt := range got {
		ids = append(ids, evt.ExternalID)
	}

	sort.Strings(ids)

	return fmt.Sprintf("%d event(s):\n  %s", len(ids), strings.Join(ids, "\n  "))
}

// TestLiveGitHubCollectorSandboxLifecycle checks that the collector's
// beliefs about GitHub's pulls and issue-events responses hold against real
// GitHub, across every lifecycle path the sandbox holds.
//
// PR #3 is the most important assertion in the file: two :closed events with
// distinct timeline ids is the case the id-suffixed ExternalID scheme exists for,
// and nothing in jcogilvie/unjira has ever exercised it.
func TestLiveGitHubCollectorSandboxLifecycle(t *testing.T) {
	creds := githubLiveCredentials(t)

	got, inserted := collectSandbox(t, liveGitHubStore(t), creds)
	inv := inventory(got)
	t.Logf("collected from %s: %s", sandboxRepo, inv)

	require.NotEmpty(t, got,
		"the collector emitted NOTHING for %s — that is not 'no activity', the fixture has %d "+
			"events. Check the backfill window and that the token can see the repo", sandboxRepo, sandboxEventTotal())

	// Every emitted event must be new in a fresh store. inserted < emitted means
	// two events shared an ExternalID and INSERT OR IGNORE silently dropped one —
	// precisely the reopen collision this scheme exists to prevent.
	assert.Equal(t, len(got), inserted,
		"emitted %d events but the fresh store inserted only %d: some ExternalIDs collided and were "+
			"silently dropped.\n%s", len(got), inserted, inv)

	assert.Len(t, got, sandboxEventTotal(), "total sandbox events.\n%s", inv)

	byPR := make(map[int][]parsedEvent)
	for _, parsed := range parseSandboxEvents(t, got) {
		byPR[parsed.number] = append(byPR[parsed.number], parsed)
	}

	for number := range byPR {
		if !slices.ContainsFunc(sandboxPRs, func(pr sandboxPR) bool { return pr.number == number }) {
			t.Errorf("events for unexpected PR #%d — the sandbox gained a PR this test does not "+
				"describe; add it to sandboxPRs.\n%s", number, inv)
		}
	}

	for _, want := range sandboxPRs {
		t.Run(fmt.Sprintf("PR#%d_%s", want.number, want.branch), func(t *testing.T) {
			assertSandboxPR(t, want, byPR[want.number], inv)
		})
	}

	// Called out separately, beyond the per-PR counts, because it is the reason
	// the sandbox exists.
	t.Run("PR#3_reclosed_has_two_distinct_closed_ids", func(t *testing.T) {
		var closedIDs []string
		for _, parsed := range byPR[3] {
			if parsed.kind == "closed" {
				closedIDs = append(closedIDs, parsed.timelineID)
			}
		}

		require.Len(t, closedIDs, 2,
			"PR #3 was closed, reopened and re-closed, so GitHub's timeline holds two closed "+
				"entries; got closed ids %v.\n%s", closedIDs, inv)
		assert.NotEqual(t, closedIDs[0], closedIDs[1],
			"PR #3's two :closed events share timeline id %s — they would collide in the store.\n%s",
			closedIDs[0], inv)
	})
}

// assertSandboxPR checks one fixture PR's events against its expected shape.
func assertSandboxPR(t *testing.T, want sandboxPR, got []parsedEvent, inv string) {
	t.Helper()

	counts := map[string]int{}
	idsByKind := map[string]map[string]bool{"merged": {}, "closed": {}}
	allIDs := map[string]bool{}

	for _, parsed := range got {
		counts[parsed.kind]++

		evt := parsed.evt
		id := evt.ExternalID

		assert.Equal(t, collectorgithub.Name, evt.Source, "%s: Source", id)
		assert.False(t, evt.OccurredAt.IsZero(), "%s: OccurredAt must parse from GitHub's timestamp", id)
		assert.NotEmpty(t, evt.Actor, "%s: Actor (the PR author's login)", id)
		assert.Equal(t, fmt.Sprintf("https://github.com/%s/pull/%d", sandboxRepo, want.number), evt.RawRef,
			"%s: RawRef (the PR's html_url)", id)

		_, marked := evt.Artifacts[events.ArtifactTrackerRecord]
		assert.False(t, marked,
			"%s carries %q (=%v): a PR is work evidence, never a tracker record",
			id, events.ArtifactTrackerRecord, evt.Artifacts[events.ArtifactTrackerRecord])

		assert.Equal(t, want.branch, evt.Artifacts[events.ArtifactGitBranch], "%s: %s", id, events.ArtifactGitBranch)
		assert.Equal(t, []string{want.key}, events.SCMKeysOf(evt),
			"%s: %s — the PR's title/body reference only its own key", id, events.ArtifactSCMKeys)

		completionKind, hasKind := evt.Artifacts["completion_kind"]
		if parsed.kind == "opened" {
			assert.False(t, hasKind, "%s: :opened must not carry completion_kind, got %v", id, completionKind)
			continue
		}

		assert.Equal(t, parsed.kind, completionKind, "%s: completion_kind must match the ExternalID kind", id)

		assert.False(t, allIDs[parsed.timelineID],
			"%s reuses timeline id %s already seen on this PR", id, parsed.timelineID)
		allIDs[parsed.timelineID] = true
		idsByKind[parsed.kind][parsed.timelineID] = true
	}

	assert.Equal(t, 1, counts["opened"], "PR #%d :opened events.\n%s", want.number, inv)
	assert.Equal(t, want.merged, counts["merged"], "PR #%d :merged events.\n%s", want.number, inv)
	assert.Equal(t, want.closed, counts["closed"], "PR #%d :closed events.\n%s", want.number, inv)

	// Distinct ids per kind, not just per PR: a merge's :merged and :closed are
	// two timeline entries with two ids, and neither suppresses the other.
	assert.Len(t, idsByKind["merged"], want.merged, "PR #%d distinct :merged timeline ids.\n%s", want.number, inv)
	assert.Len(t, idsByKind["closed"], want.closed, "PR #%d distinct :closed timeline ids.\n%s", want.number, inv)
}

// TestLiveGitHubCollectorSecondPassAddsNothing checks idempotency against real
// GitHub: a second pass into the same store re-reads at least the most recently
// updated PR (the watermark bound is inclusive) and inserts none of it.
//
// The re-read is asserted, not assumed: "0 new events" from a second pass that
// emitted nothing at all would be vacuous — the #37 shape, a zero that is not a
// measurement.
func TestLiveGitHubCollectorSecondPassAddsNothing(t *testing.T) {
	creds := githubLiveCredentials(t)
	s := liveGitHubStore(t)

	first, firstInserted := collectSandbox(t, s, creds)
	require.NotEmpty(t, first, "first pass emitted nothing, so idempotency cannot be checked")
	require.Equal(t, len(first), firstInserted, "first pass into a fresh store.\n%s", inventory(first))

	second, secondInserted := collectSandbox(t, s, creds)
	t.Logf("second pass re-emitted %s", inventory(second))

	require.NotEmpty(t, second,
		"second pass emitted nothing, so its zero inserts prove nothing. The cursor's inclusive "+
			"watermark should re-read at least the most recently updated PR")
	assert.Zero(t, secondInserted,
		"second pass inserted %d new event(s) into a store that already held the first pass — "+
			"ExternalIDs are not stable across passes.\nfirst: %s\nsecond: %s",
		secondInserted, inventory(first), inventory(second))

	firstIDs := make(map[string]bool, len(first))
	for _, evt := range first {
		firstIDs[evt.ExternalID] = true
	}

	for _, evt := range second {
		assert.True(t, firstIDs[evt.ExternalID],
			"second pass emitted %s, which the first pass did not", evt.ExternalID)
	}
}
