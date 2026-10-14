# Token attribution for tracked tickets — design

**Status: PR 1 landed 2026-10-14; PR 2 and PR 3 design** (contract from a grilling session, 2026-10-14)

This is the spec. An implementation plan points at it instead of restating it.

## Status

PR 1 landed 2026-10-14. PR 2 and PR 3 are design.

Deviations from the design as first written, measured by a probe over every local transcript (see
"Facts"):

- **Per-counter MAX upsert,** not "identical copies": 23% of ids carry growing usage across lines.
- **`<synthetic>` messages are skipped.** They are not model calls, and ledgering them would make
  `unjira tokens` fail for every user under D-10.
- **`no_event` state.** Messages in a segment that emits no event are still ledgered, with no
  covering event, rather than lost.

Rulings made during planning, all reflected in "Design" below: a below-floor primary is untracked;
the first holder of a doubly-recorded id is the lowest `(occurred_at, transcript_id)`; excluded cwds
get no rows; unreported cached tokens stay NULL and price as 0, with the output marked an upper bound.

Verification, 2026-10-14:

- `go test ./...` printed 26 `ok` lines, one `?   github.com/jcogilvie/unjira/internal/store/storetest
  [no test files]`, and exited 0. `earthly +reviewable` printed `Earthly Build  ✅ SUCCESS`.
- A scratch store, collected with only the claude_code collector (`backfill_days: 2`), ledgered
  10,925 rows over 7 models from 247 events. The first `unjira tokens` failed as criterion 5 asks:
  `Error: no price for model "claude-sonnet-5-5": add it under token_pricing`. With every model priced
  at **placeholder** rates (input 1, output 2, cache write 1, cache read 0.1, so the dollar figures
  mean nothing), it printed:

  ```
  ## Per ticket
    (none)

  ## Untracked work, by narrative id
    (none)

  ## Unattributed messages
  no_event: 324 message(s)
    claude-opus-5-5  unlogged: in 630, out 201535, cache write 1080940, cache read 14192878, $2.9039  logged: $0.0000  pending: $0.0000
    ...
  unattributed: 10601 message(s)
    ...

  ## unjira's own LLM spend
    30d    0 call(s)  prompt 0 (cached 0)  completion 0  $0.0000
    ...
  ```

  Per ticket and untracked are empty, and every message is unattributed, because that store was
  collected and never narrated or matched: no member links exist. Own spend is zero because no LLM
  call was made. The per-ticket path and the call recording are covered by the offline tests, not by
  this run.
- Row count against the transcripts: of the 1,440 transcripts the scratch pass read, those that
  produced any row held 11,106 distinct non-`<synthetic>` assistant ids with usage, up to each
  cursor's offset. 10,925 were ledgered, none outside that set. The 181 missing ids are resumed
  history: they sit in one root transcript (`c68b4c96…`) whose lines are copies from the original
  root transcript `d8159df7…`, which ended 2026-09-02, is older than the horizon and is never read.
  Verified: all 181 belong to resumed copies whose original holder is past the horizon (D-35).
- **Not verified: the own-call count.** No LLM gateway credential was available, so no
  `dev narrate --dry-run` was run, and no pass's reported call count was compared with
  `SELECT count(*) FROM llm_calls`. Call recording is covered only by the offline tests.

## Problem

unjira knows which narrative each piece of Claude Code work belongs to: a `member` link, which
`README.md` and `docs/superpowers/specs/2026-10-02-shared-context-design.md` call "the unit of token
attribution". It knows which ticket each narrative belongs to: the `primary` row in
`narrative_issues`. It does not know what the work cost. No collector reads the per-message `usage` in
a transcript, nothing adds a cost up per ticket, and nothing reports one. So the effort spent on a
ticket that already exists is invisible, both to the person doing it and to Jira. Phase 2's estimation
ensemble ("observed effort") also needs this number.

This ranks above Phase 2's spin-off tickets. It applies to tickets that already exist, so it does not
depend on the create path or on where new issues may be opened.

## Facts this contract rests on (measured 2026-10-14)

- **Usage lives on assistant lines.** A transcript line with `type:"assistant"` carries
  `message.usage` (`input_tokens`, `output_tokens`, `cache_creation_input_tokens`,
  `cache_read_input_tokens`, plus sub-objects) and `message.model`.
