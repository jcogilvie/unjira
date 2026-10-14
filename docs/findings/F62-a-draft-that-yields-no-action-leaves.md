# F62 — a draft that yields no action leaves no trace, so it is drafted again every pass

When drafting yields no action, `reconcileOne` returns a result with neither a proposal nor a
suppression (`internal/reconciler/reconciler.go:289` onward). That happens when the model returns an
empty array, or names only keys that `actionsFromVerdicts` does not recognize, which it logs and drops
(`internal/reconciler/draft.go:390`). `Persist` writes nothing for such a result, so no action row
bounds the delta. The narrative is selected again next pass, a model call each time, and the backlog
count reports it as unexamined work throughout. This is design-notes #29's livelock, with spend. Every
other "nothing to do" exit in `reconcileOne` records something: a suppression row, or a
`reconcile_examinations` watermark for a self-authored delta or a narrative with no confident
primary.

It is not closed the same way, because this outcome may be a malformed answer rather than a verdict.
The prompt requires exactly one action per issue shown (`draft.go:28`), so an empty array contradicts
it. Matching faces the same question for an empty classification
(`TestMatch_EmptyClassifierResponseIsLoggedNotSilent`). Whether to re-ask, as matching does for an
unparseable response, or to record the examination is the open question. **Consequence:** a model that
declines to draft for a narrative is billed for that narrative on every pass for as long as it keeps
declining. Unmeasured: no narrative in a real 30-day store's backlog is in this state.

## Status

open, policy question: re-ask (an empty array breaks the prompt's one-action-per-issue rule) or record an examination? Found closing the mentioned-only path, the last silent exit in `reconcileOne`. 0 in a real 30-day store's backlog
