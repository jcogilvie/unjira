# internal/clients/openai Facade Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build `internal/clients/openai`, a thin facade over the official `openai-go` SDK that
gives unjira's phase-1 correlator/reconciler/rules packages a single `Complete` call against any
OpenAI-Chat-Completions-compatible endpoint (litellm, raw OpenAI, Azure OpenAI, OpenRouter,
Ollama, ...), plus the `config.LLM` fields (`Model`, `ContextWindowTokens`, base URL, credential)
those callers will need. This is phase-1 implementation slice 1 of 7 — see
`docs/superpowers/specs/2026-08-11-phase1-correlator-design.md`. No callers exist yet; this slice
produces a fully tested, working facade + config on its own.

**Architecture:** One new package (`internal/clients/openai`) wrapping `github.com/openai/openai-go/v3`'s
root package only (no Azure/AWS-auth subpackages). One new `LLMConfig` struct in `internal/config`,
following the exact pattern `TrackerConfig`/`JiraConnection` already establish in that file:
struct + JSON tag + a resolver method where a default or validation is needed. The facade's `New`
takes the base URL and API key as plain parameters — never the SDK's own default env-var loading
(`OPENAI_API_KEY`/`OPENAI_BASE_URL`), so ambient shell state can never silently override whatever
the caller passes in. This slice does not wire up a real credential source: no CLI command
constructs a real `Client` yet (there are no callers), so *where* the API key eventually comes
from (a future `UNJIRA_LLM_API_KEY` env var in `cmd/unjira`, following the
`UNJIRA_JIRA_CREDENTIALS` precedent of "unjira's own explicit config, never ambient tool env
vars") is deferred to whichever later slice first needs it — see the Verification checklist at
the end of this plan.

**Tech Stack:** Go 1.26, `github.com/openai/openai-go/v3` (root package only), `testify`
(`assert`/`require`), `net/http/httptest` (same pattern as `internal/clients/jira`'s test suite).

---

## File structure

- **Modify:** `go.mod`, `go.sum` — add `github.com/openai/openai-go/v3`.
- **Modify:** `internal/config/config.go` — add `LLMConfig` struct, `Config.LLM` field.
- **Modify:** `internal/config/config_test.go` — tests for the new config field/parsing.
- **Modify:** `config/unjira.example.json` — add a populated `llm` block.
- **Create:** `internal/clients/openai/openai.go` — the facade (`Client`, `New`, `Complete`,
  `Error`).
- **Create:** `internal/clients/openai/openai_test.go` — `httptest`-based tests, mirroring
  `internal/clients/jira/jira_test.go`'s structure (`newTestClient` helper, `writeJSON` helper,
  `assert` not `require` inside handler closures).

No existing file is large enough to need splitting for this change.

---

## Task 1: Add `github.com/openai/openai-go/v3` to go.mod

**Files:**
- Modify: `go.mod`
- Modify: `go.sum`

- [ ] **Step 1: Add the dependency**

Run (from the repo root, `/Users/jonathan.ogilvie/workspace/unjira`):

```bash
env -u GOROOT -u GOPATH go get 'github.com/openai/openai-go/v3@v3.50.0'
```

Expected output:
```
go: added github.com/openai/openai-go/v3 v3.50.0
go: added github.com/tidwall/gjson v1.19.0
go: added github.com/tidwall/match v1.1.1
go: added github.com/tidwall/pretty v1.2.1
go: added github.com/tidwall/sjson v1.2.5
```

- [ ] **Step 2: Verify go.mod only gained the expected 5 packages**

Run: `git diff go.mod`

Expected: `github.com/openai/openai-go/v3 v3.50.0` added to the top `require` block, and the 4
`tidwall` packages added to the `// indirect` block. **If you see any `github.com/aws/...` or
`github.com/Azure/...` entries, stop** — that means something imported the SDK's optional
`bedrock`/`azure` subpackages, which this facade must never do (see spec: "no Azure/AWS-auth
subpackages"). Re-check Task 2's imports.

- [ ] **Step 3: Verify the build is still clean with the new dependency present but unused**

Run: `env -u GOROOT -u GOPATH go build ./...`
Expected: no output, exit code 0 (the dependency is now resolvable but nothing imports it yet,
which is fine — `go build` doesn't require every `go.mod` entry to be imported).

- [ ] **Step 4: Commit**

```bash
git add go.mod go.sum
git commit -m "Add github.com/openai/openai-go/v3 dependency"
```

---

## Task 2: Add `LLMConfig` to `internal/config`

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing tests**

Open `internal/config/config_test.go`. Add these tests right after `TestDefaultProjectConnection_SetAndCoveredReturnsConnection`
(before `TestDefaultConfig_HasClaudeCodeEnabledByDefault`):

```go
func TestLLMConfig_Validate_RequiresModel(t *testing.T) {
	cfg := config.LLMConfig{ContextWindowTokens: 128000}

	err := cfg.Validate()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "model")
}

func TestLLMConfig_Validate_RequiresContextWindowTokens(t *testing.T) {
	cfg := config.LLMConfig{Model: "gpt-5-2"}

	err := cfg.Validate()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "context_window_tokens")
}

func TestLLMConfig_Validate_RequiresPositiveContextWindowTokens(t *testing.T) {
	cfg := config.LLMConfig{Model: "gpt-5-2", ContextWindowTokens: 0}

	err := cfg.Validate()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "context_window_tokens")
}

func TestLLMConfig_Validate_PassesWithModelAndContextWindow(t *testing.T) {
	cfg := config.LLMConfig{Model: "gpt-5-2", ContextWindowTokens: 128000}

	err := cfg.Validate()

	require.NoError(t, err)
}

func TestLoad_ParsesLLMBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unjira.config.json")
	body := `{"llm": {"model": "gpt-5-2", "base_url": "http://localhost:4000/v1", "context_window_tokens": 128000}}`
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	cfg, err := config.Load(path)

	require.NoError(t, err)
	assert.Equal(t, "gpt-5-2", cfg.LLM.Model)
	assert.Equal(t, "http://localhost:4000/v1", cfg.LLM.BaseURL)
	assert.Equal(t, 128000, cfg.LLM.ContextWindowTokens)
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `env -u GOROOT -u GOPATH go test ./internal/config/... -run 'TestLLMConfig|TestLoad_ParsesLLMBlock' -v`

Expected: `FAIL` — compile errors like `undefined: config.LLMConfig` and
`cfg.LLM undefined (type config.Config has no field or method LLM)`.

- [ ] **Step 3: Add `LLMConfig` and wire it into `Config`**

Open `internal/config/config.go`. Add this type definition right after the `TrackerConfig`
type/doc-comment block (before `// Config is unjira's top-level configuration.`):

```go
// LLMConfig configures the OpenAI-Chat-Completions-compatible endpoint the
// phase-1 correlator/reconciler/rules packages call. Model and
// ContextWindowTokens are both required, validated by Validate — not
// defaulted or looked up. Different models have different context-window
// sizes, and there's no reliable way to query that generically across every
// possible OpenAI-compatible gateway (litellm, Azure OpenAI, OpenRouter,
// Ollama, ...); a maintained model-name->context-window lookup table would
// need constant upkeep and fail *silently wrong* for any model not yet
// added. Requiring it explicitly makes a misconfigured model a loud
// config-validation error, not a silent context-overflow risk at run time.
// BaseURL and the API key (UNJIRA_LLM_API_KEY, read in cmd/unjira, not
// here) are unjira's own explicit config, never read from ambient
// OPENAI_*/ANTHROPIC_* env vars — same precedent as UNJIRA_JIRA_CREDENTIALS.
type LLMConfig struct {
	Model                string `json:"model"`
	BaseURL              string `json:"base_url"`
	ContextWindowTokens  int    `json:"context_window_tokens"`
}

// Validate reports whether Model and ContextWindowTokens are both set to
// usable values.
func (c LLMConfig) Validate() error {
	if c.Model == "" {
		return fmt.Errorf("llm.model is required")
	}
	if c.ContextWindowTokens <= 0 {
		return fmt.Errorf("llm.context_window_tokens must be a positive number of tokens")
	}

	return nil
}
```

Then add the field to `Config`. Find:

```go
	ExcludeFromLinking []string      `json:"exclude_from_linking"`
	Tracker            TrackerConfig `json:"tracker"`
	DBPath             string        `json:"db_path"`
}
```

Replace with:

```go
	ExcludeFromLinking []string      `json:"exclude_from_linking"`
	Tracker            TrackerConfig `json:"tracker"`
	LLM                LLMConfig     `json:"llm"`
	DBPath             string        `json:"db_path"`
}
```

- [ ] **Step 4: Run `gofmt` (the struct field alignment above is illustrative, not final)**

Run: `env -u GOROOT -u GOPATH gofmt -w internal/config/config.go`

- [ ] **Step 5: Run tests to verify they pass**

Run: `env -u GOROOT -u GOPATH go test ./internal/config/... -v`

Expected: every test in the package passes, including all 4 new `TestLLMConfig_Validate_*` tests
and `TestLoad_ParsesLLMBlock`.

- [ ] **Step 6: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "Add LLMConfig with required Model/ContextWindowTokens validation"
```

---

## Task 3: Populate `config/unjira.example.json` with an `llm` block

**Files:**
- Modify: `config/unjira.example.json`

- [ ] **Step 1: Add the `llm` block**

Open `config/unjira.example.json`. Find:

```json
  "tracker": {
    "backend": "jira",
    "default_project": "PROJ"
  },
  "db_path": "data/unjira.db"
}
```

Replace with:

```json
  "tracker": {
    "backend": "jira",
    "default_project": "PROJ"
  },
  "llm": {
    "model": "gpt-5-2",
    "base_url": "http://localhost:4000/v1",
    "context_window_tokens": 128000
  },
  "db_path": "data/unjira.db"
}
```

`base_url` here is a placeholder pointing at a typical local litellm proxy port — every real
deployment sets this to their own gateway's URL. `model`/`context_window_tokens` are a real,
correct pairing (GPT-5.2's actual 128k-token context window) so someone copying this file gets a
working number, not a fictional one, even if they haven't yet picked their own model.

- [ ] **Step 2: Verify the file is still valid JSON**

Run: `python3 -c "import json; json.load(open('config/unjira.example.json'))" && echo VALID`

Expected: `VALID`

- [ ] **Step 3: Verify `config.Load` still parses the full example file without error**

This is a quick manual check, not a new automated test (the example file's shape is already
covered by `TestLoad_ParsesExampleShapedConfig`-style tests; this just confirms the *actual* file
on disk parses, catching a hand-edit typo the unit test's inline JSON string wouldn't catch).

Run:
```bash
cd /tmp && cat > check_example.go <<'EOF'
package main

import (
	"fmt"
	"github.com/jcogilvie/unjira/internal/config"
)

func main() {
	cfg, err := config.Load("/Users/jonathan.ogilvie/workspace/unjira/config/unjira.example.json")
	if err != nil {
		panic(err)
	}
	fmt.Printf("%+v\n", cfg.LLM)
}
EOF
cd /Users/jonathan.ogilvie/workspace/unjira && env -u GOROOT -u GOPATH go run /tmp/check_example.go
rm /tmp/check_example.go
```

Expected: `{Model:gpt-5-2 BaseURL:http://localhost:4000/v1 ContextWindowTokens:128000}`

- [ ] **Step 4: Commit**

```bash
git add config/unjira.example.json
git commit -m "Add populated llm config block to the example config"
```

---

## Task 4: Create the `internal/clients/openai` package skeleton + `New`

**Files:**
- Create: `internal/clients/openai/openai.go`
- Create: `internal/clients/openai/openai_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/clients/openai/openai_test.go`:

```go
// Package openai_test exercises the facade against a real HTTP server
// (httptest), rather than stubbing the SDK's internals — the SDK's own wire
// behavior is covered by its own tests; these tests cover the logic that is
// unjira's own: request construction, response extraction, error
// translation.
package openai_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/clients/openai"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) *openai.Client {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	return openai.New(server.URL, "test-key", "gpt-5-2")
}

func TestNew_ReturnsUsableClient(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	require.NotNil(t, client)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `env -u GOROOT -u GOPATH go test ./internal/clients/openai/... -v`

Expected: `FAIL` — `internal/clients/openai/openai_test.go:12:2: no non-test Go files in
.../internal/clients/openai` (the package doesn't exist yet).

- [ ] **Step 3: Write the minimal implementation**

Create `internal/clients/openai/openai.go`:

```go
// Package openai is a thin facade over the official openai-go SDK, speaking
// OpenAI-Chat-Completions-compatible wire format — the shape litellm, Azure
// OpenAI, OpenRouter, Ollama, and most other self-hosted/third-party
// gateways already speak natively, unlike Anthropic's narrower Messages
// API. See docs/superpowers/specs/2026-08-11-phase1-correlator-design.md
// for why this shape was chosen over Anthropic's.
//
// Every client is constructed with an explicit base URL and API key —
// never the SDK's own default OPENAI_API_KEY/OPENAI_BASE_URL env-var
// loading — so unjira's own config is always what's used, matching the
// "our own explicit config, never reused ambient credentials" precedent
// set by UNJIRA_JIRA_CREDENTIALS.
package openai

import (
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// Client is a facade over openai-go, exposing only the surface unjira needs.
type Client struct {
	upstream openai.Client
	model    string
}

// New constructs a Client pointed at baseURL, authenticating with apiKey,
// making every completion call against model.
func New(baseURL, apiKey, model string) *Client {
	upstream := openai.NewClient(
		option.WithBaseURL(baseURL),
		option.WithAPIKey(apiKey),
	)

	return &Client{upstream: upstream, model: model}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `env -u GOROOT -u GOPATH go test ./internal/clients/openai/... -v`

Expected: `PASS` — `TestNew_ReturnsUsableClient` passes.

- [ ] **Step 5: Commit**

```bash
git add internal/clients/openai/openai.go internal/clients/openai/openai_test.go
git commit -m "Add internal/clients/openai package skeleton with New"
```

---

## Task 5: Implement `Complete`

**Files:**
- Modify: `internal/clients/openai/openai.go`
- Modify: `internal/clients/openai/openai_test.go`

- [ ] **Step 1: Write the failing tests**

Add to `internal/clients/openai/openai_test.go` (imports will need `encoding/json` and
`github.com/stretchr/testify/assert` — add both to the import block):

```go
func writeJSON(t *testing.T, w http.ResponseWriter, status int, body any) {
	t.Helper()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// assert, not require: this runs inside an httptest handler goroutine,
	// where require's FailNow (runtime.Goexit) wouldn't fail the test itself.
	assert.NoError(t, json.NewEncoder(w).Encode(body))
}

func TestComplete_ReturnsMessageContent(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{
			"id":      "chatcmpl-1",
			"object":  "chat.completion",
			"created": 1,
			"model":   "gpt-5-2",
			"choices": []map[string]any{
				{
					"index":         0,
					"finish_reason": "stop",
					"message": map[string]any{
						"role":    "assistant",
						"content": "the answer is 4",
					},
				},
			},
		})
	})

	result, err := client.Complete(t.Context(), "You are helpful.", "What is 2+2?")

	require.NoError(t, err)
	assert.Equal(t, "the answer is 4", result)
}

func TestComplete_SendsSystemAndUserMessages(t *testing.T) {
	var gotBody map[string]any

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		writeJSON(t, w, http.StatusOK, map[string]any{
			"id": "chatcmpl-1", "object": "chat.completion", "created": 1, "model": "gpt-5-2",
			"choices": []map[string]any{
				{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": "ok"}},
			},
		})
	})

	_, err := client.Complete(t.Context(), "sys prompt", "user prompt")

	require.NoError(t, err)
	messages, ok := gotBody["messages"].([]any)
	require.True(t, ok)
	require.Len(t, messages, 2)

	sysMsg, ok := messages[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "system", sysMsg["role"])
	assert.Equal(t, "sys prompt", sysMsg["content"])

	userMsg, ok := messages[1].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "user", userMsg["role"])
	assert.Equal(t, "user prompt", userMsg["content"])

	assert.Equal(t, "gpt-5-2", gotBody["model"])
}

func TestComplete_ErrorTranslatedToOpenAIError(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusTooManyRequests, map[string]any{
			"error": map[string]any{
				"message": "rate limited",
				"type":    "rate_limit_exceeded",
				"code":    "rate_limit_exceeded",
			},
		})
	})

	_, err := client.Complete(t.Context(), "sys", "user")

	require.Error(t, err)
	var apiErr *openai.Error
	require.ErrorAs(t, err, &apiErr)
	// Assert against the struct's own Message field, not Error()'s string
	// rendering — the SDK docs confirm Message is populated from the
	// response body's error message, but don't specify Error()'s exact
	// format, so asserting on the field directly is the more reliable check.
	assert.Equal(t, "rate limited", apiErr.Message)
	assert.Equal(t, 429, apiErr.StatusCode)
}
```

Also add `"github.com/openai/openai-go/v3"` to the test file's import block (needed for the
`*openai.Error` type assertion in the last test) — note this is the upstream SDK package, imported
directly in the test to assert on its error type; the facade package itself is
`github.com/jcogilvie/unjira/internal/clients/openai`, already imported under the name `openai` in
this test file. **This creates a name collision** — resolve it by importing the upstream SDK
under an alias:

```go
import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	upstreamopenai "github.com/openai/openai-go/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/clients/openai"
)
```

And update the last test to use the alias:

```go
	var apiErr *upstreamopenai.Error
	require.ErrorAs(t, err, &apiErr)
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `env -u GOROOT -u GOPATH go test ./internal/clients/openai/... -v`

Expected: `FAIL` — `client.Complete undefined (type *openai.Client has no field or method Complete)`.

- [ ] **Step 3: Write the minimal implementation**

Add to `internal/clients/openai/openai.go` (after the `New` function):

```go
// Complete sends one non-streaming, single-turn chat completion request and
// returns the assistant's reply text.
func (c *Client) Complete(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	resp, err := c.upstream.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model: c.model,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(systemPrompt),
			openai.UserMessage(userPrompt),
		},
	})
	if err != nil {
		return "", fmt.Errorf("completing chat prompt: %w", err)
	}

	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("completing chat prompt: response had no choices")
	}

	return resp.Choices[0].Message.Content, nil
}
```

Add `"context"` and `"fmt"` to the top of the import block:

```go
import (
	"context"
	"fmt"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `env -u GOROOT -u GOPATH go test ./internal/clients/openai/... -v`

Expected: `PASS` on all 4 tests (`TestNew_ReturnsUsableClient`, `TestComplete_ReturnsMessageContent`,
`TestComplete_SendsSystemAndUserMessages`, `TestComplete_ErrorTranslatedToOpenAIError`).

**Note on the error test:** `Complete` wraps the SDK's own error with `fmt.Errorf("...: %w", err)`,
so `errors.As` still finds the underlying `*openai.Error` (aliased `upstreamopenai.Error` in the
test) through the wrap chain — `%w` preserves that. If this test fails with "error is not an
`*openai.Error`," check that `fmt.Errorf` used `%w` and not `%v` in Step 3's code.

- [ ] **Step 5: Commit**

```bash
git add internal/clients/openai/openai.go internal/clients/openai/openai_test.go
git commit -m "Implement Complete against openai-go's Chat Completions API"
```

---

## Task 6: Full regression + lint

**Files:** none (verification only)

- [ ] **Step 1: Run the full offline test suite**

Run: `env -u GOROOT -u GOPATH go test ./... -v 2>&1 | tail -60`

Expected: every package reports `ok`, including the new `internal/clients/openai` and the
modified `internal/config`.

- [ ] **Step 2: Run `go vet`**

Run: `env -u GOROOT -u GOPATH go vet ./...`

Expected: no output, exit code 0.

- [ ] **Step 3: Run the full lint+test gate**

Run: `earthly +reviewable`

Expected: `0 issues` from `golangci-lint`, all tests pass, overall `SUCCESS`. If lint finds
anything (e.g. a missing doc comment on an exported identifier, an import-order issue `gci`
wants fixed), fix it and re-run this step before moving on — do not commit past a red
`+reviewable`.

- [ ] **Step 4: Confirm no accidental Azure/AWS dependency crept in**

Run: `grep -E 'Azure|aws-sdk-go' go.mod`

Expected: no output (empty grep match) — confirms Task 1's "root package only" constraint held
through every subsequent task's imports.

- [ ] **Step 5: Commit if Step 3 required fixes**

If `earthly +reviewable` was clean on the first run in Step 3, there's nothing new to commit —
skip this step. Otherwise:

```bash
git add -A
git commit -m "Fix lint findings in internal/clients/openai"
```

---

## Task 7: Update the phase-1 spec's status for this slice (optional but recommended)

**Files:**
- Modify: `docs/superpowers/specs/2026-08-11-phase1-correlator-design.md`

- [ ] **Step 1: Mark slice 1 as landed**

Open `docs/superpowers/specs/2026-08-11-phase1-correlator-design.md`. Find the "Implementation
slices" section's first item:

```
1. **`internal/clients/openai`** — facade + tests, no callers yet. Includes `config.LLM.
   {Model, ContextWindowTokens}` and the populated `config/unjira.example.json` values.
```

Replace with:

```
1. **`internal/clients/openai`** — ✅ landed. Facade + tests, no callers yet. Includes
   `config.LLM.{Model, ContextWindowTokens}` and the populated `config/unjira.example.json`
   values. See `docs/superpowers/plans/2026-08-11-openai-client-facade.md` for the implementation
   plan.
```

- [ ] **Step 2: Commit**

```bash
git add docs/superpowers/specs/2026-08-11-phase1-correlator-design.md
git commit -m "Mark phase-1 slice 1 (openai client facade) as landed"
```

---

## Verification checklist (spec coverage for this slice)

- [x] `internal/clients/openai` speaks OpenAI-Chat-Completions shape, not Anthropic Messages —
  Task 4/5.
- [x] Root package only, no Azure/AWS subpackages imported — Task 1 Step 2, Task 6 Step 4.
- [x] Config carries required `Model`/`ContextWindowTokens`, validated, not defaulted/looked up —
  Task 2.
- [x] `BaseURL`/API key are unjira's own explicit config, not ambient env vars — `New`'s signature
  in Task 4 takes them as explicit parameters; the SDK's own env-loading is never invoked (no
  bare `openai.NewClient()` call anywhere in this facade).
- [x] `config/unjira.example.json` ships a populated, working example — Task 3.
- [x] Tested the same way as `clients/jira` (`httptest`, `assert` not `require` inside handler
  goroutines, error-translation test) — Task 4/5.

Not in scope for this slice (deferred to later slices per the spec's own sequencing): `Cluster`,
`Reconcile`, `Distill`, the split-by-time/tail-summarization overflow logic, `watch`/`triage`
commands, the lease lock. `UNJIRA_LLM_API_KEY` env var wiring in `cmd/unjira` is also deferred —
this slice's `Complete`/`New` take the API key as a plain parameter; the CLI-level credential
plumbing (mirroring `UNJIRA_JIRA_CREDENTIALS`'s Kong `env:` tag pattern) lands with whichever
later slice first constructs a real `openai.Client` from `cmd/unjira`.
