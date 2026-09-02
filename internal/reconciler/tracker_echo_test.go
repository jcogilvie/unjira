package reconciler

// tracker_echo_test.go covers task #180: a comment drafted from a narrative whose
// delta holds nothing but tracker records is unjira paraphrasing the tracker back
// to itself.
//
// Measured on real data (2026-09-02 triage pass, 173 events): 18 of 21 proposed
// comments came from narratives with zero non-tracker evidence. The worst was a
// long, genuinely well-written RCA comment proposed on PAAS-4000, whose narrative
// held exactly [status Discovery -> In Progress, the issue's own description x4,
// status In Progress -> Done] — a paraphrase of PAAS-4000's description, proposed
// as a new comment on PAAS-4000.
//
// The end-to-end test comes FIRST in this file, and was written before the
// implementation. This change spans internal/events, the jira collector and the
// reconciler, and docs/design-notes.md incidents 17, 20 and 22 are all the same
// failure: unit tests green while the pipeline disagreed, because two correct
// functions were composed wrongly. A targeted unit test on the filter cannot see
// that the filter never ran.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// trackerRecord builds an event that IS a tracker record — the shape the jira
// collector emits for a changelog entry or a comment. Deliberately built through
// events.SetTrackerRecord rather than by setting artifact keys inline, so this
// test exercises the same declared contract the collector uses.
func trackerRecord(externalID, issueKey, summary string, at time.Time) events.Event {
	e := events.NewEvent("jira", externalID, at, summary)
	e.Artifacts[events.ArtifactIssueKey] = issueKey
	events.SetTrackerRecord(&e)

	return e
}

// TestReconcile_SuppressesACommentWithOnlyTrackerEvidence is the headline case,
// and it is the real PAAS-4000 shape: every event in the delta is a record the
// tracker itself produced about this very issue.
//
// A comment here can only restate what the issue already says. unjira's job is to
// close the gap between what you did and what the tracker knows — with no evidence
// of what you did, there is no gap to report, and the "patch" is a paraphrase of
// the thing being patched.
func TestReconcile_SuppressesACommentWithOnlyTrackerEvidence(t *testing.T) {
	s := reconcileStore(t)

	at := time.Date(2026, 8, 25, 9, 30, 0, 0, time.UTC)
	seedLinkedNarrative(t, s, "PAAS-4000", store.Role("primary"),
		trackerRecord("PAAS-4000:desc:1", "PAAS-4000",
			"PAAS-4000 description: metrics-server crashlooped from cgo thread exhaustion", at),
		trackerRecord("PAAS-4000:desc:2", "PAAS-4000",
			"PAAS-4000 description: metrics-server crashlooped from cgo thread exhaustion", at.Add(time.Minute)),
	)

	tracker := &fakeTracker{
		issues: map[string]tasktracker.Issue{
			"PAAS-4000": {Key: "PAAS-4000", Summary: "metrics-server crashloop", StatusName: "Done"},
		},
	}

	client := &fakeLLM{responses: []string{
		`[{"issue_key":"PAAS-4000","type":"comment","body":"Root cause confirmed: cgo thread ` +
			`exhaustion. Fix shipped in PR #8436.","confidence":0.75,"rationale":"summarizing"}]`,
	}}

	got, _, err := Reconcile(t.Context(), s, tracker, client, testConfig())

	require.NoError(t, err)
	require.Len(t, got, 1)

	assert.Empty(t, got[0].Proposed,
		"a comment sourced only from the tracker's own records restates the issue to itself")

	require.Len(t, got[0].Suppressed, 1,
		"and it must be REPORTED, not silently dropped — 'nothing proposed' has to stay "+
			"distinguishable from 'nothing considered'")
	assert.Contains(t, got[0].Suppressed[0], "PAAS-4000")
	assert.Contains(t, got[0].Suppressed[0], "no evidence of work",
		"the reason must name the evidence gap, which is the actionable half")
}

// TestReconcile_KeepsACommentWithRealWorkEvidence is the other half, and it is
// what stops this filter from being a blanket suppression of the comment queue.
//
// One non-tracker event is enough. unjira saw work the tracker has no record of,
// so there is a genuine gap to close, and the comment is the patch.
//
// Without this assertion the filter could regress to suppressing everything and
// the test above would still pass — which on the 2026-09-02 data would have looked
// like success, since only 3 of 21 comments had work evidence at all.
func TestReconcile_KeepsACommentWithRealWorkEvidence(t *testing.T) {
	s := reconcileStore(t)

	at := time.Date(2026, 8, 25, 9, 30, 0, 0, time.UTC)
	seedLinkedNarrative(t, s, "SIA-201", store.Role("primary"),
		trackerRecord("SIA-201:status:1", "SIA-201", "SIA-201 status: Discovery → In Progress", at),
		codeEvent("cc:1", "pulled main into sia-approval and addressed the review action items"),
	)

	tracker := &fakeTracker{
		issues: map[string]tasktracker.Issue{
			"SIA-201": {Key: "SIA-201", Summary: "approval flow", StatusName: "In Progress"},
		},
	}

	client := &fakeLLM{responses: []string{
		`[{"issue_key":"SIA-201","type":"comment","body":"Merged latest main and addressed the ` +
			`outstanding action items.","confidence":0.6,"rationale":"work not yet on the ticket"}]`,
	}}

	got, _, err := Reconcile(t.Context(), s, tracker, client, testConfig())

	require.NoError(t, err)
	require.Len(t, got, 1)

	require.Len(t, got[0].Proposed, 1,
		"one non-tracker event is a real gap between what we did and what the tracker knows")
	assert.Equal(t, ActionComment, got[0].Proposed[0].Type)
	assert.Empty(t, got[0].Suppressed)
}

