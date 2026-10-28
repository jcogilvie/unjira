# Worklog sink (PR 2) Implementation Plan

> **Note:** references to CONTROLLER.md and the orchestrator documents folder are to the authoring environment, not this repo. D-37 here is the authoring environment's numbering, not the spec's D-37.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn the PR 1 token ledger into gated Jira worklogs. Each pass drafts one `worklog` action
per writable primary ticket that has unlogged attributed messages. Applying it records the Jira
worklog id on every covered ledger row. Attribution that moves after logging produces a
shrink (update) or a delete. Everything goes through the queue and the deny-by-default gate.

**Architecture:** A new package `internal/attribution` defines the `Sink` interface and builds one
`TicketState` per issue from `store.MessageAttributions` plus that issue's actions. The only sink,
`internal/attribution/worklog`, is a pure function from (`TicketState`, floor, config) to drafts.
`pipeline.RunSinks` runs after `RunReconcile`. It resolves each issue's tracker and its configured
sinks, calls each sink, and persists the drafts, superseding a pending draft where one exists. Its
rows join the auto-commit input. The gate's `Applier` gains `case "worklog"`. It reaches Jira
through an optional `tasktracker.WorklogWriter` interface, feature-detected like `SelfIdentifier`.
The applier sets or clears the ledger marker itself. Worklog actions are ticket-scoped and carry a NULL
`narrative_id`, so no narrative watermark sees them.

**Tech Stack:** Go 1.26, `modernc.org/sqlite`, testify + go-cmp, Kong, `sigs.k8s.io/yaml`
(`yaml.UnmarshalStrict`), go-jira behind `internal/clients/jira`, `runtime/debug.ReadBuildInfo`.

**Spec:** `docs/superpowers/specs/2026-10-14-token-attribution-design.md`. In scope: "Attribution sink
interface (PR 2)", "Worklog sink (PR 2)", "Open assumptions", and acceptance criteria 9-20. Decisions
are cited by ID (D-6, D-8, D-9, D-12, D-14..D-17, D-19, D-20, D-23..D-25, D-27, D-31). This plan does
not restate them. Where this plan and the spec differ, the Rulings section says why.

**Public repo.** `jcogilvie/unjira` is public. No real Jira key goes in code, tests, comments,
commits or PR text. Tests use `PROJ-1`, `PROJ-2`, `OTHER-1`.

---

## File Structure

| File | Change |
|---|---|
| `internal/config/worklog.go` (new), `worklog_test.go` (new) | `WorklogConfig`, `Date`, `SinkWorklog`, `Config.SinksFor` |
| `internal/config/config.go` | `Worklog WorklogConfig \`json:"worklog"\`` on `Config` |
| `internal/config/trackers.go`, `trackers_test.go` | `Tracker.AttributionSinks *[]string`, validation |
| `internal/store/store.go` | two `token_messages` columns, `attribution_sink_state` table |
| `internal/store/schemacheck.go` | `changeWorklogSink`, format 3, two `requiredColumns` |
| `internal/store/actions.go` | narrative id 0 ↔ NULL in insert and scan; `ActionsForIssue` |
| `internal/store/worklog.go` (new), `worklog_test.go` (new) | `ActionTypeWorklog`, `WorklogPayload`, marker functions, `SinkEnabledAt` |
| `internal/store/tokens.go`, `tokens_test.go` | `MessageAttribution.WorklogID/WorklogIssueKey` |
| `internal/store/utctime_test.go` | `TestOpen_StoreFormat` rows for format 3 |
| `internal/clients/jira/jira.go`, `jira_test.go` | `ListWorklogs`, `AddWorklog`, `UpdateWorklog`, `DeleteWorklog` |
| `internal/tasktracker/tasktracker.go`, `resolve.go`, `resolve_test.go` | `WorklogEntry`, `WorklogWriter`, routed implementation |
| `internal/clients/jira/tracker.go`, `tracker_test.go` | `jira.Tracker` implements `WorklogWriter` |
| `internal/attribution/attribution.go` (new), `attribution_test.go` (new) | `Sink`, `DraftContext`, `Draft`, `TicketState`, `BuildTicketStates`, `Totals` |
| `internal/attribution/worklog/compute.go` (new), `compute_test.go` (new) | active time, comment, numbers, property, idempotency key |
| `internal/attribution/worklog/worklog.go` (new), `worklog_test.go` (new) | `Sink.Draft`: delta, corrections, supersede, blocking |
| `internal/gate/applier.go`, `applier_test.go`, `decide_test.go` | `case "worklog"` |
| `internal/pipeline/sinks.go` (new), `sinks_test.go` (new) | `RunSinks`, `RenderSinkResult` |
| `cmd/unjira/main.go`, `watch_pass_test.go`, `main_test.go` | sink registry; wiring in `watch` and `dev narrate`; `--since` help |
| `internal/triage/triage.go`, `triage_test.go` | refuse e/t/m/s on a worklog |
| `cmd/unjira/triage.go`, `cmd/unjira/actions.go` | `bodyOf` shows a worklog; approve message names worklog |
| `internal/pipeline/tokens.go`, `tokens_test.go` | fill `Logged` and `Pending` |
| `internal/live/worklog_test.go` (new) | criterion 17 (controller) |
| docs | Task 15 |

Commands, for every task: `go test ./<pkg>/ -run '<Test>' -v`, then `go test ./...`, then
`golangci-lint run ./...`. A task is done only when all three pass.

---

## Task 1: config: `worklog` block, `Date`, `attribution_sinks`

**Tier:** standard

**Files:** `internal/config/worklog.go` (new), `internal/config/worklog_test.go` (new),
`internal/config/config.go` (`Config` at `:561`), `internal/config/trackers.go` (`Tracker` at `:75`,
`validateTracker` at `:386`), `internal/config/trackers_test.go`.

- [ ] **Test first.** Write `TestLoad_Worklog`, a table test over YAML bodies in `t.TempDir()`:
  - `worklog: {work_since: 2026-10-01, idle_gap: 20m}` parses. `WorkSince.Time()` is local midnight
    on 2026-10-01, in UTC (`time.Date(2026,10,1,0,0,0,0,time.Local).UTC()`). `IdleGap` is 20m.
  - An absent `worklog` loads. `WorkSince.IsZero()` is true and `IdleGap` is 0.
  - `work_since: 2026-10-01T00:00:00Z` is refused with an error containing `worklog.work_since` and
    `YYYY-MM-DD`.
  - `work_since: 2026-13-01` is refused the same way.
  - `idle_gap: 0s` is refused (Span already rejects non-positive values; this proves it reaches the
    struct).
  - An unknown sub-key `worklog.idle_gaps` is refused (strict mode reaches the nested struct).
- [ ] **Test first.** Write `TestValidateTrackers_AttributionSinks`, with these rows:
  - jira tracker, writable_scopes `[PROJ]`, key absent → `SinksFor` = `["worklog"]`.
  - the same with `attribution_sinks: []` → `SinksFor` = `[]` (explicitly disabled).
  - jira tracker with no writable_scopes, key absent → `SinksFor` = nil.
  - `attribution_sinks: [worklog]` on a jira tracker with no writable_scopes → refused:
    `tracker "t": attribution_sinks is set, but writable_scopes is empty: a sink only drafts writes, so it would draft nothing`.
  - `attribution_sinks: [worklog]` on a `local` or `github` tracker → refused:
    `tracker "t": attribution_sinks is set on a %s tracker: the only sink, worklog, writes Jira worklogs`.
  - `attribution_sinks: [timesheet]` → refused:
    `tracker "t": attribution_sinks names unknown sink "timesheet" (known: worklog)`.
  - `attribution_sinks: [worklog, worklog]` → refused:
    `tracker "t": attribution_sinks lists "worklog" twice`.
