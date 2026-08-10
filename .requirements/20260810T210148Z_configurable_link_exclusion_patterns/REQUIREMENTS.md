# Configurable link-exclusion patterns (replacing hardcoded sentinel keys)

## As Is

`internal/events/events.go` hardcodes a workplace-specific convention: any ticket-key-shaped
match (`TicketKeyRegexp`, `[A-Z][A-Z0-9]{1,9}-\d+`) whose numeric suffix is `0` or `1` is treated
as a "sentinel" — a fake key devs substitute to placate a commit-message linter that requires a
ticket reference, when no real ticket applies. `IsSentinelKey(key string) bool` checks a key
against the hardcoded set `{"0", "1"}`.

The only consumer is `internal/pipeline/digest.go`: when rendering the daily digest, it builds
the "linked" key list for an event by filtering `event.Artifacts["ticket_keys"]` through
`!IsSentinelKey(key)`. A key that is a sentinel is *dropped* from consideration — the event lands
in the "Untracked work" bucket exactly as if no ticket-shaped text had matched at all. Nothing
records that a sentinel was seen; the distinction between "nothing matched" and "something
matched but was deliberately not a real ticket" is lost.

Two problems:

1. **Not configurable.** `{"0", "1"}` is one company's specific convention (any prefix, `-0` or
   `-1` suffix). Another deployment might use a different fake number, a literal non-numeric
   placeholder (`NOTICKET-1`), or no such convention at all, in which case `PROJECT-1` is just
   that project's real first issue — the existing code comment on `sentinelIssueNumbers`
   acknowledges this ambiguity already. Baking one org's shorthand into the OSS default makes it
   wrong-by-default for everyone else.
