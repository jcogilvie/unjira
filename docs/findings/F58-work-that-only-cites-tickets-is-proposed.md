# F58 — work that only cites tickets is proposed nowhere

A narrative whose every verified candidate the model judged `mentioned` (a citation, a "caused by", a
"discovered while") ends matching with link rows and no primary. Matching now records that as an
examination, so it no longer re-selects the narrative. But no other path picks it up:

- **The reconciler** drafts comments and transitions onto a narrative's linked issue, and a `mentioned`
  link is not the work, so drafting onto it would put one narrative's story on a ticket it only cites.
  It selects the narrative, finds no actionable link, and records that in `reconcile_examinations`,
  so the narrative holds no slot and is not counted as unexamined work until a member event is
  linked after it. Only a member link re-admits it. A role that changes without one, such as an
  applied create adding a primary to a narrative examined while it was mentioned-only, waits for the
  next member link before the reconciler drafts for it.
- **The create path** selects on `NarrativesAwaitingCreate` (`internal/store/createbacklog.go:88`),
  whose predicate (`awaitingCreate`, `:54`) treats a link of any role as tracked.

So the work is untracked, and unjira can say nothing about it. Seen on a real 30-day store: narrative
14, citing two tickets at confidence 0.80–0.92, both `mentioned`.

This outcome is now reached more often, by design. `classifySystemPrompt` permits no primary, and a
lone candidate that was only mentioned goes to the model instead of being promoted at confidence 1.0
(`Provenance.NamesTheWork`, `internal/correlator/match_types.go`). On that store, 9 narratives had a
lone verified prose candidate after the PR-body change; every one the model judges `mentioned` lands
here. Silence is the better failure, since the alternative was a comment drafted onto a ticket the
work only cited, but it is still silence.

The open question is policy, not mechanism: should a narrative with only `mentioned` links be a create
candidate, with the cited tickets named in the proposal so a reviewer can link instead? The F13
backstop (`applyCreate` refusing once a primary exists) would still hold.

## Status

open, policy question: make an all-`mentioned` narrative a create candidate? One real instance. Narrowed: the reconciler examines such a narrative once and records it (`reconcile_examinations`), where before it held a slot and was counted as unexamined work on every pass (`TestReconcile_MentionedOnlyNarrativeYieldsUntilNewWorkIsLinked`)
