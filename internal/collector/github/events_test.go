package github_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ghclient "github.com/jcogilvie/unjira/internal/clients/github"
	collectorgithub "github.com/jcogilvie/unjira/internal/collector/github"
	"github.com/jcogilvie/unjira/internal/events"
)

func testRef(t *testing.T) ghclient.RepoRef {
	t.Helper()

	ref, err := ghclient.ParseRepoRef("o/r")
	require.NoError(t, err)

	return ref
}

func samplePR() ghclient.PullRequest {
	pr := ghclient.PullRequest{
		Number:    42,
		Title:     "Fix PROJ-1 crash",
		Body:      "Closes PROJ-1. Also touches PROJ-2.",
		State:     "open",
		HTMLURL:   "https://github.com/o/r/pull/42",
		CreatedAt: time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC),
	}
	pr.User.Login = "alice"
	pr.Head.Ref = "feature/PROJ-1"

	return pr
}

// TestOpenedEvent_ExternalIDIsBareOwnerRepoHashNumberOpened pins decision 1's
// distinction: :opened never carries a discriminator, because a PR is created
// exactly once and created_at never changes — there is no reopen-shaped
// collision for it.
func TestOpenedEvent_ExternalIDIsBareOwnerRepoHashNumberOpened(t *testing.T) {
	evt := collectorgithub.OpenedEvent(testRef(t), samplePR())

	assert.Equal(t, "o/r#42:opened", evt.ExternalID)
	assert.Equal(t, "github", evt.Source)
}

func TestOpenedEvent_OccurredAtIsCreatedAt(t *testing.T) {
	evt := collectorgithub.OpenedEvent(testRef(t), samplePR())

	assert.True(t, evt.OccurredAt.Equal(time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)))
}

func TestOpenedEvent_SummaryFormat(t *testing.T) {
	evt := collectorgithub.OpenedEvent(testRef(t), samplePR())

	assert.Equal(t, "o/r#42: Fix PROJ-1 crash\n\nCloses PROJ-1. Also touches PROJ-2.", evt.Summary)
}

func TestOpenedEvent_ActorIsPRAuthorLogin(t *testing.T) {
	evt := collectorgithub.OpenedEvent(testRef(t), samplePR())

	assert.Equal(t, "alice", evt.Actor)
}

func TestOpenedEvent_RawRefIsHTMLURL(t *testing.T) {
	evt := collectorgithub.OpenedEvent(testRef(t), samplePR())

	assert.Equal(t, "https://github.com/o/r/pull/42", evt.RawRef)
}

// TestOpenedEvent_ArtifactGitBranchIsHeadRef pins that this collector reuses
// the EXACT SAME artifact key collector/claudecode writes, so gatherCandidates
// needs zero changes to consume it.
func TestOpenedEvent_ArtifactGitBranchIsHeadRef(t *testing.T) {
	evt := collectorgithub.OpenedEvent(testRef(t), samplePR())

	assert.Equal(t, "feature/PROJ-1", evt.Artifacts[events.ArtifactGitBranch])
}

// TestOpenedEvent_ArtifactSCMKeysFromTitleAndBody pins that title+body keys
// land on ArtifactSCMKeys (the ProvenanceSCMCommand tier), not
// ArtifactTicketKeys or ArtifactIssueKey — this collector's own §2/§5
// decision to reuse the existing authoring-tier rather than invent a new one.
func TestOpenedEvent_ArtifactSCMKeysFromTitleAndBody(t *testing.T) {
	evt := collectorgithub.OpenedEvent(testRef(t), samplePR())

	assert.ElementsMatch(t, []string{"PROJ-1", "PROJ-2"}, events.SCMKeysOf(evt))
}

// TestNoGitHubEventIsEverMarkedATrackerRecord is the regression test the
// design doc calls for explicitly (§9), the mirror image of
// collector/jira's own TestEveryEmittedEventIsMarkedATrackerRecord: a future
// edit that starts marking these events as tracker records would silently
// reintroduce F18's category error in the opposite direction (a PR is work
// evidence in every deployment this slice supports, never the tracker's own
// account of itself). Both emitters are covered in one test on purpose,
// mirroring collector/jira's own reasoning: the marker's ABSENCE is set in
// annotate, the single function both OpenedEvent and CompletionEvents share,
// so a future event type that bypasses annotate is exactly the regression
// worth catching here.
func TestNoGitHubEventIsEverMarkedATrackerRecord(t *testing.T) {
	opened := collectorgithub.OpenedEvent(testRef(t), samplePR())

	closedPR := samplePR()
	closedPR.State = "closed"
	closedAt := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	closedPR.ClosedAt = &closedAt
	completion, err := collectorgithub.CompletionEvents(
		testRef(t), closedPR, []ghclient.IssueEvent{closedTimelineEvent(1, closedAt)},
	)
	require.NoError(t, err)
	require.Len(t, completion, 1)

	for _, e := range append([]events.Event{opened}, completion...) {
		assert.False(t, events.IsTrackerRecord(e),
			"%s is a thing somebody did, not the tracker's own account of itself", e.ExternalID)
	}
}