2. **Categorical drop discards a real signal.** A deliberately-placed non-ticket key indicates the
   author *knew* there was no ticket at commit time, for one of several possible reasons: an
   impedance mismatch with the commit-ties-to-ticket policy (e.g. ongoing production KTLO work
   that structurally doesn't fit a per-commit ticket), emergent work that should be retroactively
   ticketed, or someone just clearing the checker to get unblocked. Distinguishing those
   dispositions is phase-1+ judgment (clustering by author/recency/frequency), but phase-0 must
   preserve the fact that a match was deliberately excluded, or phase 1 has nothing to reason
   over. Silently dropping it violates the "never silently drop data" invariant in `CLAUDE.md`.

## To Be

- The hardcoded `{"0", "1"}` convention is gone. In its place, `unjira.config.json` accepts a
  top-level `exclude_from_linking` array of regex pattern strings, defaulting to empty (no
  assumption about anyone's convention).
- `events.ExtractTicketKeys` is unchanged — still every ticket-shaped match, no config awareness.
  Collectors stay dumb; matching behavior at extraction time does not change.
- A new pure function in `internal/events` compiles the configured patterns once and classifies a
  key list into "kept" and "excluded" subsets, matching against each key's numeric-suffix-stripped
  or full form as configured (regex match against the full key string — simplest, most flexible,
  matches the `-0$`/`-1$` example directly).
- Classification happens centrally in `internal/pipeline.RunCollect` (which already has `cfg` in
  scope), once per event, immediately before persisting it — not in each collector (collectors
  stay dumb) and not only at digest-render time (the fact must be persisted so phase 1 can use it
  later, not recomputed from a live config value that may have changed).
- Every event's `ticket_keys` artifact is left exactly as-is — nothing is removed from it. A new
  `excluded_ticket_keys` artifact is added (only when non-empty) holding the subset of
  `ticket_keys` that matched an exclusion pattern.
- `internal/pipeline.RenderDigest` still buckets an event as "linked" only if it has a non-excluded
  key, but an event with only excluded keys gets an inline annotation in the "Untracked work"
  section naming which key(s) were excluded, instead of looking identical to a plain no-match
  event.
- Bad regex in `exclude_from_linking` is a config-load-time error (loud failure), not a silent
  no-op or a panic at classification time.
- README's "Sentinel keys" bullet is rewritten to describe `exclude_from_linking` instead.

## Requirements

1. **R1 — Config schema.** `Config` gains `ExcludeFromLinking []string` (JSON key
   `exclude_from_linking`), defaulting to an empty/nil slice when absent from the config file or
   from `Default()`.
2. **R2 — Pattern compilation.** `events.CompileLinkExclusionPatterns(patterns []string)
   ([]*regexp.Regexp, error)` compiles every pattern, returning a wrapped error naming the bad
   pattern on the first invalid one (fail loudly, not silently).
3. **R3 — Classification.** `events.PartitionExcludedKeys(keys []string, compiled
   []*regexp.Regexp) (kept, excluded []string)` splits `keys` in original order: a key is
   `excluded` iff it matches at least one compiled pattern (via `Regexp.MatchString` against the
   full key), otherwise `kept`. Order-preserving, no dedup beyond what `ExtractTicketKeys` already
   did.
4. **R4 — Persisted exclusion artifact.** `pipeline.RunCollect` classifies each event's
   `ticket_keys` artifact (if present and non-empty) using the config's compiled patterns before
   inserting; when `excluded` is non-empty, sets `event.Artifacts["excluded_ticket_keys"]` to that
   list. `ticket_keys` itself is never mutated. Events with no `ticket_keys` artifact, or an empty
   one, are unaffected (no `excluded_ticket_keys` key is added).
5. **R5 — Digest bucketing and annotation.** `RenderDigest` buckets an event as "linked" if
   `ticket_keys` minus `excluded_ticket_keys` is non-empty; otherwise "untracked." An "untracked"
   event whose `excluded_ticket_keys` is non-empty gets its digest line annotated with
   `[excluded from linking: KEY1, KEY2]`. An event with a real link takes precedence over any
   excluded keys it also happens to carry (i.e. `excluded_ticket_keys` never causes a linked event
   to be reclassified as untracked).
6. **R6 — `IsSentinelKey`/`sentinelIssueNumbers` removed.** No hardcoded sentinel-number
   convention remains in `internal/events`. All prior references (implementation and tests) are
   deleted, not deprecated-in-place.
7. **R7 — Config load surfaces bad patterns loudly.** `config.Load` (or a caller wiring it into
   `cmd/unjira`) surfaces a compile error for an invalid `exclude_from_linking` pattern rather than
   silently ignoring it or crashing without context.
8. **R8 — Documentation.** `README.md`'s sentinel-key bullet and `config/unjira.example.json` are
   updated to describe/demonstrate `exclude_from_linking`.

## Acceptance Criteria

- AC1.1 `config.Load` on a config file with `"exclude_from_linking": ["-0$", "-1$"]` yields
  `cfg.ExcludeFromLinking == []string{"-0$", "-1$"}`.
- AC1.2 `config.Load` on a config file without the key, and `config.Default()`, both yield a
  nil/empty `ExcludeFromLinking`.
- AC2.1 `CompileLinkExclusionPatterns([]string{"-0$", "-1$"})` returns two compiled patterns, no
  error.
- AC2.2 `CompileLinkExclusionPatterns([]string{"("})` (invalid regex) returns a non-nil error
  whose message contains the offending pattern `"("`.
- AC2.3 `CompileLinkExclusionPatterns(nil)` returns `(nil, nil)` — empty config is a no-op, not an
  error.
- AC3.1 `PartitionExcludedKeys([]string{"PROJ-42", "PROJ-0"}, compiled(["-0$"]))` returns
  `kept=["PROJ-42"], excluded=["PROJ-0"]`.
- AC3.2 `PartitionExcludedKeys([]string{"PROJ-42"}, compiled(["-0$"]))` returns
  `kept=["PROJ-42"], excluded=nil` (no match, order preserved).
- AC3.3 `PartitionExcludedKeys(keys, nil)` (no compiled patterns) returns `kept=keys,
  excluded=nil` unchanged.
- AC4.1 Given collector-emitted `ticket_keys=["PROJ-42","PROJ-0"]` and config
  `exclude_from_linking=["-0$"]`, the event persisted by `RunCollect` has
  `ticket_keys=["PROJ-42","PROJ-0"]` (unchanged) and `excluded_ticket_keys=["PROJ-0"]`.
- AC4.2 Given `ticket_keys=["PROJ-42"]` and the same config, the persisted event has no
  `excluded_ticket_keys` key at all.
- AC4.3 Given no `exclude_from_linking` configured, no event ever gets an `excluded_ticket_keys`
  artifact, regardless of its `ticket_keys` content.
- AC5.1 An event with `ticket_keys=["PROJ-42"]`, no exclusions, appears under "Linked to tickets"
  with `PROJ-42` shown, no annotation.
- AC5.2 An event with `ticket_keys=["PROJ-0"]` and `excluded_ticket_keys=["PROJ-0"]` appears under
  "Untracked work" with an inline `[excluded from linking: PROJ-0]` annotation.
- AC5.3 An event with `ticket_keys=["PROJ-42","PROJ-0"]` and `excluded_ticket_keys=["PROJ-0"]`
  (a real link plus an excluded one on the same event) appears under "Linked to tickets" showing
  `PROJ-42`, not reclassified as untracked because of the excluded key.
- AC5.4 An event with no `ticket_keys` artifact at all still appears under "Untracked work" with
  no exclusion annotation (the "nothing matched" case stays distinguishable from "something
  matched but was excluded").
- AC6.1 `grep -ri sentinel internal/` (after implementation) finds no remaining `IsSentinelKey` /
  `sentinelIssueNumbers` symbols or tests.
- AC7.1 Given `unjira.config.json` with an invalid pattern in `exclude_from_linking`,
  `cmd/unjira`'s startup path returns a clear, wrapped error naming the pattern, not a panic or a
  silent skip.
- AC8.1 README's sentinel-key bullet is replaced with one describing `exclude_from_linking`.
  `config/unjira.example.json` includes a commented-out or empty example of the key.

## Testing Plan

Table-driven `testify`-based unit tests per `docs/go-conventions.md`, offline only (no live Jira
needed — this is pure data-shape logic).

- `internal/events`: new `events_test.go` cases for `CompileLinkExclusionPatterns` (AC2.1–AC2.3)
  and `PartitionExcludedKeys` (AC3.1–AC3.3). Delete `TestIsSentinelKey_PrefixAgnostic` entirely
  (AC6.1).
- `internal/config`: extend `config_test.go` with cases for AC1.1/AC1.2 (parses the new key;
  absent key defaults to empty).
- `internal/pipeline`: extend `collect_test.go` for AC4.1–AC4.3 (`RunCollect` needs the compiled
  patterns threaded in — see Implementation Plan for the signature change) and rewrite the
  existing sentinel-flavored case in `digest_test.go` to cover AC5.1–AC5.4.
- Full-repo regression: `go test ./...` green, `golangci-lint run ./...` (or `earthly +reviewable`)
  0 issues, after every step.

## Implementation Plan

Smallest steps, test-first each time, run `go test ./internal/events/... ` (etc.) after each red/
green pair before moving on. Numbering matches Requirements above.

1. **R2/R3 — `events` package: add compile + partition, remove sentinel (red → green).**
   - Write failing tests for `CompileLinkExclusionPatterns` (AC2.1–AC2.3) and
     `PartitionExcludedKeys` (AC3.1–AC3.3) in `internal/events/events_test.go`.
   - Delete `TestIsSentinelKey_PrefixAgnostic`.
   - Implement `CompileLinkExclusionPatterns` and `PartitionExcludedKeys` in `events.go`; delete
     `IsSentinelKey` and `sentinelIssueNumbers`.
   - Verify: `go test ./internal/events/...` green; `grep -ri sentinel internal/events` empty.

2. **R1 — `config` package: add `ExcludeFromLinking` (red → green).**
   - Write failing tests in `config_test.go` for AC1.1/AC1.2.
   - Add `ExcludeFromLinking []string \`json:"exclude_from_linking"\`` to `Config`.
   - Verify: `go test ./internal/config/...` green.

3. **R7 — surface a compile error at startup.**
   - Decide the seam: `config.Config` gains a method (e.g. `CompiledLinkExclusions() ([]*regexp.Regexp,
     error)`) that wraps `events.CompileLinkExclusionPatterns(c.ExcludeFromLinking)` — keeps
     `internal/config` free of an `internal/events` import cycle risk by checking direction first;
     if config already imports nothing from events, this is fine, otherwise put the compile call
     directly in `cmd/unjira`/`pipeline` instead.
   - Add a test asserting a bad pattern surfaces a wrapped error mentioning the pattern.
   - Wire the call into `cmd/unjira`'s `run()` (or into `pipeline.RunCollect`'s entry, whichever
     avoids doing the compile once per event) so a bad config fails fast, before any collector
     runs.

4. **R4 — thread exclusion into `RunCollect` (red → green).**
   - Decide `RunCollect`'s signature change: accept the pre-compiled `[]*regexp.Regexp` (compiled
     once by the caller per step 3) alongside `cfg`, `s`, `registry` — avoids recompiling regexes
     per event and keeps `RunCollect` itself simple to test with a fixed compiled list.
   - Write failing tests in `collect_test.go` for AC4.1–AC4.3 using `fakeCollector`.
   - Implement: after a collector's `visit` callback fires and before `s.InsertEvent`, if
     `event.Artifacts["ticket_keys"]` is a non-empty `[]any` of strings, run
     `events.PartitionExcludedKeys` and set `event.Artifacts["excluded_ticket_keys"]` only when
     `excluded` is non-empty.
   - Update `cmd/unjira/main.go`'s call site to pass the compiled patterns from step 3.
   - Verify: `go test ./internal/pipeline/...` green.

5. **R5 — digest bucketing + annotation (red → green).**
   - Rewrite the existing sentinel case in `digest_test.go` into AC5.1–AC5.4 (four cases: linked
     only, excluded only, both together, no-artifact-at-all).
   - Implement in `digest.go`: read both `ticket_keys` and `excluded_ticket_keys` artifacts; bucket
     by "any key in `ticket_keys` not present in `excluded_ticket_keys`"; when bucketing as
     untracked, append the `[excluded from linking: ...]` annotation if `excluded_ticket_keys` is
     non-empty.
   - Verify: `go test ./internal/pipeline/...` green.

6. **R8 — docs.**
   - Update `README.md`'s sentinel-key bullet.
   - Update `config/unjira.example.json` with an `exclude_from_linking` example (commented via a
     `// ...`-style note isn't valid JSON, so either an empty-array example plus prose in README,
     or a non-empty illustrative example — decide when writing, favoring a working, valid example
     over a comment).
   - Update `.serena/memories/go_port_toolchain_and_decisions.md`'s "Deferred" section to mark
     Task #10 resolved, and `CLAUDE.md`/`docs/design-notes.md` if either still says "sentinel."

7. **Final verification.**
   - `go build ./...`, `go vet ./...`, `go test ./...`.
   - `earthly +reviewable` (lint + test) — 0 issues.
   - `grep -ri sentinel .` outside `.requirements/`, `.serena/memories/` (historical), and this new
     requirements doc — should find nothing live in `internal/`, `README.md`, or
     `config/unjira.example.json`.
