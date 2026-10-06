package reconciler

// destinations_test.go covers the allowed-destination set (tracker model slice 5): where
// a narrative's work may be ticketed is decided deterministically, before any drafting
// call, and a proposal outside that set is rejected.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// destinationPolicy is a config with a writable work tracker (the default), a read-only
// upstream tracker that mirrors nowhere, and a partner tracker mirroring into work and ops.
func destinationPolicy() config.Config {
	return config.Config{
		Connections: []config.Connection{
			{Name: "j", Kind: config.KindJira, Endpoint: "https://org.atlassian.net"},
			{Name: "gh", Kind: config.KindGitHub, Endpoint: "https://api.github.com"},
		},
		Trackers: []config.Tracker{
			{Name: "work", Connection: "j", Scopes: []string{"DEVSBX", "PROJ"}, WritableScopes: []string{"DEVSBX"}, DefaultScope: "DEVSBX"},
			{Name: "ops", Connection: "j", Scopes: []string{"OPS"}, WritableScopes: []string{"OPS"}, DefaultScope: "OPS"},
			{Name: "upstream", Connection: "gh", Scopes: []string{"crossplane/*"}},
			{Name: "partner", Connection: "gh", Scopes: []string{"partner/*"}, MirrorTo: []string{"work", "ops"}},
		},
		DefaultTicketIn: []string{"work"},
	}
}

// eventIn is a claude_code event whose work went to repo (events.ArtifactWorkRepos), or,
// with cwdOnly, whose working copy's remotes are repos.
func eventIn(externalID string, cwdOnly bool, repos ...string) events.Event {
	e := codeEvent(externalID, "did the work")
	key := events.ArtifactWorkRepos
	if cwdOnly {
		key = events.ArtifactCwdRemotes
	}
	events.SetRepos(&e, key, repos)

	return e
}

func proposeWithPolicy(t *testing.T, s *store.Store, client *fakeLLM) []ReconcileResult {
	t.Helper()

	got, _, err := ProposeCreates(context.Background(), s, client, testConfig(), nil, nil,
		WithDestinations(destinationPolicy()))
	require.NoError(t, err)

	return got
}

// TestProposeCreates_UpstreamWorkGetsNoTicketAndIsRecordedAsASuppression is the
// motivating case: untracked work in crossplane's scope draws no Jira ticket, because
// crossplane's tracker mirrors nowhere. Nothing is asked of the model, and the empty set
// is a suppression with its reason, like the other filters.
func TestProposeCreates_UpstreamWorkGetsNoTicketAndIsRecordedAsASuppression(t *testing.T) {
	s := reconcileStore(t)
	seedUntracked(t, s, eventIn("e1", false, "github.com/crossplane/crossplane"))
	client := &fakeLLM{responses: []string{worthTracking}}

	got := proposeWithPolicy(t, s, client)

	require.Len(t, got, 1)
	assert.Empty(t, got[0].Proposed)
	require.Len(t, got[0].Suppressed, 1)
	assert.Contains(t, got[0].Suppressed[0], `"upstream"`)
	assert.Empty(t, client.prompts, "an empty destination set costs no drafting call")
}

// TestProposeCreates_AnEmptySetIsNotRerecordedWhileNothingChanges: the suppression is a
// watermark; an unchanged narrative must not write one per pass.
func TestProposeCreates_AnEmptySetIsNotRerecordedWhileNothingChanges(t *testing.T) {
	s := reconcileStore(t)
	seedUntracked(t, s, eventIn("e1", false, "github.com/crossplane/crossplane"))
	client := &fakeLLM{responses: []string{worthTracking}}

	first := proposeWithPolicy(t, s, client)
	_, err := Persist(s, first)
	require.NoError(t, err)

	second := proposeWithPolicy(t, s, client)

	assert.Empty(t, second, "nothing new since the recorded suppression: the narrative is not even "+
		"selected, so it neither re-records nor holds a slot (store.NarrativesAwaitingCreate)")
	assert.Empty(t, client.prompts)
}

func TestProposeCreates_UnscopedWorkGoesToDefaultTicketIn(t *testing.T) {
	s := reconcileStore(t)
	seedUntracked(t, s, eventIn("e1", false, "github.com/me/fork"))

	got := proposeWithPolicy(t, s, &fakeLLM{responses: []string{worthTracking}})

	require.Len(t, got[0].Proposed, 1)
	assert.Equal(t, "DEVSBX", got[0].Proposed[0].Scope)
}

