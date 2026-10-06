//go:build live

package live

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cenkalti/backoff/v5"

	"github.com/jcogilvie/unjira/internal/clients/jira"
)

// This file holds the machinery that separates "Jira's search index has not
// caught up" from "the collector is wrong" (docs/design-notes.md incidents 26 and
// 42). Its decision logic is the pure classifyIndexMiss/classifyFresh pair, so the
// rule that decides who gets blamed is unit-tested (index_verdict_test.go) rather
// than drilled by hand against a live instance.

// verdictClass names who a failed collector test blames.
type verdictClass string

const (
	// verdictRealBug: the evidence shows the collector itself excluded the issue.
	verdictRealBug verdictClass = "REAL BUG"
	// verdictInfrastructure: the collector's JQL demonstrably matches the issue's
	// current state, and the search index still does not return it.
	verdictInfrastructure verdictClass = "INFRASTRUCTURE"
	// verdictUndetermined: a check that discriminates could not run, so nothing
	// is ruled in or out.
	verdictUndetermined verdictClass = "UNDETERMINED"
)

type verdict struct {
	class  verdictClass
	reason string
}

// searchOutcome is one probe's result. The zero value is "ran, matched nothing".
type searchOutcome struct {
	matched bool
	err     error
}

// indexEvidence is everything the verdict is computed from.
type indexEvidence struct {
	// jqlErr is set when the collector's unbounded query could not even be built;
	// no probe below ran.
	jqlErr error
	// scopedFresh is the collector's own unbounded JQL, searched WITH
	// reconcileIssues: Jira evaluates it against the issue's current state, not the
	// index, so it answers "does this JQL match this issue" independently of lag.
	scopedFresh searchOutcome
	// keyFresh is bare `key = X`, also reconciled. It is the CONTROL for
	// scopedFresh: if even this misses, the consistent read itself is not working
	// (or the issue is gone), and scopedFresh's miss proves nothing.
	keyFresh searchOutcome
	// scopedIndexed is the collector's unbounded JQL against the index alone — what
	// the collector's own first pass actually runs.
	scopedIndexed searchOutcome
}

// classifyFresh judges the two consistent-read probes alone. ok means the
// collector's JQL demonstrably matches the issue's current state, so only the
// index (or something the collector adds on top of that JQL) can be at fault.
// When !ok, the returned verdict says why.
func classifyFresh(ev indexEvidence) (verdict, bool) {
	switch {
	case ev.jqlErr != nil:
		return verdict{verdictUndetermined, "could not even build the collector's unbounded query, " +
			"which is itself suspicious: " + ev.jqlErr.Error()}, false
	case ev.scopedFresh.err != nil:
		return verdict{verdictUndetermined, "the consistent-read (reconcileIssues) search of the " +
			"collector's JQL failed (" + ev.scopedFresh.err.Error() + "), so it rules nothing in or out"}, false
	case ev.scopedFresh.matched:
		return verdict{}, true
	case ev.keyFresh.err != nil:
		return verdict{verdictUndetermined, "the collector's JQL missed the issue on a consistent read, " +
			"but the bare-key control search failed (" + ev.keyFresh.err.Error() + "), so the miss " +
			"cannot be trusted"}, false
	case !ev.keyFresh.matched:
		return verdict{verdictUndetermined, "even bare `key = X` missed the issue on a consistent " +
			"read, so reconcileIssues is not behaving as documented (or the issue is gone) and the " +
			"discriminator is unavailable"}, false
	default:
		return verdict{verdictRealBug, "the collector's unbounded JQL does not match the issue even " +
			"against its CURRENT state (a consistent read, immune to index lag), while bare " +
			"`key = X` does. The scoping EffectiveJQL adds excludes it — no amount of waiting " +
			"would have helped"}, false
	}
}

