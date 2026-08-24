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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/clients/jira"
	collectorjira "github.com/jcogilvie/unjira/internal/collector/jira"
	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/credentials"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/pipeline"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/workflow"
)

func testClient(t *testing.T) *jira.Client {
	t.Helper()

	if os.Getenv("UNJIRA_LIVE") != "1" {
		t.Skip("set UNJIRA_LIVE=1 to run")
	}

	site := os.Getenv("UNJIRA_JIRA_SITE")
	if site == "" {
		site = "https://unjira.atlassian.net"
	}

	email := os.Getenv("UNJIRA_JIRA_EMAIL")
	token := os.Getenv("UNJIRA_JIRA_TOKEN")
	if email == "" || token == "" {
		t.Skip("UNJIRA_JIRA_EMAIL / UNJIRA_JIRA_TOKEN not set")
	}

	client, err := jira.New(site, email, token)
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

	return pipeline.CollectContext{
		Store: s,
		Config: config.Config{Jira: []config.JiraConnection{{
			Name:        "dev",
			Site:        site,
			ProjectKeys: []string{testProject()},
			Queries: []config.JiraQuery{
				{Name: "probe", JQL: fmt.Sprintf("key = %s", issueKey)},
			},
		}}},
		Credentials: credentials.NewSet(map[string]credentials.Credential{
			"dev": {
				Email: os.Getenv("UNJIRA_JIRA_EMAIL"),
				Token: os.Getenv("UNJIRA_JIRA_TOKEN"),
			},
		}),
		Options: map[string]any{},
	}
}

// waitUntilSearchable blocks until key is findable by JQL, not merely fetchable
// by key.
//
// Jira's search index is eventually consistent — measured at roughly 3 seconds
// on this instance — so a test that creates an issue and immediately searches
// for it races the index. Polling rather than sleeping a fixed duration keeps
// the common case fast and the slow case correct.
//
// This exists because the collector's first pass must actually match the issue:
// if it does not, no watermark is stored and a following pass quietly reverts to
// an unbounded query, which would make the watermark assertion vacuous.
func waitUntilSearchable(t *testing.T, client *jira.Client, key string) {
	t.Helper()

	const (
		attempts = 20
		interval = time.Second
	)

	jql := fmt.Sprintf("key = %s", key)

	for range attempts {
		var found bool
		if err := client.SearchIssues(jql, []string{"key"}, 1, func(map[string]any) {
			found = true
		}); err != nil {
			t.Fatalf("searching for %s while waiting for the index: %v", key, err)
		}

		if found {
			return
		}

		time.Sleep(interval)
	}

	t.Fatalf("%s was still not findable by JQL after %s; the search index is unusually far behind",
		key, time.Duration(attempts)*interval)
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
	// creation by a few seconds. Without waiting, Collect matches nothing, `got`
	// is empty, and the assertion loop below simply never executes — so the
	// sawComment/sawStatus flags carry the whole test. Those final require calls
	// are what keep an empty collection from reading as success.
	waitUntilSearchable(t, client, key)

	cc := liveCollectContext(t, key)

	var got []events.Event
	require.NoError(t, collectorjira.New().Collect(cc, func(e events.Event) {
		got = append(got, e)
	}))

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
		assert.Equal(t, "dev", e.Artifacts["connection"])
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

	// Jira's search index is eventually consistent: a just-created issue is
	// fetchable by key immediately but takes a few seconds to become findable
	// by JQL (measured at ~3.3s on this instance). Without waiting, the first
	// pass searches, matches nothing, stores no watermark, and the second pass
	// silently degrades into another unbounded query — proving nothing. This is
	// a property of Jira, not of the collector.
	waitUntilSearchable(t, client, key)

	cc := liveCollectContext(t, key)
	collector := collectorjira.New()

	// First pass: no cursor, so a plain unbounded query. This is what every
	// offline test already covers.
	require.NoError(t, collector.Collect(cc, func(events.Event) {}))

	position, err := cc.Store.GetCursor("jira", collectorjira.CursorResource("dev", "probe"))
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
