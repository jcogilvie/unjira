# unjira's architecture, as built

What is actually here, in the present tense. Phase 1 has shipped; the distiller has not.

This file is a **living doc**, held to the same standard as `README.md` and `docs/design-notes.md`:
if a PR changes the pipeline's shape, a package boundary, the write-authority graph, or the action
lifecycle, it updates the relevant diagram *in that PR*. A diagram that has drifted is worse than no
diagram, because it is trusted.

**Describe, do not narrate.** This is not a changelog — it says what the code is, never what it used
to be or when it changed. `git log` holds the history and `docs/design-notes.md` holds the reasoning;
a "before X, we used to…" paragraph here rots the moment the next change lands and teaches a reader to
distrust the rest.

Companion docs, none of which this duplicates:

- `README.md` — what unjira does and the locked design decisions.
- `docs/architecture-findings.md` — where the current shape falls **short**. Kept separate because a
  description is true until the code changes, while a finding is open until somebody closes it; one
  file holding both is never in a settled state.
- `docs/design-notes.md` — *why* it is shaped this way, as numbered incidents.
- `docs/go-conventions.md` — how the Go is written.
- `CLAUDE.md` — the architecture invariants, stated as rules.

Every claim below cites `file:line` where a reader could reasonably want to check it.

---

## 1. The pipeline as data flow

Deterministic stages are the load-bearing ones — the model never sees an event unless a pure
function put it there. Green is deterministic, yellow is LLM-backed, red holds write authority.

**The store at the centre is the point, not clutter.** Every arrow passes through it: no stage hands
another an in-memory value, each reads what the last one committed. That is why a failed pass costs a
retry and nothing else.

These diagrams are drawn for **zoom**, not for a thumbnail — GitHub gives rendered mermaid its own
zoom and pan controls, so the constraint that matters is label *overlap*, never pixel count. Prefer
generous `nodeSpacing`/`rankSpacing` and full labels over abbreviation.

```mermaid
---
config:
  nodeSpacing: 100
  rankSpacing: 120
---
flowchart TB
    subgraph sources["Event sources"]
        CC["Claude Code transcripts<br/>(root + subagent JSONL on disk;<br/>sliced per branch run,<br/>plus one anchor per PR created)"]
        JIRA["Jira Cloud<br/>(issue bodies + changelog<br/>+ comments)"]
        GH["GitHub<br/>(PR opened + merged/closed,<br/>via issue-events timeline)"]
    end

    subgraph collectors["Collectors"]
        CCC["collector/claudecode"]
        JC["collector/jira"]
        GHC["collector/github"]
    end

    STORE[("internal/store<br/>SQLite")]

    SPLIT["PartitionByTrackerRecord<br/>deterministic · work evidence<br/>vs tracker state"]
    PRID["preassignByPullRequest<br/>deterministic · exact PR identity<br/>joins its one open holder"]

    subgraph correlate["internal/correlator"]
        CLUSTER["Cluster<br/>LLM · groups events into narratives:<br/>one member home per event,<br/>context links elsewhere"]
        DISPUTE["dispute re-ask<br/>LLM · one home for an event<br/>two clusters claimed"]
        PERSIST["Persist<br/>deterministic · new / extend / compact<br/>MoveMember · AddContext"]
        CAND["gatherCandidates<br/>deterministic · pre-filter<br/>ranks on IssueActivity"]
        MATCH["Match<br/>LLM · narrative to issue"]
    end

    subgraph reconcile["internal/reconciler"]
        VERIFY["verifyLinks<br/>live tracker read"]
        DRAFT["draft / Redraft / create<br/>LLM · proposes actions"]
        FILTERS["runSuppression · ordered chain<br/>1 unroutable · 2 tracker-echo<br/>3 stale-transition · 4 duplicate"]
    end

    TRIAGE["internal/triage<br/>approve · reject · edit<br/>merge · split · target"]

    subgraph write["internal/gate"]
        DECIDE["Decide<br/>pure, no I/O"]
        APPLY["Applier<br/>holds TaskWriter"]
    end

    CC --> CCC --> STORE
    JIRA --> JC --> STORE
    GH --> GHC --> STORE
    STORE --> SPLIT -->|"work evidence"| PRID -->|"no exact single home"| CLUSTER --> DISPUTE --> PERSIST --> STORE
    SPLIT -->|"tracker state · never clustered"| STORE
    PRID -->|"joined by identity · never shown to the model"| STORE
    STORE --> CAND --> MATCH --> STORE
    STORE --> VERIFY --> DRAFT --> FILTERS --> STORE
    STORE --> TRIAGE --> STORE
    STORE --> DECIDE --> APPLY -->|"AddComment · SetStatus · CreateIssue"| JIRA

    classDef llm fill:#f9e79f,stroke:#b7950b,color:#1a1a1a
    classDef det fill:#d5f5e3,stroke:#1e8449,color:#1a1a1a
    classDef danger fill:#fadbd8,stroke:#c0392b,color:#1a1a1a
    class CLUSTER,DISPUTE,MATCH,DRAFT llm
    class CCC,JC,GHC,PERSIST,CAND,FILTERS,DECIDE,SPLIT,PRID det
    class APPLY danger
```

