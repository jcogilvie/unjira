package store_test

// linkseq_test.go covers finding F30: every comparison that decides whether a
// narrative_events link is "newer than" something used to compare millisecond
// timestamps with a strict `>`, and two writes inside one millisecond are
// byte-identical. A watermark compared at the resolution of the clock that writes it
// is a tombstone for anything inside one tick.
//
// Six comparison sites shared the hazard, implementing four rules, each read by two
// queries that must agree (a selector and its count, or two readers of one rule):
//
//   - match watermark:     NarrativesWithoutPrimaryLink / CountNarrativesWithoutPrimaryLink
//   - reconcile watermark: NarrativesWithActionableLinks / CountNarrativesWithDelta
//   - reconcile delta:     DeltaEvents / hasUnexaminedDelta
//   - freeze rule:         EligibleEventIDs / EligibleEvents
//
// Every test here writes the timestamps EXPLICITLY — byte-identical, or deliberately
// inverted — rather than relying on the wall clock to collide. That makes them
// deterministic where the test that surfaced F30 failed about one run in thirty, and
// it proves the property directly: the display timestamps no longer bear load in
// either direction.

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite" // the old-schema fixture below opens the file directly

	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/store"
)

// farFuture and farPast are display timestamps chosen to make a timestamp comparison
// give the WRONG answer, so a test passing with them proves no timestamp was consulted.
const (
	farFuture = "9999-12-31T23:59:59.999Z"
	farPast   = "2000-01-01T00:00:00.000Z"
)

// linkNewEvent inserts one event and links it to narrativeID, returning the event id.
func linkNewEvent(t *testing.T, s *store.Store, narrativeID int64, extID string) int64 {
	t.Helper()

	e := events.NewEvent("claude_code", extID, time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC), "work")
	_, err := s.InsertEvent(e)
	require.NoError(t, err)

	id, err := s.EventIDByExternalID(e.Source, e.ExternalID)
	require.NoError(t, err)
	require.NoError(t, s.AddNarrativeEvents(narrativeID, []int64{id}))

	return id
}

// linkedAt reads one link's display timestamp.
func linkedAt(t *testing.T, s *store.Store, narrativeID, eventID int64) string {
	t.Helper()

	ts, err := s.NarrativeEventLinkedAt(narrativeID, eventID)
	require.NoError(t, err)

	return ts
}

// -- match watermark ---------------------------------------------------------------

// TestMatchWatermark_ALinkInTheSameMillisecondReadmits is F30's reproduction made
// deterministic: the examination and the later link carry byte-identical timestamps,
// and the narrative must still be re-admitted, because the link happened AFTER.
func TestMatchWatermark_ALinkInTheSameMillisecondReadmits(t *testing.T) {
	s := openStore(t)
	id := seedNarrative(t, s, "no ticket yet", time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC))

	require.NoError(t, s.RecordMatchExamined(id, "no candidate keys"))
	eventID := linkNewEvent(t, s, id, "f30:match:later")

	ts := linkedAt(t, s, id, eventID)
	require.NoError(t, s.ExecForTest(
		`UPDATE match_examinations SET examined_at = ? WHERE narrative_id = ?`, ts, id))
	examinedAt, err := s.QueryStringForTest(
		`SELECT examined_at FROM match_examinations WHERE narrative_id = ?`, id)
	require.NoError(t, err)
	require.Equal(t, ts, examinedAt, "fixture: the two timestamps are byte-identical")

	got, err := s.NarrativesWithoutPrimaryLink(10)
	require.NoError(t, err)
	require.Len(t, got, 1,
		"a link made after the examination must re-admit the narrative even inside one "+
			"millisecond: otherwise the watermark is a tombstone for anything in that tick")
	assert.Equal(t, id, got[0].ID)

	n, err := s.CountNarrativesWithoutPrimaryLink()
	require.NoError(t, err)
	assert.Equal(t, 1, n, "the count shares the selector's predicate and must agree")
}

