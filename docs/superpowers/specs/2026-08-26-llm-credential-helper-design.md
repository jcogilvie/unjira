# LLM credential helper — design

`llm.api_key_helper`: let unjira obtain its LLM credential by *running a command* rather than reading
a string once at startup, so a short-lived token can be refreshed mid-run.

## Status: landed 2026-08-26

`llm.CredentialSource` + `StaticCredential`, per-request resolution in
`internal/clients/openai` via SDK middleware, `llm.HelperCredential`, the single-401 retry, and
`llm.api_key_helper` config. `earthly +reviewable` green.

**Verified end to end against the real gateway**: `UNJIRA_LLM_API_KEY` removed from `.env` entirely,
`api_key_helper` pointed at `~/.local/bin/get-litellm-key`, and a full `dev narrate` pass completed —
16 narratives, 24 proposed actions, every LLM call authenticated by the helper. A second pass reported
"no new events since the last proposal" for unchanged narratives and drafted two new comments where new
Jira activity had appeared.

Three findings from doing it, each recorded where the code lives:

1. **The timeout did not work on Linux.** `exec.CommandContext` kills the shell at the deadline, but
   with `Stdout` an `io.Writer` Go copies through an `os.Pipe`, and `Wait` blocks until every process
   holding the write end exits. Measured under a 100ms timeout: `sleep 2` returned after 2.001s on
   linux/amd64 versus 102ms on darwin/arm64, and a backgrounded `sleep 5 &` took 5.002s. `cmd.WaitDelay`
   bounds it. This is exactly the stall the timeout exists to prevent, and every local run masked it —
   the timeout test can only fail in the container.
2. **Two more model response shapes**, both found by running the pipeline rather than by reading it:
   newline-delimited objects and comma-separated objects where an array was demanded. Both are now
   absorbed by `llm.JSONArrayPayload`, bringing it to four tolerances. Note the trap: bracketing fixes
   commas but not newlines (JSON needs a comma *between* elements — whitespace is ignored around
   separators, not instead of them), while a `json.Decoder` loop fixes newlines but not commas. Both
   are needed.
3. **No deviation from the design's substance.** Scope stayed LLM-only, refresh is exp-based with a 401
   backstop, and the four hazards (never log stdout, bounded timeout, single-flight, don't cache a
   failure) were each implemented and tested as specified.

Two API details worth knowing: `llm.Invalidator` is an *optional* interface rather than a
`CredentialSource` method, since `StaticCredential` has nothing to invalidate and a no-op would let
callers believe an invalidation had an effect. And `WithHelperTimeout` exists as a functional option
because a hardcoded default would make the timeout test itself slow.

Still not done: **`watch` does not exist yet**, so the thing this unblocks remains unbuilt. And the
helper's own refresh has not been observed firing mid-pass — the token stayed valid throughout, so
proactive `exp` refresh and the 401 retry are proven by unit test, not by a live expiry.

Originally prompted by two real failures during slice 4's end-to-end verification (2026-08-26).

## The problem, as observed

`cmd/unjira/main.go` reads `UNJIRA_LLM_API_KEY` once into `appContext.llmAPIKey`, and `openai.New`
bakes it into the SDK client via `option.WithAPIKey` for that client's whole lifetime. There is no
path by which a running pass can obtain a fresher credential.

That is correct for a static API key. It is broken for a short-lived OIDC token, which is what this
environment's litellm proxy issues (`~/.local/bin/get-litellm-key`, an Okta PKCE flow producing a
JWT). Both failures were the same cause:

1. A `dev narrate` pass reached the reconcile stage **2.3 minutes after** the token expired, and every
   drafting call failed `401 Unauthorized`. The pass had already spent real money on clustering and
   matching; the reconciler produced nothing.
2. A later pass survived only because the token had been refreshed by hand seconds earlier.

The token's lifetime is short enough that it is not reliably longer than one pass over a real backlog.

