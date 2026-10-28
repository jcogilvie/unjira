# Approval units (PR 3) Implementation Plan

> **Note:** references to CONTROLLER.md and the orchestrator documents folder are to the authoring environment, not this repo. Decision IDs D-33 and above are the authoring environment's numbering, not the spec's: D-35 here is the spec's D-36; D-33, D-34 and D-38 are process decisions with no spec entry.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. One implementer per task, one Opus review at the end (D-33, D-34).

**Goal:** In triage, a worklog and the comment or transition proposed for the same narrative are
reviewed as ONE unit: one decision, any member droppable first, each member keeping its own ruling
and its own gate.

**Architecture:** The batch stays the flat `[]store.ActionRow` that `NewSession` already takes, and
every member keeps its flat 1-based batch position (R4). A pure function, `triage.JoinWorklogs`,
joins each proposed worklog to a narrative through its payload's `message_ids` and the read-time
attribution `store.MessageAttributions` already computes (D-35). `Session.SetUnits` hands the joins
to the Session, the same way `SetWritability` hands it config. `Run` presents a unit once, at its
first undecided member, as one `Item` whose new `Members` field lists every member. It expands the
reviewer's one decision into one decision per member. The new verb `drop` (`d <n> [why]`) records a
per-member reject immediately and re-presents what is left. `Rulings()`, `Approved()`,
`recordRulings`, `applyApproved`, `RecordRuling` and `CorrectionsSince` are unchanged, because a
unit's members are still ordinary per-action decisions. The gate is untouched. A graduated member is
applied by `RunAutoCommit` in `watch`, never reaches `status=proposed`, and so never reaches a unit
(R6, verified below).

**Tech Stack:** Go 1.26, testify + go-cmp, `modernc.org/sqlite` (real temp-file store in cmd tests).

**Spec:** `docs/superpowers/specs/2026-10-14-token-attribution-design.md`. In scope: "Approval units
(PR 3)" (`:314`) and acceptance criteria 21-23 (`:425`). Decisions cited by ID (D-18, D-21, D-22,
D-35). Where this plan and the spec differ, the Rulings section says why.

**Public repo.** `jcogilvie/unjira` is public. No real Jira key goes in code, tests, comments, commits
or PR text. Tests use `PROJ-1`, `PROJ-2`. Branch `jco-approval-units` (already created).

**Worktree:** `/Users/jonathan.ogilvie/workspace/unjira/.claude/worktrees/approval-units` (`$W` below).

---

## Verification already done (planning, 2026-10-28)

Ruling 4 asked whether AC 23 already holds. Its auto-apply half does. A scratch test (deleted after
the run) drove `runWatchPass` with `worklogPass(t)` (`cmd/unjira/watch_pass_test.go:497`) plus a
drafted comment on `PROJ-1`, once per graduation setting:

```
grad=worklog:  1 comment nar=1 proposed / 2 worklog nar=0 applied  / proposed=1
grad=comment:  1 comment nar=1 applied  / 2 worklog nar=0 proposed / proposed=1
grad=(none):   1 comment nar=1 proposed / 2 worklog nar=0 proposed / proposed=2
--- PASS: TestScratchAC23 (0.05s)
```

So the triage batch (`ActionsByStatus(StatusProposed)`, `cmd/unjira/triage.go:334`) only ever holds
members left for a human. The other half ("the unit holds only those") depends on grouping working
from that batch, and Task 7 tests it. Task 1 makes the scratch check permanent.

---

## File Structure

| File | Change |
|---|---|
| `cmd/unjira/watch_pass_test.go` | Task 1: AC 23 regression tests |
| `internal/triage/units.go` (new), `units_test.go` (new) | `JoinWorklogs` |
| `internal/triage/triage.go` | `Item.Members`, `SetUnits`, unit walk, `VerbDrop`, dissolve on restructure |
| `internal/triage/restructure.go` | `VerbDrop` in `Restructure`'s explicit not-a-restructure case (`:610`) |
| `internal/triage/units_session_test.go` (new) | Session unit tests (package `triage_test`, reuses `scriptedPrompter`, `stubHandler`) |
| `cmd/unjira/triage.go`, `triage_test.go` | `d` in `parseDecision`; unit rendering in `Ask`; `newTriageSession` |
| `cmd/unjira/triage_units_test.go` (new) | end-to-end AC 21-23 against a real store |
| `README.md`, `docs/architecture.md`, the spec | Task 8 |

