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
func TestMain(m *testing.M) {
	if err := envfile.Load(); err != nil {
		fmt.Fprintf(os.Stderr, "loading .env: %v\n", err)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

// testCredential resolves this tier's credential from UNJIRA_JIRA_CREDENTIALS.
//
// A missing var skips (this tier is simply not configured to run); a
// malformed var fails loudly via require.NoError, since a broken credential
// blob is a misconfiguration to fix, not a reason to silently skip.
func testCredential(t *testing.T) credentials.Credential {
	t.Helper()

	set, found, err := credentials.FromEnv()
	require.NoError(t, err)
	if !found {
		t.Skipf("set %s to run", credentials.EnvVar)
	}

	name := liveConnectionName()
	cred, ok := set.For(name)
	if !ok {
		t.Skipf("no credential for connection %q in %s", name, credentials.EnvVar)
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

	var keys []string
	for _, p := range projects {
		keys = append(keys, p["key"].(string))
	}
	assert.Contains(t, keys, project)
}

// TestIssueLifecycleRoundtrip exercises create -> transition -> comment ->
// changelog -> delete, asserting each hop.
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
	t.Cleanup(func() {
		_ = client.DeleteIssue(key)
	})

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

// liveCollectContext builds a CollectContext pointing at the dev instance,
// scoped to a single issue key so the test cannot be perturbed by unrelated
// activity in the project.
//
// The store is a fresh temp-file SQLite DB per call so a cursor from a previous
// run cannot make a pass look incremental when it should be a full scan.
func liveCollectContext(t *testing.T, issueKey string) pipeline.CollectContext {
	t.Helper()

	site := os.Getenv("UNJIRA_JIRA_SITE")
	if site == "" {
		site = "https://unjira.atlassian.net"
	}

	s, err := store.Open(filepath.Join(t.TempDir(), "unjira.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	connectionName := liveConnectionName()
	cred := testCredential(t)

	return pipeline.CollectContext{
		Store: s,
		Config: config.Config{Jira: []config.JiraConnection{{
			Name:        connectionName,
			Site:        site,
			ProjectKeys: []string{testProject()},
			Queries: []config.JiraQuery{
				{Name: "probe", JQL: fmt.Sprintf("key = %s", issueKey)},
			},
		}}},
		Credentials: credentials.NewSet(map[string]credentials.Credential{
			connectionName: cred,
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
// then returns every event it emitted on that successful pass.
//
// Jira's search index is eventually consistent — measured at roughly 3 seconds
// on this instance — AND non-monotonic: a document can become findable and then
// transiently stop being findable while replicas converge. That second property
// is why a one-shot readiness probe before collecting is not enough, and why
// this retries the collector itself rather than pre-checking. A probe can only
// ever report that the index WAS ready.
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
func collectUntilMatched(
	t *testing.T, collector pipeline.Collector, cc pipeline.CollectContext, opts ...backoff.RetryOption,
) []events.Event {
	t.Helper()

	// Exponential with jitter, from the library rather than hand-rolled: its
	// defaults (500ms initial, 1.5x, RandomizationFactor 0.5) clear the measured
	// ~3.3s lag in about four tries, where a fixed 1s poll spends four full
	// seconds to learn the same thing.
	//
	// Try count and elapsed time are bounded INDEPENDENTLY on purpose. Tries bound
	// how much this costs; elapsed time bounds how long a human or CI waits. A
	// slow Jira should fail on the clock rather than after N slow attempts.
	settings := append([]backoff.RetryOption{
		backoff.WithMaxTries(8),
		backoff.WithMaxElapsedTime(45 * time.Second),
	}, opts...)

	// Captured across attempts so the failure message can distinguish "never
	// matched" from "matched but emitted nothing".
	var attempts int

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
			return nil, errors.New("collector matched nothing")
		}

		return collected, nil
	}, settings...)

	if err != nil {
		// BOTH possibilities named, because only one of them is Jira's fault. If
		// this ever fires for the second reason, the retry has not masked a
		// regression — it has reported one.
		t.Fatalf("collector matched nothing across %d attempts: either Jira's search "+
			"index is unusually far behind, or the collector genuinely does not match "+
			"this issue (a real regression). Last error: %v", attempts, err)
	}

	return got
}

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

	key, err := client.CreateIssue(
		testProject(),
		"[seed] live collector test issue",
		"Task",
		"Created by internal/live; deleted by this test's cleanup.",
		[]string{jira.SeedLabel},
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.DeleteIssue(key) })

	// Transition it so there is a status changelog entry to collect.
	transitions, err := client.GetTransitions(key)
	require.NoError(t, err)
	require.NotEmpty(t, transitions)
	targetID, _ := transitions[0]["id"].(string)
	require.NoError(t, client.TransitionIssue(key, targetID, nil))

	const commentBody = "live collector probe comment"
	_, err = client.AddComment(key, commentBody)
	require.NoError(t, err)

	// The collector finds issues by JQL, and Jira's search index lags issue
	// creation by a few seconds. Without retrying, Collect matches nothing, `got`
	// is empty, and the assertion loop below simply never executes — so the
	// sawComment/sawStatus flags carry the whole test. The require calls after the
	// loop are what keep an empty collection from reading as success.
	cc := liveCollectContext(t, key)
	got := collectUntilMatched(t, collectorjira.New(), cc)

	require.NotEmpty(t, got,
		"the collector matched no events at all; every per-event assertion below would "+
			"vacuously pass on an empty slice")

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

	key, err := client.CreateIssue(
		testProject(),
		"[seed] live collector watermark test issue",
		"Task",
		"Created by internal/live; deleted by this test's cleanup.",
		[]string{jira.SeedLabel},
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.DeleteIssue(key) })

	_, err = client.AddComment(key, "watermark probe")
	require.NoError(t, err)

	cc := liveCollectContext(t, key)
	collector := collectorjira.New()

	// First pass: no cursor, so a plain unbounded query. This is what every
	// offline test already covers.
	//
	// Retried rather than run once, because the collector storing a watermark is
	// this test's PRECONDITION, not its subject. Jira's index is eventually
	// consistent AND non-monotonic (measured ~3.3s lag; observed to flap), so a
	// single pass can match nothing, store no watermark, and leave the second pass
	// silently degraded into another unbounded query — the assertion below would
	// then fail for a reason that has nothing to do with the JQL quoting this test
	// exists to prove. That is exactly how this test failed intermittently in CI.
	collectUntilMatched(t, collector, cc)

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
	var secondPass []events.Event
	err = collector.Collect(cc, func(e events.Event) { secondPass = append(secondPass, e) })
	require.NoError(t, err)

	require.NotEmpty(t, secondPass,
		"the watermark-bounded query matched nothing, but the issue's updated time is at or after "+
			"the watermark. Jira returns 200-with-zero-results for a bad date literal rather than "+
			"400, so this is what a wrong `updated >= %%q` format in collectQuery looks like — and "+
			"it is invisible to every offline test.")
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
	t.Cleanup(func() { _ = client.DeleteIssue(key) })

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
	t.Cleanup(func() { _ = client.DeleteIssue(key) })

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
	t.Cleanup(func() { _ = client.DeleteIssue(key) })

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
	t.Cleanup(func() { _ = client.DeleteIssue(key) })

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
	t.Cleanup(func() { _ = client.DeleteIssue(key) })

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