- [ ] Implement in `worklog.go`:
  - `type Date struct{ t time.Time }` with `UnmarshalText` (layout `2006-01-02`, parsed in
    `time.Local`), `Time() time.Time` (UTC) and `IsZero() bool`. A parse error says
    `worklog.work_since %q: want a date, YYYY-MM-DD`.
  - `type WorklogConfig struct { WorkSince Date \`json:"work_since"\`; IdleGap Span \`json:"idle_gap"\` }`.
    The doc comment says that `work_since` is not `--since` (D-19), and that IdleGap 0 means
    `worklog.DefaultIdleGap`, following the `WorkflowConfig.CacheTTL` precedent (`config.go:546`).
  - `const SinkWorklog = "worklog"` and `var knownSinks = []string{SinkWorklog}`.
  - `func (c Config) SinksFor(t Tracker) []string` implements R13's default.
  - `Tracker.AttributionSinks *[]string \`json:"attribution_sinks,omitempty"\``, validated in
    `validateTracker`, which already holds `kind`.

**ACs:** 9 (floor input), 19 (sinks are config), 20 (config half).

**Done when:** `go test ./internal/config/ -run 'TestLoad_Worklog|TestValidateTrackers_AttributionSinks|TestLoad_ShippedExampleConfigLoads' -v` passes, plus `go test ./...` and `golangci-lint run ./...`.

## Task 2: store format 3, the columns, the table, NULL narrative ids

**Tier:** standard

**Files:** `internal/store/store.go` (`token_messages` at `:280`), `internal/store/schemacheck.go`
(`:19-33`, `:55`, `:61`, `:75`), `internal/store/actions.go` (`insertActionImpl` `:70`,
`scanActions` `:350`), `internal/store/utctime_test.go` (`TestOpen_StoreFormat` `:511`),
`internal/store/worklog_test.go` (new).

- [ ] **Test first.** In `TestOpen_StoreFormat`, add these rows:
  - A format-2 store with tables is refused. The refusal names the worklog-sink change,
    `rename the database to a backup` and `re-collect`.
  - The format-0 row and the format-1 row now also expect the worklog-sink fragment.
  - The fresh-store row asserts that `token_messages.worklog_id`, `token_messages.worklog_issue_key`
    and the table `attribution_sink_state` exist.
- [ ] **Test first.** Write `TestInsertAction_ZeroNarrativeIsNull` in `worklog_test.go`:
  - Insert `ActionRow{NarrativeID: 0, Type: "worklog", IssueKey: "PROJ-1", Payload: "{}", Status: "proposed"}`. It succeeds, with no FK error.
  - `GetAction` returns `NarrativeID == 0`.
  - `ActionsByStatus("proposed")` returns it.
  - `ActionsForNarrative(n)` for a seeded narrative does not return it.
  - A raw `SELECT narrative_id IS NULL` confirms that NULL is stored.
- [ ] Implement:
  - `changeWorklogSink` = `"the worklog sink (docs/superpowers/specs/2026-10-14-token-attribution-design.md), which records on each ledger message the Jira worklog it was logged in; a store from before it has no such record, so every message it already logged would be proposed again"`.
    Add `3: changeWorklogSink` to `formatChanges`, and extend the "Formats:" doc paragraph.
  - Add `requiredColumns` entries `{"token_messages", "worklog_id", changeWorklogSink}` and
    `{"token_messages", "worklog_issue_key", changeWorklogSink}`.
  - Add the columns `worklog_id TEXT` and `worklog_issue_key TEXT`. Both are NULL until logged. Add
    `CHECK ((worklog_id IS NULL) = (worklog_issue_key IS NULL))` and an index
    `token_messages_worklog ON token_messages (worklog_issue_key, worklog_id)`.
  - Add `attribution_sink_state(tracker TEXT NOT NULL, sink TEXT NOT NULL, enabled_at DATETIME NOT NULL, PRIMARY KEY (tracker, sink))`,
    commented as "the first pass a sink ran for a tracker: its no-backfill floor (D-19)".
  - In `insertActionImpl`, bind `nil` when `a.NarrativeID == 0`. In `scanActions`, scan into
    `sql.NullInt64`. The error text `for narrative %d` stays.
  - In the schema comment, `type ... -- comment | transition | create | estimate | worklog`.

**ACs:** groundwork for 9, 10 and 12.

**Done when:** `go test ./internal/store/ -run 'TestOpen_StoreFormat|TestInsertAction_ZeroNarrativeIsNull' -v` passes, plus `go test ./...` (every store test opens a fresh format-3 store) and `golangci-lint run ./...`.

## Task 3: store worklog surface

**Tier:** standard

**Files:** `internal/store/worklog.go` (new), `internal/store/worklog_test.go`,
`internal/store/tokens.go` (`MessageAttribution` `:116`, `messageAttributionsSQL`),
`internal/store/tokens_test.go`, `internal/store/actions.go` (`ActionsForIssue`).

- [ ] **Test first**, in `worklog_test.go`:
  - `TestMarkWorklogged`:
    - Marking `m1,m2` with (`PROJ-1`, `"10001"`) sets both.
    - Marking the same ids with the same pair again is a no-op, with no error.
    - Marking `m1` with (`PROJ-1`, `"10002"`) errors naming `m1` and `10001`, and changes nothing
      (one transaction).
    - An unknown id errors naming it.
  - `TestClearWorklogMarkers`: with `m1,m2,m3` on (`PROJ-1`, `"10001"`), `keep=[m2]` clears `m1` and
    `m3` and returns 2. `keep=nil` clears all. Rows on another worklog are untouched.
  - `TestLoggedAmong`: returns `map[messageID]"PROJ-1/10001"` for marked ids only.
  - `TestActionsForIssue`: returns every action on the key in `created_at, id` order, and none from
    another key.
  - `TestSinkEnabledAt`: the first call stores and returns `now`. A later call with a later `now`
    returns the first value. A different (tracker, sink) pair is independent.
- [ ] **Test first.** Extend the attribution table test in `tokens_test.go`: a marked message reports
  `WorklogID` and `WorklogIssueKey`, and an unmarked one reports `""`.
