package llm_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/llm"
)

// writeScript writes a shell script to dir/name, made executable, and
// returns its path.
func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(body), 0o700))

	return path
}

// countingHelperScript writes a script that appends one line to a counter
// file (so a test can assert invocation count via readCounter) and echoes
// token to stdout.
func countingHelperScript(t *testing.T, dir, token string) (scriptPath, counterPath string) {
	t.Helper()

	counterPath = filepath.Join(dir, "counter")
	script := fmt.Sprintf("#!/bin/sh\necho invoked >> %q\necho %q\n", counterPath, token)
	scriptPath = writeScript(t, dir, "helper.sh", script)

	return scriptPath, counterPath
}

// readCounter returns the number of invocation lines recorded by a script
// written by countingHelperScript. A missing file (never invoked) counts as
// zero rather than an error.
func readCounter(t *testing.T, counterPath string) int {
	t.Helper()

	data, err := os.ReadFile(counterPath) //nolint:gosec // test-owned temp path
	if os.IsNotExist(err) {
		return 0
	}
	require.NoError(t, err)

	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return 0
	}

	return len(lines)
}

// makeJWT builds a JWT-shaped string carrying claims, for tests only. The
// signature segment is a fixed placeholder and is NEVER verified by
// HelperCredential — only the exp claim in the payload is decoded. Do not
// mistake this for real JWT authentication; it exists purely to exercise the
// exp-parsing path with inputs shaped like what a real gateway would issue.
func makeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()

	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))

	payloadBytes, err := json.Marshal(claims)
	require.NoError(t, err)
	payload := base64.RawURLEncoding.EncodeToString(payloadBytes)

	return header + "." + payload + ".unverified-test-signature"
}

func TestHelperCredential_FreshFetch(t *testing.T) {
	dir := t.TempDir()
	script, counter := countingHelperScript(t, dir, "tok-abc123")

	cred := llm.NewHelperCredential(script)

	got, err := cred.Credential(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "tok-abc123", got)
	assert.Equal(t, 1, readCounter(t, counter), "helper must run exactly once for a fresh fetch")
}

func TestHelperCredential_CacheHit(t *testing.T) {
	dir := t.TempDir()
	// An opaque (non-JWT) token: no exp to ever go stale, so a second call
	// must be a pure cache hit.
	script, counter := countingHelperScript(t, dir, "opaque-token-xyz")

	cred := llm.NewHelperCredential(script)

	first, err := cred.Credential(t.Context())
	require.NoError(t, err)

	second, err := cred.Credential(t.Context())
	require.NoError(t, err)

	assert.Equal(t, first, second)
	assert.Equal(t, 1, readCounter(t, counter), "second call must be a cache hit, not a re-exec")
}

func TestHelperCredential_ExpiredJWTTriggersRefetch(t *testing.T) {
	dir := t.TempDir()

	almostExpired := makeJWT(t, map[string]any{"exp": time.Now().Add(-1 * time.Minute).Unix()})
	freshScript, freshCounter := countingHelperScriptReturning(t, dir, []string{almostExpired, "tok-after-refresh"})

	cred := llm.NewHelperCredential(freshScript)

	got1, err := cred.Credential(t.Context())
	require.NoError(t, err)
	assert.Equal(t, almostExpired, got1)
	assert.Equal(t, 1, readCounter(t, freshCounter))

	got2, err := cred.Credential(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "tok-after-refresh", got2, "an already-expired JWT must trigger an immediate re-fetch")
	assert.Equal(t, 2, readCounter(t, freshCounter))
}

func TestHelperCredential_ValidJWTIsCached(t *testing.T) {
	dir := t.TempDir()
	longLived := makeJWT(t, map[string]any{"exp": time.Now().Add(1 * time.Hour).Unix()})
	script, counter := countingHelperScript(t, dir, longLived)

	cred := llm.NewHelperCredential(script)

	got1, err := cred.Credential(t.Context())
	require.NoError(t, err)
	got2, err := cred.Credential(t.Context())
	require.NoError(t, err)

	assert.Equal(t, longLived, got1)
	assert.Equal(t, longLived, got2)
	assert.Equal(t, 1, readCounter(t, counter), "a JWT far from expiry must be cached, not re-fetched")
}

