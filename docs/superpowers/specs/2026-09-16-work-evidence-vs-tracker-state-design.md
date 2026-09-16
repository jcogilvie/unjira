# Separating work evidence from tracker state

**Status: design**

Closes F18. Reshapes F16. Adds two findings (F19, F20) discovered while measuring.

## The problem, stated architecturally

unjira diffs **reality** (what you did, from event streams) against **the tracker** (what the org
thinks you did). Two sides of a diff.

Today both sides enter the pipeline as the same type, through the same door, and are treated as
interchangeable. `internal/events.Event` is one struct, and a Jira changelog entry is fed to
clustering exactly as a Claude Code session is. The consequence is a category error with five stages
of downstream compensation:

```
collector    emits tracker records as events, indistinguishable in kind from work
correlator   clusters them into narratives — 26 narratives that restate a ticket
matcher      links narrative 34 to PAAS-4034, from which it was derived
reconciler   proposes a comment on PAAS-4034
tracker_echo suppresses it, correctly
```

Three separate filters exist at the exit — `suppressTrackerEcho`, `AnyWorkEvidence`,
`dropSelfAuthored` — for one classification error at the entrance. That is the smell.

### Measured consequences

Of 68 narratives in the dev store:

```
claude_code only   36
jira ONLY          29   ← every event is tracker bookkeeping
mixed               3   ← work joined to the ticket it concerns: unjira's actual job
```

Of the 29 jira-only narratives, **26 contain exactly one distinct issue key, and all 26 have a
primary link to that same ticket.** A narrative whose entire content is PAAS-4034's own changelog,
linked to PAAS-4034. All 26 are suppressed downstream by `AnyWorkEvidence`
(`internal/reconciler/tracker_echo.go:60`), so nothing wrong is *written* — but every pass pays to
cluster the bookkeeping, name it, persist it, hydrate it as context next pass, propose a comment, and
suppress it.

Cost, from the F16 probe:

| source | `tracker_record` | events | chars |
|---|---|---|---|
| `claude_code` | absent | 229 | 45,550 |
| `jira` | **true** | 96 | **210,350** |

**82% of the clustering prompt is tracker bookkeeping.** Excluding it takes a 60-day window from
**108 candidates to 36**.

## What the vocabulary already says

`events.ArtifactTrackerRecord` (`internal/events/tracker_record.go`) already names this exactly — *"a
record the tracker itself produced about a work item it already tracks"* — and its doc comment already
answers the operative question:

> "if unjira writes prose sourced only from events like this one, is it telling anybody anything they
> don't have?" The answer is no.

It also already rejects the two wrong ways to infer it (source name; presence of `ArtifactIssueKey`),
for reasons that remain correct. The concept is right and complete. It has **one consumer**, in the
reconciler. This design lifts it to where it belongs: the pipeline's entrance.

## Design

### 1. Tracker records never become clustering candidates

`RunNarrate` partitions its candidate set before building the prompt. Work evidence goes to
clustering; tracker records do not.

```go
// internal/pipeline/narrate.go
candidates, err := s.UnlinkedEventsInRange(window.Start, window.End)
work, trackerRecords := events.PartitionByTrackerRecord(candidates)
```

**A pure function in `internal/events`,** not a store query, for three reasons: the correlator
invariant says pre-filters are pure functions; the partition is needed by two callers (narrate and
the tracker-intent hint below); and a SQL `WHERE json_extract(...) IS NULL` would make the exclusion
invisible to anyone reading the correlator.

**Both halves are returned.** Discarding the tracker half inside the filter would be silent data loss
— and the count is needed for reporting (§5).

### 2. Tracker records are not linked, and that is deliberate

An excluded event has no `narrative_events` row, so `UnlinkedEventsInRange` returns it again next
pass. That is **correct and cheap**: it is filtered by a pure function before any model call, so it
costs a `map` lookup per pass, not tokens.

This is explicitly *not* incident 29's livelock. That failure was an *outcome that left no trace* at
**full model cost** — 20 narratives re-examined per pass by the LLM. Here nothing reaches the model.
The distinction is the cost of the repeat, and it must be stated in the code comment or a future
reader will "fix" this by adding a watermark it does not need.

### 3. Tracker state stays fully available to its real consumers

Verified: nothing that reads tracker state depends on narrative membership.

| consumer | reads | affected? |
|---|---|---|
| `store.LatestStatusEvent` (staleness guard) | `events` table directly | no |
| `store.IssueActivity` (corroboration ranking) | `events` table directly | no |
| `reconciler` delta | `narrative_events` | yes — see §4 |

So the staleness guard and matching's corroboration tier keep every tracker record they have today.
Excluding them from *clustering* costs those consumers nothing.

### 4. `ProvenanceTrackerIntent`: a transition into a working state is work evidence

The blanket claim "tracker records are never work evidence" is **false**, and this design would be
wrong without this section.

Consider: you move a ticket to In Progress, work locally without naming it, and open a PR. The
transition is not the tracker describing itself — it is the tracker recording *a human's declaration
of intent to work*, timestamped. That is the closest thing to a branch name that exists outside the
repo.

Measured, transitions in the dev store:

```
In Progress   47      ← declaration of intent to work
Done          34
Ready for Dev 32      ← grooming: a statement about a queue
Backlog       31
Blocked       14
In Review     12
```