Commands, for every task: `go -C $W test ./<pkg>/ -run '<Test>' -v`, then `go -C $W test ./...`, then
`golangci-lint run ./...` from `$W`. A task is done only when all three pass.

---

## Task 1: AC 23 holds today: lock it in

**Tier:** mechanical

**Files:** `cmd/unjira/watch_pass_test.go` (after `TestRunWatchPass_GraduatedWorklogAutoApplies`, `:546`).

- [ ] Write `TestRunWatchPass_GraduatedMemberAppliesAndOnlyTheOtherQueues`, a table over two rows,
  each starting from `worklogPass(t)` and a `watchLLM` whose one response drafts
  `{"issue_key":"PROJ-1","type":"comment","body":"the work landed","confidence":0.9,"rationale":"r"}`:
  - `auto_commit.worklog` graduated (floor 0.5): the worklog is `applied` and the comment is `proposed`.
  - `auto_commit.comment` graduated (floor 0.5): the comment is `applied` and the worklog is `proposed`.

  In both rows, assert that `s.ActionsByStatus(store.StatusProposed)` holds exactly one row, the
  ungraduated one. Read the actions with `s.ActionsForIssue("PROJ-1")`.
- [ ] This test is expected to **pass on first run**. It guards behaviour that exists (see
  "Verification already done"). If it fails, STOP and report: R6 is wrong and the plan needs a gate
  task.

**ACs:** 23 (auto-apply half).

**Done when:** `go -C $W test ./cmd/unjira/ -run TestRunWatchPass_GraduatedMemberAppliesAndOnlyTheOtherQueues -v` passes, plus `go test ./...` and lint.

## Task 2: `triage.JoinWorklogs`, the pure join

**Tier:** standard

**Files:** `internal/triage/units.go` (new), `internal/triage/units_test.go` (new, package `triage_test`).

Signature:

```go
// JoinWorklogs joins each proposed worklog in batch to the one narrative whose
// comment or transition it is reviewed with (D-18, D-35), and reports every
// worklog it could not join unambiguously. joins maps worklog action id to
// narrative id; a worklog absent from it is reviewed alone.
func JoinWorklogs(batch []store.ActionRow, msgs []store.MessageAttribution) (joins map[int64]int64, reports []string)
```

Rules (R1, R2):
1. Only rows with `Type == store.ActionTypeWorklog` are joined. Decode `store.WorklogPayload` from
   `Payload`.
2. The worklog's narratives: the distinct `m.NarrativeID` over `msgs` where `m.MessageID` is in the
   payload's `MessageIDs`, `m.State == store.AttributionTracked` and `m.IssueKey == row.IssueKey`.
   Ids missing from the ledger are ignored. A delete carries no ids and so joins nothing.
3. A narrative is a *candidate* when the batch holds at least one row with that `NarrativeID` and a
   `Type` of `comment` or `transition`. Compare against the reconciler's type strings
   (`string(reconciler.ActionComment)` / `string(reconciler.ActionTransition)`). Triage already
   imports `reconciler` (`restructure.go:11`).
4. With zero candidates there is no join and no report. This is the ordinary case: a worklog with no
   other pending work.
5. With exactly one candidate holding exactly one comment or transition, join the worklog to it.
6. With one candidate holding more than one, there is no join. Report
   `worklog action %d on %s: narrative %d has %d pending comment/transition actions: reviewed alone`.
7. With more than one candidate, there is no join. Report
   `worklog action %d on %s covers work from narratives %s, each with a pending action: reviewed alone`,
   with the ids sorted ascending and joined by `, `.