- [ ] Implement in `worklog.go`:
  ```go
  const ActionTypeWorklog = "worklog"
  const (WorklogOpAdd = "add"; WorklogOpUpdate = "update"; WorklogOpDelete = "delete")
  // WorklogPayload is a worklog action's payload, written by the worklog sink and read by
  // gate.Applier. One definition for both (R5), here because store is the cycle-free home.
  type WorklogPayload struct {
      Op               string          `json:"op"`
      WorklogID        string          `json:"worklog_id,omitempty"` // update, delete
      Started          time.Time       `json:"started"`
      TimeSpentSeconds int             `json:"time_spent_seconds"`
      Comment          string          `json:"comment"`
      MessageIDs       []string        `json:"message_ids"` // add: to mark; update: that remain
      IdempotencyKey   string          `json:"idempotency_key"`
      Property         json.RawMessage `json:"property,omitempty"`
  }
  func (s *Store) MarkWorklogged(issueKey, worklogID string, messageIDs []string) error
  func (s *Store) ClearWorklogMarkers(issueKey, worklogID string, keep []string) (int, error)
  func (s *Store) LoggedAmong(messageIDs []string) (map[string]string, error)
  func (s *Store) SinkEnabledAt(tracker, sink string, now time.Time) (time.Time, error)
  ```
  - `MarkWorklogged` runs in one `WithTx`: `UPDATE ... WHERE message_id = ? AND (worklog_id IS NULL OR (worklog_id = ? AND worklog_issue_key = ?))`, row by row. A 0-row result reads the row to choose its error. Never overwrite another worklog's marker (R9).
  - `SinkEnabledAt`: `INSERT ... ON CONFLICT DO NOTHING`, then `SELECT`.
  - In `actions.go`, `ActionsForIssue(issueKey string) ([]ActionRow, error)` uses `actionSelect`.
  - Add `COALESCE(tm.worklog_id,'')` and `COALESCE(tm.worklog_issue_key,'')` to
    `messageAttributionsSQL`, and add `WorklogID` and `WorklogIssueKey string` to `MessageAttribution`.

**ACs:** 9 (unlogged = no marker), 10 (store half), 12 (store half).

**Done when:** `go test ./internal/store/ -run 'TestMarkWorklogged|TestClearWorklogMarkers|TestLoggedAmong|TestActionsForIssue|TestSinkEnabledAt|TestMessageAttributions' -v` passes, plus `go test ./...` and `golangci-lint run ./...`.

## Task 4: Jira client worklog methods

**Tier:** standard

**Files:** `internal/clients/jira/jira.go` (writes section `:362`, the `GetComments` paging loop
`:294-319` as the model), `internal/clients/jira/jira_test.go` (`newTestClient`/`writeJSON` `:21-41`).

- [ ] **Test first.**
  - `TestWorklogWrites_SendNotifyAndEstimateFlags` (AC14). The handler records
    `r.Method + " " + r.URL.Path + "?" + r.URL.RawQuery` for each request. Call `AddWorklog`,
    `UpdateWorklog` and `DeleteWorklog` once each. Assert:
    - three records;
    - each path `/rest/api/2/issue/PROJ-1/worklog[/10001]`;
    - each query parses (`url.ParseQuery`) to exactly `notifyUsers=false` and `adjustEstimate=leave`.
  - `TestAddWorklog_PostsBodyWithProperties`: the decoded body has
    - `started` `"2026-10-14T09:30:00.000+0000"` for a UTC input;
    - `timeSpentSeconds` 600;
    - `comment` as a plain string;
    - `properties` `[{"key":"unjira.attribution","value":{...}}]`.

    It returns the response's `id`.
  - `TestListWorklogs_WalksEveryPage`: two pages (`startAt`/`maxResults`/`total`). The query carries
    `expand=properties`. Advance by the page length. Return every worklog with its properties.
  - `TestAddWorklog_ErrorKeepsJiraStatus`: a 400 with Jira's error body yields a `*jira.Error` with
    `Status == 400` and the message.
- [ ] Implement:
  ```go
  type EntityProperty struct { Key string `json:"key"`; Value json.RawMessage `json:"value"` }
  type WorklogInput struct { Started time.Time; TimeSpentSeconds int; Comment string; Properties []EntityProperty }
  type Worklog struct { ID string `json:"id"`; Properties []EntityProperty `json:"properties"` }
  const worklogWriteFlags = "notifyUsers=false&adjustEstimate=leave"
  func (c *Client) ListWorklogs(key string) ([]Worklog, error)
  func (c *Client) AddWorklog(key string, in WorklogInput) (string, error)
  func (c *Client) UpdateWorklog(key, id string, in WorklogInput) error
  func (c *Client) DeleteWorklog(key, id string) error
  ```
  - Format `started` with `issueTimeFormat` (`tracker.go:146`; same layout).
  - Build the body as `map[string]any`, as `AddComment` does.
  - The three writes append `"?" + worklogWriteFlags` to the path. That constant is the only place
    the flags live.

**ACs:** 14, groundwork for 15.

**Done when:** `go test ./internal/clients/jira/ -run 'Worklog' -v` passes, plus `go test ./...` and `golangci-lint run ./...`.

## Task 5: `tasktracker.WorklogWriter`, on `jira.Tracker` and routed

**Tier:** standard

**Files:** `internal/tasktracker/tasktracker.go` (next to `TaskWriter` `:150-173`),
`internal/tasktracker/resolve.go` (routed `:327-395`), `internal/tasktracker/resolve_test.go`,
`internal/clients/jira/tracker.go`, `internal/clients/jira/tracker_test.go`.

- [ ] **Test first**, in `tracker_test.go` (httptest, as the file already does):
  - `TestTracker_FindWorklog`: three worklogs come back:
    - one with no properties;
    - one whose `unjira.attribution` property has `idempotency_key: "unjira-aaa"`;
    - one with `"unjira-bbb"`.

    `FindWorklog("PROJ-1","unjira-bbb")` returns its id and `true`. `"unjira-ccc"` returns `false`
    with no error. A property value that is not a JSON object is skipped, not an error, since another
    tool may write the key.
  - `TestTracker_AddWorklog_SetsTheProperty`: the POST body's `properties[0].key` is
    `unjira.attribution` and its value is the entry's `Property` bytes.
  - `TestTracker_DeleteWorklog_AbsentAfter404IsDone`:
    - DELETE answers 404, and the list shows no worklog with that id → nil.
    - DELETE answers 404, and the list still shows it → an error.
    - DELETE answers 404, and the list also 404s → an error naming the issue (R10).
  - `TestTracker_WorklogErrorsNameTheIssue`: the error from a failed add wraps
    `adding worklog to jira issue PROJ-1: ` and keeps the `*jira.Error` (`errors.As`).
- [ ] **Test first**, in `resolve_test.go`, `TestRouted_WorklogWriter`:
  - A routed tracker whose writer implements `WorklogWriter` forwards all four calls.
  - A writer that does not implement it returns
    `tracker "t" cannot write worklogs: its backend has no worklog support`.
  - An unwritable key returns the existing `cannot write %s` error.
- [ ] Implement:
  ```go
  // WorklogEntry is one worklog's content. Property is the JSON value of unjira's
  // attribution property; the backend owns the property key.
  type WorklogEntry struct { Started time.Time; TimeSpentSeconds int; Comment string; Property json.RawMessage }
  // WorklogWriter is optional on a TaskWriter, detected like SelfIdentifier (resolve.go:64).
  type WorklogWriter interface {
      FindWorklog(key, idempotencyKey string) (id string, found bool, err error)
      AddWorklog(key string, e WorklogEntry) (id string, err error)
      UpdateWorklog(key, id string, e WorklogEntry) error
      // DeleteWorklog returns nil once the worklog is absent from the issue.
      DeleteWorklog(key, id string) error
  }
  ```
  - `jira.Tracker` gets a `const worklogPropertyKey = "unjira.attribution"`. `FindWorklog` decodes
    each property value into `struct{ IdempotencyKey string \`json:"idempotency_key"\` }`.
  - `routed` gets the four methods through `t.r.Resolve(key)`, which needs `res.Tracker` for the
    message.
  - Assert `var _ tasktracker.WorklogWriter = (*jira.Tracker)(nil)` and the same for `routed`.
    `TaskWriter` is unchanged, so the six test fakes and `clients/local` compile unchanged.