// classifyIndexMiss decides who a failed collector test blames.
//
// Every path assigns a verdict explicitly; any path where a discriminating check
// did not complete is UNDETERMINED. An earlier version defaulted to
// INFRASTRUCTURE and, with a deliberately-broken watermarkClause, reported "not
// caused by your change" — confidently wrong, which is worse than ambiguous.
func classifyIndexMiss(ev indexEvidence) verdict {
	if v, ok := classifyFresh(ev); !ok {
		return v
	}

	switch {
	case ev.scopedIndexed.err != nil:
		return verdict{verdictUndetermined, "the index search of the collector's unbounded JQL " +
			"failed (" + ev.scopedIndexed.err.Error() + "), so it rules nothing in or out"}
	case ev.scopedIndexed.matched:
		return verdict{verdictRealBug, "the issue IS visible in the index to the collector's " +
			"unbounded JQL, so what the collector adds on top of it — its `updated >=` bound — or " +
			"how it processes results excluded it. Jira answers 200-with-zero-results for a date " +
			"literal it cannot use rather than 400, so this is exactly what a wrong watermarkClause " +
			"looks like, and no offline test can see it"}
	default:
		return verdict{verdictInfrastructure, "the collector's JQL matches the issue's current state " +
			"on a consistent read, but the search index still does not return it: Jira's index " +
			"has not converged (docs/design-notes.md incidents 26 and 42). NOT caused by the " +
			"change under test; re-run the job"}
	}
}

// gatherFreshEvidence runs the two consistent-read probes for key. The issue id
// reconcileIssues needs comes from GetIssue, which reads the document store and is
// immediately consistent.
func gatherFreshEvidence(client *jira.Client, scopedJQL, key string) indexEvidence {
	id, err := issueID(client, key)
	if err != nil {
		failed := searchOutcome{err: fmt.Errorf("resolving the id of %s for reconcileIssues: %w", key, err)}

		return indexEvidence{scopedFresh: failed, keyFresh: failed}
	}

	return indexEvidence{
		scopedFresh: probe(client, scopedJQL, []int64{id}),
		keyFresh:    probe(client, "key = "+key, []int64{id}),
	}
}

func issueID(client *jira.Client, key string) (int64, error) {
	raw, err := client.GetIssue(key, "")
	if err != nil {
		return 0, err
	}

	idStr, _ := raw["id"].(string)

	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("issue %s has no numeric id (got %q): %w", key, idStr, err)
	}

	return id, nil
}

// probe searches jql for at most one issue; reconcile, when non-empty, makes the
// search a consistent read for those ids.
func probe(client *jira.Client, jql string, reconcile []int64) searchOutcome {
	var hits int

	visit := func(map[string]any) { hits++ }

	var err error
	if len(reconcile) > 0 {
		err = client.SearchIssuesReconciled(jql, []string{"key"}, 1, reconcile, visit)
	} else {
		err = client.SearchIssues(jql, []string{"key"}, 1, visit)
	}

	return searchOutcome{matched: hits > 0, err: err}
}

// errNotYetIndexed is awaitIndexed's retryable condition.
var errNotYetIndexed = errors.New("not yet visible in the search index")

// indexReadinessBound is how long awaitIndexed waits for Jira's search index to
// return a just-written issue at all.
//
// Sized against what is KNOWN about the lag, not its median. Atlassian documents it
// as "from a few seconds to minutes" (search-and-reconcile docs). Measured here: a
// ~3s median and a 24s tail across seven samples (incident 26). Then two CI runs
// in one hour exceeded the old ~37s window. Run 37042794135 still could not see
// its issue ~46s after creation. Run 37053871018 lost two issues created ~36s
// apart for ~80 consecutive seconds (incident 43). Those figures are LOWER bounds:
// the tests gave up, the index did not report. Three minutes is ~4x the longest
// lag actually seen.
//
// This bound is affordable for two reasons. It is paid once per package run, not
// once per test (sharedCollectorIssue). And it is decoupled from bug detection: a
// scoping bug is caught by the consistent-read pre-check before any waiting, and
// a watermark bug by collectUntilMatched's own shorter collector budget, which
// starts only after this wait succeeds. Neither real bug pays these three minutes.
// Only genuine index lag does.
//
// If this starts failing, RE-MEASURE before raising it. The per-run "visible ...
// after" log line (CI runs -v) is that measurement, recorded on every run.
const indexReadinessBound = 3 * time.Minute

