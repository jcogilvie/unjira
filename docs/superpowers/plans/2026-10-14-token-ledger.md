# Token ledger, attribution and `unjira tokens` (PR 1) Implementation Plan

> **Note:** references to CONTROLLER.md and the orchestrator documents folder are to the authoring environment, not this repo.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Record what each Claude Code assistant message cost, attribute it at read time through
member links to each narrative's primary issue, record unjira's own LLM spend, and report both through
a read-only `unjira tokens`.

**Architecture:** The claude_code collector extracts one ledger row per assistant `message.id` from the
owned lines it already segments, and writes the rows through the store it already uses for cursors.
Each row names the segment event that covers it in the newest snapshot. The store keeps two new tables
(`token_messages`, `llm_calls`) behind a store-format bump. Attribution is a read-time join:
message → covering event → `member` link → `primary` row. unjira's own calls are recorded by one
`llm.Client` decorator, wrapped at the single construction point `appContext.llmClient()`. The pipeline
stage comes from a context value set at each `pipeline.Run*` entry. `pipeline.RenderTokens` prices
rows from `config.TokenPricing`, and `unjira tokens` prints the result.

**Tech Stack:** Go 1.26, `modernc.org/sqlite`, testify + go-cmp, Kong, `sigs.k8s.io/yaml`
(`yaml.UnmarshalStrict`), `openai-go/v3` v3.50.0.

**Spec:** `docs/superpowers/specs/2026-10-14-token-attribution-design.md`. Sections in scope: "Facts
this contract rests on", "Ledger (PR 1)", "`unjira tokens` (PR 1, read-only)", "Open assumptions",
acceptance criteria 1–8. Decisions are cited by ID (D-3, D-4, D-5, D-10, D-24, D-26, D-29). This plan
does not restate them.

---

## READ THIS FIRST: three corrections to the spec, measured 2026-10-14

These come from a throwaway probe. It ran the collector's own `discoverTranscripts`,
`buildLineOwners` and `segments` (floor `DefaultMinSegmentMessages`) over every transcript under
`~/.claude/projects`: 1,413 files (1,184 subagents), 155,321 usage lines, 77,089 distinct owned
`message.id`s. The probe was deleted afterwards. Counts only.

### Correction 1: usage is NOT identical across a message's lines

The spec's second fact says every line carries "an identical full copy" of its usage. That is false
for **17,475 of 77,089 ids (23%)**. The differing keys are mostly `output_tokens` (18,552
line-pairs) and the sub-objects (`iterations`, `server_tool_use`, `speed`, `inference_geo`). A few
differ on `input_tokens` (20), `cache_creation_input_tokens` (21) and `cache_read_input_tokens` (11).
In **every** differing id, the last line's four counts are ≥ every earlier line's.

So the upsert takes the **per-counter MAX**, never the first or the last value written:
`ON CONFLICT(message_id) DO UPDATE SET output_tokens = MAX(output_tokens, excluded.output_tokens), …`.
MAX does not depend on order, so it is idempotent. It also converges when a snapshot taken
mid-stream holds only the early lines. Criterion 1's fixture (one id repeated with identical usage)
still holds, and Task 3 adds a second fixture whose usage grows.

### Correction 2: `<synthetic>` messages

513 ids carry `message.model = "<synthetic>"`, and every one of their counts is zero. These are
Claude Code's own locally generated assistant lines, not model calls. Under D-10 an unpriced model is
an error, so ledgering them would make `unjira tokens` fail for every user unless the user prices a
fake model. The collector skips them through a named constant, with this measurement in its doc
comment, and a test pins it (Task 4). **Deviation from the spec: record it in the Status section.**

### Correction 3: some messages have no covering event

49 ids sit only in a segment that `sessionEvents` does not emit (`len(seg.userTexts) == 0`,
`claudecode.go:300`). Segments under an excluded cwd are also never emitted (`claudecode.go:303`;
the probe did not count these). If ledger rows travelled on events, those messages would never
reach the store, which is silent loss. Hence finding (a) below. Such a row has a NULL covering event
and is reported as unattributed, with the reason "no event".

