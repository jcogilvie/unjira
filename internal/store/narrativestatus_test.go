package store_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/store"
)

func TestSetNarrativeStatus_RecordsTheStatus(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC)

	id, err := s.InsertNarrative(base, base.Add(time.Hour), "t", "s")
	require.NoError(t, err)

	require.NoError(t, s.SetNarrativeStatus(id, store.StatusSplit))

	got, err := s.GetNarrative(id)
	require.NoError(t, err)
	assert.Equal(t, store.StatusSplit, got.Status)
}

// TestSetNarrativeStatus_MissingNarrativeErrors: a no-op UPDATE would hide a
// caller naming a narrative that does not exist.
func TestSetNarrativeStatus_MissingNarrativeErrors(t *testing.T) {
	s := openStore(t)

	err := s.SetNarrativeStatus(999999, store.StatusSplit)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no such narrative")
}

// TestNarrativesOverlapping_ExcludesSplitNarratives is the reason
// SetNarrativeStatus exists. An emptied narrative that stays selectable is
// rendered into every subsequent Cluster prompt as an existing narrative holding
// zero events — the model asked to reason about a story whose substance moved.
func TestNarrativesOverlapping_ExcludesSplitNarratives(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC)

	live, err := s.InsertNarrative(base, base.Add(time.Hour), "still live", "s")
	require.NoError(t, err)
	gone, err := s.InsertNarrative(base, base.Add(time.Hour), "was split apart", "s")
	require.NoError(t, err)

	before, err := s.NarrativesOverlapping(base, base.Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, before, 2, "precondition: both are selected while both are open")

	require.NoError(t, s.SetNarrativeStatus(gone, store.StatusSplit))

	after, err := s.NarrativesOverlapping(base, base.Add(time.Hour))

	require.NoError(t, err)
	require.Len(t, after, 1, "a split narrative must not be fed to Cluster as context")
	assert.Equal(t, live, after[0].ID)
}

// TestNarrativesOverlapping_KeepsUnknownStatuses: only 'split' is excluded, not
// "anything not open". A status this query has never seen is more likely a new
// lifecycle state than a reason to hide work from clustering.
func TestNarrativesOverlapping_KeepsUnknownStatuses(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC)

	id, err := s.InsertNarrative(base, base.Add(time.Hour), "some future state", "s")
	require.NoError(t, err)
	require.NoError(t, s.SetNarrativeStatus(id, "archived-by-some-future-slice"))

	got, err := s.NarrativesOverlapping(base, base.Add(time.Hour))

	require.NoError(t, err)
	assert.Len(t, got, 1,
		"an unrecognized status must not silently drop a narrative from clustering")
}

// TestSetNarrativeStatus_PreservesTheRowAndItsActions: the row is kept rather than
// deleted precisely because actions still reference it — including an APPLIED
// action, whose record of "unjira posted this about this work" must survive.
func TestSetNarrativeStatus_PreservesTheRowAndItsActions(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC)

	id, err := s.InsertNarrative(base, base.Add(time.Hour), "had a comment posted", "s")
	require.NoError(t, err)

	aid, err := s.InsertAction(store.ActionRow{
		NarrativeID: id, Type: "comment", IssueKey: "PROJ-1",
		Payload: `{"body":"posted"}`, Status: "proposed",
	})
	require.NoError(t, err)
	require.NoError(t, s.UpdateActionStatus(aid, "applied"))

	require.NoError(t, s.SetNarrativeStatus(id, store.StatusSplit))

	got, err := s.GetNarrative(id)
	require.NoError(t, err)
	assert.Equal(t, store.StatusSplit, got.Status, "the row survives")

	actions, err := s.ActionsForNarrative(id)
	require.NoError(t, err)
	require.Len(t, actions, 1)
	assert.Equal(t, "applied", actions[0].Status,
		"the record of what unjira actually posted must outlive the restructure")
}

func markSplitIfEmptied(t *testing.T, s *store.Store, nid int64) (contextLinks int, emptied bool, err error) {
	t.Helper()

	err = s.WithTx(func(tx *store.Tx) error {
		var inner error
		contextLinks, emptied, inner = tx.MarkSplitIfEmptied(nid)

		return inner
	})

	return contextLinks, emptied, err
}