**This is not a test-harness annoyance.** `watch` — the long-running loop that is phase 1's whole
point (see `docs/superpowers/specs/2026-08-11-phase1-correlator-design.md`, slice 5) — cannot run
unattended against a proxy whose credential expires, because a process that captures a bearer token
at startup and never refreshes it will fail on its second hour and every hour after. Slice 5 is
blocked on this in practice even though nothing in slice 5's own spec mentions it.

## Scope

**LLM only.** Jira keeps `UNJIRA_JIRA_CREDENTIALS` as-is: Atlassian API tokens are long-lived, and no
failure has been observed there. Deliberately not generalized to a shared `credentials.Provider`
today — that would be designing for a second consumer that does not exist, and `jira.Client` bakes
`BasicAuthTransport` at construction, so adopting it would touch materially more code for no current
benefit. The interface below lives in `internal/llm` rather than `internal/credentials` for the same
reason: it is the LLM contract's concern until something else needs it, at which point moving it is a
mechanical change with a real second consumer to validate the shape against.

## Shape

### `llm.CredentialSource`

```go
// CredentialSource yields the bearer credential for one completion.
type CredentialSource interface {
    Credential(ctx context.Context) (string, error)
}
```

A one-method interface in `internal/llm`, alongside `Client`. Two implementations:

- **`llm.StaticCredential(string)`** — returns the same value forever. What `UNJIRA_LLM_API_KEY`
  becomes, so the existing path is a special case of the new one rather than a parallel branch.
- **`llm.HelperCredential`** — execs a configured command, caches the result, refreshes when stale.

### Why a per-request source rather than re-reading a field

`openai.Client` must consult the source **per request**, not once at construction. The SDK supports
exactly this via `option.WithMiddleware` (verified present in openai-go v1.12.0):

```go
type Middleware = func(*http.Request, MiddlewareNext) (*http.Response, error)
```

So `openai.New` registers a middleware that sets `Authorization: Bearer <credential>` on every
outbound request, replacing `option.WithAPIKey`. This keeps the refresh entirely inside
`internal/clients/openai` — `correlator`, `reconciler`, and `pipeline` are unchanged, because they
only ever see `llm.Client.Complete`.

### Caching and refresh

`HelperCredential` caches the token and refreshes on two independent triggers:

1. **Proactive, from `exp`.** If the token parses as a JWT, decode its `exp` claim and refresh once
   the remaining lifetime drops below a safety margin. `get-litellm-key` uses a 300-second margin
   internally; matching it means unjira refreshes at roughly the same moment the helper itself would
   consider the token stale, rather than fighting it.
2. **Reactive, on `401`.** If a completion fails with HTTP 401, discard the cached credential, fetch
   once, and retry the request exactly once. This covers opaque (non-JWT) tokens, where there is no
   `exp` to read, and clock skew, where our arithmetic says "valid" and the server disagrees.

Both, not one: `exp` alone silently does nothing for an opaque token, and 401-only wastes a request —
and, worse, wastes a *large* request, since these prompts run to tens of thousands of tokens.

The 401 retry must be **exactly once**. A loop on a genuinely-bad credential would hammer the proxy
and mask a misconfiguration as a hang. Detection uses `openai.Error`, a type alias the SDK exports for
its internal `apierror.Error`, carrying `StatusCode int` — verified, so no string matching on error
text.

### Config

```json
"llm": {
  "api_key_helper": "~/.local/bin/get-litellm-key"
}
```

