---
scope: correlator
confidence: high
learned: 2026-07-16
source: predecessor operational experience (see docs/design-notes.md)
---

Every proposed `(narrative, issue-key)` binding must be verified against the real issue before it
drives an action: `getIssue(key)` must resolve, and its summary/component must actually match the
narrative. Narrative→issue matching is exactly where an LLM invents plausible-but-false links — the
predecessor confidently reported a `repo#573 = PROJ-3852` binding where both halves were wrong (the
PR belonged to a different ticket, and the issue key was someone else's unrelated work).

Confidence scores are not enough; a hallucinated match can be high-confidence. Treat any ref whose
timestamp is in the future, or that does not resolve, as a transcript artifact (a simulated/planned
action), not reality. Cheap API verification before the review queue keeps the queue signal-rich.
