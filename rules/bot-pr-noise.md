---
scope: reconciler
confidence: high
learned: 2026-07-16
source: predecessor operational experience (see docs/design-notes.md)
---

`[bot]`-authored dep-bump PRs (`renovate[bot]`, `dependabot`) are rubber-stamps, not substantive
review effort — they dominated the raw count (18 of 23 in one window) and buried the real reviews.

Exclude `[bot]`-authored PRs from *itemized* narratives, but keep them in a rollup count (dropping
them entirely loses legitimate volume). More broadly: a review or commit is not automatically
creditable work — apply the same substance filter to what counts as trackable effort at all.