// TestMatchWatermark_TimestampsDoNotDecide is the converse: the link happened BEFORE
// the examination, and its display timestamp claims otherwise. The sequence decides,
// so the narrative stays skipped.
func TestMatchWatermark_TimestampsDoNotDecide(t *testing.T) {
	s := openStore(t)
	id := seedNarrative(t, s, "no ticket", time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC))

	linkNewEvent(t, s, id, "f30:match:earlier")
	require.NoError(t, s.RecordMatchExamined(id, "no candidate keys"))
	require.NoError(t, s.ExecForTest(
		`UPDATE match_examinations SET examined_at = ? WHERE narrative_id = ?`, farPast, id))

	got, err := s.NarrativesWithoutPrimaryLink(10)
	require.NoError(t, err)
	assert.Empty(t, got, "examined after its only link: skipped, whatever examined_at says")

	n, err := s.CountNarrativesWithoutPrimaryLink()
	require.NoError(t, err)
	assert.Zero(t, n)
}

// TestMatchWatermark_ADeletedLinksSequenceIsNeverReused pins AUTOINCREMENT. A plain
// INTEGER PRIMARY KEY hands out max(rowid)+1, so deleting the newest link and linking
// another reissues the deleted number — which equals the examination's high-water mark
// and so reads as "not newer". Restructures delete links (UnlinkNarrativeEvents,
// UnlinkEventFromOtherNarratives), so this is reachable, not theoretical.
func TestMatchWatermark_ADeletedLinksSequenceIsNeverReused(t *testing.T) {
	s := openStore(t)
	id := seedNarrative(t, s, "no ticket", time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC))

	linkNewEvent(t, s, id, "f30:reuse:1")
	newest := linkNewEvent(t, s, id, "f30:reuse:2")
	require.NoError(t, s.RecordMatchExamined(id, "no candidate keys"))
	require.NoError(t, s.UnlinkNarrativeEvents(id, []int64{newest}))

	later := linkNewEvent(t, s, id, "f30:reuse:3")
	// Make the timestamps unable to help: they now say the examination came last.
	require.NoError(t, s.ExecForTest(
		`UPDATE match_examinations SET examined_at = ? WHERE narrative_id = ?`, farFuture, id))

	got, err := s.NarrativesWithoutPrimaryLink(10)
	require.NoError(t, err)
	require.Len(t, got, 1,
		"event %d was linked after the examination; a reused sequence number would hide it", later)
	assert.Equal(t, id, got[0].ID)
}

// -- reconcile watermark -----------------------------------------------------------

// TestReconcileWatermark_ALinkInTheSameMillisecondReadmits is the reconcile-side twin
// of the match test: same predicate shape, same hazard.
func TestReconcileWatermark_ALinkInTheSameMillisecondReadmits(t *testing.T) {
	s := openStore(t)
	id := seedActionableLinked(t, s, "DEVSBX-30", "f30:rec:first", time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC))

	require.NoError(t, s.RecordReconcileExamined(id, "delta entirely self-authored"))
	eventID := linkNewEvent(t, s, id, "f30:rec:later")

	ts := linkedAt(t, s, id, eventID)
	require.NoError(t, s.ExecForTest(
		`UPDATE reconcile_examinations SET examined_at = ? WHERE narrative_id = ?`, ts, id))

	got, err := s.NarrativesWithActionableLinks(10, reconcileRoles)
	require.NoError(t, err)
	require.Len(t, got, 1, "a link after the examination re-admits, even inside one millisecond")
	assert.Equal(t, id, got[0].ID)

	n, err := s.CountNarrativesWithDelta(reconcileRoles)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "the count shares the selector's predicate and must agree")
}

