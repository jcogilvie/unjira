package github

// retry.go mirrors internal/clients/jira/retry.go's retryTransport, verbatim in
// shape: same rationale (F23 — one transient timeout must not abort a whole
// collection pass), same method gate, same bounds. Duplicated rather than
// shared, because there is no existing internal/httputil seam and this
// package's own doc rule ("thin facade, no business logic shared across
// clients") treats each clients/<system> package as self-contained — see
// docs/go-conventions.md's package-layout section. Revisit if a third client
// needs the same transport: three copies is where extraction earns its keep,
// two is not yet worth the indirection.

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
	// maxAttempts bounds one request's total tries. See clients/jira/retry.go's
	// identical constant for the reasoning: a per-request budget generous
	// enough to ride out an outage turns one dead pass into a very slow one.
	maxAttempts = 4

	// defaultMaxElapsed caps wall time per request, independent of attempt
	// count.
	defaultMaxElapsed = 30 * time.Second

	// defaultInitialInterval is the first backoff delay, jittered by
	// backoff/v5's own 0.5 randomization factor.
	defaultInitialInterval = 250 * time.Millisecond

	// clientTimeout bounds a single attempt's read.
	clientTimeout = 20 * time.Second
)

// retryTransport wraps an http.RoundTripper, retrying idempotent requests.
type retryTransport struct {
	inner http.RoundTripper
	log   *slog.Logger

	// initialInterval and maxElapsed are fields rather than constants so tests
	// can collapse real delays. Production always uses the defaults.
	initialInterval time.Duration
	maxElapsed      time.Duration
}

// newRetryTransport wraps inner. A nil inner falls back to
// http.DefaultTransport, and a nil logger is silent.
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

// RoundTrip retries GET and HEAD on transient failures, and passes everything
// else through untouched.
//
// THE METHOD GATE IS THE LOAD-BEARING PART, same as clients/jira's. This
// slice never writes to GitHub, so today nothing sends a non-GET/HEAD request
// through this transport — but the gate stays regardless, because a
// RoundTripper covers whatever methods a later write path adds, and a
// transport that could silently double a write is unacceptable by
// construction, not by current usage.
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

		drainAndClose(resp)
		t.report(req, attempt, fmt.Sprintf("HTTP %d", resp.StatusCode))

		if after > 0 {
			// Honor Retry-After (and GitHub's secondary-rate-limit header, which
			// carries the same seconds-to-wait shape) rather than the
			// exponential curve.
			return nil, backoff.RetryAfter(int(after.Seconds()))
		}

		return nil, fmt.Errorf("github responded %d", resp.StatusCode)
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

// retryableResponse reports whether resp is worth another attempt, and how
// long to wait if it said so.
//
// A 404 is deliberately NOT retryable, mirroring clients/jira: a PR that
// genuinely does not exist will not exist on retry either.
func retryableResponse(resp *http.Response) (retry bool, after time.Duration) {
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return true, retryAfter(resp)
	case resp.StatusCode == http.StatusForbidden && resp.Header.Get("Retry-After") != "":
		// GitHub's secondary rate limit responds 403 with Retry-After, distinct
		// from the primary limit's 429 — see
		// https://docs.github.com/rest/overview/rate-limits-for-the-rest-api.
		// Gated on the header's presence so an ordinary permission-denied 403
		// (no Retry-After) is not retried, matching the "no other 4xx retries"
		// rule below.
		return true, retryAfter(resp)
	case resp.StatusCode >= http.StatusInternalServerError:
		return true, retryAfter(resp)
	default:
		return false, 0
	}
}

// retryAfter reads the Retry-After header as seconds, returning 0 when absent
// or unparseable.
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
func (t *retryTransport) report(req *http.Request, attempt int, reason string) {
	logging.For(t.log, "github").Debug("retrying github request",
		"method", req.Method, "path", req.URL.Path, "attempt", attempt, "reason", reason)
}
