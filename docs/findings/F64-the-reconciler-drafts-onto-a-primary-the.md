# F64 — the reconciler drafts onto a primary the model itself doubted

`match.confidence_floor` is documented as governing "what unjira asserts, not what it records"
(`internal/config/config.go`, `MatchConfig.ConfidenceFloor`). Matching honours that: a sub-floor
primary is written and reported as not promoted (`persistLinks`, `internal/correlator/match.go:486`).
Nothing downstream reads the floor. `actionableLinks` (`internal/reconciler/reconciler.go:349`) takes
every primary and `same_work` link at any confidence, so the reconciler drafts a comment onto a
ticket the model put at 0.15, and the drafter's own confidence is unrelated: one such comment was
drafted at 0.95.

On a real 30-day store, 12 primaries sat below the example config's floor of 0.7. Every one was
drafted for: 8 proposed comments (narratives 2, 30, 32, 49, 53, 66, 87, 124) and 4 suppressed drafts
(20, 88, 107, 126). That is 8 of the 24 proposed comments a sweep reviewed, which judged about 20 of
the 24 unwarranted. Four of the eight (2, 30, 53, 66) were sessions that only scanned a journal (F67).

Not fixed here, because it reverses a decision rather than repairing a bug. The reconciler spec chose
to select low-confidence primaries so the reconciler is not "blind to exactly the narratives it most
needs", and a gate would leave such a narrative with nothing at all: not re-matched (a primary at any
confidence takes it out of matching's backlog), not drafted, not proposed for creation (F58's
predicate treats any link as tracked). The options:

- **Gate `actionableLinks` on the floor** and record the examination with its reason, accepting F58's
  silence for these narratives.
- **Show the drafter the link's confidence** and floor the action's confidence by it, the way
  `floorConfidence` (`internal/reconciler/draft.go:196`) already floors by deterministic facts.
- **Re-admit a sub-floor narrative to matching** when new member evidence arrives.
