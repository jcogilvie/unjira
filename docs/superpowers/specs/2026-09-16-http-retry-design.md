# HTTP retry for the Jira client

**Status: design** — closes F23.

## The problem

A drain pass died on one request out of hundreds:

```
Error: matching narratives: verifying candidate PAAS-3042 for narrative 55:
getting jira issue PAAS-3042: read tcp …: read: operation timed out
```

Measured: **zero** occurrences of `retry` or `backoff` under `internal/clients/jira/`, and no
`http.Client` timeout configured. `verifyLinks` issues one `GetIssue` per candidate, so a pass's
failure probability grows with its candidate count.

Store-mediation bounds the damage (a re-run resumes), so this is robustness, not correctness. But the
intended deployment is a cron running `collect` and a human running `triage` — a pipeline that dies on
any single flaky request will erode trust in the cron.

## Decision: no new dependency

**Promote `github.com/cenkalti/backoff/v5` from test-only to production, behind a small
`http.RoundTripper` in `internal/clients/jira`.**

It is already a direct dependency (`go.mod`), currently used only by `internal/live/jira_test.go`,
which also already establishes the `WithMaxTries` + `WithMaxElapsedTime` pairing this will use.

### Why not a resilience library

The alternatives were reviewed against current maintenance status, not reputation:

| Library | Status | Why not |
|---|---|---|
| `failsafe-go/failsafe-go` | Active, growing; `failsafehttp` subpackage | The closest resilience4j analogue and the best-designed option. Rejected only because it is a *second* dependency for capability `backoff/v5` already provides. Revisit if adaptive rate limiting or hedging is ever wanted. |
| `hashicorp/go-retryablehttp` | Active (v0.7.8) | Its `DefaultRetryPolicy` retries 5xx **regardless of method** — actively dangerous here. Its own docs admit it "doesn't always act exactly as a RoundTripper should", since the transport shim is secondary to its `*Client` design. |
| `sony/gobreaker/v2` | Active | Circuit breaking, not retry. See "deferred" below. |
| `avast/retry-go` | Coasting | Its own README now points readers at a fork. |
| `sethvargo/go-retry` | Low activity | Adds nothing over the dependency already present. |

The deciding argument is the one `internal/logging` already made for choosing `log/slog`: this module
keeps its dependency surface small deliberately. Here it is stronger, because the dependency is
*already there* — the only question is whether it is imported from production code.

### What `backoff/v5` supplies, so we do not write it

- **Jitter** — `ExponentialBackOff` defaults to `RandomizationFactor: 0.5`.
- **`Retry-After`** — `backoff.RetryAfter(d, err)` schedules against the header instead of the
  exponential curve. First-class, not a workaround.
- **Context cancellation** — `Retry(ctx, fn, opts...)` stops on cancel/deadline.
- **Bounded attempts** — `WithMaxTries` and `WithMaxElapsedTime`.

## Design

A `retryTransport` wrapping the client's existing `Transport`, installed in `jira.New`:

```go
transport := jiracloud.BasicAuthTransport{Username: email, APIToken: token}
httpClient := transport.Client()
httpClient.Transport = newRetryTransport(httpClient.Transport)
upstream, err := jiracloud.NewClient(site, httpClient)
```

### Only idempotent methods are retried

**The load-bearing constraint.** `AddComment`, `TransitionIssue`, `CreateIssue` and `DeleteIssue` are
POST/PUT/DELETE, and Jira exposes no idempotency key — a blind retry risks a duplicate comment or a
double transition. Since unjira's whole safety story is that writes are gated and deliberate, a
transport that could silently double one is unacceptable.

The gate is a single visible early return, not a policy callback:

```go
if req.Method != http.MethodGet && req.Method != http.MethodHead {
    return t.inner.RoundTrip(req)  // one attempt, no retry
}
```

The HTTP method already encodes safety here, so no per-call opt-in mechanism is needed.

### Retry conditions

| condition | retry? |
|---|---|
| transport error (timeout, connection reset) | yes — this is F23's case |
| 429 | yes, honoring `Retry-After` |
| 5xx | yes |
| 4xx other than 429 | **no** — a 404 on a deleted issue will 404 again |
| 2xx | no |

A 404 must not retry: `verifyCandidates` uses exactly that to prune candidates whose issues no longer
exist, so retrying it would triple the cost of the normal pruning path.

### Bodies are drained and closed between attempts

The one thing no library does for us here. Without `io.Copy(io.Discard, resp.Body)` and `Close()`, the
connection is not returned to the pool and a retry storm exhausts them.

### Bounds

`WithMaxTries(4)` and `WithMaxElapsedTime(30s)`. Chosen so a pass fails visibly rather than hanging:
`verifyLinks` runs one call per candidate and a pass can hold dozens, so a per-request budget large
enough to mask an outage would turn one dead pass into a very slow one. An explicit
`http.Client.Timeout` is set for the same reason — an unbounded read is what F23 actually observed.

### Observability

The transport takes an optional `*slog.Logger` and logs each retry at debug with method, path, attempt
and reason. Silent retries make latency inexplicable — the failure mode F17 was about — and this is
the one place a wait can now be spent without any stage announcing it.

## Circuit breaking: deferred, deliberately

`unjira` is a short-lived CLI. A breaker's value is remembering "do not bother" *across* calls, which
a process that exits after one pass cannot use. The bounded retry budget above already fails fast.

If a long-running daemon mode ever lands, `sony/gobreaker/v2` is the pick, composed *inside* the retry
transport (`retryTransport{inner: breakerTransport{inner: base}}`) so each attempt consults the
breaker — not the reverse, which would let one breaker trip abort a retry sequence that was about to
succeed.

## Testing

- A `RoundTripper` stub returning a timeout then a 200: succeeds, exactly 2 attempts.
- POST with a stub that would succeed on attempt 2: **1 attempt only.** The most important test here —
  a regression means duplicate comments on real tickets.
- 429 with `Retry-After: 1`: honors the header rather than the exponential delay.
- 404: 1 attempt, and the response is returned rather than an error, so `verifyCandidates` still prunes.
- Attempt cap: a stub that always fails is called `MaxTries` times, no more.
- Context cancellation mid-backoff returns promptly.
- Response bodies are closed on every retried attempt (a counting `ReadCloser`).

## Not in scope

- **The LLM client.** `internal/clients/openai` uses the official SDK, which has its own retry policy.
  Adding a second layer would multiply attempt counts rather than add resilience.
- **Retrying at the pipeline level.** A pass that dies is already re-runnable; the fix belongs at the
  transport, where the transient failure actually is.
