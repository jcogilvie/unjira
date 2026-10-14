# F50 — a split narrative is still read by the create path and the review queue

`StatusSplit` is honored by one reader, `NarrativesOverlapping` (`internal/store/narratives.go:293`), so
a narrative emptied of its work (`Tx.MarkSplitIfEmptied`, `internal/store/narrativestatus.go:57`, from a
clustering pass, a triage split or a triage merge) stops being clustering context and nothing else
changes. Two other readers still see it:

- **The create path, once.** `NarrativesAwaitingCreate` (`internal/store/createbacklog.go:88`) has no
  status predicate, so a split narrative with no issue link is selected by the next create pass.
  `proposeCreateOne` finds no member events and suppresses it with a false reason, "every event is
  unjira's own output" (`internal/reconciler/create.go:203`). It no longer holds the slot: that branch
  now records a create examination (`store.RecordCreateExamined`), which excludes the narrative until a
  member event is linked to it again. So the cost is one slot on one pass, no model call, and a
  misleading line in that pass's summary (`TestProposeCreates_AnEmptiedNarrativeYieldsItsSlot`: cap 1,
  the split narrative examined on pass 1, the real one behind it reached on pass 2).
- **The review queue.** A proposed action drafted for the narrative before its work moved stays at
  `proposed` (`ActionsByStatus`, `internal/store/actions.go:102`, filters on action status alone; probed),
  so triage presents text describing work now attributed elsewhere. By reading, not probed: while it is
  open, `suppressDuplicates` (`internal/reconciler/reconciler.go:429`) drops the receiving narrative's own
  proposal on the same issue, because neither `NarrativesForIssue` nor `openProposalFromAnother` reads
  narrative status.

**Consequence:** a reviewer can approve a comment on behalf of a narrative that holds nothing, and each
emptied narrative is reported once as unjira's own output when it is not. Pre-existing for triage split's
sources, which have been marked `split` since that verb landed. Not fixed here: what a split narrative's
pending actions should become (superseded, rejected, left for the reviewer) is an action-lifecycle
decision, and whether the create selector should name `split` outright should be decided with it.
Frequency on real data is unmeasured.

## Status

open, narrowed: the create path examines a split narrative once and its create examination then excludes it, so it no longer holds a slot (`TestProposeCreates_AnEmptiedNarrativeYieldsItsSlot`); it is still reported with a false reason, and the review-queue half is untouched. Pre-existing for triage split's sources; found while fixing F40, which made clustering and merge mark emptied narratives split too. Probed before the create backlog fix: a split narrative held a create slot for 3 of 3 passes at cap 1
