package reconciler

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// recencyLink builds a verifiedLink for an issue currently at liveStatus, with
// In Review offered as a legal destination.
func recencyLink(key, liveStatus string, last store.StatusEvent, haveLast bool) verifiedLink {
	return verifiedLink{
		Link:  store.NarrativeIssue{IssueKey: key, Role: store.Role("primary")},
		Issue: tasktracker.Issue{Key: key, StatusName: liveStatus},
		Transitions: []tasktracker.Transition{
			{ToStatus: "In Review", ToCategory: tasktracker.StatusInProgress},
		},
		LastStatus:     last,
		HaveLastStatus: haveLast,
	}
}

func prEvent(extID string, at time.Time) events.Event {
	return events.NewEvent("claude_code", extID, at, "opened a PR for the work")
}

// jiraStatusEvent is what the jira collector emits once it has seen a status
// change — the same event shape the guard reads through store.LatestStatusEvent.
func jiraStatusEvent(extID, issueKey, from, to string, at time.Time) events.Event {
	e := events.NewEvent("jira", extID, at, issueKey+" status: "+from+" → "+to)
	e.Artifacts["issue_key"] = issueKey
	e.Artifacts["field"] = "status"
	e.Artifacts["status_from"] = from
	e.Artifacts["status_to"] = to

	return e
}

var (
	t0 = time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	t1 = t0.Add(time.Hour)
	t2 = t0.Add(2 * time.Hour)
)

func transitionTo(key, target string) ProposedAction {
	return ProposedAction{
		Type: ActionTransition, IssueKey: key, TargetStatus: target, Confidence: 0.9,
	}
}

// TestSuppressStaleTransitions_ProposesWhenTheWorkIsNewer is the steady state:
// we pushed, nobody else has touched the ticket since it entered In Progress, so
// the move to In Review is ours to propose.
func TestSuppressStaleTransitions_ProposesWhenTheWorkIsNewer(t *testing.T) {
	verified := []verifiedLink{recencyLink("PAAS-1", "In Progress",
		store.StatusEvent{From: "Ready for Dev", To: "In Progress", OccurredAt: t0}, true)}

	kept, suppressed := suppressStaleTransitions(
		[]events.Event{prEvent("pr:1", t1)}, verified,
		[]ProposedAction{transitionTo("PAAS-1", "In Review")},
	)

	require.Len(t, kept, 1, "work at T1 postdates the status change at T0")
	assert.Empty(t, suppressed)
}

// TestSuppressStaleTransitions_SuppressesWhenTheTrackerMovedAfterOurWork is the
// security-scan case, once the move has been collected.
//
// Security bounced the ticket to In Progress at T2; our newest work evidence is
// the PR at T1. Without this, unjira still holds that PR evidence, still
// concludes In Review, and proposes moving it forward again — undoing a
// deliberate handoff, every pass, until the evidence ages out. A
// direction-based guard (never move backwards) would not catch this: the
// proposal IS forwards.
func TestSuppressStaleTransitions_SuppressesWhenTheTrackerMovedAfterOurWork(t *testing.T) {
	verified := []verifiedLink{recencyLink("PAAS-1", "In Progress",
		store.StatusEvent{From: "In Review", To: "In Progress", OccurredAt: t2}, true)}

	kept, suppressed := suppressStaleTransitions(
		[]events.Event{prEvent("pr:1", t1)}, verified,
		[]ProposedAction{transitionTo("PAAS-1", "In Review")},
	)

	assert.Empty(t, kept, "whoever moved it at T2 knew something our T1 events do not record")
	require.Len(t, suppressed, 1)
	assert.Contains(t, suppressed[0], "PAAS-1")
	assert.Contains(t, suppressed[0], "In Review",
		"the suppressed target must be named, or the reason is unactionable")
}

