# llm.HelperCredential

## As Is

`internal/llm/llm.go` defines `CredentialSource` (interface, one method:
`Credential(ctx context.Context) (string, error)`) and `StaticCredential`
(string-backed, never expires). `internal/clients/openai.New` already resolves
the credential per-request via SDK middleware (landed in commit 4059bf7).
There is no implementation that obtains a credential by running an external
command, so unjira cannot yet consume a short-lived OIDC token
(`~/.local/bin/get-litellm-key`) without the caller re-running the helper by
hand.

Design spec: `docs/superpowers/specs/2026-08-26-llm-credential-helper-design.md`.

## To Be

A new type, `llm.HelperCredential`, satisfying `llm.CredentialSource`. It
execs a configured command whose trimmed stdout is the credential; caches the
result; if the result parses as a JWT, proactively refreshes once remaining
lifetime drops below a 300-second margin; if not a JWT, caches indefinitely
until an exported `Invalidate()` is called (hook for the future 401-retry
path — not implemented here). Concurrent callers must single-flight the
refresh. A failed helper run is never cached. The helper's stdout must never
appear in a log or error string; a failure surfaces the helper's stderr and
exit code only. A hung helper must be bounded by a timeout with an explicit,
actionable error.

## Requirements

1. **Construction**: `NewHelperCredential(command string, opts ...HelperCredentialOption) *HelperCredential`, returning a concrete type (not the interface — matches `StaticCredential`'s pattern of being a value assignable to `CredentialSource`; constructors doc says `New<Interface>` returns the interface, but existing `StaticCredential` itself is a concrete type used directly as a `CredentialSource`, so `*HelperCredential` implementing the interface is consistent). Command is run via the shell (`sh -c command`) so it supports the same command strings a user would type, matching `get-litellm-key`'s invocation as a plain path.
2. **Fresh fetch**: first `Credential` call execs the command, trims stdout, caches it, returns it.
3. **Cache hit**: a second call before expiry does not re-exec the helper.
4. **JWT expiry triggers refresh**: if cached token is a JWT (three dot-separated base64url segments) whose `exp` claim (from the payload segment) is within 300 seconds of now (or already past), the next `Credential` call re-execs the helper.
5. **Non-JWT (opaque) tokens cache indefinitely**: no `exp` to read, so no proactive refresh. `Invalidate()` clears the cache so the next call re-execs; its doc comment says the intended caller is the (future) 401-retry path.
6. **Malformed JWT-shaped tokens do not panic and are treated as opaque**: e.g. 3 segments where the payload isn't valid base64/JSON, or lacks an `exp` field — must not error or crash; must behave like a non-JWT (opaque, infinite cache) since a gateway may legitimately issue opaque tokens that happen not to be JWT-shaped, or issue a JWT without exp.
7. **Non-zero helper exit**: `Credential` returns an error including the helper's stderr output and exit code. The error string must NOT contain the helper's stdout under any circumstance (even a partial token written before failure).
8. **Timeout**: the helper is run with a bounded timeout (a few seconds, configurable via option, default short enough for a cache hit / token refresh, not for an interactive human login). On timeout, the error explicitly names the timeout and instructs the operator to run the helper by hand once, since unjira cannot complete an interactive login.
9. **Concurrency**: concurrent `Credential` calls must not each spawn a subprocess — single-flight via mutex, so N concurrent callers during a required refresh result in exactly one helper execution, and all callers observe its result (success or failure).
10. **Failure not cached**: if the helper fails (non-zero exit or timeout), the next `Credential` call retries (re-execs) rather than returning a cached error.
11. **No stdout leakage anywhere**: not in logs, not in any error message, on any path (success or failure), including the timeout and non-zero-exit paths.

## Acceptance Criteria

1. `NewHelperCredential("...")` compiles and returns `*HelperCredential`; `var _ llm.CredentialSource = (*HelperCredential)(nil)` compiles.
2. Test: helper script increments a counter file and echoes a token; first `Credential(ctx)` call returns the token and counter == 1.
3. Test: second `Credential(ctx)` call (opaque token, or JWT not yet near expiry) returns cached value; counter stays at whatever it was (no re-exec).
4. Test: cached token is a JWT with `exp` in the past (or within margin); next `Credential` call re-execs; counter increments; returns the new token.
5. Test: opaque (non-JWT) token cached; multiple calls across a simulated time gap never re-exec (no time control needed — just multiple calls).
6. Test: malformed JWT-shaped string (e.g., 3 segments, invalid base64/JSON payload, or valid JSON without `exp`) is accepted without panic/error and treated as opaque (cached, no crash).
7. Test: helper script exits non-zero and writes a distinctive string to stderr and a different distinctive string to stdout; assert returned error `ErrorContains` the stderr string and `NotContains` the stdout string.
8. Test: helper script sleeps past a short configured timeout; `Credential` returns before the sleep completes, with an error naming "timeout" and instructing the operator to run the helper manually.
9. Test (run with `-race`): N goroutines call `Credential` concurrently against a helper that needs to run (empty cache); assert helper invocation count == 1 and all goroutines receive the same successful result.
10. Test: helper fails on first call (non-zero exit), succeeds on second call; assert first call errors, second call succeeds and re-executed (i.e., failure wasn't cached forever).
11. Grep-based self-review (not an automated test): diff contains no path where stdout can reach a log/error string.

## Testing Plan

All tests in `internal/llm/helper_credential_test.go`, package `llm_test`, using `t.TempDir()` shell scripts (`#!/bin/sh`), testify (`assert`/`require`), `t.Context()`. JWTs constructed inline with a helper function `makeJWT(t, claims map[string]any) string` doing manual base64url encoding of a header + claims JSON + fake signature — comment explicitly states the signature is never verified, only `exp` is decoded, so this must not be mistaken for real JWT validation/authentication.

Cases (mirrors design spec's "Testing" section + task hazards):
- Fresh fetch (helper invoked once, correct value returned).
- Cache hit (helper NOT re-run).
- Expired-by-`exp` triggers re-run.
- Non-JWT cached indefinitely (no re-run across repeated calls).
- Malformed JWT-shaped string treated as opaque, no panic.
- Helper non-zero exit surfaces stderr+exit code, never stdout.
- Helper timeout: explicit timeout error + manual-run guidance, bounded wall-clock (assert test doesn't take as long as the sleep).
- Concurrent callers under `-race`: single-flight, helper runs exactly once.
- Failure not cached: retry succeeds on next call.
- `Invalidate()` forces the next call to re-exec even for a cached opaque token.

## Implementation Plan

1. Write `internal/llm/helper_credential_test.go` with `TestHelperCredential_FreshFetch` (helper script + counter file, assert one invocation, correct value). Run — expect compile failure (no `HelperCredential` yet).
2. Implement minimal `internal/llm/helper_credential.go`: `HelperCredential` struct (mutex, cached string, cached expiry `time.Time` — zero value meaning "no expiry known yet, must check on first call" is ambiguous with "opaque, cache forever"; use an explicit `expiresAt *time.Time`, nil = opaque/unknown, cache-forever), `NewHelperCredential`, `Credential` that always execs (no caching yet) — get the fresh-fetch test green.
3. Add caching: track cached value + whether it's still valid (opaque => always valid once fetched; JWT => check `exp` against margin). Add `TestHelperCredential_CacheHit`. Green.
4. Add JWT `exp` parsing (helper function `jwtExpiry(token string) (time.Time, bool)` — three segments, base64url-decode payload, unmarshal `{"exp": float64/int64}`, return `(time, true)` on success, `(zero, false)` on any failure — this is the "treated as opaque" fallback). Add `TestHelperCredential_ExpiredJWTRefetches` and `TestHelperCredential_MalformedJWTTreatedAsOpaque`. Green.
5. Add non-zero-exit handling: exec captures stderr separately (`cmd.Stderr = &stderrBuf`), stdout via `cmd.Output()` or separate buffer; on `*exec.ExitError`, build error from stderr + exit code, explicitly never touching stdout buffer in the error path. Add `TestHelperCredential_NonZeroExitDoesNotLeakStdout`. Green.
6. Add timeout: `exec.CommandContext` with a `context.WithTimeout` derived from an option-configurable duration (default e.g. 5s), on `ctx.Err() == context.DeadlineExceeded` return explicit timeout error naming the remedy. Add `TestHelperCredential_Timeout` (script sleeps past a short configured timeout, e.g. `WithTimeout(200*time.Millisecond)`, script sleeps 2s). Green.
7. Add single-flight concurrency guard: mutex held across the whole "check cache, maybe exec, update cache" critical section (simplest correct single-flight for this size). Add `TestHelperCredential_ConcurrentCallersSingleFlight` run under `-race`. Green.
8. Add failure-not-cached: ensure on error path, cached fields are left unset/cleared so next call retries. Add `TestHelperCredential_FailureNotCached`. Green.
9. Add `Invalidate()` method + doc comment naming the 401-retry path as intended caller. Add `TestHelperCredential_InvalidateForcesRefetch`. Green.
10. Full regression: `go build ./...`, `go test ./...`, `go test -race ./internal/llm/`, `go vet -tags=live ./...`, `golangci-lint run ./...`. Fix any findings.
11. Self-review pass per task's "SELF-REVIEW after committing" checklist (grep for stdout leakage, confirm timeout error wording, confirm single-flight by reading code, confirm failure not cached, confirm no test was weakened).
12. Commit with message per repo convention.