- **Summing lines double-counts.** One `message.id` spans several lines, one per content block, and
  each line carries a full copy of its usage. The copies are not always identical: across every local
  transcript (1,413 files, 77,089 distinct owned ids), 17,475 ids (23%) differ between lines, mostly
  in `output_tokens`. In every one, the last line's four counts are at least every earlier line's.
  513 ids carry `message.model = "<synthetic>"` with all counts zero: Claude Code's own locally
  generated lines, not model calls. On the three largest local transcripts there
  were 7830, 3825 and 6185 usage lines for 4519, 2289 and 3004 distinct ids, so summing lines
  overcounts 1.7–2x. Do not key on `requestId`: the measurement found one distinct value per file.
- **Subagents are separate transcripts, already collected.** They live at
  `<project>/<session>/subagents/agent-<id>.jsonl`, with a `.meta.json` alongside (`toolUseId`,
  `parentAgentId`, `spawnDepth`), and each line carries its own usage. The parent's `toolUseResult`
  has a `totalTokens` figure that looks like only the subagent's final turn (one sample). Never add it
  to the per-line sums.
- **Snapshots overlap.** A segment event's ExternalID is `<transcript>:<fileSize>:<segIndex>`
  (`collector/claudecode/claudecode.go:323`), so every snapshot re-covers all of its segment's lines.
  Segment indices can also shift between snapshots (`segments.go:189-272`). A segment's raw lines are
  available as `factLines` (`segments.go:27-60`).
- **Writes today.** `TaskWriter` has `AddComment`, `SetStatus` and `CreateIssue`. There is no worklog
  method and no field edit. `actions.type` is `comment|transition|create`, and `estimate` is a type
  value with no applier (`gate/applier.go:193-207`). `suppressDuplicates` looks only at *proposed*
  actions, never at applied ones.
- **The Jira collector reads no worklogs,** so a worklog unjira writes never comes back as an event.
  It does read field changes, and unlisted fields fall through (`collector/jira/events.go:44-49`).
- **The store has no migrations.** It refuses an old store instead (`schemacheck.go`, `storeFormat`,
  `checkRequiredColumns`).
- **unjira's own LLM calls report usage,** prompt and completion tokens (`llm.Usage`), but nothing
  saves it.
- **Jira worklog API** (Cloud REST v2 and v3 specs):
  - `started` is required, plus exactly one of `timeSpent` or `timeSpentSeconds`.
  - `notifyUsers` defaults to `true` (emails watchers) and `adjustEstimate` defaults to `auto` (reduces
    the remaining estimate), so both must be passed explicitly.
  - PUT and DELETE by worklog id exist. An API user holding only the *own* permissions can change
    only its own worklogs.
  - Worklog entity properties hold up to 32 KB of JSON each. The UI does not show them, and they can be
    set inline on POST and read back with `expand=properties`.
  - An API-token user cannot log against an issue that is not editable, which is often true of closed
    issues.
- **Not verified, because no credentials were available:**
  - the minimum `timeSpentSeconds` (0? 60?);
  - whether time tracking is enabled on DEVSBX;
  - whether the unjira user holds *Work on issues* and *Edit/Delete own worklogs*.

  PR 2's live test settles all three.

## Design

### Ledger (PR 1)

- **One row per assistant `message.id`,** holding: transcript id, subagent agent id (if any),
  timestamp, model, all four token counts, and the segment event that covers it. Written by an
  idempotent upsert that keeps each counter's maximum, so repeated lines, re-snapshots and a snapshot
  taken mid-stream converge on the final usage without double-counting. MAX does not depend on order.
- **First holder owns the row.** An id recorded by two transcripts (239 ids, always two subagent
  transcripts) belongs to the transcript with the lowest `(occurred_at, transcript_id)`. The upsert
  takes ownership only when the incoming pair sorts lower, so read order cannot change the owner. A
  later holder may still raise counters through MAX. Ownership can move once, when an earlier
  transcript is collected late, and never back.
- **What is not ledgered.** `<synthetic>` messages, through a named constant in the collector, since
  an unpriced model is an error (D-10) and they are not model calls. Messages in a segment under an
  excluded cwd: the user excluded that work, so it gets no row at all. A resumed transcript's copied
  history, when its original (earliest) holder is older than the collection horizon: ownership gives
  those lines to the original, which is never read, and events drop the same lines. The tokens were
  spent on the original's date, outside the window. Widening `backfill_days` on a fresh store
  recovers them (D-35).
