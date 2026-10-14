# F52 — a single dispute too large for the context window fails the pass

The dispute re-ask describes each claimant of a disputed event by ALL of its other member events, in
full (`writeClaimant`, `internal/correlator/cluster_dispute.go:419`). A dispute set that does not fit
one call is split into batches that each fit (`batchDisputes`, `:359`), so the set's size no longer
matters. One dispute's size still does: if a single disputed event's claimants alone exceed the
context window, nothing smaller can be asked, and the pass refuses before spending a dispute call
(`requireDisputeFits`, `:391`). It never truncates and never sends a prompt over budget. The case
that makes this likely is a bisected pass, which bisected because the window did not fit: each
claimant can hold a whole half, so one dispute can list more member text than either half's prompt
did (`TestCluster_ADisputeTooLargeAloneFailsLoudly`). **Consequence:** a wide window whose halves
each fit can still die at the dispute step, without having clustered anything wrong. Unmeasured on
real data: claimant sizes on a real disputed pass are what M4 and M5 will show. The remaining bounds,
capping each listed member summary or listing at most N members per claimant, trade the model's view
of a claimant for the call fitting. That is the trade F16 measured for context narratives, and
deciding it needs those numbers first.

## Status

open. What remains of the dispute re-ask's size once a dispute set too large for one call is batched. Unmeasured
