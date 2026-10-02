package correlator_test

// cluster_dispute_test.go pins the shared-context design's member/context contract
// (docs/superpowers/specs/2026-10-02-shared-context-design.md): every numbered event
// gets exactly one member home, context_indices add background without moving
// anything, and an event two clusters claim as work is resolved by asking the model —
// never by response order.

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/events"
)

// doubleAssignment is the coverage fixture's response placing pr-7-opened (index 2)
// in BOTH clusters: extends 9 (position 0) and the new PR 7 cluster (position 1).
const doubleAssignment = `[{"kind":"extends","narrative_id":9,"summary":"cache","confidence":0.9,"event_indices":[0,1,2]},` +
	`{"kind":"new","title":"PR 7","summary":"PR 7 opened and merged","confidence":0.85,"event_indices":[2,3]}]`

func idsOf(evts []correlator.Event) []string {
	out := make([]string, 0, len(evts))
	for _, e := range evts {
		out = append(out, e.Source+"/"+e.ExternalID)
	}

	return out
}

// TestCluster_DisputeGivesOneMemberHomeAndContextElsewhere is the spec's first test:
// a double assignment resolves into exactly one member home, the other claimant keeps
// the event as context, and the winner records the dispute's per-event confidence.
func TestCluster_DisputeGivesOneMemberHomeAndContextElsewhere(t *testing.T) {
	f := newCoverageFixture(t)
	client := &fakeLLM{responses: []string{
		doubleAssignment,
		`[{"rationale":"it is PR 7's own opening","event_index":0,"member":{"cluster_position":1},"confidence":0.8}]`,
	}}

	results, stats, err := f.cluster(t, client)

	require.NoError(t, err)
	require.Len(t, client.prompts, 2, "one clustering call, one dispute re-ask")
	require.Len(t, results, 2)
	assert.Equal(t, []string{"claude_code/e0", "claude_code/e1"}, idsOf(results[0].Events))
	assert.Equal(t, []string{"github/pr-7-opened"}, idsOf(results[0].ContextEvents),
		"the losing claimant keeps the event as background")
	assert.Equal(t, []string{"github/pr-7-opened", "github/pr-7-merged"}, idsOf(results[1].Events))
	assert.Empty(t, results[1].ContextEvents)

	opened := results[1].Events[0]
	assert.InDelta(t, 0.8, results[1].ConfidenceOf(opened), 1e-9, "the dispute's per-event confidence")
	assert.InDelta(t, 0.85, results[1].ConfidenceOf(results[1].Events[1]), 1e-9, "the rest keep the cluster's")

	assert.Equal(t, 1, stats.DisputedEvents)
	require.Len(t, stats.Disputes, 1)
	assert.Equal(t, "github/pr-7-opened", stats.Disputes[0].Event)
	assert.Equal(t, `new "PR 7"`, stats.Disputes[0].Chosen)
	assert.Equal(t, []string{"narrative 9", `new "PR 7"`}, stats.Disputes[0].Claimants)
	assert.Equal(t, "it is PR 7's own opening", stats.Disputes[0].Rationale)
	assert.Equal(t, 2, stats.Calls)
}

// TestCluster_DisputeWinnerIsTheModelsChoiceNotResponseOrder is the spec's "not
// response order" test, both ways round: the model picking the LATER claimant must
// win (which first-in-response-order would get wrong), and so must the model picking
// the EARLIER one (which last-writer-wins would get wrong).
func TestCluster_DisputeWinnerIsTheModelsChoiceNotResponseOrder(t *testing.T) {
	tests := []struct {
		name       string
		chosen     string
		wantMember int // result position that must hold pr-7-opened as a member
		wantCtx    int // result position that must hold it as context
	}{
		{"the later claimant", "1", 1, 0},
		{"the earlier claimant", "0", 0, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newCoverageFixture(t)
			client := &fakeLLM{responses: []string{
				doubleAssignment,
				`[{"rationale":"r","event_index":0,"member":{"cluster_position":` + tt.chosen + `},"confidence":0.6}]`,
			}}

			results, _, err := f.cluster(t, client)

			require.NoError(t, err)
			assert.Contains(t, idsOf(results[tt.wantMember].Events), "github/pr-7-opened",
				"the model's choice keeps the member link")
			assert.NotContains(t, idsOf(results[tt.wantCtx].Events), "github/pr-7-opened",
				"the other claimant does not")
			assert.Contains(t, idsOf(results[tt.wantCtx].ContextEvents), "github/pr-7-opened")
		})
	}
}

