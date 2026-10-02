package store_test

// linkkind_test.go covers member and context links
// (docs/superpowers/specs/2026-10-02-shared-context-design.md): the schema's
// one-member-home invariant, the two write operations that replace relinkEvents, and
// the property every downstream reader depends on — a context link is invisible to
// every delta, watermark and member reader.

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/store"
)

// insertBareEvent inserts one event, linked to nothing, returning its id.
func insertBareEvent(t *testing.T, s *store.Store, extID string, at time.Time) int64 {
	t.Helper()

	_, err := s.InsertEvent(events.NewEvent("claude_code", extID, at, "work "+extID))
	require.NoError(t, err)

	id, err := s.EventIDByExternalID("claude_code", extID)
	require.NoError(t, err)

	return id
}

// commitNarrative records an applied action on nid, freezing every link it holds now.
func commitNarrative(t *testing.T, s *store.Store, nid int64) {
	t.Helper()

	id, err := s.InsertAction(store.ActionRow{
		NarrativeID: nid, Type: "comment", IssueKey: "PROJ-1",
		Payload: `{"body":"posted"}`, Status: store.StatusProposed,
	})
	require.NoError(t, err)
	require.NoError(t, s.UpdateActionStatus(id, store.StatusApplied))
}

func link(t *testing.T, s *store.Store, nid, eid int64) store.NarrativeLink {
	t.Helper()

	l, err := s.NarrativeEventLink(nid, eid)
	require.NoError(t, err)

	return l
}

func moveMember(t *testing.T, s *store.Store, nid, eid int64, confidence float64) error {
	t.Helper()

	return s.WithTx(func(tx *store.Tx) error { return tx.MoveMember(nid, eid, confidence, store.PlacedByModel) })
}

// -- old stores --------------------------------------------------------------------

// TestOpen_RefusesAStoreThatPredatesLinkKinds: a store with the link sequence but no
// link kind would otherwise open and fail mid-pass on "no such column: kind" — or, for
// any query that does not mention the column, read every link as a member. Refused
// BEFORE any DDL, on F30's precedent, naming the change and the fix.
func TestOpen_RefusesAStoreThatPredatesLinkKinds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pre-kinds.db")

	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = db.Exec(`
CREATE TABLE narrative_events (
    link_seq     INTEGER PRIMARY KEY AUTOINCREMENT,
    narrative_id INTEGER NOT NULL,
    event_id     INTEGER NOT NULL,
    linked_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (narrative_id, event_id)
);`)
	require.NoError(t, err)

	tablesBefore := countTables(t, db)

	s, err := store.Open(path)
	if s != nil {
		_ = s.Close()
	}

	require.Error(t, err)
	require.ErrorContains(t, err, path)
	require.ErrorContains(t, err, "narrative_events.kind")
	require.ErrorContains(t, err, "narrative_events.member_confidence")
	require.ErrorContains(t, err, "2026-10-02-shared-context-design.md", "names the change")
	require.ErrorContains(t, err, "rename the database to a backup")
	require.ErrorContains(t, err, "re-collect")
	assert.NotContains(t, err.Error(), "link_seq", "a store that HAS the link sequence is not blamed for lacking it")
	assert.Equal(t, tablesBefore, countTables(t, db), "a refused open must not create any table")
	require.NoError(t, db.Close())
}

// TestOpen_RefusesAStoreThatPredatesMemberPlacement: a store with link kinds but no
// member_placement would fail mid-pass on the first member insert. Refused before any
// DDL, naming only the column it lacks.
func TestOpen_RefusesAStoreThatPredatesMemberPlacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pre-placement.db")

	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = db.Exec(`
CREATE TABLE narrative_events (
    link_seq     INTEGER PRIMARY KEY AUTOINCREMENT,
    narrative_id INTEGER NOT NULL,
    event_id     INTEGER NOT NULL,
    linked_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    kind         TEXT NOT NULL,
    member_confidence REAL,
    UNIQUE (narrative_id, event_id)
);`)
	require.NoError(t, err)

	s, err := store.Open(path)
	if s != nil {
		_ = s.Close()
	}

	require.Error(t, err)
	require.ErrorContains(t, err, "narrative_events.member_placement")
	require.ErrorContains(t, err, "re-collect")
	assert.NotContains(t, err.Error(), "narrative_events.kind", "a store that HAS kinds is not blamed for lacking them")
	require.NoError(t, db.Close())
}