// TestSuppressStaleTransitions_SuppressesWhenLiveStateDisagreesWithOurHistory is
// the same case BEFORE the move has been collected, and it is the half a
// timestamp comparison alone cannot see.
//
// Our newest status event still says In Review; the live read says In Progress.
// The two disagree, so somebody moved the issue since the last collect and unjira
// cannot know when. The timestamp test would pass here (our PR at T1 postdates
// the last status change we know about at T0) and propose exactly the wrong
// thing.
func TestSuppressStaleTransitions_SuppressesWhenLiveStateDisagreesWithOurHistory(t *testing.T) {
	verified := []verifiedLink{recencyLink("PAAS-1", "In Progress",
		store.StatusEvent{From: "In Progress", To: "In Review", OccurredAt: t0}, true)}

	kept, suppressed := suppressStaleTransitions(
		[]events.Event{prEvent("pr:1", t1)}, verified,
		[]ProposedAction{transitionTo("PAAS-1", "In Review")},
	)

	assert.Empty(t, kept)
	require.Len(t, suppressed, 1)
	assert.Contains(t, suppressed[0], "since unjira last collected",
		"the reason must distinguish stale-collection from superseded-work")
}

// TestSuppressStaleTransitions_IgnoresACollectedStatusChangeAsEvidence is the
// subtle one, and it is what makes the two checks compose instead of
// cancelling out.
//
// Once security's move IS collected, it enters the delta as a jira status event.
// Counting that as work evidence makes the status change its own justification:
// its timestamp equals the last status change, evidence looks current, and the
// suppression silently stops working the moment collection catches up. Only
// non-status events count as work.
func TestSuppressStaleTransitions_IgnoresACollectedStatusChangeAsEvidence(t *testing.T) {
	verified := []verifiedLink{recencyLink("PAAS-1", "In Progress",
		store.StatusEvent{From: "In Review", To: "In Progress", OccurredAt: t2}, true)}

	delta := []events.Event{
		prEvent("pr:1", t1),
		// Security's move, now collected — newer than our PR.
		jiraStatusEvent("PAAS-1:status:9", "PAAS-1", "In Review", "In Progress", t2),
	}

	kept, suppressed := suppressStaleTransitions(
		delta, verified, []ProposedAction{transitionTo("PAAS-1", "In Review")})

	assert.Empty(t, kept,
		"a collected status change must not count as evidence that work advanced")
	require.Len(t, suppressed, 1)
}

// TestSuppressStaleTransitions_AStatusEventLatestStatusEventSkippedIsNotWork is
// the case that makes the exclusion load-bearing, and it is narrower than it
// first appears.
//
// For a normally-collected status change the exclusion changes nothing:
// LatestStatusEvent returns the NEWEST status event for the issue, so a status
// event in the delta can only equal LastStatus.OccurredAt, never exceed it — and
// ties suppress. The exclusion is therefore invisible on the main path.
//
// Where it matters is a status event LatestStatusEvent SKIPPED. It requires a
// non-null status_to (see its doc comment and
// TestLatestStatusEvent_ToleratesAMissingStatusToArtifact), so an event with
// field=status but no recorded destination is absent from LastStatus while still
// sitting in the delta — and it can be arbitrarily newer. Counted as work, a
// status change unjira could not even read would license the very transition the
// guard exists to suppress.
func TestSuppressStaleTransitions_AStatusEventLatestStatusEventSkippedIsNotWork(t *testing.T) {
	verified := []verifiedLink{recencyLink("PAAS-1", "In Progress",
		store.StatusEvent{From: "In Review", To: "In Progress", OccurredAt: t1}, true)}

	// field=status but no status_to: LatestStatusEvent skips it, so it is not
	// what LastStatus describes, yet it is newer than LastStatus.
	unreadable := events.NewEvent("jira", "PAAS-1:status:legacy", t2,
		"PAAS-1 status: In Progress → somewhere")
	unreadable.Artifacts["issue_key"] = "PAAS-1"
	unreadable.Artifacts["field"] = "status"

	kept, suppressed := suppressStaleTransitions(
		[]events.Event{unreadable}, verified,
		[]ProposedAction{transitionTo("PAAS-1", "In Review")},
	)

	assert.Empty(t, kept,
		"a status change with no readable destination is not evidence that WORK advanced")
	require.Len(t, suppressed, 1)
	assert.Contains(t, suppressed[0], "none: the delta holds only status changes",
		"and the reason must say the delta held no work, not name a bogus timestamp")
}