### Findings the brief asked to resolve

**(a) How rows reach the store.** The collector writes them directly with
`cc.Store.UpsertMessageUsage(rows)`. That is the same handle and the same pattern it already uses for
cursors (`claudecode.go:126-147`, and `pipeline/collect.go:19` says so in its doc). The write happens
per transcript, **before** `SetCursor`, so a failed write leaves the cursor in place and the next pass
re-reads the file. The upsert is idempotent, so the re-read is safe.

Rejected alternatives:
- **An `Artifacts` entry that the store explodes.** It loses the rows of every non-emitted segment
  (correction 3). It also bloats `events.artifacts`, which every event reader unmarshals
  (`store/events.go:140-161`, `linkkind.go:365`, `narratives.go:231`). The three largest transcripts
  hold 2,289–4,519 ids, so each snapshot event would grow by hundreds of KB.
- **A typed field on `events.Event`.** Same loss. It is also never populated on read, so a reader
  would see an empty field and could not tell why.
- **Changing the `Collector` interface.** It touches every collector and fake for one producer.

The invariant holds: extraction is a pure function of lines the collector already reads (owned
lines only, `claudecode.go:264-271`). There is no judgment and no network. Attribution is not
written. It is computed at read time (Task 5). Note this in `CLAUDE.md`'s collector invariant (Task 10).

**(b) Mapping a message to its covering event.** Within one snapshot, the segments partition the
owned lines contiguously. `rawRuns` appends every line to the open run (`segments.go:143`), and
`absorb` only merges a run into an adjacent one (`segments.go:209-263`). So each message line sits in
exactly one segment `i`. That segment's event id is `fmt.Sprintf("%s:%d:%d", t.id, stat.Size(), i)`
(`claudecode.go:323`). Extract that into `segmentExternalID(t, size, i)` and use it in both places, so
the ledger and the event cannot drift apart.

The row stores `covering_external_id` for the snapshot being written. A re-snapshot overwrites it,
which is how "newest" works: a transcript is only ever re-read larger (`claudecode.go:125-131`). A
segment that does not emit an event writes NULL, and the upsert **does not overwrite** a non-NULL
value with NULL (`COALESCE(excluded.covering_external_id, covering_external_id)`).

Segment indices shifting between snapshots (`segments.go:189-272`) do not matter, because the id is
recomputed on every snapshot. The covering event is resolved at read time with
`events.source = 'claude_code' AND external_id = covering_external_id`. That is a fact about lines, not
an attribution, so storing it does not contradict "attribution is computed, never copied".

**(c) Can a `message.id` straddle a segment boundary?** It does not today: **0 of 77,089 ids, in
0 of 1,413 files**. The open assumption's fallback ("charge the segment holding its first line") is
still the implemented rule, because it is free: the upsert sees the first line's segment first and
never moves a row to a later segment within the same snapshot. Task 4 pins it with a test.

Two related counts:
- 267 ids have their lines interleaved with another id's lines. This is harmless, because rows are
  keyed by id, not by position.
- **239 ids appear in two subagent transcripts** (always 2 subagent transcripts, never a root). With
  one row per `message.id` (D-3), the transcript and covering event of such a message are whichever
  of the two was read last. That is deterministic for a given pass, but it is an open question below.

**(d) Refusing old stores.** New tables never trip `checkRequiredColumns`. `columnMissing` returns
false for a table that does not exist yet (`schemacheck.go:141-150`), so an old store would open and
get empty ledger tables. Its transcripts are already at their cursor (`claudecode.go:130`), so they
are never re-read, and their spend would be **silently absent**. So bump `storeFormat` to 2
(`schemacheck.go:48`), and replace the single `hasTables && format < storeFormat` clause
(`schemacheck.go:121-124`) with a list indexed by format: `{1: changeUTCTimestamps, 2: changeTokenLedger}`.
The refusal then names every change newer than the store's format. The message pattern is unchanged
("predates …; rename the database to a backup … and re-collect"), as criterion 7 asks.

## File Structure

