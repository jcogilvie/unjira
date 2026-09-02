# Narrative→issue matching — design

Writes `narratives.issue_key`, which nothing does today. This is the blocker that resequenced the
reconciler: its verification step is conditional on an issue link, so on current code that branch is
unreachable, every narrative is unlinked forever, and every drafted action would be a `create`.

`README.md` assigns matching to the **correlator**, not the reconciler.

## Amendment: per-candidate tracker resolution (landed 2026-09-02, task #130)

As shipped, `Match` took ONE `tasktracker.TaskReader` for a whole pass, resolved by the caller from a
single default project. For the jira backend that project selects which `JiraConnection` to talk to —
so on a multi-connection setup, a candidate whose `Connection` named a different one was verified
against the **wrong site**. It 404'd for a ticket that exists, or, if the key collided, resolved to an
unrelated issue. Either way it landed in `Unresolved`, indistinguishable from a genuinely stale key.

That is precisely the PAAS/SUMO cross-connection case the `same_work` role below exists for, so the
design's own headline scenario was unreachable in practice.

`Match` now takes a `correlator.TrackerResolver` — `func(connection string) (TaskReader, error)` —
and `verifyCandidates` calls it **per candidate**. A function rather than a map or a widened
interface, because the correlator must not learn what a "connection" is: it selects a site for jira,
means nothing for the local backend, and would mean something else again for GitHub. Resolution stays
with `cmd/unjira`, which has the config.

Three cases, deliberately distinguished:

| Candidate | Resolution |
|---|---|
| `Connection` names a configured connection | that connection's own tracker, built once and memoized |
| `Connection` empty (branch- or prose-derived, the common case) | the default project's tracker — a documented guess |
| `Connection` names something unconfigured (renamed, removed, typo) | recorded in `Unresolved` **with the reason**, per candidate |

The third is the new failure mode this introduces, and it is deliberately *not* treated as a
transport error. A config gap does not fix itself between passes, so aborting the narrative would fail
it on every pass forever — the exact "genuinely deleted ticket misclassified as transport" trap
`IsTransportError`'s doc comment warns about. It also carries the reason, unlike the ordinary
not-found case which records a bare key: a bare key would tell a reviewer a ticket is missing when the
truth is that unjira never looked.

`correlator.SingleTracker` adapts one tracker to a resolver, so the local backend and every
single-connection setup stay a special case of the general path rather than a second code path.

