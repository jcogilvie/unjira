package store_test

// correction_seq_test.go covers the correction sequence: a number stamped when an action
// BECOMES a correction, which is what the learn cursor orders by (findings F41 and F42).

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/store"
)

func proposedAction(t *testing.T, s *store.Store, narrativeID int64) int64 {
	t.Helper()

	id, err := s.InsertAction(store.ActionRow{
		NarrativeID: narrativeID, Type: "comment", IssueKey: "DEVSBX-1",
		Payload: `{"body":"drafted prose"}`, Status: store.StatusProposed,
	})
	require.NoError(t, err)

	return id
}

// TestCorrectionsSince_FeedbackAddedByALaterRulingIsOffered is F42. A rejection with no
// feedback is not a correction; `actions decide N --edit "<lesson>"` later makes it one. The
// row kept its FIRST decided_at, so once a learn pass had read past that instant the lesson
// was never offered. Measured before the fix: 0 rows.
func TestCorrectionsSince_FeedbackAddedByALaterRulingIsOffered(t *testing.T) {
	s, nid := correctionsStore(t)
	silent := proposedAction(t, s, nid)
	require.NoError(t, s.UpdateActionStatusAndFeedback(silent, store.StatusRejected, ""))
	insertRuled(t, s, nid, store.StatusRejected, "read by the draft")

	_, readThrough, err := s.CorrectionsSince(store.CorrectionsCursor{})
	require.NoError(t, err)

	require.NoError(t, s.UpdateActionStatusAndFeedback(silent, store.StatusEdited, "the lesson"))

	got, _, err := s.CorrectionsSince(readThrough)
	require.NoError(t, err)
	require.Len(t, got, 1, "the lesson must be offered once it exists")
	assert.Equal(t, silent, got[0].ActionID)
	assert.Equal(t, "the lesson", got[0].Feedback)
}

// TestCorrectionsSince_ARevisedLessonIsOfferedAgain: a reviewer who rewrites their feedback
// has said something new, and the draft that read the old text never saw it.
func TestCorrectionsSince_ARevisedLessonIsOfferedAgain(t *testing.T) {
	s, nid := correctionsStore(t)
	id := insertRuled(t, s, nid, store.StatusRejected, "first wording")

	_, readThrough, err := s.CorrectionsSince(store.CorrectionsCursor{})
	require.NoError(t, err)

	require.NoError(t, s.UpdateActionStatusAndFeedback(id, store.StatusRejected, "the corrected wording"))

	got, _, err := s.CorrectionsSince(readThrough)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "the corrected wording", got[0].Feedback)
}

// TestCorrectionsSince_ReRulingWithTheSameLessonIsNotReOffered: switching reject to edit with
// identical text teaches nothing new, and re-offering it would ask the reviewer to rule on a
// rule they already kept. Status is not part of a correction's content.
func TestCorrectionsSince_ReRulingWithTheSameLessonIsNotReOffered(t *testing.T) {
	s, nid := correctionsStore(t)
	id := insertRuled(t, s, nid, store.StatusRejected, "same words")

	_, readThrough, err := s.CorrectionsSince(store.CorrectionsCursor{})
	require.NoError(t, err)

	require.NoError(t, s.UpdateActionStatusAndFeedback(id, store.StatusEdited, "same words"))
	require.NoError(t, s.UpdateActionStatus(id, store.StatusEdited))

	got, _, err := s.CorrectionsSince(readThrough)
	require.NoError(t, err)
	assert.Empty(t, got)
}

// TestCorrectionsSince_ACorrectionThatLapsedAndReturnedIsOfferedAgain: approved in between,
// it stopped being a correction; rejected again, it is one anew.
func TestCorrectionsSince_ACorrectionThatLapsedAndReturnedIsOfferedAgain(t *testing.T) {
	s, nid := correctionsStore(t)
	id := insertRuled(t, s, nid, store.StatusRejected, "not this")
	_, readThrough, err := s.CorrectionsSince(store.CorrectionsCursor{})
	require.NoError(t, err)

	require.NoError(t, s.UpdateActionStatus(id, store.StatusApproved))
	got, _, err := s.CorrectionsSince(readThrough)
	require.NoError(t, err)
	assert.Empty(t, got, "an approved action is not a correction")

	require.NoError(t, s.UpdateActionStatus(id, store.StatusRejected))
	got, _, err = s.CorrectionsSince(readThrough)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, id, got[0].ActionID)
}