| File | Change |
|---|---|
| `internal/config/config.go`, `config_test.go` | `TokenPricing map[string]ModelPrice \`json:"token_pricing"\``, validated in `Load` |
| `internal/store/store.go` | `token_messages`, `llm_calls` tables |
| `internal/store/schemacheck.go` | `storeFormat = 2`, per-format change list |
| `internal/store/tokens.go` (new), `tokens_test.go` | `UpsertMessageUsage`, `RecordLLMCall`, `MessageAttributions`, `LLMCallsSince` |
| `internal/store/storetest/storetest.go` | `MessageUsage` builder |
| `internal/store/utctime_test.go` | `TestOpen_StoreFormat` rows |
| `internal/collector/claudecode/usage.go` (new), `usage_test.go` | extraction of rows from owned segment lines |
| `internal/collector/claudecode/claudecode.go` | `segmentExternalID`; `sessionEvents` also returns rows; `Collect` writes them before `SetCursor` |
| `internal/collector/claudecode/fixture_test.go` | `Assistant(ts, msgID, model, usage)` builder method |
| `internal/llm/llm.go`, `llm/recording.go` (new), `recording_test.go` | `Usage.CachedPromptTokens`; `WithStage`/`StageOf`; `NewRecordingClient` |
| `internal/clients/openai/openai.go`, `openai_test.go` | map `PromptTokensDetails.CachedTokens` |
| `internal/pipeline/{narrate,match,reconcile,learn}.go`, `internal/triage/restructure.go` | set the stage on ctx at entry |
| `internal/pipeline/tokens.go` (new), `tokens_test.go` | `RenderTokens` |
| `cmd/unjira/main.go`, `main_test.go` | wrap in `llmClient()`; `Tokens tokensCmd` in `cli` (`main.go:1113-1120`) |
| docs | Task 10 |

---

## Task 1: `config.TokenPricing`

**Tier:** standard

**Files:** `internal/config/config.go` (the `Config` struct at `:559-594`, `Load` at `:728-771`),
`internal/config/config_test.go`.

- [ ] **Test first.** Write `TestLoad_TokenPricing` as a table test against YAML bodies written to
  `t.TempDir()`:
  - A valid block with two models parses, and `cmp.Diff` matches the expected `ModelPrice` values.
  - An unknown sub-key (`input_per_millon`) is refused. `UnmarshalStrict` should already do this, so
    the test proves the strictness reaches the nested struct.
  - A missing rate is refused, naming both the model and the field. Use `*float64` fields, so an
    absent rate is not a silent $0 (D-10).
  - A negative rate is refused.
  - An absent `token_pricing` loads, so a fresh clone still works. The error surfaces only when
    `unjira tokens` meets a model (Task 9).
- [ ] Add a `TokenPricing.Lookup(model string) (ModelPrice, error)` test. Its error must name the
  model and `token_pricing`. The four fields are `input`, `output`, `cache_write` and `cache_read`,
  all per million (D-10).
- [ ] Implement. Validate in `Load`, next to `ReaskBudgets` (`config.go:766`).

**Done when:** `go test ./internal/config/` passes, and `golangci-lint run ./internal/config/` is clean.

## Task 2: store format 2, the two tables, and the refusal

**Tier:** standard

**Files:** `internal/store/store.go` (the schema const `:35-280`), `internal/store/schemacheck.go`,
`internal/store/utctime_test.go` (`TestOpen_StoreFormat` `:511`).

- [ ] **Test first.** In `TestOpen_StoreFormat`, add these rows:
  - `{Format: 1, Tables: true, Without: []string{"token_messages"}}`. The refusal names the token
    ledger change, `rename the database to a backup` and `re-collect`. `token_messages` is in
    `absent`.
  - The format-0 row now expects both the UTC-timestamps fragment and the token-ledger fragment.
  - Add a `token_messages` / `llm_calls` existence assertion to the fresh-store row.
- [ ] Add `changeTokenLedger` next to the other change constants (`schemacheck.go:19-30`). It should
  say that a store collected before the ledger lacks rows for every transcript it has already read.
  Bump `storeFormat` to 2, and update its "Formats:" doc paragraph.
