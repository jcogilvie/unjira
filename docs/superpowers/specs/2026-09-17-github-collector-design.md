# GitHub collector — design

**Status: design**

Pulls GitHub pull-request activity into the event log as a second **work-evidence** source,
alongside `claude_code`. This is the moment `internal/correlator/refs` and
`internal/correlator/fanout` — pure, tested, zero-caller since the Python→Go port — get the
collector `CLAUDE.md` says they are waiting for. It does **not** close finding **F1**: `fanout` gets a
real integration point but is deferred past this slice, and §6 finds that `refs` has no consumer even
*with* a collector in hand. F1 narrows rather than closes.

## Which side of the diff GitHub sits on (and why that depends on the deployment)

Before any event shape can be decided, one architectural question has to be answered first, because
every other decision in this spec depends on the answer: **which side of unjira's reconciliation does
GitHub sit on?**

`README.md`'s own framing: unjira diffs **reality** (event streams: Claude Code, GitHub, Slack)
against **the tracker** (what the org believes — today, Jira; `tracker.backend` also accepts
`local`, a mimicked tracker for when no real one is reachable). `internal/tasktracker.TaskTracker` is
the interface that makes something "the tracker" in this codebase's vocabulary — `clients/jira` and
`clients/local` implement it; nothing else does, and this spec does not add a third implementation.
GitHub, in the collector this spec designs, is a `pipeline.Collector` exactly like `collector/jira`
and `collector/claudecode` are — but unlike `collector/jira`, it does not read from the connection
`gate.Applier` writes to. It reads from a system unjira **observes** and does not **govern**.

That placement answers the spec's hardest open question — **for this deployment, not for all
deployments.** The distinction is load-bearing, because unjira must operate whether or not a
collector's system is also the tracker, across arbitrary combinations (Jira, GitHub, GitLab, Trello,
YouTrack trackers; collectors for each of those plus Slack and other streams):

> **No event this collector emits is a tracker record in a deployment where GitHub is not the
> tracker** (`events.ArtifactTrackerRecord`). Slice 1's events — PR opened, PR merged/closed — are
> work evidence in *every* deployment, for a reason independent of configuration: **a pull request is
> not a work item in a tracker, it is a thing somebody did.** That is what makes the scope decision
> below safe to settle now, ahead of any multi-tracker design.

**Tracker-record-ness is a property of (artifact kind × which systems are trackers in this
deployment), not a property of the collector.** `events.ArtifactTrackerRecord`'s own doc comment says
so directly, while explaining why the marker is declared rather than inferred:

> *"**NOT the source name.** "Is this the jira collector?" is incident 21 exactly — the reconciler once
> recognized only Jira's own changelog vocabulary and would have silently never fired for a GitHub
> collector. **A GitHub-Issues collector's timeline events are tracker records too**, and must be
> recognized without editing any consumer."*

So "GitHub is never the tracker side" would be a *false* structural claim — contradicted by the file
that defines the concept. What is true structurally is narrower, and still sufficient for this slice:

| event kind | tracker = Jira | tracker = GitHub Issues |
|---|---|---|
| PR opened / merged / closed | work evidence | work evidence |
| Issue closed / labeled / commented | work evidence¹ | **tracker record** |
| Jira changelog entry | tracker record | work evidence¹ |

¹ Bookkeeping in a system that is *not* the tracker remains a real act the tracker does not know
about, so it is evidence. Symmetrically: a Jira comment becomes work evidence in a deployment running
GitHub Issues as the tracker with Jira as a merely-observed stream.

`events.ArtifactTrackerRecord`'s test — "if unjira writes prose sourced only from events like this
one, is it telling anybody anything they don't have?" — is therefore evaluated against *which systems
are trackers here*, not against how official an artifact looks. A GitHub PR body reads like
bookkeeping, but it is nobody's tracker bookkeeping: it is evidence something happened in the world,
which is exactly what unjira diffs against the tracker.

**Slice 1 avoids the ambiguous row entirely** by collecting no issue-shaped artifacts, so nothing here
needs config-aware classification yet. **A slice that collects GitHub Issues does**, and it must ask
"is GitHub a tracker in this deployment?" rather than answering from its own package name.
`CollectContext` already carries `Config`, so the collector *can* ask; what does not exist yet is a
typed way to get an answer — `config.JiraConnection` is Jira-shaped and `events.ArtifactConnection` is
documented as "the configured **Jira** connection name." That gap is **F28**, cross-referencing **F7**.

`docs/architecture.md` already names the hypothetical GitHub-Issues-as-tracker backend
(`clients/github` implementing `TaskWriter`) under `tasktracker.StatusCategory` — unbuilt, with no
`tracker.backend` value for it. If built, its *issues* surface needs `events.SetTrackerRecord` exactly
as `collector/jira` sets it today, and it would additionally need a self-identity equivalent to
`SelfAccountID` (see F28: the echo loop is the one thing that genuinely differs when collector and
tracker coincide). This spec's PR events stay work evidence either way.

**Consequence for `AnyWorkEvidence`.** Because no event from this slice is a tracker record, the mere
presence of one of its GitHub events in a narrative's delta always satisfies
`events.AnyWorkEvidence` — correct for PR lifecycle events, which are evidence real work happened
rather than bookkeeping to paraphrase. A later issues-collecting slice must not assume this: in a
GitHub-Issues-as-tracker deployment its events *are* bookkeeping, and a narrative holding only those
must not satisfy `AnyWorkEvidence`, exactly as a Jira-only narrative does not today.

`reconciler.suppressTrackerEcho` therefore behaves toward a GitHub-only narrative exactly as it does
toward a `claude_code`-only one today: it does not suppress on tracker-echo grounds. (It can still be
suppressed on *other* grounds — staleness, duplication — same as any narrative.)

**Consequence for identity design (§3).** Because these events are clusterable work evidence, not
inert tracker records, the identity scheme cannot follow the Jira issue-body precedent
(`<KEY>:body:<updated-unix>`, deliberately over-collecting on every field edit because an inert
duplicate tracker record costs nothing). Doing that here would mint a fresh clusterable candidate on
every trivial PR title/body edit — the expensive failure mode, not a safe one. §3 designs around this
directly.

## Scope

In scope:

- `internal/collector/github` — a `pipeline.Collector` implementation, PRs only.
- A `clients/github` facade (see §1 for why) — read-only for this slice.
- `config.Collectors["github"]` options (repos to watch; already stubbed inert in
  `config/unjira.example.json`).
- Registration in `cmd/unjira/main.go`'s `registry`.
- Wiring `internal/correlator/fanout` and stating exactly how (and why not yet) `refs` connects — see
  §6.

