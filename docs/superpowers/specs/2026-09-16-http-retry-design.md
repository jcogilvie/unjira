# HTTP retry for the Jira client

## Status: landed 2026-09-16

Implemented as `internal/clients/jira/retry.go`, installed in `jira.New`. Closes F23.

**Verification.** `go test ./internal/clients/jira/` covers each retry condition, and the
load-bearing one — POST/PUT/DELETE gets exactly 1 attempt even when a retry *would* have
succeeded — is scripted to fail loudly rather than pass by luck. `TestNew_InstallsTheRetryTransport`
asserts the wiring, drilled by removing it: it fails with "or F23's fix is inert — which is exactly
how F14 shipped".

**One deviation from the design below.** The spec said `WithMaxTries(4)` and `WithMaxElapsedTime(30s)`
without noting that they interact: backoff/v5 stops when `elapsed + next > MaxElapsedTime`
(`retry.go:122`), so a `Retry-After` longer than the remaining budget aborts rather than waiting. That
is correct behaviour — a 60s rate-limit wait should not silently extend a 30s budget — but it means the
two bounds are not independent, and the Retry-After test needs headroom to exercise the header at all.
Worth knowing before tuning either number.

**Not implemented:** the optional `*slog.Logger` is plumbed through `newRetryTransport` but `jira.New`
passes nil, because it has no logger to hand it — `New(site, email, token)` predates the logging seam.
Wiring it means widening that signature or adding an option, which belongs with whatever next needs it
rather than here.

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

## Decision: `backoff/v5` behind a RoundTripper, on the merits

**Promote `github.com/cenkalti/backoff/v5` from test-only to production, behind a small
`http.RoundTripper` in `internal/clients/jira`.**

An earlier draft of this spec reached the same conclusion for the wrong reasons, and the reasoning is
rewritten here rather than quietly corrected. What it argued — *"the deciding argument is the one
`internal/logging` already made for choosing `log/slog`"* — is a **false analogy**. `log/slog` won
because it is **stdlib**: zero dependency, permanently. `backoff/v5` is a third-party module that
happens to already sit in `go.mod`. "Already present" and "costs nothing" are different properties,
and the draft substituted one for the other. Inertia is not a merit, particularly on a greenfield
project where swapping is cheap now and expensive later.

Re-evaluated against ranked criteria, with the numbers measured rather than asserted:

### 1. Does it make the load-bearing safety property easy to express?

The property is **never retry a Jira write** — no idempotency key exists, so a retried POST risks a
duplicate comment on a real ticket.

`failsafehttp`'s retry predicate is:

```go
HandleIf(func(resp *http.Response, err error) bool)
```

It never receives the `*http.Request`. On a *successful* response the method is recoverable via
`resp.Request`, but **F23's actual failure is a transport error, where `resp == nil`** — so in exactly
the case that matters for write safety (a POST whose connection died mid-flight; did the server see
it?) the predicate cannot see the method at all. The outer method-gate below is therefore
*structurally required* with either library.

**Verdict: a wash.** The earlier draft called failsafe-go "the best-designed option", which overstated
what its HTTP integration actually provides for this constraint.

### 2. Does it fit where the project is going?

`watch` is already a long-running interval loop, four rate-limited APIs are planned (Jira, GitHub,
Slack, the LLM gateway), and structured JSON logging was made first-class explicitly because unjira
"is also expected to run as a service across an org."

**Verdict: failsafe-go's composition model wins on this axis** — see criterion 5 — but it argues for
*when to revisit*, not for adopting a policy suite before a second policy is needed.

### 3. Dependency cost, measured

| build | binary | third-party packages compiled |
|---|---|---|
| `net/http` only | 5.04 MB | none |
| `+ backoff/v5` | 5.10 MB | 1 |
| `+ failsafehttp` (retry only) | 5.60 MB | failsafe-go core, `failsafehttp`, **`bits-and-blooms/bitset`**, **`influxdata/tdigest`**, and every policy package |

`failsafehttp` is one package whose server-side helpers import all policies, so **retry-only usage
still compiles the whole suite**. That directly contradicts pay-for-what-you-use.

**Verdict: `backoff/v5`.**

### 4. Fit to the actual constraint