// TestReconcileWatermark_TimestampsDoNotDecide: examined after its only link, with a
// display timestamp claiming the opposite. Stays skipped.
func TestReconcileWatermark_TimestampsDoNotDecide(t *testing.T) {
	s := openStore(t)
	id := seedActionableLinked(t, s, "DEVSBX-31", "f30:rec:only", time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC))

	require.NoError(t, s.RecordReconcileExamined(id, "delta entirely self-authored"))
	require.NoError(t, s.ExecForTest(
		`UPDATE reconcile_examinations SET examined_at = ? WHERE narrative_id = ?`, farPast, id))

	got, err := s.NarrativesWithActionableLinks(10, reconcileRoles)
	require.NoError(t, err)
	assert.Empty(t, got)

	n, err := s.CountNarrativesWithDelta(reconcileRoles)
	require.NoError(t, err)
	assert.Zero(t, n)
}

// -- reconcile delta ---------------------------------------------------------------

// TestDelta_ALinkInTheSameMillisecondAsTheActionCounts: the delta is "linked since the
// narrative's last action". An event linked after the action, with a byte-identical
// timestamp, is delta — and DeltaEvents and the selector's count must both say so.
func TestDelta_ALinkInTheSameMillisecondAsTheActionCounts(t *testing.T) {
	s := openStore(t)
	id := seedActionableLinked(t, s, "DEVSBX-32", "f30:delta:first", time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC))

	actionID, err := s.InsertAction(store.ActionRow{
		NarrativeID: id, Type: "comment", IssueKey: "DEVSBX-32",
		Payload: `{"body":"considered"}`, Status: store.StatusDeclined,
	})
	require.NoError(t, err)

	eventID := linkNewEvent(t, s, id, "f30:delta:later")
	require.NoError(t, s.ExecForTest(
		`UPDATE actions SET created_at = ? WHERE id = ?`, linkedAt(t, s, id, eventID), actionID))

	delta, err := s.DeltaEvents(id)
	require.NoError(t, err)
	require.Len(t, delta, 1, "the link after the action is the delta, even inside one millisecond")
	assert.Equal(t, "f30:delta:later", delta[0].ExternalID)

	n, err := s.CountNarrativesWithDelta(reconcileRoles)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "hasUnexaminedDelta must agree with DeltaEvents")
}

// TestDelta_TimestampsDoNotDecide: the action came after every link, and its display
// timestamp claims it came first. Nothing is delta.
func TestDelta_TimestampsDoNotDecide(t *testing.T) {
	s := openStore(t)
	id := seedActionableLinked(t, s, "DEVSBX-33", "f30:delta:only", time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC))

	actionID, err := s.InsertAction(store.ActionRow{
		NarrativeID: id, Type: "comment", IssueKey: "DEVSBX-33",
		Payload: `{"body":"considered"}`, Status: store.StatusDeclined,
	})
	require.NoError(t, err)
	require.NoError(t, s.ExecForTest(`UPDATE actions SET created_at = ? WHERE id = ?`, farPast, actionID))

	delta, err := s.DeltaEvents(id)
	require.NoError(t, err)
	assert.Empty(t, delta, "every link predates the action")

	n, err := s.CountNarrativesWithDelta(reconcileRoles)
	require.NoError(t, err)
	assert.Zero(t, n)
}

// -- freeze rule -------------------------------------------------------------------

// TestFreeze_ALinkInTheSameMillisecondAsTheCommitIsEligible: the freeze rule's
// "linked after the last applied action" read executed_at, written by the same
// millisecond clock. An event linked after the commit with an identical timestamp
// must stay eligible for restructuring — a frozen event can never be moved again.
func TestFreeze_ALinkInTheSameMillisecondAsTheCommitIsEligible(t *testing.T) {
	s := openStore(t)
	nid, before := seedNarrativeWithEvents(t, s, 2)

	actionID, err := s.InsertAction(store.ActionRow{
		NarrativeID: nid, Type: "comment", IssueKey: "PROJ-1",
		Payload: `{"body":"posted"}`, Status: store.StatusProposed,
	})
	require.NoError(t, err)
	require.NoError(t, s.UpdateActionStatus(actionID, store.StatusApplied))

	after := linkNewEvent(t, s, nid, "f30:freeze:after")
	require.NoError(t, s.ExecForTest(
		`UPDATE actions SET executed_at = ? WHERE id = ?`, linkedAt(t, s, nid, after), actionID))

	ids, err := s.EligibleEventIDs(nid)
	require.NoError(t, err)
	assert.Equal(t, []int64{after}, ids, "linked after the commit: eligible, even inside one millisecond")
	assert.NotContains(t, ids, before[0])

	evts, err := s.EligibleEvents(nid)
	require.NoError(t, err)
	require.Len(t, evts, 1, "EligibleEvents must agree with EligibleEventIDs")
	assert.Equal(t, "f30:freeze:after", evts[0].ExternalID)
}

