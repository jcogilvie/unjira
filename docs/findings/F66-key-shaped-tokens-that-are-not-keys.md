# F66 — key-shaped tokens that are not keys are candidates until verification

`events.TicketKeyRegexp` (`internal/events/events.go:45`) accepts any `[A-Z][A-Z0-9]{1,9}-\d+`, so line
ranges (`L242-276`), encodings (`UTF-8`), digests (`SHA-256`), model names (`GPT-4`) and labels such as
`AC1-3` are extracted. A key in a project no tracker scopes is still read, through the resolver's read
fallback (`tasktracker.Resolver.WithReadFallback`), so each such token costs a tracker read that returns
not-found and an `Unresolved` entry. On a real 30-day store, 11 narratives carried one among their capped
candidates. None survived verification, and none pushed a real key past the candidate cap.

**Not a pattern list.** The shapes are legal keys: `projectScopeRE` (`internal/config/trackers.go:58`)
accepts a project `L310`, and a site can have a project `HTTP`. A built-in or recommended regex would
silently hide those keys, and it would only fit `PROJ-N` syntax, not a GitHub tracker's `owner/repo#N`.
`exclude_from_linking` stays for its purpose, keys that are real-looking and in scope but should never
be linked.

**Planned fix: ask the tracker which scopes exist.** Each backend reports its existing scopes, cached
once per pass (Jira: `GET /rest/api/3/project`). Matching's verify step then drops a candidate whose
scope does not exist on its tracker, as "not a project on this tracker", before any per-key read. That
is exact, needs no config, keeps a real `HTTP-12`, works per tracker syntax, and stays in verification,
where network checks belong. It also replaces one read per junk key with one call per pass. **Deferred:**
build it when a junk key first displaces a real one at the cap, or when the wasted reads show up in a
measurement.

## Status

open, deferred. Fix planned: each tracker reports its existing scopes, and verification drops a candidate in a scope that doesn't exist. Not a pattern list. 11 narratives affected, 0 real keys displaced
