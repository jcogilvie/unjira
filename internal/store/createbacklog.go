package store

import "fmt"

// awaitingCreate is the SQL predicate for "the create path still owes this narrative a
// decision", shared verbatim by NarrativesAwaitingCreate (what a create pass selects)
// and CountNarrativesAwaitingCreate (what the pass summary reports as left over).
//
// It is everything reconciler.proposeCreateOne would skip WITHOUT a model call, moved
// into SQL. The Go checks stay as a backstop, but on their own they were a livelock:
// the selector took the oldest untracked narratives by (window_start, id) and the pass
// then skipped the handled ones, so those kept the first max_narratives_per_pass slots
// on every pass and nothing behind them was reached. On a real 30-day store, 129
// untracked narratives under a cap of 20: pass 1 proposed 11 and declined 9, every
// later pass proposed nothing, and 109 were never examined. F12's and F26's shape on
// the create half, fixed the same way — one predicate, so the selector and the count
// cannot drift (F10).
//
// Three clauses, one per kind of skip:
//
//   - No CONFIDENT primary: no narrative_issues row with role primary at or above
//     match.confidence_floor (the one bound parameter). Every weaker link leaves the
//     narrative untracked work: a `mentioned` citation, a `same_work` with no primary
//     beside it, and a primary the model itself doubted. The proposal names those
//     tickets (reconciler.CreateCandidate), so a reviewer can approve the new ticket or
//     link the work to one of them instead; the reconciler drafts onto none of them
//     (reconciler.confidentPrimary), so this is the only path that can speak for such
//     work. Before this, any link of any role excluded the narrative, and with the
//     reconciler also silent on a `mentioned` link the work was proposed nowhere
//     (findings F58 and F64). The boundary is matching's own: persistLinks withholds
//     promotion when confidence < floor, so a primary AT the floor is confident. A NULL
//     confidence reads as 0, as store.NarrativeIssues reads it, so Go and SQL agree.
//
//   - Not a narrative whose create-path decision stands: one whose actions include a
//     create at proposed, approved, applied or declined, or a suppression, with no
//     member link since the narrative's latest action. linkedSinceLastAction is
//     DeltaEvents' own bound, so "new work since the decision" means here exactly what
//     it means to the pass.
//
//   - A live or applied create: proposing again would double the review queue or open
//     a duplicate ticket. With new work the pass looks again, and proposeCreateOne
//     blocks the taken scope per destination, so a create open in one scope does not
//     settle another for later work. Without new work the decision stands, including
//     the model's choice among several destinations: re-offering the others on every
//     pass would override that choice as soon as one remained.
//
//   - A decline: the model judged the work not worth tracking (hasDecline, then
//     DeltaEvents).
//
//   - A suppression: written when there is no allowed destination, or every allowed
//     scope already holds a create. Without this, every narrative whose work went
//     upstream would hold a slot on every pass.
//
//     Rejected and failed creates are not decisions here, matching openOrAppliedCreate:
//     a human said no to that draft, or the write never happened (F59).
//
//   - Not examined-and-entirely-self-authored with no member link since
//     (createExaminationPredicate, written where dropSelfAuthored empties a narrative).
//
// Correlated on n.id with exactly ONE bound parameter, the confidence floor, which is
// config and so cannot be a literal here. Both queries bind it first, so it drops into
// either WHERE clause unchanged.
const awaitingCreate = `NOT EXISTS (
		     SELECT 1 FROM narrative_issues ni
		     WHERE ni.narrative_id = n.id AND ni.role = '` + string(RolePrimary) + `'
		       AND COALESCE(ni.confidence, 0) >= ?
		 )
		 AND NOT (
		     EXISTS (
		         SELECT 1 FROM actions a
		         WHERE a.narrative_id = n.id
		           AND ((a.type = '` + ActionTypeCreate + `'
		                 AND a.status IN ('` + StatusProposed + `', '` + StatusApproved + `', '` + StatusApplied + `', '` + StatusDeclined + `'))
		                OR a.status = '` + StatusSuppressed + `')
		     )
		     AND NOT EXISTS (
		         SELECT 1 FROM narrative_events ne
		         WHERE ne.narrative_id = n.id AND ` + linkedSinceLastAction + `
		     )
		 )` + createExaminationPredicate

// NarrativesAwaitingCreate returns up to limit narratives the create path still owes a
// decision — untracked work (no primary at or above confidenceFloor, which is
// match.confidence_floor) it has not already proposed for, applied, declined with
// nothing new since, or found to be only unjira's own output — ordered by
// (window_start, id).
//
// RENAMED from NarrativesWithNoIssueLink when its meaning narrowed from "no issue link"
// to "no issue link and not already handled", so no caller could keep the old meaning
// by accident. Its population widened again, from "no issue link" to "no confident
// primary", and the floor became a parameter; the new parameter puts every caller in
// front of that change. See awaitingCreate for each clause and why it is in SQL.
//
// Exists because the reconciler could not see untracked narratives at all.
// NarrativesWithActionableLinks requires a link by construction, so reconcileOne
// was never invoked for an unlinked narrative — proven by probe:
//
//	NarrativesWithActionableLinks (reconciler's backlog) -> 0 rows
//	NarrativesWithoutPrimaryLink (matching's backlog)    -> 1 rows
//
// which is why adding "create" to the drafting prompt would have changed nothing.
func (s *Store) NarrativesAwaitingCreate(limit int, confidenceFloor float64) ([]NarrativeRow, error) {
	rows, err := s.db.Query(
		`SELECT n.id, n.window_start, n.window_end, n.title, n.summary,
		        n.status, n.compaction_boundary, n.compaction_boundary_event_id
		 FROM narratives n
		 WHERE `+awaitingCreate+`
		 ORDER BY n.window_start, n.id
		 LIMIT ?`,
		confidenceFloor, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("querying narratives awaiting a create decision: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []NarrativeRow
	for rows.Next() {
		row, err := scanNarrativeRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning narrative awaiting a create decision: %w", err)
		}
		out = append(out, row)
	}

	return out, rows.Err()
}

// CountNarrativesAwaitingCreate is how many narratives the create path still owes a
// decision — the create backlog behind reconciler.max_narratives_per_pass.
//
// Same predicate and the same floor as NarrativesAwaitingCreate by construction
// (awaitingCreate), so the caller must pass the floor the pass selected under. Counted
// after a pass, it says how much untracked work that pass's cap left unexamined: before
// it existed, a pass starved by already-handled narratives rendered exactly like a pass
// with nothing left to create.
func (s *Store) CountNarrativesAwaitingCreate(confidenceFloor float64) (int, error) {
	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM narratives n WHERE `+awaitingCreate, confidenceFloor,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting narratives awaiting a create decision: %w", err)
	}

	return n, nil
}
