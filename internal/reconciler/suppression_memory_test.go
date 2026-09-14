package reconciler

// suppression_memory_test.go covers the second half of finding F12: a suppressed
// narrative left no trace, so the reconciler re-derived the same suppression every
// pass forever and never reached the rest of its backlog.
//
// The mechanism, measured on the live store: Reconcile selects
// ORDER BY (window_start, id) LIMIT 20 — a STABLE order. The 20 oldest linked
// narratives were all pure tracker-echo, so suppressTrackerEcho correctly declined
// to comment. Suppression wrote no action row; no row meant no watermark; and
// DeltaEvents bounds on MAX(created_at) over actions, so the same 20 were selected
// again. Three consecutive passes examined the identical 20 and the 35 narratives
// at position 21+ were never reached.
//
// This is not a new problem in this codebase, and that is the argument for the
// shape of the fix. The CREATE path hit it first and solved it the same way —
// StatusDeclined's doc comment records the identical three-pass measurement ("one
// LLM call per untracked narrative per watch tick... Remembering the answer is the
// whole fix"). The comment/transition path simply never got that treatment: before
// this change every action row in the live store was a create, and zero came from
// reconcile.
//
// StatusSuppressed is a distinct value rather than a reuse of StatusDeclined, for
// the reason StatusDeclined itself is distinct from StatusRejected: those record a
// MODEL's judgment and a HUMAN's ruling respectively, and this records neither — a
// deterministic filter fired. Collapsing them would feed slice 7's rules.Distill
// filter outcomes as though a model had weighed them.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/store"
)

// TestPersist_RecordsSuppressionSoItIsNotRederived is the finding. Without a row,
// the narrative's delta never shrinks and the next pass repeats the work.
func TestPersist_RecordsSuppressionSoItIsNotRederived(t *testing.T) {
	s := reconcileStore(t)
	nid := seedUntracked(t, s, codeEvent("sup:1", "work"))

	before, err := s.DeltaEvents(nid)
	require.NoError(t, err)
	require.NotEmpty(t, before, "precondition: the narrative has unexamined events")

	_, err = Persist(s, []ReconcileResult{{
		NarrativeID: nid,
		Suppressed:  []string{"comment on DEVSBX-1: no evidence of work in the delta"},
	}})
	require.NoError(t, err)

	after, err := s.DeltaEvents(nid)
	require.NoError(t, err)
	assert.Empty(t, after,
		"a recorded suppression must advance the watermark; without it the same suppression is "+
			"re-derived every pass, at full model cost, and the narratives behind this one in the "+
			"selection order are never reached")
}

// TestPersist_SuppressionRowCarriesTheReason keeps the row diagnostic. A watermark
// that says only "something happened here" would make a starved backlog and a
// correctly-quiet one indistinguishable — the same reasoning behind
// ReconcileResult.Suppressed existing at all.
func TestPersist_SuppressionRowCarriesTheReason(t *testing.T) {
	s := reconcileStore(t)
	nid := seedUntracked(t, s, codeEvent("sup:1", "work"))

	reason := "comment on DEVSBX-1: every event is a record the tracker produced about itself"

	_, err := Persist(s, []ReconcileResult{{
		NarrativeID: nid,
		Suppressed:  []string{reason},
	}})
	require.NoError(t, err)

	rows, err := s.ActionsForNarrative(nid)
	require.NoError(t, err)
	require.Len(t, rows, 1, "one suppression, one row")

	assert.Equal(t, store.StatusSuppressed, rows[0].Status)
	assert.Contains(t, rows[0].Rationale, "tracker produced about itself",
		"the reason must survive onto the row: a bare watermark cannot be audited")
}

// TestPersist_SuppressionRowIsNotAProposal is the safety property. triage reads
// ActionsByStatus(StatusProposed), so a suppression must never appear in a human's
// review queue — it is unjira's own bookkeeping, and a queue full of "we decided
// not to say anything" is how a reviewer learns to skim.
func TestPersist_SuppressionRowIsNotAProposal(t *testing.T) {
	s := reconcileStore(t)
	nid := seedUntracked(t, s, codeEvent("sup:1", "work"))

	_, err := Persist(s, []ReconcileResult{{
		NarrativeID: nid,
		Suppressed:  []string{"comment on DEVSBX-1: suppressed"},
	}})
	require.NoError(t, err)

	queue, err := s.ActionsByStatus(store.StatusProposed)
	require.NoError(t, err)
	assert.Empty(t, queue, "a suppression is bookkeeping, not something to review")
}