Of 12 In-Progress transitions with exactly one same-day session, **12 of 12** had sessions naming the
ticket in neither prose nor branch. So the hint would be new information in every case.

**But the ambiguity is real, and measured in the direction that matters.** Asking "per transition, how
many same-day sessions?" gives a flattering 1:1. The question matching actually faces is the inverse:

```
per session, tickets moved to In Progress the same day:
  1 ticket  → 28 sessions
  2 tickets → 23
  3+        →  2
```

Only **28 of 53** are unambiguous, and the failure is concrete: one 39-message session on
`crossplane-render-version-pin` had PAAS-3946, PAAS-3944, PAAS-3939 and PAAS-3898 all move to In
Progress that day. At most one is right.

So the design:

- A new tier `ProvenanceTrackerIntent`, ranked **below `ProvenanceProseFirst`** (rank 4, shifting
  prose_later to 5). A co-timed transition is weaker than a human typing the key.
- It **creates** a candidate rather than only ranking one. `ProvenanceCorroborated` cannot serve here:
  it requires the key to already appear in prose, which is exactly the case this addresses.
- **All co-timed tickets are emitted**, not suppressed on ambiguity. Suppressing would discard the 28
  clean cases to avoid the 25 ambiguous ones, and verification exists to sort them out. The weak rank
  is what keeps an ambiguous set from outranking a real signal.
- **Which statuses count as "working" is config**, not a hardcoded list. `In Progress` is Jira-shaped;
  this workflow also has `In Test`, `In Review`, `Paused`. A new key
  `correlator.working_statuses: []string` (empty = feature off, so it ships inert).

### 5. Reporting

Per the never-silently-drop-data invariant, a pass reports what it excluded:

```
excluded 72 tracker records from clustering (36 candidates remain)
tracker-intent candidates: 3 tickets from 2 transitions
```

## Consequences accepted

**Narratives 17 and 25 lose their primary links, and should.** Both currently link at
`ProvenanceJiraEvent` with confidence **0.98 and 1.0** — provenance meaning "a Jira event in this
cluster is about this issue." That is not evidence the session did the work; it is evidence both
happened in the same window and the model grouped them. Co-clustering manufactures near-certainty
from temporal coincidence, and a 1.0-confidence link derived from co-occurrence is worse than no link:
nothing downstream can falsify it. After this change they become visibly unmatched, which is a true
statement about what unjira can currently prove. (F19.)

**Narrative 18 keeps its link**, earned via `ProvenanceProseFirst` — its session prose says *"let's fix
PAAS-4017."*

**The loss is recoverable, deterministically, and that is the sequel work.** The ticket keys for those
sessions exist in artifacts unjira does not yet collect:

| artifact | carries the key? |
|---|---|
| branch name | **no** — 0 of 3 (`paas-xelasticache-autoscaling` ↛ PAAS-3939) |
| commit messages | **yes** — 32 of 47 commits across the three PRs |
| PR titles | **yes** — 3 of 3 |
| Claude Code SCM tool inputs | **yes** — 10 of 20 sessions (F20) |

## Non-goals

- **The Jira dev panel is rejected.** `/rest/dev-status/latest/issue/detail` does return the
  branch↔key mapping (verified live: 28 of 115 keys have PRs, 9 branches match a collected session
  branch). But it is an internal unversioned endpoint requiring an opaque
  `applicationType=oAuth-com.github.integration.production` — `GitHub` returns empty — and it is
  *tracker state*: Jira's account of what it thinks your SCM did. Collecting the SCM directly is
  strictly better and makes it redundant.
- **No per-event truncation here.** That is F16, and sizing a cap against a corpus that should not be
  there would calibrate it wrong. Land this first, re-measure, then cap.
- **No schema change.** The partition is a pure function over artifacts that already exist.

## Testing

- `PartitionByTrackerRecord` — pure, table-driven: marked, unmarked, wrongly-typed artifact (must read
  as absence, matching `IsTrackerRecord`'s documented behaviour), empty input.
- `RunNarrate` excludes tracker records from candidates, and reports the count.
- Repeated passes do not re-cluster excluded events *at model cost* — assert the fake client's call
  count, not just the narrative count.
- `ProvenanceTrackerIntent` ranks below `ProvenanceProseFirst` and above `ProvenanceProseLater`.
- A same-window transition into a configured working status **creates** a candidate for a session that
  names no key.
- Empty `working_statuses` produces no tracker-intent candidates.
- **The regression gate:** build narrative 18's exact event shape (session prose naming PAAS-4017,
  plus PAAS-4017 tracker records) and assert the primary link survives with zero tracker records in
  the cluster. If this fails, the coupling is deeper than measured and this design is wrong.

## Open questions

- Should a tracker record ever enter clustering as *context* (visible for `EXTENDS` judgment) while
  being excluded as a *candidate*? This design says no — 82% of the payload is the reason — but
  context and candidacy are genuinely separable, and the F16 probe can measure the cost of re-admitting
  context-only.
- `ProvenanceTrackerIntent`'s window is "same day" in the measurement above. Same-day is a crude
  bound; the session's own window is better but needs the narrative to exist first, which is a
  chicken-and-egg with matching. Unresolved.
- 7 of 115 keys failed to probe against the dev panel. Undiagnosed, and moot if the dev panel is not
  used.