**GitHub PR events never enter `SPLIT`'s tracker-state branch.** A pull request is a thing
somebody did, work evidence in every deployment `collector/github` supports today — never the
tracker's own account of itself, since this slice collects no issue-shaped artifacts (a
GitHub-Issues-as-tracker deployment is a materially different, unbuilt feature; see
`docs/architecture-findings.md` F28). So every arrow out of `GHC` in the diagram above only ever
reaches `CLUSTER`, never the `SPLIT -->|"tracker state"|` branch — there is no code path for it to
take.

**A pull request is seen from both sides, under one identifier.** `collector/claudecode` reads root
session transcripts and the subagent transcripts beneath them (`<session>/subagents/`), and emits two
event shapes: one per branch run, and one *anchor* per tool call that created a pull request
(`gh pr create` or the GitHub MCP), keyed on the immutable `tool_use` id. When the call's own result
names exactly one PR, the anchor carries `events.ArtifactPullRequest` — the same value
`collector/github` sets on every PR event. The value is the PR's host-qualified, case-folded identity,
`<host>/<owner>/<repo>#<N>`, written by one function (`events.PullRequestRef`) in both collectors and
read through `events.PullRequestOf`, which accepts only that shape. The host is part of it because
`acme/infra#12` on github.com and on a GHES instance are different pull requests. An anchor whose
result named no PR, several, or a failure carries none, and nothing else sets it: a segment that merely
mentions a PR never does. Two readers: the PR identity join (below), and the dispute re-ask, which shows
it to the model as evidence. Anchors feed no provenance tier, so `gatherCandidates` and matching read
exactly what they did before.

