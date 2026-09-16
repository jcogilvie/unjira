package jira

// retry.go retries idempotent Jira requests on transient failures — finding F23.
//
// A drain pass died on one request out of hundreds:
//
//	Error: matching narratives: verifying candidate PAAS-3042 for narrative 55:
//	getting jira issue PAAS-3042: read tcp …: read: operation timed out
//
// Nothing retried it. verifyLinks issues one GetIssue per candidate, so a pass's
// failure probability grows with its candidate count — and the intended deployment is a
// cron plus a human running triage, so a pipeline that dies on any single flaky request
// erodes trust in the cron.
//
// A RoundTripper rather than retry logic at the call sites, because go-jira accepts a
// custom *http.Client and every method here funnels through c.do. One decorator covers
// the whole surface, including methods added later.
//
// WHY backoff/v5 AND NOT A RESILIENCE LIBRARY. See
// docs/superpowers/specs/2026-09-16-http-retry-design.md, which re-argues this on
// merits after a first draft leaned on "it's already a dependency". The short version:
// failsafe-go's HandleIf predicate receives only (*http.Response, error) and so cannot
// see the request method on the transport-error path — F23's actual case, where resp is
// nil — which means the method gate below is structurally required either way, and it
// compiles ~10x the dependency for retry-only use.

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/cenkalti/backoff/v5"

	"github.com/jcogilvie/unjira/internal/logging"
)

const (
	// maxAttempts bounds one request's total tries.
	//
	// Deliberately small. verifyLinks runs one call per candidate and a pass can hold
	// dozens, so a per-request budget generous enough to ride out an outage would turn
	// one dead pass into a very slow one — and a pass that fails visibly is worth more
	// than a pass that hangs, which is what F23 actually observed.
	maxAttempts = 4

	// defaultMaxElapsed caps wall time per request, independent of attempt count: four
	// tries can still be slow if each one hangs.
	defaultMaxElapsed = 30 * time.Second

	// defaultInitialInterval is the first backoff delay. backoff/v5 applies its own
	// 0.5 randomization factor, so this is jittered without extra work.
	defaultInitialInterval = 250 * time.Millisecond

	// clientTimeout bounds a single attempt's read. F23's failure was an unbounded
	// read, so retrying without this would just retry hangs.
	clientTimeout = 20 * time.Second
)

// retryTransport wraps an http.RoundTripper, retrying idempotent requests.
type retryTransport struct {
	inner http.RoundTripper
	log   *slog.Logger

	// initialInterval and maxElapsed are fields rather than constants so tests can
	// collapse real delays. Production always uses the defaults.
	initialInterval time.Duration
	maxElapsed      time.Duration
}

// newRetryTransport wraps inner. A nil inner falls back to http.DefaultTransport, and a
// nil logger is silent — the same optional-dependency contract every other seam in this
// tree uses.
func newRetryTransport(inner http.RoundTripper, log *slog.Logger) *retryTransport {
	if inner == nil {
		inner = http.DefaultTransport
	}

	return &retryTransport{
		inner:           inner,
		log:             log,
		initialInterval: defaultInitialInterval,
		maxElapsed:      defaultMaxElapsed,
	}
}

// RoundTrip retries GET and HEAD on transient failures, and passes everything else
// through untouched.
//
// THE METHOD GATE IS THE LOAD-BEARING PART. Jira exposes no idempotency key, so a
// retried POST/PUT/DELETE risks a duplicate comment or a double transition —
// AddComment, TransitionIssue, CreateIssue and DeleteIssue are all in that set. unjira's
// entire safety story is that writes are gated and deliberate, so a transport that could
// silently double one is unacceptable. Written as a single early return rather than a
// policy predicate, so it is visible at a glance and cannot be reconfigured away.
func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return t.inner.RoundTrip(req)
	}

	attempt := 0
	operation := func() (*http.Response, error) {
		attempt++

		resp, err := t.inner.RoundTrip(req)
		if err != nil {
			t.report(req, attempt, err.Error())

			return nil, err
		}

		retryable, after := retryableResponse(resp)
		if !retryable {
			return resp, nil
		}

		// The body is discarded, so drain and close it: an unclosed body keeps its
		// connection out of the pool, and a retry storm exhausts them.
		drainAndClose(resp)
		t.report(req, attempt, fmt.Sprintf("HTTP %d", resp.StatusCode))

		if after > 0 {
			// Honor Retry-After rather than the exponential curve. Jira Cloud
			// rate-limits and sends this header; ignoring it makes a retry an
			// amplifier of the limit it was just told about.
			return nil, backoff.RetryAfter(int(after.Seconds()))
		}

		return nil, fmt.Errorf("jira responded %d", resp.StatusCode)
	}

	return backoff.Retry(req.Context(), operation,
		backoff.WithMaxTries(maxAttempts),
		backoff.WithMaxElapsedTime(t.maxElapsed),
		backoff.WithBackOff(&backoff.ExponentialBackOff{
			InitialInterval:     t.initialInterval,
			RandomizationFactor: backoff.DefaultRandomizationFactor,
			Multiplier:          backoff.DefaultMultiplier,
			MaxInterval:         backoff.DefaultMaxInterval,
		}))
}

// retryableResponse reports whether resp is worth another attempt, and how long to wait
// if it said so.
//
// A 404 is deliberately NOT retryable: verifyCandidates uses exactly that response to
// prune candidates whose issues no longer exist, so retrying would triple the cost of
// the normal pruning path. Nor is any other 4xx — a malformed request or a permission
// denial will be refused identically next time.
func retryableResponse(resp *http.Response) (retry bool, after time.Duration) {
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return true, retryAfter(resp)
	case resp.StatusCode >= http.StatusInternalServerError:
		return true, retryAfter(resp)
	default:
		return false, 0
	}
}

// retryAfter reads the Retry-After header as seconds, returning 0 when absent or
// unparseable. The HTTP-date form is not handled: Jira Cloud sends seconds, and
// guessing at a date format would risk a wildly wrong delay where 0 just falls back to
// the exponential curve.
func retryAfter(resp *http.Response) time.Duration {
	raw := resp.Header.Get("Retry-After")
	if raw == "" {
		return 0
	}

	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds <= 0 {
		return 0
	}

	return time.Duration(seconds) * time.Second
}

// drainAndClose returns a discarded response's connection to the pool.
func drainAndClose(resp *http.Response) {
	if resp.Body == nil {
		return
	}

	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

// report logs one retry at debug.
//
// Silent retries make latency inexplicable, which is the failure F17 was about: this is
// now the one place a pass can spend seconds without any stage announcing it. Debug
// rather than info because a healthy pass should be quiet, and the path is logged rather
// than the full URL so a query string cannot carry anything sensitive into a log.
func (t *retryTransport) report(req *http.Request, attempt int, reason string) {
	logging.For(t.log, "jira").Debug("retrying jira request",
		"method", req.Method, "path", req.URL.Path, "attempt", attempt, "reason", reason)
}
