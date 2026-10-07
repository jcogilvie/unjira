package reconciler

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

func liveAt(key, status string, category tasktracker.StatusCategory) verifiedLink {
	return verifiedLink{
		Link:  store.NarrativeIssue{IssueKey: key, Role: store.RolePrimary},
		Issue: tasktracker.Issue{Key: key, StatusName: status, StatusCategory: category},
	}
}

// A transition to the status the issue already has changes nothing, so it is not a
// proposal. Seen on a real store: an Implementing ticket proposed -> "Implementing".
// Judged from the live status alone, so it holds when no status history was
// collected for the issue, which is exactly when the staleness guard cannot run.
func TestSuppressSettledStatus_ATransitionToTheCurrentStatusIsSuppressed(t *testing.T) {
	verified := []verifiedLink{liveAt("SUMO-1", "Implementing", tasktracker.StatusInProgress)}

	kept, reasons := suppressSettledStatus(verified, []ProposedAction{transitionTo("SUMO-1", " implementing ")})

	assert.Empty(t, kept)
	require.Len(t, reasons, 1)
	assert.Contains(t, reasons[0], "SUMO-1")
	assert.Contains(t, reasons[0], "already")
}

// A done issue is never moved out of done by unjira. Reopening a closed ticket is a
// judgment about why it was closed, which no collected event records. Seen on a real
// store: a Closed incident ticket, owned by someone else, proposed -> "Ongoing" from
// work done before it closed.
func TestSuppressSettledStatus_ADoneIssueIsNeverReopened(t *testing.T) {
	verified := []verifiedLink{liveAt("SUMO-2", "Closed", tasktracker.StatusDone)}

	kept, reasons := suppressSettledStatus(verified, []ProposedAction{transitionTo("SUMO-2", "Ongoing")})

	assert.Empty(t, kept)
	require.Len(t, reasons, 1)
	assert.Contains(t, reasons[0], "SUMO-2")
	assert.Contains(t, reasons[0], "Closed")
}

// Everything else passes through: a real move, a comment on a done issue (a note on a
// just-closed ticket is legitimate), and an action whose issue this filter cannot see.
func TestSuppressSettledStatus_LeavesEverythingElseAlone(t *testing.T) {
	verified := []verifiedLink{
		liveAt("PROJ-1", "In Progress", tasktracker.StatusInProgress),
		liveAt("PROJ-2", "Done", tasktracker.StatusDone),
	}
	drafted := []ProposedAction{
		transitionTo("PROJ-1", "In Review"),
		{Type: ActionComment, IssueKey: "PROJ-2", Body: "follow-up", Confidence: 0.8},
		transitionTo("PROJ-9", "In Review"),
	}

	kept, reasons := suppressSettledStatus(verified, drafted)

	assert.Equal(t, drafted, kept)
	assert.Empty(t, reasons)
}

// End to end: the guard runs in a real pass, with no status history collected for the
// issue, which is the case the staleness guard cannot judge.
func TestReconcile_AClosedIssueIsNotReopenedWithoutStatusHistory(t *testing.T) {
	s := reconcileStore(t)
	tracker := &fakeTracker{
		issues: map[string]tasktracker.Issue{
			"SUMO-2": {Key: "SUMO-2", Summary: "incident", StatusName: "Closed", StatusCategory: tasktracker.StatusDone},
		},
		transitions: map[string][]tasktracker.Transition{
			"SUMO-2": {{ToStatus: "Ongoing", ToCategory: tasktracker.StatusInProgress}},
		},
	}
	seedLinkedNarrative(t, s, "SUMO-2", store.Role("primary"), codeEvent("e1", "investigated the outage"))
	llmClient := &fakeLLM{responses: []string{
		`[{"issue_key":"SUMO-2","type":"transition","target_status":"Ongoing","confidence":0.6,"rationale":"work happened"}]`,
	}}

	results, _, err := Reconcile(t.Context(), s, tracker, llmClient,
		config.ReconcilerConfig{MaxNarrativesPerPass: 10, MinConfidenceToPropose: 0.5})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Empty(t, results[0].Proposed)
	require.Len(t, results[0].Suppressed, 1)
	assert.Contains(t, results[0].Suppressed[0], "reopening a done issue")
}