// TestSuppressStaleTransitions_ANonStatusJiraEventStillCountsAsWork: the
// exclusion is scoped to status changes, not to the jira source. A comment or a
// description edit is real activity, and dropping every jira event would make
// the Jira collector's own output invisible as evidence.
func TestSuppressStaleTransitions_ANonStatusJiraEventStillCountsAsWork(t *testing.T) {
	verified := []verifiedLink{recencyLink("PAAS-1", "In Progress",
		store.StatusEvent{From: "Ready for Dev", To: "In Progress", OccurredAt: t0}, true)}

	edit := events.NewEvent("jira", "PAAS-1:description:2", t1, "PAAS-1 description: rewritten")
	edit.Artifacts["issue_key"] = "PAAS-1"
	edit.Artifacts["field"] = "description"

	kept, _ := suppressStaleTransitions(
		[]events.Event{edit}, verified, []ProposedAction{transitionTo("PAAS-1", "In Review")})

	assert.Len(t, kept, 1, "a jira event that is not a status change is still work")
}

// TestSuppressStaleTransitions_ExcludesOnlyTheSubjectIssuesStatusEvents: a status
// change on a DIFFERENT issue is ordinary activity in this narrative and must
// still count as work. Excluding every status event regardless of issue would let
// one ticket's move silence proposals on another.
func TestSuppressStaleTransitions_ExcludesOnlyTheSubjectIssuesStatusEvents(t *testing.T) {
	verified := []verifiedLink{recencyLink("PAAS-1", "In Progress",
		store.StatusEvent{From: "Ready for Dev", To: "In Progress", OccurredAt: t0}, true)}

	delta := []events.Event{
		jiraStatusEvent("PAAS-2:status:1", "PAAS-2", "To Do", "In Progress", t1),
	}

	kept, _ := suppressStaleTransitions(
		delta, verified, []ProposedAction{transitionTo("PAAS-1", "In Review")})

	assert.Len(t, kept, 1,
		"another issue's transition is activity on this narrative, not evidence about PAAS-1")
}

// TestSuppressStaleTransitions_ProposesWhenThereIsNoCollectedHistory: the guard
// cannot run without a status-event source (the jira collector disabled, or a
// tracker whose changes unjira does not collect). It degrades to today's
// behavior — proposing — rather than silently disabling transitions.
//
// Deliberate, and the weaker of the two directions: a proposal is reviewed by a
// human, while a suppression is invisible. Making a missing collector silently
// disable a feature is the failure mode task #174 exists to fix properly.
func TestSuppressStaleTransitions_ProposesWhenThereIsNoCollectedHistory(t *testing.T) {
	verified := []verifiedLink{recencyLink("PAAS-1", "In Progress", store.StatusEvent{}, false)}

	kept, suppressed := suppressStaleTransitions(
		[]events.Event{prEvent("pr:1", t1)}, verified,
		[]ProposedAction{transitionTo("PAAS-1", "In Review")},
	)

	assert.Len(t, kept, 1, "no history means the guard cannot fire, not that nothing may move")
	assert.Empty(t, suppressed)
}

// TestSuppressStaleTransitions_LeavesCommentsAlone: the guard is about
// transitions. A comment describing work is still worth posting even when
// somebody else moved the ticket — arguably especially then.
func TestSuppressStaleTransitions_LeavesCommentsAlone(t *testing.T) {
	verified := []verifiedLink{recencyLink("PAAS-1", "In Progress",
		store.StatusEvent{From: "In Review", To: "In Progress", OccurredAt: t2}, true)}

	comment := ProposedAction{
		Type: ActionComment, IssueKey: "PAAS-1", Body: "what changed", Confidence: 0.9,
	}

	kept, suppressed := suppressStaleTransitions(
		[]events.Event{prEvent("pr:1", t1)}, verified, []ProposedAction{comment})

	require.Len(t, kept, 1, "a comment is not a state change and is never suppressed here")
	assert.Empty(t, suppressed)
}