- **Covering event.** Within one snapshot the segments partition the owned lines, so each message
  sits in exactly one segment, and the row stores that segment's event id (one shared
  `segmentExternalID`, so the ledger and the event cannot drift). A re-snapshot overwrites it, which
  is how "newest" works. A segment that emits no event for a reason other than an excluded cwd (49
  ids, segments with no user text) writes none, and an existing covering event is never overwritten
  with none. Such a message is reported as `no_event`.
- **Collector stays dumb.** The claude_code collector extracts the rows deterministically from the
  lines it already reads. No judgment, no network. It writes them through the store it already
  holds for cursors, per transcript and before `SetCursor`, so a failed write leaves the cursor in
  place and the next pass re-reads the file.
- **Attribution chain.** message → the newest collected event whose lines cover it → that event's
  `member` narrative → that narrative's `primary` issue.
  - A subagent's messages follow the subagent event's own member link (D-4).
  - A narrative with no confident primary, meaning none at or above `match.confidence_floor`, is
    **untracked**, labeled with its candidate primary key if it has one. This is the create path's
    rule.
  - A message whose covering event has no member link is **unattributed**. One with no covering
    event is **`no_event`**.
  - `store.MessageAttributions` returns each row's model and four counters with its attribution, so
    pricing needs no second query. Its plan is a scan of `token_messages` plus a lookup on the events
    unique index. The `covering_external_id` index serves PR 2's per-event lookups.
- **Attribution is computed, never copied.** It is derived from current links at read time, so
  `Tx.MoveMember`, a primary relink and a triage merge or split are reflected automatically.
- **Cost.** USD from the config price table `token_pricing.<model>`: per-million rates for input,
  output, cache write and cache read. A model missing from the table is an error naming the model,
  never $0.
- **unjira's own spend.** One row per unjira LLM call: time, pipeline stage, model, prompt tokens,
  completion tokens, and cached tokens if the server reports them, NULL otherwise. These rows are never
  attributed to a ticket or narrative (D-29).
  - One `llm.RecordingClient` decorator writes them, wrapped once at `cmd/unjira`'s `llmClient()`. A
    small adapter in `cmd/unjira` maps `llm.CallRecord` to `store.LLMCall`, so neither `llm` nor
    `store` imports the other.
  - The stage is a context value set at each `pipeline.Run*` entry (`cluster`, `match`, `draft`,
    `distil`) and in `triage.NewSession` (`triage`). A recluster done inside triage is recorded as
    `triage`: the label records who spent it.
  - Cost is (prompt − cached) × input + cached × cache read + completion × output. NULL cached is
    priced as 0, so every prompt token is charged at the input rate. That overestimates, and
    `unjira tokens` marks any window holding such a row as an upper bound.
  - `--dry-run` on `dev narrate` and `watch` persists no narratives and writes no actions, but it
    still stores collected events and one `llm_calls` row per call. Its help text says so.
- **The store refuses older stores** through the existing schema check. It does not migrate them.

### `unjira tokens` (PR 1, read-only)

- **Per ticket:** logged, unlogged and pending tokens and cost, per model. "Logged" and "pending" read
  as zero until PR 2.
- **The untracked bucket,** per narrative, each labeled with its candidate primary key if it has one
  (D-33).
- **Unattributed messages,** in two groups: `unattributed` (the covering event has no member link)
  and `no_event` (no covering event).
- **unjira's own spend** for the windows `30d`, `7d`, `1d` (rolling 24h) and `today` (local calendar
  day), computed on the fly from the call rows (D-29). A window holding a call with no reported
  cached count is marked an upper bound.

### Attribution sink interface (PR 2)

- **The ledger feeds Jira through a sink interface,** so an output can be swapped, enabled per
  tracker, or composed with others: several sinks reading the same ledger (D-31).
- **A sink's input** is a ticket's attribution state: its messages, which ones are already logged
  where, and its per-model totals.
- **A sink's output** is drafted actions. It never writes Jira directly: drafts go through the queue
  and the gate like every other action.
- **The worklog sink is the only implementation in this work.** Which sinks a tracker uses is config,
  and the worklog sink is the default for a writable Jira tracker.

