# Learned rules

Corrections from the review queue get distilled into markdown rules here — one file per
rule, human-auditable and diffable. The correlator and reconciler load these into their
prompts each pass.

Keeping rules as plain markdown in the repo is deliberate: lifting team-level norms into a
shared repo later (so anybody's copy of the agent benefits) is a `git remote`, not a
redesign.

## Format

```markdown
---
scope: correlator | reconciler | estimator
confidence: high | provisional
learned: 2026-07-11
source: review-queue correction on action #42
---

Commits touching `auth/` belong to the SSO epic (PROJ-88), not new tickets.
```

Nothing writes here in phase 0 — seed it manually if you already know norms the agent
should start with.