func countTables(t *testing.T, db *sql.DB) int {
	t.Helper()

	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table'`).Scan(&n))

	return n
}

// -- schema ------------------------------------------------------------------------

// TestSchema_EnforcesOneMemberHomeAndKindShape: the invariant is the database's, not a
// convention. Each statement here is one a buggy writer could issue.
func TestSchema_EnforcesOneMemberHomeAndKindShape(t *testing.T) {
	base := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		stmt string
	}{
		{"a second member link for one event", `INSERT INTO narrative_events (narrative_id, event_id, kind, member_confidence, member_placement) VALUES (?2, ?3, 'member', 0.5, 'model')`},
		{"a link with no kind", `INSERT INTO narrative_events (narrative_id, event_id) VALUES (?2, ?3)`},
		{"an unknown kind", `INSERT INTO narrative_events (narrative_id, event_id, kind) VALUES (?2, ?3, 'supporting')`},
		{"a member with no confidence", `INSERT INTO narrative_events (narrative_id, event_id, kind, member_placement) VALUES (?2, ?4, 'member', 'model')`},
		{"a member confidence above 1", `INSERT INTO narrative_events (narrative_id, event_id, kind, member_confidence, member_placement) VALUES (?2, ?4, 'member', 1.5, 'model')`},
		{"a context link with a confidence", `INSERT INTO narrative_events (narrative_id, event_id, kind, member_confidence) VALUES (?2, ?3, 'context', 0.5)`},
		// member_placement mirrors member_confidence: required on a member, absent on
		// context, and the IS NOT NULL is what makes "required" true (design-notes #44).
		{"a member with no placement", `INSERT INTO narrative_events (narrative_id, event_id, kind, member_confidence) VALUES (?2, ?4, 'member', 0.5)`},
		{"an unknown placement", `INSERT INTO narrative_events (narrative_id, event_id, kind, member_confidence, member_placement) VALUES (?2, ?4, 'member', 0.5, 'guess')`},
		{"a context link with a placement", `INSERT INTO narrative_events (narrative_id, event_id, kind, member_placement) VALUES (?2, ?3, 'context', 'model')`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := openStore(t)
			a := seedNarrative(t, s, "a", base)
			b := seedNarrative(t, s, "b", base)
			e := insertBareEvent(t, s, "e", base)
			f := insertBareEvent(t, s, "f", base)
			require.NoError(t, s.LinkMembers(a, []int64{e}, 0.9))

			err := s.ExecForTest(tt.stmt, a, b, e, f)

			require.Error(t, err, "the schema must reject %s", tt.name)
		})
	}
}

// -- MoveMember --------------------------------------------------------------------

func TestMoveMember_MovesAnEligibleMemberWithANewPosition(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	a := seedNarrative(t, s, "a", base)
	b := seedNarrative(t, s, "b", base)
	e := insertBareEvent(t, s, "e", base)
	require.NoError(t, s.LinkMembers(a, []int64{e}, 0.9))
	before := link(t, s, a, e)

	require.NoError(t, moveMember(t, s, b, e, 0.7))

	_, err := s.NarrativeEventLink(a, e)
	require.ErrorIs(t, err, sql.ErrNoRows, "moved, not copied")
	after := link(t, s, b, e)
	assert.Equal(t, store.LinkMember, after.Kind)
	assert.Greater(t, after.LinkSeq, before.LinkSeq, "a move is a new link on the destination")
	require.NotNil(t, after.MemberConfidence)
	assert.InDelta(t, 0.7, *after.MemberConfidence, 1e-9)
}

// TestMoveMember_RecordsWhoPlacedTheMember: a member link says how it was placed, so a
// reader can tell the model's stated confidence from a reviewer's ruling or an exact
// pull-request identity, all three of which can carry 1.0. Calibrating
// correlator.member_confidence_floor against reviewer rulings is the reader that needs
// it: an identity placement is not the model being right. A context link carries none.
func TestMoveMember_RecordsWhoPlacedTheMember(t *testing.T) {
	for _, by := range []store.MemberPlacement{store.PlacedByModel, store.PlacedByIdentity, store.PlacedByReviewer} {
		t.Run(string(by), func(t *testing.T) {
			s := openStore(t)
			base := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
			a := seedNarrative(t, s, "a", base)
			b := seedNarrative(t, s, "b", base)
			e := insertBareEvent(t, s, "e", base)

			require.NoError(t, s.WithTx(func(tx *store.Tx) error { return tx.MoveMember(a, e, 1.0, by) }))
			require.NoError(t, s.LinkContext(b, []int64{e}))

			assert.Equal(t, by, link(t, s, a, e).Placement)
			assert.Empty(t, link(t, s, b, e).Placement, "a context link places no member")
		})
	}
}