### Worklog sink (PR 2)

- **What gets proposed.** Every pass, for each primary ticket inside `writable_scopes`
  (D-9), the sink drafts a `worklog` action covering that ticket's attributed messages that are not in
  any applied worklog yet (D-14, D-20).
  - No message timestamped before `worklog.work_since` is ever included. Absent means no backfill: the
    floor is when the feature was enabled (D-19).
  - **`worklog.work_since` is not `--since`.** `--since` is the per-pass narration window on `dev
    narrate` and `watch` (`cmd/unjira/main.go:672`, `:856`). Neither defaults from the other, and the
    help text and docs say so.
- **The marker.** Applying a worklog records its Jira worklog id on each ledger message it covers
  (D-15). "Unlogged" means no such id, so a late-collected subagent transcript is still caught.
- **One pending worklog per ticket.** A newer draft supersedes the pending one (D-16).
- **Time spent.** Active wall-clock time of the covered parent-session messages, with gaps longer than
  `worklog.idle_gap` (default 15m) excluded (D-6, D-23). Subagent messages add tokens, not time.
  `started` is the earliest covered message.
- **Visible comment.** A deterministic template: totals in, totals out, cost, and a per-model split
  (D-27). No model call, and no transcript text.
- **Worklog properties** (one key, JSON): per-model counts of all four kinds, per-model cost, the rates
  used, an idempotency key, and the unjira version (D-24, D-25).
- **Write flags.** Always `notifyUsers=false&adjustEstimate=leave`.
- **Corrections.** A message logged on ticket A but now attributed elsewhere produces a proposal to
  shrink A's worklog, or to delete it if nothing remains. The message joins its new ticket's next delta
  (D-8, D-17). Corrections are ordinary gated actions.
- **Closed or non-editable tickets.** The action lands `failed` with a persisted reason, and its tokens
  stay unlogged in the ledger (D-12). Nothing is redirected.
- **Retries cannot double-log.** Before re-sending a POST whose outcome is unknown, the client looks
  for the idempotency key among the issue's worklogs.
- **Gate.** `auto_commit.worklog` follows the same deny-by-default gate as every other type. Nothing in
  unjira can graduate it.

### Approval units (PR 3)

- **One unit per workstream.** A worklog and a comment or transition proposed for the same workstream
  (narrative) in the same pass are presented in triage as one unit (D-18).
- **One decision, members droppable.** The reviewer makes one decision on the unit, and may drop a
  member first. Each member records its own ruling, so learning and calibration are unchanged (D-21).
- **Each member keeps its own gate.** A graduated member auto-applies on its own, and only the members
  that reach a human form the unit (D-22).

## Non-goals

- A custom-field sink. It is a considered alternative (below), not built.
- Breakdown by agent name, type or instance. Local agent names are not comparable across users (D-25).
- Cache-miss % or other work-pattern metrics in per-ticket output.
- Attributing unjira's own LLM spend to tickets.
- Redirecting worklogs from closed tickets to a parent or epic.
- Any GitHub write.
- Collecting tokens from tools other than Claude Code.
- A model-written worklog comment.

## Considered alternative: custom fields

Number fields per issue (`LLM tokens in/out`, `LLM cost`), plus an issue property for the per-model
split, set to the ledger total each pass.

**For:**
- It is state, not events, so it fits the reconciler: compare the ledger with the live value and
  propose the difference.
- No marker, supersede or shrink logic is needed, and retries are naturally idempotent.
- JQL can search the fields and dashboards can sum them.

**Against:**
- One number per ticket is last-writer-wins when two people's unjiras track the same ticket. Worklogs
  are per-author and additive, which is what lets multi-user use (Phase 3) work without coordination.
- It needs a Jira admin to create the fields and add them to screens, plus site-specific
  `customfield_*` ids in config.
- Suppressing notifications on an issue edit is believed (not verified) to need admin or project-admin
  rights.
- The Jira collector reads field changes, so every write comes back and needs a self-authorship check.
- Jira's time tracking gets nothing.

**Door:** two-way for the data. The ledger is the source of truth and identical under either sink, and
unjira can edit or delete its own worklogs and overwrite fields. Switching costs PR 2's sink code plus a
cleanup pass. The one-way parts are social: people and reports start relying on worklog time, and sent
notifications cannot be recalled. The sink interface keeps the switch, or running both, cheap.

