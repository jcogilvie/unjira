package reconciler

// no_confident_primary_test.go pins one rule from both of its sides: a narrative with no
// confident primary is untracked work. "No confident primary" is every link set short of
// a primary at or above match.confidence_floor — only `mentioned` citations (finding
// F58), a `same_work` with no primary beside it, or a primary the model itself doubted
// (finding F64).
//
// The reconciler drafts onto none of those links, and records why, so the narrative
// does not hold a slot. The create path proposes a ticket for the work and names every
// such link as a candidate, so a reviewer can approve the new ticket or link the work to
// one of them instead.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

const exampleFloor = 0.7

// seedWithLinks inserts a narrative with one member event and exactly links.
func seedWithLinks(t *testing.T, s *store.Store, extID string, offset time.Duration, links ...store.NarrativeIssue) int64 {
	t.Helper()

	base := time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC).Add(offset)
	id, err := s.InsertNarrative(base, base.Add(time.Hour), "work "+extID, "summary of "+extID)
	require.NoError(t, err)
	linkEventToNarrative(t, s, id, codeEvent(extID, "wrote code for "+extID))

	if len(links) > 0 {
		require.NoError(t, s.AddNarrativeIssues(id, links))
	}

	return id
}

func link(key string, role store.Role, confidence float64) store.NarrativeIssue {
	return store.NarrativeIssue{IssueKey: key, Role: role, Provenance: "prose_first", Confidence: confidence}
}

// TestReconcile_NoConfidentPrimaryIsNotDraftedOnto: a sub-floor primary, its same_work
// partner, and a same_work with no primary at all are each drafted for by nobody here.
// No tracker read, no model call. The same_work links share the primary's doubt: a
// same_work link is the same work recorded again, so with no confident first record it
// is not a confident home either, and drafting onto it alone would put the story on the
// paired ticket while the one it pairs with is in doubt.
func TestReconcile_NoConfidentPrimaryIsNotDraftedOnto(t *testing.T) {
	cases := []struct {
		name   string
		links  []store.NarrativeIssue
		reason string
	}{
		{
			name: "primary below the floor, with a confident same_work",
			links: []store.NarrativeIssue{
				link("PROJ-1", store.RolePrimary, 0.3), link("PROJ-2", "same_work", 0.95),
			},
			reason: "PROJ-1 at confidence 0.30 is below match.confidence_floor 0.70",
		},
		{
			name:   "same_work with no primary",
			links:  []store.NarrativeIssue{link("PROJ-2", "same_work", 0.95)},
			reason: "no primary link",
		},
		{
			name:   "mentioned only",
			links:  []store.NarrativeIssue{link("PROJ-9", "mentioned", 0.9)},
			reason: "every link is mentioned",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := reconcileStore(t)
			tracker := &fakeTracker{issues: map[string]tasktracker.Issue{
				"PROJ-1": {Key: "PROJ-1"}, "PROJ-2": {Key: "PROJ-2"}, "PROJ-9": {Key: "PROJ-9"},
			}}
			client := &fakeLLM{responses: []string{
				`[{"issue_key":"PROJ-1","type":"comment","body":"b","confidence":0.95,"rationale":"r"}]`,
			}}

			nid := seedWithLinks(t, s, "ncp:1", 0, tc.links...)

			results, _, err := Reconcile(t.Context(), s, tracker, client, testConfig(),
				WithConfidenceFloor(exampleFloor))
			require.NoError(t, err)
			require.Len(t, results, 1, "considered, so it gets its result row")
			assert.Equal(t, nid, results[0].NarrativeID)
			assert.Empty(t, results[0].Proposed, "no confident home, so nothing is drafted onto any link")
			assert.Empty(t, client.prompts, "and no model call is spent drafting")
			assert.Empty(t, tracker.getCalls, "and no link is read")
			require.Len(t, results[0].Notes, 1, "the reason is reported, not silent")
			assert.Contains(t, results[0].Notes[0], tc.reason)
			assert.Contains(t, results[0].Notes[0], "create candidate")

			remaining, err := s.CountNarrativesWithDelta(SelectionRoles)
			require.NoError(t, err)
			assert.Zero(t, remaining, "recorded as examined, so it is not counted as unexamined work")

			again, _, err := Reconcile(t.Context(), s, tracker, client, testConfig(),
				WithConfidenceFloor(exampleFloor))
			require.NoError(t, err)
			assert.Empty(t, again, "and not selected again until a member event is linked after it")

			linkEventToNarrative(t, s, nid, codeEvent("ncp:2", "more work"))

			readmitted, _, err := Reconcile(t.Context(), s, tracker, client, testConfig(),
				WithConfidenceFloor(exampleFloor))
			require.NoError(t, err)
			require.Len(t, readmitted, 1, "a watermark, not a tombstone")
			assert.Empty(t, readmitted[0].Proposed, "still no confident home")
		})
	}
}

