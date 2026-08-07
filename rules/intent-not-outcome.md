---
scope: reconciler
confidence: high
learned: 2026-07-16
source: predecessor operational experience (see docs/design-notes.md)
---

Never emit a state-bearing action from transcript intent. A transcript shows what someone was
*doing or drafting*, not the final state — the predecessor recorded "review not yet posted" from
a drafted comment when CHANGES_REQUESTED had already been submitted three days earlier.

Before any comment, transition, or ticket creation, confirm the *current* state on the system of
record (live Jira/GitHub) — do not infer it from the narrative. "I'll open a PR" is not evidence a
PR exists. Drafting ≠ done. This is the line between a correlator (reads streams) and a reconciler
(must diff against live state now).