// TestPersist_SuppressionIsReturnedSeparatelyFromProposals pins the return
// contract. Persist's []store.ActionRow feeds the auto-commit path, and a
// suppression row must not reach it — gate.Applier would be handed a row with no
// payload to apply.
func TestPersist_SuppressionIsReturnedSeparatelyFromProposals(t *testing.T) {
	s := reconcileStore(t)
	nid := seedUntracked(t, s, codeEvent("sup:1", "work"))

	persisted, err := Persist(s, []ReconcileResult{{
		NarrativeID: nid,
		Suppressed:  []string{"comment on DEVSBX-1: suppressed"},
	}})
	require.NoError(t, err)

	assert.Empty(t, persisted,
		"Persist's return value is what the auto-commit path may apply; a suppression has no "+
			"payload and must never appear there")
}

// TestPersist_SuppressionDoesNotBlockALaterRealAction is what makes the memory a
// watermark rather than a tombstone. When new work arrives the narrative must come
// back — a remembered refusal is "nothing to say YET", not "never again".
//
// This mirrors openOrAppliedCreate, which deliberately switches on
// applied/proposed/approved and ignores declined for the same reason.
func TestPersist_SuppressionDoesNotBlockALaterRealAction(t *testing.T) {
	s := reconcileStore(t)
	nid := seedUntracked(t, s, codeEvent("sup:1", "old work"))

	_, err := Persist(s, []ReconcileResult{{
		NarrativeID: nid,
		Suppressed:  []string{"comment on DEVSBX-1: suppressed"},
	}})
	require.NoError(t, err)

	drained, err := s.DeltaEvents(nid)
	require.NoError(t, err)
	require.Empty(t, drained, "precondition: the suppression advanced the watermark")

	// New evidence arrives after the suppression.
	linkEventToNarrative(t, s, nid, codeEvent("sup:2", "new work"))

	revived, err := s.DeltaEvents(nid)
	require.NoError(t, err)
	assert.NotEmpty(t, revived,
		"new work must bring the narrative back: the row records that nothing was worth saying at "+
			"that point, not that nothing ever will be")
}

// TestPersist_ManySuppressionsOnOneNarrativeRecordOnce guards against inflating the
// queue. A narrative can suppress several drafted actions in one pass (a comment
// and a transition, say), and one row per pass is enough to advance the watermark —
// per-reason rows would multiply bookkeeping without adding information the
// rationale cannot hold.
func TestPersist_ManySuppressionsOnOneNarrativeRecordOnce(t *testing.T) {
	s := reconcileStore(t)
	nid := seedUntracked(t, s, codeEvent("sup:1", "work"))

	_, err := Persist(s, []ReconcileResult{{
		NarrativeID: nid,
		Suppressed: []string{
			"comment on DEVSBX-1: no evidence of work",
			"transition on DEVSBX-1: already at the target status",
		},
	}})
	require.NoError(t, err)

	rows, err := s.ActionsForNarrative(nid)
	require.NoError(t, err)
	assert.Len(t, rows, 1, "one pass, one watermark row, however many reasons it carries")
	assert.Contains(t, rows[0].Rationale, "no evidence of work")
	assert.Contains(t, rows[0].Rationale, "already at the target status",
		"every reason must survive, since the row is the only record the pass leaves")
}

// TestPersist_NoSuppressionWritesNothing keeps the change inert on the happy path.
// A narrative that proposed something records that proposal and nothing else.
func TestPersist_NoSuppressionWritesNothing(t *testing.T) {
	s := reconcileStore(t)
	nid := seedUntracked(t, s, codeEvent("sup:1", "work"))

	_, err := Persist(s, []ReconcileResult{{NarrativeID: nid}})
	require.NoError(t, err)

	rows, err := s.ActionsForNarrative(nid)
	require.NoError(t, err)
	assert.Empty(t, rows, "nothing suppressed, nothing proposed, nothing written")
}

// linkEventToNarrative attaches one more event to an existing narrative, so a
// narrative can gain evidence after a suppression was recorded.
func linkEventToNarrative(t *testing.T, s *store.Store, narrativeID int64, evt events.Event) {
	t.Helper()

	// A later linked_at than the suppression row's created_at is what puts the event
	// past the watermark, and both default to the wall clock at write time. The
	// event's own occurred_at is irrelevant to that comparison — DeltaEvents bounds
	// on linked_at, not on when the work happened.
	time.Sleep(2 * time.Millisecond)

	inserted, err := s.InsertEvent(evt)
	require.NoError(t, err)
	require.True(t, inserted, "fixture events must be new")

	id, err := s.EventIDByExternalID(evt.Source, evt.ExternalID)
	require.NoError(t, err)
	require.NoError(t, s.AddNarrativeEvents(narrativeID, []int64{id}))
}