- [ ] Add a per-format change list, and generalise the clause at `:121-124`.
- [ ] Add the schema:
  - `token_messages(message_id TEXT PRIMARY KEY, transcript_id TEXT NOT NULL, agent_id TEXT,
    occurred_at DATETIME NOT NULL, model TEXT NOT NULL, input_tokens, output_tokens,
    cache_write_tokens, cache_read_tokens INTEGER NOT NULL CHECK (>= 0), covering_external_id TEXT)`.
    Add an index on `covering_external_id`.
  - `llm_calls(id INTEGER PRIMARY KEY, called_at DATETIME NOT NULL, stage TEXT NOT NULL,
    model TEXT NOT NULL, prompt_tokens, completion_tokens INTEGER NOT NULL, cached_tokens INTEGER)`.
    `cached_tokens` is NULL when the server reported none (spec "unjira's own spend"). Add an index on
    `called_at`.
  - Comment each column the way `narrative_events` is commented. Times are bound as `time.Time`
    (package doc, `store.go:16-21`).

**Done when:**
- `go test ./internal/store/` passes.
- Every existing store test still passes. They open fresh stores, which are created at format 2.

## Task 3: `UpsertMessageUsage` and `RecordLLMCall`

**Tier:** standard

**Files:** `internal/store/tokens.go`, `internal/store/tokens_test.go`, and the `MessageUsage`
builder in `internal/store/storetest/storetest.go`, following `EventBuilder` (`:67-100`).

- [ ] **Test first.** Write `TestUpsertMessageUsage`, a table over sequences of upserts that
  compares `MessageUsageRows()` with `cmp.Diff`:
  - The same row twice gives one row. This is criterion 1, store half.
  - Usage that grows (output 10, then 40) gives 40. Sequenced the other way, it still gives 40
    (correction 1).
  - The covering id moves to a newer snapshot's id.
  - A NULL covering id does not erase a set one.
  - `occurred_at` keeps the earliest timestamp.