// TestProposeCreates_TranscriptEvidenceBeatsADisagreeingCwd: where the work WENT wins over
// where the session sat — a helm-charts worktree pushing to another repository.
func TestProposeCreates_TranscriptEvidenceBeatsADisagreeingCwd(t *testing.T) {
	s := reconcileStore(t)
	e := eventIn("e1", true, "github.com/me/helm-charts")
	events.SetRepos(&e, events.ArtifactWorkRepos, []string{"github.com/crossplane/crossplane"})
	seedUntracked(t, s, e)

	got := proposeWithPolicy(t, s, &fakeLLM{responses: []string{worthTracking}})

	assert.Empty(t, got[0].Proposed, "the push went upstream, so upstream's empty mirror_to decides")
	require.Len(t, got[0].Suppressed, 1)
}

// TestProposeCreates_CwdRemotesAreTheFallback: with no SCM action in the transcript, the
// working copy's remotes say where the work happened.
func TestProposeCreates_CwdRemotesAreTheFallback(t *testing.T) {
	s := reconcileStore(t)
	seedUntracked(t, s, eventIn("e1", true, "github.com/crossplane/crossplane", "github.com/me/crossplane"))

	got := proposeWithPolicy(t, s, &fakeLLM{responses: []string{worthTracking}})

	assert.Empty(t, got[0].Proposed, "the fork drops out by scope; upstream decides")
}

// TestProposeCreates_AmbiguousLocationIsReportedAndTreatedAsUnscoped.
func TestProposeCreates_AmbiguousLocationIsReportedAndTreatedAsUnscoped(t *testing.T) {
	s := reconcileStore(t)
	seedUntracked(t, s, eventIn("e1", false, "github.com/crossplane/crossplane", "github.com/partner/sdk"))

	got := proposeWithPolicy(t, s, &fakeLLM{responses: []string{worthTracking}})

	require.Len(t, got[0].Proposed, 1)
	assert.Equal(t, "DEVSBX", got[0].Proposed[0].Scope)
	require.Len(t, got[0].Notes, 1)
	assert.Contains(t, got[0].Notes[0], "ambiguous")
}

// TestProposeCreates_TheModelChoosesAmongSeveralDestinations: mirror_to may name several
// trackers; the model proposes onto whichever fit, one create each.
func TestProposeCreates_TheModelChoosesAmongSeveralDestinations(t *testing.T) {
	s := reconcileStore(t)
	seedUntracked(t, s, eventIn("e1", false, "github.com/partner/sdk"))
	client := &fakeLLM{responses: []string{`{"worth_tracking":true,"summary":"S","description":"D",` +
		`"confidence":0.8,"rationale":"r","destinations":["ops"]}`}}

	got := proposeWithPolicy(t, s, client)

	require.Len(t, got[0].Proposed, 1)
	assert.Equal(t, "OPS", got[0].Proposed[0].Scope)
	require.Len(t, client.prompts, 1)
	assert.Contains(t, client.prompts[0], `"work"`, "the prompt lists the allowed destinations")
	assert.Contains(t, client.prompts[0], `"ops"`)
}

// TestProposeCreates_ADestinationOutsideTheSetIsRejected: the model cannot widen the set.
func TestProposeCreates_ADestinationOutsideTheSetIsRejected(t *testing.T) {
	s := reconcileStore(t)
	seedUntracked(t, s, eventIn("e1", false, "github.com/partner/sdk"))
	client := &fakeLLM{responses: []string{`{"worth_tracking":true,"summary":"S","description":"D",` +
		`"confidence":0.8,"rationale":"r","destinations":["upstream","work"]}`}}

	got := proposeWithPolicy(t, s, client)

	require.Len(t, got[0].Proposed, 1)
	assert.Equal(t, "DEVSBX", got[0].Proposed[0].Scope)
	require.Len(t, got[0].Suppressed, 1)
	assert.Contains(t, got[0].Suppressed[0], `"upstream"`)
	assert.Contains(t, got[0].Suppressed[0], "outside")
}

// TestReconcile_ALinkOnAReadOnlyTrackerIsNotDrafted: a comment on an upstream issue could
// never be applied, so it is not drafted (this closed F56). A readable-but-unwritable Jira
// link is still drafted, gated at apply as before.
func TestReconcile_ALinkOnAReadOnlyTrackerIsNotDrafted(t *testing.T) {
	s := reconcileStore(t)
	seedLinkedNarrative(t, s, "crossplane/crossplane#6812", "primary", codeEvent("e1", "fixed it upstream"))
	tracker := &fakeTracker{issues: map[string]tasktracker.Issue{}}
	client := &fakeLLM{}

	got, _, err := Reconcile(context.Background(), s, tracker, client, testConfig(), WithDestinations(destinationPolicy()))

	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Empty(t, got[0].Proposed)
	require.Len(t, got[0].Suppressed, 1)
	assert.Contains(t, got[0].Suppressed[0], "read-only")
	assert.Empty(t, tracker.getCalls, "nothing is verified for a link nothing could be written to")
	assert.Empty(t, client.prompts)
}