**ACs:** 15 (tracker half), 16 (error carries the reason).

**Done when:** `go test ./internal/tasktracker/ ./internal/clients/jira/ -run 'Worklog' -v` passes, plus `go test ./...` and `golangci-lint run ./...`.

## Task 6: `internal/attribution`: the sink interface and ticket state

**Tier:** standard

**Files:** `internal/attribution/attribution.go` (new), `internal/attribution/attribution_test.go` (new).

- [ ] **Test first.** Write `TestBuildTicketStates`, a table over `[]store.MessageAttribution` and
  actions. Compare with `cmp.Diff`:
  - A tracked, unmarked message on `PROJ-1` lands in `PROJ-1`'s `Unlogged`.
  - A tracked message marked `PROJ-1/10001` and still tracked to `PROJ-1` lands in
    `Logged["10001"]`.
  - A message marked `PROJ-1/10001` but now tracked to `PROJ-2` lands in `PROJ-1`'s
    `Moved["10001"]`. It is in nothing on `PROJ-2` (R7).
  - A message marked `PROJ-1/10001` and now untracked, unattributed or `no_event` also lands in
    `PROJ-1`'s `Moved["10001"]`.
  - Untracked, unattributed and no_event unmarked messages produce no state.
  - `Actions` holds the actions passed for that key, unchanged.
  - States come back sorted by `IssueKey`.
- [ ] **Test first.** Write `TestTotals`:
  - Two models are summed per kind, and cost = Σ(count·rate)/1e6 per kind (the same formula as
    `pipeline/tokens.go:101`).
  - An unpriced model errors with `pricing.Lookup`'s message.
- [ ] Implement:
  ```go
  type TicketState struct {
      IssueKey string
      Unlogged []store.MessageAttribution            // tracked here, no marker
      Logged   map[string][]store.MessageAttribution // worklog id -> still tracked here
      Moved    map[string][]store.MessageAttribution // worklog id -> logged here, now not
      Actions  []store.ActionRow                     // every action on IssueKey
  }
  type DraftContext struct { Tracker config.Tracker; EnabledAt time.Time }
  // Draft is one write a sink proposes. Supersedes is the id of a pending action this
  // replaces; Row nil with Supersedes set retires that action with no replacement.
  type Draft struct { Row *store.ActionRow; Supersedes int64 }
  type Sink interface {
      Name() string
      Draft(dc DraftContext, st TicketState) ([]Draft, error)
  }
  type ModelTotal struct { Input, Output, CacheWrite, CacheRead int64; Cost float64; Price config.ModelPrice }
  func BuildTicketStates(msgs []store.MessageAttribution, actions map[string][]store.ActionRow) []TicketState
  func Totals(msgs []store.MessageAttribution, pricing config.TokenPricing) (map[string]ModelTotal, error)
  ```
  The package doc states D-31: a sink only drafts, and never writes.

**ACs:** 12 (moved detection), 19 (the interface).

**Done when:** `go test ./internal/attribution/ -v` passes, plus `go test ./...` and `golangci-lint run ./...`.

## Task 7: worklog sink computations

**Tier:** standard

**Files:** `internal/attribution/worklog/compute.go` (new), `internal/attribution/worklog/compute_test.go` (new).

- [ ] **Test first.** Write `TestActiveSeconds` (AC13), with fixed `storetest.UTC` timestamps and
  `idleGap` 15m:
  - parent messages at 09:00, 09:05, 09:10 → 600;
  - parent messages at 09:00, 09:10, 09:40, 09:45 → 600 + 300 = 900 (the 30m gap is excluded);
  - parent 09:00, subagent (`AgentID: "a1"`) 09:05, parent 09:10 → 600 (subagent messages add none);
  - a gap of exactly 15m counts, and 15m1s does not;
  - unsorted input gives the same result;
  - one parent message, or none, → 0 before the floor.
- [ ] **Test first.** Write `TestWorklogSeconds`: it returns `max(active, minWorklogSeconds)`, with
  `minWorklogSeconds = 60`.
- [ ] **Test first.** Write `TestStarted`: the earliest `OccurredAt` across all messages, subagent
  included.
- [ ] **Test first.** Write `TestHumanCount`, for exactly these values: 0→`0`, 999→`999`, 1000→`1k`,
  1234→`1.2k`, 45000→`45k`, 999949→`999.9k`, 999950→`1M`, 1200000→`1.2M`, 3000000→`3M`.
- [ ] **Test first.** Write `TestComment` (D-27). Two models:
  - the result is exactly `unjira: 1.2M in / 45k out, $3.41 (claude-opus-5-5 $3.10, claude-sonnet-5-5 $0.31)`;
  - in = input + cache_write + cache_read, and out = output;
  - models are ordered by cost descending, then by name;
  - the comment never contains a message id, a transcript id or any transcript text.
- [ ] **Test first.** Write `TestProperty` (D-24, D-25). Unmarshal into a map and compare. The keys
  are:
  - `version` = 1;
  - `idempotency_key`;
  - `unjira_version`;
  - `models.<m>.{input,output,cache_write,cache_read,cost_usd,rates.{input,output,cache_write,cache_read}}`;
  - `total_cost_usd`.

  No agent id or name appears anywhere.
- [ ] **Test first.** Write `TestIdempotencyKey`:
  - the key is stable under reordering the ids;
  - it differs by op, worklog id, issue and id set;
  - it has the form `unjira-<64 hex>`.
- [ ] Implement:
  - `const DefaultIdleGap = 15 * time.Minute`.
  - `const minWorklogSeconds = 60`, with `// ponytail: Jira's minimum accepted duration is assumed to be 60s; criterion 17's live test measures it. Change this to the measured value.`.
  - `func activeSeconds(msgs, idleGap) int`, `worklogSeconds`, `started`, `humanCount`,
    `comment(totals)`, `property(key, totals)` and `idempotencyKey(op, issue, worklogID string, ids []string) string`.
    The key is sha256 over `op\x00issue\x00worklogID\x00` plus the sorted ids, each joined with `\x00`.
  - `unjiraVersion` comes from `debug.ReadBuildInfo()`'s `Main.Version`, or `"unknown"` when it
    reports none. Use a package var so tests can pin it.

**ACs:** 13, plus the comment and properties parts of the spec.

**Done when:** `go test ./internal/attribution/worklog/ -run 'TestActiveSeconds|TestWorklogSeconds|TestStarted|TestHumanCount|TestComment|TestProperty|TestIdempotencyKey' -v` passes, plus `go test ./...` and `golangci-lint run ./...`.

## Task 8: worklog sink `Draft`: delta, corrections, one pending

**Tier:** standard. **Flag:** this is the subtlest task in the PR. The implementer must test every
branch below. The reviewer should read it first.

**Files:** `internal/attribution/worklog/worklog.go` (new), `internal/attribution/worklog/worklog_test.go` (new).

- [ ] **Test first.** Write `TestDraft_Delta` (AC9):
  - Unlogged messages at or after the floor become one `add` draft. Its `message_ids` are exactly
    those messages. `IssueKey` is the ticket, `NarrativeID` 0, `Status` proposed and `Confidence` 1.
  - Messages before `work_since` are excluded.
  - With `work_since` absent, messages before `dc.EnabledAt` are excluded.
  - No unlogged messages at or after the floor and nothing pending → no drafts.
