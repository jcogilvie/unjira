package correlator_test

// match_corroboration_test.go covers finding F9: within the prose tiers, ties
// broke ALPHABETICALLY, which is not merely unrelated to relevance — it is
// anti-correlated with it, because lexicographic order on a monotonic issue
// counter favours OLDER tickets.
//
// Measured on real data (event 15, a Claude Code session mentioning 73 keys):
// PAAS-4001, a ticket with genuine recent Jira activity, ranked 45 of 73 and was
// truncated away at the default cap of 10, while PAAS-1073 and PAAS-2801 — years
// older and mentioned in passing — survived.
//
// The fix adds ProvenanceCorroborated between JiraEvent and ProseFirst, and
// orders WITHIN that tier by most-recent Jira activity.
//
// Both halves are load-bearing, which is the part worth remembering. The tier
// ALONE does not fix the finding's own example: 30 of those 73 keys are
// corroborated, still 3x the cap, so an alphabetical sort inside the new tier
// just re-decides the same way and PAAS-4001 lands at 26 of 30 — still cut.
// Measured, not assumed. The finding as originally written proposed gating the
// tier on a bounded recency WINDOW instead, and that was measured too: it only
// works between roughly 21 and 30 days. At 14d the pool is 4 and excludes
// PAAS-4001; at 60d the pool is 22 and alphabetical wins again. A config knob
// whose correct value is a narrow band nobody can calibrate is a latent bug, so
// the ordering does the work and no knob was added.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/correlator"
)

// corroborationFixture is the shape F9 was measured on, in miniature: one event
// mentioning many keys in prose, where the RELEVANT key sorts late.
func corroborationFixture(t *testing.T) []correlator.Event {
	t.Helper()

	return []correlator.Event{
		claudeEvent(t, "session-1", "",
			// Deliberately in an order that is neither alphabetical nor the answer,
			// so a test passing by accident of input order is not possible.
			"PROJ-9001", "PROJ-1002", "PROJ-1001", "PROJ-9002"),
	}
}

// TestGatherCandidates_CorroboratedOutranksProse is the tier half of the fix.
func TestGatherCandidates_CorroboratedOutranksProse(t *testing.T) {
	base := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)

	// PROJ-9001 sorts LAST alphabetically among these four, and is the only one
	// with Jira activity. Under the old ranking it lost to three older keys.
	activity := map[string]time.Time{"PROJ-9001": base}

	got := correlator.GatherCandidatesForTest(corroborationFixture(t), nil, 0, activity)

	require.NotEmpty(t, got)
	assert.Equal(t, "PROJ-9001", got[0].IssueKey,
		"a prose key whose issue has real Jira activity must outrank prose keys that have none, "+
			"regardless of where it falls alphabetically")
	assert.Equal(t, correlator.ProvenanceCorroborated, got[0].Provenance,
		"and it must be RECORDED as corroborated, because provenance reaches the matching prompt "+
			"as a prior — silently reordering without saying why would leave the model ranking on "+
			"a signal it cannot see")
}

// TestGatherCandidates_CorroboratedTierOrdersByRecency is the half the finding
// missed, and the half that actually rescues its cited example. With a
// corroborated pool larger than the cap, only the ordering decides who survives.
func TestGatherCandidates_CorroboratedTierOrdersByRecency(t *testing.T) {
	base := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)

	// All four corroborated, so the tier cannot discriminate — but their activity
	// is ordered inversely to their alphabetical order. An alphabetical tiebreak
	// inside the tier would return exactly the reverse of the right answer, which
	// is what makes this a real assertion rather than a restatement of the tier.
	activity := map[string]time.Time{
		"PROJ-1001": base.Add(-72 * time.Hour),
		"PROJ-1002": base.Add(-48 * time.Hour),
		"PROJ-9001": base.Add(-24 * time.Hour),
		"PROJ-9002": base,
	}

	got := correlator.GatherCandidatesForTest(corroborationFixture(t), nil, 0, activity)

	keys := make([]string, 0, len(got))
	for _, c := range got {
		keys = append(keys, c.IssueKey)
	}

	assert.Equal(t, []string{"PROJ-9002", "PROJ-9001", "PROJ-1002", "PROJ-1001"}, keys,
		"most recent Jira activity first; the exact reverse of the alphabetical order this replaces")
}