// TestFreeze_TimestampsDoNotDecide: links made before the commit stay frozen even when
// their display timestamps claim to be later than it.
func TestFreeze_TimestampsDoNotDecide(t *testing.T) {
	s := openStore(t)
	nid, _ := seedNarrativeWithEvents(t, s, 2)

	actionID, err := s.InsertAction(store.ActionRow{
		NarrativeID: nid, Type: "comment", IssueKey: "PROJ-1",
		Payload: `{"body":"posted"}`, Status: store.StatusProposed,
	})
	require.NoError(t, err)
	require.NoError(t, s.UpdateActionStatus(actionID, store.StatusApplied))
	require.NoError(t, s.ExecForTest(`UPDATE narrative_events SET linked_at = ? WHERE narrative_id = ?`, farFuture, nid))

	ids, err := s.EligibleEventIDs(nid)
	require.NoError(t, err)
	assert.Empty(t, ids, "both links predate the commit: frozen, whatever linked_at says")

	evts, err := s.EligibleEvents(nid)
	require.NoError(t, err)
	assert.Empty(t, evts)
}

// -- relink ------------------------------------------------------------------------

// TestRelink_SameNarrativeKeepsTheLinksPosition pins what correlator.relinkEvents does
// to an event already linked to the narrative it is being linked to:
// UnlinkEventFromOtherNarratives deletes nothing (it spares keepNarrativeID), and
// AddNarrativeEvents is INSERT OR IGNORE, so the existing row — its sequence position
// AND its display linked_at — survives untouched.
//
// Load-bearing for the freeze rule. If a re-link replaced the row it would get a new
// sequence position, read as newly linked, and UN-FREEZE an event a posted comment
// already describes.
func TestRelink_SameNarrativeKeepsTheLinksPosition(t *testing.T) {
	s := openStore(t)
	nid, ids := seedNarrativeWithEvents(t, s, 2)
	beforeTS := linkedAt(t, s, nid, ids[0])

	actionID, err := s.InsertAction(store.ActionRow{
		NarrativeID: nid, Type: "comment", IssueKey: "PROJ-1",
		Payload: `{"body":"posted"}`, Status: store.StatusProposed,
	})
	require.NoError(t, err)
	require.NoError(t, s.UpdateActionStatus(actionID, store.StatusApplied))

	// relinkEvents' exact two calls, in its transaction shape.
	require.NoError(t, s.WithTx(func(tx *store.Tx) error {
		for _, id := range ids {
			if err := tx.UnlinkEventFromOtherNarratives(nid, id); err != nil {
				return err
			}
		}

		return tx.AddNarrativeEvents(nid, ids)
	}))

	eligible, err := s.EligibleEventIDs(nid)
	require.NoError(t, err)
	assert.Empty(t, eligible, "a re-link must not un-freeze a committed event")
	assert.Equal(t, beforeTS, linkedAt(t, s, nid, ids[0]), "the display timestamp is kept as well")
}

