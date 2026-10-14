# F45 — the model can still reshuffle a PR's placed members apart

The PR identity join (`internal/pipeline/preassign.go`, `planPRIdentity` at `:83`) places only
UNPLACED events. A narrative's eligible members, the PR's `:opened` among them, are still numbered in
the clustering prompt whenever that narrative is context, and `Persist` moves any the model puts in
another cluster (`moveMembers`, `internal/correlator/correlator.go:1704`). The join makes this more
likely to be offered, though not more likely to be taken: placing a merge extends its narrative's
`window_end` into the current window, so the narrative becomes context in the very pass that placed
it (`hidePreAssigned`, `preassign.go:277`, hides only the placed event). A reshuffle that moves
`:opened` away splits the PR again, with the merge left where identity put it, and a later event for
that PR then finds two holders and falls back to the model (`PRSeveralHolders`).

Not introduced by the join. Every eligible member has always been open to reshuffling. Measured since
the join landed, in six two-pass acceptance reps (`SHARED_CUT=2026-09-21`, window 2026-09-16 to
2026-10-02): 24 identity placements in total, M3 PR integrity 26/26 in every rep, and 0 identity-placed
members moved by the model. So the model has not been observed doing this. It stays open because the
mechanism is still there, and six reps of one window bound how often it happens, not whether it can.
If it occurs, it shows as an M3 split whose identity half was placed by `identity`
(`narrative_events.member_placement`), and the remedy is to number no eligible member whose PR an open
narrative already holds by identity. That is a prompt-shape decision, and is not taken here.

## Status

open. Pre-existing for every eligible member; the join only places unplaced events. Measured in six two-pass reps after the join: 24 identity placements, M3 26/26 in every rep, 0 identity-placed members moved by the model
