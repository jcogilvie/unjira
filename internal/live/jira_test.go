//go:build live

// Package live holds integration tests against the dev Jira instance.
//
// Gated twice: the "live" build tag keeps this file out of `go test ./...`
// entirely (it does not even compile), and the UNJIRA_LIVE=1 check makes the
// intent explicit — these tests WRITE to the instance (and clean up after
// themselves). Locally: UNJIRA_LIVE=1 go test -tags=live ./internal/live/...
// In CI: the required "integration" job (see
// docs/superpowers/specs/2026-08-07-go-port-design.md).
package live

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cenkalti/backoff/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/clients/jira"
	collectorjira "github.com/jcogilvie/unjira/internal/collector/jira"
	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/credentials"
	"github.com/jcogilvie/unjira/internal/envfile"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/pipeline"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
	"github.com/jcogilvie/unjira/internal/workflow"
)

// liveConnectionNameEnvVar overrides liveConnectionName, defaulting to "dev".
// Kept overridable rather than hardcoded because the connection name must key
// both the UNJIRA_JIRA_CREDENTIALS lookup and the config.JiraConnection.Name
// this test builds, and someone pointing this tier at a differently-named
// connection in their own UNJIRA_JIRA_CREDENTIALS (e.g. a shared credentials
// blob also used by `unjira collect`) should not have to edit this file to do
// it.
const liveConnectionNameEnvVar = "UNJIRA_LIVE_JIRA_CONNECTION"

// liveConnectionName returns the Jira connection name this tier authenticates
// as: the key into UNJIRA_JIRA_CREDENTIALS and the config.JiraConnection.Name
// used to build a CollectContext.
func liveConnectionName() string {
	if name := os.Getenv(liveConnectionNameEnvVar); name != "" {
		return name
	}

	return "dev"
}