- [ ] **Test first.** Write `TestDraft_OnePending` (AC11):
  - A pending (`proposed` or `approved`) add with the same idempotency key → no draft.
  - A pending add whose key differs (the delta grew) → one draft with `Supersedes` = the pending
    action's id.
  - A pending add, but no unlogged messages left → `Draft{Row: nil, Supersedes: id}`.
  - Two pending adds (impossible through RunSinks; built by hand) → the newest is superseded, and
    the error says the older one must be resolved by hand. Never silently pick.
- [ ] **Test first.** Write `TestDraft_BlockedByRuling` (D-12): a `rejected` or `failed` worklog
  action with the same key → no draft. Once a message is added, the key differs → a draft.
- [ ] **Test first.** Write `TestDraft_Corrections` (AC12):
  - `Moved["10001"]` non-empty and `Logged["10001"]` non-empty → an `update` draft. Its `worklog_id`
    is 10001, its `message_ids` are the remaining ones, and its time, comment and property are
    recomputed over the remaining messages.
  - `Moved["10001"]` non-empty and `Logged["10001"]` empty → a `delete` draft with `worklog_id`
    10001, no message ids, and time 0.
  - The work_since floor does not apply to corrections.
  - The pending, supersede and blocked rules apply per worklog id, keyed by the idempotency key.
  - A correction draft and an add draft coexist on one ticket.
- [ ] **Test first.** Write `TestDraft_UnpricedModelErrors`: the error names the model and the issue,
  and the result holds no drafts.
- [ ] Implement `type Sink struct{ cfg config.WorklogConfig; pricing config.TokenPricing }`,
  `func New(cfg config.WorklogConfig, pricing config.TokenPricing) *Sink`, `Name()` returning
  `config.SinkWorklog`, and `Draft` per R6, R7 and R8.
  - Rationale: `computed from the token ledger: N message(s), <first RFC3339> to <last RFC3339>`
    for an add, and `correction: M message(s) logged here are now attributed elsewhere` for an
    update or delete.
  - The floor is `cfg.WorkSince.Time()` when it is set, else `dc.EnabledAt`.

**ACs:** 9, 11, 12, and D-12's "stay unlogged".

**Done when:** `go test ./internal/attribution/worklog/ -v` passes, plus `go test ./...` and `golangci-lint run ./...`.

## Task 9: the applier's `worklog` case

**Tier:** standard. **Flag:** idempotency and marker ordering (R9, R10) must match exactly.

**Files:** `internal/gate/applier.go` (`write` `:192-208`, `checkWritable` `:447`),
`internal/gate/applier_test.go`, `internal/gate/decide_test.go`.

- [ ] **Test first.** In `applier_test.go`, add `worklogWriter`: an embedded `fakeWriter` plus a
  recording `WorklogWriter`. Keep `var _ tasktracker.WorklogWriter = (*worklogWriter)(nil)`. Seed
  ledger rows with `storetest.MessageUsage` and `UpsertMessageUsage`.
  - `TestApplier_WorklogAdd_SetsTheMarker` (AC10):
    - one `AddWorklog("PROJ-1", ...)` call with the payload's started, seconds, comment and property;
    - the row is applied;
    - every `message_ids` row has `WorklogID` = the returned id.
  - `TestApplier_WorklogAdd_RetryFindsTheExistingWorklog` (AC15): `FindWorklog` reports found
    `"10001"` → no `AddWorklog` call, the marker is set to `10001`, and the row is applied.
  - `TestApplier_WorklogAdd_ClosedIssueFailsAndStaysUnlogged` (AC16): `AddWorklog` returns
    `&jira.Error{Status: 400, Message: "..."}` → the row is failed, `Error` contains the Jira message,
    and no message is marked.
  - `TestApplier_WorklogAdd_StaleDraftRefused`: one of `message_ids` is already marked on another
    worklog → failed with `message m1 is already logged in PROJ-1/10002: this draft is stale`, and
    there are no Find or Add calls.
  - `TestApplier_WorklogAdd_MarkerFailureNamesTheWorklog`: a payload id is absent from the ledger.
    The POST happens. The row fails with a reason naming the created worklog id and saying a
    re-approve records it.
  - `TestApplier_WorklogUpdate_ClearsMovedMarkers`: `UpdateWorklog("PROJ-1","10001",...)` is called,
    the markers of the messages not in `message_ids` are cleared, and the kept ones are untouched.
  - `TestApplier_WorklogDelete_ClearsAllMarkers`.
  - `TestApplier_Worklog_UnwritableScopeRefused`: `OTHER-1` is not in writable_scopes → the existing
    `checkWritable` refusal, and no tracker call.
  - `TestApplier_Worklog_WriterWithoutWorklogSupport`: a plain `fakeWriter` → failed with
    `action %d: the configured tracker writer cannot write worklogs`.
  - `TestApplier_Worklog_MalformedPayload`: these are refused before any call:
    - an unknown `op`;
    - an add with no message ids;
    - an update or delete with no `worklog_id`;
    - an add or update with `time_spent_seconds < 1`.
- [ ] **Test first.** In `decide_test.go`, add the rows for AC18:
  - `{Type: "worklog", Confidence: 1}` with nil rules → `DecisionQueue`;
  - with `{"worklog": {Graduated: false, ConfidenceFloor: 0}}` → `DecisionQueue`.
- [ ] Implement `case store.ActionTypeWorklog: return a.applyWorklog(action)` and the
  `applyWorklog` function:
  - decode `store.WorklogPayload`, then validate;
  - call `checkWritable`;
  - type-assert `a.writer.(tasktracker.WorklogWriter)`;
  - for an add: call `LoggedAmong`, then `FindWorklog`, then `AddWorklog` if not found, then
    `MarkWorklogged`;
  - for an update: call `UpdateWorklog`, then `ClearWorklogMarkers(keep=message_ids)`;
  - for a delete: call `DeleteWorklog`, then `ClearWorklogMarkers(keep=nil)`.

  `Apply` itself is unchanged.

**ACs:** 10, 15, 16, 18.

**Done when:** `go test ./internal/gate/ -run 'Worklog|TestDecide' -v` passes, plus `go test ./...` and `golangci-lint run ./...`.

## Task 10: `pipeline.RunSinks` and wiring

**Tier:** standard

**Files:** `internal/pipeline/sinks.go` (new), `internal/pipeline/sinks_test.go` (new),
`cmd/unjira/main.go` (`runWatchPass` `:1072-1141`, the `dev narrate` reconcile `:806-828`, a new
`sinkRegistry` next to `registry` `:59`), `cmd/unjira/watch_pass_test.go`.

