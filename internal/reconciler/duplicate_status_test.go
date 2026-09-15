package reconciler

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/store"
)

// TestOpenProposalFromAnother_RecognizesTheStoredProposedStatus is the guard
// task #182/F2 was missing: openProposalFromAnother's only signal is a
// second narrative's action sitting at status=StatusProposed. A drill that
// broke this comparison (comparing against a status no writer ever produces)
// passed every existing test in the repo — suppressDuplicates has no direct
// test, and the only place its message text appears
// (pipeline.TestRenderReconcileResultNamesWhatWasSuppressedAndWhy) builds the
// ReconcileResult by hand rather than calling the filter. This test closes
// that gap directly.
func TestOpenProposalFromAnother_RecognizesTheStoredProposedStatus(t *testing.T) {
	s := reconcileStore(t)

	other := seedLinkedNarrative(t, s, "PROJ-1", store.Role("primary"), codeEvent("e1", "other narrative's work"))
	_, err := s.InsertAction(store.ActionRow{
		NarrativeID: other, Type: "comment", IssueKey: "PROJ-1",
		Payload: `{"body":"already proposed"}`, Status: StatusProposed,
	})
	require.NoError(t, err)

	refs, err := s.NarrativesForIssue("PROJ-1")
	require.NoError(t, err)
	require.NotEmpty(t, refs, "precondition: the other narrative's link is visible via the reverse lookup")

	got := openProposalFromAnother(s, refs, other+1, "PROJ-1", nil)

	assert.True(t, got,
		"a status=StatusProposed action on another narrative sharing this issue must be found")
}

// TestOpenProposalFromAnother_IgnoresATerminalStatus: an applied or rejected
// action on the other narrative is not an OPEN proposal, so it must not
// suppress a fresh one on this narrative.
func TestOpenProposalFromAnother_IgnoresATerminalStatus(t *testing.T) {
	s := reconcileStore(t)

	other := seedLinkedNarrative(t, s, "PROJ-1", store.Role("primary"), codeEvent("e1", "other narrative's work"))
	_, err := s.InsertAction(store.ActionRow{
		NarrativeID: other, Type: "comment", IssueKey: "PROJ-1",
		Payload: `{"body":"already applied"}`, Status: store.StatusApplied,
	})
	require.NoError(t, err)

	refs, err := s.NarrativesForIssue("PROJ-1")
	require.NoError(t, err)

	got := openProposalFromAnother(s, refs, other+1, "PROJ-1", nil)

	assert.False(t, got, "an applied action already reached the tracker; it is not an open proposal")
}

// TestSuppressDuplicates_SuppressesACommentWhenAnotherNarrativeHasAnOpenProposal
// exercises suppressDuplicates itself (rather than only its helper), pinning
// the message text pipeline's rendering test assumes exists.
func TestSuppressDuplicates_SuppressesACommentWhenAnotherNarrativeHasAnOpenProposal(t *testing.T) {
	s := reconcileStore(t)

	other := seedLinkedNarrative(t, s, "PROJ-1", store.Role("primary"), codeEvent("e1", "other narrative's work"))
	_, err := s.InsertAction(store.ActionRow{
		NarrativeID: other, Type: "comment", IssueKey: "PROJ-1",
		Payload: `{"body":"already proposed"}`, Status: StatusProposed,
	})
	require.NoError(t, err)

	drafted := []ProposedAction{{Type: ActionComment, IssueKey: "PROJ-1", Body: "a fresh comment"}}

	kept, suppressed := suppressDuplicates(s, other+1, drafted, nil)

	assert.Empty(t, kept)
	require.Len(t, suppressed, 1)
	assert.Contains(t, suppressed[0], "already has an open proposal")
}