// TestSuppressStaleTransitions_ComparesLiveStatusCaseInsensitively: the live
// name and the collected name both come from the same Jira, but through
// different endpoints, and Jira treats status names as display strings with no
// casing guarantee. A casing difference is not evidence that somebody moved the
// issue, and treating it as such would suppress every transition on the issue
// permanently.
func TestSuppressStaleTransitions_ComparesLiveStatusCaseInsensitively(t *testing.T) {
	verified := []verifiedLink{recencyLink("PAAS-1", "in progress",
		store.StatusEvent{From: "Ready for Dev", To: "In Progress", OccurredAt: t0}, true)}

	kept, _ := suppressStaleTransitions(
		[]events.Event{prEvent("pr:1", t1)}, verified,
		[]ProposedAction{transitionTo("PAAS-1", "In Review")},
	)

	assert.Len(t, kept, 1, "casing is not a handoff")
}

// TestSuppressStaleTransitions_SuppressesWhenWorkPredatesTheStatusChangeExactly:
// equal timestamps must suppress, not propose. Jira's changelog is
// second-granular, so a status change and a piece of work in the same second are
// indistinguishable in order — and proposing on a tie means guessing that we
// came second, which is the unsafe direction.
func TestSuppressStaleTransitions_SuppressesWhenWorkPredatesTheStatusChangeExactly(t *testing.T) {
	verified := []verifiedLink{recencyLink("PAAS-1", "In Progress",
		store.StatusEvent{From: "In Review", To: "In Progress", OccurredAt: t1}, true)}

	kept, suppressed := suppressStaleTransitions(
		[]events.Event{prEvent("pr:1", t1)}, verified,
		[]ProposedAction{transitionTo("PAAS-1", "In Review")},
	)

	assert.Empty(t, kept, "a tie is not evidence that our work came after")
	require.Len(t, suppressed, 1)
}

// TestSuppressStaleTransitions_AnUnknownIssueKeyIsLeftAlone: a drafted action
// whose key is not among the verified links cannot be judged by this guard.
// Dropping it here would silently swallow it; actionsFromVerdicts already
// discards unrecognized keys, so anything reaching this point with an unknown key
// is a bug elsewhere and must stay visible.
func TestSuppressStaleTransitions_AnUnknownIssueKeyIsLeftAlone(t *testing.T) {
	verified := []verifiedLink{recencyLink("PAAS-1", "In Progress",
		store.StatusEvent{From: "In Review", To: "In Progress", OccurredAt: t2}, true)}

	kept, suppressed := suppressStaleTransitions(
		[]events.Event{prEvent("pr:1", t1)}, verified,
		[]ProposedAction{transitionTo("GHOST-1", "In Review")},
	)

	assert.Len(t, kept, 1, "this guard judges only issues it has state for")
	assert.Empty(t, suppressed)
}

