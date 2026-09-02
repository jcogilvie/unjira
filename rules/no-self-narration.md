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

The stronger form of the same principle: **if the delta contains nothing but the tracker's own
records — status changes, field edits, comments already on the issue — there is nothing to say.** Not
"lead differently"; say nothing. Every sentence available to you came from the issue you would be
posting on, so the comment restates the ticket to the people reading the ticket. A measured drafting
pass produced 18 such comments out of 21, one of them a long, well-argued RCA that was a paraphrase
of its own issue's description.

This one is also enforced in code (`reconciler.suppressTrackerEcho`), because a prompt cannot see
where its context came from — you should recognize the situation, but you are not the last line of
defense for it. What you *can* do is notice when your only material is the ticket itself and say so
in the rationale rather than writing a summary of it.