- [ ] **Test first.** In `sinks_test.go`:
  - `TestRunSinks_BothSinksSeeTheSameState` (AC19):
    - register two fake sinks, `a` and `b`, that record `(DraftContext, TicketState)`;
    - configure the tracker with `attribution_sinks: [worklog]`, with the registry mapping `worklog`
      to `a`; then, in a second run, configure two names, using a test-only registry
      (`map[string]attribution.Sink`) with names `a` and `b` and a config built in code that bypasses
      `Load`'s name validation;
    - both sinks receive `cmp.Equal` TicketStates.
  - `TestRunSinks_OnlyWritableScopesAndConfiguredTrackers` (AC9): a tracked message on `OTHER-1`
    (readable, not writable) and one on a tracker with `attribution_sinks: []` → the sink is never
    called for either.
  - `TestRunSinks_PersistsAndSupersedes` (AC11):
    - the first run inserts one proposed worklog;
    - after a new message is added, the second run leaves exactly one proposed worklog on the issue,
      and the first is `suppressed`;
    - the persisted rows carry real ids;
    - `CorrectionsSince` returns nothing (R8).
  - `TestRunSinks_MarkerThenDelta` (AC10):
    - run, then apply the draft with a `gate.Applier` and a fake `WorklogWriter`;
    - add a message, then run again;
    - the new draft's `message_ids` equal exactly the new message.
  - `TestRunSinks_CorrectionThenNewTicket` (AC12):
    - log m1 on `PROJ-1`, then relink the narrative's primary to `PROJ-2`;
    - the run drafts a `delete` for `PROJ-1` and nothing for `PROJ-2`;
    - apply the delete, then run again: `PROJ-2` drafts an add containing m1.
  - `TestRunSinks_EnabledAtFloor`: with no `work_since`, a message older than the first run's `now`
    is never drafted, and one after it is.
  - `TestRunSinks_OneTicketsErrorDoesNotStopOthers`: an unpriced model on one ticket → that ticket's
    error is in the joined error, and the other ticket's draft persists.
- [ ] **Test first.** In `watch_pass_test.go`:
  - `TestRunWatchPass_DefaultConfigAppliesNoWorklog` (AC18): a writable `PROJ`, ledger rows tracked
    to `PROJ-1`, and no `auto_commit` → one proposed worklog row, and the writer saw no call.
  - `TestRunWatchPass_GraduatedWorklogAutoApplies`: with `auto_commit.worklog` graduated at floor 0.9,
    the worklog is applied in the same pass.
- [ ] Implement:
  ```go
  type SinkOptions struct { Sinks map[string]attribution.Sink; Now time.Time; Log *slog.Logger }
  type SinkRunResult struct { Persisted []store.ActionRow; Superseded, Retired int }
  func RunSinks(s *store.Store, cfg config.Config, opts SinkOptions) (SinkRunResult, error)
  func RenderSinkResult(r SinkRunResult) string // "sinks: N drafted, M superseded, K retired\n"
  ```
  RunSinks does the following:
  - reads `MessageAttributions(cfg.Match.ConfidenceFloor)` once;
  - collects issue keys from `IssueKey` (tracked) and `WorklogIssueKey`, and loads `ActionsForIssue`
    for each;
  - calls `attribution.BuildTicketStates`;
  - per state, resolves the tracker with `tasktracker.ParseIssueKey`, then `cfg.TrackerForScope`,
    then `IsScopeWritable`, and skips the state if any step fails;
  - per sink in `cfg.SinksFor(tracker)`, gets the floor from `SinkEnabledAt(tracker.Name, name, opts.Now)`
    and calls `Draft`;
  - persists each draft: `Supersedes != 0 && Row != nil` → `SupersedeAction(old, StatusSuppressed, "", *Row)`;
    `Row == nil` → `UpdateActionStatus(old, StatusSuppressed)`; else `InsertAction`;
  - isolates errors per ticket with `errors.Join`.

  An unknown sink name in config with no registry entry is a startup error. Config validation makes
  it unreachable, and the test in `main_test.go` pins that every `config` known sink has a registry
  entry.
- [ ] Wire it into `cmd/unjira/main.go`:
  - `var sinkRegistry = map[string]func(config.Config) attribution.Sink{config.SinkWorklog: func(c config.Config) attribution.Sink { return worklog.New(c.Worklog, c.TokenPricing) }}`.
  - In `runWatchPass`, after a clean reconcile: call `RunSinks` and print the result; auto-commit
    `append(reconcileResult.Persisted, sinkResult.Persisted...)`; return
    `errors.Join(sinkErr, autoCommitErr)`.
  - In `dev narrate`, after a clean reconcile: call `RunSinks` and print the result. There is no
    auto-commit, as today.

**ACs:** 9, 10, 11, 12, 18, 19.

**Done when:** `go test ./internal/pipeline/ ./cmd/unjira/ -run 'RunSinks|Worklog|SinkRegistry' -v` passes, plus `go test ./...` and `golangci-lint run ./...`.

## Task 11: triage and `actions` surfaces

**Tier:** mechanical

