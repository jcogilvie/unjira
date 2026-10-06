# Tracker model: connections, trackers, routing and destinations

Status: approved, landing slice by slice. See "Status by slice" at the end.

Resolves the tracker-model chain in `docs/architecture-findings.md`:

- **F7:** one struct carries four concerns.
- **F28:** a collector cannot ask whether its system is a tracker here.
- **F29:** nothing expresses which tracker a narrative's work belongs to.
- **F8:** `TrackerResolver` lives in the wrong package.
- **F3:** the correlator imports `clients/jira` for one error check.
- **F1:** partly; `refs` gets its consumer.

## Goal

unjira reconciles against more than one tracker. The motivating deployment does employer work tracked in
Jira and open-source work tracked in upstream GitHub issues, in the same transcript corpus, often on the
same day. Two things go wrong today:

- Upstream work has no Jira key, so it looks untracked, and the reconciler may propose a Jira ticket for it.
- No config surface can say which tracker work belongs to, or that a tracker may be read but never written.

## Decisions

**GitHub is a read-only tracker.**
- unjira reads upstream issues to match and verify work, and never writes to a public tracker.
- The GitHub backend implements `tasktracker.TaskReader` only. A public write is therefore structurally
  impossible, not merely gated.
- A GitHub writer is out of scope, and would need its own spec.

**No tracker is special by name.**
- Trackers form a named list, and work may be ticketed in whichever allowed trackers fit.
- The schema permits several destinations for one narrative.
- Per-destination prose (one narrative ticketed in two trackers with different text) is a later slice.
- Every rule is stated in terms of configured trackers, never "Jira".

**Mirroring is a per-tracker rule, and the default is none.**
- Work tracked in, or done in, tracker *T*'s scope may also be ticketed in the trackers named by
  *T*`.mirror_to`.
- Unlisted means nowhere else, which is deny by default.
- The policy belongs to the operator, and differs by org. unjira encodes the mechanism, not a policy.

**Untracked work in an external scope follows the same rule.**
- A crossplane PR with no upstream issue gets no ticket in another tracker unless crossplane's tracker
  says `mirror_to` it.

**Config is YAML, with JSON still accepted.**
- `unjira.config.yaml` / `.yml`, parsed with `sigs.k8s.io/yaml` in strict mode, so unknown keys are
  errors. `.json` keeps working, because that library reads JSON too.
- Short scope keys must be quoted. YAML reads an unquoted `NO`, `ON` or `YES` as a boolean.
  - Every scope is a typed string field, so this fails at load, not silently.
  - The load error says to quote the value.
  - The example config quotes short keys.

## Config model

```yaml
connections:            # WHO and WHERE: system kind, endpoint. Nothing about scope.
  - name: work-jira
    kind: jira
    endpoint: https://org.atlassian.net
  - name: github
    kind: github
    endpoint: https://api.github.com
  - name: local         # the existing local backend, as a connection kind
    kind: local

trackers:               # WHAT: scopes and write authority, per tracker
  - name: work
    connection: work-jira
    scopes: ["PAAS", "DEVSBX"]      # read scope: which issues route here
    writable_scopes: ["DEVSBX"]     # write authority; absent or empty means nothing is writable
    default_scope: "DEVSBX"         # where this tracker's creates land; required if writable and a destination
    queries:                        # what the Jira collector reads (was jira[].queries)
      - name: mine
        jql: "assignee = currentUser()"
    max_issues_per_query: 200
  - name: upstream
    connection: github
    scopes: ["crossplane/crossplane", "crossplane-contrib/*"]
    writable_scopes: []             # read-only; no GitHub writer exists
    mirror_to: []                   # work here is ticketed nowhere else

default_ticket_in: ["work"]         # untracked work outside every scope; empty means propose nothing
```

**Credentials** stay in the environment, under the existing variables, so no credential contract changes:
- **`jira`** connections read `UNJIRA_JIRA_CREDENTIALS`, keyed by connection name, as today.
- **`github`** connections read `UNJIRA_GITHUB_CREDENTIALS`, keyed by the endpoint's host (`github.com`),
  as the GitHub collector does today.
