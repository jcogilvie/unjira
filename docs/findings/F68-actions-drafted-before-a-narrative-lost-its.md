# F68 — actions drafted before a narrative lost its confident primary stay where they are

A narrative with no confident primary is drafted onto by nobody and is proposed as a create naming its
links (`reconciler.confidentPrimary`, `internal/reconciler/reconciler.go`). The rule applies when a pass
selects, so it says nothing about rows written before it applied: a store that predates it, or a
`match.confidence_floor` raised after a pass. Two consequences:

- A comment or transition drafted onto the doubted primary stays `proposed`, so the queue holds it beside
  the create that names the same ticket as a candidate. On a real 30-day store at floor 0.7: 8
  narratives (2, 30, 32, 49, 53, 66, 87, 124), one proposed comment each.
- A reconciler suppression excludes the narrative from the create path until new member work arrives.
  `awaitingCreate`'s decision clause counts any `suppressed` row (`internal/store/createbacklog.go`), and
  a reconciler suppression and a create-path suppression are the same row (`recordSuppression`,
  `internal/reconciler/persist.go`: type `comment`, no issue key). Same store: 4 narratives (20, 88,
  107, 126) whose sub-floor primaries drew suppressed drafts.

**Consequence:** a reviewer may approve a comment on a ticket the work's own create proposal names as
doubtful, and a narrative whose draft was once suppressed waits for new work before its create is
considered. Not fixed here: what to do with an open draft whose link stopped being confident is an
action-lifecycle decision (F50 has the same shape), and telling the two suppressions apart needs a type
or column on the row.

## Status

open. Found closing F58/F64. On a real 30-day store at floor 0.7: 8 narratives keep a proposed comment beside their new create candidacy, 4 are held out of the create path by an old reconciler suppression until new work