// TestRelink_MovingToAnotherNarrativeIsANewLink pins the other half: an event moved
// between narratives is a NEW link on the destination — as it always was under
// linked_at, where the destination row got a fresh timestamp — so it is eligible there
// and re-admits the destination past its match watermark.
func TestRelink_MovingToAnotherNarrativeIsANewLink(t *testing.T) {
	s := openStore(t)
	source, ids := seedNarrativeWithEvents(t, s, 1)
	dest := seedNarrative(t, s, "destination", time.Date(2026, 9, 2, 9, 0, 0, 0, time.UTC))
	require.NoError(t, s.RecordMatchExamined(dest, "no candidate keys"))
	require.NoError(t, s.ExecForTest(
		`UPDATE match_examinations SET examined_at = ? WHERE narrative_id = ?`, farFuture, dest))

	require.NoError(t, s.WithTx(func(tx *store.Tx) error {
		if err := tx.UnlinkEventFromOtherNarratives(dest, ids[0]); err != nil {
			return err
		}

		return tx.AddNarrativeEvents(dest, ids)
	}))

	n, err := s.NarrativeEventCount(source)
	require.NoError(t, err)
	assert.Zero(t, n, "moved, not copied")

	got, err := s.NarrativesWithoutPrimaryLink(10)
	require.NoError(t, err)
	gotIDs := make([]int64, 0, len(got))
	for _, r := range got {
		gotIDs = append(gotIDs, r.ID)
	}
	assert.Contains(t, gotIDs, dest, "a moved-in event is linked after the destination's examination")
}

// -- old stores --------------------------------------------------------------------

// TestOpen_RefusesAStoreThatPredatesTheLinkSequence: this package has no migrations
// (every statement is CREATE TABLE IF NOT EXISTS), so a database created before F30
// keeps its old narrative_events table. Run against it, every watermark query would
// fail mid-pass with "no such column" — or, worse, a future query might not reference
// the column and run with timestamp semantics. Open must refuse up front and name the
// fix.
func TestOpen_RefusesAStoreThatPredatesTheLinkSequence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")

	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = db.Exec(`
CREATE TABLE narrative_events (
    narrative_id INTEGER NOT NULL,
    event_id     INTEGER NOT NULL,
    linked_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (narrative_id, event_id)
);`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	s, err := store.Open(path)
	if s != nil {
		_ = s.Close()
	}

	require.Error(t, err, "an old store must not open with silently different semantics")
	require.ErrorContains(t, err, path, "names which database")
	require.ErrorContains(t, err, "narrative_events.link_seq", "names what is missing")
	require.ErrorContains(t, err, "delete the database and re-collect", "names the fix")
}

// TestOpen_RefusingAnOldStoreLeavesItUntouched: the refusal must happen BEFORE any
// schema statement runs. CREATE TABLE IF NOT EXISTS is not harmless on an old store:
// it creates whichever tables the old build never had, in the NEW shape. That breaks
// the README's escape hatch ("run learn on the old build first"), since the old
// build then meets a new-shaped table it cannot use, and it shortens the error,
// because a table created in the new shape no longer reports its column missing.
// Found by opening a copy of a real pre-F30 store: the refused open had added
// reconcile_examinations.
func TestOpen_RefusingAnOldStoreLeavesItUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")

	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = db.Exec(`
CREATE TABLE narrative_events (
    narrative_id INTEGER NOT NULL,
    event_id     INTEGER NOT NULL,
    linked_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (narrative_id, event_id)
);`)
	require.NoError(t, err)

	tables := func() []string {
		rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
		require.NoError(t, err)
		defer func() { _ = rows.Close() }()

		var names []string
		for rows.Next() {
			var n string
			require.NoError(t, rows.Scan(&n))
			names = append(names, n)
		}
		require.NoError(t, rows.Err())

		return names
	}
	before := tables()

	s, err := store.Open(path)
	if s != nil {
		_ = s.Close()
	}
	require.Error(t, err)

	assert.Equal(t, before, tables(), "a refused open must not create any table")
	require.NoError(t, db.Close())
}

// TestOpen_AFreshStoreOpensTwice: the schema check must pass on a store this build
// created, including on reopen.
func TestOpen_AFreshStoreOpensTwice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.db")

	s, err := store.Open(path)
	require.NoError(t, err)
	require.NoError(t, s.Close())

	s, err = store.Open(path)
	require.NoError(t, err)
	require.NoError(t, s.Close())
}