// TestReconcile_AConfidentPrimaryIsDraftedOnto: at the floor is confident, as matching
// itself promotes at confidence >= floor. With no floor option every primary is
// confident, which is how an unset match.confidence_floor reads.
func TestReconcile_AConfidentPrimaryIsDraftedOnto(t *testing.T) {
	cases := []struct {
		name       string
		confidence float64
		opts       []ReconcileOption
	}{
		{"at the floor", exampleFloor, []ReconcileOption{WithConfidenceFloor(exampleFloor)}},
		{"above the floor", 0.9, []ReconcileOption{WithConfidenceFloor(exampleFloor)}},
		{"no floor configured", 0.1, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := reconcileStore(t)
			tracker := &fakeTracker{issues: map[string]tasktracker.Issue{"PROJ-1": {Key: "PROJ-1"}}}
			client := &fakeLLM{responses: []string{
				`[{"issue_key":"PROJ-1","type":"comment","body":"b","confidence":0.9,"rationale":"r"}]`,
			}}

			seedWithLinks(t, s, "cp:1", 0, link("PROJ-1", store.RolePrimary, tc.confidence))

			results, _, err := Reconcile(t.Context(), s, tracker, client, testConfig(), tc.opts...)
			require.NoError(t, err)
			require.Len(t, results, 1)
			require.Len(t, results[0].Proposed, 1, "a confident primary is drafted onto as before")
			assert.Equal(t, "PROJ-1", results[0].Proposed[0].IssueKey)
		})
	}
}

// candidateTracker is a fakeTracker carrying the two tickets the candidate tests name.
func candidateTracker() *fakeTracker {
	return &fakeTracker{issues: map[string]tasktracker.Issue{
		"PROJ-1": {Key: "PROJ-1", Summary: "Rotate the signing keys", StatusName: "In Progress"},
		"PROJ-2": {Key: "PROJ-2", Summary: "Change record for the key rotation", StatusName: "Open"},
		"PROJ-9": {Key: "PROJ-9", Summary: "Flaky retry test", StatusName: "Done"},
	}}
}