// awaitIndexed polls the index-only search of scopedJQL until it returns the
// issue, logging every attempt, and returns the last outcome.
//
// This is NOT the readiness probe incident 26 rejected. That one was a SUBSTITUTE
// for retrying the collector, and failed because the index is non-monotonic: it can
// return an issue and then lose it. This is a PRECONDITION that separates two
// different waits — a long one for the index to converge at all (cheap: one
// key-only search per poll) and a short one, still around the collector itself, for
// transient flaps after it has. Merging them would force one budget to be both long
// enough for lag and short enough to report a real bug promptly.
//
// Elapsed time is the bound that binds. MaxTries is only a cost backstop: with
// these intervals three minutes is ~18 key-only polls on average and ~30 if every
// jitter draw lands low, so 60 never trips first.
func awaitIndexed(t *testing.T, client *jira.Client, scopedJQL string) searchOutcome {
	t.Helper()

	started := time.Now()
	attempts := 0

	var last searchOutcome

	_, err := backoff.Retry(t.Context(), func() (struct{}, error) {
		attempts++
		last = probe(client, scopedJQL, nil)

		switch {
		case last.err != nil:
			// The client already retries transient GET failures, so an error here is
			// decided; stop rather than burn the budget.
			return struct{}{}, backoff.Permanent(last.err)
		case !last.matched:
			return struct{}{}, errNotYetIndexed
		default:
			return struct{}{}, nil
		}
	},
		backoff.WithBackOff(&backoff.ExponentialBackOff{
			InitialInterval:     time.Second,
			RandomizationFactor: 0.5,
			Multiplier:          1.5,
			MaxInterval:         15 * time.Second,
		}),
		backoff.WithMaxElapsedTime(indexReadinessBound),
		backoff.WithMaxTries(60),
		backoff.WithNotify(func(err error, next time.Duration) {
			t.Logf("index readiness: poll %d at %s: %v; next poll in %s",
				attempts, time.Since(started).Round(time.Millisecond), err, next.Round(time.Millisecond))
		}),
	)
	if err == nil {
		t.Logf("index readiness: visible to %q after %s (%d polls)",
			scopedJQL, time.Since(started).Round(time.Millisecond), attempts)
	} else {
		t.Logf("index readiness: gave up after %s (%d polls): %v",
			time.Since(started).Round(time.Millisecond), attempts, err)
	}

	return last
}

// failWithVerdict ends the test with v, and — under GitHub Actions — also emits it
// as an error annotation, so the PR's checks page names the verdict without anyone
// opening the log. It still FAILS: an INFRASTRUCTURE verdict is a distinct,
// labelled failure, never a skip, because a skipped live run prints PASS
// (design-notes #37).
func failWithVerdict(t *testing.T, v verdict, detail string) {
	t.Helper()

	if inGitHubActions() {
		// Straight to stdout rather than t.Log: workflow commands must start the
		// line, and t.Log indents.
		fmt.Println(githubAnnotation(v, detail))
	}

	t.Fatalf("%s\nVERDICT: %s — %s.", detail, v.class, v.reason)
}

// githubAnnotation renders v as a GitHub Actions ::error workflow command,
// escaped per the runner's rules (data: % CR LF; properties additionally : ,).
func githubAnnotation(v verdict, detail string) string {
	data := escapeWorkflowData(v.reason + "\n\n" + detail)
	title := strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A", ":", "%3A", ",", "%2C").
		Replace("Live Jira verdict: " + string(v.class))

	return "::error title=" + title + "::" + data
}