// TestMarkSplitIfEmptied_MarksANarrativeHoldingOnlyBackground: a narrative left with
// context links and no member holds no work (F40's shape). It is marked split and its
// context links are deleted, which loses nothing, because each such event keeps its
// member home elsewhere. Its issue link and its actions stay: the row is kept for them.
func TestMarkSplitIfEmptied_MarksANarrativeHoldingOnlyBackground(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	home := seedNarrative(t, s, "home", base)
	emptied := seedNarrative(t, s, "emptied", base)
	bg := insertBareEvent(t, s, "bg", base)
	require.NoError(t, s.LinkMembers(home, []int64{bg}, 0.9))
	require.NoError(t, s.LinkContext(emptied, []int64{bg}))
	require.NoError(t, s.AddNarrativeIssues(emptied, []store.NarrativeIssue{{
		IssueKey: "PROJ-1", Role: store.RolePrimary, Provenance: "branch", Confidence: 0.9,
	}}))
	_, err := s.InsertAction(store.ActionRow{
		NarrativeID: emptied, Type: "comment", IssueKey: "PROJ-1",
		Payload: `{"body":"drafted"}`, Status: store.StatusProposed,
	})
	require.NoError(t, err)

	contextLinks, marked, err := markSplitIfEmptied(t, s, emptied)

	require.NoError(t, err)
	assert.True(t, marked)
	assert.Equal(t, 1, contextLinks)
	row, err := s.GetNarrative(emptied)
	require.NoError(t, err)
	assert.Equal(t, store.StatusSplit, row.Status)
	_, err = s.NarrativeEventLink(emptied, bg)
	require.ErrorIs(t, err, sql.ErrNoRows, "the context link is deleted")
	assert.Equal(t, store.LinkMember, link(t, s, home, bg).Kind, "and the event keeps its member home")
	issues, err := s.NarrativeIssues(emptied)
	require.NoError(t, err)
	assert.Len(t, issues, 1, "the issue link is kept on the row")
	actions, err := s.ActionsForNarrative(emptied)
	require.NoError(t, err)
	assert.Len(t, actions, 1, "and so are its actions")
}

// TestMarkSplitIfEmptied_LeavesANarrativeThatHoldsWork: one member is work, so the
// narrative stays open with its context links, which are counted.
func TestMarkSplitIfEmptied_LeavesANarrativeThatHoldsWork(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	home := seedNarrative(t, s, "home", base)
	live := seedNarrative(t, s, "live", base)
	bg := insertBareEvent(t, s, "bg", base)
	own := insertBareEvent(t, s, "own", base)
	require.NoError(t, s.LinkMembers(home, []int64{bg}, 0.9))
	require.NoError(t, s.LinkMembers(live, []int64{own}, 0.9))
	require.NoError(t, s.LinkContext(live, []int64{bg}))

	contextLinks, marked, err := markSplitIfEmptied(t, s, live)

	require.NoError(t, err)
	assert.False(t, marked)
	assert.Equal(t, 1, contextLinks)
	row, err := s.GetNarrative(live)
	require.NoError(t, err)
	assert.Equal(t, store.StatusOpen, row.Status)
	assert.Equal(t, store.LinkContext, link(t, s, live, bg).Kind, "its background stays")
}

// TestMarkSplitIfEmptied_RefusesAnUnknownStatus: overwriting a status this code has
// never seen would silently discard a lifecycle state somebody added. Loud instead, so
// whoever adds one decides what emptying it means.
func TestMarkSplitIfEmptied_RefusesAnUnknownStatus(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	id := seedNarrative(t, s, "future", base)
	require.NoError(t, s.SetNarrativeStatus(id, "archived-by-some-future-slice"))

	_, _, err := markSplitIfEmptied(t, s, id)

	require.ErrorContains(t, err, `status "archived-by-some-future-slice"`)
	row, err := s.GetNarrative(id)
	require.NoError(t, err)
	assert.Equal(t, "archived-by-some-future-slice", row.Status, "nothing was overwritten")
}

// TestMemberHolders_ReturnsEachHolderOnce: the narratives a pass is about to move
// members off, which are the only ones the pass can empty.
func TestMemberHolders_ReturnsEachHolderOnce(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	a := seedNarrative(t, s, "a", base)
	b := seedNarrative(t, s, "b", base)
	a1 := insertBareEvent(t, s, "a1", base)
	a2 := insertBareEvent(t, s, "a2", base)
	b1 := insertBareEvent(t, s, "b1", base)
	shared := insertBareEvent(t, s, "shared", base)
	unlinked := insertBareEvent(t, s, "unlinked", base)
	require.NoError(t, s.LinkMembers(a, []int64{a1, a2, shared}, 0.9))
	require.NoError(t, s.LinkMembers(b, []int64{b1}, 0.9))
	require.NoError(t, s.LinkContext(b, []int64{shared}))

	got := memberHolders(t, s, []int64{b1, a2, unlinked, a1})
	assert.Equal(t, []int64{a, b}, got, "ascending, each once; an unlinked event has no holder")

	got = memberHolders(t, s, []int64{shared})
	assert.Equal(t, []int64{a}, got, "a context link does not make its narrative a holder")
}

func memberHolders(t *testing.T, s *store.Store, eventIDs []int64) []int64 {
	t.Helper()

	var got []int64
	require.NoError(t, s.WithTx(func(tx *store.Tx) error {
		var err error
		got, err = tx.MemberHolders(eventIDs)

		return err
	}))

	return got
}