// TestCluster_DisputeThatEmptiesANewClusterFailsLoudly is F37's shape,
// [NEW{E}, EXTENDS 9{E, F}]: if the model gives E to the extend, the new cluster has
// no work left. That is malformed, and the pass fails naming both clusters — never an
// empty open narrative persisted with a title describing an event it does not hold.
func TestCluster_DisputeThatEmptiesANewClusterFailsLoudly(t *testing.T) {
	f := newCoverageFixture(t)
	client := &fakeLLM{responses: []string{
		`[{"kind":"new","title":"Just E","summary":"s","confidence":0.9,"event_indices":[2]},` +
			`{"kind":"extends","narrative_id":9,"summary":"s","confidence":0.9,"event_indices":[0,1,2,3]}]`,
		`[{"rationale":"r","event_index":0,"member":{"cluster_position":1},"confidence":0.9}]`,
	}}

	results, _, err := f.cluster(t, client)

	require.Error(t, err)
	assert.Nil(t, results)
	require.ErrorContains(t, err, `new "Just E"`)
	require.ErrorContains(t, err, "narrative 9")
	require.ErrorContains(t, err, "F37")
}

func TestCluster_DisputePromptCarriesWhatTheModelNeeds(t *testing.T) {
	f := newCoverageFixture(t)
	client := &fakeLLM{responses: []string{
		doubleAssignment,
		`[{"rationale":"r","event_index":0,"member":{"cluster_position":1},"confidence":0.8}]`,
	}}

	_, _, err := correlator.Cluster(t.Context(), f.evts, f.existing, client, f.window, 128000,
		correlator.WithInstruction("SENTINEL INSTRUCTION"))
	require.NoError(t, err)
	require.Len(t, client.prompts, 2)

	prompt := client.prompts[1]
	assert.Contains(t, prompt, `event_index=0 [github] "PR #7 opened"`)
	assert.Contains(t, prompt, `cluster_position=0 kind=extends narrative_id=9 summary="cache"`)
	assert.Contains(t, prompt, `cluster_position=1 kind=new title="PR 7"`)
	assert.Contains(t, prompt, `"PR #7 merged"`, "claimants are described by their other member events")

	sys := client.systemPrompts[1]
	assert.Less(t, strings.Index(sys, `"rationale"`), strings.Index(sys, `"member"`),
		"the schema asks for the rationale BEFORE the decision")
	assert.Contains(t, sys, "SENTINEL INSTRUCTION", "a reviewer's instruction governs the dispute too")
	assert.Contains(t, sys, "not by where it was listed first")
}