8. When the payload does not decode, there is no join. Report
   `worklog action %d on %s: payload does not decode (%v): reviewed alone`.

Reports come out in batch order. `joins` is never nil.

- [ ] **Test first.** Write `TestJoinWorklogs`, a table test with `cmp.Diff` on both returns. Rows:
  - joins one worklog to the narrative of one pending comment;
  - joins to a transition;
  - message tracked to `PROJ-2` while the worklog is on `PROJ-1`: no join, no report;
  - message `untracked` or `unattributed`: no join;
  - narrative has no pending comment or transition (an applied comment is absent from the batch):
    no join, no report;
  - two narratives each with a pending comment: no join, report rule 7 (`narratives 3, 7`);
  - two narratives, only one with a pending comment: joins that one;
  - one narrative with two pending comments: no join, report rule 6;
  - a delete worklog (no `message_ids`): no join;
  - undecodable payload: report rule 8;
  - two worklogs (an add and an update on the same ticket) both covering one narrative: both join it.
- [ ] Implement in `units.go`. The doc comment names D-35 and states why ambiguity is reported, not
  forced: a worklog is drafted per ticket, so it may span several narratives (R2).

**ACs:** groundwork for 21. Ruling 3 of the brief.

**Done when:** `go -C $W test ./internal/triage/ -run TestJoinWorklogs -v` passes, plus `go test ./...` and lint.

## Task 3: Session presents a unit and expands one decision

**Tier:** complex. The walk change is the load-bearing part of this PR.

**Files:** `internal/triage/triage.go` (`Item` `:85`, `Session` `:133`, `Run` `:201`),
`internal/triage/units_session_test.go` (new).

- [ ] **Test first.** Write these in `units_session_test.go`. Batches are built inline: comment
  `{ID: 1, NarrativeID: 7, Type: "comment", IssueKey: "PROJ-1"}`, then an unrelated comment
  `{ID: 2, NarrativeID: 8}`, then a worklog `{ID: 3, NarrativeID: 0, Type: worklog, IssueKey: "PROJ-1"}`.
  The tests call `SetUnits(map[int64]int64{3: 7})`.
  - `TestSession_PresentsAUnitOnce`: the first `Ask` is a unit. `Members` holds the actions with ids
    `[1, 3]` in batch order, and their `Position`s are `[1, 3]`. `Item.Action.ID == 1` (the
    comment), `Item.Position == 1`. The second `Ask` is action 2 alone, with `Members` empty. There
    are exactly 2 `Ask` calls for 3 actions.
  - `TestSession_UnitApproveApprovesEveryMember`: answers `approve`, then `skip`. `Approved()` holds
    ids `[1, 3]`.
  - `TestSession_UnitRejectRulesOnEveryMember`: answers `reject "wrong work"`, then `skip`.
    `Rulings()` is `[{1, rejected, "wrong work"}, {3, rejected, "wrong work"}]` (brief ruling 2).
  - `TestSession_UnitSkipSkipsEveryMember`: answers `skip`, then `approve`. `Approved()` holds `[2]`.
    Action 3 is never asked alone.
  - `TestSession_UnitQuitDiscardsEverything`: `quit` gives `ErrAbandoned`, an empty `Approved()` and
    an empty `Rulings()`.
  - `TestSession_WorklogFirstStillPresentsTheUnitAtItsFirstPosition`: worklog at position 1, its
    comment at 3. The unit is asked first. `Item.Position == 3` (the comment), and `Members`
    positions are `[1, 3]`.
  - `TestSession_NoJoinMeansNoUnit`: no `SetUnits` call. Three single `Ask`s, which is today's
    behaviour.
  - `TestSession_UnappliableMemberRefusesUnitApprove`: `SetWritability` with `PROJ` not writable, a
    unit on `PROJ-1`, and answers `approve`, then `skip`. One notice starts `cannot apply: ` and
    contains `position 1` and `d <n>`. The unit is re-asked, and nothing is approved.
  - `TestSession_WorkVerbOnAUnitActsOnItsCommentOrTransition`: unit with the worklog at position 1
    and the comment at 2, answers `edit "shorter"`, then `skip`, then `skip`. `stubHandler.redraftCalls`
    names action 2, not the worklog. No `WorklogRefusal` is notified. The replacement (same
    `NarrativeID` 7) is re-presented inside the unit with the worklog.