func TestHelperCredential_JWTWithinMarginTriggersRefetch(t *testing.T) {
	dir := t.TempDir()
	// Within the 300s safety margin but not yet expired: must still refresh,
	// matching get-litellm-key's own internal margin so unjira refreshes at
	// about the moment the helper itself would consider the token stale.
	nearExpiry := makeJWT(t, map[string]any{"exp": time.Now().Add(30 * time.Second).Unix()})
	script, counter := countingHelperScriptReturning(t, dir, []string{nearExpiry, "tok-refreshed"})

	cred := llm.NewHelperCredential(script)

	got1, err := cred.Credential(t.Context())
	require.NoError(t, err)
	assert.Equal(t, nearExpiry, got1)

	got2, err := cred.Credential(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "tok-refreshed", got2, "a token within the 300s margin must be refreshed proactively")
	assert.Equal(t, 2, readCounter(t, counter))
}

func TestHelperCredential_MalformedJWTTreatedAsOpaque(t *testing.T) {
	// Each of these is JWT-shaped enough to reach the exp-decoding path but
	// must not panic, must not error, and must be cached like a genuine
	// opaque token — a gateway may legitimately issue tokens that look like
	// this by coincidence (e.g. an opaque token with two literal dots).
	cases := []struct {
		name  string
		token string
	}{
		{"three segments, payload not base64", "not-base64.not-base64-either.sig"},
		{
			"three segments, payload not JSON",
			"aGVhZGVy." + base64.RawURLEncoding.EncodeToString([]byte("not json")) + ".sig",
		},
		{
			"three segments, JSON payload with no exp field",
			"aGVhZGVy." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"user"}`)) + ".sig",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			script, counter := countingHelperScript(t, dir, tc.token)
			cred := llm.NewHelperCredential(script)

			require.NotPanics(t, func() {
				got1, err := cred.Credential(t.Context())
				require.NoError(t, err)
				assert.Equal(t, tc.token, got1)

				got2, err := cred.Credential(t.Context())
				require.NoError(t, err)
				assert.Equal(t, tc.token, got2)
			})
			assert.Equal(t, 1, readCounter(t, counter),
				"malformed-JWT-shaped token must be treated as opaque and cached, not re-fetched")
		})
	}
}

func TestHelperCredential_NonZeroExitDoesNotLeakStdout(t *testing.T) {
	dir := t.TempDir()
	const secretPartialToken = "PARTIAL-SECRET-TOKEN-abc123"
	const distinctiveStderr = "okta-refresh-failed: refresh_token expired"

	script := writeScript(t, dir, "helper.sh", fmt.Sprintf(`#!/bin/sh
echo %q
echo %q >&2
exit 7
`, secretPartialToken, distinctiveStderr))

	cred := llm.NewHelperCredential(script)

	_, err := cred.Credential(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), distinctiveStderr, "error must surface the helper's stderr")
	assert.Contains(t, err.Error(), "7", "error must name the exit code")
	assert.NotContains(t, err.Error(), secretPartialToken,
		"error must never contain the helper's stdout, even a partially-written token")
}

// TestHelperCredential_Timeout pins the bound that stops a hung helper from
// stalling a watch loop.
//
// The helper backgrounds a child that outlives it and inherits its stdout —
// get-litellm-key's shape when it launches a browser. On Linux that makes Wait
// block on the pipe until the *grandchild* exits (5s here), well past the 100ms
// deadline; cmd.WaitDelay is what cuts it short.
//
// HONEST LIMITATION: this test cannot fail on darwin, which returns at the
// deadline whether or not WaitDelay is set (measured 102ms in all four
// combinations — see runHelper's table). So a local green run proves nothing
// about this behaviour; only the containerised `earthly +reviewable` or CI does.
// Kept as a regression guard for the platform where it bites rather than
// weakened into something that passes everywhere for the wrong reason.
func TestHelperCredential_Timeout(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, "helper.sh",
		"#!/bin/sh\nsleep 5 &\nsleep 2\necho should-not-see-this\n")

	cred := llm.NewHelperCredential(script, llm.WithHelperTimeout(100*time.Millisecond))

	start := time.Now()
	_, err := cred.Credential(t.Context())
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "timeout",
		"a hung helper must produce an error explicitly naming the timeout")
	assert.Contains(t, err.Error(), "run", "error should tell the operator to run the helper by hand once")
	assert.Less(t, elapsed, 1*time.Second,
		"Credential must return promptly after the deadline, not wait out a backgrounded child")
}

func TestHelperCredential_ConcurrentCallersSingleFlight(t *testing.T) {
	dir := t.TempDir()
	script, counter := countingHelperScript(t, dir, "shared-token")

	cred := llm.NewHelperCredential(script)

	const n = 20

	var wg sync.WaitGroup

	results := make([]string, n)
	errs := make([]error, n)

	for i := range n {
		wg.Add(1)

		go func(idx int) {
			defer wg.Done()

			results[idx], errs[idx] = cred.Credential(t.Context())
		}(i)
	}

	wg.Wait()

	for i := range n {
		require.NoError(t, errs[i])
		assert.Equal(t, "shared-token", results[i])
	}

	assert.Equal(t, 1, readCounter(t, counter), "concurrent callers racing an empty cache must spawn exactly one helper")
}

func TestHelperCredential_FailureNotCached(t *testing.T) {
	dir := t.TempDir()
	failFlag := filepath.Join(dir, "fail-once")
	script := writeScript(t, dir, "helper.sh", fmt.Sprintf(`#!/bin/sh
if [ -f %q ]; then
  echo tok-after-recovery
  exit 0
fi
touch %q
echo boom >&2
exit 1
`, failFlag, failFlag))

	cred := llm.NewHelperCredential(script)

	_, err := cred.Credential(t.Context())
	require.Error(t, err)

	got, err := cred.Credential(t.Context())
	require.NoError(t, err, "a failed fetch must not poison the cache for subsequent calls")
	assert.Equal(t, "tok-after-recovery", got)
}

func TestHelperCredential_InvalidateForcesRefetch(t *testing.T) {
	dir := t.TempDir()
	// Opaque token: without Invalidate, this would be cached forever.
	script, counter := countingHelperScriptReturning(t, dir, []string{"tok-v1", "tok-v2"})

	cred := llm.NewHelperCredential(script)

	got1, err := cred.Credential(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "tok-v1", got1)

	cred.Invalidate()

	got2, err := cred.Credential(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "tok-v2", got2, "Invalidate must force the next call to re-run the helper")
	assert.Equal(t, 2, readCounter(t, counter))
}

// countingHelperScriptReturning writes a script that returns tokens[0] on
// its first invocation, tokens[1] on its second, and so on (repeating the
// last entry if invoked more times than len(tokens)), while still recording
// invocation count via readCounter.
func countingHelperScriptReturning(t *testing.T, dir string, tokens []string) (scriptPath, counterPath string) {
	t.Helper()

	counterPath = filepath.Join(dir, "counter")
	tokensPath := filepath.Join(dir, "tokens")
	require.NoError(t, os.WriteFile(tokensPath, []byte(strings.Join(tokens, "\n")+"\n"), 0o600))

	script := fmt.Sprintf(`#!/bin/sh
echo invoked >> %q
n=$(wc -l < %q | tr -d ' ')
total=$(wc -l < %q | tr -d ' ')
if [ "$n" -gt "$total" ]; then
  n=$total
fi
sed -n "${n}p" %q
`, counterPath, counterPath, tokensPath, tokensPath)
	scriptPath = writeScript(t, dir, "helper.sh", script)

	return scriptPath, counterPath
}