// TestOpenedEvent_DoesNotSetIssueKeyArtifact: ArtifactIssueKey means "this
// event is already about that issue" (the jira_event provenance tier); a PR
// mentioning a key is a weaker, inferred signal (branch/scm_command tiers),
// and setting ArtifactIssueKey here would misrank it above ProvenanceJiraEvent.
func TestOpenedEvent_DoesNotSetIssueKeyArtifact(t *testing.T) {
	evt := collectorgithub.OpenedEvent(testRef(t), samplePR())

	_, ok := evt.Artifacts[events.ArtifactIssueKey]
	assert.False(t, ok)
}

func TestOpenedEvent_EmptyBodyOmitsTrailingBlankSection(t *testing.T) {
	pr := samplePR()
	pr.Body = ""

	evt := collectorgithub.OpenedEvent(testRef(t), pr)

	assert.Equal(t, "o/r#42: Fix PROJ-1 crash", evt.Summary)
}

// -- completion events (:merged / :closed) ---------------------------------

func closedTimelineEvent(id int64, at time.Time) ghclient.IssueEvent {
	return ghclient.IssueEvent{ID: id, Event: "closed", CreatedAt: at}
}

func mergedTimelineEvent(id int64, at time.Time) ghclient.IssueEvent {
	return ghclient.IssueEvent{ID: id, Event: "merged", CreatedAt: at}
}

// TestCompletionEvents_ClosedUnmergedProducesOneClosedEvent: a closed-unmerged
// PR is still work evidence — someone opened it and abandoned it — so it must
// produce a real event, not be suppressed.
func TestCompletionEvents_ClosedUnmergedProducesOneClosedEvent(t *testing.T) {
	pr := samplePR()
	pr.State = "closed"
	closedAt := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	pr.ClosedAt = &closedAt

	timeline := []ghclient.IssueEvent{closedTimelineEvent(1001, closedAt)}

	evts, err := collectorgithub.CompletionEvents(testRef(t), pr, timeline)

	require.NoError(t, err)
	require.Len(t, evts, 1)
	assert.Equal(t, "o/r#42:closed:1001", evts[0].ExternalID)
	assert.True(t, evts[0].OccurredAt.Equal(closedAt))
	assert.False(t, events.IsTrackerRecord(evts[0]))
}

// TestCompletionEvents_MergedPRProducesBothMergedAndClosedEvents is the
// corrected behavior: naive consumption of GitHub's timeline is fine. A merge
// emits both a "merged" and a "closed" timeline entry (measured 93ms apart on
// a real PR), and both are emitted as distinct events with distinct
// ExternalIDs — deduplicating them would be the collector asserting an
// interpretation, which CLAUDE.md's "collectors are dumb and deterministic"
// reserves for the reconciler.
func TestCompletionEvents_MergedPRProducesBothMergedAndClosedEvents(t *testing.T) {
	pr := samplePR()
	pr.State = "closed"
	mergedAt := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	closedAt := mergedAt.Add(93 * time.Millisecond)
	pr.MergedAt = &mergedAt
	pr.ClosedAt = &closedAt

	timeline := []ghclient.IssueEvent{
		mergedTimelineEvent(2001, mergedAt),
		closedTimelineEvent(2002, closedAt),
	}

	evts, err := collectorgithub.CompletionEvents(testRef(t), pr, timeline)

	require.NoError(t, err)
	require.Len(t, evts, 2, "a merge must produce exactly the two distinct facts GitHub recorded")

	externalIDs := []string{evts[0].ExternalID, evts[1].ExternalID}
	assert.ElementsMatch(t, []string{"o/r#42:merged:2001", "o/r#42:closed:2002"}, externalIDs)
}