// TestMoveMember_RefusesAFrozenMember: a frozen member is never numbered and never on
// a restructure's eligible list, so a move that finds one is a bug. It must fail
// loudly and change nothing, or committed work is silently reattributed (#13).
func TestMoveMember_RefusesAFrozenMember(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	a := seedNarrative(t, s, "a", base)
	b := seedNarrative(t, s, "b", base)
	e := insertBareEvent(t, s, "e", base)
	require.NoError(t, s.LinkMembers(a, []int64{e}, 0.9))
	commitNarrative(t, s, a)

	err := moveMember(t, s, b, e, 0.7)

	require.Error(t, err)
	require.ErrorContains(t, err, "frozen")
	assert.Equal(t, store.LinkMember, link(t, s, a, e).Kind, "the frozen member stays where it was")
	_, err = s.NarrativeEventLink(b, e)
	require.ErrorIs(t, err, sql.ErrNoRows)
}

// TestMoveMember_UpgradeIsNewWorkInTheDelta is the spec's "an upgraded link is in the
// delta". B has held E as background since before B's last action; the model now says
// E is B's work. Keeping the context row's position would put it below the action's
// created_link_seq, and the reconciler would never draft about it.
func TestMoveMember_UpgradeIsNewWorkInTheDelta(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	a := seedNarrative(t, s, "a", base)
	b := seedNarrative(t, s, "b", base)
	e := insertBareEvent(t, s, "e", base)
	require.NoError(t, s.LinkMembers(a, []int64{e}, 0.9))
	require.NoError(t, s.LinkContext(b, []int64{e}))
	contextSeq := link(t, s, b, e).LinkSeq
	_, err := s.InsertAction(store.ActionRow{
		NarrativeID: b, Type: "comment", IssueKey: "PROJ-2",
		Payload: `{"body":"drafted before"}`, Status: store.StatusProposed,
	})
	require.NoError(t, err)

	delta, err := s.DeltaEvents(b)
	require.NoError(t, err)
	require.Empty(t, delta, "precondition: context is never delta")

	require.NoError(t, moveMember(t, s, b, e, 0.8))

	upgraded := link(t, s, b, e)
	assert.Equal(t, store.LinkMember, upgraded.Kind)
	assert.Greater(t, upgraded.LinkSeq, contextSeq, "an upgrade issues a new position")
	delta, err = s.DeltaEvents(b)
	require.NoError(t, err)
	require.Len(t, delta, 1, "background that became this narrative's work today is new work today")
	assert.Equal(t, "e", delta[0].ExternalID)
	_, err = s.NarrativeEventLink(a, e)
	require.ErrorIs(t, err, sql.ErrNoRows, "and its old member home gave it up")
}

func TestMoveMember_LeavesOtherNarrativesContextLinksAlone(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	a := seedNarrative(t, s, "a", base)
	b := seedNarrative(t, s, "b", base)
	c := seedNarrative(t, s, "c", base)
	e := insertBareEvent(t, s, "e", base)
	require.NoError(t, s.LinkMembers(a, []int64{e}, 0.9))
	require.NoError(t, s.LinkContext(c, []int64{e}))
	contextBefore := link(t, s, c, e)

	require.NoError(t, moveMember(t, s, b, e, 0.6))

	assert.Equal(t, contextBefore, link(t, s, c, e), "a move touches member links only")
}

// -- AddContext --------------------------------------------------------------------

func TestAddContext_NeverDeletesAndMemberWins(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	a := seedNarrative(t, s, "a", base)
	b := seedNarrative(t, s, "b", base)
	e := insertBareEvent(t, s, "e", base)
	require.NoError(t, s.LinkMembers(a, []int64{e}, 0.9))
	memberBefore := link(t, s, a, e)

	var inserted [3]bool
	require.NoError(t, s.WithTx(func(tx *store.Tx) error {
		var err error
		if inserted[0], err = tx.AddContext(b, e); err != nil {
			return err
		}
		if inserted[1], err = tx.AddContext(b, e); err != nil {
			return err
		}
		inserted[2], err = tx.AddContext(a, e)

		return err
	}))

	assert.Equal(t, [3]bool{true, false, false}, inserted)
	assert.Equal(t, memberBefore, link(t, s, a, e), "a member link wins over context, untouched")
	assert.Equal(t, store.LinkContext, link(t, s, b, e).Kind)
	assert.Nil(t, link(t, s, b, e).MemberConfidence, "context attributes nothing")
}