**Before clustering, an exact pull-request identity places an event without the model**
(`internal/pipeline/preassign.go`). After `PartitionByTrackerRecord`, each candidate carrying
`ArtifactPullRequest` is looked up (`store.PullRequestMemberHolders`, over all narratives, not just
the window's). If exactly one narrative holds a member event with the same value and that narrative
is `open`, the event joins it in one transaction: a member link via `Tx.MoveMember` with a fresh
`link_seq` (so the reconciler sees it as new work, even on a committed narrative), confidence 1.0,
`member_placement = 'identity'`, and `window_end` moved forward to cover it. The summary is not
rewritten, since the model never saw the event. The event then leaves the candidates, and
`hidePreAssigned` keeps it out of its narrative's hydrated context, so no prompt numbers it. Zero
holders, two or more holders, or a holder that is not `open` (an allowlist) all send the event to the
model unchanged, and the pass summary reports each with its reason. The join keys on exact PR identity
only, never on issue keys, which are many-to-many with PRs. It does not group in-window events among
themselves; a PR's first events go to the model together. It writes before clustering rather than
after `Persist`, so a pass whose model call fails still places what identity settles. A dry run
decides the same placements and writes none of them. Clustering context is read in both modes from
one query, `store.NarrativesOverlappingExtended`, answered as if each placed-into narrative's
`window_end` already covered its latest placed event. The real pass has written that extension, so
for it the query is plain `NarrativesOverlapping`. A dry run has not, and the query still returns a
holder that the join brings into the window. Without that, a dry run would cluster against fewer
narratives than the real pass.

**Clustering produces two kinds of link** (`narrative_events.kind`,
`docs/superpowers/specs/2026-10-02-shared-context-design.md`). A **member** link says the event is the
narrative's work; a **context** link says it is relevant background that is some other narrative's
work — one root-session investigation feeding several fixes is one narrative's member and the others'
context. Every linked event has exactly one member home: at most one by the partial unique index
`one_member_link_per_event`, at least one by a check when `Persist` commits. The model places each
numbered event in exactly one cluster's `event_indices` and may also list it in other clusters'
`context_indices`, with a confidence per cluster stored on each member link. An event the model puts in
two clusters' `event_indices` is not resolved by response order: `Cluster` makes one **dispute re-ask**
per pass, after any bisection has merged its halves, asking which claimant the event is primarily the
work of (rationale first, per-event confidence, PR and branch evidence presented but not applied). The
others keep it as context. The re-ask is one call, or, when the disputes together exceed the context
window, one call per batch that fits; every disputed event is asked about once, and no answer applies
until every batch has answered. One dispute too large alone fails the pass (F52). `Persist` writes members with `Tx.MoveMember` (moves the one home, refuses a
frozen one, upgrades a context row to a member with a new `link_seq`), then context with
`Tx.AddContext` (never deletes). Windows, summaries and compaction come from members only. Every
member link records who placed it in `member_placement`: `model` (`Persist`, including a triage
split), `identity` (the PR identity join), or `reviewer` (triage merge), because a 1.0
`member_confidence` alone cannot say which.

**A narrative left holding no member is marked `split`.** A context narrative's eligible members are
numbered in the clustering prompt, so a pass may place all of them in other clusters, and a triage merge
or split moves a source's eligible members by design. Every one of those paths ends in
`Tx.MarkSplitIfEmptied`, inside the transaction that moved the members: `Persist` checks each narrative
it took a member from (read with `Tx.MemberHolders` before anything moves), and merge checks its source.
A narrative with no member link holds no work, background or not, so it gets `store.StatusSplit`, which
`NarrativesOverlapping` excludes from every later prompt's context, and its context links are deleted,
which loses nothing because each such event keeps its member home elsewhere. The row, its actions and its
issue links are kept. A frozen member never moves, so a narrative with an applied action keeps the work
that action described. The pass summary lists each emptied narrative (`Stats.Emptied`), since it is in
no cluster of that pass.

**Nothing downstream of clustering reads a context link.** The reconciler's delta, the create and
redraft inputs, matching's candidates, and both examination watermarks read member links only, so a
context link re-admits no narrative and reaches no prompt that drafts or matches. Context is read by
the next clustering prompt (rendered under each context narrative as background, a numbered event as
`-> #N`), by the pass summary, and by nothing else.

**Ten LLM call sites**, in three packages — `correlator/correlator.go:505` (cluster), `:1116` (same-story
check at a bisection seam), `:1796` (compaction), `correlator/cluster_reask.go:119` (omission re-ask),
`correlator/cluster_dispute.go:177` (dispute re-ask), `correlator/match.go:627`,
`reconciler/draft.go:92`, `:339`, `reconciler/create.go:244`, and `rules/distill.go:126` (`learn`).
Nothing else in the tree calls a model.

Store-mediation is what makes a failed pass cost a retry and nothing else: a stage that dies has
committed either everything or nothing, and the next pass picks up from the persisted state rather
than from a lost in-memory value.

---

## 2. Write authority

The safety-critical diagram. `TaskReader` and `TaskWriter` are separate interfaces
(`internal/tasktracker/tasktracker.go:81`, `:128`) so that "this code cannot write" is a fact the
compiler checks rather than a claim a comment makes.

```mermaid
---
config:
  nodeSpacing: 70
  rankSpacing: 130
---
flowchart LR
    subgraph readers["Hold TaskReader"]
        R1["reconciler.Reconcile"]
        R2["reconciler.Rework"]
        R3["correlator.Match<br/>(via TrackerResolver)"]
        R4["triage.restructure"]
        R5["pipeline.RunReconcile"]
    end

    subgraph gates["Deny-by-default gates"]
        G1["auto_commit.graduated<br/>false by zero value<br/>gate/decide.go:66"]
        G2["confidence >= confidence_floor<br/>gate/decide.go:66"]
        G3["trackers[].writable_scopes<br/>empty denies everything<br/>gate/applier.go:401"]
    end

    HUMAN["Human approval<br/>via triage / actions decide"]
    A["gate.Applier<br/>internal/gate/applier.go:42<br/>the ONLY TaskWriter holder"]
    TRACKER[("Jira / local tracker")]

    R1 -->|"read only"| TRACKER
    R2 -->|"read only"| TRACKER
    R3 -->|"read only"| TRACKER
    R4 -->|"read only"| TRACKER
    R5 -->|"read only"| TRACKER
    G1 --> A
    G2 --> A
    G3 --> A
    HUMAN --> A
    A ==>|"the only write path"| TRACKER

    classDef danger fill:#fadbd8,stroke:#c0392b,color:#1a1a1a
    classDef gate fill:#fdebd0,stroke:#ca6f1e,color:#1a1a1a
    class A danger
    class G1,G2,G3 gate
```

**Verified: exactly one `TaskWriter` holder in the tree** — `gate.Applier`. `cmd/unjira/actions.go:186`
resolves one to hand it over, and nothing else names the type outside the interface definition and
doc comments. Every other consumer takes `TaskReader`.

`reconciler/create.go:107` is worth reading as a design statement: it takes no tracker at all, and
its comment says *"the parameter's absence is the guarantee."*

Each gate is independently load-bearing, per `docs/design-notes.md` incident 16 — a gate that
duplicates another is not a gate. Gate 3 answers a different question from gates 1–2: not "is this
action trusted" but "is this project one we may write to at all."

**Where gate 3's answer comes from.** Config separates a *connection* (`config.Connection`: a system
kind and endpoint, `internal/config/trackers.go`) from a *tracker* (`config.Tracker`: the scopes it
owns on one connection, and the `writable_scopes` subset unjira may write). `config.ProjectWritability`
finds the one tracker whose scopes cover a project and asks whether that scope is writable; both
`gate.Applier` and triage read it. `config.Load` runs `ValidateTrackers` before any command does work,
so the statically-checkable half of write scope fails at startup: a writable scope the tracker cannot
read, a writable scope on a kind with no writer (`github`), overlapping scopes across trackers, and a
`default_ticket_in` tracker that is not writable or names no `default_scope`.

