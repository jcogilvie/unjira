package llm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// jwtExpiryMargin matches get-litellm-key's own internal safety margin: it
// considers its cached token stale 300 seconds before the JWT's exp claim.
// Refreshing unjira's own cache at the same threshold means unjira asks for
// a new token at roughly the moment the helper itself would have refreshed
// anyway, rather than either fighting the helper's cache (refreshing too
// early, every call) or racing it (refreshing too late, into the same 401
// window that motivated this type).
const jwtExpiryMargin = 300 * time.Second

// defaultHelperTimeout bounds how long HelperCredential waits for the
// configured command. get-litellm-key can open a browser for an interactive
// Okta login and waits up to 300 seconds for that — inside a long-running
// watch loop, waiting that long would stall every completion with no visible
// cause. This default is sized for a cache hit or a refresh-token exchange,
// both of which are sub-second in practice, not for a human clicking through
// a login page. Callers whose helper genuinely needs longer for headless
// token exchange (slow network, not an interactive prompt) can override via
// WithHelperTimeout.
const defaultHelperTimeout = 5 * time.Second

// HelperCredential is a CredentialSource that obtains its token by executing
// a configured shell command, caching the result until it is known (or
// likely) to be stale.
//
// Constructed via NewHelperCredential; see its doc comment for the caching
// and refresh rules. The zero value is not usable — always construct through
// NewHelperCredential.
type HelperCredential struct {
	command string
	timeout time.Duration

	mu        sync.Mutex
	cached    string
	haveToken bool
	// expiresAt is the JWT's exp claim minus jwtExpiryMargin. Nil means
	// either "no token cached yet" (haveToken is false) or "cached token is
	// opaque / has no readable exp" (haveToken is true, cache forever until
	// Invalidate).
	expiresAt *time.Time
}

// HelperCredentialOption configures a HelperCredential at construction.
type HelperCredentialOption func(*HelperCredential)

// WithHelperTimeout overrides the default timeout bound on the configured
// helper command. See defaultHelperTimeout for why the default is short.
func WithHelperTimeout(d time.Duration) HelperCredentialOption {
	return func(h *HelperCredential) {
		h.timeout = d
	}
}

// NewHelperCredential constructs a HelperCredential that runs command (via
// `sh -c`, so it accepts the same command strings an operator would type
// interactively — matching how get-litellm-key is documented as a plain
// path) to obtain a token.
//
// command's capability is real: anything that can write unjira's config can
// make unjira execute an arbitrary program. That is the same trust model as
// Claude Code's own apiKeyHelper, and is deliberate — see
// docs/superpowers/specs/2026-08-26-llm-credential-helper-design.md.
func NewHelperCredential(command string, opts ...HelperCredentialOption) *HelperCredential {
	h := &HelperCredential{command: command, timeout: defaultHelperTimeout}
	for _, opt := range opts {
		opt(h)
	}

	return h
}

// Compile-time proof HelperCredential satisfies the contract the correlator
// (via internal/clients/openai) consumes.
var _ CredentialSource = (*HelperCredential)(nil)

// Credential returns the cached token if it is still fresh, otherwise runs
// the configured helper and returns its (trimmed) stdout.
//
// Concurrency: guarded by a single mutex held across the whole
// check-then-maybe-refresh sequence. watch will plausibly call Credential
// from multiple goroutines at once, and two callers racing to refresh must
// not each spawn a subprocess — holding the lock for the exec is a
// deliberately simple single-flight: every other caller just waits for the
// one in-flight refresh rather than starting its own.
func (h *HelperCredential) Credential(ctx context.Context) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.haveToken && !h.isStaleLocked() {
		return h.cached, nil
	}

	token, err := h.runHelper(ctx)
	if err != nil {
		// Deliberately leave any previously cached token/expiry untouched on
		// failure so a transient error (a network blip during an Okta
		// refresh) doesn't poison the process: the next call retries rather
		// than replaying this error forever. There is one exception this
		// still handles correctly: if there was no prior cache (first call
		// failed), haveToken is already false, so the next call retries too.
		return "", err
	}

	h.cached = token
	h.haveToken = true
	h.expiresAt = jwtRefreshAt(token)

	return token, nil
}

// Invalidate clears the cached token so the next Credential call re-runs the
// helper unconditionally, regardless of any exp-based staleness check.
//
// Its intended caller is the (not yet implemented) HTTP 401 retry path in
// internal/clients/openai: an opaque token has no exp for this type to judge
// stale on its own, so a 401 is the only signal that credential is no longer
// valid, and that signal must be able to force a refresh the same way an
// expired JWT does.
func (h *HelperCredential) Invalidate() {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.haveToken = false
	h.cached = ""
	h.expiresAt = nil
}

