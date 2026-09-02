package events_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/events"
)

// TestStatusChange_RoundTripsThroughArtifacts is the contract every
// status-emitting collector implements and every consumer reads. It lives in
// internal/events because Artifacts is a bare map[string]any with no declared
// keys: without a named accessor the "contract" is three string literals a
// collector and a consumer each hardcode and hope agree.
//
// Concretely, the reconciler's staleness guard needs to know an event IS a status
// change. Deciding that by reading Jira's changelog vocabulary (field == "status")
// puts a Jira detail in the reconciler and cannot generalize: GitHub Issues has no
// `field` concept at all — open/closed arrives as a timeline event.
func TestStatusChange_RoundTripsThroughArtifacts(t *testing.T) {
	e := events.NewEvent("jira", "PAAS-1:status:1",
		time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC), "PAAS-1 status: To Do → In Progress")

	events.SetStatusChange(&e, "PAAS-1", "To Do", "In Progress")

	got, ok := events.StatusChangeOf(e)

	require.True(t, ok)
	assert.Equal(t, "PAAS-1", got.IssueKey)
	assert.Equal(t, "To Do", got.From)
	assert.Equal(t, "In Progress", got.To)
}

// TestStatusChangeOf_ReportsAbsenceForANonStatusEvent: a description edit or a
// transcript event is not a status change. Reporting one as a zero-valued
// StatusChange would let a summary edit masquerade as a transition, and the
// guard would compare live state against a destination of "" — matching no real
// status, silently suppressing every transition on that issue.
func TestStatusChangeOf_ReportsAbsenceForANonStatusEvent(t *testing.T) {
	plain := events.NewEvent("claude_code", "cc:1",
		time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC), "did some work")

	_, ok := events.StatusChangeOf(plain)

	assert.False(t, ok)
}

// TestSetStatusChange_RefusesAnEmptyDestination: a status change with no
// destination is not a status change. Recording one would produce exactly the
// unreadable event the reconciler has to defend against — present enough to look
// like evidence, empty enough to be useless.
func TestSetStatusChange_RefusesAnEmptyDestination(t *testing.T) {
	e := events.NewEvent("jira", "PAAS-1:status:1",
		time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC), "PAAS-1 status: To Do →")

	events.SetStatusChange(&e, "PAAS-1", "To Do", "")

	_, ok := events.StatusChangeOf(e)
	assert.False(t, ok, "no destination means no status change was recorded")
}

// TestStatusChangeOf_TreatsAnEmptyDestinationAsAbsence guards the READER
// independently of the writer.
//
// SetStatusChange refuses an empty destination, so no caller using it can produce
// this state — which is exactly why the reader needs its own test. Artifacts
// round-trips through JSON in the store and can be written by any future
// collector, a migration, or a hand-seeded row; a reader whose only protection is
// its writer is unprotected the moment something else writes.
//
// Verified by drill: relaxing this check produces no failure at all if the
// artifacts are only ever set through SetStatusChange.
func TestStatusChangeOf_TreatsAnEmptyDestinationAsAbsence(t *testing.T) {
	e := events.NewEvent("jira", "PAAS-1:status:1",
		time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC), "moved")

	// Set directly, as a store round trip or another writer could.
	e.Artifacts[events.ArtifactIssueKey] = "PAAS-1"
	e.Artifacts[events.ArtifactStatusFrom] = "In Progress"
	e.Artifacts[events.ArtifactStatusTo] = ""

	_, ok := events.StatusChangeOf(e)

	assert.False(t, ok,
		"a recorded destination of \"\" is no evidence, not evidence of \"\" — a consumer "+
			"comparing live state against it would match no real status and suppress forever")
}

// TestSetStatusChange_AllowsAnEmptyOrigin: an issue's first status has no
// predecessor, and GitHub's "closed" timeline event names no prior state. From is
// informational; only To is load-bearing.
func TestSetStatusChange_AllowsAnEmptyOrigin(t *testing.T) {
	e := events.NewEvent("github", "gh:1",
		time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC), "issue closed")

	events.SetStatusChange(&e, "owner/repo#7", "", "closed")

	got, ok := events.StatusChangeOf(e)

	require.True(t, ok, "a backend with no notion of the previous state still reports a change")
	assert.Empty(t, got.From)
	assert.Equal(t, "closed", got.To)
}

// TestStatusChangeOf_IsSourceAgnostic is the whole point: the accessor must not
// care which collector produced the event. A GitHub collector emitting a
// closed-timeline event and a Jira collector emitting a changelog transition are
// the same fact to the reconciler.
func TestStatusChangeOf_IsSourceAgnostic(t *testing.T) {
	for _, source := range []string{"jira", "github", "linear", "some_future_tracker"} {
		e := events.NewEvent(source, source+":1",
			time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC), "moved")
		events.SetStatusChange(&e, "KEY-1", "a", "b")

		got, ok := events.StatusChangeOf(e)

		require.True(t, ok, "source %q", source)
		assert.Equal(t, "b", got.To)
	}
}

// TestStatusChangeOf_ToleratesNonStringArtifacts: Artifacts survives a JSON round
// trip through the store, and a hand-written or corrupted row can hold any type.
// A type assertion failure must read as "not a status change" rather than panic.
func TestStatusChangeOf_ToleratesNonStringArtifacts(t *testing.T) {
	e := events.NewEvent("jira", "PAAS-1:status:1",
		time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC), "moved")
	e.Artifacts[events.ArtifactStatusTo] = 42
	e.Artifacts[events.ArtifactIssueKey] = []string{"PAAS-1"}

	_, ok := events.StatusChangeOf(e)

	assert.False(t, ok, "a wrongly-typed artifact is absence, not a panic")
}