---

## 3. Action lifecycle

```mermaid
---
config:
  nodeSpacing: 110
  rankSpacing: 140
---
stateDiagram-v2
    direction LR
    [*] --> proposed: reconciler drafts
    [*] --> declined: model judged<br/>not worth tracking<br/>(create only)
    [*] --> suppressed: deterministic filter<br/>refused the draft<br/>(watermark, not reviewed)

    proposed --> approved: triage [a]pprove<br/>or auto_commit
    proposed --> rejected: triage [r]eject<br/>or [t]arget
    proposed --> edited: triage [e]dit / [m]erge
    proposed --> proposed: triage [s]kip

    approved --> applied: Applier wrote it
    approved --> failed: tracker refused<br/>actions.error records<br/>how far it got
    approved --> failed: Applier refused —<br/>create whose narrative<br/>gained a primary link

    failed --> proposed: next reconcile pass
    edited --> proposed: replacement row
    rejected --> proposed: replacement row<br/>([t]arget only)

    applied --> [*]
    rejected --> [*]
    declined --> [*]
    suppressed --> [*]

    note right of declined
        Distinct from rejected:
        no human ruled.
        reconciler/create.go:67
    end note
```

Reading the transitions, since some distinctions matter more than an edge label can carry:

- **`declined`** is the model judging work not worth a ticket — creates only, and **distinct from
  `rejected`, which is a human's ruling** (`reconciler/create.go:67`). Conflating them would teach
  slice 7's distiller that unjira's own taste and a reviewer's are the same signal.
