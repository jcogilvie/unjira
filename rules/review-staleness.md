---
scope: reconciler
confidence: high
learned: 2026-07-16
source: predecessor operational experience (see docs/design-notes.md)
---

A review is "done" only until the author pushes commits past the reviewed SHA. Detect re-review-owed
deterministically: compare the PR's current head SHA to the `commit_id` of the reviewer's latest
review. Head advanced ⇒ bounce back to "owed by me"; head unchanged ⇒ stays "waiting on author".

This discriminated correctly with zero false positives in practice. Store the reviewed `commit_id`
with the review event and diff against head on each pass. Note also that PR state (open/merged/
mergeable) is not review state — you must fetch the reviews endpoint to know a verdict was left.