// TestCluster_DisputePresentsEvidenceWithoutApplyingIt: the event carries a
// claimant's pull request and branch. That is shown to the model, and the model's
// answer — here, AGAINST the evidence — is what holds.
func TestCluster_DisputePresentsEvidenceWithoutApplyingIt(t *testing.T) {
	f := newCoverageFixture(t)
	opened := f.evts[2]
	opened.Artifacts[events.ArtifactPullRequest] = "github.com/jcogilvie/unjira#7"
	opened.Artifacts[events.ArtifactGitBranch] = "fix/cache"
	segment := f.evts[0]
	segment.Artifacts[events.ArtifactPullRequest] = "github.com/jcogilvie/unjira#7"
	segment.Artifacts[events.ArtifactGitBranch] = "fix/cache"

	client := &fakeLLM{responses: []string{
		// e0 (index 0) claimed by extends 9 and the new PR 7 cluster, which holds the PR.
		`[{"kind":"extends","narrative_id":9,"summary":"cache","confidence":0.9,"event_indices":[0,1]},` +
			`{"kind":"new","title":"PR 7","summary":"s","confidence":0.9,"event_indices":[0,2,3]}]`,
		`[{"rationale":"the investigation is cache work","event_index":0,"member":{"cluster_position":0},"confidence":0.7}]`,
	}}

	results, _, err := f.cluster(t, client)

	require.NoError(t, err)
	prompt := client.prompts[1]
	assert.Contains(t, prompt, "carries pull request github.com/jcogilvie/unjira#7, and so does cluster_position=1")
	assert.Contains(t, prompt, `recorded git branch "fix/cache", the head branch of pull request github.com/jcogilvie/unjira#7 in cluster_position=1`)
	assert.Contains(t, idsOf(results[0].Events), "claude_code/e0", "evidence is presented, never applied")
	assert.Contains(t, idsOf(results[1].ContextEvents), "claude_code/e0")
}

// TestCluster_DisputeIgnoresAHostlessPullRequest: the pre-F43 "<owner>/<repo>#<N>" value
// cannot tell two hosts apart, so two such values are not evidence of one PR. The
// dispute reads the artifact through events.PullRequestOf, as the pre-assignment join
// does, so the two readers agree on what counts as the same pull request.
func TestCluster_DisputeIgnoresAHostlessPullRequest(t *testing.T) {
	f := newCoverageFixture(t)
	f.evts[2].Artifacts[events.ArtifactPullRequest] = "jcogilvie/unjira#7"
	f.evts[0].Artifacts[events.ArtifactPullRequest] = "jcogilvie/unjira#7"

	client := &fakeLLM{responses: []string{
		`[{"kind":"extends","narrative_id":9,"summary":"cache","confidence":0.9,"event_indices":[0,1]},` +
			`{"kind":"new","title":"PR 7","summary":"s","confidence":0.9,"event_indices":[0,2,3]}]`,
		`[{"rationale":"r","event_index":0,"member":{"cluster_position":0},"confidence":0.7}]`,
	}}

	_, _, err := f.cluster(t, client)

	require.NoError(t, err)
	assert.NotContains(t, client.prompts[1], "carries pull request")
}