// TestReconcile_SuppressesAStaleTransitionEndToEnd drives the whole real path —
// store, verifyLinks, draft, the guard — rather than calling
// suppressStaleTransitions directly.
//
// This exists because the unit tests above can all pass while the guard is never
// reached: it has to be wired into reconcileOne, verifyLinks has to actually
// populate LastStatus from the store, and the model's transition has to survive
// floorConfidence to get as far as being suppressed. A previous session lost hours
// to exactly this gap — a targeted unit test passing while the pipeline disagreed
// — and the lesson recorded then was that the gap IS the finding.
func TestReconcile_SuppressesAStaleTransitionEndToEnd(t *testing.T) {
	s := reconcileStore(t)

	// Work at 09:30 (codeEvent's timestamp).
	nid := seedLinkedNarrative(t, s, "PAAS-1", store.Role("primary"),
		codeEvent("cc:1", "opened a PR for the retry rework"))

	// Security bounced it to In Progress at 11:00 — AFTER our work, and the live
	// tracker agrees, so this is the superseded case rather than the stale-
	// collection one.
	moved := time.Date(2026, 8, 25, 11, 0, 0, 0, time.UTC)
	insertStatusEvent(t, s, "PAAS-1:status:9", "PAAS-1", "In Review", "In Progress", moved)

	tracker := &fakeTracker{
		issues: map[string]tasktracker.Issue{
			"PAAS-1": {Key: "PAAS-1", Summary: "the ticket", StatusName: "In Progress"},
		},
		transitions: map[string][]tasktracker.Transition{
			"PAAS-1": {{ToStatus: "In Review", ToCategory: tasktracker.StatusInProgress}},
		},
	}

	client := &fakeLLM{responses: []string{
		`[{"issue_key":"PAAS-1","type":"transition","target_status":"In Review",` +
			`"confidence":0.95,"rationale":"a PR is open"}]`,
	}}

	got, _, err := Reconcile(t.Context(), s, tracker, client, testConfig())

	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, nid, got[0].NarrativeID)

	assert.Empty(t, got[0].Proposed,
		"the transition must not survive: security moved the ticket after our newest work")
	require.Len(t, got[0].Suppressed, 1,
		"and the suppression must be reported, or 'nothing proposed' is indistinguishable "+
			"from 'nothing considered'")
	assert.Contains(t, got[0].Suppressed[0], "superseded")
	assert.Contains(t, got[0].Suppressed[0], "PAAS-1")

	assert.Empty(t, tracker.writeCalls, "and nothing was written, as always")
}

// TestReconcile_ProposesATransitionWhenNobodyElseHasMoved is the other half: the
// same wiring, with the tracker's last move BEFORE our work. Without it, a guard
// that suppressed everything unconditionally would pass the test above.
func TestReconcile_ProposesATransitionWhenNobodyElseHasMoved(t *testing.T) {
	s := reconcileStore(t)

	seedLinkedNarrative(t, s, "PAAS-1", store.Role("primary"),
		codeEvent("cc:1", "opened a PR for the retry rework"))

	// We entered In Progress at 08:00; our work is at 09:30.
	entered := time.Date(2026, 8, 25, 8, 0, 0, 0, time.UTC)
	insertStatusEvent(t, s, "PAAS-1:status:1", "PAAS-1", "Ready for Dev", "In Progress", entered)

	tracker := &fakeTracker{
		issues: map[string]tasktracker.Issue{
			"PAAS-1": {Key: "PAAS-1", Summary: "the ticket", StatusName: "In Progress"},
		},
		transitions: map[string][]tasktracker.Transition{
			"PAAS-1": {{ToStatus: "In Review", ToCategory: tasktracker.StatusInProgress}},
		},
	}

	client := &fakeLLM{responses: []string{
		`[{"issue_key":"PAAS-1","type":"transition","target_status":"In Review",` +
			`"confidence":0.95,"rationale":"a PR is open"}]`,
	}}

	got, _, err := Reconcile(t.Context(), s, tracker, client, testConfig())

	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Len(t, got[0].Proposed, 1,
		"our work postdates the last status change, so the move is ours to propose")
	assert.Equal(t, ActionTransition, got[0].Proposed[0].Type)
	assert.Equal(t, "In Review", got[0].Proposed[0].TargetStatus)
}

// insertStatusEvent adds a collected status change and links it to nothing — the
// guard reads it via store.LatestStatusEvent, which is issue-scoped rather than
// narrative-scoped, so it does not need to be part of any narrative.
func insertStatusEvent(t *testing.T, s *store.Store, extID, issueKey, from, to string, at time.Time) {
	t.Helper()

	_, err := s.InsertEvent(jiraStatusEvent(extID, issueKey, from, to, at))
	require.NoError(t, err)
}
