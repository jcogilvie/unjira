# F63 — a transition on someone else's ticket is proposed like one on your own

The reconciler proposes a status move on whichever issue a narrative's work is linked to, whoever owns
it. On a real 30-day store, upstream work that a teammate's blocked ticket was waiting on drew a proposal
to move that teammate's ticket to In Review. The work was real and the link was defensible, but moving
another person's ticket is a statement about their work. A comment would carry the same news without
making it.

This needs a decision, not just a guard, so none is built. The options:

- **Never transition** an issue assigned to someone other than unjira's operator, and draft a comment
  instead.
- **Make it a per-tracker rule**, defaulting to the above.
- **Leave it to review**, as today.

The operator's identity is already available on every writable tracker (slice 3's self-identity check).
The issue's assignee is not yet on `tasktracker.Issue`.

## Status

open, policy question: never, per-tracker rule, or leave to review. One real instance
