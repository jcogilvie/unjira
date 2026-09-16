package jira

// retry_test.go covers finding F23: one transient timeout aborted a whole pipeline
// pass.
//
//	Error: matching narratives: verifying candidate PAAS-3042 for narrative 55:
//	getting jira issue PAAS-3042: read tcp …: read: operation timed out
//
// One request out of hundreds, nothing retried it, pass over. verifyLinks issues one
// GetIssue per candidate, so a pass's failure probability grows with its candidate
// count — and the intended deployment is a cron.
//
// THE MOST IMPORTANT TEST IN THIS FILE is TestRetryTransport_NeverRetriesAWrite. Jira
// exposes no idempotency key, so a retried POST risks a duplicate comment on a real
// ticket. Everything else here is throughput; that one is correctness.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubTransport returns a scripted sequence of outcomes, counting attempts.
type stubTransport struct {
	outcomes []stubOutcome
	attempts int
	// bodiesClosed counts response bodies the transport closed, so a test can assert
	// connections are returned to the pool rather than leaked.
	bodiesClosed *int
}

type stubOutcome struct {
	status int
	err    error
	header http.Header
}

func (s *stubTransport) RoundTrip(*http.Request) (*http.Response, error) {
	i := s.attempts
	s.attempts++
	if i >= len(s.outcomes) {
		i = len(s.outcomes) - 1
	}

	out := s.outcomes[i]
	if out.err != nil {
		return nil, out.err
	}

	header := out.header
	if header == nil {
		header = http.Header{}
	}

	return &http.Response{
		StatusCode: out.status,
		Header:     header,
		Body:       &countingBody{closed: s.bodiesClosed},
	}, nil
}

// countingBody records that it was closed, and is otherwise empty.
type countingBody struct {
	closed *int
}

func (b *countingBody) Read(_ []byte) (int, error) { return 0, io.EOF }

func (b *countingBody) Close() error {
	if b.closed != nil {
		*b.closed++
	}

	return nil
}

// fastRetry is the transport under test with delays collapsed, so the suite does not
// spend real backoff time.
func fastRetry(inner http.RoundTripper) *retryTransport {
	t := newRetryTransport(inner, nil)
	t.initialInterval = time.Microsecond
	t.maxElapsed = time.Second

	return t
}

// closeBody closes a response the test received. bodyclose is right to demand it, and
// a transport whose whole point is connection hygiene should be tested by something
// that models it rather than by a lint suppression.
func closeBody(t *testing.T, resp *http.Response) {
	t.Helper()

	if resp != nil && resp.Body != nil {
		require.NoError(t, resp.Body.Close())
	}
}

func request(t *testing.T, method string) *http.Request {
	t.Helper()

	req, err := http.NewRequestWithContext(
		t.Context(), method, "https://example.atlassian.net/rest/api/2/issue/PROJ-1",
		strings.NewReader(""))
	require.NoError(t, err)

	return req
}

// TestRetryTransport_RetriesATransportError is F23's exact case: a timeout, then
// success.
func TestRetryTransport_RetriesATransportError(t *testing.T) {
	stub := &stubTransport{outcomes: []stubOutcome{
		{err: errors.New("read tcp: operation timed out")},
		{status: http.StatusOK},
	}}

	resp, err := fastRetry(stub).RoundTrip(request(t, http.MethodGet))

	require.NoError(t, err)
	defer closeBody(t, resp)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, 2, stub.attempts, "one retry, not more")
}

// TestRetryTransport_NeverRetriesAWrite is the correctness test. A regression here
// means a duplicate comment on somebody's real ticket.
func TestRetryTransport_NeverRetriesAWrite(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			// Scripted so that a retry WOULD succeed — the test fails loudly if the
			// gate is ever removed, rather than passing by luck.
			stub := &stubTransport{outcomes: []stubOutcome{
				{err: errors.New("read tcp: operation timed out")},
				{status: http.StatusOK},
			}}

			resp, err := fastRetry(stub).RoundTrip(request(t, method))
			closeBody(t, resp)

			require.Error(t, err, "the transport error must surface, not be retried away")
			assert.Equal(t, 1, stub.attempts,
				"Jira has no idempotency key, so a retried %s risks a duplicate comment or a "+
					"double transition", method)
		})
	}
}

// TestRetryTransport_Retries5xx covers the server-error path.
func TestRetryTransport_Retries5xx(t *testing.T) {
	closed := 0
	stub := &stubTransport{bodiesClosed: &closed, outcomes: []stubOutcome{
		{status: http.StatusBadGateway},
		{status: http.StatusOK},
	}}

	resp, err := fastRetry(stub).RoundTrip(request(t, http.MethodGet))

	require.NoError(t, err)
	defer closeBody(t, resp)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, 2, stub.attempts)
	assert.Equal(t, 1, closed,
		"the discarded 502's body must be closed, or its connection never returns to the pool")
}

