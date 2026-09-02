package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/store"
)

// statusEvent builds the shape internal/collector/jira emits for a status
// change: field=status plus the transition's endpoints as artifacts.
func statusEvent(extID, issueKey, from, to string, at time.Time) events.Event {
	e := events.NewEvent("jira", extID, at, issueKey+" status: "+from+" → "+to)
	e.Artifacts["issue_key"] = issueKey
	e.Artifacts["field"] = "status"
	e.Artifacts["status_from"] = from
	e.Artifacts["status_to"] = to

	return e
}

func insertEvent(t *testing.T, s *store.Store, e events.Event) {
	t.Helper()

	_, err := s.InsertEvent(e)
	require.NoError(t, err)
}

// TestLatestStatusEvent_ReturnsTheNewestForThatIssue is the accessor the
// reconciler's recency guard is built on. Returning any status event other than
// the newest would compare live state against a stale destination, and the guard
// would then either suppress a legitimate transition or propose one that
// contradicts a move somebody just made.
func TestLatestStatusEvent_ReturnsTheNewestForThatIssue(t *testing.T) {
	s := openStore(t)

	base := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	insertEvent(t, s, statusEvent("PAAS-1:status:1", "PAAS-1", "Discovery", "Ready for Dev", base))
	insertEvent(t, s, statusEvent("PAAS-1:status:2", "PAAS-1", "Ready for Dev", "In Progress",
		base.Add(2*time.Hour)))
	insertEvent(t, s, statusEvent("PAAS-1:status:3", "PAAS-1", "In Progress", "In Review",
		base.Add(time.Hour)))

	got, ok, err := s.LatestStatusEvent("PAAS-1")

	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "In Progress", got.To, "newest by occurred_at, not by insertion order")
	assert.Equal(t, "Ready for Dev", got.From)
	assert.Equal(t, base.Add(2*time.Hour), got.OccurredAt.UTC())
}

// TestLatestStatusEvent_ScopesToTheIssue: two issues' histories must not mix. A
// cross-issue leak would compare PAAS-1's live status against PAAS-2's last
// move, which is a comparison of unrelated facts that happens to type-check.
func TestLatestStatusEvent_ScopesToTheIssue(t *testing.T) {
	s := openStore(t)

	base := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	insertEvent(t, s, statusEvent("PAAS-1:status:1", "PAAS-1", "To Do", "In Progress", base))
	// Newer, but a different issue.
	insertEvent(t, s, statusEvent("PAAS-2:status:1", "PAAS-2", "To Do", "Done",
		base.Add(5*time.Hour)))

	got, ok, err := s.LatestStatusEvent("PAAS-1")

	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "In Progress", got.To)
}

// TestLatestStatusEvent_IgnoresNonStatusEvents: a description or summary edit is
// not a transition. Selecting one would give the guard a status_to of "", which
// compares unequal to every real live status and would suppress every transition
// on that issue forever.
func TestLatestStatusEvent_IgnoresNonStatusEvents(t *testing.T) {
	s := openStore(t)

	base := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	insertEvent(t, s, statusEvent("PAAS-1:status:1", "PAAS-1", "To Do", "In Progress", base))

	// A newer description edit on the same issue, as the collector emits it.
	edit := events.NewEvent("jira", "PAAS-1:description:2", base.Add(3*time.Hour),
		"PAAS-1 description: rewritten")
	edit.Artifacts["issue_key"] = "PAAS-1"
	edit.Artifacts["field"] = "description"
	insertEvent(t, s, edit)

	got, ok, err := s.LatestStatusEvent("PAAS-1")

	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "In Progress", got.To,
		"the newest STATUS event, not the newest event on the issue")
}

// TestLatestStatusEvent_ReportsAbsenceRatherThanAZeroValue: "no status history"
// and "moved to the empty string" must be distinguishable. A zero-valued return
// with no ok flag would read as a real destination of "", which no live status
// ever equals — silently disabling transitions for every issue the collector has
// not seen transition.
func TestLatestStatusEvent_ReportsAbsenceRatherThanAZeroValue(t *testing.T) {
	s := openStore(t)

	_, ok, err := s.LatestStatusEvent("PAAS-404")

	require.NoError(t, err, "an issue with no collected history is not an error")
	assert.False(t, ok, "absence must be reported as absence")
}

// TestLatestStatusEvent_BreaksTiesDeterministically: two status changes can share
// a timestamp (Jira's changelog is second-granular, and a bulk transition moves
// many issues at once). Without a tiebreaker the winner is whatever SQLite
// happens to return, so the guard's verdict could flip between passes on
// unchanged data — the hardest kind of bug to reproduce.
//
// Highest event id wins: it is the later INSERT, which for same-timestamp
// changelog entries is the later entry id, so this agrees with Jira's own order.
func TestLatestStatusEvent_BreaksTiesDeterministically(t *testing.T) {
	s := openStore(t)

	at := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	insertEvent(t, s, statusEvent("PAAS-1:status:1", "PAAS-1", "To Do", "In Progress", at))
	insertEvent(t, s, statusEvent("PAAS-1:status:2", "PAAS-1", "In Progress", "In Review", at))

	for range 5 {
		got, ok, err := s.LatestStatusEvent("PAAS-1")
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, "In Review", got.To, "the same answer on every call")
	}
}

// TestLatestStatusEvent_ToleratesAMissingStatusToArtifact: events collected
// before status_to existed have field=status but no endpoints. Treating one as a
// destination of "" would suppress transitions on that issue; it must read as
// absent instead, so the guard degrades to unguarded rather than
// permanently-blocked.
func TestLatestStatusEvent_ToleratesAMissingStatusToArtifact(t *testing.T) {
	s := openStore(t)

	base := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)

	// The pre-status_to shape: field=status, endpoints only in the summary.
	old := events.NewEvent("jira", "PAAS-1:status:1", base, "PAAS-1 status: To Do → In Progress")
	old.Artifacts["issue_key"] = "PAAS-1"
	old.Artifacts["field"] = "status"
	insertEvent(t, s, old)

	_, ok, err := s.LatestStatusEvent("PAAS-1")

	require.NoError(t, err)
	assert.False(t, ok,
		"an event with no recorded destination is no evidence, not evidence of \"\"")
}