func TestEventsWithoutMemberHome_FindsAContextOnlyEvent(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	a := seedNarrative(t, s, "a", base)
	housed := insertBareEvent(t, s, "housed", base)
	orphan := insertBareEvent(t, s, "orphan", base)
	require.NoError(t, s.LinkMembers(a, []int64{housed}, 0.9))
	require.NoError(t, s.LinkContext(a, []int64{orphan}))

	var got []int64
	require.NoError(t, s.WithTx(func(tx *store.Tx) error {
		var err error
		got, err = tx.EventsWithoutMemberHome([]int64{housed, orphan})

		return err
	}))

	assert.Equal(t, []int64{orphan}, got)
}

// -- readers -----------------------------------------------------------------------

// TestContextLink_ChangesNeitherBacklogCount is the spec's §7 property: adding a
// context link re-admits nothing, so matching and the reconciler add zero calls. Both
// narratives are examined first, so a context link that counted as "linked since"
// would re-open them.
func TestContextLink_ChangesNeitherBacklogCount(t *testing.T) {
	s := openStore(t)
	unmatched := seedNarrative(t, s, "unmatched", time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC))
	require.NoError(t, s.RecordMatchExamined(unmatched, "no candidate keys"))
	linked := seedLinkedWithEvent(t, s, "PROJ-9", "linked-work")
	_, err := s.InsertAction(store.ActionRow{
		NarrativeID: linked, Type: "comment", IssueKey: "PROJ-9",
		Payload: `{"body":"drafted"}`, Status: store.StatusProposed,
	})
	require.NoError(t, err)
	shared := insertBareEvent(t, s, "shared-root-segment", time.Date(2026, 9, 20, 9, 30, 0, 0, time.UTC))
	other := seedNarrative(t, s, "home", time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC))
	require.NoError(t, s.LinkMembers(other, []int64{shared}, 0.9))

	withoutPrimaryBefore, err := s.CountNarrativesWithoutPrimaryLink()
	require.NoError(t, err)
	withDeltaBefore, err := s.CountNarrativesWithDelta(reconcileRoles)
	require.NoError(t, err)

	require.NoError(t, s.LinkContext(unmatched, []int64{shared}))
	require.NoError(t, s.LinkContext(linked, []int64{shared}))

	withoutPrimaryAfter, err := s.CountNarrativesWithoutPrimaryLink()
	require.NoError(t, err)
	withDeltaAfter, err := s.CountNarrativesWithDelta(reconcileRoles)
	require.NoError(t, err)
	assert.Equal(t, withoutPrimaryBefore, withoutPrimaryAfter, "a context link must not re-admit a narrative to matching")
	assert.Equal(t, withDeltaBefore, withDeltaAfter, "a context link must not re-admit a narrative to reconcile")

	// The counts' selectors must agree with them (F10's failure mode).
	actionable, err := s.NarrativesWithActionableLinks(10, reconcileRoles)
	require.NoError(t, err)
	for _, n := range actionable {
		assert.NotEqual(t, linked, n.ID, "the selector must not pick up what the count excluded")
	}
}