**Still open (task #177):** the reconciler has the same shape. `verifyLinks` reads every link through
one tracker regardless of `store.NarrativeIssue.Connection`, which is persisted and available. Out of
scope here, and the seam it needs now exists.

## The problem is attribution, not extraction

The obvious framing — "find the ticket key in the text" — is already solved.
`events.ExtractTicketKeys` does it deterministically, and the Jira collector writes a structured
`issue_key` artifact on every event. Neither is read for matching today.

The real problem is that a narrative's events routinely name **several** keys, with no signal about
which one owns the work. `internal/collector/claudecode` scans every message in a session transcript
plus the git branch and flattens all hits into one `ticket_keys` artifact
(`claudecode.go:162,171,239`). A session that mentions `PROJ-100` as prior art, `PROJ-205` as a
tangent, and implements `PROJ-42` off branch `feature/PROJ-42` yields
`["PROJ-100","PROJ-205","PROJ-42"]` — a bag of mentions, not a claim of attribution.

`rules/verify-correlations.md` records what goes wrong here. The predecessor confidently reported a
`repo#573 = PROJ-3852` binding where *both halves were wrong*. That was a wrong **attribution**, not
a failure to find a key. The rule's conclusion is load-bearing for this design: "Confidence scores
are not enough; a hallucinated match can be high-confidence."

## Sometimes several issues legitimately own the work

A real workflow in use today: feature work is tracked in `PAAS`, but the production deployment is
gated by a System Change task in `SUMO`. Two tickets, one body of work, both legitimate.

The first instinct is to model this as a dependency — `gating`. That is wrong, and the distinction
matters because it changes what the reconciler may do. This is not "A blocks B"; it is **one body of
work represented in several places for different audiences**: `PAAS-x` is the engineering record,
`SUMO-y` is the change-management record. A genuine blocker would be a *different narrative's* work.

So the model is co-representation, not dependency:

| Model | What a consumer does |
| --- | --- |
| dependency (`gating`) | Update the primary; separately note the blocker |
| **co-representation (`same_work`)** | **The narrative owns both. Both may need updating, possibly with different content** |

This also means `narratives.issue_key` — a single `TEXT` column — cannot express the truth on its
own.

## Roles: a closed enumeration

An open string field would rot. If the model emits `related-to`, `relates`, `blocked-by`, and
`blocker` as synonyms, every consumer must pattern-match a vocabulary that grows without bound. A
closed set lets the parser **reject** an unknown role loudly, which this codebase prefers over
accepting data it cannot interpret.

Each role must earn its place by changing downstream behaviour — otherwise the enum is modelling
English, not consequences.

| Role | Meaning | Cardinality |
| --- | --- | --- |
| `primary` | The record this work is principally tracked in | exactly 1 |
| `same_work` | A co-representation of the same body of work in another tracking context | 0..n |
| `mentioned` | Referenced in the events, but not the work | 0..n |

`primary` is what `narratives.issue_key` denormalizes, so existing readers keep working untouched.

### Rejected roles, and why

- **`gating`** — the motivating case turned out to be co-representation. A true blocker belongs to
  another narrative. Recording it as a role here would assert a dependency the data does not show.
- **`caused_by` / `discovered_while`** — both real relationships, but no statable behavioural
  difference from `mentioned`: all three mean "do not attribute the work here." Adding them now
  invents distinctions ahead of any consumer. The enum grows additively when an action type needs
  the difference.
- **Jira's native link types** (`Blocks`, `Duplicate`, `Clones`, `Relates`) — these describe
  ticket-to-ticket relationships as humans filed them, which is a different question from "does this
  work narrative belong here." Adopting five would produce five roles that cannot be told apart
  behaviourally.

## What is NOT used to classify

**Workflow graphs.** `internal/workflow` models status transitions *within one issue* —
`Observe(fromStatus, toStatus)`, `Neighbors`, `Path`, mined per project from changelogs. It has no
concept of a second issue. Wrong shape; using it would be overloading.

**Jira issue links.** The system of record for exactly this relationship, and the most accurate
possible source — but reading them is new collector scope (a new event kind or artifact), the local
backend has no equivalent, and the classification benefit is unproven. Deliberately deferred.

**`rules/`, in this slice only.** `rules/README.md` states the intended mechanism: "The correlator
and reconciler load these into their prompts each pass," with frontmatter carrying
`scope: correlator | reconciler | estimator` and `confidence: high | provisional`. **Verified: no Go
code reads `rules/`** — the six files are documentation and the loader was never built.

This is the right long-term home for the PAAS/SUMO knowledge: non-generalizable org process,
human-auditable, diffable, and explicitly earmarked for it. It is a separate slice so it serves all
three declared scopes rather than being half-built for the correlator. Matching classifies
`same_work` without it; it will classify better with it.

## Candidate generation and provenance

Candidates come only from deterministic extraction. Provenance tiers are limited to what is actually
derivable from today's artifacts:

| Tier | Source | Strength |
| --- | --- | --- |
| `branch` | `ExtractTicketKeys(artifacts["git_branch"])` | strong |
| `jira_event` | `artifacts["issue_key"]` on a jira-source event | strong |
| `prose_first` | first key in `ticket_keys` order | weak |
| `prose_later` | subsequent keys | weakest |

`git_branch` survives as its own artifact (`claudecode.go:238`), so branch provenance is recoverable
today with no collector change — re-run `ExtractTicketKeys` over it at match time.

**Stated limitation:** prose keys are not separable from each other. `addKey`
(`claudecode.go:136-139`) dedupes into one ordered slice, so "prior art" and "tangent" are
indistinguishable; only first-mention order survives. No commit-trailer tier is invented, because
nothing records commit trailers.

Keys matching `exclude_from_linking` are dropped from candidacy but recorded as excluded, honouring
README's promise that such a key "stays visible for later triage instead of vanishing."

### No semantic search (settles task #29)

No vector index in this slice. The keys are usually already in the text, so embedding similarity
would add a dependency and an index to solve a problem deterministic extraction already covers.
`README.md` calls a vector store "a real future need"; the honest trigger for revisiting is evidence
that zero-candidate narratives are common in practice — which this slice will produce, since it
records them.

## Placement: a separate pass

```go
// Role is the closed set above. An unrecognized value from the model is a
// parse error, never coerced.
type Role string

const (
	RolePrimary   Role = "primary"
	RoleSameWork  Role = "same_work"
	RoleMentioned Role = "mentioned"
)

// Provenance records how a candidate key was found, so a wrong match is
// diagnosable after the fact rather than only visible as a bad outcome.
type Provenance string

const (
	ProvenanceBranch     Provenance = "branch"
	ProvenanceJiraEvent  Provenance = "jira_event"
	ProvenanceProseFirst Provenance = "prose_first"
	ProvenanceProseLater Provenance = "prose_later"
)

// NarrativeIssue is one (narrative, issue) link — the narrative_issues row
// shape, mirroring how store.NarrativeRow mirrors narratives.
type NarrativeIssue struct {
	IssueKey string
	Role     Role
	// Provenance is stored as a plain string rather than the Provenance type:
	// the row shape lives in internal/store, which must not import the
	// correlator, and a persisted value read back from SQLite is untyped text
	// regardless. The correlator converts at the boundary.
	Provenance string
	Confidence float64
	// Connection is the config.JiraConnection.Name that resolved this key,
	// recorded because a co-representation can live on a different site than
	// the primary — which is what makes the PAAS/SUMO case cross-connection.
	Connection string
}

// MatchResult is what one narrative's resolution produced, for rendering and
// for tests. Links is empty when no candidate survived verification.
type MatchResult struct {
	NarrativeID int64
	Links       []NarrativeIssue
	// Primary is the promoted key, or "" when there was none or when
	// confidence fell below ConfidenceFloor.
	Primary string
	// Excluded and Unresolved carry the candidates that were dropped, and why,
	// so a narrative that looks untracked can be explained without re-running.
	Excluded   []string
	Unresolved []string
	// Rationale is the model's stated reason when an LLM call was made; empty
	// for the deterministic single-candidate and zero-candidate paths.
	Rationale string
}

func Match(
	ctx context.Context,
	s *store.Store,
	tracker tasktracker.TaskTracker,
	client llm.Client,
	cfg config.MatchConfig,
) ([]MatchResult, Stats, error)
```

`Persist` continues to write narratives unmatched. `Match` then selects narratives lacking an
`issue_key` and resolves them.

Why not inside `Persist`: matching needs both `GetIssue` network calls and an LLM call, and
`Persist`'s contract is that no SQLite transaction is held open across an LLM round-trip
(`correlator.go:592-599`). Folding matching in would also couple clustering to tracker availability,
so a Jira outage would fail narration entirely.

Selecting on "no `issue_key` yet" makes the pass **re-runnable over a backlog**: a tracker outage
defers matching rather than losing it. It returns the existing `Stats` type so `dev narrate` reports
LLM cost without new plumbing.

`Match` acquires no lease; the caller holds it, matching `RunNarrate`'s existing convention
(`narrate.go:69-72`).

## Resolution flow

Per unmatched narrative:

1. **Gather candidates** from every linked event. Deterministic, no I/O.
2. **Zero candidates** → leave unlinked. No LLM call, no tracker call. This is README's
   "untracked-work detection is the default path, not a special case," and it is the cheap common
   case.
3. **Verify every candidate** via `tracker.GetIssue`. A key that does not resolve is dropped and
   recorded as unresolved. This happens regardless of provenance strength, because
   `verify-correlations.md` requires it and because a strong-provenance key can still be a stale
   branch name.
4. **One survivor** → `primary`, deterministically. No LLM call.
5. **Multiple survivors** → one LLM call with the narrative and each candidate's
   summary/description/status, returning a role, confidence, and rationale per candidate.
6. **Write** in a single transaction.

Steps 3 and 5 are exactly why this cannot live inside `Persist`.

## Store seams

All of the following are absent today. Naming them here is the slice-3 lesson applied: that slice
shipped without any way to assemble `Cluster`'s inputs because every test hand-built them.

```sql
CREATE TABLE IF NOT EXISTS narrative_issues (
    narrative_id INTEGER NOT NULL REFERENCES narratives (id),
    issue_key    TEXT    NOT NULL,
    role         TEXT    NOT NULL,   -- primary | same_work | mentioned
    provenance   TEXT    NOT NULL,   -- branch | jira_event | prose_first | prose_later
    confidence   REAL,
    connection   TEXT,               -- which JiraConnection resolved it
    created_at   TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    PRIMARY KEY (narrative_id, issue_key)
);

-- Exactly one primary per narrative, enforced by the database rather than by
-- convention: narratives.issue_key denormalizes the primary, so a second
-- primary row would make that column arbitrary. The composite PRIMARY KEY
-- above prevents duplicate *keys* per narrative but permits two rows both
-- marked primary, which is the case this closes.
--
-- Verified against modernc.org/sqlite v1.56.0: a second primary insert fails
-- with "UNIQUE constraint failed: narrative_issues.narrative_id (2067)", while
-- same_work/mentioned siblings and other narratives' primaries insert fine.
CREATE UNIQUE INDEX IF NOT EXISTS one_primary_per_narrative
    ON narrative_issues (narrative_id) WHERE role = 'primary';

-- Supports the reverse lookup (issue_key → narratives), which is unindexed
-- otherwise since issue_key is the trailing column of the composite key.
CREATE INDEX IF NOT EXISTS narrative_issues_by_key
    ON narrative_issues (issue_key);
```

### Directionality: there is none between issues

Every row relates a **narrative to an issue**. There is no issue-to-issue column, so nothing ever
compares two key columns for a match.

`same_work` is not a claim that `PAAS-x` relates to `SUMO-y`; it is a claim that *this narrative's
work* is also recorded in `SUMO-y`. Co-representation is therefore expressed by a **shared
`narrative_id`**, and the PAAS/SUMO case is two sibling rows:

| narrative_id | issue_key | role |
| --- | --- | --- |
| 42 | PAAS-x | `primary` |
| 42 | SUMO-y | `same_work` |

Symmetry is derived, not stored: "what else is this work recorded in?" is one query on
`narrative_id`. Nothing asserts "SUMO-y is same_work-with PAAS-x" as its own fact, which keeps the
table free of a relation that would then need maintaining in two places.

The asymmetry that *does* exist is `primary` versus `same_work` — a difference of role within one
narrative, not a directed edge between issues. The partial unique index above is what makes "exactly
one primary" true rather than merely intended.

Follows the 10 existing idempotent `CREATE TABLE IF NOT EXISTS` statements; still greenfield, so no
migration machinery.

```go
func (s *Store) NarrativesWithoutIssueKey(limit int) ([]NarrativeRow, error)
func (s *Store) NarrativeIssues(narrativeID int64) ([]NarrativeIssue, error)
func (s *Store) AllNarrativeEvents(narrativeID int64) ([]events.Event, error)
func (tx *Tx) SetNarrativeIssueLink(id int64, issueKey string, confidence float64) error
func (tx *Tx) AddNarrativeIssues(narrativeID int64, links []NarrativeIssue) error

// NarrativesForIssue is the reverse direction: every narrative that links this
// issue, in any role.
//
// Matching itself never needs it — it walks narrative → issues. It is specified
// here anyway because the reconciler will: before commenting on a same_work
// ticket it has to ask "did another narrative already update this?", and a
// shared SUMO ticket makes that a real duplicate-comment risk rather than a
// hypothetical one. Adding the accessor and its test now costs less than
// discovering the seam missing mid-implementation, which is the lesson slice 3
// paid for.
func (s *Store) NarrativesForIssue(issueKey string) ([]NarrativeIssueRef, error)

// NarrativeIssueRef pairs a link with the narrative holding it, since the
// caller of the reverse lookup knows the key already and needs the narrative.
type NarrativeIssueRef struct {
	NarrativeID int64
	Role        Role
	Confidence  float64
}
```

`AllNarrativeEvents` is load-bearing and easy to miss: `NarrativeEventsForContext` deliberately
excludes pre-compaction-boundary events. Matching must see **every** event ever linked, or a
compacted narrative silently loses the `git_branch` artifact carrying its strongest signal.

`PRIMARY KEY (narrative_id, issue_key)` makes re-running idempotent — the same property
`(source, external_id)` gives collectors.

### Verified safe: the denormalized pointer

Only two production sites read `IssueKey` — `narrate.go:199` (copies it into `Cluster`'s context)
and the struct field itself. Keeping `narratives.issue_key` as the primary's denormalization means
both keep working with no change.

## `tasktracker.Issue` gains `Description`

`Issue` carries `Summary` but no `Description`, and `normalizeIssue` reads only summary, status, and
labels (`tracker.go:73-83`). The Jira collector's own spec names summary *and* description as the
matching signal, so the gap blocks the classification step.

This does **not** contradict the collector's decision to exclude `summary`/`description` from its
`searchFields`. That decision (jira-collector spec, "Collect flow" step 3) is about **the event
log's shape** — requesting them would invite emitting a snapshot event on every pass, an event
stream of "the description is still X," which is not an observation that anything happened. Matching
reads description on the **live path**, via a `GetIssue` call `verify-correlations.md` already
requires. Different paths, different purposes; the field is free, being extra fields off a call
already obligated.

The local backend needs no new storage: `store.LocalIssue.Description` already exists
(`store.go:386`) and is simply dropped at the `tasktracker.Issue` boundary.

## Errors

**Per narrative, not per pass.** A tracker 500 on one narrative's candidates leaves that narrative
unmatched and moves on, accumulating failures via `errors.Join`. Unmatched is a valid resting state,
so a failed pass costs a retry and nothing else — the same reasoning as the Jira collector's
per-query isolation.

An unknown role in the LLM response is a hard parse error, not a coerced value.

## Config

New `MatchConfig`, validated like `CorrelatorConfig` (which errors loudly on zero/negative):

```go
type MatchConfig struct {
	MaxCandidatesPerNarrative int     `json:"max_candidates_per_narrative"`
	ConfidenceFloor           float64 `json:"confidence_floor"`
}

func (c MatchConfig) Validate() error
```

`Validate` rejects a negative `MaxCandidatesPerNarrative` (zero means the default) and a
`ConfidenceFloor` outside `[0, 1]` — a floor above 1 would silently never promote a primary, which
would present as "matching is broken" rather than as a misconfiguration.

- `MaxCandidatesPerNarrative` (default 10) — bounds both the LLM prompt and the `GetIssue` fan-out.
  Hitting it is logged, never silent, naming the narrative: a silent cap would look like a clean pass
  that quietly ignored the candidate that mattered.
- `ConfidenceFloor` — governs **only** whether a `primary` is promoted into the denormalized
  `narratives.issue_key`. Below the floor, every `narrative_issues` row is still written — including
  the `primary` one — but `narratives.issue_key` stays NULL.

  It deliberately does not filter `same_work` or `mentioned` rows, and never suppresses a
  `narrative_issues` write. Those rows are the evidence a human needs in order to judge a bad match;
  dropping them would make a low-confidence narrative indistinguishable from one where nothing was
  found. The floor governs what unjira *asserts*, not what it *records*.

  Consequence, and it is intended: a narrative whose primary falls below the floor stays selectable
  by `NarrativesWithoutIssueKey`, so a later pass — with better rules, or after the ticket is
  corrected — revisits it. The floor cannot be used to permanently silence a narrative.

## Testing

Offline, following existing patterns:

- A fake `TaskTracker` plus the existing `fakeLLM` shape (`correlator_test.go:28-56`); real
  temp-file SQLite via the `openStore` helper.
- Table-driven: zero candidates, one candidate, many candidates, unresolvable key, excluded key,
  unknown role rejected, and an idempotent second run.
- Store tests for each new accessor against a real DB, including two invariants the schema now
  enforces rather than merely intends:
  - inserting a second `primary` for one narrative fails (the partial unique index), while
    `same_work`/`mentioned` siblings and another narrative's own `primary` succeed;
  - `NarrativesForIssue` returns every narrative linking a shared key across roles — the
    duplicate-comment risk the reconciler will need it for.

Break-it drills on the two load-bearing behaviours:

1. **`AllNarrativeEvents` sees pre-boundary events** — break by substituting
   `NarrativeEventsForContext`. A fixture without a compaction boundary would pass either way, so
   the fixture must have one.
2. **Verification actually gates** — break by trusting `branch` provenance without calling
   `GetIssue`. This is the `verify-correlations.md` failure mode.

Live tier: seed two issues in different projects, confirm a real `GetIssue` resolves both and that
`Description` arrives populated — the one thing fixtures cannot establish, since they encode what we
believe the API returns.

## Out of scope

- `internal/reconciler` and any action policy for `same_work` — this slice records links; the
  reconciler decides behaviour.
- The `rules/` loader (its own slice).
- Semantic/vector search.
- Reading Jira issue links.
- Any write path to Jira. This slice reads.
