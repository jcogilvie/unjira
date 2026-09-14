package store_test

// issueactivity_test.go covers the store half of finding F9's fix.
//
// IssueActivity is a ranking signal, not a correctness gate, and its tests are
// shaped around that: the interesting cases are all about degrading usefully
// rather than about exact numbers.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/events"
)

func TestIssueActivity_ReturnsTheLatestPerIssue(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 9, 2, 9, 0, 0, 0, time.UTC)

	// Two events on one issue, inserted OLDEST LAST so a query that returned "the
	// last row it happened to see" would pick the wrong one. MAX(occurred_at) is
	// what makes insertion order irrelevant.
	newer := base
	older := base.Add(-48 * time.Hour)

	for i, at := range []time.Time{newer, older} {
		e := events.NewEvent("jira", "PROJ-1:evt:"+string(rune('a'+i)), at, "did a thing")
		e.Artifacts[events.ArtifactIssueKey] = "PROJ-1"
		insertEvent(t, s, e)
	}

	activity, err := s.IssueActivity()
	require.NoError(t, err)

	got, ok := activity["PROJ-1"]
	require.True(t, ok, "an issue with collected events must appear")
	assert.True(t, got.Equal(newer),
		"the LATEST activity is the ranking signal; got %s, want %s", got, newer)
}

// TestIssueActivity_IgnoresEventsWithNoIssueKey is what keeps the map honest. A
// claude_code session event names no single issue, and a key of "" is not an
// issue — including either would put a bogus entry in the map, and every bogus
// entry is a candidate wrongly promoted to the corroborated tier.
func TestIssueActivity_IgnoresEventsWithNoIssueKey(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 9, 2, 9, 0, 0, 0, time.UTC)

	noKey := events.NewEvent("claude_code", "session-1", base, "a coding session")
	insertEvent(t, s, noKey)

	emptyKey := events.NewEvent("jira", "PROJ-2:evt:a", base, "malformed")
	emptyKey.Artifacts[events.ArtifactIssueKey] = ""
	insertEvent(t, s, emptyKey)

	activity, err := s.IssueActivity()
	require.NoError(t, err)

	assert.Empty(t, activity,
		"neither a keyless event nor an empty key is evidence that some issue moved")
}

// TestIssueActivity_IsNotFilteredBySource is a deliberate forward-compatibility
// assertion, and the one most likely to be "simplified" away by a future reader.
//
// The obvious implementation filters `WHERE source = 'jira'`. That would be wrong
// the moment a second tracker backend lands: a GitHub Issues collector's events
// are equally good evidence that an issue moved, and a source filter would make
// the ranking silently blind to them while still returning plausible numbers.
// What qualifies an event here is that it NAMES an issue.
func TestIssueActivity_IsNotFilteredBySource(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 9, 2, 9, 0, 0, 0, time.UTC)

	// A source name no collector uses today, standing in for a future backend.
	e := events.NewEvent("github", "gh-issue-7:evt:a", base, "closed the issue")
	e.Artifacts[events.ArtifactIssueKey] = "PROJ-7"
	insertEvent(t, s, e)

	activity, err := s.IssueActivity()
	require.NoError(t, err)

	assert.Contains(t, activity, "PROJ-7",
		"activity from any tracker-shaped source counts; adding a source filter here would make "+
			"candidate ranking silently stop working for every backend but Jira")
}

// TestIssueActivity_EmptyStoreIsNotAnError pins the degradation contract. An empty
// map means "we know nothing", which gatherCandidates treats as "rank exactly as
// you did before F9" — so a fresh store must not produce an error the caller
// would have to special-case.
func TestIssueActivity_EmptyStoreIsNotAnError(t *testing.T) {
	s := openStore(t)

	activity, err := s.IssueActivity()
	require.NoError(t, err)
	assert.Empty(t, activity)
}
