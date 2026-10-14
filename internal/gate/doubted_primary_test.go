package gate_test

// doubted_primary_test.go pins what the F13 backstop means for a create proposed over a
// primary the model itself doubted (below match.confidence_floor, finding F64).
//
// That primary IS a primary row, and the backstop refuses a create once a primary
// exists. Refusing it would refuse the very creates the reconciler now proposes for
// such work. So the payload records the links the create was proposed over, and the
// backstop lets through exactly one primary: the one the proposal names, unchanged. A
// primary linked or changed after the proposal (a reviewer's [t]arget, a re-match, a
// promotion) is new information the reviewer did not see, and the create is refused as
// before. The floor itself is not consulted here: it was applied when the proposal was
// drafted, and a config change between proposal and approval must not turn an approved
// ticket into a refused one or the reverse.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/gate"
	"github.com/jcogilvie/unjira/internal/store"
)

// doubtedPayload is a create proposed over PROJ-7 as a primary at 0.3 and PROJ-8 as a
// mentioned citation.
const doubtedPayload = `{"summary":"s","description":"d","candidates":[` +
	`{"key":"PROJ-7","role":"primary","confidence":0.3,"provenance":"prose_first"},` +
	`{"key":"PROJ-8","role":"mentioned","confidence":0.9,"provenance":"prose_later"}]}`

func doubtedLinks() []store.NarrativeIssue {
	return []store.NarrativeIssue{
		{IssueKey: "PROJ-7", Role: store.RolePrimary, Provenance: "prose_first", Confidence: 0.3},
		{IssueKey: "PROJ-8", Role: "mentioned", Provenance: "prose_later", Confidence: 0.9},
	}
}

// TestApplier_Create_AppliesOverTheDoubtedPrimaryItWasProposedOver: the new ticket
// becomes the narrative's primary, and the doubted one is demoted to `mentioned` with
// its provenance and confidence kept, so nothing about what matching recorded is lost
// and the payload still holds its original role. one_primary_per_narrative allows one
// primary, and approving the create is the reviewer's ruling that the new ticket is it.
func TestApplier_Create_AppliesOverTheDoubtedPrimaryItWasProposedOver(t *testing.T) {
	s := applierStore(t)
	w := &fakeWriter{}
	action := insertAction(t, s, store.ActionRow{Type: "create", Payload: doubtedPayload})
	require.NoError(t, s.AddNarrativeIssues(action.NarrativeID, doubtedLinks()))

	applier := gate.NewApplier(s, w, "PROJ", writableTrackers("PROJ"))
	require.NoError(t, applier.Apply(action))
	assert.Len(t, w.calls, 1, "the doubted primary the reviewer saw does not block the create")

	links, err := s.NarrativeIssues(action.NarrativeID)
	require.NoError(t, err)

	byKey := map[string]store.NarrativeIssue{}
	for _, l := range links {
		byKey[l.IssueKey] = l
	}

	require.Len(t, byKey, 3)
	assert.Equal(t, store.RolePrimary, byKey["NEW-1"].Role, "the created ticket is the work's home")
	assert.Equal(t, store.NarrativeIssue{
		IssueKey: "PROJ-7", Role: "mentioned", Provenance: "prose_first", Confidence: 0.3,
	}, byKey["PROJ-7"], "demoted, with what matching recorded kept")
	assert.Equal(t, store.Role("mentioned"), byKey["PROJ-8"].Role)
}

// TestApplier_Create_RefusesWhenThePrimaryChangedAfterTheProposal: each way the primary
// can differ from the one the proposal names is refused before any write, because the
// reviewer approved a ticket in light of links that are no longer the narrative's.
func TestApplier_Create_RefusesWhenThePrimaryChangedAfterTheProposal(t *testing.T) {
	cases := []struct {
		name    string
		primary store.NarrativeIssue
	}{
		{
			name:    "a reviewer linked another ticket",
			primary: store.NarrativeIssue{IssueKey: "PROJ-9", Role: store.RolePrimary, Provenance: "reviewer", Confidence: 1},
		},
		{
			name:    "a reviewer confirmed the doubted ticket",
			primary: store.NarrativeIssue{IssueKey: "PROJ-7", Role: store.RolePrimary, Provenance: "reviewer", Confidence: 1},
		},
		{
			name:    "the doubted primary was promoted",
			primary: store.NarrativeIssue{IssueKey: "PROJ-7", Role: store.RolePrimary, Provenance: "prose_first", Confidence: 0.9},
		},
		{
			name:    "a cited ticket became the primary",
			primary: store.NarrativeIssue{IssueKey: "PROJ-8", Role: store.RolePrimary, Provenance: "prose_later", Confidence: 0.9},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := applierStore(t)
			w := &fakeWriter{}
			action := insertAction(t, s, store.ActionRow{Type: "create", Payload: doubtedPayload})

			links := []store.NarrativeIssue{tc.primary}
			if tc.primary.IssueKey != "PROJ-7" {
				links = append(links, store.NarrativeIssue{
					IssueKey: "PROJ-7", Role: "mentioned", Provenance: "prose_first", Confidence: 0.3,
				})
			}
			require.NoError(t, s.AddNarrativeIssues(action.NarrativeID, links))

			applier := gate.NewApplier(s, w, "PROJ", writableTrackers("PROJ"))
			err := applier.Apply(action)

			require.Error(t, err, "a primary the reviewer never saw must refuse the create")
			assert.Contains(t, err.Error(), tc.primary.IssueKey)
			assert.Empty(t, w.calls, "refused BEFORE the write: a duplicate ticket cannot be un-opened")
		})
	}
}

// TestApplier_Create_APayloadWithoutCandidatesRefusesAnyPrimary: a create proposed for
// work with no link at all (and every create persisted before candidates existed) names
// no doubted primary, so any primary refuses it, which is the backstop as it always was.
func TestApplier_Create_APayloadWithoutCandidatesRefusesAnyPrimary(t *testing.T) {
	s := applierStore(t)
	w := &fakeWriter{}
	action := insertAction(t, s, store.ActionRow{Type: "create", Payload: `{"summary":"s","description":"d"}`})
	require.NoError(t, s.AddNarrativeIssues(action.NarrativeID, doubtedLinks()))

	applier := gate.NewApplier(s, w, "PROJ", writableTrackers("PROJ"))

	err := applier.Apply(action)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PROJ-7")
	assert.Empty(t, w.calls)
}

// TestApplier_Create_ACandidateNamedAsMentionedDoesNotExcuseAPrimary: the exemption is
// for the primary the proposal names AS a primary. A ticket it named as a citation
// that has since become the primary is a change, whatever its confidence.
func TestApplier_Create_ACandidateNamedAsMentionedDoesNotExcuseAPrimary(t *testing.T) {
	s := applierStore(t)
	w := &fakeWriter{}
	action := insertAction(t, s, store.ActionRow{Type: "create", Payload: `{"summary":"s","description":"d",` +
		`"candidates":[{"key":"PROJ-8","role":"mentioned","confidence":0.3,"provenance":"prose_later"}]}`})
	require.NoError(t, s.AddNarrativeIssues(action.NarrativeID, []store.NarrativeIssue{
		{IssueKey: "PROJ-8", Role: store.RolePrimary, Provenance: "prose_later", Confidence: 0.3},
	}))

	applier := gate.NewApplier(s, w, "PROJ", writableTrackers("PROJ"))

	require.Error(t, applier.Apply(action))
	assert.Empty(t, w.calls)
}
