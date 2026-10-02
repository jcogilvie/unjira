package reconciler

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// TestReconcile_AContextLinkIsNotNewWork is the reconcile-level pin of the shared-
// context design's §2: the reconciler sees no context links at all. B's own work was
// already examined (an action exists); B then gains a context link to another
// narrative's investigation. That investigation is real work evidence, so if context
// reached the delta it would pass suppressTrackerEcho and B would draft a comment out
// of A's work — the one-paragraph-on-three-tickets failure. B must not be selected,
// and no model call may be made.
//
// The drill this test exists for: drop the kind filter from linkedSinceLastAction and
// this fails, because the context link is newer than B's action.
func TestReconcile_AContextLinkIsNotNewWork(t *testing.T) {
	s := reconcileStore(t)
	tracker := &fakeTracker{issues: map[string]tasktracker.Issue{
		"PROJ-1": {Key: "PROJ-1", Summary: "the ticket", StatusCategory: tasktracker.StatusInProgress},
	}}

	b := seedLinkedNarrative(t, s, "PROJ-1", store.Role("primary"), codeEvent("b-own", "B's own work"))
	_, err := s.InsertAction(store.ActionRow{
		NarrativeID: b, Type: "comment", IssueKey: "PROJ-1",
		Payload: `{"body":"about B's own work"}`, Status: store.StatusProposed,
	})
	require.NoError(t, err)

	a := seedLinkedNarrative(t, s, "", "", codeEvent("a-investigation", "A's root-cause investigation"))
	investigation, err := s.EventIDByExternalID("claude_code", "a-investigation")
	require.NoError(t, err)
	require.NoError(t, s.LinkContext(b, []int64{investigation}))
	require.NotEqual(t, a, b)

	delta, err := s.DeltaEvents(b)
	require.NoError(t, err)
	assert.Empty(t, delta, "a context link is never delta")

	llmClient := &fakeLLM{}
	results, _, err := Reconcile(t.Context(), s, tracker, llmClient, testConfig())

	require.NoError(t, err)
	for _, r := range results {
		assert.NotEqual(t, b, r.NarrativeID, "a context link must not re-admit B to the reconcile backlog")
	}
	assert.Empty(t, llmClient.prompts, "and no model call is spent on another narrative's work")
}