func TestCluster_MalformedDisputeAnswersErrorLoudly(t *testing.T) {
	tests := []struct {
		name    string
		answer  string
		wantErr string
	}{
		{"unresolved", `[]`, "1 disputed event(s) left unresolved: 0 (github/pr-7-opened)"},
		{
			"a non-claimant", `[{"rationale":"r","event_index":0,"member":{"cluster_position":2},"confidence":0.5}]`,
			"names cluster_position 2, which did not claim it",
		},
		{"no member", `[{"rationale":"r","event_index":0,"confidence":0.5}]`, "has no member.cluster_position"},
		{"no confidence", `[{"rationale":"r","event_index":0,"member":{"cluster_position":1}}]`, "needs a confidence"},
		{"no rationale", `[{"event_index":0,"member":{"cluster_position":1},"confidence":0.5}]`, "has no rationale"},
		{"answered twice", `[{"rationale":"r","event_index":0,"member":{"cluster_position":1},"confidence":0.5},` +
			`{"rationale":"r","event_index":0,"member":{"cluster_position":0},"confidence":0.5}]`, "answered twice"},
		{
			"out of range", `[{"rationale":"r","event_index":3,"member":{"cluster_position":1},"confidence":0.5}]`,
			"event_index 3 out of range [0,1)",
		},
		{"invalid JSON", `not json`, "parsing cluster dispute response"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newCoverageFixture(t)
			client := &fakeLLM{responses: []string{doubleAssignment, tt.answer}}

			results, _, err := f.cluster(t, client)

			require.Error(t, err)
			assert.Nil(t, results, "never a best-effort partial result")
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

// TestCluster_DisputeAcrossBisectedHalvesIsResolved is F36: a context narrative
// spanning the split point contributes its eligible event to BOTH halves' numbered
// slices, and each half places it in a different cluster. No single response holds
// the double placement, so only a pass-level check can see it — the dispute re-ask
// runs once, over the merged halves.
func TestCluster_DisputeAcrossBisectedHalvesIsResolved(t *testing.T) {
	// 600 characters each: large enough that the whole window does not fit the budget
	// below and bisects, small enough that the dispute prompt — which lists every
	// claimant's other members, from both halves — does (see F39).
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	evts := []correlator.Event{
		mustEvent(t, "claude_code", "a1", strings.Repeat("a", 600), base),
		mustEvent(t, "claude_code", "a2", strings.Repeat("b", 600), base.Add(time.Minute)),
		mustEvent(t, "claude_code", "b1", strings.Repeat("c", 600), base.Add(time.Hour)),
		mustEvent(t, "claude_code", "b2", strings.Repeat("d", 600), base.Add(time.Hour+time.Minute)),
	}
	spanning := mustEvent(t, "claude_code", "elig", "eligible work spanning the split", base.Add(30*time.Minute))
	existing := []correlator.Narrative{{
		ID: 9, Title: "spans the split", Summary: "s",
		WindowStart: base, WindowEnd: base.Add(2 * time.Hour),
		EligibleEvents: []correlator.Event{spanning},
	}}
	// Each half numbers [two in-window events, the eligible one] = indices 0, 1, 2.
	client := &fakeLLM{responses: []string{
		`[{"kind":"new","title":"A","summary":"s","confidence":0.9,"event_indices":[0,1,2]}]`,
		`[{"kind":"new","title":"B","summary":"s","confidence":0.9,"event_indices":[0,1,2]}]`,
		`{"same_story":false}`,
		`[{"rationale":"r","event_index":0,"member":{"cluster_position":1},"confidence":0.6}]`,
	}}

	sys, user := correlator.BuildClusterPromptForTest(append(evts[:3:3], spanning), existing)
	budget := correlator.EstimateTokensForTest(sys + user)

	results, stats, err := correlator.Cluster(t.Context(), evts, existing, client,
		correlator.TimeRange{Start: base, End: base.Add(2 * time.Hour)}, budget)

	require.NoError(t, err)
	require.Equal(t, 1, stats.Splits, "precondition: the window bisected")
	require.Len(t, client.prompts, 4, "two halves, one same-story check, ONE dispute re-ask")
	assert.Contains(t, client.prompts[3], `"eligible work spanning the split"`)
	assert.Equal(t, 1, stats.DisputedEvents)
	require.Len(t, results, 2)
	assert.NotContains(t, idsOf(results[0].Events), "claude_code/elig")
	assert.Contains(t, idsOf(results[0].ContextEvents), "claude_code/elig")
	assert.Contains(t, idsOf(results[1].Events), "claude_code/elig")
}

func TestCluster_ContextIndicesBecomeContextEvents(t *testing.T) {
	f := newCoverageFixture(t)
	client := &fakeLLM{responses: []string{
		// index 0 is a member of the extend AND (ignored) in its own context; it is
		// also background for PR 7. A repeated index is one placement.
		`[{"kind":"extends","narrative_id":9,"summary":"s","confidence":0.9,"event_indices":[0,1,1],"context_indices":[0]},` +
			`{"kind":"new","title":"PR 7","summary":"s","confidence":0.9,"event_indices":[2,3],"context_indices":[0,0]}]`,
	}}

	results, stats, err := f.cluster(t, client)

	require.NoError(t, err)
	require.Len(t, client.prompts, 1, "context is not a dispute and not an omission")
	assert.Zero(t, stats.DisputedEvents)
	assert.Equal(t, []string{"claude_code/e0", "claude_code/e1"}, idsOf(results[0].Events))
	assert.Empty(t, results[0].ContextEvents, "member wins within one cluster")
	assert.Equal(t, []string{"claude_code/e0"}, idsOf(results[1].ContextEvents))
}

func TestCluster_ConfidenceIsRequiredAndBounded(t *testing.T) {
	tests := []struct {
		name     string
		response string
		wantErr  string
	}{
		{"missing", `[{"kind":"new","title":"t","summary":"s","event_indices":[0,1,2,3]}]`, "has no confidence"},
		{"above one", `[{"kind":"new","title":"t","summary":"s","confidence":1.2,"event_indices":[0,1,2,3]}]`, "outside [0, 1]"},
		{
			"context index out of range", `[{"kind":"new","title":"t","summary":"s","confidence":0.5,"event_indices":[0,1,2,3],"context_indices":[9]}]`,
			"context_indices value 9 out of range",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newCoverageFixture(t)

			_, _, err := f.cluster(t, &fakeLLM{responses: []string{tt.response}})

			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

// TestCluster_ARepeatedAssignableEventIsAnError: an event in the window AND on a
// context narrative's eligible list would be numbered twice. That means the
// one-member-home invariant broke upstream; deduplicating would hide it, so Cluster
// refuses before spending a call.
func TestCluster_ARepeatedAssignableEventIsAnError(t *testing.T) {
	f := newCoverageFixture(t)
	f.existing[0].EligibleEvents = []correlator.Event{f.evts[1]}
	client := &fakeLLM{responses: []string{`[]`}}

	_, _, err := f.cluster(t, client)

	require.ErrorContains(t, err, "claude_code/e1 would be numbered twice")
	assert.Empty(t, client.prompts, "no model call on a broken index space")
}

// TestCluster_ContextLinksRenderUnderTheirOwnHeadingWithBackReferences (§5): a context
// narrative's context links render apart from its events; one that is also numbered
// (another narrative's eligible member) is a back-reference to its number, from the
// same slice the numbers come from.
func TestCluster_ContextLinksRenderUnderTheirOwnHeadingWithBackReferences(t *testing.T) {
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	window := correlator.TimeRange{Start: base, End: base.Add(time.Hour)}
	inWindow := mustEvent(t, "claude_code", "new-work", "new work", base.Add(time.Minute))
	shared := mustEvent(t, "claude_code", "investigation", "SHARED INVESTIGATION", base.Add(-time.Hour))
	elsewhere := mustEvent(t, "claude_code", "frozen-bg", "FROZEN BACKGROUND", base.Add(-2*time.Hour))
	existing := []correlator.Narrative{
		{
			ID: 1, Title: "A", Summary: "a", WindowStart: base.Add(-3 * time.Hour), WindowEnd: base,
			EligibleEvents: []correlator.Event{shared},
		},
		{
			ID: 2, Title: "B", Summary: "b", WindowStart: base.Add(-3 * time.Hour), WindowEnd: base,
			ContextEvents: []correlator.Event{shared, elsewhere},
		},
	}
	client := &fakeLLM{responses: []string{
		`[{"kind":"new","title":"t","summary":"s","confidence":0.9,"event_indices":[0]}]`,
	}}

	_, _, err := correlator.Cluster(t.Context(), []correlator.Event{inWindow}, existing, client, window, 128000)

	require.NoError(t, err)
	prompt := client.prompts[0]
	assert.Contains(t, prompt, `1. [claude_code] "SHARED INVESTIGATION"`, "the eligible member is numbered once")
	assert.Equal(t, 1, strings.Count(prompt, "SHARED INVESTIGATION"), "and rendered once")
	assert.Contains(t, prompt, "background (another narrative's work, linked here as context):\n    - -> #1\n")
	assert.Contains(t, prompt, `    - [claude_code] "FROZEN BACKGROUND"`, "an unnumbered context link renders in full")
}

func TestClusterSystemPrompt_StatesTheSummaryRule(t *testing.T) {
	prompt := correlator.ClusterSystemPromptForTest()

	assert.Contains(t, prompt, "Refer to background by reference, never by re-telling it")
	assert.Contains(t, prompt, `"confidence"`)
	assert.Contains(t, prompt, `"context_indices"`)
	assert.Contains(t, prompt, "context_indices never replaces a home")
}