- [ ] Write `TestRecordLLMCall`: one row per call, `called_at` exactly as passed (no `time.Now`
  inside, for criterion 6's clock), and NULL `cached_tokens` round-trips as nil.
- [ ] Implement with `ON CONFLICT(message_id) DO UPDATE` and per-counter `MAX`. The covering id uses
  `COALESCE`. Upsert a batch inside `WithTx` (`store.go:327`).

**Done when:** the tests pass, and an upsert of 4,500 rows completes in one transaction.

## Task 4: the collector extracts and writes ledger rows

**Tier:** complex

**Files:**
- `internal/collector/claudecode/usage.go` (new), `usage_test.go`
- `claudecode.go`: `sessionEvents` `:243-371`, `Collect` `:118-154`, the ExternalID `:321-326`
- `fixture_test.go`: add `Assistant(ts, msgID, model string, usage map[string]any)` after `ToolUse`
  `:115`. It writes `type:"assistant"` with `message.id`, `message.model` and `message.usage`.

- [ ] **Test first** (black-box, through `Collect`, as `claudecode_test.go:46` opens a store). Write
  these tests:
  - `TestCollect_LedgersOneRowPerMessageID`:
    1. One id spans three lines with identical usage. Collect, collect again, then append lines and
       collect again.
    2. Expect one row whose counts equal that id's usage, and whose covering id equals the newest
       snapshot's segment ExternalID. This is criterion 1.
  - `TestCollect_LedgerTakesMaxAcrossLines`: same id, output 3 then 120, gives 120.
  - `TestCollect_SubagentRowsCarryAgentID`: rows from `WriteSubagent` carry the agent id, and a
    covering id prefixed `agent-<id>:`. This is the D-4 half of criterion 2.
  - `TestCollect_LedgersMessagesWithoutEvent`: a transcript whose segment is skipped (no user text,
    or an excluded cwd) still ledgers its rows with a NULL covering id (correction 3).
  - `TestCollect_SkipsSyntheticMessages`: correction 2.
  - `TestCollect_ResumedCopyLedgersOnce`: lines copied by `Resumed(...)` are ledgered only from their
    owner, because extraction reads `owned` (`claudecode.go:266`).
  - `TestCollect_LedgerWriteFailureLeavesCursor`: a closed store makes `Collect` error before
    `SetCursor`, so the file is re-read next pass.
- [ ] Unit-test the pure extractor, `messageUsages(segs []segment, ext func(i int) string) []store.MessageUsage`:
  - A message whose lines are split across segments is charged to its first segment (finding (c)).
  - An assistant line without `message.id` or `message.usage` yields no row, and is counted for a
    `cc.Log` debug line, never dropped silently.
- [ ] Implement:
  1. Extract `segmentExternalID`.
  2. Have `sessionEvents` return rows alongside events, using the **same** `stat.Size()` it uses for
     ExternalIDs (`:291`).
  3. In `Collect`, call `cc.Store.UpsertMessageUsage` before `SetCursor` (`:147`).
  4. Update the doc of `CollectContext` (`pipeline/collect.go:14-22`) to say claude_code also writes
     ledger rows.
- [ ] Re-run `HIDDEN_AUTHORING_PROBE` and `go test ./internal/collector/...`. Nothing else may change.

**Done when:**
- All of the above pass.
- `go run ./cmd/unjira collect` on a fresh store fills `token_messages`, and the `sqlite3` row count
  equals the probe's distinct-id count (within the backfill window, minus `<synthetic>`).

## Task 5: read-time attribution

**Tier:** complex

**Files:** `internal/store/tokens.go`, `tokens_test.go`. Reuse `storetest.Seed`, `Event` and
`Narrative` (`storetest.go:67-229`) plus the new `MessageUsage` builder.

- [ ] **Test first.** Write `TestMessageAttributions`, a table test. Each row seeds events,
  narratives, links, primaries and messages, then compares a `[]MessageAttribution{MessageID,
  IssueKey, Connection, NarrativeID, State}` with `cmp.Diff`. The state is `tracked`, `untracked`,
  `unattributed` or `no_event`. Rows:
  - A parent event and a subagent event in different narratives, each with a primary. Each message
    follows its own event's member link. This is criterion 2.
  - After `Tx.MoveMember` (`linkkind.go:99`), the same message reports the new narrative's issue
    with no re-upsert. This is criterion 3.
  - After the primary is relinked (replace the `role='primary'` row, `narrativeissues.go`), the
    message reports the new key. This is also criterion 3.
  - A narrative with no primary reports `untracked` with its narrative id. This is criterion 4.
  - A covering event with only a `context` link, or none, reports `unattributed`. This is
    criterion 4.
  - A NULL covering id, or one whose event was never inserted, reports `no_event`.
  - Two snapshot events both containing the message: only the row's covering id is used, never an
    older snapshot's link.
- [ ] Implement it as one query:
  `token_messages LEFT JOIN events ON source='claude_code' AND external_id=covering_external_id`,
  then `LEFT JOIN narrative_events ON kind='member'`, then `LEFT JOIN narrative_issues ON role='primary'`.
  The `kind = 'member'` filter is mandatory (CLAUDE.md: context must not reach attribution). Nothing
  is cached.
- [ ] **Decision before coding** (open question 1): whether a primary below
  `match.confidence_floor` counts as tracked. Implement whatever the reviewer rules, and pin it with a
  row.

**Done when:**
- The table passes.
- `go vet`/lint are clean.
- The query plan uses the `covering_external_id` index (`EXPLAIN QUERY PLAN` in a test comment, or
  quoted in the PR).

## Task 6: `llm.Usage` reports cached prompt tokens

**Tier:** mechanical

**Files:**
- `internal/llm/llm.go`: `Usage` `:36-42`. Add `CachedPromptTokens *int64`. A pointer, because "not
  reported" differs from zero (spec "cached tokens if the server reports them").
- `internal/clients/openai/openai.go`: `:123-127`. Map from `resp.Usage.PromptTokensDetails.CachedTokens`.
  It is present only when `resp.Usage.PromptTokensDetails.JSON.CachedTokens.Valid()` (SDK
  `completion.go:240-245`).
- `internal/clients/openai/openai_test.go`

- [ ] **Test first.** Write two httptest responses, one with `prompt_tokens_details.cached_tokens: 7`
  (→ `ptr(7)`) and one without it (→ nil).
- [ ] Check that no caller compares `Usage` with `==`. Use `grep -rn 'Usage{}' internal`. A pointer
  field keeps the struct comparable, but it changes what equality means.

**Done when:** `go test ./internal/llm/ ./internal/clients/openai/ ./internal/correlator/` passes.

## Task 7: the recording decorator and the stage context

**Tier:** standard

**Files:** `internal/llm/recording.go`, `recording_test.go`, `internal/store/tokens.go` (the
`RecordLLMCall` method from Task 3 satisfies the interface).

- [ ] **Test first.** Write `TestRecordingClient` against a fake `llm.Client` and a fake recorder,
  with an injected `now func() time.Time`:
  - A success records one `CallRecord{At, Stage, Model, Prompt, Completion, Cached}`.
  - A `finish_reason=length` error that carries usage still records, and returns the original error
    (openai `:139-150`).
  - An error with zero usage records zeros, so every call writes one row (criterion 6).
  - The stage comes from `llm.WithStage(ctx, "cluster")`. An unset stage records `unstaged`, never
    an empty string.
  - The model recorded is the **configured** model passed to the constructor, not `Usage.Model`
    (open assumption: own calls are priced by their configured name).
  - A recorder error is returned wrapped, and the completion text is still returned with it. The
    store a recorder cannot write is the same SQLite file the pass is about to write, so carrying on
    would only fail later.
- [ ] Add `var _ llm.Client = (*RecordingClient)(nil)`. The constructor is
  `NewRecordingClient(inner Client, rec Recorder, model string, now func() time.Time) Client`, which
  returns the interface (go-conventions "Interfaces and constructors"). `internal/llm` must not
  import `internal/store`: `Recorder` is declared in `llm`.

**Done when:** `go test ./internal/llm/` passes. `go list -deps ./internal/llm` shows no `internal/store`.

## Task 8: wire the decorator once and label the stages

**Tier:** standard

**Files:**
- `cmd/unjira/main.go`: `llmClient()` `:430-450` wraps `openai.New(...)` in
  `llm.NewRecordingClient(…, a.store, a.config.LLM.Model, time.Now)`. This is the only construction
  point. Its callers are `learn.go:33`, `main.go:681`, `main.go:868` and `triage.go:423`.
- Set the stage at entry:
  - `pipeline.RunNarrate` (`narrate.go:138`) → `cluster`
  - `RunMatch` (`match.go:64`) → `match`
  - `RunReconcile` (`reconcile.go:143`) → `draft`
  - `RunLearn` (`learn.go:93`) → `distil`
  - triage `NewStoreHandler` calls (`restructure.go:154`) → `triage`

  The names follow D-29's list.

- [ ] **Test first.** Write `TestRunStages_LabelContext` in `internal/pipeline`. A capturing fake
  `llm.Client` records `llm.StageOf(ctx)` for each call. Drive each `Run*` with the existing fakes
  (`narrate_test.go`, `match_test.go`, `reconcile_test.go`, `learn_test.go`) and assert the stage
  set. Add one triage test.
- [ ] Write a `cmd/unjira` test: `llmClient()` returns a `*llm.RecordingClient`. Use an env key and a
  base URL, and make no network call.

**Done when:**
- `go test ./...` passes.
- One `go run ./cmd/unjira dev narrate --dry-run` against a real gateway leaves one `llm_calls` row
  per completion, compared with the pass's reported call count.

## Task 9: `pipeline.RenderTokens` and `unjira tokens`

**Tier:** standard

**Files:** `internal/pipeline/tokens.go`, `tokens_test.go` (modelled on `RenderDigest`,
`pipeline/digest.go`), and `cmd/unjira/main.go`. Add `Tokens tokensCmd \`cmd:"" help:"…"\`` to `cli`
(`:1113-1120`), with `Run(app *appContext) error` following `statusCmd` (`:525-553`).

- [ ] **Test first.** Write table tests over a seeded store, with `cmp.Diff` on a structured
  `TokenReport`. Render the text separately, with one golden assertion. Cases:
  - **Per ticket and per model:** unlogged tokens and cost, with logged and pending zero (spec
    "`unjira tokens`"). Cost = Σ count × rate / 1e6 over the four kinds (D-10, D-24).
  - **The untracked bucket**, per narrative, and the **unattributed** messages (`unattributed` plus
    `no_event`) (criterion 4, output half).
  - **A model missing from `token_pricing`** errors with its name, for a transcript model and for
    the configured LLM model alike (criterion 5).
  - **Own spend:** `30d`, `7d` and `1d` are rolling, and `today` is the local calendar day. Use an
    injected `now` and a fixed `time.Location`, with calls placed just inside and just outside each
    edge (criterion 6). Own-call cost is (prompt − cached) × input + cached × cache_read +
    completion × output. Cached is 0 when nil (open question 3).
- [ ] Write a `cmd/unjira` test: `kong.Parse` accepts `tokens`, and `Run` on an empty store prints a
  "no ledger rows yet, run: unjira collect" line, as `statusCmd` does.

**Done when:**
- The tests pass.
- `go run ./cmd/unjira tokens` against a store collected in Task 4 prints per-ticket totals with no
  error once `token_pricing` names every model seen.

## Task 10: docs, spec status, full regression

**Tier:** mechanical

**Files:**
- `README.md`: the CLI block (`:153`), the config-key table (`:265` area) and the Status section
  (`:38`). Add `unjira tokens` and `token_pricing`, and say plainly that PR 1 writes nothing to Jira.
- `config/unjira.example.yaml`: add a commented `token_pricing:` block with one Claude model and
  placeholder rates, saying that unknown models error.
- `docs/architecture.md`:
  - §1, the pipeline as data flow: the ledger, read-time attribution and the recording decorator.
  - §4, the package graph: `llm`'s new `Recorder`, `cmd → llm.NewRecordingClient`, `pipeline/tokens`.
  - Present tense only.
- `docs/superpowers/specs/2026-10-14-token-attribution-design.md`: change Status to
  `**Status: PR 1 landed <date>; PR 2 and PR 3 design**` and add a Status section with:
  - the verification evidence, quoted: the row count from Task 4, the call count from Task 8, and
    `go test ./...` output;
  - the three corrections above as deviations (MAX upsert, `<synthetic>` skipped, `no_event` state).

  Leave "Open assumptions" as written, and note there that (c) measured 0 straddles.
- `CLAUDE.md`: the collector invariant gains one clause. The claude_code collector writes
  per-message ledger rows through the store it holds for cursors: deterministic, extraction only.
- `docs/findings/`: open a finding for the 239 ids duplicated across subagent transcripts if
  reviewers don't settle it in this PR (rules in `docs/findings/README.md`).

- [ ] Run `earthly +reviewable` (lint + test). Quote the result line in the PR.

**Done when:** every item of criterion 8 is in the diff, and `earthly +reviewable` passes.

---

## Rulings (user, 2026-10-14) — these override the questions below

1. **Below-floor primary is untracked.** A narrative whose primary is below `match.confidence_floor`
   (or has none) goes to the untracked bucket, labeled with its candidate primary key. Same rule as
   the create path. Task 5 implements this and pins it with a test.
2. **First holder wins** for an id seen in two transcripts: the first-recorded transcript and covering
   event keep the row (F33's rule). A later transcript with the same id may still raise counters via
   MAX, but it never reassigns `transcript`/`covering_external_id`. Test: read order A,B and B,A give
   the same owner after the first pass.
3. **Excluded cwds are not ledgered at all.** No rows for messages in a segment under an excluded cwd.
   Segments that are never emitted for other reasons (the 49 ids) are still ledgered as `no_event`.
4. **Cached own-spend tokens:** if the gateway reports none, cached = 0 and every prompt token is
   priced at the input rate. That is a conservative overestimate, and the `unjira tokens` output says so.
   No blocker.

- R1. "First holder" is the order-independent minimum of (occurred_at, transcript_id) across the
  transcripts that record the id. The upsert takes ownership only when the incoming pair sorts lower.
  Why: plain "first upsert wins" depends on file read order, so A,B and B,A would disagree. Cost if
  wrong: a late-collected earlier transcript can move ownership once, which PR 2 would see as a
  correction; it can't flip back and forth.

- R2. `MessageAttribution` embeds `MessageUsage` (model + four counters), not only the five planned
  fields. Why: Task 9 prices rows and would otherwise need a second query joined back on message_id.
  Cost if wrong: a wider struct; easy to narrow later.
- R3. Task 5's "query plan uses the covering_external_id index" check is dropped for the full read; the
  real plan (a scan of token_messages plus a lookup on events' unique index) is documented in a comment,
  and the index stays for PR 2's per-event lookups. Why: a full read can't use that index. Cost if
  wrong: an unused index costs some write overhead.

- R4. Ruling 4 made precise: `llm.Usage.CachedPromptTokens` (the plan's name) is nil when the gateway
  reports none, and the store keeps NULL. Pricing (Task 9) treats nil as 0 cached, so every prompt token
  is priced at the input rate, and `unjira tokens` says the figure is an upper bound when any row in a
  window has NULL. Why: nil records "not reported", which a stored 0 would erase. Cost if wrong: none
  to the data; the rendered caveat is cosmetic.

- R5. `store.RecordLLMCall` keeps taking `store.LLMCall`. Task 8 adds a small adapter in `cmd/unjira`
  that maps `llm.CallRecord` to `store.LLMCall`, passing `Cached` through as is (nil stays nil). Why:
  neither `llm` nor `store` should import the other for one struct, and cmd is already the wiring
  layer. Cost if wrong: one small mapping function.

- R6. The triage stage label is set in `triage.NewSession`, not `NewStoreHandler`, and a recluster done
  inside triage is recorded as `triage`. Why: the handler has no context of its own, and D-29 only
  requires own spend to be counted and never attributed. The label records who spent it. Cost if wrong:
  a stage label to change.

- R7. Task 10 also corrects the `--dry-run` help for `dev narrate` and `watch` (cmd/unjira/main.go
  ~695, ~880) and README.md:156. Dry-run proposes nothing and writes no actions, but it still stores
  events and `llm_calls` rows. Why: Task 8's review found these strings overstate what dry-run skips.
  Cost if wrong: help text.

- R8. A resumed transcript's copied history is not ledgered when its original (earliest) holder is
  older than the collection horizon. Ownership gives those lines to the original, which is never read.
  This is intended, and it matches events: the tokens were spent on the original's date, outside the
  window. The spec lists it under "What is not ledgered", and the Task 4 comparison cites it as the
  cause of the gap. No finding is opened. Why: diagnosis showed the 181 missing ids are exactly the
  resumed copies of d8159df7's lines, which ended 09-02. Cost if wrong: spend older than backfill_days
  that arrives through a resume is not counted, so widening backfill on a fresh store recovers it.

The spec must carry these and the plan's three corrections (per-counter MAX, `<synthetic>` skip,
`no_event` state). Task 10 edits the spec's design and acceptance sections to match, with no
"previously" language.

## Open questions for the reviewer (answered above)

1. **Untracked vs. confidence floor.** The spec says a narrative "with no primary" is untracked. The
   repo's settled meaning since F58/F64 is that no **confident** primary is untracked
   (`config.go:359-374`: "A narrative with no primary at all is treated as one below the floor").
   Should a below-floor primary charge its ticket? I recommend no, for consistency with the create
   path, but the spec says yes. This blocks Task 5.
2. **239 ids in two subagent transcripts.** With one row per id, the row's transcript and covering
   event are those of the last transcript read. Is that acceptable, or should the first holder win,
   as F33's ownership rule does?
3. **Own-spend cost with cached tokens.** OpenAI-style `prompt_tokens` includes `cached_tokens`. The
   formula above assumes that, and assumes the gateway reports cache reads there. Unverified for
   litellm-fronted Claude.
4. **Excluded cwds.** Their messages are ledgered as `no_event` (correction 3). Should a cwd the
   user excluded be left out of the ledger entirely instead?