Out of scope, named so the smallest slice stays honest about what it is not:

- Reviews, review comments, CI/check runs, releases, direct-push commits (§2 says why, per artifact).
- Any write path. `clients/github` gets no write methods in this slice; there is nothing for
  `gate.Applier` to call, matching the Jira collector spec's own "this slice is read-only" precedent.
- Narrative→GitHub-object matching. Matching today resolves a narrative to a **Jira** issue key
  (`narrative_issues.issue_key`); this collector produces candidates for that same resolution (PR
  bodies/titles/branch names carry Jira keys), not a new kind of link.

**Success criterion**, matching the Jira collector spec's own bar: GitHub PR events appear in the
event log, correctly shaped, idempotently, visible to the correlator as work evidence — and
`fanout.ClusterFanout` demonstrably collapses a real fan-out family before an LLM call ever sees it.
It does not prove anything about issue-key resolution quality; that is an existing consumer
(`gatherCandidates`) this collector merely feeds better.

## 1. Transport: an HTTP client with a token from the environment, not the `gh` CLI

Three options, weighed against the same four axes the task named:

| | credential handling | rate limits | offline testability | dependency |
|---|---|---|---|---|
| **HTTP client + `UNJIRA_GITHUB_CREDENTIALS`** | the same path as Jira: `internal/credentials` decodes one env var (keyed by host — see §8) | self-managed — and it inherits F23's `retryTransport`, since a `RoundTripper` is already the seam | `httptest`, exactly as `clients/jira` and `clients/openai` do today | `google/go-github`, or none at all for a surface this small |
| **`gh` CLI (`exec.Command`)** | borrows whatever `gh auth login` left in the OS keychain | `gh` manages its own backoff, opaquely | a fake process runner — workable, but a second testing idiom | no Go dependency, but a **runtime binary dependency** with no introspection into its version, auth state, or behaviour |
| **GitHub MCP server** | session-scoped, and `unjira collect` is a cron/CLI invocation with no MCP session to borrow | opaque | cannot run in `go test ./...` at all — out-of-process, not fake-able | wrong layer: MCP is for an agent driving GitHub interactively, not an unattended collector |

**Recommendation: the HTTP client**, with its token read from the environment through
`internal/credentials`, the same way every Jira credential is read. An earlier draft of this spec
recommended `gh`; that is **withdrawn**, and the reasoning is recorded because the `gh` case was not
unreasonable — it was wrong for reasons that only surface when you ask how it fails.

**Why `gh` was rejected.**

- **An undiagnosable runtime dependency.** `gh` is an external binary whose version, auth state, and
  output shape unjira cannot introspect. `--json` field availability varies across versions; a token
  can be absent, expired, scoped wrongly, or attached to the wrong account; a keychain can be locked.
  Each is a different failure, and a subprocess surfaces them all as an exit code plus a line of text
  that unjira would have to pattern-match to tell apart. An HTTP client gets a status code and a typed
  body — 401 is not 403 is not 404 — and `X-RateLimit-Remaining` as a header rather than something
  `gh` already decided on the operator's behalf.
- **Two credential mechanisms is worse than one.** unjira has exactly one answer to "where do secrets
  come from": a single JSON env var per credential kind, decoded by `internal/credentials`, keyed by
  the config connection it belongs to. Reading Jira's credentials from the environment while reading
  GitHub's out of another tool's keychain means two things to explain, two ways to fail, and two places
  to look when a cron cannot authenticate. `CLAUDE.md`'s "credentials come from the environment, never
  config files" is satisfied more directly by *being* an env var than by delegating to a tool that
  happens to store one elsewhere.
- **It would not have inherited F23's retry work.** `retryTransport` is a `RoundTripper`, chosen
  because "one decorator covers the whole surface, including methods added later"
  (`clients/jira/retry.go`). A GitHub HTTP client composes the same transport and gets
  transient-failure retry, the `GET`/`HEAD` method gate, and `Retry-After` handling for free. A
  subprocess inherits none of it and needs its own retry story — real work the `gh` option was
  implicitly charging to "zero dependencies."

**What the operator does instead**, one line in the README. `gh` remains a fine way to *mint* a token;
it just is not unjira's transport:

```sh
export UNJIRA_GITHUB_CREDENTIALS="{\"github.com\":{\"token\":\"$(gh auth token)\"}}"
```

A classic PAT or a fine-grained token works identically. The point is that unjira reads a token from
its own environment and never consults another tool's state at runtime.

**Credential shape.** `credentials.Credential` is `{Email, Token}`, and GitHub needs no email. **Reuse
the type with `Email` empty** rather than adding a parallel kind: a second near-identical type invites
the two decoders to drift, and the package is explicitly built around "exactly one parser." `EnvVar`
becomes per-kind (`UNJIRA_JIRA_CREDENTIALS`, `UNJIRA_GITHUB_CREDENTIALS`), which is the shape the
package already anticipates — its own comment frames it as "one var per credential kind, not one pair
per connection." **Open question:** whether `FromEnv` grows a kind parameter or the package grows a
second constructor. Both preserve the single-parser property, which is the part that matters.

**Dependency: start with no SDK.** This slice needs one listing call. `google/go-github` is
well-maintained and a reasonable later choice, but a REST response through `encoding/json` needs no
library, and the facade exists precisely so swapping the innards later does not ripple. This differs
from `clients/jira`, which wraps `go-jira` because Jira Cloud's API is large and churning — the GitHub
surface here is not. **Open question**, cheap to revisit once real pagination and secondary-rate-limit
behaviour show up in the live tier.

**The `internal/clients/github` seam**, per `docs/go-conventions.md`'s "remote-system clients live
under `internal/clients/<system>`" rule and the README's own locked list
(`clients/litellm/clients/github/clients/slack` — `README.md:301`) — a thin facade, no business logic,
mirroring `clients/jira`'s shape including a constructor that takes an `*http.Client` so the caller
composes transports:

```go
// internal/clients/github/github.go

// Client is a facade over GitHub's REST API, exposing only the surface unjira
// needs. Takes an *http.Client so the caller composes transports — in
// particular the retry decorator clients/jira already uses, since "one
// decorator covers the whole surface, including methods added later" applies
// identically here.
type Client struct {
    http  *http.Client
    base  string // api.github.com, overridable for httptest and GHES
    token string
}

// ListPullRequests returns every PR in repo updated at or after since.
// Ordering is requested explicitly (sort=updated&direction=asc) rather than
// assumed, mirroring the Jira collector's "the JQL carries no ORDER BY" note
// (jira.go:186) — a watermark advanced from unordered results skips events.
func (c *Client) ListPullRequests(repo string, since time.Time) ([]PullRequest, error)
```