// TestMemberReaders_NeverSeeContext: every accessor a reconciler, matching or
// clustering path reads returns members only; ContextEvents is the one way in.
func TestMemberReaders_NeverSeeContext(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	home := seedNarrative(t, s, "home", base)
	reader := seedNarrative(t, s, "reader", base)
	own := insertBareEvent(t, s, "own", base.Add(time.Minute))
	shared := insertBareEvent(t, s, "shared", base.Add(2*time.Minute))
	require.NoError(t, s.LinkMembers(reader, []int64{own}, 0.9))
	require.NoError(t, s.LinkMembers(home, []int64{shared}, 0.9))
	require.NoError(t, s.LinkContext(reader, []int64{shared}))

	ids := func(evts []events.Event) []string {
		out := make([]string, 0, len(evts))
		for _, e := range evts {
			out = append(out, e.ExternalID)
		}

		return out
	}

	delta, err := s.DeltaEvents(reader)
	require.NoError(t, err)
	assert.Equal(t, []string{"own"}, ids(delta), "DeltaEvents")

	all, err := s.AllMemberEvents(reader)
	require.NoError(t, err)
	assert.Equal(t, []string{"own"}, ids(all), "AllMemberEvents")

	eligible, err := s.EligibleMemberEvents(reader)
	require.NoError(t, err)
	assert.Equal(t, []string{"own"}, ids(eligible), "EligibleMemberEvents")

	tail, err := s.MemberEventsAfterBoundary(reader)
	require.NoError(t, err)
	assert.Equal(t, []string{"own"}, ids(tail), "MemberEventsAfterBoundary")

	eligibleIDs, err := s.EligibleMemberEventIDs(reader)
	require.NoError(t, err)
	assert.Equal(t, []int64{own}, eligibleIDs, "EligibleMemberEventIDs")

	contextIDs, err := s.EligibleContextEventIDs(reader)
	require.NoError(t, err)
	assert.Equal(t, []int64{shared}, contextIDs, "EligibleContextEventIDs")

	members, err := s.MemberEventCount(reader)
	require.NoError(t, err)
	assert.Equal(t, 1, members, "MemberEventCount")

	ctx, err := s.ContextEvents(reader)
	require.NoError(t, err)
	assert.Equal(t, []string{"shared"}, ids(ctx), "ContextEvents is the one way to read context")

	ctxTail, err := s.ContextEventsAfterBoundary(reader)
	require.NoError(t, err)
	assert.Equal(t, []string{"shared"}, ids(ctxTail))
}

// TestUnlinkedEventsInRange_AsksForAMemberHome: the candidate question is "has this
// event a home", so an event whose only link is context is still a candidate. The
// invariant forbids that state at Persist's commit; this pins that the selector asks
// the right question regardless (#28).
func TestUnlinkedEventsInRange_AsksForAMemberHome(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	n := seedNarrative(t, s, "n", base)
	housed := insertBareEvent(t, s, "housed", base)
	contextOnly := insertBareEvent(t, s, "context-only", base.Add(time.Minute))
	require.NoError(t, s.LinkMembers(n, []int64{housed}, 0.9))
	require.NoError(t, s.LinkContext(n, []int64{contextOnly}))

	got, err := s.UnlinkedEventsInRange(base, base.Add(time.Hour))

	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "context-only", got[0].ExternalID)
}

// TestContextEventsAfterBoundary_IsBoundedLikeMembers: §7 relies on the compaction
// boundary bounding context accumulation, which only holds if the boundary filters by
// event position whatever the link's kind.
func TestContextEventsAfterBoundary_IsBoundedLikeMembers(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	home := seedNarrative(t, s, "home", base)
	reader := seedNarrative(t, s, "reader", base)
	old := insertBareEvent(t, s, "old", base)
	recent := insertBareEvent(t, s, "recent", base.Add(time.Hour))
	require.NoError(t, s.LinkMembers(home, []int64{old, recent}, 0.9))
	require.NoError(t, s.LinkContext(reader, []int64{old, recent}))
	require.NoError(t, s.SetCompactionBoundary(reader, base, old, "recap"))

	tail, err := s.ContextEventsAfterBoundary(reader)
	require.NoError(t, err)
	require.Len(t, tail, 1)
	assert.Equal(t, "recent", tail[0].ExternalID)

	all, err := s.ContextEvents(reader)
	require.NoError(t, err)
	assert.Len(t, all, 2, "ContextEvents ignores the boundary, for reporting")
}

func TestMembersBelowConfidence(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	n := seedNarrative(t, s, "n", base)
	sure := insertBareEvent(t, s, "sure", base)
	doubtful := insertBareEvent(t, s, "doubtful", base.Add(time.Minute))
	require.NoError(t, s.LinkMembers(n, []int64{sure}, 0.9))
	require.NoError(t, s.LinkMembers(n, []int64{doubtful}, 0.4))

	got, err := s.MembersBelowConfidence(n, 0.5)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "doubtful", got[0].Event.ExternalID)
	assert.InDelta(t, 0.4, got[0].Confidence, 1e-9)

	off, err := s.MembersBelowConfidence(n, 0)
	require.NoError(t, err)
	assert.Empty(t, off, "a floor of 0 is off")
}