// TestMain loads .env before any test reads an environment variable.
//
// This must happen here rather than inside a per-test helper. Loading it lazily
// (from testCredential, say) is a real bug that this tier hit: testClient reads
// UNJIRA_JIRA_SITE *before* it asks for a credential, so a late Load left the
// site at its unjira.atlassian.net default while the credentials came from
// .env — pointing a correctly-authenticated client at the wrong instance, where
// the configured project does not exist. The failure surfaced as "The target
// project doesn't exist or you don't have permission to create issues in it",
// which reads like a permissions problem and is not one.
//
// Load walks up to the repository root, which `go test ./internal/live/` needs:
// the test binary runs with CWD set to the package directory, not the repo root.
//
// It also deletes the collector tests' shared issue (sharedCollectorIssue), which
// outlives any single test and so cannot use t.Cleanup.
func TestMain(m *testing.M) {
	if err := envfile.Load(); err != nil {
		fmt.Fprintf(os.Stderr, "loading .env: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()
	deleteCollectorFixture()
	os.Exit(code)
}

// testCredential resolves this tier's credential from UNJIRA_JIRA_CREDENTIALS.
//
// Every caller reaches this only after UNJIRA_LIVE=1 has been checked, so a
// missing credential here means someone ASKED for the live run and cannot get
// one. That fails rather than skips: a skipped live run prints PASS, which reads
// as "the live tier passed" when nothing ran — design-notes #37. A malformed var
// fails too, via require.NoError. The "this tier is not configured" case is
// UNJIRA_LIVE being unset, which testClient already skips on.
func testCredential(t *testing.T) credentials.Credential {
	t.Helper()

	set, found, err := credentials.FromEnv()
	require.NoError(t, err)
	if !found {
		t.Fatalf("UNJIRA_LIVE=1 but %s is unset (checked the environment and .env): "+
			"this is a FAILURE, not a skip, since a skipped live run reads as a passing one", credentials.EnvVar)
	}

	name := liveConnectionName()
	cred, ok := set.For(name)
	if !ok {
		t.Fatalf("UNJIRA_LIVE=1 but %s has no credential for connection %q", credentials.EnvVar, name)
	}

	return cred
}

func testClient(t *testing.T) *jira.Client {
	t.Helper()

	if os.Getenv("UNJIRA_LIVE") != "1" {
		t.Skip("set UNJIRA_LIVE=1 to run")
	}

	site := os.Getenv("UNJIRA_JIRA_SITE")
	if site == "" {
		site = "https://unjira.atlassian.net"
	}

	cred := testCredential(t)

	client, err := jira.New(site, cred.Email, cred.Token)
	require.NoError(t, err)

	return client
}

func testProject() string {
	if project := os.Getenv("UNJIRA_LIVE_PROJECT"); project != "" {
		return project
	}

	return "SCRUM"
}

func TestAuthAndProjectVisible(t *testing.T) {
	client := testClient(t)
	project := testProject()

	me, err := client.Myself()
	require.NoError(t, err)
	assert.NotEmpty(t, me["accountId"])

	projects, err := client.SearchProjects()
	require.NoError(t, err)

	keys := make([]string, 0, len(projects))
	for _, p := range projects {
		keys = append(keys, p["key"].(string))
	}
	assert.Contains(t, keys, project)
}

// TestIssueLifecycleRoundtrip exercises create -> transition -> comment ->
// changelog -> delete, asserting each hop but the last: the delete is this test's
// cleanup, so a failure there is reported rather than asserted (cleanup_test.go).
func TestIssueLifecycleRoundtrip(t *testing.T) {
	client := testClient(t)
	project := testProject()

	key, err := client.CreateIssue(
		project,
		"[seed] live roundtrip test issue",
		"Task",
		"Created by internal/live; deleted by this test's cleanup.",
		[]string{jira.SeedLabel},
	)
	require.NoError(t, err)
	deleteIssueOnCleanup(t, client, key)

	transitions, err := client.GetTransitions(key)
	require.NoError(t, err)
	require.NotEmpty(t, transitions, "expected at least one legal transition from the initial status")

	target := transitions[0]
	targetID, _ := target["id"].(string)
	targetTo, _ := target["to"].(map[string]any)
	targetToName, _ := targetTo["name"].(string)

	require.NoError(t, client.TransitionIssue(key, targetID, nil))

	refreshed, err := client.GetIssue(key, "")
	require.NoError(t, err)
	fields, _ := refreshed["fields"].(map[string]any)
	status, _ := fields["status"].(map[string]any)
	assert.Equal(t, targetToName, status["name"])

	_, err = client.AddComment(key, "Live-test comment.\n\nSecond paragraph survives the trip.")
	require.NoError(t, err)

	changes, err := client.StatusChanges(key)
	require.NoError(t, err)

	var foundTarget bool
	for _, c := range changes {
		if c.To == targetToName {
			foundTarget = true
			break
		}
	}
	assert.True(t, foundTarget)
}

func TestWorkflowMiningProducesAGraph(t *testing.T) {
	client := testClient(t)
	project := testProject()

	graph, err := workflow.MineProject(client, project, 50)
	require.NoError(t, err)

	categories := graph.StatusCategories()
	assert.NotEmpty(t, categories, "project should report at least one status")
}

// probeConnection is the connection every collector test runs: the dev instance,
// scoped to a single issue key so the test cannot be perturbed by unrelated
// activity in the project. It is shared by liveCollectContext and the index probes
// (index_test.go). The probes judge "the collector's own query", and that only
// holds while both are built from this one definition.
func probeConnection(issueKey string) config.JiraConnection {
	site := os.Getenv("UNJIRA_JIRA_SITE")
	if site == "" {
		site = "https://unjira.atlassian.net"
	}

	return config.JiraConnection{
		Name:        liveConnectionName(),
		Site:        site,
		ProjectKeys: []string{testProject()},
		Queries:     []config.JiraQuery{probeQuery(issueKey)},
	}
}

func probeQuery(issueKey string) config.JiraQuery {
	return config.JiraQuery{Name: "probe", JQL: fmt.Sprintf("key = %s", issueKey)}
}

// liveCollectContext builds a CollectContext for probeConnection.
//
// The store is a fresh temp-file SQLite DB per call so a cursor from a previous
// run cannot make a pass look incremental when it should be a full scan.
func liveCollectContext(t *testing.T, issueKey string) pipeline.CollectContext {
	t.Helper()

	s, err := store.Open(filepath.Join(t.TempDir(), "unjira.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	conn := probeConnection(issueKey)
	cred := testCredential(t)

	return pipeline.CollectContext{
		Store:  s,
		Config: config.Config{Jira: []config.JiraConnection{conn}},
		Credentials: credentials.NewSet(map[string]credentials.Credential{
			conn.Name: cred,
		}),
		Options: map[string]any{},
	}
}

// USE THIS whenever a live test CREATES data and then expects a COLLECTOR to
// find it. Fetching by key (GetIssue) needs none of this — that reads the
// document store and is immediately consistent. It is the search index that
// lags, so the hazard is specific: create-then-SEARCH is racy, create-then-
// FETCH-BY-KEY is not. See docs/design-notes.md incident 26.
//
// collectUntilMatched runs collector until its query actually matches something,
// then returns every event it emitted on that successful pass. key is the issue
// the collector's query targets, and the verdict on failure is about it.
//
// Jira's search index is eventually consistent AND non-monotonic. Atlassian
// documents the lag as "from a few seconds to minutes". It is measured here at a
// ~3s median with a tail past 80s (incidents 26 and 42). A document can also
// become findable and then transiently stop being findable while replicas
// converge. So there are TWO waits, sized separately:
//
//   - for the index to have the issue at all: sharedCollectorIssue's awaitIndexed,
//     long (minutes) and cheap. Callers obtain key from it, so this helper starts
//     only once the index has returned the issue at least once.
//   - for a transient flap after that: this helper's retry around the collector
//     ITSELF, short. A probe cannot replace it, since a probe only reports that
//     the index WAS ready (incident 26).
//
// Keeping the waits apart lets lag tolerance and bug-detection latency be sized
// independently. A real watermark bug fails every attempt identically, so it is
// reported after THIS budget (~46s), never after the minutes-long readiness wait.
//
// Only the create-then-search seam is retried. Assertions are not: they still
// fail on the first wrong answer, because this tier exists to catch collector
// regressions and a blanket retry would mask exactly those.
//
// Production does not have this race, so this is a test-framework concern rather
// than a missing production retry: watch's pass collects FIRST and applies LAST
// (cmd/unjira/main.go's runWatchPass), so a write and the next search are a full
// interval apart — 5 minutes by default against a ~3 second lag. And the
// reconciler deliberately DISCARDS unjira's own writes (dropSelfAuthored), so a
// self-authored event arriving late is the desired outcome either way. A pass
// that matches nothing simply leaves the watermark alone and retries next pass,
// which is correct behaviour, not a failure.
// client is taken so a failure can DISCRIMINATE rather than merely report (see
// classifyIndexMiss). It is passed in rather than rebuilt from cc.Credentials
// because every caller already has one, and reconstructing it would add a second
// way for this helper to fail.
func collectUntilMatched(
	t *testing.T, client *jira.Client, collector pipeline.Collector, cc pipeline.CollectContext,
	key string, opts ...backoff.RetryOption,
) []events.Event {
	t.Helper()

	// Exponential with jitter, from the library's defaults: 500ms initial, 1.5x,
	// RandomizationFactor 0.5.
	//
	// N tries is N-1 WAITS. 10 tries sleeps 0.5 x (1.5^9 - 1)/0.5 = ~37s on
	// average. Add ~1s per collector pass (Myself, search, changelog, comments) and
	// the whole budget is ~46s, give or take jitter. The three captured CI failures
	// took 36s to 46s per test, seeding included. An earlier version of this comment
	// put the budget at "~57s" by counting tries as waits. That overstated the
	// margin past the 24s tail by half.
	//
	// This budget is for FLAPS, not lag. Callers reach it only after the index has
	// returned the issue (awaitIndexed). It is also the latency of a REAL BUG
	// verdict for a watermark bug, which fails every attempt identically. Keep it
	// short. Lag tolerance belongs to indexReadinessBound.
	//
	// Try count and elapsed time are bounded INDEPENDENTLY. Tries bound cost and are
	// what normally binds (~46s). 90s elapsed is the backstop for a pathologically
	// slow Jira.
	started := time.Now()

	var attempts int

	settings := append([]backoff.RetryOption{
		backoff.WithMaxTries(10),
		backoff.WithMaxElapsedTime(90 * time.Second),
		backoff.WithNotify(func(err error, next time.Duration) {
			t.Logf("collector: attempt %d at %s: %v; next attempt in %s",
				attempts, time.Since(started).Round(time.Millisecond), err, next.Round(time.Millisecond))
		}),
	}, opts...)

	got, err := backoff.Retry(t.Context(), func() ([]events.Event, error) {
		attempts++

		var collected []events.Event
		if err := collector.Collect(cc, func(e events.Event) {
			collected = append(collected, e)
		}); err != nil {
			// A transport or config error is not an index race and will not fix
			// itself — stop immediately rather than burning the whole budget on
			// a failure that is already decided.
			return nil, backoff.Permanent(err)
		}

		if len(collected) == 0 {
			return nil, errCollectorMatchedNothing
		}

		return collected, nil
	}, settings...)
	if err == nil {
		t.Logf("collector: matched on attempt %d after %s", attempts, time.Since(started).Round(time.Millisecond))

		return got
	}

	// The collector ERRORED rather than matching nothing: that is a failure in its
	// own right, and the index probes cannot speak to it. Classifying it by issue
	// visibility, as an earlier version did, could call a collector parse error
	// "INFRASTRUCTURE" whenever the index happened to be lagging.
	if !errors.Is(err, errCollectorMatchedNothing) {
		t.Fatalf("collector FAILED on attempt %d after %s (not an index miss, so no index "+
			"verdict applies): %v", attempts, time.Since(started).Round(time.Millisecond), err)
	}

	// Captured from a real failure: stored watermark 2026-09-09T12:46:49.711-07:00
	// against a live updated of exactly 2026-09-09T12:46:49.711-0700, so the
	// minute-floored bound provably included the issue. Without a verdict that
	// evidence reads as ambiguous, and an OSS contributor cannot tell whether their
	// PR broke something.
	conn := cc.Config.Jira[0]
	unbounded, jqlErr := conn.EffectiveJQL(conn.Queries[0])

	ev := indexEvidence{jqlErr: jqlErr}
	if jqlErr == nil {
		ev = gatherFreshEvidence(client, unbounded, key)
		ev.scopedIndexed = probe(client, unbounded, nil)
	}

	failWithVerdict(t, classifyIndexMiss(ev), fmt.Sprintf(
		"collector matched nothing across %d attempts in %s.\n"+
			"  unbounded JQL: %q (err: %v)\n"+
			"  consistent read of it: matched=%v err=%v; bare-key control: matched=%v err=%v\n"+
			"  index read of it:      matched=%v err=%v",
		attempts, time.Since(started).Round(time.Millisecond), unbounded, jqlErr,
		ev.scopedFresh.matched, ev.scopedFresh.err, ev.keyFresh.matched, ev.keyFresh.err,
		ev.scopedIndexed.matched, ev.scopedIndexed.err))

	return nil
}

// errCollectorMatchedNothing is collectUntilMatched's one retryable condition.
var errCollectorMatchedNothing = errors.New("collector matched nothing")

// TestLiveCollectorSeesSeededCommentAndTransition is the first real check that
// this collector's JSON-shape assumptions match Jira's actual responses. The
// offline fakes encode what we *believe* Jira returns; only this can establish
// that the belief is right.
//
// The authored_by_unjira assertion is the highest-value part: no fixture can
// verify it, since it depends on the real Myself() accountId matching the real
// comment author.
func TestLiveCollectorSeesSeededCommentAndTransition(t *testing.T) {
	client := testClient(t)

	// The run's shared issue: created, transitioned once (so there is a status
	// changelog entry to collect) and commented once, then waited on until the
	// search index returns it. See sharedCollectorIssue for why it is shared.
	key := sharedCollectorIssue(t, client)
	commentBody := collectorFixtureCommentBody

	// The collector finds issues by JQL, and Jira's search index lags writes.
	// Without retrying, Collect matches nothing, `got` is empty, and the assertion
	// loop below simply never executes — so the sawComment/sawStatus flags carry the
	// whole test. collectUntilMatched fatals if nothing matched, with a verdict that
	// distinguishes index lag from a real regression, so no require.NotEmpty is
	// needed here. The sawComment/sawStatus requires at the end remain load-bearing:
	// they are what stop a non-empty-but-wrong collection from passing vacuously.
	cc := liveCollectContext(t, key)
	got := collectUntilMatched(t, client, collectorjira.New(), cc, key)

	var sawComment, sawStatus bool
	for _, e := range got {
		assert.Equal(t, "jira", e.Source)

		switch {
		case strings.HasPrefix(e.ExternalID, key+":comment:"):
			sawComment = true
			assert.Contains(t, e.Summary, commentBody)
			assert.Equal(t, true, e.Artifacts["authored_by_unjira"],
				"we posted this comment, so the tag must be set against the live accountId")
		case strings.HasPrefix(e.ExternalID, key+":status:"):
			sawStatus = true
			assert.Equal(t, true, e.Artifacts["authored_by_unjira"],
				"we performed this transition")
		}

		assert.Equal(t, key, e.Artifacts["issue_key"])
		assert.Equal(t, testProject(), e.Artifacts["project_key"])
		assert.Equal(t, liveConnectionName(), e.Artifacts["connection"])
		assert.False(t, e.OccurredAt.IsZero(), "OccurredAt must parse from Jira's timestamp format")
	}

	assert.True(t, sawComment, "the comment we just posted must appear as an event")
	assert.True(t, sawStatus, "the transition we just performed must appear as an event")
}

// TestLiveCollectorSecondPassWatermarkJQLIsAccepted is the reason this task
// exists. After a successful first pass the collector stores a watermark and
// appends `AND updated >= "YYYY-MM-DD HH:MM"` to the query on every later pass.
// Nothing offline can prove Jira accepts that double-quoted date literal — the
// fake records the JQL string without parsing it. If the quoting is wrong,
// every incremental pass fails against real Jira while all offline tests stay
// green.
func TestLiveCollectorSecondPassWatermarkJQLIsAccepted(t *testing.T) {
	client := testClient(t)

	// The run's shared, already-indexed issue (sharedCollectorIssue). This test
	// needs only that the issue was updated recently and carries something to
	// collect, which the shared seed provides. It does not write to the issue: the
	// fixture's immutability is what keeps the two tests independent.
	key := sharedCollectorIssue(t, client)

	// A fresh store, so this test's first pass is unbounded regardless of what the
	// sibling test's collection stored in ITS store.
	cc := liveCollectContext(t, key)
	collector := collectorjira.New()

	// First pass: no cursor, so a plain unbounded query. This is what every
	// offline test already covers.
	//
	// Retried rather than run once, because the collector storing a watermark is
	// this test's PRECONDITION, not its subject. Jira's index is eventually
	// consistent AND non-monotonic (incidents 26, 42), so a single pass can match
	// nothing, store no watermark, and leave the second pass silently degraded into
	// another unbounded query — the assertion below would then fail for a reason
	// that has nothing to do with the JQL quoting this test exists to prove.
	collectUntilMatched(t, client, collector, cc, key)

	position, err := cc.Store.GetCursor("jira", collectorjira.CursorResource(liveConnectionName(), "probe"))
	require.NoError(t, err)
	require.NotEmpty(t, position,
		"a successful first pass must store a watermark, or the second pass proves nothing")

	// Second pass: the stored watermark is now decoded and rendered into the
	// JQL.
	//
	// Asserting only NoError here would be nearly worthless, which is worth
	// spelling out because the obvious version of this test is exactly that.
	// Measured against this instance: Jira answers HTTP 200 with an EMPTY result
	// set for a malformed date literal (`updated >= "totally-not-a-date"`), for
	// an RFC3339 literal, and even for an unknown field name — it reserves 400
	// for structural syntax errors like an unclosed paren. So a wrong date
	// format would not raise; the collector would quietly match nothing on every
	// incremental pass, forever, while reporting success.
	//
	// The only assertion that discriminates is that the watermarked query still
	// MATCHES the issue. The watermark is floored to the minute and the issue
	// was just updated, so `updated >= <watermark>` must include it.
	//
	// Retried for the same reason the first pass is: the index can transiently lose
	// the issue between two searches seconds apart, and this pass searches too.
	// Captured evidence from a real failure — stored watermark
	// 2026-09-09T12:46:49.711-07:00 against a live updated of exactly
	// 2026-09-09T12:46:49.711-0700, so the floored bound `>= 12:46` provably
	// included it — showed the query was correct and the index simply did not
	// return the row. Leaving this pass un-retried is what let that reach CI.
	//
	// The retry does NOT weaken what this test proves. A wrong date literal fails
	// every attempt identically (Jira is deterministic about a bound it cannot
	// use), so the watermarkClause bug this exists to catch still fails loudly —
	// only the convergence race is absorbed.
	secondPass := collectUntilMatched(t, client, collector, cc, key)

	// collectUntilMatched already fatals on an empty collection, so this asserts
	// the stronger property: the watermarked query matched THIS issue, not merely
	// something. A bound that silently widened to match unrelated rows would
	// otherwise pass.
	var sawSubject bool
	for _, e := range secondPass {
		if strings.HasPrefix(e.ExternalID, key+":") {
			sawSubject = true

			break
		}
	}
	require.True(t, sawSubject,
		"the watermark-bounded query returned events, but none for %s — the bound matched "+
			"something other than the issue this pass is about", key)
}

// TestLiveMatchingSignalIsAvailable verifies the two things matching depends on
// that no fixture can establish: that GetIssue resolves a real key through the
// tasktracker seam, and that Description actually arrives populated.
//
// Description is the reason tasktracker.Issue gained the field — the Jira
// collector deliberately never emits description snapshots as events, so a
// never-edited ticket's body is available only on this live read path. If Jira
// returns Atlassian Document Format here rather than a string, normalizeIssue's
// type assertion leaves it empty and matching loses its strongest signal
// silently. This test is what makes that visible.
//
// Measured against sumologic.atlassian.net: whether the body survives depends
// entirely on the REST API version. rest/api/2 returns description as a plain
// string; rest/api/3 returns an ADF object ({type, version, content}), which
// normalizeIssue's string assertion drops to "". Client.GetIssue uses
// rest/api/2, so this passes — but the client is already MIXED (every call is
// v2 except rest/api/3/search/jql), which makes migrating GetIssue to v3 a
// plausible edit that would silently blind matching.
//
// TestLiveGetIssueUsesAStringDescriptionAPI below pins that dependency
// explicitly, so the breakage announces itself instead of showing up as
// mysteriously worse matching.
func TestLiveMatchingSignalIsAvailable(t *testing.T) {
	client := testClient(t)
	tracker := jira.NewTracker(client)

	const body = "Live matching probe: this body is the signal matching compares against."

	key, err := client.CreateIssue(
		testProject(),
		"[seed] live matching signal probe",
		"Task",
		body,
		[]string{jira.SeedLabel},
	)
	require.NoError(t, err)
	deleteIssueOnCleanup(t, client, key)

	issue, err := tracker.GetIssue(key)

	require.NoError(t, err, "a key we just created must resolve — this is what verification relies on")
	assert.Equal(t, key, issue.Key)
	assert.Equal(t, "[seed] live matching signal probe", issue.Summary)
	assert.Equal(t, body, issue.Description,
		"Description must survive normalizeIssue; if this fails, Jira returned ADF rather than a "+
			"string and matching loses its strongest signal")
	assert.NotEmpty(t, issue.StatusCategory, "status is part of the classification prompt")
}

// TestLiveGetIssueUsesAStringDescriptionAPI pins matching's dependency on the
// REST API version, because that dependency is invisible at the call site.
//
// Measured on this instance: rest/api/2 returns `description` as a string,
// rest/api/3 returns an ADF object. normalizeIssue asserts `.(string)` and
// leaves Description empty on failure — a deliberate degradation, not a bug,
// since rendering ADF to text is out of scope. But it means a v3 migration
// would silently blind narrative→issue matching: no error, no failing offline
// test, just worse attribution. The client is already mixed (v2 everywhere
// except rest/api/3/search/jql), so that edit is plausible rather than
// far-fetched.
//
// This asserts the shape directly rather than through normalizeIssue, so a
// failure names the actual cause instead of surfacing as an empty field.
func TestLiveGetIssueUsesAStringDescriptionAPI(t *testing.T) {
	client := testClient(t)

	const body = "Plain-string body: this must not arrive as an ADF object."

	key, err := client.CreateIssue(
		testProject(),
		"[seed] description api-version probe",
		"Task",
		body,
		[]string{jira.SeedLabel},
	)
	require.NoError(t, err)
	deleteIssueOnCleanup(t, client, key)

	raw, err := client.GetIssue(key, "")
	require.NoError(t, err)

	fields, ok := raw["fields"].(map[string]any)
	require.True(t, ok, "issue payload has no fields object")

	description, ok := fields["description"].(string)
	require.True(t, ok,
		"description arrived as %T, not a string — GetIssue is on a REST API version that returns "+
			"Atlassian Document Format, so normalizeIssue drops the body and matching silently "+
			"loses its strongest signal. Either keep GetIssue on rest/api/2 or teach normalizeIssue "+
			"to render ADF.",
		fields["description"])
	assert.Equal(t, body, description)
}

// TestLiveUnresolvableKeyIsNotTransport confirms the classification
// verifyCandidates depends on.
//
// A stale branch name or a hallucinated key must DROP the candidate (recorded
// as unresolved, pass continues), not fail the narrative. That hinges on
// correlator.IsTransportError reading a real Jira 404 as not-found. The offline
// tests assert this against a hand-built &jira.Error{Status: 404}; only a live
// call proves the real client produces that shape.
func TestLiveUnresolvableKeyIsNotTransport(t *testing.T) {
	client := testClient(t)
	tracker := jira.NewTracker(client)

	_, err := tracker.GetIssue(testProject() + "-99999999")

	require.Error(t, err, "a nonexistent key must error rather than returning a zero Issue")
	assert.False(t, correlator.IsTransportError(err),
		"a 404 must classify as not-found: misread as transport, a stale branch name would fail "+
			"its narrative on every pass instead of being reported unresolved")
}

// TestLiveAvailableTransitionsReflectsTheRealWorkflow is why
// AvailableTransitions exists. The offline fakes assert what we BELIEVE Jira
// reports as legal; only a live call establishes that a freshly-created issue
// genuinely cannot reach every status, which is what makes floorConfidence's
// transition check meaningful rather than vacuous.
//
// Asserted as a property, not against a hardcoded status list: a Jira project's
// workflow is admin-configurable, so pinning "a new Task can go to In Progress"
// would be pinning this instance's configuration rather than the behaviour under
// test.
func TestLiveAvailableTransitionsReflectsTheRealWorkflow(t *testing.T) {
	client := testClient(t)
	tracker := jira.NewTracker(client)

	key, err := client.CreateIssue(
		testProject(),
		"[seed] live transition gating probe",
		"Task",
		"Created by internal/live; deleted by this test's cleanup.",
		[]string{jira.SeedLabel},
	)
	require.NoError(t, err)
	// Deletes exactly the key this test created. Never query-driven cleanup
	// against the shared sandbox — a JQL sweep can delete issues this test did
	// not create.
	deleteIssueOnCleanup(t, client, key)

	transitions, err := tracker.AvailableTransitions(key)
	require.NoError(t, err)

	require.NotEmpty(t, transitions,
		"a new issue must offer at least one legal transition, or every transition proposal "+
			"would be floored to zero and this check would be vacuous")

	// Cross-check against the raw API: every NAME reported must trace back to a
	// real transition Jira offers. This is what catches a normalization bug that
	// invented a destination.
	raw, err := client.GetTransitions(key)
	require.NoError(t, err)

	rawNames := make(map[string]bool, len(raw))
	for _, transition := range raw {
		to, ok := transition["to"].(map[string]any)
		require.True(t, ok, "a transition with no 'to' object: %v", transition)
		if name, _ := to["name"].(string); name != "" {
			rawNames[name] = true
		}
	}

	require.NotEmpty(t, rawNames,
		"GetTransitions returned transitions but none carried a to.name — the shape "+
			"AvailableTransitions depends on has changed")

	for _, transition := range transitions {
		assert.NotEmpty(t, transition.ToStatus,
			"a nameless destination cannot be targeted, since the name IS the target")
		assert.True(t, rawNames[transition.ToStatus],
			"reported destination %q is not one Jira offered — normalization invented it",
			transition.ToStatus)
	}

	assert.LessOrEqual(t, len(transitions), len(rawNames)+1,
		"destinations are deduplicated by name, so there cannot be more of them than "+
			"distinct names Jira offered")

	// The load-bearing property this whole change exists for: names must survive
	// as distinct values even when they share a category. Asserted only when the
	// live workflow actually offers such a pair, so it never fails on a project
	// whose configuration happens not to.
	byCategory := make(map[tasktracker.StatusCategory][]string)
	for _, transition := range transitions {
		byCategory[transition.ToCategory] = append(byCategory[transition.ToCategory], transition.ToStatus)
	}
	for category, names := range byCategory {
		if len(names) < 2 {
			continue
		}
		assert.Len(t, uniqueStrings(names), len(names),
			"category %q holds %v: these must stay distinguishable, which is exactly what "+
				"the old category-keyed result could not do", category, names)
	}
}

// TestLiveSetStatusMovesToTheNamedStatus is the write-side half, and the only
// place the name-matching claim is checked against a real workflow rather than a
// fake's canned response.
func TestLiveSetStatusMovesToTheNamedStatus(t *testing.T) {
	client := testClient(t)
	tracker := jira.NewTracker(client)

	key, err := client.CreateIssue(
		testProject(),
		"[seed] live named transition probe",
		"Task",
		"Created by internal/live; deleted by this test's cleanup.",
		[]string{jira.SeedLabel},
	)
	require.NoError(t, err)
	deleteIssueOnCleanup(t, client, key)

	transitions, err := tracker.AvailableTransitions(key)
	require.NoError(t, err)
	require.NotEmpty(t, transitions)

	// Whatever this workflow offers first — not a hardcoded status, since the
	// project's workflow is admin-configurable.
	target := transitions[0].ToStatus

	require.NoError(t, tracker.SetStatus(key, target))

	moved, err := tracker.GetIssue(key)
	require.NoError(t, err)
	assert.Equal(t, target, moved.StatusName,
		"the issue must land on the status that was NAMED; landing on a different status "+
			"with the same category is the exact wrong write this change fixes")
}

// TestLiveSetStatusRefusesAnUnofferedName: the error path, live. A fake can be
// made to return anything; only a real call proves Jira does not quietly accept
// an unknown target and pick something.
func TestLiveSetStatusRefusesAnUnofferedName(t *testing.T) {
	client := testClient(t)
	tracker := jira.NewTracker(client)

	key, err := client.CreateIssue(
		testProject(),
		"[seed] live unoffered transition probe",
		"Task",
		"Created by internal/live; deleted by this test's cleanup.",
		[]string{jira.SeedLabel},
	)
	require.NoError(t, err)
	deleteIssueOnCleanup(t, client, key)

	before, err := tracker.GetIssue(key)
	require.NoError(t, err)

	err = tracker.SetStatus(key, "Definitely Not A Real Status")

	require.Error(t, err)
	assert.Contains(t, err.Error(), key)

	after, err := tracker.GetIssue(key)
	require.NoError(t, err)
	assert.Equal(t, before.StatusName, after.StatusName,
		"a refused transition must leave the issue where it was")
}

// uniqueStrings returns in with duplicates removed, preserving order.
func uniqueStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}

	return out
}