**Future work (not scoped):**
- A field sink that sets the field directly when unjira's user is the ticket's only worklog author
  (D-32), which avoids the overwrite problem.
- Queryability. JQL reaches worklogs through `worklogAuthor`, `worklogDate`, `worklogComment` and the
  issue's `timespent`, but not through worklog properties. Indexing properties for JQL takes a
  Connect/Forge app.

## Acceptance criteria

### PR 1: ledger, attribution, `unjira tokens`

1. **No double counting.** Ingesting the same transcript twice, then again after it grows, yields one
   ledger row per `message.id`, and its counts equal that id's usage. A test fixture repeats one id
   across several lines with identical usage, and a second fixture's usage grows across lines and
   lands at the per-counter maximum.
2. **Subagent tokens follow their own link.** Subagent messages are attributed through the subagent
   event's member link, not the parent's (test with a parent and a subagent in different narratives).
3. **Totals track membership.** After `Tx.MoveMember`, or after a primary relink, a ticket's total
   changes with no re-ingest.
4. **Untracked and unattributed are both reported.** A narrative with no confident primary appears
   in the untracked bucket. A message whose covering event has no member link appears as
   unattributed, and one with no covering event as `no_event`.
5. **Unknown models fail loudly.** A model absent from `token_pricing` makes `unjira tokens` error,
   naming the model.