// TestRetryTransport_HonorsRetryAfterOn429 matters because Jira Cloud rate-limits and
// sends the header. Ignoring it turns a retry into an amplifier.
func TestRetryTransport_HonorsRetryAfterOn429(t *testing.T) {
	stub := &stubTransport{outcomes: []stubOutcome{
		{status: http.StatusTooManyRequests, header: http.Header{"Retry-After": {"1"}}},
		{status: http.StatusOK},
	}}

	// fastRetry's 1s maxElapsed would abort a 1s Retry-After outright: backoff/v5
	// stops when elapsed+next exceeds the budget (retry.go:122), so the wait has to
	// fit inside it. This is the one test that needs real headroom.
	transport := fastRetry(stub)
	transport.maxElapsed = 10 * time.Second

	start := time.Now()
	resp, err := transport.RoundTrip(request(t, http.MethodGet))
	elapsed := time.Since(start)

	require.NoError(t, err)
	defer closeBody(t, resp)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.GreaterOrEqual(t, elapsed, 900*time.Millisecond,
		"a Retry-After of 1s must be waited out, not replaced by the microsecond test interval — "+
			"otherwise this transport amplifies the rate limit it was told about")
}

// TestRetryTransport_DoesNotRetryA404 is load-bearing for verifyCandidates, which uses
// exactly that response to prune candidates whose issues no longer exist. Retrying
// would triple the cost of the normal pruning path.
func TestRetryTransport_DoesNotRetryA404(t *testing.T) {
	stub := &stubTransport{outcomes: []stubOutcome{{status: http.StatusNotFound}}}

	resp, err := fastRetry(stub).RoundTrip(request(t, http.MethodGet))

	require.NoError(t, err, "a 404 is an answer, not a failure: it must be RETURNED, not errored")
	defer closeBody(t, resp)

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Equal(t, 1, stub.attempts)
}

// TestRetryTransport_StopsAtTheAttemptCap keeps a sustained outage from turning one
// dead pass into a very slow one.
func TestRetryTransport_StopsAtTheAttemptCap(t *testing.T) {
	stub := &stubTransport{outcomes: []stubOutcome{{err: errors.New("read tcp: timed out")}}}

	resp, err := fastRetry(stub).RoundTrip(request(t, http.MethodGet))
	closeBody(t, resp)

	require.Error(t, err)
	assert.Equal(t, maxAttempts, stub.attempts,
		"verifyLinks runs one call per candidate and a pass holds dozens, so an unbounded budget "+
			"would mask an outage instead of failing visibly")
}

// TestRetryTransport_HonorsContextCancellation keeps a retry loop from outliving the
// pass that started it.
func TestRetryTransport_HonorsContextCancellation(t *testing.T) {
	stub := &stubTransport{outcomes: []stubOutcome{{err: errors.New("read tcp: timed out")}}}

	transport := newRetryTransport(stub, nil) // real intervals, so cancellation wins
	req := request(t, http.MethodGet)
	ctx, cancel := context.WithCancel(req.Context())
	cancel()

	resp, err := transport.RoundTrip(req.WithContext(ctx))
	closeBody(t, resp)

	require.Error(t, err)
	assert.LessOrEqual(t, stub.attempts, 1,
		"a cancelled context must stop the loop rather than spending the whole attempt budget")
}

// TestRetryTransport_PassesSuccessThrough is the no-op case: nothing to retry, one
// attempt, body untouched.
func TestRetryTransport_PassesSuccessThrough(t *testing.T) {
	closed := 0
	stub := &stubTransport{bodiesClosed: &closed, outcomes: []stubOutcome{{status: http.StatusOK}}}

	resp, err := fastRetry(stub).RoundTrip(request(t, http.MethodGet))

	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, 1, stub.attempts)
	assert.Zero(t, closed, "a returned response's body belongs to the caller, unread and unclosed")

	// Closed only after asserting it was NOT closed by the transport: that is the
	// property under test, and the caller owning the body is why.
	closeBody(t, resp)
}

// TestNew_InstallsTheRetryTransport is the inert-shipping guard. F14 shipped complete,
// green, and doing nothing because its constructor was never wired to the field list it
// needed; go-conventions.md's "wire it, then prove it fires" exists because of it.
//
// Asserts the observable consequences rather than the concrete type: a caller cannot
// reach inside, and what matters is that the client bounds its reads and retries.
func TestNew_InstallsTheRetryTransport(t *testing.T) {
	client, err := New("https://example.atlassian.net", "e@x.com", "token")
	require.NoError(t, err)
	require.NotNil(t, client)

	httpClient := client.upstream.Client()
	require.NotNil(t, httpClient)

	assert.Equal(t, clientTimeout, httpClient.Timeout,
		"an unbounded read is what F23 actually observed, so retrying without a timeout would "+
			"only retry hangs")

	_, ok := httpClient.Transport.(*retryTransport)
	assert.True(t, ok,
		"the retry transport must be installed, or F23's fix is inert — which is exactly how F14 "+
			"shipped")
}