Offline tests use `httptest`, the idiom `clients/jira` and `clients/openai` already establish — one
testing pattern for every remote system rather than a second one for subprocesses. A **live** tier
(`internal/live`, `live` build tag, as the Jira collector spec's live tier works) hits real GitHub
against a low-stakes public repo; this project's own `jcogilvie/unjira` is the natural target.

**Rejected: a hybrid** (HTTP for reads, `gh auth token` shelled out for the credential). That keeps the
runtime binary dependency and its undiagnosable failure modes to save one `export`, and puts a
subprocess call on the critical path of every authenticated request.

## 2. What is collected: PR lifecycle only, in this slice

Two event kinds, chosen the same way the Jira collector spec chose its four — "derived from what
phase 1 can actually *do*," not from completeness:

| `ExternalID` | `OccurredAt` | Source data |
|---|---|---|
| `<owner>/<repo>#<N>:opened` | PR's `created_at` | the PR list response, at collection time nearest to creation |
| `<owner>/<repo>#<N>:merged` or `:closed` | PR's `merged_at` or `closed_at` | same, once the PR leaves the open state |

Both carry the PR's title + body as `Summary` (format: `"<owner>/<repo>#<N>: <title>\n\n<body>"`,
mirroring `EventFromIssueBody`'s `"<KEY>: <summary>\n\n<body>"` shape), `Actor` = the PR author's
login, `RawRef` = the PR's HTML URL, and artifacts:

- `events.ArtifactGitBranch` = the PR's head branch name — this is the **exact same artifact key**
  `collector/claudecode` already writes, and `gatherCandidates` already re-derives ticket keys from
  it via `ProvenanceBranch` (`match_candidates.go:121`, the strongest tier short of a human
  reviewer). No new provenance tier is needed for this: a PR's head branch is the identical signal a
  Claude Code session's branch is — "an explicit human act of naming the ticket for this work" — and
  reusing the existing artifact key means `gatherCandidates` needs **zero changes** to consume it.
- `events.ArtifactSCMKeys` = ticket keys extracted from the PR's **title and body** via
  `events.ExtractTicketKeys`. This reuses `ProvenanceSCMCommand`'s existing tier
  (`match_types.go:109`) rather than inventing a GitHub-specific one, on the same reasoning
  `scm.go`'s doc comment gives for a commit message or `gh pr create` tool call: a PR title/body is
  "a developer naming the ticket for this work," the same explicit-authoring act, just observed from
  GitHub's side of the fence instead of Claude Code's. **This is a deliberate, load-bearing
  decision**, not a shortcut: it means this collector needs no new `Provenance` constant, no
  `Rank()` case, and no `gatherCandidates` change — every existing consumer already handles it
  correctly the day this collector ships.
- A private (collector-internal, per `docs/go-conventions.md`'s "only keys read outside their
  producing collector belong [in the shared vocabulary]") `repo`/`number`/`author` triple, used
  internally to build `fanout.Item` (§6) — not exposed as a shared `events.Artifact*` constant
  because nothing outside this collector and its own pipeline wiring reads it yet.

### Why these two, and not the rest

Phase 1's write surface is "comments and forward transitions" (`README.md`'s own Status section);
the two events above are exactly the moments that change what a comment or transition should say:
opening a PR is the strongest available signal that work started, and merging/closing is the
strongest available signal that it finished. Everything else considered and deferred:

- **Reviews / review comments.** Real, valuable (`rules/review-staleness.md`,
  `rules/bot-pr-noise.md` are both precedent-established norms this data would eventually feed), but
  add a genuinely new identity question this slice does not need to answer to ship (§3's "editable
  after submission" gap) and a second API shape (`/pulls/{n}/reviews`). Deferred
  to slice 2, once PR-lifecycle events are live and reviewed.
- **CI / check runs.** High-churn (reruns, flaky retries) in a way that mirrors
  `rules/bot-pr-noise.md`'s own documented failure mode almost exactly — a check-run stream risks
  becoming the new dependency-bump-approval noise. Deferred until there is real volume data to size
  a filter against, matching this codebase's own "measure before capping" discipline
  (`docs/architecture-findings.md` F16's four falsified proposals are the cautionary example).
- **Commits (within a PR, or direct pushes).** Real value — F20 measured 32 of 47 commits carrying a
  key in their message — but `collector/claudecode`'s own `scmKeys` (F20) already recovers commit
  messages for every commit *authored through a Claude Code session*, which is unjira's primary
  interface with git per `CLAUDE.md`. A GitHub-side commit collector's *marginal* value is commits
  made outside a Claude Code session (another tool, another teammate) — real, but a second event
  kind and a second API call (`/pulls/{n}/commits`) this slice does not need to ship to prove
  the seam.
- **Releases.** No consumer: phase 1 proposes comments and transitions, never anything a release
  event would inform. Same "an action type needs it, it gets emitted" discipline the Jira collector
  spec states for its own skipped changelog fields.
- **Merge commit as its own event.** Redundant with the `:merged` event above — same underlying
  fact, observed twice.

### Full text, no truncation — and this is where F16's own foresight pays off

PR bodies are exactly the "whale-shaped" input `docs/architecture-findings.md` F16 already
anticipated when it kept `correlator.WithMaxEventSummaryChars` alive after measuring it inert on the
current (all-Jira, all-`claude_code`) store: *"a future GitHub or Slack collector has whale-shaped
inputs (PR bodies, thread transcripts)."* This collector does not add a new cap — the existing one
already covers it, config-driven (`correlator.max_event_summary_chars`, currently `0` = unbounded in
`config/unjira.example.json`), reporting truncation counts on `Stats` the way it already does for any
source. Truncating here, by contrast, would repeat the Jira collector spec's own rejected argument
almost verbatim: `Cluster` already bisects on overflow and errors loudly on a genuinely irreducible
unit, so per-event self-censorship is unnecessary and violates the standing preference for erroring
loudly over silently dropping data.

## 3. `ExternalID` and idempotency — discrete lifecycle facts, not a revisioned snapshot

This is the section the identity-mutability concern actually resolves in, and the resolution follows
directly from §"Which side of the diff GitHub sits on": **because these events are work evidence
(clusterable), the Jira issue-body precedent — mint a fresh id keyed on a revision discriminator,
because an inert duplicate tracker record costs nothing — is the wrong model here.** Adopting it
verbatim (`owner/repo#N:body:<updated-unix>`, re-minting on every edit) would manufacture a fresh
clusterable candidate on every trivial title/body edit, which is expensive for the reason F18
measured precisely: tracker-record exclusion cut a clustering prompt 8.3× by keeping non-work-evidence
out of the pool; a duplicate work-evidence event does the opposite — it *adds* pool volume every time
a PR is touched.

The model that *does* transfer from Jira is a different one: `collector/jira`'s **changelog** events
(`EventsFromChangelogEntry`), not its issue-body event. A changelog entry is a discrete, immutable
historical fact — "at this instant, status moved from A to B" — recorded once and never re-derived
from a later, possibly-edited value. This collector's two event kinds are exactly that shape:

- `<owner>/<repo>#<N>:opened` is captured **once**, at (or shortly after) PR creation. GitHub's
  `created_at` does not change once set. The title/body captured are a **frozen snapshot at that
  instant** — if the author edits the title an hour later, this event does not update, and no new
  event is minted for the edit either. That is an accepted limitation, not an oversight, on the same
  grounds `collector/jira`'s Jira collector spec already accepts one for issues that leave a query's
  scope: **the reconciler's own live verification is the backstop**, not perfect freshness at
  collection time. (`rules/intent-not-outcome.md` already requires this regardless — a state-bearing
  action must confirm current state before acting, so a stale PR title in the event log was never
  going to be load-bearing for a *write* on its own.)
- `<owner>/<repo>#<N>:merged` (or `:closed`) is captured **once**, when the PR's state transitions.
  `merged_at`/`closed_at` are likewise set once. Whatever title/body is current *at that later
  moment* is what this event captures — a genuine (if partial) freshness improvement over the
  `:opened` snapshot, for free, simply because it is a second, later, independent observation of the
  same PR, not a re-derivation of the first one.

Neither id embeds a revision discriminator, and neither needs one: each names a specific, one-time,
immutable-once-recorded historical instant, exactly as `<KEY>:status:<changelog-id>` does for Jira. A
PR that is *reopened* after being closed (GitHub permits this) produces a **new** `:opened`-shaped
question this design leaves genuinely unresolved (see Open Questions) rather than silently
mis-keying it — reopening is rare enough, and consequential enough to get wrong, that guessing is
worse than naming the gap.

**One PR is (at most) two events, never one "current state" event.** This directly answers the
task's framing ("say what identity a PR event has, whether one PR is one event or many"): it is
many, but a small, *fixed* many — bounded by the lifecycle states this slice cares about (open,
end), not by how many times the PR was edited. A PR that is opened and never merged or closed (still
open at collection time, indefinitely) produces exactly one event, re-examined every pass (cheap: the
watermark, §4, means it is only re-fetched, not re-emitted, once `:opened` is already stored) until
it eventually closes.

**What re-collection does to an existing row.** `INSERT OR IGNORE` on `(source, external_id)` means:
once `<owner>/<repo>#N:opened` exists, re-collecting the same PR while it is still open is a no-op
(the row is frozen — including its title/body snapshot — exactly as F21 describes for any collector).
This collector does not attempt artifact re-derivation; per F21's own recommendation (deferred,
`docs/superpowers/specs/2026-09-17-artifact-rederivation-design.md`), that is a separate, later
mechanism this collector would use unmodified if it is ever built, not something this spec designs
its own version of.

## 4. Cursors and watermark

One cursor per configured repo, in the existing `cursors` table (`PRIMARY KEY (collector,
resource)`), following the Jira collector's own pattern exactly:

```
collector: "github"
resource:  "owner/repo"
position:  "<max PR updated_at seen>"
```

No hash-of-query component (contrast the Jira collector's `sha256(effective JQL)` watermark key):
there is no operator-authored query to invalidate here, only a fixed list of configured repos. Adding
or removing a repo from config changes which cursor *rows* exist, not what an existing row's
watermark means — so there is nothing analogous to the Jira collector's "widening `project_keys`
invalidates the old watermark" hazard.

The watermark bounds *which PRs are fetched*, mirroring the Jira collector's own discipline: it must
not be read as bounding which events are emitted for a PR that matches. In practice this matters less
here than for Jira's changelog case (a PR usually has few lifecycle transitions, not months of
history), but the principle transfers identically — a PR whose `updated_at` just crossed the watermark
(say, it was just merged) still gets its `:opened` event emitted if this collector has never seen it
before, e.g. a repo just added to config. The watermark advances only after a repo's list-and-fetch
completes without error, to the max `updated_at` observed — the same all-or-nothing-per-scope
discipline as the Jira collector's per-query advance, scoped here to per-repo.

**GitHub's two rate-limit buckets are the real constraint, and they are far apart**: `core` allows
5000 requests/hour while `search` allows **30/minute**, tracked separately (verified against
`/rate_limit` in this environment). That gap decides the query shape.

**Recommendation: use `GET /repos/{owner}/{repo}/pulls?state=all&sort=updated&direction=desc` — a
`core` endpoint — and never the search API for the incremental query.** 30/minute is a genuine
constraint for a `watch` loop across several configured repos; `core`'s 5000/hour has enormous
headroom for this collector's volume (one list call per repo per pass, plus pagination only as far
back as the watermark). The watermark comparison happens client-side: page through descending
`updated_at` and **stop at the first PR older than the watermark**, which also bounds pagination
naturally instead of fetching a repo's whole history. That is an intentional trade of one client-side
comparison for staying entirely inside the generous bucket.

`sort=updated&direction=desc` is requested explicitly rather than assumed — the endpoint's default
ordering is by PR number, and an early-stop scan over unordered results would skip events silently,
which is the failure mode the Jira collector's "the JQL carries no `ORDER BY`" note guards against.

Reading `X-RateLimit-Remaining` and `Retry-After` off the response is available here and worth using
for a warning when headroom runs low. That visibility is one of the concrete reasons this transport
was chosen over a subprocess, which reports neither.

## 5. Issue-key provenance — no new tier, reusing the existing ladder

Restated from §2 because the task asks for it explicitly: **branch name → `ProvenanceBranch`, title/body
→ `ProvenanceSCMCommand`.** Both are existing tiers (`match_types.go:88`, `:109`), already ranked
correctly relative to each other and to Jira-sourced/prose-sourced candidates. Measured precedent for
why `ProvenanceSCMCommand` is the right tier and not `ProvenanceProseFirst`: `scm.go`'s own doc
comment argues a `gh pr create` tool call is "the same explicit human act that makes ProvenanceBranch
the strongest inferred tier," and a PR's title/body — regardless of which side observed it — is that
same act. No `Provenance` case needs adding; `gatherCandidates`, `promoteCorroborated`, and
`rankCandidates` (`match_candidates.go`) need zero code changes to correctly rank a GitHub-sourced
candidate against a Claude-Code- or Jira-sourced one, because the artifact keys they already read
(`ArtifactGitBranch`, `ArtifactSCMKeys`) are exactly the ones this collector writes.

A candidate this collector's PR body/title genuinely cannot supply: whether an author *read* about a
ticket versus *authored against* it (`scm.go`'s own careful authoring-verb-only design). A PR body is
inherently an authoring artifact — nobody opens a PR to investigate, only to propose a change — so
there is no analogous "reading command" filter to build here; every PR title/body key extracted is,
by construction, an authoring-tier signal.

## 6. `refs` and `fanout` — how each wires in, and the one that genuinely does not yet have a home

### `fanout` — wires in, at the pipeline layer, before the LLM `Cluster` call

`fanout.Item{Repo, Author, Title, Number}` maps directly onto this collector's own PR data — the
collector's job is only to emit events; the **pipeline** layer is where fan-out detection runs,
analogous to how `events.PartitionByTrackerRecord` runs in `RunNarrate` today (see
`docs/superpowers/specs/2026-09-16-work-evidence-vs-tracker-state-design.md`'s §1 for that precedent)
rather than inside `collector/jira` itself. Concretely, in `internal/pipeline` alongside the existing
tracker-record partition:

```go
// internal/pipeline/narrate.go (extended)
work, trackerRecords := events.PartitionByTrackerRecord(candidates)
prGroups := groupByFanoutFamily(work)   // new: filters to github :opened events, builds
                                         // []fanout.Item, runs fanout.ClusterFanout
```

**What a detected family does, and why this stops short of merging events outright.** The package's
own doc comment says fan-out clustering "decides nothing about meaning (that is the LLM correlator's
job) — it just collapses the mechanical fan-out so the correlator sees one story instead of twelve
fragments." Taken literally, "sees one story" argues for physically reducing N raw events to one
before `Cluster` ever runs. This design does **not** do that, for a reason CLAUDE.md itself supplies:
"never silently drop data" and the entrance/exit-filter distinction the tracker-record work already
established — `PartitionByTrackerRecord` excludes events from the *candidate pool* while keeping them
fully queryable at the store layer, never deleting or merging rows. Physically collapsing 12 PR
events into 1 before clustering would mean 11 of them stop being independently visible to anything
downstream (a later `NarrativeEventsForContext` hydration, a future artifact re-derivation pass, a
human in `triage` wanting to see all 12 links) — the same category of loss `docs/design-notes.md`'s
incident 13 names for a different operation ("the unsafe case stops being forbidden and becomes
unreachable" is the right shape when the unsafe thing is a *write*; it is the wrong shape when the
"unsafe thing" is a human wanting to audit the raw data later).

So the design instead uses the **existing** `correlator.WithInstruction` mechanism
(`correlator/correlator.go:228`) — already built for exactly "a deterministic signal that should
strongly shape, but not dictate, one `Cluster` call's outcome" (its current caller is triage's
`[s]plit`, telling the model a reviewer has already judged something). A detected fan-out family
becomes an instruction appended to the cluster prompt: *"Events 3-8 are a detected environment-mirror
fan-out family (jcogilvie/infra#678-689, same author, same normalized title 'switch shared module to
managed mode'). Per this codebase's own operational history (a rule at
confidence: high), these are almost certainly one logical change and should become one narrative with
a range, unless you have specific evidence otherwise."* This keeps the LLM as the final arbiter of
*meaning* (a family could, rarely, span two unrelated coincidentally-similar PRs — the model can
still say so) while making the deterministic, high-confidence heuristic
(`rules/env-mirror-fanout.md`, unmodified) load-bearing on every pass rather than hoped-for.

**Why not a rule file instead of an instruction.** `rules/env-mirror-fanout.md` already exists and is
already rendered into every `Cluster` call via `WithClusterRules` — so in one sense this is "already
wired," and has been since the rule was written. What it lacks is the **per-window, per-PR
specificity** a static rule file cannot carry: the rule states the *heuristic* ("normalize titles,
group by repo+author+adjacent numbers"), but does not, and structurally cannot, name *which* events in
*this* window matched it — that requires running `fanout.ClusterFanout` against this pass's actual
data. The instruction is the deterministic pre-filter's output; the rule is the standing policy that
justifies trusting it. Both are needed, and `WithInstruction`'s own doc comment already establishes
the append order (rules, then instruction) that makes the two compose without conflict.

**Deferred to slice 2, explicitly.** Per the "smallest first slice" discipline, this design is
recorded now (`fanout.Item` construction, the `WithInstruction` integration point, the rejected
event-merging alternative) but **not implemented in slice 1**. Slice 1 ships bare PR events with no
fan-out detection; slice 2 adds `groupByFanoutFamily` once real fan-out data exists in a collected
store to validate the heuristic against — matching this codebase's own repeated lesson (design-notes
incidents 27, 32, 36: measure against real data before implementing a documented fix, because a
finding's diagnosis and its proposed mechanism have different evidentiary standing). Shipping slice 1
first is also what makes that later measurement possible at all.

### `refs` — extractable, but genuinely has no consumer yet, and this spec says so plainly

`refs.ParsePRRefs` matches GitHub's own `#N` / `owner/repo#N` cross-reference syntax — "Fixes #123",
"see #456-460" — inside PR bodies. This collector **can** run it (on PR body text, via
`refs.ParsePRRefs(body, refs.WithDefaultRepo(thisRepo))`) and **could** store the result as a new,
collector-private artifact for future use. This design recommends doing exactly that much — extract
and store, cheaply, deterministically — but stops there, because tracing the result forward exposes
a real gap `CLAUDE.md`'s own F1 entry did not have the collector in hand to check yet:

**There is no consumer for a GitHub-cross-reference in this pipeline, and building this collector
does not create one.** Phase 1's matching resolves a narrative to a **Jira issue key**
(`narratives.issue_key` → gone; today, `narrative_issues.issue_key`, per F11's resolution) — nothing
in `internal/correlator` or `internal/reconciler` represents "this PR relates to that other PR" as a
first-class relationship. `refs.Ref.Key()` produces `"owner/repo#123"`, which is not a Jira issue key
and cannot become a `narrative_issues` row under today's schema (that table's `issue_key` column is
explicitly a tracker key — `store.Role` values `primary`/`same_work`/`mentioned` are all defined
relative to *a Jira issue*, per `correlator.Role`'s doc comment discussing "the motivating PAAS/SUMO
case").

So `CLAUDE.md`'s own framing turns out to be exactly right, and this investigation confirms rather
than resolves it: **`refs` awaits not just *a* GitHub collector, but a GitHub-relationship-modeling
feature this spec does not design** — representing "PR A references PR B" or "PR A closes issue B"
as data a narrative or an action can use. That is a materially different, larger feature (a second
relationship type alongside `narrative_issues`, or a same-repo PR-to-PR graph) than "collect GitHub
PRs," and inventing a half-designed consumer here to give `refs` a caller would be worse than leaving
it uncalled with the reason recorded — the same restraint F1 itself already modeled for the
pre-collector state.

**Recommendation:** extract via `refs.ParsePRRefs` and store as a private artifact in slice 1 (cheap,
and it means the data exists the day a consumer is designed — the same "write now, read later"
shape `docs/architecture-findings.md` F6 defends for `ArtifactGitBranch`'s siblings), but do **not**
claim this collector "wires in" `refs` in the sense the Jira collector's own spec wired in
`gatherCandidates`. It does not. This is recorded as an **open question** below rather than resolved,
per the task's own instruction not to force a resolution where none is earned.

## 7. `claude_code` double-observation — the highest-risk interaction, addressed directly

The scenario: a Claude Code session runs `gh pr create` to open `owner/repo#456` for `PAAS-123`. Two
independent collectors now observe **the same real-world PR**:

- `collector/claudecode`'s `scmKeys` (F20) reads the tool call's input, extracting `PAAS-123` onto
  the **transcript** event's `ArtifactSCMKeys`.
- This collector's `:opened` event, reading the PR itself from GitHub's API, extracts the **same**
  `PAAS-123` from the PR's title/body onto its **own** `ArtifactSCMKeys` — plus the PR's head branch
  onto `ArtifactGitBranch`.

Both are work evidence (§"Which side of the diff GitHub sits on" + F18's existing claude_code treatment); both
are clusterable; both will very likely share a near-identical `OccurredAt` (the tool call and GitHub's
`created_at` are seconds apart at most).

**This is not automatically a bug — it is the corroboration case the architecture already has
machinery for.** `docs/superpowers/specs/2026-09-16-work-evidence-vs-tracker-state-design.md`'s own
measured table already shows "mixed" narratives — `claude_code` + `jira` events clustered
together — as "unjira's actual job," not a defect: three of 68 narratives in that measurement were
exactly this shape, and they are the ones matching resolves correctly. Adding `github` as a third
work-evidence source into the same clustering pool is architecturally identical to that existing
case, not a new category of problem. If `Cluster` correctly merges the transcript event and the PR
event into one narrative (both timestamped within seconds, both mentioning `PAAS-123`), the result is
*better* evidence, not duplicated evidence — `gatherCandidates`'s `upsert` (`match_candidates.go:91`)
already collapses the same key seen from multiple events in one narrative into one `Candidate`,
regardless of how many events named it.

**The real, residual risk is a clustering *miss*, not a matching miss.** If `Cluster` does *not*
recognize the transcript event and the PR event as one story — plausible, since their `Summary` text
differs (a tool-call description vs. a rendered PR title/body) even though they describe the same
act — the result is **two narratives for one piece of work**. Two backstops already exist for what
that costs, and this design leans on both rather than building a third:

- **At the write layer:** `reconciler.suppressDuplicates` (`reconciler.go:429`) already drops an
  action whose issue already has an open proposal from a *different* narrative — so even an
  unmerged pair cannot produce two comments/transitions on the same Jira issue. This is an existing,
  tested backstop, not something this spec adds.
- **At the review-queue layer:** an unmerged pair still costs two `Cluster`/`Match`/`Reconcile` passes
  and, if the two narratives' actions differ in *type* (one narrative proposes a comment, the other a
  transition), `suppressDuplicates` does not collapse them — a reviewer sees two queue entries about
  one PR. This is a real cost `suppressDuplicates` does not fully absorb, and this design does not
  claim it does.

**What would close the residual gap, and why it is not this spec's job.** The clean fix is a
deterministic cross-reference between the two event *sources*, not a smarter `Cluster` prompt: if
`collector/claudecode`'s `scmKeys` were extended to also extract a PR number (via `refs.ParsePRRefs`
on the same tool-call text it already reads) and stored it on a shared artifact both collectors
write, `gatherCandidates` — or a new, narrower deterministic pre-filter — could recognize "these two
events name the same `owner/repo#N`" before any model call, the same way `PartitionByTrackerRecord`
recognizes tracker records today. That is a genuine, concrete design, and it is explicitly **not**
built here: it requires a change to `collector/claudecode` (a second collector this spec does not
own) in addition to this one, making it a cross-collector feature rather than something "add a
GitHub collector" can ship alone. Recorded as an open question below.

**Recommended validation, once both collectors run together on real data**: measure, the way this
codebase's own design notes repeatedly insist on measuring before building (incidents 27, 32, 35, 36)
— for real `gh pr create` sessions, does `Cluster` merge the transcript event and the PR event into
one narrative or two? If the answer is "reliably one" (plausible: both carry the same ticket key in
`ArtifactSCMKeys`, which is exactly the kind of textual overlap clustering already uses), the
residual gap this section names may not be worth building a fix for at all — matching F16's own
lesson that a plausible mechanism is not a finding until measured.

## 8. Configuration

`config/unjira.example.json` already stubs `collectors.github` inert
(`{"enabled": false, "repos": ["yourorg/yourrepo"]}`) — this design fills that stub in rather than
inventing a new shape:

```json
"collectors": {
  "github": {
    "enabled": true,
    "repos": ["yourorg/yourrepo", "github.acme.corp/platform/infra"],
    "backfill_days": 30
  }
}
```

- **`repos`**, a flat list of `owner/repo` strings, optionally host-qualified as
  `host/owner/repo` — no per-repo query customization the way Jira's `queries[]` has, because there is
  exactly one query this slice runs (list PRs since watermark) and nothing analogous to Jira's "several
  views on one site" need. The host determines both which credential is used and which API base URL is
  called; see "Credentials are keyed by HOST" below for the parsing rule and why locality lives here
  rather than in a second parallel list.
- **`backfill_days`**, mirroring `collector/claudecode`'s own option name and default shape
  (`DefaultBackfillDays = 14`, `claudecode.go:28`) — how far back to look on a repo's first-ever
  collection pass (no existing cursor row). A GitHub-specific default of 30 is proposed (PRs often
  stay open longer than a Claude Code session backfill window needs to reach) but is genuinely a
  guess, not a measurement — flagged as an open question below rather than asserted as tuned.
- **No `project_keys`-shaped read-scope declaration**, unlike `config.JiraConnection`. There is no
  analogous "which projects can this write to" question, because this slice never writes to GitHub —
  `IsProjectWritable`'s whole reason for existing (gating `gate.Applier`) has no counterpart here
  until a write path exists, which this spec does not add.
- **No credential field in config**, per `CLAUDE.md`'s "credentials come from the environment, never
  config files." The credential arrives through the same mechanism as Jira's —
  `UNJIRA_GITHUB_CREDENTIALS`, decoded by `internal/credentials`, handed to the collector through
  `CollectContext.Credentials` (which `RunCollect` already passes "so a remote collector can
  authenticate without reading the environment itself"). But it is **keyed by host, not by a config
  connection name** — see below.

### Credentials are keyed by HOST, because GitHub is an identity provider

An earlier draft copied Jira's `map[connectionName]Credential` wholesale and proposed
`{"oss":{"token":"..."}}`. **That key was undefined**: this collector's config has no connections, only
a flat `repos` list, so nothing said what `"oss"` meant or which repos it covered. The symmetry with
Jira was carried further than the systems actually match.

**Where the two systems genuinely differ.** A Jira connection is a tenant: separate site, separate URL,
separate token, genuinely separate auth surface — so keying by connection name models something real.
GitHub is an **IDP**: one token carries org membership, and you do not authenticate per org. A user
belonging to five orgs on github.com has one auth surface, not five. Keying those by connection name
would ask an operator to invent names for distinctions that do not exist.

**But more than one auth surface does exist — by host.** A self-hosted GitHub Enterprise Server
instance and github.com are two credentials, two base URLs, and two account namespaces: structurally
the same as two Jira sites. That is the axis worth keying on, and the only one.

So the map stays (it is the same parser and the same package) and the key becomes the host:

```
UNJIRA_GITHUB_CREDENTIALS='{
  "github.com":        {"token": "ghp_..."},
  "github.acme.corp":  {"token": "ghp_..."}
}'
```

**`github.com` is the well-known key**, spelled exactly as the host. Not a sentinel like `"default"` or
`""`: an operator looking at a `repos` entry can read off which key applies without consulting a
mapping, and a typo produces "no credential for host github.com" rather than silently selecting a
default that happens to exist.

**Repo locality resolves from the repo string itself**, so nothing needs declaring twice:

```json
"repos": [
  "yourorg/yourrepo",                    // short form -> github.com
  "github.acme.corp/platform/infra"      // explicit host -> that GHES instance
]
```

A two-segment `owner/repo` **elides to `github.com`**, which keeps the common case exactly as short as
it is today and makes the existing example config still correct. A leading segment containing a `.` is
a host, and the remainder is `owner/repo`. Parsing rule, stated so an implementer does not improvise:
**split on `/`; if the first segment contains a `.`, it is the host, otherwise the host is
`github.com`.** Exactly two segments must remain after the host is removed, and anything else is a
config error naming the offending entry — a three-segment string whose first segment has no dot is
ambiguous, and guessing is how a repo gets silently collected from the wrong instance.

The API base URL derives from the same host: `github.com` → `https://api.github.com`, and a GHES host
→ `https://<host>/api/v3` (GHES's documented REST prefix). One field in config drives both the
credential lookup and the endpoint, which is the property that makes this better than a second
parallel list of hosts.

**Why not a flat `{"token": "..."}`.** It is tempting, and for a github.com-only deployment it is all
anyone needs. Rejected because GHES is not hypothetical and the migration is worse than the upfront
cost: a flat form would have to grow into a keyed form later, breaking every configured env var, to buy
the removal of one well-known key today. **Open question left open:** whether to also accept the flat
form as sugar for `{"github.com": ...}`. It costs one branch in the decoder and removes a small papercut
for the overwhelmingly common case, but two accepted shapes for one variable is exactly the kind of
thing that makes an error message harder to write. Not resolved here.

**Multiple identities on one host: out of scope, and sufficient rather than a compromise.** A bot
account alongside a human account on github.com is a real configuration this shape cannot express —
the host is the key, so there is one credential per host. That is not a trade-off for slice 1, it is
adequate: **a read needs *a* token that can reach the host, not a particular account.** `GET
/repos/{owner}/{repo}/pulls` succeeds with any token holding access, so host keying is exactly the
right granularity for a read-only collector.

Identity becomes a real requirement for two things, and both are write-adjacent: **which account
performs a write**, and **self-authorship detection** (`ArtifactAuthoredByUnjira` — F28's second gap,
where a tracker-collector without a self-identity narrates unjira's own output back at itself). So the
constraint is better stated as: reads and writes want different things from a credential, and this
shape models reads.

When that moment comes, the mechanism should be **discovery, not a naming convention.** `GET /user` is
GitHub's `Myself()`, and `collector/jira` already establishes the pattern — `SelfAccountID`, "from one
`Myself()` call per pass" (`collector/jira/events.go:71`), one call per connection per pass. The
authenticated account is a *fact about the token*, so deriving it cannot drift; a config label can.
Two shapes are therefore **rejected in advance**, because both are natural guesses:

- **`"user@github.com"` as the credential key.** Reads well, and matches how git remotes and SSH
  configs look — but it duplicates what the token already knows. A key reading `bot@github.com` over a
  token that is actually the human's is a stale label nothing detects, the same defect class as the
  `"oss"` key this section removed. It also still does not say which identity a given repo should use,
  so it needs a second mechanism pointing at it.
- **User-specified opaque keys** (`"bot"`, `"personal"`). Honest about being arbitrary, but they move
  the question rather than answering it: something in config still has to reference them, which is the
  invented-key problem one level up.

The likely shape instead: a write destination names its identity (`"write_identity": "bot"`, omitted
meaning "the read token"), `UNJIRA_GITHUB_CREDENTIALS` grows a nested `identities` map only for hosts
that need one, and the composite `bot@github.com` form appears where it is genuinely load-bearing — as
the **discovered** identity stamped on events for per-host self-authorship comparison, derived from
`/user` rather than declared in config.

Not designed here, because **selection is a write-path question and the write path is F29's
territory** (per-tracker write authority, deny-by-default). Designing identity selection before
knowing what a write destination looks like means guessing at the thing that does the selecting.

A missing or empty credential fails loudly at `Collect`, naming the env var and the **host** it was
looked up under — `credentials.Set.For` already distinguishes "missing" from "empty" for precisely this
reason.

**Registration** (`cmd/unjira/main.go:60`'s `registry` map):

```go
var registry = map[string]func() pipeline.Collector{
    "claude_code": func() pipeline.Collector { return claudecode.New() },
    backendJira:   func() pipeline.Collector { return collectorjira.New() },
    "github":      func() pipeline.Collector { return collectorgithub.New() },
}
```

One line, matching the Strategy+registry pattern `docs/architecture.md` §5 already credits this
codebase with ("Adding a collector is one map entry. Verified: nothing downstream switches on
collector name.") — this design changes nothing about that claim; it is exercised, not revisited.

## 9. Testing strategy

Offline (`go test ./...`, what CI runs), following the Jira collector spec's own established
pattern, substituting a fake process runner for `httptest`:

- **Collector unit tests** — a fake `github.Client` (canned `[]PullRequest` per call, no subprocess),
  covering: `:opened`/`:merged`/`:closed` event shapes and `ExternalID`s; `ArtifactGitBranch` and
  `ArtifactSCMKeys` populated correctly from head-branch and title/body respectively; **never**
  `events.SetTrackerRecord` called (a regression test worth having given how load-bearing §"Why
  GitHub is not the tracker" is — a future edit that starts marking these as tracker records would
  silently reintroduce the exact category error F18's own spec fixed for Jira, and this is exactly
  the shape `TestEveryEmittedEventIsMarkedATrackerRecord` polices for the Jira collector in reverse:
  a `TestNoGitHubEventIsEverMarkedATrackerRecord` belongs in this collector's own package for the
  identical reason that one lives in `collector/jira`'s).
- **Identity/idempotency** — real temp-file SQLite (`store.Open`), covering: re-collecting an
  already-`:opened` PR is a no-op (dedupe); a PR that transitions from open to merged between two
  collection passes produces exactly one new event (`:merged`), not a duplicate `:opened`; a
  reopened PR's handling matches whatever this spec's own open question below resolves to (currently
  unresolved — the test that would pin the answer cannot be written until it is).
- **Watermark** — real store, covering: cursor advances only after a repo's fetch completes without
  error; a repo new to config (no existing cursor) backfills from `backfill_days`; a transient `gh`
  failure on one repo does not block another configured repo's cursor from advancing (same
  per-scope failure granularity the Jira collector's own spec establishes for per-query failures).
- **Fanout integration point** (slice 2, when built) — `groupByFanoutFamily` over a synthetic
  12-PR fan-out family produces the expected `WithInstruction` text; a non-fanout PR set produces no
  instruction.
- **`RunCollect` integration** — collector registered, run end-to-end against the fake client plus a
  real store, asserting dedup on a second identical pass — matching the Jira collector spec's own
  integration test shape exactly.

Every regression test gets the break-it drill this codebase's own conventions require
(`docs/superpowers/specs/2026-08-21-jira-collector-design.md`'s own testing section: "this project
has caught four tests this month that looked like coverage and were not").

**Live tier.** `internal/live` (the `live` build tag, `UNJIRA_LIVE=1`) gets a new test file running
real GitHub against a real, low-stakes repo — `jcogilvie/unjira` itself is the natural
choice, since it is already this project's own remote and the operator (verified in this
environment: `gh auth status` shows an authenticated `jcogilvie` session) already has push access for
a throwaway branch+PR the live test can open and close as part of its own setup/teardown, mirroring
`internal/live/jira_test.go`'s `dev seed`-then-assert shape.

## Open questions

Left genuinely open, per `CLAUDE.md`'s "the record of what was uncertain is worth more than a doc
that looks prescient" — none resolved by picking arbitrarily:

- **Whether the flat credential form is also accepted.** `{"token": "..."}` as sugar for
  `{"github.com": {"token": "..."}}` costs one decoder branch and removes a papercut for the
  overwhelmingly common single-host case. Against: two accepted shapes for one env var makes the
  error message harder to write, and "no credential for host X" is the message that has to stay clear.
- **Reopened PRs.** GitHub permits reopening a closed PR. This design's two-event lifecycle
  (`:opened`, `:merged`/`:closed`) has no slot for "reopened, and possibly re-closed again later" —
  a second closure would collide with the first `:closed` `ExternalID` and dedupe away silently
  (`INSERT OR IGNORE`), which is exactly the silent-data-loss shape this codebase's own conventions
  warn hardest against. Genuinely rare in practice, but "rare" is not "impossible," and this spec
  does not invent a numbering scheme (`:closed:2`? keyed on GitHub's own state-transition timestamp
  instead of a fixed suffix?) without evidence for which shape is worth the complexity.
- **The `claude_code` double-observation residual gap (§7).** Whether `Cluster` reliably merges a
  transcript-observed PR-creation event with this collector's own `:opened` event for the same PR is
  an empirical question this spec cannot answer without both collectors running against real data.
  If it merges reliably, the gap may not need a fix. If it does not, the clean fix (a shared
  cross-reference artifact via `refs.ParsePRRefs` on both collectors' text) requires a
  `collector/claudecode` change this spec does not own. Left for measurement, not resolved here.
- **`refs`' consumer (§6).** This design extracts and stores GitHub cross-references but names no
  consumer for them — matching creates. Whether that consumer is a same-repo PR-relationship graph, a
  richer `narrative_issues`-adjacent table, or something else entirely is undesigned. Recorded rather
  than guessed at.
- **`backfill_days` default of 30.** Proposed by analogy to `collector/claudecode`'s own backfill
  option, not measured against any real repo's PR lifetime distribution. A repo with long-lived PRs
  (weeks between open and merge) may want a materially larger value; this spec does not have the data
  to tune it and says so rather than asserting a number that looks considered.
- **Whether reviews/CI belong in slice 2 or a slice 3.** This spec defers both past slice 1 with a
  stated reason each (§2), but does not commit to an order between them — that depends on which
  gets requested first, or which measured gap (from running slice 1 against real data) turns out to
  matter more.

## Non-goals (restated, for the same reason the Jira collector spec restates its own)

- No write path. `clients/github` has no `TaskWriter`-shaped methods; nothing in `gate.Applier`
  changes.
- No narrative→GitHub-object matching (only narrative→Jira-issue-key, feeding the existing pipeline
  better candidates).
- No GitHub-Issues-as-tracker backend (a materially different, unbuilt feature — see the scoping
  caveat in §"Which side of the diff GitHub sits on").
- No `fanout`/`refs` implementation in slice 1 — designed (§6), deferred, with the deferral reasoned
  rather than assumed.
