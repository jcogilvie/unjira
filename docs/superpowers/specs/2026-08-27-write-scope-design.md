# Write scope — design

Which projects unjira may **write** to, declared independently of which it **reads** from.

Status: design, approved by the user (deny-all default, real config edited in lockstep).

## Why now

PR #22 proved the apply path works against live Jira. That turned a latent config problem into a
live one: **there is no notion of which projects unjira is allowed to write to.**

The real `unjira.config.json`:

```json
"jira": [{ "name": "dev", "project_keys": ["PAAS", "DEVSBX"], ... }],
"tracker": { "backend": "jira", "default_project": "DEVSBX" }
```

`project_keys` is a *read* scope being implicitly reused as the write surface. **24 of the 26 queued
actions target PAAS**, a live production project; 2 target DEVSBX. Graduating `comment` today makes
`watch` post to PAAS on its next pass.

Two mechanisms, both unguarded, verified against the code:

- `comment`/`transition` route by `action.IssueKey`, with **no project check anywhere** —
  `applier.go:150/172` call `a.writer.AddComment`/`SetStatus` directly.
- `create` routes by `tracker.default_project`, which is `DEVSBX` today **by luck**. An operator
  changing it to `PAAS` for a legitimate reason would start opening production tickets with no
  check at all. Same-shaped hole, different field.

And `cmd/unjira/main.go:112` `projectKey()` falls back to `config.Jira[0].ProjectKeys[0]` — **PAAS**.

## Shape: `writable_project_keys` on `JiraConnection`

A per-connection list, required to be a subset of that connection's `project_keys`.

Rejected alternatives, with reasons:

- **`tracker.writable_projects` (flat allowlist).** `TrackerConfig` has no notion of *connection*,
  so a flat list would duplicate the project→site/credential mapping `project_keys` already
  encodes, and could name a project no connection covers — leaving `JiraConnectionForProject`
  unable to resolve a site at all. The "writable ⊆ readable" invariant has to be checked anyway;
  putting the field next to `project_keys` makes that a same-struct check instead of a cross-struct
  one.
- **Per-project keys inside `auto_commit` (`"comment@DEVSBX"`).** Conflates two independent axes.
  `AutoCommitRule` answers "has a human graduated this *action type*"; write scope answers "is this
  *project* ever writable, autocommit or not." Decisive: `unjira actions decide --approve` is a
  human-triggered write that **never consults `Decide`** (verified — `actions.go` calls
  `gate.NewApplier(...).Apply` directly), so scope living inside `AutoCommitRule` would need a
  second, duplicate check on that path. Also a string-key typo would silently create an unmatched
  key — queue-forever rather than loud failure.
- **Two connections, read-only and writable.** Multi-connection exists for genuinely different
  sites/credentials ("after a migration or an acquisition merges two orgs' Jiras"). Using it for
  "same site, same credential, narrower write scope" duplicates `Name`/`Site`/credential for one
  real connection, and `JiraConnectionForProject` returns the *first* iteration match — inviting
  exactly the "which connection covers this key" ambiguity the existing error messages work to
  avoid.

## Default: absent means nothing is writable

Mirrors `AutoCommitRule.Graduated`: absence of an explicit human grant means no authority. The two
compose as independent zero-value-safe checks — `Graduated` per action type, writability per
project — both of which must hold.

Rejected: **inherit `project_keys` when unset.** That defaults to "already armed for everything you
read," which is the bug being fixed under a new name. It preserves current behaviour and fixes
nothing unless an operator acts.

**Consequence, and it must land in the same PR:** the real `unjira.config.json` needs
`"writable_project_keys": ["DEVSBX"]` added, or the just-proven DEVSBX path starts refusing for a
reason nobody will recognize. `config/unjira.example.json` stays deliberately unpopulated for this
key, for the same reason it has no `auto_commit` block: an example that grants write authority
risks being copy-pasted into a real deployment.

## Choke point: `gate.Applier`, plus startup validation

Two complementary layers, not a choice between them.

**`Applier.Apply` is the runtime choke point.** It is the *only* code in the repo holding a
`tasktracker.TaskWriter` and calling the three write methods — verified by grep across
`internal/` and `cmd/`, excluding the client/interface definitions themselves. Both routes to a
write construct an `Applier` and call `Apply`: `RunAutoCommit` (automatic) and `approveAction`
(human). A check anywhere else is a hole.