// TestProposeCreates_NamesTheCandidatesOfWorkWithNoConfidentPrimary: both shapes of
// "no confident primary" reach the create path, and each proposal carries every link as
// a candidate (key, role, confidence, provenance, and the ticket's summary and status
// read live), both on the proposal and in its payload, so the review queue can show
// them. The prompt shows the model the same candidates and says the reviewer will see
// them, so it can judge whether the work deserves its own ticket.
func TestProposeCreates_NamesTheCandidatesOfWorkWithNoConfidentPrimary(t *testing.T) {
	s := reconcileStore(t)
	citesOnly := seedWithLinks(t, s, "f58:1", 0, link("PROJ-9", "mentioned", 0.8))
	doubted := seedWithLinks(t, s, "f64:1", time.Minute,
		link("PROJ-1", store.RolePrimary, 0.3), link("PROJ-2", "same_work", 0.9))
	seedWithLinks(t, s, "tracked:1", 2*time.Minute, link("PROJ-1", store.RolePrimary, 0.9))

	client := &fakeLLM{responses: []string{worthTracking}}

	got, _, err := ProposeCreates(context.Background(), s, client, testConfig(), nil, nil,
		WithConfidenceFloor(exampleFloor), WithCandidateReader(candidateTracker()))
	require.NoError(t, err)
	assert.Equal(t, []int64{citesOnly, doubted}, resultIDs(got),
		"the confidently linked narrative is tracked work and is not a create candidate")

	require.Len(t, got[1].Proposed, 1)
	proposal := got[1].Proposed[0]
	assert.Equal(t, ActionCreate, proposal.Type)
	assert.Equal(t, []CreateCandidate{
		{
			IssueKey: "PROJ-1", Role: "primary", Confidence: 0.3, Provenance: "prose_first",
			Summary: "Rotate the signing keys", Status: "In Progress",
		},
		{
			IssueKey: "PROJ-2", Role: "same_work", Confidence: 0.9, Provenance: "prose_first",
			Summary: "Change record for the key rotation", Status: "Open",
		},
	}, proposal.Candidates)

	require.Len(t, client.prompts, 2)
	prompt := client.prompts[1]
	for _, want := range []string{
		"PROJ-1", "primary", "0.30", "Rotate the signing keys", "In Progress",
		"PROJ-2", "same_work", "0.90", "reviewer",
	} {
		assert.Contains(t, prompt, want, "the create prompt must show the candidates")
	}
	assert.NotContains(t, prompt, "no tracker issue exists for any of it",
		"the prompt must not tell the model no ticket exists when it lists some")

	payload, err := ActionPayload(proposal)
	require.NoError(t, err)

	var decoded struct {
		Candidates []CreateCandidate `json:"candidates"`
	}
	require.NoError(t, json.Unmarshal([]byte(payload), &decoded))
	assert.Equal(t, proposal.Candidates, decoded.Candidates, "the payload carries the candidates")
	assert.Equal(t, proposal.Candidates, CreateCandidatesOf(payload))

	require.Len(t, got[0].Proposed, 1)
	assert.Equal(t, []CreateCandidate{{
		IssueKey: "PROJ-9", Role: "mentioned", Confidence: 0.8, Provenance: "prose_first",
		Summary: "Flaky retry test", Status: "Done",
	}}, got[0].Proposed[0].Candidates)
}

// TestProposeCreates_AnUntrackedNarrativeNamesNoCandidates: with no link at all the
// prompt and payload are what they always were.
func TestProposeCreates_AnUntrackedNarrativeNamesNoCandidates(t *testing.T) {
	s := reconcileStore(t)
	seedWithLinks(t, s, "none:1", 0)

	client := &fakeLLM{responses: []string{worthTracking}}

	got, _, err := ProposeCreates(context.Background(), s, client, testConfig(), nil, nil,
		WithConfidenceFloor(exampleFloor), WithCandidateReader(candidateTracker()))
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Len(t, got[0].Proposed, 1)
	assert.Empty(t, got[0].Proposed[0].Candidates)
	assert.Contains(t, client.prompts[0], "no tracker issue exists for any of it")

	payload, err := ActionPayload(got[0].Proposed[0])
	require.NoError(t, err)
	assert.NotContains(t, payload, "candidates", "a create with no candidates keeps the earlier payload shape")
}

// TestProposeCreates_AnUnreadableCandidateIsStillNamed: a candidate the tracker reports
// as missing is listed with that fact rather than dropped. Dropping it would show the
// reviewer fewer tickets than matching linked.
func TestProposeCreates_AnUnreadableCandidateIsStillNamed(t *testing.T) {
	s := reconcileStore(t)
	seedWithLinks(t, s, "gone:1", 0, link("PROJ-404", "mentioned", 0.6))

	client := &fakeLLM{responses: []string{worthTracking}}

	got, _, err := ProposeCreates(context.Background(), s, client, testConfig(), nil, nil,
		WithConfidenceFloor(exampleFloor), WithCandidateReader(candidateTracker()))
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Len(t, got[0].Proposed, 1)
	require.Len(t, got[0].Proposed[0].Candidates, 1)
	assert.Equal(t, "PROJ-404", got[0].Proposed[0].Candidates[0].IssueKey)
	assert.NotEmpty(t, got[0].Proposed[0].Candidates[0].Unread)
	assert.Contains(t, client.prompts[0], "PROJ-404")
}

