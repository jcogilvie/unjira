# F60 — upstream work done for an internal ticket is routed by repository, not by purpose

Destinations route untracked work by the repository it happened in, which is right for the common case.
But some upstream work exists to serve an internal ticket: a design review that unblocks an internal
story, an upstream fix for an internal blocker, or a regression analysis behind an internal rollout. If
that upstream repository's tracker mirrors nowhere, the work is ticketed nowhere, and it never reaches
the internal ticket it serves.

On a real 30-day store, under a policy that mirrored the upstream repositories the operator does ticket,
three of the five narratives sent nowhere were exactly this. Each had an internal ticket it plausibly
belonged to, and matching linked none of them.

The fix is not a routing rule, since "serves ticket X" is semantic. It is matching: an upstream
narrative that matches an internal ticket is linked work, so it is commented on that ticket rather than
proposed as a create anywhere. Two things are not measured yet: how often an internally motivated
upstream narrative names its internal ticket at all, and whether the candidate ranking reaches the
ticket when it does.

## Status

open. Matching's job, not routing's. 3 of 5 measured cases