- **`reject / retarget`** are both recorded `rejected`, but only retarget produces a replacement row:
  the old action named the wrong issue, so it was ruled against rather than reworded
  (`triage/restructure.go:478`).
- **`edit / merge`** supersede with a replacement row and status `edited`.
- **`failed`** carries `actions.error` recording how far a multi-hop route got. **Retry is simply the
  next reconcile pass** — there is no separate retry path.
- **`skip`** leaves the action `proposed` for the next session.

**The eight `actions.status` values are named constants in one place.** `internal/store/actionstatus.go`
declares `StatusProposed`, `StatusApproved`, `StatusEdited`, `StatusRejected`, `StatusApplied`,
`StatusFailed`, `StatusDeclined`, `StatusSuppressed` — `store` rather than `reconciler`, `triage`, or `gate` (each of
which writes or compares at least one) because `store` is the only package none of the other three
import, so every writer reaches the constants through an import edge it already has. `gate` and
`triage` reference them as `store.StatusX`; `reconciler.StatusProposed` and
`reconciler.StatusDeclined` are aliases to the same constants, kept because this package already
spells both unqualified in several places, including its own tests. No `actions.status` comparison
or assignment anywhere in `internal/` or `cmd/` uses a bare string literal for one of these seven
values. `"open"`/`"split"` (`store.StatusOpen`/`store.StatusSplit`) are a separate, narrative-level
enum and are declared the same way, in `internal/store/narrativestatus.go`.

---

## 4. Package dependency graph

Acyclic (it compiles). Direction is downward; no `internal/` package imports `cmd/`.

```mermaid
---
config:
  nodeSpacing: 90
  rankSpacing: 130
---
flowchart TB
    CMD["cmd/unjira"]
    PIPE["pipeline"]
    TRIAGE["triage"]
    RECON["reconciler"]
    CORR["correlator"]
    GATE["gate"]
    STORE["store"]
    CONFIG["config"]
    CJIRA["clients/jira"]
    CGH["clients/github"]
    CLOCAL["clients/local"]
    COAI["clients/openai"]
    COLLJ["collector/jira"]
    COLLC["collector/claudecode"]
    COLLGH["collector/github"]

    subgraph contracts["Shared contracts"]
        EVENTS["events"]
        TT["tasktracker"]
        LLM["llm"]
        WF["workflow"]
        RULES["rules"]
        CRED["credentials"]
    end

    subgraph orphans["Uncalled — findings F1"]
        REFS["correlator/refs"]
        FANOUT["correlator/fanout"]
    end

    CMD --> PIPE & TRIAGE & GATE & CORR & STORE & CONFIG
    CMD --> CJIRA & CGH & CLOCAL & COAI & COLLJ & COLLC & COLLGH
    PIPE --> RECON & CORR & GATE & STORE & CONFIG
    TRIAGE --> RECON & CORR & STORE
    RECON --> CORR & STORE & CONFIG & WF & LLM & RULES & EVENTS & TT
    CORR --> STORE & CONFIG & LLM & RULES & EVENTS & TT
    CORR -.->|"F3: concrete backend"| CJIRA
    GATE --> STORE & CONFIG & TT
    STORE --> EVENTS
    CONFIG --> EVENTS
    CJIRA --> TT & WF
    CLOCAL --> STORE & TT & WF
    COAI --> LLM
    COLLJ --> CJIRA & CONFIG & EVENTS & PIPE
    COLLC --> EVENTS & PIPE
    COLLGH --> CGH & CONFIG & EVENTS & PIPE & CRED
    PIPE --> CRED

    classDef orphan fill:#eaeaea,stroke:#888,stroke-dasharray: 5 5,color:#1a1a1a
    class REFS,FANOUT orphan
```