// collectorFixtureCommentBody is the comment the shared fixture issue carries.
const collectorFixtureCommentBody = "live collector probe comment"

// collectorIssue is the one issue both collector tests search for: created,
// transitioned once and commented once, then never mutated again.
//
// ONE per package run, shared, because the index lag that fails these tests is
// RUN-WIDE, not per-issue bad luck. CI run 37053871018 lost SCRUM-2270 and then
// SCRUM-2271, created ~36s apart, for ~80 consecutive seconds. With an issue per
// test, a lag window makes every test pay its own full readiness wait and fail in
// turn. With one, the run waits once.
//
// Sharing is safe only because the fixture is immutable after seeding: neither
// test writes to it, so neither can depend on the other's leftovers. That is the
// line between this and the long-lived cross-run fixture incident 26 warns
// against. That one accumulates every past run's comments and transitions, which
// makes an assertion like "a comment event appeared" pass vacuously on an old
// comment. This one is created fresh each run and deleted at the end.
type collectorIssue struct {
	key string
	// failure is set when the fixture is unusable. Every test that asks for it fails
	// with the same verdict, so the wait is paid once and not re-paid per test.
	failure *verdict
	detail  string
}

var (
	fixtureOnce   sync.Once
	fixture       collectorIssue
	fixtureClient *jira.Client
)

// sharedCollectorIssue returns the run's collector fixture, seeding it and waiting
// for the index on first use. A test that cannot have it fails here, with the
// verdict that explains why.
func sharedCollectorIssue(t *testing.T, client *jira.Client) string {
	t.Helper()

	// No t.Fatal inside Do: a Goexit there would leave the Once spent with the
	// fixture half-built. Failures are recorded, then reported below, by every caller.
	fixtureOnce.Do(func() { fixture = seedCollectorIssue(t, client) })

	if fixture.failure != nil {
		failWithVerdict(t, *fixture.failure, fixture.detail)
	}

	return fixture.key
}

func seedCollectorIssue(t *testing.T, client *jira.Client) collectorIssue {
	t.Helper()

	var key string

	// key is carried into the failure so a half-seeded issue is still deleted by
	// deleteCollectorFixture, which keys off fixture.key.
	setupFailure := func(step string, err error) collectorIssue {
		return collectorIssue{
			key: key,
			failure: &verdict{verdictUndetermined, "seeding the shared collector issue failed at " +
				step + " — a Jira write failed before any search ran, so this says nothing about " +
				"the collector"},
			detail: fmt.Sprintf("seeding: %s: %v", step, err),
		}
	}

	key, err := client.CreateIssue(testProject(), "[seed] live collector test issue", "Task",
		"Created by internal/live; deleted when the package's tests finish.", []string{jira.SeedLabel})
	if err != nil {
		return setupFailure("create", err)
	}
	// Registered before anything else can fail, so a half-seeded issue is still
	// deleted.
	fixtureClient = client

	// A transition, so there is a status changelog entry to collect.
	transitions, err := client.GetTransitions(key)
	if err != nil {
		return setupFailure("get transitions", err)
	}
	if len(transitions) == 0 {
		return setupFailure("get transitions", errors.New("no legal transition from the initial status"))
	}
	targetID, _ := transitions[0]["id"].(string)
	if err := client.TransitionIssue(key, targetID, nil); err != nil {
		return setupFailure("transition", err)
	}

	if _, err := client.AddComment(key, collectorFixtureCommentBody); err != nil {
		return setupFailure("comment", err)
	}

	fx := collectorIssue{key: key}

	scopedJQL, jqlErr := probeTracker(key).EffectiveJQL(probeQuery(key))
	if jqlErr != nil {
		v := classifyIndexMiss(indexEvidence{jqlErr: jqlErr})
		fx.failure, fx.detail = &v, "building the collector's query for "+key

		return fx
	}

	// Pre-check, BEFORE any waiting: does the collector's own JQL match the issue's
	// current state? A consistent read answers that immediately and immune to lag,
	// so a scoping bug fails now as REAL BUG, not after a three-minute wait that ends
	// in a misleading INFRASTRUCTURE.
	//
	// Only a REAL BUG that REPRODUCES stops the run here. If the instrument itself
	// fails (the control misses, or the search errors), the test goes on without
	// it, and the final verdict degrades to UNDETERMINED, which is honest. An
	// unverified reconcileIssues must not be able to fail every run.
	if v := confirmedFreshVerdict(t, client, scopedJQL, key); v != nil {
		if v.class == verdictRealBug {
			fx.failure, fx.detail = v, fmt.Sprintf("pre-check of %s against %q, reproduced %d times",
				key, scopedJQL, freshConfirmRounds)

			return fx
		}

		t.Logf("index pre-check UNAVAILABLE, continuing without it: %s", v.reason)
	} else {
		// Logged on success too, deliberately. Silence on the happy path would make a
		// run where the pre-check worked indistinguishable from one where it never
		// ran — and this is the only positive evidence that reconcileIssues behaves as
		// Atlassian documents, which nothing offline can check (design-notes #37).
		t.Logf("index pre-check OK: consistent read matches %s under %q, before any waiting", key, scopedJQL)
	}

	if last := awaitIndexed(t, client, scopedJQL); !last.matched {
		ev := gatherFreshEvidence(client, scopedJQL, key)
		ev.scopedIndexed = last
		v := classifyIndexMiss(ev)
		fx.failure, fx.detail = &v, fmt.Sprintf(
			"%s never became visible to the collector's unbounded JQL %q within %s",
			key, scopedJQL, indexReadinessBound)
	}

	return fx
}