// TestCompletionEvents_ReopenedThenReclosedProducesTwoDistinctClosedEvents is
// decision 1's whole point: the reopened-PR collision is fixed by keying on
// GitHub's own immutable timeline-event id, not deferred. Two closures must
// produce two distinct ExternalIDs rather than silently deduping via INSERT OR
// IGNORE.
func TestCompletionEvents_ReopenedThenReclosedProducesTwoDistinctClosedEvents(t *testing.T) {
	pr := samplePR()
	pr.State = "closed"
	firstClose := time.Date(2026, 9, 1, 1, 13, 11, 0, time.UTC)
	secondClose := time.Date(2026, 9, 21, 1, 13, 11, 0, time.UTC)
	pr.ClosedAt = &secondClose

	timeline := []ghclient.IssueEvent{
		closedTimelineEvent(31498552227, firstClose),
		{ID: 31498554963, Event: "reopened", CreatedAt: firstClose.Add(6 * time.Second)},
		closedTimelineEvent(31498560000, secondClose),
	}

	evts, err := collectorgithub.CompletionEvents(testRef(t), pr, timeline)

	require.NoError(t, err)
	require.Len(t, evts, 2, "reopened is not itself a scoped event kind, but both closures must survive")
	assert.Equal(t, "o/r#42:closed:31498552227", evts[0].ExternalID)
	assert.Equal(t, "o/r#42:closed:31498560000", evts[1].ExternalID)
}

// TestCompletionEvents_IgnoresUnrelatedTimelineEntries: labeled/assigned/etc.
// entries are out of scope and must not become events, nor error.
func TestCompletionEvents_IgnoresUnrelatedTimelineEntries(t *testing.T) {
	pr := samplePR()
	pr.State = "closed"
	closedAt := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	pr.ClosedAt = &closedAt

	timeline := []ghclient.IssueEvent{
		{ID: 1, Event: "labeled", CreatedAt: closedAt.Add(-time.Hour)},
		{ID: 2, Event: "assigned", CreatedAt: closedAt.Add(-time.Minute)},
		closedTimelineEvent(3, closedAt),
	}

	evts, err := collectorgithub.CompletionEvents(testRef(t), pr, timeline)

	require.NoError(t, err)
	require.Len(t, evts, 1)
	assert.Equal(t, "o/r#42:closed:3", evts[0].ExternalID)
}

// TestCompletionEvents_ClosedPRWithNoMatchingTimelineEntryErrors guards
// "never silently drop data": if the PR is closed but the timeline holds no
// closed/merged entry (an API quirk, an unexpected filter bug), this must
// fail loudly rather than silently produce zero completion events for work
// that demonstrably finished.
func TestCompletionEvents_ClosedPRWithNoMatchingTimelineEntryErrors(t *testing.T) {
	pr := samplePR()
	pr.State = "closed"
	closedAt := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	pr.ClosedAt = &closedAt

	timeline := []ghclient.IssueEvent{
		{ID: 1, Event: "labeled", CreatedAt: closedAt.Add(-time.Hour)},
	}

	_, err := collectorgithub.CompletionEvents(testRef(t), pr, timeline)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "o/r#42")
}

// TestCompletionEvents_OpenPRProducesNoEvents: an open PR has not left the
// open state, so there is nothing to fetch a timeline for and no completion
// event to emit — the "at most two events, never more" bound's other half.
func TestCompletionEvents_OpenPRProducesNoEvents(t *testing.T) {
	pr := samplePR() // State: "open"

	evts, err := collectorgithub.CompletionEvents(testRef(t), pr, nil)

	require.NoError(t, err)
	assert.Empty(t, evts)
}

func TestCompletionEvents_SummaryFormatMatchesOpenedEvent(t *testing.T) {
	pr := samplePR()
	pr.State = "closed"
	closedAt := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	pr.ClosedAt = &closedAt

	timeline := []ghclient.IssueEvent{closedTimelineEvent(1, closedAt)}

	evts, err := collectorgithub.CompletionEvents(testRef(t), pr, timeline)

	require.NoError(t, err)
	require.Len(t, evts, 1)
	assert.Equal(t, "o/r#42: Fix PROJ-1 crash\n\nCloses PROJ-1. Also touches PROJ-2.", evts[0].Summary)
}

func TestCompletionEvents_ArtifactsMatchOpenedEvent(t *testing.T) {
	pr := samplePR()
	pr.State = "closed"
	closedAt := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	pr.ClosedAt = &closedAt

	timeline := []ghclient.IssueEvent{closedTimelineEvent(1, closedAt)}

	evts, err := collectorgithub.CompletionEvents(testRef(t), pr, timeline)

	require.NoError(t, err)
	require.Len(t, evts, 1)
	assert.Equal(t, "feature/PROJ-1", evts[0].Artifacts[events.ArtifactGitBranch])
	assert.ElementsMatch(t, []string{"PROJ-1", "PROJ-2"}, events.SCMKeysOf(evts[0]))
	assert.False(t, events.IsTrackerRecord(evts[0]))
}