// TestProposeCreates_ACandidateTransportErrorFailsThatNarrativeOnly: an unreachable
// tracker is not an answer about the ticket, so nothing is proposed from a partial
// picture; the narrative is left for the next pass, and the others still get theirs.
func TestProposeCreates_ACandidateTransportErrorFailsThatNarrativeOnly(t *testing.T) {
	s := reconcileStore(t)
	seedWithLinks(t, s, "down:1", 0, link("PROJ-1", "mentioned", 0.6))
	healthy := seedWithLinks(t, s, "up:1", time.Minute, link("PROJ-9", "mentioned", 0.6))

	tracker := candidateTracker()
	tracker.getErr = map[string]error{"PROJ-1": errors.New("dial tcp: connection refused")}
	client := &fakeLLM{responses: []string{worthTracking}}

	got, _, err := ProposeCreates(context.Background(), s, client, testConfig(), nil, nil,
		WithConfidenceFloor(exampleFloor), WithCandidateReader(tracker))
	require.NoError(t, err, "per narrative, as every create failure is")
	require.Len(t, got, 2)
	assert.Empty(t, got[0].Proposed, "no proposal from a candidate list the tracker could not answer for")
	require.Len(t, got[1].Proposed, 1)
	assert.Equal(t, healthy, got[1].NarrativeID)
	assert.Len(t, client.prompts, 1, "the failed narrative cost no model call")
}

// TestProposeCreates_WithoutAReaderTheCandidatesAreStillNamed: the reader only describes
// the candidates. A caller without one still gets their keys, roles and confidences.
func TestProposeCreates_WithoutAReaderTheCandidatesAreStillNamed(t *testing.T) {
	s := reconcileStore(t)
	seedWithLinks(t, s, "noreader:1", 0, link("PROJ-9", "mentioned", 0.6))

	client := &fakeLLM{responses: []string{worthTracking}}

	got, _, err := ProposeCreates(context.Background(), s, client, testConfig(), nil, nil,
		WithConfidenceFloor(exampleFloor))
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Len(t, got[0].Proposed, 1)
	assert.Equal(t, []CreateCandidate{
		{IssueKey: "PROJ-9", Role: "mentioned", Confidence: 0.6, Provenance: "prose_first"},
	}, got[0].Proposed[0].Candidates)
}

// TestProposeCreateOne_AConfidentPrimaryIsNeverProposedFor: the selector already
// excludes a confidently linked narrative; this is the Go backstop, for the same reason
// the others stay (a selector bug would cost a duplicate ticket).
func TestProposeCreateOne_AConfidentPrimaryIsNeverProposedFor(t *testing.T) {
	s := reconcileStore(t)
	nid := seedWithLinks(t, s, "bs:1", 0, link("PROJ-1", store.RolePrimary, 0.9))

	awaiting, err := s.NarrativesAwaitingCreate(10, exampleFloor)
	require.NoError(t, err)
	require.Empty(t, awaiting, "precondition: the selector already excludes it")

	client := &fakeLLM{responses: []string{worthTracking}}
	got, _, err := proposeCreateOne(context.Background(), s, client, store.NarrativeRow{ID: nid}, nil,
		&reconcileOptions{floor: exampleFloor})

	require.NoError(t, err)
	assert.Empty(t, got.Proposed)
	assert.Empty(t, client.prompts, "a backstop skip costs no model call")
	require.Len(t, got.Notes, 1)
	assert.Contains(t, got.Notes[0], "PROJ-1")
}