Jira Cloud's limiter is a **token bucket** (documented: GET 100/s, POST 100/s, PUT/DELETE 50/s, plus an
hourly points quota). failsafe-go offers smooth (leaky-bucket) and bursty (fixed-window) modes;
`golang.org/x/time/rate` is a token bucket and therefore matches the thing being protected against
more precisely.

Its **adaptive** limiter infers a safe *concurrency* ceiling from latency trends — a congestion-control
mechanism for a dependency whose safe concurrency is unknown. Jira publishes fixed numbers, and unjira
issues requests serially, so adaptive limiting would slowly converge on a value Atlassian already
states.

**Verdict: neither — `x/time/rate` if proactive limiting is ever wanted, which needs no library swap.**

### 5. Composability across 3+ policies

`failsafe.With(fallback, retry, breaker, timeout)` composes outermost-to-innermost with documented
semantics, so retry-outside-breaker means each attempt consults the breaker fresh. Sound and
comprehensible, and it removes a nesting decision a hand-rolled stack has to get right.

**Verdict: failsafe-go, uncontested — and this is the criterion that should trigger a revisit.**

### 6. API stability

The earlier draft leaned on this hardest and had it backwards. `cenkalti/backoff` shipped **v5 (Dec
2024), v6 (Jun 2026), and v7 (Jun 2026)** — three import-path-breaking majors in under two years, with
real signature changes. unjira is pinned to v5.0.3. failsafe-go churns via in-place 0.x renames
(`Builder()` → `NewBuilder()`, an `int` → `float64` threshold change), which is quieter but invisible
to SemVer.

**Verdict: neither is stable. They fail differently, and this criterion decides nothing.**

### Why the alternatives lose on merits

| Library | Why not |
|---|---|
| `failsafe-go/failsafe-go` | No advantage on criterion 1 (the predicate cannot see the request on the error path), 10× the compiled dependency cost for retry-only, and its rate limiter is a worse mechanism match than `x/time/rate`. Genuinely wins criterion 5 — revisit *there*. |
| `hashicorp/go-retryablehttp` | `DefaultRetryPolicy` retries 5xx **regardless of method** — actively dangerous here. Its own docs admit it "doesn't always act exactly as a RoundTripper should." |
| `sony/gobreaker/v2` | Circuit breaking, not retry. The pick *if* a breaker is added. |
| `avast/retry-go` | Its own README points readers at a fork. |
| `sethvargo/go-retry` | Adds nothing `backoff/v5` does not already provide. |

**The honest summary: `backoff/v5` wins on measured dependency cost and loses nothing that matters
today, while failsafe-go's one real advantage (composition) applies to a policy stack unjira does not
yet have.** Revisit when adding a second policy — most plausibly a circuit breaker once GitHub and
Slack collectors land — not before, and not for adaptive limiting, hedging, cache or fallback, all of
which remain speculative for unjira's serial request pattern.

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

## Circuit breaking: deferred, for a different reason than first written

An earlier draft justified deferring this with *"unjira is a short-lived CLI; a breaker's value is
remembering 'do not bother' across calls, which a process that exits after one pass cannot use."*
**That premise is false, and checkably so.** `watchCmd.Run` (`cmd/unjira/main.go:754-857`) builds the
`jira.Client`, tracker and applier **once, before `watchLoop`**, and reuses them across every tick of
`--interval`. So breaker state *would* persist across passes within one `watch` process, and `watch` is
the long-running mode the README already documents as shipped.

Worse, the draft used the opposite premise elsewhere in the same repo: `internal/logging`'s package
comment justifies first-class JSON output because unjira "is also expected to run as a service across
an org." One roadmap, evaluated two ways depending on which conclusion was convenient.

**The correct reason to defer:** there is no incident evidence. F23 was a *single transient timeout*,
which a bounded retry handles. A breaker earns its place against a *sustained* outage — where retrying
every candidate in a pass wastes the whole budget rediscovering that Jira is down — and nothing has
been observed like that yet. Revisit when it is, or when GitHub and Slack collectors make a
multi-API blast radius real.

When that happens, `sony/gobreaker/v2` composed *inside* the retry transport
(`retryTransport{inner: breakerTransport{inner: base}}`) so each attempt consults the breaker — or
`failsafe-go`, whose documented outermost-to-innermost composition expresses the same ordering without
hand-nesting, and which wins on that criterion specifically.

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
