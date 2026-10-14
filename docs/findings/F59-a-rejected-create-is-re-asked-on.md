# F59 — a rejected create is re-asked on the next pass whether or not anything changed

`openOrAppliedCreate` (`internal/reconciler/create.go:466`) deliberately does not treat a `rejected` create
as a blocker, on the grounds that "a later pass with more events may be right". Nothing checks for more
events. `awaitingCreate` (`internal/store/createbacklog.go:54`) counts a proposed, approved, applied
or declined create, or a suppression, as a decision that stands until new member work arrives, but not a
rejected one, so a narrative whose create a reviewer just rejected is selected by the very next create pass, and the model is asked again about exactly the events the reviewer ruled on.
`TestProposeCreates_ARejectedCreateDoesNotBlockForever` (`internal/reconciler/create_test.go:162`) pins
it: no event linked after the rejection, one model call, one fresh proposal. Contrast `DeltaEvents`
(`internal/store/narratives.go:429`), which bounds on the latest action of any status precisely so that a
rejected action is not re-proposed identically and the reviewer's "no" carries weight.

**Consequence:** every rejected create costs a model call on the next pass and, usually, puts a
near-identical proposal back in the review queue, so a reviewer who says no to a ticket says it again.
Bounded at one re-ask per rejection rather than per pass, because the re-proposal is itself a `proposed`
create and excludes the narrative, so it does not starve the create backlog. A `failed` create is re-asked
the same way, which is defensible, since nothing was written. Not fixed here: the shape is a selector clause
(a rejected create with no member link since the narrative's latest action) plus the matching backstop, but
it changes what a rejection means on the create path, which is a review-semantics decision. Frequency on
real data is unmeasured.

## Status

open. Found fixing the create backlog; pinned as current behaviour by `TestProposeCreates_ARejectedCreateDoesNotBlockForever` (no new event, one call, one fresh proposal). Bounded at one re-ask per rejection. Unmeasured on real data