Precedence, most specific first: `api_key_helper` if set, else `UNJIRA_LLM_API_KEY`, else fail as
today. The helper goes in **config**, not the environment, because it is not itself a secret — it is a
path, exactly like `base_url`. That is consistent with the standing rule ("credentials come from the
environment, never config files"): the *credential* still never appears in config; only the means of
obtaining one does. `~` expands, since Claude Code's own `apiKeyHelper` is conventionally written that
way and a config that silently fails on a tilde would be a bad surprise.

## Decisions worth review

- **Executing a configured command is a real capability grant**, and it should be stated rather than
  buried. Anyone who can write `unjira.config.json` can make unjira run an arbitrary program. That is
  the same trust model as Claude Code's `apiKeyHelper`, and the config file is already
  gitignored/local, but it is a genuine widening of what a config file can cause. The alternative —
  an allowlist of helper paths — buys little, since an attacker who can edit the config can also edit
  the allowlist.
- **The helper's stdout is a credential, so it must never be logged.** Not on success, not in an
  error path, not in a debug mode. A non-zero exit must surface the helper's *stderr* and exit code
  while saying nothing about stdout, since a partially-written token on a failed run is still
  sensitive. This is the one place in this design where a careless error message does real damage.
- **A hung helper must time out.** `get-litellm-key` can open a browser for an interactive Okta login
  and waits up to 300 seconds. Inside `watch` that would stall the loop indefinitely with no
  indication why. So: a bounded `exec.CommandContext` timeout (a few seconds — enough for a cache
  hit or a refresh-token exchange, not for a human), and an error naming the timeout explicitly.
  Corollary worth stating: **unjira cannot complete an interactive login.** If the helper needs a
  browser, unjira must fail with a message telling the operator to run the helper by hand once, not
  appear to hang.
- **Refresh must be concurrency-safe.** Nothing in phase 1 calls `Complete` concurrently today, but
  `watch` plausibly will, and two goroutines racing to exec the helper would both spawn a subprocess.
  A mutex around the cache, with a single-flight refresh.
- **Usage accounting must survive a retry.** The 401 path retries a request that may already have
  reported usage. Double-counting would corrupt the token telemetry that
  `estimateTokens` is validated against (see `Stats.AddUsage`). A failed attempt reports no usage, so
  only the successful attempt's usage is folded.

## What this does NOT do

- **Jira.** Out of scope; see above.
- **Storing or writing credentials.** unjira reads from the helper and holds the value in memory for
  its process lifetime. It never writes a token to disk — the helper owns its own cache
  (`~/.sumologic/claude-cli-tokens.json`), and duplicating that would create a second thing to expire.
- **Retrying anything but 401.** The SDK already retries 408/409/429/5xx (verified in
  `requestconfig.shouldRetry`). 401 is deliberately absent from that list, which is why it needs
  handling here and why adding it does not collide with existing behaviour.
- **A general subprocess-credential framework.** One interface, two implementations, one consumer.

## Testing

Offline, no real proxy:

- `StaticCredential` returns the configured value; a client built from it sends
  `Authorization: Bearer <value>` — asserted against `httptest`, matching how
  `internal/clients/openai`'s existing tests already work.
- `HelperCredential` execs a **test script** (written to `t.TempDir()`) that prints a token and
  records its invocation count. Then: a second call within the validity window does **not** re-exec
  (cache hit); a call after `exp` passes **does**; a helper exiting non-zero produces an error naming
  its stderr and **not** its stdout.
- **The 401 retry, table-driven against a fake server:** first request 401s, second succeeds ⇒ one
  retry, helper invoked twice, success returned. Both requests 401 ⇒ error, helper invoked twice, and
  **no third attempt** — the assertion that pins "exactly once".
- **Usage is not double-counted across a retry** — a distinct test, because it is the subtle one.
- A hung helper (a script that sleeps past the timeout) errors, naming the timeout.

Live tier: one gated test that a real `Complete` succeeds with `api_key_helper` configured, given
`UNJIRA_LIVE=1`. This is the only way to establish that the middleware actually replaces
`WithAPIKey`'s header rather than adding a second one the proxy rejects.

## Sequencing

Independent of slice 4 (landed) and a **prerequisite for slice 5's `watch`**, not a nice-to-have: the
auto-commit gate and the loop both assume a pass can run unattended. Best done before `watch`, so
that command is written against a credential source that can refresh rather than being retrofitted.