---

## 5. Patterns and principles

Read against SOLID and the pattern vocabulary of Gamma et al. and Bass's *Software Architecture in
Practice*. Every row below was checked against the source, not inferred from a doc comment.

The **deliberate non-applications** below matter as much as the conformances: without a record, a
future reader sees a `switch` where a registry "should" be and changes it without knowing the
trade-off was weighed.

### Where the code already conforms

| Principle / pattern | Where | Evidence |
|---|---|---|
| **Interface Segregation** | every interface in `internal/` | **14 interfaces, all 1–3 methods.** `llm.Client` has one. No fat interface anywhere in the tree. |
| **Dependency Inversion, used for safety** | `tasktracker.TaskReader` / `TaskWriter` | The split exists so "this code cannot write" is a compile error. `reconciler/create.go:107` states it outright: *"this takes no TaskReader — the parameter's absence is the guarantee."* DIP for a security property, not for testability. |
| **Liskov substitution** | `clients/jira` vs `clients/local` | Both satisfy the full `TaskTracker` *and* `workflow.GraphProvider`, asserted at compile time (`_ workflow.GraphProvider = (*Tracker)(nil)`, `local/local.go:36`, `jira/tracker.go:27`). `local`'s graph is static, `jira`'s is mined — same postcondition, no weakening. |
| **Strategy + registry (OCP)** | `cmd/unjira/main.go:59` | Adding a collector is one map entry. Verified: **nothing downstream switches on collector name.** |
| **Adapter** | `internal/clients/*` | Thin facades, business logic one layer up. `clients/local` adapts two SQLite tables to a tracker interface. |
| **Bass: "limit access to critical resources"** | `gate.Applier` | Exactly one `TaskWriter` holder in the tree, behind three independent gates. The tactic implemented as a type constraint rather than a review convention. |
| **Command + audit log** | the `actions` table | Each action is a reified request carrying its own lifecycle and `actions.error`. Retry is re-execution on the next pass, not a separate path. |
| **Chain of Responsibility** | `reconciler.suppressionChain` | Four filters, uniform contract, order asserted as data — see below. |
| **Marker interface for optional capability** | `pipeline.StatusHistorySource`, `workflow.GraphProvider` | Type-asserted, not name-checked. `status_history.go:25-34` explains why the marker returns nothing: a `bool` would let the assertion and the value disagree. |

### Chain of Responsibility, and why the chain is data

`internal/reconciler/filters.go` defines `filterContext`, `suppressionFilter`, and
`suppressionChain`; `runSuppression` walks it, feeding each filter what survived the last. All four
filters share one context type and one return contract, so the chain is a slice rather than four
hand-wired calls.

**A suppression is recorded, not merely reported.** `Persist` writes one `StatusSuppressed` row per
narrative per pass that suppressed anything. That row is the watermark `DeltaEvents` reads, so the
narrative yields its slot to the next one instead of being re-derived every pass — without it, a
stable selection order plus a trace-less outcome is a livelock (finding F12, design note 29). The row
never reaches the review queue (`triage` reads `StatusProposed`) and never reaches `gate.Applier`
(`Persist` excludes it from its return value), and it is a watermark rather than a tombstone: new
events land past it, so the narrative returns when there is finally something to say.

**The order is asserted, not implied** — `TestSuppressionChain_OrderIsExplicitAndLoadBearing` reads it
directly, and a dropped filter fails three tests including the tracker-echo end-to-end case. That is
the property worth having: a reordering is a test failure with a reason attached, where a sequence of
statements would make it a diff that reads as harmless. `docs/design-notes.md` incident 25 records the
reasoning, including the ordering bug (incident 22) that a hand-wired chain permitted.

One asymmetry is deliberate and documented on the type: three filters are pure, `suppressDuplicates`
needs the store to ask whether another narrative already holds an open proposal. Uniformity costs it a
parameter the others ignore, which is cheaper than an uncomposable chain.

