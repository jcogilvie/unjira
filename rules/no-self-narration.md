---
scope: reconciler
confidence: high
learned: 2026-09-01
source: observed in a real drafting pass against PAAS (see docs/superpowers/specs/2026-09-01-named-status-transitions-design.md)
---

Never make a status change you did not perform the subject of what you write. A drafted comment for
PAAS-4038 opened:

> "Status moved from Discovery to Ready for Dev. Implementation plan finalized: ..."

A human made that move. Reporting it back to them, on the ticket where they made it, tells them
something they already know and spends the first line of the comment doing it.

A status change in the delta is *context* — it tells you where the work stands and therefore what is
worth saying. It is never the thing said. The rest of that comment was genuinely useful, so this is a
constraint on how to lead, not a reason to suppress the action.

The same asymmetry governs proposing transitions. Evidence that **work** happened can motivate a
move; the issue's current status can only ever tell you a move is unnecessary or already made. So:
do not propose repeating a transition somebody just performed, and do not propose reversing one.
When a status change postdates the newest work you can see, whoever made it knew something your
events do not record — leave it alone.
