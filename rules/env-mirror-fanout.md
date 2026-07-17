---
scope: correlator
confidence: high
learned: 2026-07-16
source: predecessor operational experience (see docs/design-notes.md)
---

One logical change often fans out into many near-identical per-region PRs (e.g. "switch
the shared infra module to managed mode" → `repo#678–689`, one PR per region). That is **one**
review effort and one narrative, not 12.

Collapse env-mirror fan-out into a single narrative with a PR/issue range. Heuristic that worked:
normalize titles by stripping a trailing `(region)` and folding `for <env>` / `Switch <env>` /
`in <env>` / `-<env>` tokens where `<env>` is a known region (zrh, us2, tky, syd, mon, kor, fra,
fed, dub, corp, long, prod, dev, stag); then same-author + same-normalized-title + adjacent numbers
⇒ one narrative. This also protects estimation: 12 mirror PRs must not inflate velocity as 12 items.