- [ ] Implement:
  - `Item.Members []Item`. Its doc comment: set only for an approval unit (D-18), and each member is
    decorated and given writability as a single Item would be. `Action`, `Position`, `Narrative` and
    `Issue` are then the unit's comment or transition, the member the work verbs act on (R7).
    `Appliable` is true only when every member is appliable. `UnappliableReason` is one line per
    unappliable member, `position %d (%s on %s): %s`, followed by the line
    `drop each member that cannot be applied (d <n>), or reject the unit`.
  - `Session.units map[int64]int64` (worklog action id → narrative id), set by
    `func (s *Session) SetUnits(joins map[int64]int64)`. Doc comment: a setter for SetWritability's
    reason, and nil means no units.
  - `unitAt(i int) []int`, which returns the batch indices of i's unit in batch order, or nil. Take
    the narrative n of `s.batch[i]`: `s.units[id]` for a worklog, `NarrativeID` for a comment or
    transition. Then gather every **undecided** index j that is either a worklog with
    `s.units[id] == n` or a comment/transition with `NarrativeID == n`. Return them only when exactly
    one is a comment/transition and at least one is a worklog. Everything is computed at walk time,
    so a drop, a splice or a dissolve reshapes the unit with no extra bookkeeping.
  - In `Run`, first `if _, decided := s.decisions[a.ID]; decided { continue }`. Today no index is
    ever decided when the walk reaches it, so this changes nothing outside units. Then build the
    single Item as today, or the unit Item when `unitAt(i)` is non-nil. Let `ai` be the index of
    `Item.Action`: `i` for a single, the comment's index for a unit. Pass `ai` (not `i`) to
    `worklogTouched`, `handle` and `spliceBatch`. After a splice, keep `i--` so the walk re-presents
    from `i`, which is at most `ai`.
  - For a unit, approve, reject and skip write `d` to `s.decisions` for every member. Quit is
    unchanged.

**ACs:** 21 (Session half), 22 (approve and reject halves). Brief rulings 1 and 2.

**Done when:** `go -C $W test ./internal/triage/ -v` passes (every existing triage test unchanged),
plus `go test ./...` and lint.

## Task 4: the `drop` verb

**Tier:** standard

**Files:** `internal/triage/triage.go` (verbs `:21`, `Run`, `handle` `:503`),
`internal/triage/restructure.go` (`:610`), `internal/triage/units_session_test.go`.

- [ ] **Test first.** Use the Task 3 batch and `SetUnits({3: 7})`:
  - `TestSession_DropThenApproveAppliesTheRest` (AC 22): answers `{drop, Positions: [1], Text: "wrong ticket"}`,
    `approve`, `skip`. The second `Ask` is action 3 **alone** (`Members` empty), because the unit
    dissolves to one member. `Approved()` is `[3]`. `Rulings()` is `[{1, rejected, "wrong ticket"}]`.
  - `TestSession_DropInAThreeMemberUnitRepresentsTheRest`: comment plus two joined worklogs. Drop one
    worklog, and the re-presented unit has 2 `Members`.
  - `TestSession_DropOutsideAUnitIsRefused`: a single item, then `drop [1]`. The notice equals
    `triage.DropRefusal` and the item is re-asked.
  - `TestSession_DropNamesAMember`: `drop [2]` while the unit is positions 1 and 3. The notice is
    `drop names one member of this unit: positions 1, 3`, and the unit is re-asked. Also `drop []`
    and `drop [1, 3]` give the same notice.
  - `TestSession_DropThenQuitRecordsNothing`: `Rulings()` is empty.