6. **unjira's own spend is recorded and windowed.** Every unjira LLM call writes one row, and `unjira
   tokens` reports 30d/7d/1d/today from those rows. Test with an injected clock.
7. **Old stores are refused.** A store created before this change is refused with the existing
   message pattern.
8. **Docs ship with the code:**
   - the README CLI surface and config keys;
   - `config/unjira.example.yaml` with `token_pricing`, commented;
   - `docs/architecture.md`;
   - the `--dry-run` help for `dev narrate` and `watch`, which still store events and `llm_calls`.

### PR 2: worklog sink

9. **Only unlogged tokens are proposed.** A worklog action is drafted only for primary tickets in
   writable scope, and covers only messages with no worklog id and timestamps at or after
   `worklog.work_since`.
10. **Applying sets the marker.** Applying a worklog records its id on every covered message. The next
    pass proposes only messages added afterwards.
11. **At most one pending worklog per ticket.** A larger delta supersedes the pending draft.
12. **Corrections shrink or delete.** Moving a logged message to another ticket produces a shrink, or
    a delete, on the old worklog and adds the message to the new ticket's delta.
13. **Time spent excludes idle gaps.** `timeSpentSeconds` is the active time with gaps over `idle_gap`
    excluded, and subagent messages add none. Test with fixed timestamps.
14. **Every write sends the flags.** `notifyUsers=false&adjustEstimate=leave` is on every POST, PUT
    and DELETE. Test against a recording transport.
15. **A retry cannot double-log.** A retried POST whose idempotency key already exists on the issue
    does not create a second worklog.
16. **Closed tickets fail with a reason.** A non-editable issue lands the action `failed` with a
    persisted reason, and its tokens stay unlogged.
17. **Live test on DEVSBX** (`-tags=live`) posts, updates and deletes one worklog, and records:
    - the minimum accepted duration;
    - whether time tracking is enabled;
    - the permissions the unjira user needs.

    Quote what it printed in this spec's Status section.
18. **The gate is unchanged.** `auto_commit.worklog` defaults to deny. A fresh clone applies no
    worklog.
19. **Sinks are config.** The sink set is per-tracker config, and the worklog sink sits behind the
    interface. A test registers a second fake sink and both receive the same ledger state.
20. **`work_since` and `--since` stay distinct.** The help text and docs describe both and say how
    they differ.

### PR 3: approval units

21. **Pending actions condense into a unit.** A worklog and a comment or transition for the same
    narrative in one pass show in triage as one unit.
22. **Members can be dropped.** Approving a unit after dropping one member applies the rest, and the
    dropped member records its own ruling.
23. **Graduated members skip the unit.** A graduated member auto-applies, and the unit holds only the
    members left for a human.

## Decisions

| ID | Decision | Reason | From | Status |
|---|---|---|---|---|
| D-1 | ~~Metrics recorded per ticket: Record raw input and output tokens plus a USD cost, with an optional per-agent breakdown (parent session vs each subagent/agent type). Cache-miss % is out of scope for per-ticket attribution: it describes work patterns, not the work.~~ | User: raw in/out and dollars are what describe the work's cost; cache efficiency is a property of how we work, so it belongs elsewhere if anywhere. |  | superseded by D-24 |
| D-2 | First output surface: Per-ticket token totals are written to Jira as worklogs (not a local-report-first slice). | User picked Jira worklog in round 1 over the recommended local report. |  | active |
| D-3 | Dedup key: A per-message ledger keyed on message.id (idempotent upsert), each message charged through the newest event that covers it. | User accepted the recommendation. It is immune to repeated lines per message (1.7-2x overcount measured), re-snapshots and segment boundary shifts. |  | active (holder rule refined by D-34) |
| D-4 | Subagent attribution: A subagent transcript's tokens follow that subagent event's own member link, not the parent session's narrative. | User accepted the recommendation. Consistent with member links being the unit of attribution. |  | active |
| D-5 | ~~Ticket mapping: A narrative's tokens are charged to its primary issue only; a narrative with no primary goes to an untracked bucket.~~ | User accepted the recommendation. Sums stay exact because there is at most one primary per narrative. |  | superseded by D-33 |
| D-6 | timeSpent: Jira requires a time value on every worklog. What should it hold?: Active wall-clock time (Recommended) | user picked Active wall-clock time (Recommended): The parent session's time spans across its member segments, with idle gaps over a threshold left out. Subagents add tokens but no time because they run in parallel. Anyone reading Jira's time field gets a real number. | Q-1 | active |
| D-7 | ~~Granularity: How many worklogs per ticket?: One per ticket per day (Recommended)~~ | user picked One per ticket per day (Recommended): `started` is that day. Updated in place while the day's total grows, found by an idempotency key in its properties. Matches how people log time. | Q-2 | superseded by D-14 |
| D-8 | Corrections: When attribution changes after a worklog is posted (a member moves, a primary is relinked), what happens?: Correct it (Recommended) | user picked Correct it (Recommended): Propose an update, or a delete plus a new worklog, so Jira matches the ledger. Allowed because unjira can edit its own worklogs. Each correction goes through the gate like any write. | Q-3 | active |
| D-9 | Ticket scope: Which tickets get worklogs?: Any primary in writable scope (Recommended) | user picked Any primary in writable scope (Recommended): Effort is effort. `writable_scopes` and the gate already decide where unjira may write, and logging work on a colleague's ticket is normal in Jira. | Q-4 | active |
| D-10 | Pricing: Where do per-model prices for the dollar cost come from?: Config price table (Recommended) | user picked Config price table (Recommended): `token_pricing.<model>` per-million rates for input, output, cache write and cache read. Deterministic, no network, auditable. The rates used are stamped into the worklog properties. An unknown model fails loudly rather than costing $0. | Q-6 | active |
| D-11 | ~~Backfill: Should worklogs cover work from before the feature is turned on?: From a configured `since` date (Recommended)~~ | user picked From a configured `since` date (Recommended): No backfill by default. Setting `since` lets you choose how far back to go, limited to days whose events are already clustered and matched. | Q-7 | superseded by D-19 |
| D-12 | Closed tix: What happens when the primary ticket can't take a worklog (closed or not editable)?: Fail with a reason (Recommended) | user picked Fail with a reason (Recommended): The action lands `failed` with the persisted reason, visible in triage and `actions list`, and the tokens stay in the ledger. Nothing is redirected silently. | Q-8 | active |
| D-13 | ~~Worklog proposal timing and watermark: Each pass proposes a worklog for every ticket with unlogged attributed tokens. Applying a worklog is the marker: the next worklog on that ticket covers only tokens attributed after it (a delta, not a running total). A config option suppresses a worklog proposal for a ticket when no other action (comment/transition) is proposed for it in the same pass.~~ | User (free text on Q-5): always propose, optionally only alongside other changes; an applied proposal is a marker and added tokens become a new worklog from there. | Q-5 | superseded by D-20 |
| D-14 | Worklog granularity: One worklog per applied proposal, covering the ledger messages on that ticket not yet in any applied worklog. Not one per ticket per day updated in place. | Follows from the Q-5 watermark decision: an applied worklog is a marker and later tokens become a new worklog. | Q-2 | active (replaces D-7) |
| D-15 | Marker: What exactly is the marker an applied worklog leaves?: Per-message link to the worklog (Recommended) | user picked Per-message link to the worklog (Recommended): Each ledger message records the worklog id it was logged in, so unlogged means not yet in any worklog. Messages that arrive late, such as a subagent transcript that's collected afterwards, are still caught. | Q-9 | active |
| D-16 | Pending: More tokens arrive while a worklog proposal is still pending review. What happens?: Replace the pending proposal (Recommended) | user picked Replace the pending proposal (Recommended): At most one pending worklog per ticket. The new draft covers the larger delta and supersedes the old one, so triage never has two. | Q-10 | active |
| D-17 | Correction: A message already logged on ticket A is now attributed to ticket B. How is it corrected?: Update A's worklog and add it to B's delta (Recommended) | user picked Update A's worklog and add it to B's delta (Recommended): Propose an update shrinking A's worklog, and B's next worklog includes the message. Totals stay exact and both writes go through the gate. | Q-11 | active |
| D-18 | Worklog proposal default and approval unit: Worklogs are always proposed; there is no 'only alongside other changes' option. When a worklog and another action (comment/transition) are proposed for the same workstream (narrative), triage presents them as one condensed approval unit. | User (free text on Q-12): always propose, but condense a worklog plus another change for the same workstream into one approval unit. | Q-12 | active |
| D-19 | Backfill floor: work_since: Worklog backfill is bounded by a new config key `worklog.work_since` (an absolute date): no worklog covers a message before it, and absent means no backfill (start from when the feature is enabled). It is distinct from the existing `--since` span flag on `dev narrate`/`watch` (cmd/unjira/main.go:672, :856), which sets how far back each pass narrates. Neither derives from or defaults to the other, and docs/help text must name the difference. | User: don't conflate the new work-since flag with the existing since flag. They answer different questions (worklog eligibility floor vs per-pass narration window). | Q-7 | active (replaces D-11) |
| D-20 | Worklog proposal timing and watermark: Each pass proposes a worklog for every ticket with unlogged attributed tokens. Applying a worklog is the marker: the next worklog on that ticket covers only tokens attributed after it (a delta, not a running total). There is no option to suppress worklogs; see the approval-unit decision for how they sit beside other actions. | Q-12 dropped the piggyback/suppress option in favour of always proposing and condensing into one approval unit. | Q-5 | active (replaces D-13) |
| D-21 | Unit review: How does a condensed approval unit (a worklog plus a comment or transition for one workstream) behave in triage?: One decision, members can be dropped (Recommended) | user picked One decision, members can be dropped (Recommended): Approve or reject the whole unit at once, but the reviewer can first drop one member, such as rejecting the comment while keeping the worklog. Each member still records its own ruling, so learning and calibration are unaffected. | Q-13 | active |
| D-22 | Auto-commit: If one member's type is graduated for auto-commit and the other isn't, what happens?: Each member gated by its own type (Recommended) | user picked Each member gated by its own type (Recommended): A graduated worklog auto-applies, and only the members that reach a human form the unit. Keeps the per-type gate exactly as it is today. | Q-14 | active |
| D-23 | Idle gap: For active wall-clock time, how long a gap between messages counts as idle and is excluded?: Configurable, default 15 min (Recommended) | user picked Configurable, default 15 min (Recommended): `worklog.idle_gap`. Long enough to cover reading or a slow tool run, short enough to drop lunch. | Q-16 | active |
| D-24 | Metrics recorded per ticket: Record input and output tokens plus a USD cost, broken down per model only. There is no breakdown by agent name, type or instance. The ledger keeps all four token kinds (input, output, cache write, cache read) because the cost needs them. Cache-miss % is out of scope. | User (Q-15): local agent names aren't comparable across users, so count per model only. |  | active (replaces D-1) |
| D-25 | Breakdown granularity: Worklog properties and comments break tokens and cost down per model only, never by agent name, type or instance. | User: local agent names aren't comparable across users. | Q-15 | active |
| D-26 | Local view: Should there also be a read-only local view of the ledger?: Yes, `unjira tokens` (Recommended) | user picked Yes, `unjira tokens` (Recommended): Per ticket: logged, unlogged and pending, plus the untracked bucket (narratives with no confident primary, D-33) and unattributed messages. Lets you check the numbers before arming any write, and untracked tokens show up nowhere else. | Q-17 | active |
| D-27 | Comment: What text goes in the worklog's visible comment?: Deterministic template (Recommended) | user picked Deterministic template (Recommended): E.g. 'unjira: 1.2M in / 45k out, $3.41 (claude-opus-5-5 $3.10, claude-sonnet-5-5 $0.31)'. No model call, nothing from transcripts reaches Jira, and the text can't hallucinate. | Q-19 | active |
| D-28 | Slicing: How should this ship?: Three stacked PRs (Recommended) | user picked Three stacked PRs (Recommended): (1) Ledger, attribution and the read-only view. (2) Worklog action type, Jira client, gate, corrections and a live test on DEVSBX. (3) Condensed approval units in triage. Each layer ships something usable. | Q-20 | active |
| D-29 | unjira's own LLM spend: unjira's own LLM spend (clustering, matching, drafting, distil) is counted but never attributed to tickets or narratives. It is tracked separately, codeburn-style, and `unjira tokens` reports it computed on the fly for 30d, 7d, 1d and today windows. | User (Q-18): count it but don't attribute it; it is useful as `unjira tokens` output. | Q-18 | active |
| D-30 | Docs location: The contract, plan and PR descriptions for this work live in the repo's docs/superpowers/ folder (spec under docs/superpowers/specs/, plan under docs/superpowers/plans/), not in the ~/.claude documents folder. | User: use the preexisting in-repo docs folder that superpowers uses. |  | active |
| D-31 | Attribution sink interface: Build the Jira output behind a sink interface so it can be swapped, flagged per tracker, or composed (several sinks fed from the same ledger). The first and only implementation in this work is the worklog sink. A custom-field sink is a considered alternative, not built. | User: definitely worklogs to start, but build with an interface that allows swapping, flagging or composing. |  | active |
| D-32 | Future: field sink when sole worklog author: Recorded as future work, not in scope: a field sink that sets a custom field directly when unjira's user is the only worklog author on the ticket, which avoids the multi-user overwrite problem. | User's idea for future work. |  | active |
| D-33 | Untracked means no confident primary: A narrative whose primary is below `match.confidence_floor`, or that has no primary at all, goes to the untracked bucket, labeled with its candidate primary key if it has one. Its tokens are never charged to that ticket. | User ruling, 2026-10-14: the same rule as the create path, consistent with the repo's meaning since F58/F64. |  | active (replaces D-5) |
| D-34 | First holder owns a doubly-recorded message: When one message.id is recorded by two transcripts, the row's transcript and covering event belong to the lowest (occurred_at, transcript_id) across them. A later holder may raise counters through MAX but never takes ownership unless its pair sorts lower. | User ruling, 2026-10-14 (F33's rule), made order-independent so read order A,B and B,A agree. |  | active |
| D-35 | Resumed history past the horizon is not ledgered: A resumed transcript's copied lines belong to their original (earliest) holder. When that original is older than the collection horizon it is never read, so those messages get no ledger row, as they get no event. | Planning ruling, 2026-10-14: the tokens were spent on the original's date, outside the window. The 181 ids missing from a scratch collect were exactly such copies. Cost if wrong: resumed spend older than `backfill_days` is not counted; widening backfill on a fresh store recovers it. |  | active |

## Open assumptions

- Worklogs are authored by whoever owns the Jira token on that connection. unjira writes as that
  person, not as a service account.
- `today` is the local calendar day and `1d` is a rolling 24h. Other windows are rolling.
- The message → covering-event mapping uses the newest snapshot event that contains the message's line.
  Unchecked: whether one `message.id` can straddle a segment boundary. If it can, PR 1 charges it to the
  segment holding its first line and says so. Measured 2026-10-14: 0 of 77,089 ids straddle, in 0 of
  1,413 files. The first-line rule is implemented and pinned by a test.
- The model name in a transcript (e.g. `claude-opus-5-5`) is the `token_pricing` key as written. No
  alias table.
- unjira's own calls are priced from the same `token_pricing` table, keyed by its configured model
  name.
- Time spent counts parent-session messages only. A run made purely by a subagent, with no
  parent-session member messages on the ticket, logs Jira's minimum duration (settled by criterion 17)
  rather than zero.
