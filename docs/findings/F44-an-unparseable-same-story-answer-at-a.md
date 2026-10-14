# F44 — an unparseable same-story answer at a bisection seam still kills the whole pass

Two lossless classes are absorbed: a trailing comma (`llm.JSONArrayPayload` and `llm.JSONObjectPayload`
drop it via `hujson.Standardize`), and a NEW cluster holding no event (`pipeline.dropEmptyClusters`
discards it and reports it in the pass summary). Every other model response on the clustering and
matching paths that the parser refuses is re-asked, quoting the refusal and the parser's reason,
within its configured budget, then fails loudly: a match response (`llm.max_match_reasks`), the
clustering response itself (`llm.max_cluster_reasks`, each re-ask re-sending the whole window prompt),
an omission round (`llm.max_omission_reasks`) and a dispute batch (`llm.max_dispute_reasks`).

What remains is `checkSameStory` (`internal/correlator/correlator.go`), the one call a bisected pass
makes at each seam to ask whether the two adjacent NEW clusters are one story. Its response is a JSON
object, and one `json.Unmarshal` refuses still fails the pass with no follow-up, after both halves
have been paid for. It only runs on a pass that bisected, and its answer is a boolean plus a title and
summary, so the exposure is small: no such failure has been observed (0 in the 32 passes measured on
2026-10-02 and 2026-10-04, and none in a 30-day drain). The same shape would fit, at the cost of a
tiny prompt rather than a window's: re-ask quoting the reason, bounded by a budget, then fail loudly.

## Status

open, narrowed: the observed trailing comma is absorbed (hujson); matching, clustering, omission-round and dispute responses are re-asked within configurable budgets. Only the same-story check at a bisection seam is still never retried. 0 such deaths in 32 passes; a budgeted re-ask is the shape if one appears