- Collectors name a connection (the GitHub collector's `connection: github`). A collector and a tracker on
  one connection share one credential.

**Validation at startup.** Each check is a loud error naming the field:
- every `connection`, `mirror_to` and `default_ticket_in` name resolves;
- `writable_scopes` ⊆ `scopes` (today's `ValidateWriteScope`, generalized);
- a `writable_scopes` entry on a connection kind with no writer is refused, never silently ignored;
- **scopes do not overlap across trackers**, so every issue routes to exactly one tracker;
- a writable tracker's backend can report unjira's own identity (see "Classification and self-authorship").

**Old keys are refused.** Config is user-local, and the store has no migrations. So the top-level `jira`
and `tracker` keys are an error naming the new shape, not silently mapped. The example config is rewritten
as commented YAML.

`tracker.default_project` becomes `default_ticket_in` plus each tracker's own default scope for creates
(`default_scope`, required when the tracker is writable and named in a destination list). The global
`workflow` (named status transitions) stays global in slice 1. Make it per-tracker when a second writable
backend exists.

## Issue identity and routing

- **An issue key stays in its tracker's native syntax:** Jira `PAAS-123`, GitHub
  `crossplane/crossplane#6812`. The two cannot be confused, so stored keys (`narrative_issues`,
  `actions`) need no schema change.
- **Routing is a pure function of key and config.**
  - Parse the key's scope by syntax. Jira: the project prefix. GitHub: `owner/repo`, case-folded, the way
    `events.PullRequestRef` folds case.
  - Then find the one tracker whose `scopes` contain it. Glob entries (`crossplane-contrib/*`) match a
    path segment.
  - Routing is not stored, so config stays the single source of truth.
  - A stored key that no longer routes after a config change is reported at startup, never dropped.
- **One resolver for every path (F8).** `tasktracker.Resolve(issueKey)` returns the tracker's reader, plus
  its writer when the scope is writable. Matching, the reconciler and the applier all use it.
  `correlator.TrackerResolver` is removed.
- **Each backend classifies its own errors (F3).** Transport errors are recognized through a
  `tasktracker` interface method or error type, so `internal/correlator` stops importing `clients/jira`.
- **Matching learns every configured kind.**
  - `gatherCandidates` extracts Jira keys as today.
  - It also extracts qualified GitHub references: `owner/repo#N`, issue URLs, and "Fixes owner/repo#N" in
    a PR body.
  - Each candidate is verified against its own tracker's reader.
  - Bare `#N` does not count yet: resolving it needs repo context, which is what `internal/correlator/refs`
    is for (F1, a later slice).
- **A GitHub PR is not an issue.** It stays work evidence. The GitHub reader resolves issues only, and a
  PR that `Fixes` an upstream issue is how work becomes "tracked upstream".

## Classification and self-authorship (F28)

- **Classification asks the config.** `pipeline.CollectContext` gains `TrackerFor(kind, scope)`. It
  reports whether an artifact's scope is a configured tracker's scope, and returns that tracker.
  - The Jira collector uses it as it uses its connection today.
  - A future GitHub Issues collector marks an issue event as a tracker record only when its repo is a
    tracker scope here.
- **Unchanged:** PRs stay work evidence in every configuration, and the exit filters
  (`AnyWorkEvidence`, `suppressTrackerEcho`, `dropSelfAuthored`) stay as they are.
- **Self-authorship is a requirement on writers.**
  - A tracker with any `writable_scopes` must sit on a backend that reports unjira's own identity (Jira's
    `Myself()` today). Otherwise startup refuses it, because unjira would narrate its own writes back as
    new work.
  - A read-only tracker has no writes to echo.
- **Collectors read through connections.** The Jira collector's queries are scoped to the scopes of the
  trackers on its connection, generalizing `EffectiveJQL`. A collector with no tracker on its connection
  (the GitHub PR collector) produces evidence only.

## Destinations

Before any drafting call, the pipeline computes each narrative's **allowed destinations** deterministically:

| The narrative's work… | Allowed destinations |
|---|---|
| is linked to issue *I* in tracker *T* | comments and transitions on *I*, gated by *T*'s `writable_scopes` as today; plus creates in *T*`.mirror_to` |
| is untracked, and happened in a scope of tracker *T* | *T* if writable, plus *T*`.mirror_to` |
| is untracked, with no known scope | `default_ticket_in` |
| has an empty set | nothing is proposed; recorded as a suppression with its reason, like the other filters |

- The model proposes onto whichever allowed destinations fit.
- A proposal naming a destination outside the set is rejected deterministically.
- The three existing write gates still apply on top.

**Work location**, meaning where untracked work happened, uses two kinds of evidence in strict order.

1. **The transcript's own SCM actions:**
   - the remote named in `git push` output (`To github.com:owner/repo.git`);
   - PR-creation URLs;
   - `gh … -R/--repo`;
   - GitHub MCP write calls.

   Where this exists it wins, because it names where work *went*. Measured over 1,296 local segments, it
   disagreed with the cwd's repo in 5 of 123 root and 33 of 104 subagent segments where both existed. One
   example: a `helm-charts` worktree pushing to `cse-gitops`.

2. **Fallback: the cwd's git remotes, read with go-git** (`github.com/go-git/go-git/v5`).
   - **Opened with** `PlainOpenWithOptions(cwd, {DetectDotGit: true, EnableDotGitCommonDir: true})`.
     Verified: a linked worktree, whose `.git` is a file, resolves to its main repository's remotes, a
     subdirectory resolves to its repository, and a non-repo returns an error.
   - **No `git` binary,** no network, no `$PATH` dependency.
   - **Sorted,** since go-git's remote order is not stable.
   - **Read at collect time, once per distinct cwd per pass.** 8% of root and 13% of subagent cwds no
     longer exist later, mostly deleted worktrees.
   - **Stored as an artifact:** the normalized `owner/repo` of every remote. If the cwd is gone or is not
     a repository, the artifact records why, as the existing `*_omitted` artifacts do.

   This fallback covers the 61 of 255 root segments (24%) that have no push or PR evidence. That is
   investigation and in-progress work, exactly what would otherwise fall to `default_ticket_in`.

**Scope is decided at reconcile time,** from config, so a config change applies without re-collecting:
- Every remote is matched against tracker scopes, not just `origin`.
- Contributor forks match no scope, and drop out. `crossplane-diff` has six fork remotes.
- One matching tracker is the work's scope.
- Remotes matching **different** trackers are ambiguous: reported, and treated as no known scope, never
  guessed.

**Rejected: tracking branches.** Of 86 multi-remote root segments, 36 had an upstream set; for subagents,
6 of 379. That is too rare to justify the complexity.

**Invariant change.** CLAUDE.md's "collectors are dumb and deterministic" gains a named, bounded
exception: the claude_code collector reads a working tree's local git config through go-git. This is
local, deterministic, judgment-free extraction, with no network.

## Slices

Each slice ships on its own and keeps the write path safe throughout.

1. **Config model.** YAML/JSON loading, `connections` and `trackers`, validation, refusal of the old keys,
   and the example rewritten. Behaviour is unchanged: one Jira tracker, as today.
2. **Resolver.** `tasktracker.Resolve(issueKey)`, typed key parsing per kind, and per-backend error
   classification (F8, F3). Matching, the reconciler and the applier move onto it.
3. **Classification and self-authorship (F28).** `CollectContext.TrackerFor`, and the startup check that a
   writable tracker has a self-identity.
4. **GitHub read-only tracker.** A `TaskReader` over GitHub Issues on the shared connection, and qualified
   `owner/repo#N` candidates in matching.
5. **Destinations.** Work-location evidence (transcript first, then go-git remotes), the allowed-destination
   set, `mirror_to` and `default_ticket_in`, and the empty set recorded as a suppression.

Slices 1–3 are refactors behind unchanged behaviour. Slices 4–5 change behaviour.

## Out of scope, recorded as follow-ups

- Per-destination prose: one narrative ticketed in two trackers with different text.
- A GitHub writer, and any public write.
- Bare `#N` references, with repo context (F1's `refs`).
- A GitHub Issues collector.
- Per-tracker `workflow` (named status transitions).

## Testing

- **Every slice is TDD**, with break-it drills on each guard. Each drill is a one-line mutation that must
  compile and fail a named test.
- **Slice 1:**
  - an old-shape config refusal test per removed key;
  - the YAML `NO`/`ON`/`YES` load error;
  - the scope-overlap refusal;
  - a writable scope on a writer-less kind refused.
- **Slice 2:**
  - routing tests per key syntax, including a glob scope and case folding;
  - the stored-key-no-longer-routes report.
- **Slice 5:**
  - transcript evidence beats a disagreeing cwd;
  - a linked worktree resolves to its main repository's remotes, against a real temporary repository
    built in the test;
  - fork remotes drop out by scope;
  - a two-tracker match is ambiguous;
  - an empty destination set is recorded as a suppression.
- **Measured before and after slice 5,** on a fresh snapshot of local transcripts: how many narratives
  change destination, and how many would-be creates for externally-scoped work disappear.

## Status by slice

### Slice 1, config model: landed 2026-10-06

`config.Connection` and `config.Tracker` (`internal/config/trackers.go`), `default_ticket_in`, strict
YAML/JSON loading, `ValidateTrackers` run by `Load`, refusal of the `jira` and `tracker` keys, and
`config/unjira.example.yaml` as commented YAML. Behaviour is unchanged for a config that converts one
`jira[]` entry into one connection plus one tracker of the same name.

Verified: `go vet ./...` clean, `go test ./...` all packages ok, `golangci-lint run --build-tags=live
./...` 0 issues. 24 break-it drills, each a one-line mutation failing a named test (see the slice's
commit message).

**Deviation: sigs.k8s.io/yaml does not fail on an unquoted `NO`.** The design assumed a typed string
field would make it fail at load. It does not: the library converts YAML to JSON guided by the target
type, and renders the YAML 1.1 boolean into the string field as `"false"` (`ON`/`YES` as `"true"`),
without an error. Measured with `scopes: [PAAS, NO, ON, YES, 123]`, which decoded to
`["PAAS", "false", "true", "true", "123"]`. The guard is therefore scope syntax: neither `"false"` nor
`"true"` is a project key or an `owner/repo`, so validation refuses it, and the message names the
quoting fix whenever the offending value is `true` or `false`.

Decisions the design left open:
- **Discovery.** With no `--config`, `Load` reads whichever of `unjira.config.yaml`, `.yml` and `.json`
  exists in the working directory. Two is an error, not a precedence order.
- **Validation runs in `Load`,** so every command refuses a bad tracker model, not only `watch`.
- **A destination must be ready.** A tracker named in `default_ticket_in` or `mirror_to` must have
  `writable_scopes` and a `default_scope`. A read-only destination is refused rather than ignored.
- **One `default_ticket_in` until slice 5.** The create path has one default; naming two is refused
  rather than silently using the first.
- **Scope syntax per kind.** Jira and local scopes are project keys (`^[A-Z][A-Z0-9]*$`), and share one
  key space, so a project cannot be claimed by a Jira and a local tracker at once. GitHub scopes are
  `owner/repo`, matched case-insensitively, with `*` allowed only as the whole repository segment. An
  owner glob would make every fork remote match a scope.
- **Cursor key unchanged.** The Jira collector's cursor stays keyed by connection and query name, so a
  converted config keeps its watermarks. Two trackers on one connection may not reuse a query name.
- **Endpoints.** Required for `jira` and `github` connections, refused on `local`.

### Slice 2, resolver: landed 2026-10-06

`tasktracker.ParseIssueKey`, `ScopeMatches`, `Resolver` (`Resolve`, `ResolveScope`, `Locate`) and
`Routed`, in `internal/tasktracker/key.go` and `resolve.go`. `correlator.TrackerResolver` and
`SingleTracker` are removed. Matching, the reconciler and `gate.Applier` all read and write through one
resolver built in `cmd/unjira` (`appContext.resolver`). `IsTransportError` moved to `tasktracker` and
asks the error: `*jira.Error` and `*tasktracker.UnroutedError` implement `IsTransport()`, the local
backend wraps `tasktracker.ErrNotFound`, so `internal/correlator` imports no backend (F3, F8 deleted).
`store.StoredIssueKeys` and `pipeline.UnroutedStoredKeys` report stranded keys at the start of `watch`
and `dev narrate`.

Verified: `go vet ./...` clean, `go test ./...` all packages ok, `golangci-lint run --build-tags=live
./...` 0 issues. 26 break-it drills, each failing a named test (see the slice's commit message).

Decisions the design left open:
- **`Routed` adapts the resolver to `TaskReader`/`TaskWriter`,** so `correlator.Match`,
  `reconciler.Reconcile` and `gate.Applier` keep their signatures and route per call. The write path
  then holds two independent refusals: config's writability check, which names the remedy, and a
  routed writer that has no writer to give for a read-only scope.
- **A read fallback, kept on purpose (F55).** A project-syntax key no tracker covers is still read
  through the tracker covering the first project scope, the tracker every candidate used to be
  verified against. Removing it would turn work on an unlisted project's ticket into a duplicate create
  proposal, a behaviour change this refactor slice should not make. A fallback read never writes, and
  every key that depends on it is in the startup report.
- **Reported keys come from both tables:** `narrative_issues` and `actions`. Routing uses `Locate`, so
  the report opens no backend and needs no credential.
- **Writability reads the key, not a "-" split.** `config.IssueWritability` parses by syntax, so
  `crossplane-contrib/x#1` is scope `crossplane-contrib/x`, not project `crossplane`. A malformed key
  is untracked in triage now, where a key without a "-" used to read as appliable.

### Slice 3, classification and self-authorship: landed 2026-10-06

`pipeline.CollectContext.TrackerFor(kind, scope)` returns the configured tracker whose scope covers an
artifact's scope on a connection of that kind. The Jira collector marks an issue's events as tracker
records only when `TrackerFor(jira, project)` answers, which query scoping makes every issue it reads,
so behaviour is unchanged. `tasktracker.SelfIdentifier` is the requirement on writers:
`jira.Tracker.SelfIdentity` returns the `Myself()` accountId, and the local backend reports `unjira`.
`Resolver.CheckWriters` refuses a writable route whose writer is not one, and runs at the start of
`watch` and `dev narrate` before the store is touched. F28 deleted.

Verified: `go vet ./...` clean, `go test ./...` all packages ok, `golangci-lint run --build-tags=live
./...` 0 issues. Break-it drills in the slice's commit message.

Decisions the design left open:
- **The identity check is by type, at the backend.** Config cannot know what a backend implements, so
  the check opens each writable route's writer (building a client, sending nothing) and asserts the
  interface. The resolver also refuses to hand out a writer without it, so `actions decide` and
  `triage`, which skip the startup check, still cannot write anonymously.
- **The local backend reports a constant.** Every local write is unjira's and nothing collects the
  local store back, so there is no echo to tag; reporting `unjira` lets a writable local tracker meet
  the same rule as any other writer rather than being an exception.
- **The Jira collector's scoping stays per tracker.** Each query is bounded by its own tracker's
  scopes (slice 1), which is a subset of "the trackers on its connection" and narrower than their
  union. An issue the search returns outside that scope is now work evidence rather than a tracker
  record.
- **A zero-value `IssueContext` still marks tracker records** (`ScopeUntracked` is the opt-out), so
  the pure event builders keep their behaviour for every caller that does not classify.

### Slice 4, GitHub read-only tracker: landed 2026-10-06

`clients/github.Reader` implements `tasktracker.TaskReader` over GitHub Issues and nothing else; the
client's one request method is now `get`, so it cannot send anything but GET. `cmd/unjira` opens it for
a `github` connection with the `UNJIRA_GITHUB_CREDENTIALS` entry for the endpoint's host
(`clients/github.HostOfBaseURL`, the inverse of `BaseURL`), the same entry the GitHub collector uses.
`events.ExtractGitHubIssueRefs` reads `owner/repo#N` and github.com issue URLs, and `gatherCandidates`
takes them from every event's summary.

Verified: `go vet ./...` clean, `go test ./...` all packages ok, `golangci-lint run --build-tags=live
./...` 0 issues. Read-only live run against the sandbox, `UNJIRA_LIVE=1 go test -tags=live -run
TestLiveGitHubReader -v ./internal/live/`: both tests PASS — every sandbox PR reads as not found
through the issues endpoint, and a missing issue is not a transport error.

Decisions the design left open:
- **Candidates come from the summary, not a collector artifact,** so every stored event yields them,
  including ones collected before the extraction existed (F21). On an event about a pull request the
  text is its authored title and body, so its references rank with SCM authoring commands; the pull
  request's own `owner/repo#N`, which heads its summary, is not a candidate.
- **Prose ordering.** The order between Jira keys and GitHub references in one text is not recorded,
  so a GitHub reference is the event's first mention only when it names no Jira key. Lower-cased
  references sort after uppercase project keys within a tier, so they cannot push Jira keys past the
  candidate cap.
- **Pull requests read as not found** (`tasktracker.ErrNotFound`, "is a pull request, not an issue"),
  open is todo and closed is done, and `AvailableTransitions` offers nothing: unjira cannot move a
  GitHub issue, so the reconciler proposes no transition there.
- **Error classification.** 404 and 410 are answers; 0, 429, 5xx and a 403 saying "rate limit" are
  transport.
- **Issue URLs on github.com only.** A GHES issue URL is not a candidate yet.
- **No collector `connection` option.** Credentials are keyed by host, so the collector and a tracker
  on one GitHub already share one credential without the collector naming the connection.
- **Interim gap, F56.** A narrative linked to an upstream issue can still draft a comment on it, which
  triage shows as unappliable and both write layers refuse. Slice 5's destination set closes it.

### Slice 5, destinations: landed 2026-10-06, measurement pending

Work location: `collector/claudecode` records `events.ArtifactWorkRepos` (a `git push` result's
remote, a PR a call created, the `-R/--repo` of an authoring `gh` command, a GitHub MCP write call's
owner/repo) and `events.ArtifactCwdRemotes` (every remote of the working copy, read with go-git
`PlainOpenWithOptions(cwd, {DetectDotGit: true, EnableDotGitCommonDir: true})`, sorted, once per
distinct directory per pass), or `events.ArtifactCwdRemotesOmitted` with the reason. The allowed set:
`config.UntrackedDestinations` and `config.IssueAcceptsWrites`, applied by
`reconciler.WithDestinations` before any drafting call. A create carries its destination's scope in
its payload, and `gate.Applier` writes it there. `default_ticket_in` may name several trackers. F29 and
F56 deleted; F57 added.

Verified: `go vet ./...` clean, `go test ./...` all packages ok, `golangci-lint run --build-tags=live
./...` 0 issues. The tests this spec lists: transcript evidence beats a disagreeing cwd
(`TestProposeCreates_TranscriptEvidenceBeatsADisagreeingCwd`); a linked worktree resolves to its main
repository's remotes, against a repository built in the test
(`TestCollect_LinkedWorktreeResolvesToItsMainRepositorysRemotes`); fork remotes drop out by scope
(`TestUntrackedDestinations_ForkRemotesDropOutByScope`, `TestProposeCreates_CwdRemotesAreTheFallback`);
a two-tracker match is ambiguous (`TestUntrackedDestinations_TwoTrackerMatchIsAmbiguous`); an empty set
is recorded as a suppression (`TestRunReconcile_AnEmptyDestinationSetIsRecordedAsASuppression`).

**Not yet measured.** The before/after measurement on a fresh snapshot of local transcripts — how many
narratives change destination, and how many would-be creates for externally scoped work disappear —
needs real transcripts and LLM credentials, and has not been run.

Decisions the design left open:
- **Repositories are host-qualified** (`<host>/<owner>/<repo>`), and a repository matches a scope only
  on the host its tracker's connection serves (`config.Connection.Host`, which now also keys the GitHub
  credential). The spec said `owner/repo`; the host keeps a GitLab or GHES remote from matching a
  github.com scope.
- **"T if writable" needs a `default_scope`** to be a destination: a create must land somewhere. In
  practice T is a GitHub tracker, so only `mirror_to` applies.
- **The model is asked to choose only among several destinations.** With one, it is the answer and the
  prompt is unchanged. With several, the user prompt lists them and asks for `"destinations"`; a name
  outside the set is rejected and recorded, and a create is proposed per chosen destination.
- **An open or applied create blocks its own scope only;** one recorded before destinations existed
  names no scope and blocks every destination.
- **`gate.Applier`'s duplicate backstop is per tracker.** A primary link in the destination's own
  tracker still refuses a create (F13); one in another tracker does not, and the second ticket is
  linked as `same_work`, since a narrative has one primary.
- **A link on a tracker with no writer is not drafted for** (`IssueAcceptsWrites`), which closed F56.
  A readable-but-unwritable Jira link is still drafted and refused at apply with its remedy, as the
  table's "gated by T's writable_scopes as today" says.
- **An empty set is recorded once,** then again only when new work arrives, so an unchanged narrative
  does not write a suppression row per pass.
- **Not built: creates in T.mirror_to for work LINKED to an issue in T** (F57). The create path selects
  only narratives with no link, and the schema's one-primary rule and the applier's backstop assumed
  one tracker per narrative; both want a design of their own.
- **Extraction limits:** `gh -R host/owner/repo` is recorded as github.com (the parser drops the
  host), and the GitHub MCP is assumed to be github.com. Only a `git push`'s own result is read for its
  remote.