### Patterns considered and not currently applied

These are places a pattern would plausibly fit and where the case for adopting it is not yet strong
enough to act on. **None of them are settled against** — each is recorded with the condition that
would change the answer, so a later reader can check whether that condition now holds rather than
re-deriving the analysis from scratch. Where a note says "revisit when X," X arriving is a reason to
act, not a reason to argue.

**A registry for tracker backends.** Today it is a `switch` on the tracker's connection kind
(`appContext.taskTracker`, `cmd/unjira/main.go:137`), with the other kind-aware sites confined to `cmd`
and `config`. That is an asymmetry with the collector
registry, and a defensible one at two backends — one of which exists only for tests. Registries start
paying off around three variants. *Revisit when a third tracker lands*, or if backend-aware sites
begin appearing outside `cmd`/`config`.

**A Repository interface over `internal/store`.** The package holds two responsibilities — unjira's own
records (events, narratives, actions, cursors, `pipeline_lock`) and the local tasktracker backend's
mimicked issue store (`local_issues`/`local_issue_comments`, `localissues.go`) — sharing one `*Store`
handle and one `Open` because both need exactly one SQLite file, not because they share query logic.
That split is expressed at the file level (each concern has its own file; §4's dependency graph is
unaffected, since both stay inside `internal/store`), not via an interface: with one implementation and
no second datastore in prospect, an interface would add indirection without inversion. *Revisit if a
second store implementation becomes real* — an in-memory store for tests, or a non-SQLite backend.

**`correlator.Stats` as a Visitor.** It is a plain accumulator with `Add`/`AddUsage`
(`correlator.go:266`, `:297`), and the pattern name would not change the code. *Revisit if traversal
logic accumulates* — several stats types over one event walk, say, where double-dispatch would earn
its keep.

**A common "external system" interface over `Collector` and `TaskTracker`.** These sit at opposite ends
of the dependency graph: a collector is a *source* unjira reads without judgment, a tracker is a *sink*
only `gate.Applier` may write. Unifying them would put read and write authority behind one type, which
is the property §2 rests on — so this one carries a real cost, not just insufficient benefit. If a
future system is genuinely both (a tracker unjira both collects from and writes to — Jira already is,
via two separate seams), the shape worth reaching for is probably two interfaces on one adapter, not
one interface over both.

## Properties that hold

The load-bearing guarantees, each checkable against the tree. A change that breaks one of these is a
change to unjira's architecture, not a refactor — treat it accordingly.

- **Write authority is singular.** One `TaskWriter` holder, three independent gates, deny-by-default
  at every one. The `TaskReader`/`TaskWriter` split makes this a compile error rather than a
  convention.
- **Collectors are dumb.** No collector imports `tasktracker`. `collector/jira` imports
  `clients/jira`, which is it reading *its own source* — allowed, and not judgment.
- **No layering inversions.** Nothing in `internal/` imports `cmd/`. No import cycles.
- **The store is the only channel between stages**, which is what makes per-narrative failure
  isolation work.
- **Two deterministic pre-filters run before clustering.** `PartitionByTrackerRecord` keeps tracker
  state out, and the PR identity join places an event whose exact, host-qualified pull request one open
  narrative already holds. The join never reasons from issue keys, and anything it cannot settle
  exactly goes to the model unchanged.
- **Deterministic filters run before and after the model** in the reconciler, in an order asserted by
  a test rather than implied by statement sequence.
- **Every linked event has exactly one member home, and only member links reach a tracker path.** The
  database enforces at most one (`one_member_link_per_event`), `Persist`'s commit enforces at least
  one, and every reader feeding the reconciler, matching or a watermark filters on `kind = 'member'`.
  The readers whose meaning narrowed were renamed (`AllMemberEvents`, `MemberEventsAfterBoundary`,
  `EligibleMemberEvents`), so reading context from a drafting path means calling `ContextEvents`, which
  nothing there does.

Where the current shape falls short of these, or of good practice generally, is
`docs/architecture-findings.md`.