// TestGatherCandidates_RecencyOrderingSurvivesTruncation is the finding's actual
// failure, reproduced at the cap. This is the test that would have caught the
// tier-without-ordering fix shipping as a fix.
func TestGatherCandidates_RecencyOrderingSurvivesTruncation(t *testing.T) {
	base := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)

	activity := map[string]time.Time{
		"PROJ-1001": base.Add(-72 * time.Hour),
		"PROJ-1002": base.Add(-48 * time.Hour),
		"PROJ-9001": base, // most recent, alphabetically third of four
	}

	got := correlator.GatherCandidatesForTest(corroborationFixture(t), nil, 2, activity)

	require.Len(t, got, 2, "the cap must still bound the list")

	keys := make([]string, 0, len(got))
	for _, c := range got {
		keys = append(keys, c.IssueKey)
	}
	assert.Contains(t, keys, "PROJ-9001",
		"the recently-active key must survive truncation; under the alphabetical tiebreak this "+
			"is exactly the candidate that was cut while older, quieter keys were kept")
	assert.NotContains(t, keys, "PROJ-1001",
		"and the oldest-activity key must be the one dropped")
}

// TestGatherCandidates_NoActivityKeepsAlphabeticalOrder pins the fallback. A key
// with no Jira activity is not evidence of irrelevance — most keys in a fresh
// store have none, because the jira collector only ever saw what its JQL scoped.
// Those keys stay in their existing tier with their existing deterministic
// order, so this change cannot reorder a narrative it has no signal about.
func TestGatherCandidates_NoActivityKeepsAlphabeticalOrder(t *testing.T) {
	got := correlator.GatherCandidatesForTest(corroborationFixture(t), nil, 0, nil)

	keys := make([]string, 0, len(got))
	for _, c := range got {
		keys = append(keys, c.IssueKey)
	}

	// PROJ-9001 leads because it is index 0 of the fixture's ticket_keys, making it
	// prose_first — a stronger TIER, which corroboration was never supposed to
	// disturb. The other three are prose_later and are alphabetical among
	// themselves. Writing this expectation as a flat alphabetical list was my first
	// draft and it failed here, which is the assertion doing its job: it caught a
	// wrong expectation rather than a wrong implementation.
	assert.Equal(t, []string{"PROJ-9001", "PROJ-1001", "PROJ-1002", "PROJ-9002"}, keys,
		"with no corroboration data at all, ranking must be byte-for-byte what it was before "+
			"this change — an empty activity map is 'we know nothing', not 'nothing is relevant'")

	for _, c := range got {
		assert.NotEqual(t, correlator.ProvenanceCorroborated, c.Provenance,
			"and nothing may be labelled corroborated without evidence")
	}
}

// TestGatherCandidates_CorroborationDoesNotOutrankBranch pins the tier's
// placement. A branch name is an explicit human act of naming the ticket for
// this work; corroboration is an inference from activity timing. Ranking the
// inference above the explicit act would invert the whole point of the ordering,
// and it is an easy mistake to make because the corroborated key has "more"
// evidence attached to it.
func TestGatherCandidates_CorroborationDoesNotOutrankBranch(t *testing.T) {
	base := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)

	evts := []correlator.Event{
		claudeEvent(t, "session-1", "feature/PROJ-1001-the-work", "PROJ-9002"),
	}

	// The prose key is corroborated; the branch key is not. Even so, the branch
	// wins: recent activity on a ticket someone merely mentioned does not beat a
	// ticket they named the branch after.
	activity := map[string]time.Time{"PROJ-9002": base}

	got := correlator.GatherCandidatesForTest(evts, nil, 0, activity)

	require.Len(t, got, 2)
	assert.Equal(t, "PROJ-1001", got[0].IssueKey,
		"the branch candidate must stay strongest")
	assert.Equal(t, correlator.ProvenanceBranch, got[0].Provenance)
	assert.Equal(t, correlator.ProvenanceCorroborated, got[1].Provenance)
}

// TestGatherCandidates_CorroborationDoesNotDemoteAJiraEventKey guards the other
// boundary. ProvenanceJiraEvent means the event IS about that issue — direct,
// not inferred — so corroboration must never overwrite it with a weaker label.
// upsert keeps only the strongest provenance, and this asserts corroborated
// loses that comparison rather than winning it on recency.
func TestGatherCandidates_CorroborationDoesNotDemoteAJiraEventKey(t *testing.T) {
	base := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)

	evts := []correlator.Event{
		jiraEvent(t, "PROJ-5000"),
		claudeEvent(t, "session-1", "", "PROJ-5000"),
	}

	activity := map[string]time.Time{"PROJ-5000": base}

	got := correlator.GatherCandidatesForTest(evts, nil, 0, activity)

	require.Len(t, got, 1, "the same key from two sources is one candidate")
	assert.Equal(t, correlator.ProvenanceJiraEvent, got[0].Provenance,
		"a key the event is directly about must not be relabelled as merely corroborated")
}