// TestCorrectionsSince_IgnoresTheWallClock is F41, and the two F31 cases it subsumes. A
// ruling whose decided_at is identical to, or EARLIER than, the newest one read (a clock
// stepping back, or two rulings in one millisecond) is still offered, because the cursor
// orders by the correction sequence and never reads decided_at.
func TestCorrectionsSince_IgnoresTheWallClock(t *testing.T) {
	s, nid := correctionsStore(t)
	read := insertRuled(t, s, nid, store.StatusRejected, "read by the draft")
	setDecidedAt(t, s, read, "2026-01-01T12:00:05.950Z")

	_, readThrough, err := s.CorrectionsSince(store.CorrectionsCursor{})
	require.NoError(t, err)

	same := insertRuled(t, s, nid, store.StatusRejected, "same millisecond, after the read")
	setDecidedAt(t, s, same, "2026-01-01T12:00:05.950Z")
	stepped := insertRuled(t, s, nid, store.StatusEdited, "the clock stepped back")
	setDecidedAt(t, s, stepped, "2026-01-01T11:59:00.000Z")

	got, _, err := s.CorrectionsSince(readThrough)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, []int64{same, stepped}, []int64{got[0].ActionID, got[1].ActionID},
		"in the order they became corrections")
}

// TestCorrectionsSince_ReadThroughCoversEverythingRead: the cursor is the highest sequence
// read, so a second read offers only what came after, and a third offers nothing.
func TestCorrectionsSince_ReadThroughCoversEverythingRead(t *testing.T) {
	s, nid := correctionsStore(t)
	insertRuled(t, s, nid, store.StatusRejected, "a")
	insertRuled(t, s, nid, store.StatusRejected, "b")

	got, first, err := s.CorrectionsSince(store.CorrectionsCursor{})
	require.NoError(t, err)
	require.Len(t, got, 2)

	c := insertRuled(t, s, nid, store.StatusEdited, "c")
	got, second, err := s.CorrectionsSince(first)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, c, got[0].ActionID)
	assert.Greater(t, second.Seq, first.Seq)

	got, third, err := s.CorrectionsSince(second)
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.Equal(t, second, third, "reading nothing leaves the cursor where it was")
}

// TestActions_ACorrectionWithoutASequenceIsRefused: a write path that makes a row a correction
// without stamping it would hide it from every learn pass. The schema refuses it, so the
// mistake is an error rather than a lesson nobody ever sees.
func TestActions_ACorrectionWithoutASequenceIsRefused(t *testing.T) {
	s, nid := correctionsStore(t)
	id := proposedAction(t, s, nid)

	err := s.ExecForTest(`UPDATE actions SET status = 'rejected', feedback = 'unstamped' WHERE id = ?`, id)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "CHECK constraint failed")
}

// TestCorrectionsCursor_RoundTripsAndRejectsWhatItCannotRead pins the stored form: a positive
// decimal sequence, or "" for zero. Anything else, including both earlier timestamp forms,
// is an error, since a misread cursor silently skips or re-offers corrections.
func TestCorrectionsCursor_RoundTripsAndRejectsWhatItCannotRead(t *testing.T) {
	c := store.CorrectionsCursor{Seq: 42}
	parsed, err := store.ParseCorrectionsCursor(c.String())
	require.NoError(t, err)
	assert.Equal(t, c, parsed)
	assert.Equal(t, "42", c.String())

	zero, err := store.ParseCorrectionsCursor("")
	require.NoError(t, err)
	assert.True(t, zero.IsZero())
	assert.Empty(t, store.CorrectionsCursor{}.String(), "zero is stored as absence")

	for _, raw := range []string{
		"2026-01-01T12:00:05Z",                                // the pre-F31 watermark
		`{"decided_at":"2026-01-01T12:00:05.950Z","ids":[3]}`, // the F31 cursor
		"0", "-3", "+3", " 3", "3 ", "3.0", "0x10", "abc",
	} {
		_, err := store.ParseCorrectionsCursor(raw)
		assert.Error(t, err, "ParseCorrectionsCursor(%q)", raw)
	}
}

func TestCorrectionsCursor_LaterNeverMovesBackwards(t *testing.T) {
	early, late := store.CorrectionsCursor{Seq: 3}, store.CorrectionsCursor{Seq: 9}

	assert.Equal(t, late, early.Later(late))
	assert.Equal(t, late, late.Later(early))
	assert.Equal(t, late, store.CorrectionsCursor{}.Later(late))
}

// TestOpen_RefusesAStoreThatPredatesTheCorrectionSequence: in an older store every existing
// correction has no sequence, so it would never be offered. Refused before any DDL.
func TestOpen_RefusesAStoreThatPredatesTheCorrectionSequence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pre-correction-seq.db")

	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE actions (
    id INTEGER PRIMARY KEY, created_link_seq INTEGER, executed_link_seq INTEGER)`)
	require.NoError(t, err)
	tablesBefore := countTables(t, db)

	s, err := store.Open(path)
	if s != nil {
		_ = s.Close()
	}

	require.Error(t, err)
	require.ErrorContains(t, err, "actions.corrected_seq")
	require.ErrorContains(t, err, "re-collect")
	assert.Equal(t, tablesBefore, countTables(t, db), "a refused open must not create any table")
	require.NoError(t, db.Close())
}
