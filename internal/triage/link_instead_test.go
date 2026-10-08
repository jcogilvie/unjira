package triage

// link_instead_test.go pins triage's "link instead" for a create proposed over work with
// no confident primary: [t]arget on the create, naming one of its candidate tickets.
// Retarget links the work there as the reviewer's primary and drafts for it, and the
// create is ruled against.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// createOverDoubtedPrimary turns wireStore's narrative into one whose primary DEVSBX-9
// is doubted, adds a cited DEVSBX-42, and proposes a create naming both.
func createOverDoubtedPrimary(t *testing.T) (*store.Store, int64, store.ActionRow) {
	t.Helper()

	s, nid, _ := wireStore(t)
	require.NoError(t, s.AddNarrativeIssues(nid, []store.NarrativeIssue{
		{IssueKey: "DEVSBX-9", Role: store.RolePrimary, Provenance: "prose_first", Confidence: 0.3},
		{IssueKey: "DEVSBX-42", Role: "mentioned", Provenance: "prose_later", Confidence: 0.8},
	}))

	aid, err := s.InsertAction(store.ActionRow{
		NarrativeID: nid, Type: "create",
		Payload: `{"summary":"s","description":"d","candidates":[` +
			`{"key":"DEVSBX-42","role":"mentioned","confidence":0.8,"provenance":"prose_later"},` +
			`{"key":"DEVSBX-9","role":"primary","confidence":0.3,"provenance":"prose_first"}]}`,
		Confidence: 0.8, Status: "proposed",
	})
	require.NoError(t, err)

	action, err := s.GetAction(aid)
	require.NoError(t, err)

	return s, nid, action
}

// TestStoreHandlerRetarget_LinksACreateToACandidateInsteadOfItsDoubtedPrimary: the
// narrative already holds a primary (the doubted one), and one_primary_per_narrative
// admits one, so linking another as primary must demote it in the same transaction, or
// the reviewer's link-instead would fail on the index. The doubted row keeps its
// provenance and confidence; only its role changes.
func TestStoreHandlerRetarget_LinksACreateToACandidateInsteadOfItsDoubtedPrimary(t *testing.T) {
	s, nid, create := createOverDoubtedPrimary(t)

	tracker := &wireTracker{issues: map[string]tasktracker.Issue{
		"DEVSBX-9":  {Key: "DEVSBX-9", StatusName: "Done"},
		"DEVSBX-42": {Key: "DEVSBX-42", StatusName: "In Progress", Summary: "the right ticket"},
	}}
	client := &wireLLM{responses: []string{
		`[{"issue_key":"DEVSBX-42","type":"comment","body":"the work","confidence":0.9,"rationale":"r"}]`,
	}}

	h := NewStoreHandler(s, tracker, client, nil, testCorrelatorConfig(), 100000)

	got, err := h.Retarget(context.Background(), create, "DEVSBX-42")
	require.NoError(t, err)
	assert.Equal(t, "comment", got.Type, "the create is replaced by a draft for the linked ticket")
	assert.Equal(t, "DEVSBX-42", got.IssueKey)
	require.NotEmpty(t, client.prompts)
	assert.Contains(t, client.prompts[0], "DEVSBX-42")
	assert.NotContains(t, client.prompts[0], "reattributed this work from  to",
		"a create has no old ticket to name")

	links, err := s.NarrativeIssues(nid)
	require.NoError(t, err)

	byKey := map[string]store.NarrativeIssue{}
	for _, l := range links {
		byKey[l.IssueKey] = l
	}

	assert.Equal(t, store.NarrativeIssue{
		IssueKey: "DEVSBX-42", Role: store.RolePrimary, Provenance: "reviewer", Confidence: 1,
	}, byKey["DEVSBX-42"], "the reviewer's link is the primary")
	assert.Equal(t, store.Role("mentioned"), byKey["DEVSBX-9"].Role, "the doubted primary is demoted")
	assert.Equal(t, "prose_first", byKey["DEVSBX-9"].Provenance)
	assert.InDelta(t, 0.3, byKey["DEVSBX-9"].Confidence, 1e-9)

	ruled, err := s.GetAction(create.ID)
	require.NoError(t, err)
	assert.Equal(t, store.StatusRejected, ruled.Status, "the create is ruled against, not left open")
}