// TestSuppressTrackerEcho_LeavesTransitionsAndCreatesAlone: the argument for this
// filter is specific to comments — a comment's whole content is prose that has to
// come from somewhere.
//
// A transition asserts no prose; its justification is the recency guard's job
// (suppressStaleTransitions), which weighs work evidence on its own terms and has
// a deliberate degrade-to-proposing stance this must not override. A create
// concerns work with no issue at all, so "restating the issue" is incoherent.
//
// Suppressing either here would duplicate one guard and break the other.
func TestSuppressTrackerEcho_LeavesTransitionsAndCreatesAlone(t *testing.T) {
	at := time.Date(2026, 8, 25, 9, 30, 0, 0, time.UTC)
	delta := []events.Event{
		trackerRecord("PAAS-1:status:1", "PAAS-1", "PAAS-1 status: Discovery → In Progress", at),
	}

	drafted := []ProposedAction{
		{Type: ActionTransition, IssueKey: "PAAS-1", TargetStatus: "In Review", Confidence: 0.9},
		{Type: ActionCreate, Summary: "untracked work", Body: "d", Confidence: 0.7},
	}

	kept, suppressed := suppressTrackerEcho(delta, drafted)

	assert.Len(t, kept, 2, "only comments are this filter's business")
	assert.Empty(t, suppressed)
}

// TestSuppressTrackerEcho_AnyNonTrackerEventQualifies pins the predicate to the
// declared capability rather than to a source name.
//
// A source-name test ("is it the jira collector?") is docs/design-notes.md
// incident 21 exactly: the reconciler once recognized only Jira's own changelog
// vocabulary and would have silently never fired for a GitHub collector. A future
// GitHub-Issues collector emitting tracker records must be suppressed by this
// filter without touching it, and a future Slack or CI collector must count as
// work evidence for the same reason.
func TestSuppressTrackerEcho_AnyNonTrackerEventQualifies(t *testing.T) {
	at := time.Date(2026, 8, 25, 9, 30, 0, 0, time.UTC)

	// Same source as the tracker records above, but NOT marked as one: a
	// hypothetical collector that reads a tracker's API for something other than
	// the tracker's own bookkeeping still supplies work evidence.
	work := events.NewEvent("jira", "some:webhook:1", at, "CI reported the build green")

	delta := []events.Event{
		trackerRecord("PAAS-1:status:1", "PAAS-1", "PAAS-1 status: Discovery → In Progress", at),
		work,
	}

	kept, suppressed := suppressTrackerEcho(delta, []ProposedAction{
		{Type: ActionComment, IssueKey: "PAAS-1", Body: "the build is green", Confidence: 0.7},
	})

	assert.Len(t, kept, 1,
		"the predicate is 'is this a tracker record', declared per event — never 'which "+
			"collector emitted it'")
	assert.Empty(t, suppressed)
}

// TestSuppressTrackerEcho_JudgesTheWholeDeltaNotPerIssue is the scoping decision,
// and it differs deliberately from newestWorkEvidence's per-issue scope.
//
// A narrative is one story. Work evidence anywhere in it establishes that unjira
// observed something the tracker did not, and a same_work sibling's evidence is
// legitimate grounds to comment on either issue. Scoping per issue would suppress
// the second half of every genuine multi-issue narrative.
func TestSuppressTrackerEcho_JudgesTheWholeDeltaNotPerIssue(t *testing.T) {
	at := time.Date(2026, 8, 25, 9, 30, 0, 0, time.UTC)
	delta := []events.Event{
		trackerRecord("PAAS-1:status:1", "PAAS-1", "PAAS-1 status: Discovery → In Progress", at),
		codeEvent("cc:1", "implemented the shared fix across both tickets"),
	}

	kept, suppressed := suppressTrackerEcho(delta, []ProposedAction{
		{Type: ActionComment, IssueKey: "PAAS-1", Body: "a", Confidence: 0.7},
		{Type: ActionComment, IssueKey: "PAAS-2", Body: "b", Confidence: 0.7},
	})

	assert.Len(t, kept, 2, "one story's work evidence serves every issue in it")
	assert.Empty(t, suppressed)
}

// TestSuppressTrackerEcho_AnEmptyDeltaSuppresses: reconcileOne returns early on an
// empty delta, so this cannot arise today. Asserted anyway because the safe
// direction is not obvious — an empty delta contains no work evidence, so the
// honest answer is "no gap demonstrated", and a future caller that reaches here
// with one should not get a proposal by default.
func TestSuppressTrackerEcho_AnEmptyDeltaSuppresses(t *testing.T) {
	kept, suppressed := suppressTrackerEcho(nil, []ProposedAction{
		{Type: ActionComment, IssueKey: "PAAS-1", Body: "a", Confidence: 0.7},
	})

	assert.Empty(t, kept)
	require.Len(t, suppressed, 1)
	assert.Contains(t, suppressed[0], "no evidence of work")
}