// freshConfirmRounds and freshConfirmGap set how many times, and how far apart, the
// pre-check must see the same REAL BUG before it stops the run early.
const (
	freshConfirmRounds = 3
	freshConfirmGap    = 5 * time.Second
)

// confirmedFreshVerdict runs the consistent-read pre-check. It returns nil when
// the collector's JQL matches the issue's current state, and a REAL BUG only if
// every one of freshConfirmRounds rounds says so.
//
// Why repeat a check that is supposed to be deterministic: it is deterministic
// only if reconcileIssues works as documented, and that was not verified live when
// this was written. If Jira ignored the parameter, both "fresh" probes would really
// be index reads. Incident 26 recorded that `key = X` and the scoped JQL converge at
// DIFFERENT times, so one round could see key-matches, scoped-misses and wrongly
// blame the scoping. A real scoping bug is unchanged by ~10s of waiting. A
// convergence gap rarely survives it. The cost to a real bug is those ~10s, against
// minutes for the readiness wait it would otherwise sit through.
func confirmedFreshVerdict(t *testing.T, client *jira.Client, scopedJQL, key string) *verdict {
	t.Helper()

	var last verdict

	for round := 1; round <= freshConfirmRounds; round++ {
		v, ok := classifyFresh(gatherFreshEvidence(client, scopedJQL, key))
		if ok {
			return nil
		}

		if v.class != verdictRealBug {
			return &v
		}

		t.Logf("index pre-check: round %d/%d says %s: %s", round, freshConfirmRounds, v.class, v.reason)

		last = v

		if round < freshConfirmRounds {
			time.Sleep(freshConfirmGap)
		}
	}

	return &last
}

// deleteCollectorFixture runs from TestMain after every test. A failed delete is
// announced, not swallowed: a silently leaked seed issue per run is how a sandbox
// project fills up unnoticed. It does not fail the run, because a cleanup hiccup
// after every assertion passed would be this tier's own new flake.
func deleteCollectorFixture() {
	if fixtureClient == nil || fixture.key == "" {
		return
	}

	reportCleanupFailure(os.Stderr, os.Stdout, inGitHubActions(),
		"the shared collector issue", fixture.key, fixtureClient.DeleteIssue(fixture.key))
}
