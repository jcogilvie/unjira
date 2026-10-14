# F57 — work linked to an upstream issue is never mirrored into another tracker

The spec's destination table gives work linked to issue *I* in tracker *T* two kinds of destination:
comments and transitions on *I*, and **creates in *T*.mirror_to**. Only the first is built. The create
path (`reconciler.ProposeCreates`, `internal/reconciler/create.go`) selects narratives with no link
at all (`store.NarrativesWithNoIssueLink`), so a narrative matched to `crossplane/crossplane#6812`
never reaches it, whatever `upstream.mirror_to` says.

**Why it was not built with the rest.** A mirror create for linked work needs a selection of its own,
and two invariants that assume one tracker per narrative to give way first: matching's
`one_primary_per_narrative` index, and `gate.Applier`'s duplicate backstop, which refuses a create for
a narrative with a primary (now narrowed to a primary in the destination's own tracker). It also wants
a memory of "already mirrored", or every pass re-proposes. Each is a design decision, not a line.

**Consequence:** an operator who sets `mirror_to` on an upstream tracker gets mirrored tickets for
untracked upstream work only; upstream work that names its issue gets none. Silent in the sense that
nothing reports the gap per narrative.

## Status

open. The tracker model's slice 5 built mirror_to for untracked work only