Explicitly **not** `gate.Decide`. Its whole value is being a pure `(action, rules)` lookup with no
I/O, and — decisively — **it is not on the `--approve` path at all.**

Deriving the project from an issue key needs a small helper; nothing in the repo parses one today
(confirmed: `correlator/refs.go`'s regex is for `owner/repo#N` PR refs). It belongs in
`internal/tasktracker` as a backend-shape fact rather than gate business logic, and must error
loudly on a key that doesn't match `<PROJECT>-<NUMBER>` — per CLAUDE.md, don't guess.

**Startup validation is the second layer**, catching statically-checkable misconfiguration before
the first pass: `writable_project_keys ⊆ project_keys`, and — since `tracker.default_project` is
static config — that `create`'s target is writable, via `DefaultProjectConnection()`. That extends
an existing loud error rather than inventing a channel. Validation can't cover comment/transition,
whose project is a per-action runtime value; `Applier`'s check can't sensibly re-validate config
shape on every action. Different jobs.

## Not compiler-enforced, and that is deliberate

The `TaskReader`/`TaskWriter` split works as a *type* because it partitions capability that never
varies at runtime: the reconciler structurally never writes. Project writability is the opposite —
a **runtime, data-dependent** fact. The same `Applier`, in one process, must answer "PAAS: no,
DEVSBX: yes" per call, differing only by `action.IssueKey`'s value at invocation.

Expressing "a writer constructible only for an allowed project" would mean either constructing a
fresh typed writer per issue key (buying nothing over a runtime check, since the type just wraps
the same check) or monomorphizing per project at compile time (nonsensical for a config-driven
allowlist). `Applier.defaultProject` is already a plain runtime field for the same reason; follow
that precedent.

## A refusal is `status=failed` with a persisted reason, never a silent skip

Reuses the failure-reason machinery from PR #21: the check returns an error from `write`, so the
existing `status=failed` + `UpdateActionStatusAndError` flow carries it, and it appears in both
`actions list --status failed` and `RenderAutoCommitResult` with no new plumbing.

```
action 42: project "PAAS" is not writable
  (jira[].writable_project_keys does not include it for connection "dev")
```

**Explicitly NOT `DecisionQueue`.** That would read as "still pending triage" — materially different
from, and wrong about, "unjira tried and was blocked by scope," and invisible until someone
wondered why a graduated high-confidence action never applied. Silent refusal being
indistinguishable from "nothing to do" is the exact failure mode this project keeps fighting.

## Do not touch `JiraConnectionForProject`

It resolves *which site and credential to talk to*. `writable_project_keys` governs *authorization
to write*. A project can be writable while credential routing still goes through `project_keys`.
Conflating them would merge two genuinely different questions the current code correctly keeps
apart.

## Testing, with the drill for each

1. **Refuses a write outside the writable set.** `IssueKey: "PAAS-1"`, writable set `{DEVSBX}` ⇒
   the recording fake sees **no calls**, `status=failed`, error names `PAAS` and
   `writable_project_keys`. *Drill:* remove the check ⇒ fake records `AddComment:PAAS-1`.
2. **Empty set refuses everything**, including a project listed in `project_keys`. *Drill:* switch
   the default to inherit ⇒ the write succeeds, catching a reversion to the rejected option.
3. **Both paths covered — the hole test.** Same action, once via `RunAutoCommit` and once via
   `Applier.Apply` directly (standing in for `--approve`); both must refuse identically. *Drill:*
   put the check in `RunAutoCommit` instead of `Applier` ⇒ the direct-`Apply` test fails, proving
   the single-choke-point property rather than asserting it.
4. **`create` honours the writable set**, not merely `project_keys` coverage. *Drill:* check only
   `defaultProject != ""` (today's guard) ⇒ regresses to passing.
5. **`writable ⊄ readable` is a loud config error** at startup. *Drill:* remove the subset check ⇒
   an unreachable-but-writable project loads silently.
6. **`create`'s target validated at startup**, not per-action. *Drill:* skip it ⇒ fail-eventually
   instead of fail-fast.
7. **Live-path regression guard** (`internal/live`, in-process config, never reading real
   `unjira.config.json`): a graduated comment against a DEVSBX throwaway issue still applies.
   *Drill:* invert the check ⇒ this fails. The cheapest and most important drill, because a
   sign-flipped safety check is worse than none — it would look like it protects PAAS while
   actually blocking DEVSBX and letting PAAS through.
