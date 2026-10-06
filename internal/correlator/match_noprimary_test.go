package correlator_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// A narrative whose every candidate the model judged a citation, not the work, ends a
// pass with link rows and no primary. Before this was recorded as an examination it
// was re-selected on every pass, at a model call each time, and, because it counted
// as unmatched, it deferred every create proposal for every narrative forever.
// Observed on a real 30-day store: one such narrative, re-classified on nine
// consecutive passes, the only reason no pass ever proposed a ticket.
const allMentionedResponse = `[{"issue_key":"PAAS-1","role":"mentioned","confidence":0.9,"rationale":"cited"},` +
	`{"issue_key":"SUMO-2","role":"mentioned","confidence":0.9,"rationale":"cited"}]`

func TestMatch_NoPrimaryAssignedIsExaminedNotReselected(t *testing.T) {
	s := matchStore(t)
	id := seedNarrative(t, s, "Investigation", "summary", claudeEvent(t, "s1", "feature/PAAS-1", "SUMO-2"))
	tracker := &fakeTracker{issues: map[string]tasktracker.Issue{
		"PAAS-1": {Key: "PAAS-1", Summary: "One", StatusName: "To Do"},
		"SUMO-2": {Key: "SUMO-2", Summary: "Two", StatusName: "To Do"},
	}}
	llmFake := &fakeLLM{responses: []string{allMentionedResponse}}

	_, stats, err := correlator.Match(t.Context(), s, tracker, llmFake, matchCfg())
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Calls)
	assert.Empty(t, primaryOf(t, s, id))

	backlog, err := s.NarrativesWithoutPrimaryLink(10)
	require.NoError(t, err)
	assert.Empty(t, backlog, "a narrative the model gave no primary is examined, not still waiting")

	remaining, err := s.CountNarrativesWithoutPrimaryLink()
	require.NoError(t, err)
	assert.Zero(t, remaining, "nor may it count as unmatched, which defers every create")

	_, stats, err = correlator.Match(t.Context(), s, tracker, llmFake, matchCfg())
	require.NoError(t, err)
	assert.Zero(t, stats.Calls, "the next pass must not pay to classify it again")
}