- [ ] Implement:
  - `VerbDrop Verb = "drop"`. Update the "eight dispositions" comment to nine, with letters
    `a r e m s k t d q`. `Decision.Positions` now carries merge/split/drop positions, and `Text`
    carries the drop's reason as well.
  - `const DropRefusal = "drop removes one member of an approval unit; this action is reviewed alone: reject it instead"`.
  - In `Run`, `case VerbDrop`. On a single item, `Notify(DropRefusal)` and re-ask. On a unit, check
    for exactly one position naming a member. Then
    `s.decisions[s.batch[p-1].ID] = Decision{Verb: VerbReject, Text: d.Text}` and re-ask with `i--`
    (R5). A drop IS a reject of that member, so `Rulings()` and `Summary()` need no change.
  - Add `VerbDrop` to the explicit "does not require a handler" case in `handle` and to
    `Restructure`'s not-a-restructure case, so both switches still name every verb.

**ACs:** 22.

**Done when:** `go -C $W test ./internal/triage/ -run 'Drop' -v` passes, plus `go test ./...` and lint.

## Task 5: a restructure dissolves the unit it touches

**Tier:** standard

**Files:** `internal/triage/triage.go` (`Run`'s work-verb branch), `internal/triage/units_session_test.go`.

- [ ] **Test first.** Write `TestSession_RestructureDissolvesTheUnit`, a table over target, merge
  (`Positions: [1, 2]`, both comments) and split (`Positions: [1]`). Use a unit of comment 1
  (narrative 7) and worklog 3, plus comment 2 (narrative 8), and `stubHandler.replacement` /
  `restructured` with a comment still on narrative 7. After the verb succeeds, action 3 is asked
  **alone**, and the replacement comment is asked alone too. Edit (Task 3's test) does not dissolve.
- [ ] Implement: before `handle` for target, merge or split, collect the narrative ids of
  `s.batch[ai]` and of every `d.Positions` row. On success, delete every `s.units` entry whose value
  is one of them. Why: these verbs change which narrative holds which work, so the message → narrative
  attribution the join was computed from no longer holds. Edit rewords text and keeps the unit (R7).
  A failed handler call dissolves nothing.

**ACs:** none directly. It keeps a unit from presenting a worklog beside work it no longer covers.

**Done when:** `go -C $W test ./internal/triage/ -run 'Dissolves' -v` passes, plus `go test ./...` and lint.

## Task 6: terminal: `d` and the unit rendering

**Tier:** mechanical

**Files:** `cmd/unjira/triage.go` (`parseDecision` `:48`, `Ask` `:134`), `cmd/unjira/triage_test.go`.

- [ ] **Test first.**
  - Add these `TestParseDecision` rows:
    - `d 3` gives `VerbDrop`, positions `[3]`, empty text;
    - `d 3 not this ticket` gives text `not this ticket`;
    - `d` errors with `drop requires a position`;
    - `d x` errors with `is not a number`.

    Change the unknown-verb rows to expect `a r e m s k t d q`.
  - `TestTerminalPrompter_AUnitShowsEveryMember`: an `Item` with two `Members` (a comment body `the
    work landed`, and a worklog add with comment `unjira: 1 in / 1 out`) at positions 1 and 3, and
    input `q\n`. The output contains both bodies, `[1/3]` and `[3/3]`, `approval unit` and
    `[d]rop <n>`. The prompt line `s[k]ip` appears exactly once.
  - `TestTerminalPrompter_ASingleItemOffersNoDrop`: the output does not contain `[d]rop`.
- [ ] Implement:
  - The `d` case: `d <n> [why]`, using `parsePositions(rest[:1])`. The missing-position error is
    `drop requires a position: d <n> [why]`. Update the doc comment's letter list and both
    unknown-verb messages.
  - In `Ask`, move the per-action block (the `[%d/%d] type key confidence` line, body, `why`,
    unappliable note) into `printAction(item triage.Item)`. A single item renders exactly as today.
    For a unit:
    - print `\n== approval unit: %d actions for one piece of work (positions %s) ==`;
    - then the issue, work and confirm lines from `item`, printed once;
    - then `printAction` for each member;
    - then the prompt, with `[d]rop <n> ` inserted before `s[k]ip`.

**ACs:** 21 (display half).

**Done when:** `go -C $W test ./cmd/unjira/ -run 'TestParseDecision|TestTerminalPrompter' -v` passes, plus `go test ./...` and lint.

## Task 7: wire units into `unjira triage`, end to end

**Tier:** standard

**Files:** `cmd/unjira/triage.go` (`triageCmd.Run` `:359-367`), `cmd/unjira/triage_units_test.go` (new).

- [ ] Implement `func (a *appContext) newTriageSession(batch []store.ActionRow, p triage.Prompter, h triage.Handler) *triage.Session`.
  It holds what `Run` does today at `:359-362`, plus:
  - `msgs, err := a.store.MessageAttributions(a.config.Match.ConfidenceFloor)`. On error, print
    `  note: approval units are unavailable this session: reading the token ledger: %v` and return
    the ungrouped session. Units are presentation, best-effort for decorate's reason (R9).
  - `joins, reports := triage.JoinWorklogs(batch, msgs)`, then `session.SetUnits(joins)`. Print each
    report as `  note: %s`. When `joins` is non-empty, print
    `%d worklog(s) are reviewed together with the comment or transition for the same work.`
  - `Run` calls it in place of `:359-362`.
- [ ] **Test first**, in `triage_units_test.go`. Use a real store (`triageTestStore`),
  `seedReconcilableNarrative(t, s, "e1", "PROJ-1", ...)` on a local issue, and ledger rows m1 and m2
  covering `e1` (as in `worklogPass`). Insert a proposed comment (narrative `nid`) and a proposed
  worklog add (`NarrativeID` 0, `message_ids` `["m1","m2"]`). Config: writable `PROJ`,
  `Match.ConfidenceFloor` 0.5. Drive `newTriageSession(batch, &terminalPrompter{in: bufio.NewScanner(strings.NewReader(input))}, nil)`
  with `captureStdout`:
  - `TestTriage_AWorklogAndItsCommentAreOneUnit` (AC 21): input `k\n`. The output contains
    `approval unit`, and `s[k]ip` appears once.
  - `TestTriage_DropThenApproveAppliesTheRestAndRulesOnTheDropped` (AC 22): input
    `d 1 wrong ticket\na\n`. Then run `app.recordRulings(session.Rulings())`. Then
    `gate.NewApplier(s, &worklogLocalTracker{Tracker: local.New(s)}, "PROJ", cfg.Trackers).Apply` over
    `session.Approved()`. Afterwards:
    - the worklog is `applied`;
    - the comment is `rejected` with feedback `wrong ticket`;
    - `s.CorrectionsSince(store.CorrectionsCursor{})` returns exactly the comment, with that feedback
      (learning unchanged, D-21);
    - m1 and m2 carry the worklog id (`MessageAttributions`).
  - `TestTriage_AGraduatedMemberLeavesNoUnit` (AC 23, triage half): insert the comment as `applied`,
    not `proposed`. The batch from `ActionsByStatus(StatusProposed)` is the worklog alone. Input `k\n`.
    The output has no `approval unit` and no `[d]rop`.
  - `TestTriage_AWorklogOverTwoNarrativesIsReviewedAlone` (brief ruling 3): a second narrative on
    `PROJ-1` with its own pending comment, and m2 covering its event. The output contains
    `covers work from narratives` and no `approval unit`.

**ACs:** 21, 22, 23 (triage half), end to end through the real store.

**Done when:** `go -C $W test ./cmd/unjira/ -run 'TestTriage_' -v` passes, plus `go test ./...` and lint.

## Task 8: docs, spec status, full regression

**Tier:** mechanical

**Files:**
- `README.md`:
  - **Status** (`:40`): the bold lead and the paragraph gain PR 3. A worklog and the comment or
    transition for the same work are reviewed in triage as one approval unit, with one decision and
    members droppable with `d`. A graduated member applies on its own and joins no unit.
  - In the gates paragraph (`:95-98`), add that each member of a unit keeps its own gate.
  - In the triage paragraph (`:267`), add units and `d <n> [why]`, which rejects one member and
    records its own ruling.
- `docs/architecture.md`, in the present tense:
  - §1: the `TRIAGE` node (`:82`) reads `approve · reject · edit<br/>merge · split · target · drop<br/>approval units: worklog + its work's comment/transition`.
  - §3: the edge `proposed --> rejected` (`:424`) reads `triage [r]eject, [t]arget<br/>or [d]rop`.
    Add one bullet: units are a presentation over per-action rows, joined through `message_ids` and
    read-time attribution. Each member's status, gate and ruling are its own. A graduated member is
    applied before triage and joins none.
  - §2 and §4 are unchanged (no new write path, no new package edge: `triage` already imports
    `store` and `reconciler`). Say so in the PR description.
- The spec, `docs/superpowers/specs/2026-10-14-token-attribution-design.md`:
  - Both status lines move: the bold header (`:3`, `...; PR 3 design**`) becomes `PR 3 landed <date>`, and
    the Status section line (`:9`) becomes `PR 1 landed 2026-10-14. PR 2 landed 2026-10-23. PR 3 landed <date>.`
  - Add a "PR 3" block in Status:
    - the verification evidence: the `go test ./...` result line and the `earthly +reviewable` result
      line;
    - the deviations, one bullet each: R1 (a unit is one comment or transition plus its worklogs), R2
      (ambiguous joins are reviewed alone and reported) and R3 ("same pass" means pending together at
      review).
  - Leave the "Approval units (PR 3)" section and "Open assumptions" as written.
- `CLAUDE.md`: no change (no invariant moves). `docs/design-notes.md`: no change (no new failure
  mode).
- `docs/superpowers/plans/` (D-38): copy this plan in as
  `2026-10-28-approval-units.md`, and copy `plan-pr1.md` and `plan-pr2.md` from the orchestrator
  documents folder (strip any local path preamble into repo-relative paths) so the repo holds the
  full plan record for the epic.
- `docs/findings/`: open `F69-<slug>.md` per `docs/findings/README.md` only for an open question the
  reviewer leaves unsettled. Check `git log --all` and `ls docs/findings` for the next free number
  first.
- [ ] Run `earthly +reviewable` from `$W`. Quote its result line in the spec and the PR description.

**ACs:** documentation for 21-23 (brief ruling 5).

**Done when:** every doc above is in the diff, and `earthly +reviewable` passes.

---

## Dependency order and parallel safety

```
1 (AC23 lock-in, cmd/watch_pass_test) ───────────────────────────────┐
2 (JoinWorklogs, triage/units.go) ───────────────────────────────────┤
3 (Session units, triage.go) → 4 (drop) → 5 (dissolve) ──┬→ 6 (terminal) → 7 (wiring, e2e) → 8 (docs)
                                                         └───────────────↗
```

- **Wave A, in parallel:** 1, 2 and 3. Their files are disjoint: `cmd/unjira/watch_pass_test.go`;
  `triage/units.go` plus `units_test.go`; `triage/triage.go` plus `units_session_test.go`. Task 3
  tests use `SetUnits` directly, so they do not need Task 2.
- **Then serially:** 4 and 5 (both `triage.go`), 6 (needs `VerbDrop`), 7 (needs 2, 3, 4 and 6), then 8.

On a shared worktree, run writers one after another (CONTROLLER.md). The waves say what may run in
separate worktrees.

---

## Rulings

- **R1. A unit is exactly one comment or transition plus every worklog joined to its narrative.**
  Why: D-18 says "a worklog and a comment or transition". With one non-worklog member, edit and
  target have one unambiguous object (`Item.Action`), so the Prompter interface and the verbs' shapes
  do not change. A narrative holds two pending comments or transitions only through mirror
  destinations, and then its worklog is reviewed alone and reported (R2).
  Cost if wrong: a bigger unit needs a member position on edit and target. That would be
  `unitAt`'s "exactly one" check, plus a parse change.
- **R2. The join uses current attribution, the tracked state and the worklog's own issue key, and it
  is never forced.** No candidate means the worklog is alone, silently (the ordinary case). One
  candidate with one comment or transition joins. Several candidates, or one with several, leave it
  alone and print a report naming them. Messages missing from the ledger, untracked or
  re-attributed to another ticket are ignored.
  Why: a worklog is drafted per ticket (plan-pr2 R2), so it can span every narrative whose primary
  is that ticket. Re-attributed messages must not drag a worklog into the unit of work it no longer
  covers.
  Cost if wrong: the reviewer sees a worklog alone that could have been grouped. Nothing is
  mis-applied.
- **R3. "In the same pass" means pending together at review time.** Why: no pass id exists on an
  action. A pending worklog is superseded each pass its delta grows (D-16), so pending-together is the
  observable form of "proposed together".
  Cost if wrong: an older comment groups with a newer worklog. Both are for the same work.
- **R4. The batch stays flat and is not reordered.** A unit is presented at its first undecided
  member. Every member keeps and shows its own flat 1-based position. So `Decision.Positions`
  (merge, split, drop), `spliceBatch`, `Approved()`'s order and `worklogTouched` keep their meaning,
  with no re-mapping (brief ruling 1).
  Why: re-mapping positions is the only alternative, and it buys nothing a contiguous display
  cannot. The terminal prints each member with its position.
  Cost if wrong: the positions inside a unit may be non-contiguous (e.g. `1, 4`). That is a display
  matter only.
- **R5. A drop is an immediate reject of that member, with optional feedback. A unit reject is a
  reject per member with the same text. A unit skip is a skip per member.**
  Why: AC 22 and D-21 say each member records its own ruling. Recording a drop as `VerbReject` makes
  `Rulings()`, `recordRulings`, `RecordRuling` (`corrected_seq`) and `CorrectionsSince` see exactly
  what a plain reject produces (brief ruling 2). Nothing downstream changes.
  Cost if wrong: there is no un-drop within a session. Quit discards every decision, which is today's
  undo.
- **R6. No gate change. AC 23's auto-apply half already holds.** `runWatchPass` passes reconcile and
  sink drafts together to `RunAutoCommit` (`cmd/unjira/main.go:1157`), which applies graduated
  members before anything is `proposed`. Measured in "Verification already done". Triage's approve
  path still never consults `gate.Decide` (`cmd/unjira/triage.go:33`).
  Cost if wrong: Task 1 fails first, and the plan stops.
- **R7. Work verbs on a unit act on its comment or transition. Target, merge and split dissolve the
  unit of every narrative they touch. Edit keeps it.**
  Why: the worklog refuses work verbs (R17 of PR 2). Retarget, merge and split change attribution,
  so the join no longer holds. An edit changes only text.
  Cost if wrong: a dissolved unit presents its worklog alone, which is the pre-PR-3 behaviour.
- **R8. The Prompter interface is unchanged.** A unit is an `Item` with `Members`, and drop is a
  `Verb`. The five Prompter implementations (`terminalPrompter` and four test fakes) compile as they
  are.
- **R9. A ledger read failure means no units, with a note, and the session still opens.** Why:
  grouping is presentation, so decorate's best-effort rule applies. Refusing to open the queue would
  be worse.
- **R10. `--auto-approve` ignores units** (it approves every row). `--dry-run` shows them, since the
  ledger read is read-only.

---

## Open questions for the user

1. **D-30 says plans live in `docs/superpowers/plans/`.** No token-attribution plan is in the repo
   (`ls docs/superpowers/plans` shows none), and this plan is in the documents folder as briefed.
   Should Task 8 copy plan-pr3 (and pr1/pr2) in?
2. **A unit reject copies the reviewer's text onto every member.** The learning corpus then gets the
   same lesson as a worklog correction too. Alternative: put the feedback only on the comment or
   transition, and reject the worklog without text. The plan keeps the copy, because D-21 says each
   member records its own ruling.
3. **A narrative with two pending comments or transitions (mirror destinations) gets no unit** (R1).
   Acceptable, or should units grow to N members, with edit and target taking a position?