**Files:** `internal/triage/triage.go` (`Run`'s work-verb case `:229-252`), `internal/triage/triage_test.go`,
`cmd/unjira/triage.go` (`bodyOf` `:262`), `cmd/unjira/actions.go` (`:178`).

- [ ] **Test first.** Write `TestSession_WorklogRefusesWorkVerbs`, for each of edit, target, merge
  (with the worklog at either position) and split on a batch holding a worklog:
  - the prompter is notified with exactly
    `a worklog is computed from the token ledger: edit, target, merge and split do not apply; approve, reject or skip it`;
  - the item is re-prompted;
  - the handler is never called.

  Approve, reject and skip work as for any action.
- [ ] **Test first.** Write `TestBodyOf_Worklog` in `cmd/unjira`:
  - an add shows its `comment`;
  - an update shows `update worklog 10001: <comment>`;
  - a delete shows `delete worklog 10001`.
- [ ] Implement:
  - In `Run`, before `s.handle`, check every batch position the decision touches (`i`, plus
    `d.Positions`) for `store.ActionTypeWorklog`.
  - Add a `case` in `bodyOf` keyed on `a.Type`.
  - Change the approve message to `(comment/transition/create/worklog)`.

**ACs:** none directly. It keeps triage from routing a worklog into `ReworkOne` with narrative 0.

**Done when:** `go test ./internal/triage/ ./cmd/unjira/ -run 'Worklog' -v` passes, plus `go test ./...` and `golangci-lint run ./...`.

## Task 12: token report `Logged` and `Pending`

**Tier:** mechanical

**Files:** `internal/pipeline/tokens.go` (`BuildTokenReport` `:82`, the `ModelTokens` comment `:29`),
`internal/pipeline/tokens_test.go`.

- [ ] **Test first.** Extend `TestBuildTokenReport_Messages`:
  - a tracked message with `WorklogIssueKey == IssueKey` lands in `Logged`;
  - a tracked message named in a proposed worklog add's `message_ids` lands in `Pending`;
  - a tracked message logged on another issue lands in `Unlogged` on its current ticket (R12);
  - the three sum to the ticket total.
- [ ] Implement:
  - read `ActionsByStatus(store.StatusProposed)`;
  - keep `Type == store.ActionTypeWorklog` with `Op == add`, and collect the message ids;
  - bucket each tracked message;
  - drop the "stay zero until" sentence from the `ModelTokens` comment.

**ACs:** none directly (D-26's pending column).

**Done when:** `go test ./internal/pipeline/ -run 'TestBuildTokenReport|TestRenderTokens' -v` passes, plus `go test ./...` and `golangci-lint run ./...`.

## Task 13: `--since` help names `work_since`

**Tier:** mechanical

**Files:** `cmd/unjira/main.go` (`devNarrateCmd.Since` `:707`, `watchCmd.Since` `:891`), `cmd/unjira/main_test.go`.

- [ ] **Test first.** Write `TestSinceHelpIsNotWorkSince`. Use `reflect` to read the `help` tag of
  `Since` on `devNarrateCmd` and on `watchCmd`. Each contains `worklog.work_since`.
- [ ] Set the help tags:
  - `dev narrate`: `How far back to narrate (e.g. 36h, 7d, 2w, 7d12h). Not worklog.work_since, the config date before which no worklog covers a message.`
  - `watch`: `How far back each pass narrates (e.g. 1h, 24h). Not worklog.work_since, the config date before which no worklog covers a message.`

**ACs:** 20 (help half).

**Done when:** `go test ./cmd/unjira/ -run TestSinceHelpIsNotWorkSince -v` passes, plus `go test ./...` and `golangci-lint run ./...`.

## Task 14: live DEVSBX worklog lifecycle (CONTROLLER)

**Tier:** controller. This task needs credentials and writes to DEVSBX, so it is not for an
implementer subagent.

**Files:** `internal/live/worklog_test.go` (new, `//go:build live`). Reuse `testClient(t)`,
`testProject()`, `client.CreateIssue(..., []string{jira.SeedLabel})` and `deleteIssueOnCleanup`.

- [ ] Write `TestLiveWorklogLifecycle`:
  1. GET `rest/api/2/configuration`. `t.Logf("time tracking enabled: %v", ...)`.
  2. GET `rest/api/2/mypermissions?permissions=WORK_ON_ISSUES,EDIT_OWN_WORKLOGS,DELETE_OWN_WORKLOGS&projectKey=<p>`.
     Log each permission's `havePermission`.
  3. Create a seed issue. Probe the minimum duration: `AddWorklog` with 1s, 30s, then 60s, logging
     each outcome. The smallest accepted value is the measured minimum. Delete each probe worklog
     after it is accepted.
  4. `AddWorklog` with 600s and a property. `ListWorklogs` with `expand=properties` returns the
     property and the idempotency key. `jira.Tracker.FindWorklog` finds it.
  5. `UpdateWorklog` to 300s with a new property. List again, and confirm the seconds and the
     property were replaced.
  6. `DeleteWorklog`. List again, and confirm it is gone. A second `jira.Tracker.DeleteWorklog`
     returns nil (R10).
- [ ] Run `UNJIRA_LIVE=1 go test -tags=live -run TestLiveWorklogLifecycle -v ./internal/live/`. Quote
  the three logged facts in the spec's Status section.
- [ ] If the minimum is not 60s, change `minWorklogSeconds` and remove its `ponytail:`. If PUT does
  not replace the property, stop and re-plan R11.

**ACs:** 17.

**Done when:** the live run passes and its output is quoted in the spec.

## Task 15: docs, spec status, full regression

**Tier:** mechanical

**Files:**
- `README.md`:
  - **Status** (`:38`): unjira now drafts worklogs on writable Jira trackers, on by default through
    `attribution_sinks`. They queue unless `auto_commit.worklog` is graduated, and nothing graduates
    it. Name the flags `notifyUsers=false&adjustEstimate=leave`.
  - Extend the "Three independent gates" section (`:72`) with the worklog type.
  - Add `worklog.work_since`, `worklog.idle_gap` and `trackers[].attribution_sinks` to the config
    table, and state how `work_since` differs from `--since` (AC20).
  - Remove the "Worklogs are PR 2" sentence (`:43`).
- `config/unjira.example.yaml`:
  - Add a commented `worklog:` block. `work_since` is a date, absent means no backfill, and it is
    not `--since`. `idle_gap` defaults to 15m.
  - Add a commented `attribution_sinks` line on the writable tracker example, where absent means
    `[worklog]` and `[]` disables it.
  - Add a commented `auto_commit.worklog` example left at `graduated: false`.
  - `TestLoad_ShippedExampleConfigLoads` must still pass.
- `docs/architecture.md`, in the present tense:
  - §1 gains the sink stage after reconcile;
  - §2 adds the worklog write through `Applier` and `WorklogWriter`;
  - §3 adds the `worklog` type, its NULL `narrative_id`, the `suppressed` supersede, and the ledger
    marker;
  - §4 adds `attribution` and `attribution/worklog`, and the edges `pipeline → attribution`,
    `cmd → attribution/worklog` and `gate → store.WorklogPayload`.
- `CLAUDE.md`: add one clause to the member-link invariant. An action with a NULL `narrative_id` is
  ticket-scoped (a worklog), and no narrative watermark counts it.
- The spec: change the Status line to `PR 1 landed 2026-10-14; PR 2 landed <date>; PR 3 design`.
  Add:
  - the verification evidence: the `go test ./...` result line, the `earthly +reviewable` result, and
    Task 14's three facts quoted;
  - the deviations, as one bullet each: R2 (ticket-scoped worklogs), R7 (moved messages join after
    the correction applies) and R13 (default on).

  Leave "Open assumptions" as written, and add the measured minimum duration under it.
- `docs/findings/`: open a finding (next free number, per `docs/findings/README.md`; F67 and F68
  exist, so check `git log --all` first) for whatever the reviewer leaves unsettled from the open
  questions below.
- [ ] Run `earthly +reviewable`. Quote the result line in the PR description.

**ACs:** 17 (quoted), 20 (docs half).

**Done when:** every doc above is in the diff, and `earthly +reviewable` passes.

---

## Dependency order and parallel safety

```
1 (config) ─────────────┐
2 (store fmt) → 3 (store surface) ─┬─ 6 (attribution) ─┐
                                   ├─ 7 (compute) ─────┴─ 8 (Draft) ─┐
4 (jira client) → 5 (WorklogWriter) ─────────── 9 (applier) ─────────┴─ 10 (RunSinks, wiring) → 13 (help) → 15 (docs)
                                   ├─ 11 (triage)                                                         ↑
                                   └─ 12 (token report)                       5 → 14 (live, controller) ──┘
```

- **Wave A, in parallel:** 1, 2, 4. They share no file.
- **Wave B:** 3 (after 2) and 5 (after 4), in parallel.
- **Wave C, in parallel:** 6, 7 (also needs 1), 9 (needs 3 and 5), 11 and 12. Their files are
  disjoint. 6 and 7 are in different packages. 12 touches `pipeline/tokens.go` only.
- **Wave D:** 8 (after 6 and 7). 14 can run any time after 5.
- **Wave E:** 10 (after 1, 8 and 9), then 13 (shares `cmd/unjira/main.go` with 10), then 15.

On a shared worktree, run writers one after another (CONTROLLER.md). The waves say what may run in
separate worktrees.

---

## Rulings

- **R1. The marker is two nullable columns on `token_messages`, and the enable floor is its own table.** The columns are `worklog_id` and `worklog_issue_key`. The table `attribution_sink_state` rides format 3.
  Why: D-15 says per-message. A Jira worklog id needs its issue for update and delete. The `cursors`
  table would surface the floor as a fake collector in `CursorCounts` (`cursors.go:60`).
  Cost if wrong: one more table, droppable while the store has no migrations.
- **R2. Worklog actions are ticket-scoped. Their `narrative_id` is NULL.** `ActionRow.NarrativeID` stays `int64`, with 0 ↔ NULL at the store boundary.
  Why: the narrative-keyed watermarks count all action types:
  - `linkedSinceLastAction` (`narratives.go:404`);
  - the freeze rule (`eligibility.go:33`);
  - `hasCommittedAction` (`restructure.go:72`);
  - `awaitingCreate` (`createbacklog.go:70`).

  A worklog carrying a narrative id would freeze links, hide deltas and block creates. A worklog
  also spans every narrative whose primary is the ticket.
  Cost if wrong: PR 3's approval units are per narrative (D-18), so it must join a worklog to its
  narratives through its `message_ids`. That is a query, not a schema change.
- **R3. One action type, `worklog`, with `op` add|update|delete.**
  Why: a single gate key, `auto_commit.worklog`, covers corrections, as the spec says ("corrections
  are ordinary gated actions"). It is also one applier case.
  Cost if wrong: graduating adds without graduating deletes would need a new type and a config key.
- **R4. `WorklogWriter` is an optional interface, feature-detected.**
  Why: `TaskWriter` has six fakes and `clients/local`. Feature detection follows `SelfIdentifier`.
  Only Jira can log work.
  Cost if wrong: none at runtime. A writer without it fails the action with a reason.
- **R5. `store.WorklogPayload` is one shared type,** where `comment`, `transition` and `create` use mirrored structs.
  Why: the payload carries ids the applier writes back to the ledger. A drifted mirror would mark the
  wrong rows, which is silent data loss.
  Cost if wrong: an import of `store` that gate already has.
- **R6. The delta is all of these together:**
  - the message is tracked;
  - the primary is at or above `match.confidence_floor` (D-33);
  - the issue is in `writable_scopes` (D-9);
  - the tracker's `SinksFor` includes `worklog`;
  - `worklog_id IS NULL`;
  - `occurred_at` ≥ `work_since`, or ≥ `enabled_at` when `work_since` is absent.

  Why: this is AC9 and D-19 taken literally.
  Cost if wrong: there is no backfill without a configured date, which is D-19's intent.
- **R7. Corrections first, then a delta.** A message logged on A and now not tracked to A goes into A's correction. It joins B's delta only after A's correction applies and clears its marker.
  Why: if B logs it while A's correction is pending or rejected, Jira holds it twice. Sequencing
  keeps the totals exact, which is D-17's reason.
  Cost if wrong: B's worklog lags by one apply. A rejected correction leaves the message logged on A
  until a reviewer approves one.
- **R8. One pending add per ticket, and one pending correction per worklog id.** A newer draft supersedes the pending one through `SupersedeAction(old, "suppressed", "", new)`. An identical idempotency key is skipped. A `rejected` or `failed` row with the same key blocks the draft until the set changes.
  Why: `suppressed` is not a human ruling. `markCorrection` only fires for rejected or edited rows,
  so learning never sees a supersede. Blocking keeps D-12's failure from re-proposing forever.
  Cost if wrong: a reviewer who rejects one worklog sees the next, larger one on the next pass.
- **R9. Add order: `LoggedAmong` refusal → `FindWorklog` by idempotency key → POST if absent → `MarkWorklogged`.** `MarkWorklogged` never overwrites another worklog's marker.
  Why: the retry transport never retries POST (`retry.go:99`). A "retry" is a re-approve or a later
  apply, and the find step makes it a no-op (AC15). The `LoggedAmong` refusal stops a stale draft
  from double-logging.
  Cost if wrong: one extra GET per add.
- **R10. A delete succeeds once the worklog is absent.** A 404 on DELETE is checked against `ListWorklogs`. It counts as done only if the issue lists and the id is not on it.
  Why: a delete whose marker clear failed must be re-appliable. A blanket "404 is success" would also
  pass a wrong id.
  Cost if wrong: a re-applied delete costs one extra GET.
- **R11. Properties are sent inline on POST and PUT,** under the key `unjira.attribution`, with JSON `version:1`.
  Why: it takes one request, and Task 14 verifies PUT replaces the property.
  Cost if wrong: switch to `PUT .../worklog/{id}/properties/unjira.attribution`, which is one more
  call in the tracker. Task 14 stops the PR if needed.
- **R11a (2026-10-23, D-37). Measured: POST honours inline properties; the worklog PUT silently ignores them.**
  `TestLiveWorklogLifecycle` on DEVSBX listed the POST's value ("add") after an update that sent "update".
  Resolution: `Client.UpdateWorklog` sends the worklog PUT, then sets each property through
  `PUT /rest/api/2/issue/{key}/worklog/{id}/properties/{propertyKey}` (raw value body, same
  `notifyUsers=false&adjustEstimate=leave` flags). `AddWorklog` keeps inline properties.
- **R12. The report:** `Logged` means the marker is on this issue. `Pending` means the message is in a proposed add. A message logged on another issue counts as `Unlogged` on its current ticket.
  Why: this matches R7. It will be logged there once the correction applies.
  Cost if wrong: a display difference only.
- **R13. Sinks default on for a writable Jira tracker.** When `attribution_sinks` is absent, the default is `[worklog]`. `[]` disables it. Unknown names, duplicates, non-Jira kinds and trackers with no writable scopes are refused at load.
  Why: the spec says "the worklog sink is the default for a writable Jira tracker". The gate still
  queues everything (AC18).
  Cost if wrong: an upgraded operator sees worklog proposals in triage without opting in.
- **R14. Time:**
  - parent messages are those with `AgentID == ""`;
  - consecutive gaps of at most `idle_gap` are summed;
  - the floor is `minWorklogSeconds = 60`, measured on DEVSBX 2026-10-23 (1s and 30s rejected with
    a 400, 60s accepted). Every delta shorter than that rounds up to 60s, so many short deltas log
    more than their active time. Intended;
  - each worklog is computed on its own delta, so the gap between the last message of an applied
    worklog and the first of the next delta is never counted, even under `idle_gap`. Intended: a
    delta's time depends only on its own messages, so a correction or a re-draft never changes an
    applied worklog's time;
  - `started` is the earliest covered message.

  Why: D-6, D-23 and the open assumption that a subagent-only run logs Jira's minimum.
  Cost if wrong: one constant, measured by Task 14.
- **R15. Confidence is 1.0.** The rationale names the message count and span.
  Why: no model judgment is involved, as with identity placements. Graduation is the operator's
  lever.
  Cost if wrong: an operator wanting a floor for worklogs has nothing to tune but `graduated`.
- **R16. Sinks run after a clean reconcile,** in `watch` and `dev narrate`. Their rows join auto-commit. Errors are isolated per ticket and joined.
  Why: it keeps the pass shape. A skipped pass loses nothing, since the delta is marker-based.
  Cost if wrong: a persistently failing reconcile also stalls worklogs.
- **R17. Triage refuses edit, target, merge and split on a worklog** with a re-prompt. `bodyOf` shows the comment.
  Why: `Redraft` and `Retarget` call `ReworkOne` with the narrative id, which is 0. A worklog has no
  text to redraft.
  Cost if wrong: none. Reject and skip remain.
- **R18. Worklog rejections with feedback stay in `CorrectionsSince`.**
  Why: each correction carries its `ActionType`, and a lesson only reaches the rules through `learn --keep`, which a human reviews.
  Cost if wrong: a distilled rule candidate about worklogs that the reviewer discards.

## Open questions for the user

1. **R2 vs PR 3:** worklogs carry no narrative, but D-18's approval unit is per narrative. Is a join
   through `message_ids` acceptable for PR 3?
2. **R13:** should the default be on for every existing writable Jira tracker, or opt-in?
3. **R7:** a moved message reaches B only after A's correction applies, so a rejected correction
   keeps it on A. Is that acceptable?
