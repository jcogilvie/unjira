package store

import "fmt"

// matchExaminationsSchema records that matching looked at a narrative and found nothing
// to match against — the watermark that keeps its backlog draining (finding F22).
//
// A separate table rather than a column on `narratives` because this package has no
// migration mechanism: every schema statement is CREATE TABLE IF NOT EXISTS, so a new
// TABLE lands on an existing database while a new COLUMN would need an ALTER nothing
// runs. A one-row-per-narrative side table gets the same effect and is additive.
//
// examined_at is the watermark itself, compared lexically against
// narrative_events.linked_at, so it MUST use that column's exact format:
// %Y-%m-%dT%H:%M:%fZ — milliseconds, not whole seconds.
//
// The narrative_events schema comment states why, and this got it wrong on the first
// attempt: mixing %f with %S inverts the comparison, because '.' (0x2E) sorts before
// 'Z' (0x5A), so "…:05Z" > "…:05.123Z" and a LATER event reads as earlier. The
// re-admission then silently never fires and the watermark becomes a tombstone —
// exactly the failure the rest of this file argues against. Written by SQLite's own
// strftime rather than Go's time.Format so there is one source of the format.
const matchExaminationsSchema = `
CREATE TABLE IF NOT EXISTS match_examinations (
    narrative_id INTEGER PRIMARY KEY REFERENCES narratives (id),
    examined_at  TEXT NOT NULL,
    reason       TEXT NOT NULL
);`

// matchExaminationPredicate excludes narratives matching already examined and found
// nothing to match against, UNLESS an event has been linked since — the watermark half
// of F22's fix.
//
// A shared const, interpolated into both NarrativesWithoutPrimaryLink and
// CountNarrativesWithoutPrimaryLink, because that method's own doc comment states the
// invariant: "the predicate MUST stay identical." Two hand-written copies would be two
// things to keep in sync, and the failure mode is a count reporting progress against a
// population the pass never examined — how F22 presented in the first place.
//
// Carries no bound parameters, so it can be concatenated without disturbing either
// query's argument order. Comparing linked_at > examined_at makes this a watermark: an
// EXTENDS that links new events re-opens the narrative, because those events can carry
// keys it did not have when examined.
const matchExaminationPredicate = `
		 AND NOT EXISTS (
		     SELECT 1 FROM match_examinations me
		     WHERE me.narrative_id = n.id
		       AND NOT EXISTS (
		           SELECT 1 FROM narrative_events ne
		           WHERE ne.narrative_id = n.id AND ne.linked_at > me.examined_at
		       )
		 )`

// RecordMatchExamined marks narrativeID as examined by matching at a moment when it
// had nothing to match against, so the selector yields to the narratives behind it.
//
// WHY THIS EXISTS. NarrativesWithoutPrimaryLink orders by (window_start, id) — stable
// — and matchOne correctly writes nothing when gatherCandidates finds no keys
// ("untracked work is the default path, not a special case"). Stable order plus an
// outcome that leaves no trace is a livelock, which is design-notes #29 precisely.
// Measured before this: eleven consecutive passes each reported 49 unmatched, of which
// 33 had zero candidate keys, and 18 of the 20 selected per pass were among those 33.
// The 16 narratives that DID have candidates sat behind them permanently.
//
// It also blocked the whole queue, not just the counter: creates are gated on matching
// catching up (F13's precondition), so a count that can never reach zero means no
// create is ever proposed.
//
// A WATERMARK, NOT A TOMBSTONE. Upserting rather than inserting keeps one row per
// narrative, and the row records WHEN. A narrative can gain events later via
// ClusterExtends, and those events can carry keys it did not have when examined — so
// NarrativesWithoutPrimaryLink re-admits it as soon as an event is linked after
// examined_at. Recording "never match this" would make an early absence permanent,
// which is the mistake StatusSuppressed's own doc comment warns about for its case.
//
// reason is stored for the operator, never read as control flow: "no candidate keys in
// any event" and "every candidate failed verification" are different situations worth
// telling apart when someone asks why a narrative is quiet.
func (s *Store) RecordMatchExamined(narrativeID int64, reason string) error {
	if _, err := s.db.Exec(
		`INSERT INTO match_examinations (narrative_id, examined_at, reason)
		 VALUES (?, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), ?)
		 ON CONFLICT(narrative_id) DO UPDATE SET
		     examined_at = excluded.examined_at, reason = excluded.reason`,
		narrativeID, reason,
	); err != nil {
		return fmt.Errorf("recording match examination for narrative %d: %w", narrativeID, err)
	}

	return nil
}

// CountMatchExaminations is how many watermark rows narrativeID has. Exists for the
// test that pins one-row-per-narrative: a pass that re-examines must not grow the
// table without bound.
func (s *Store) CountMatchExaminations(narrativeID int64) (int, error) {
	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM match_examinations WHERE narrative_id = ?`, narrativeID,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting match examinations for narrative %d: %w", narrativeID, err)
	}

	return n, nil
}
