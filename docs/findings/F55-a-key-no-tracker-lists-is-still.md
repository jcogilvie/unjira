# F55 — a key no tracker lists is still read, through a fallback

The tracker model's routing is a pure function of key and config, and the spec reads that as "find the
one tracker whose scopes contain it". The resolver keeps one exception, deliberately: a project-syntax
key no tracker's scopes cover is **read** through the tracker covering the first configured project
scope (`appContext.resolver`, `cmd/unjira/main.go`; `tasktracker.Resolver.WithReadFallback`). That is
the tracker every candidate used to be verified against, so behaviour is unchanged by the refactor.
A fallback resolution never carries a writer, and the write gate refuses the key as untracked.

**Why it stays for now.** Without it, work on another team's ticket (a key in an unlisted project) no
longer verifies, the narrative stays unmatched, and the create path proposes a new ticket for work that
is already tracked: a duplicate, which is worse than today's "linked, refused as untracked, retarget
in triage".

**The cost.** Such a key reads from a site chosen by config order rather than by ownership, so on a
multi-site setup it can be checked against the wrong site, the bug per-key routing otherwise removes.
Every stored key that resolves only this way is reported at startup (`pipeline.UnroutedStoredKeys`),
so the population is visible. Deciding whether to keep the fallback needs that population measured
on a real store: how many links depend on it, and how many of them a scope added to config would
cover instead.

## Status

open. Kept by the tracker model's slice 2 to preserve behaviour; reported at startup. Unmeasured