// isStaleLocked reports whether the cached token should be treated as no
// longer usable. Must be called with h.mu held. An opaque token (expiresAt
// nil) is never stale on its own — only Invalidate (or process restart)
// clears it.
func (h *HelperCredential) isStaleLocked() bool {
	if h.expiresAt == nil {
		return false
	}

	return !time.Now().Before(*h.expiresAt)
}

// runHelper execs the configured command with a bounded timeout and returns
// its trimmed stdout.
//
// The command's stdout is a credential and must never be logged or appear in
// any returned error — a non-zero exit surfaces only stderr and the exit
// code, since a partially-written token on a failed run is still sensitive.
// stdout is captured into its own buffer specifically so it can be kept out
// of every error path below; there is no code path in this function that
// includes stdoutBuf's contents in an error.
func (h *HelperCredential) runHelper(ctx context.Context) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, "sh", "-c", h.command)

	// WaitDelay is what makes the timeout real on Linux. CommandContext kills
	// `sh` at the deadline, but because Stdout/Stderr are io.Writers rather than
	// *os.File, Go copies output through an os.Pipe — and Wait blocks until that
	// pipe closes, which happens only once every process holding its write end
	// has exited.
	//
	// Measured under a 100ms timeout, same code both platforms:
	//
	//	                       linux/amd64   darwin/arm64
	//	`sleep 2`, no WaitDelay      2.001s          102ms
	//	`sleep 5 &` + `sleep 2`      5.002s          102ms
	//	either, WaitDelay=100ms        202ms          102ms
	//
	// So Linux waits out the longest-lived process holding the pipe — including a
	// backgrounded grandchild the helper never waited for — while darwin returns
	// at the deadline regardless. That asymmetry is why this surfaced only in CI:
	// every local run masked it.
	//
	// The production consequence is a `watch` loop stalling for as long as an
	// abandoned interactive login stays open, which is precisely what this
	// timeout exists to prevent. WaitDelay bounds it: once the context is done,
	// Wait gives the pipe this long and then returns regardless.
	cmd.WaitDelay = 100 * time.Millisecond

	var stdoutBuf, stderrBuf bytes.Buffer

	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	runErr := cmd.Run()

	if runCtx.Err() != nil {
		// A DeadlineExceeded context error can also surface as a generic
		// "signal: killed" from cmd.Run(); check the context directly rather
		// than pattern-matching runErr's text.
		return "", fmt.Errorf(
			"llm credential helper %q timed out after %s: it may be waiting on an interactive login "+
				"(e.g. a browser-based Okta flow); this command cannot complete an interactive login "+
				"unattended — run it by hand once, then retry",
			h.command, h.timeout,
		)
	}

	if runErr != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](runErr); ok {
			return "", fmt.Errorf(
				"llm credential helper %q exited %d: %s",
				h.command, exitErr.ExitCode(), strings.TrimSpace(stderrBuf.String()),
			)
		}

		return "", fmt.Errorf("running llm credential helper %q: %w", h.command, runErr)
	}

	return strings.TrimSpace(stdoutBuf.String()), nil
}

// jwtRefreshAt decodes a JWT's exp claim and returns the time at which
// HelperCredential should treat the token as due for refresh (exp minus
// jwtExpiryMargin), or nil if token is not a decodable JWT with a numeric
// exp claim.
//
// This deliberately does NOT verify the token's signature — it has no key
// to verify against, and verification is the gateway's job, not the
// caller's. It only reads a claim from the middle segment. A token that
// merely resembles a JWT (three dot-separated segments) but fails to decode
// as one is treated as opaque rather than as an error: a gateway may
// legitimately issue an opaque token, and this function's caller has no way
// to distinguish "malformed JWT" from "not a JWT at all" — nor does it need
// to, since both get the same safe fallback (cache until Invalidate).
func jwtRefreshAt(token string) *time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}

	var claims struct {
		Exp float64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil
	}

	if claims.Exp == 0 {
		// Either the payload had no exp field at all (Go's zero value) or a
		// gateway genuinely set exp to the epoch, which is indistinguishable
		// from "absent" and not a lifetime any real token would have.
		// Treating it as absent, i.e. opaque, is the safe reading.
		return nil
	}

	expiresAt := time.Unix(int64(claims.Exp), 0)
	refreshAt := expiresAt.Add(-jwtExpiryMargin)

	return &refreshAt
}
