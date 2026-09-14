package triage

// context_test.go covers the review context an Item carries: the narrative the
// action came from, and the live issue it targets.
//
// The gap this closes was found by a human actually reviewing a queue. Presented
// with a comment proposal for PAAS-3969, the reviewer said: "note how this
// doesn't actually tell me about the ticket itself, so i really don't have what i
// need to decide." Correct — and the interface, not the reviewer, was at fault.
//
// Ask rendered the action's body, its rationale, and nothing else. Two facts a
// reviewer needs were missing for different reasons:
//
//   - The NARRATIVE (title + summary) was already in the store and simply never
//     passed to the prompter. Pure plumbing.
//   - The ISSUE's own summary and live status were not in the store AT ALL. The
//     Jira collector records status transitions and field edits, not a snapshot
//     of the ticket, so `PAAS-3969 status: Discovery → In Progress` was the most
//     a reviewer could learn locally. That one needs a live read.
//
// A reviewer who cannot tell what a ticket is about cannot judge whether a
// comment belongs on it, and every decision they make under that handicap becomes
// training data for slice 7's distiller. Bad context does not merely slow review
// down; it poisons what the distiller learns.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// TestSession_AsksWithNarrativeContext is the plumbing half: the narrative is
// already in the store, so an Item must carry it.
//
// Without this, a reviewer sees a body and a rationale with no indication of what
// work produced them — and the rationale is the model's own reasoning, which is
// exactly the thing under review and therefore cannot be the reviewer's only
// source of context.
func TestSession_AsksWithNarrativeContext(t *testing.T) {
	action := store.ActionRow{
		ID: 1, NarrativeID: 7, Type: "comment", IssueKey: "PAAS-1",
		Payload: `{"body":"what we did"}`, Status: store.StatusProposed,
	}

	prompter := &recordingPrompter{decisions: []Decision{{Verb: VerbSkip}}}
	handler := &contextHandler{
		narratives: map[int64]NarrativeContext{
			7: {Title: "helm chart parallelism fix", Summary: "Fixed a race in the test harness."},
		},
	}

	s := NewSession(t.Context(), []store.ActionRow{action}, prompter, handler)
	require.NoError(t, s.Run())

	require.Len(t, prompter.seen, 1)
	assert.Equal(t, "helm chart parallelism fix", prompter.seen[0].Narrative.Title,
		"the reviewer must see which work produced this action")
	assert.Equal(t, "Fixed a race in the test harness.", prompter.seen[0].Narrative.Summary)
}

// TestSession_AsksWithLiveIssueContext is the live-read half, and the one the
// reviewer's complaint was actually about.
//
// The store holds no ticket summary — only transition events — so "what is this
// ticket?" is unanswerable locally. Reading it at review time is the only way,
// and it is also the freshest: a status that changed since collection shows the
// reviewer the truth rather than unjira's stale belief.
func TestSession_AsksWithLiveIssueContext(t *testing.T) {
	action := store.ActionRow{
		ID: 1, NarrativeID: 7, Type: "comment", IssueKey: "PAAS-3969",
		Payload: `{"body":"asking about the blocker"}`, Status: store.StatusProposed,
	}

	prompter := &recordingPrompter{decisions: []Decision{{Verb: VerbSkip}}}
	handler := &contextHandler{
		issues: map[string]tasktracker.Issue{
			"PAAS-3969": {
				Key: "PAAS-3969", Summary: "zrh cluster autoscaler thrashing",
				StatusName: "Blocked",
			},
		},
	}

	s := NewSession(t.Context(), []store.ActionRow{action}, prompter, handler)
	require.NoError(t, s.Run())

	require.Len(t, prompter.seen, 1)
	assert.Equal(t, "zrh cluster autoscaler thrashing", prompter.seen[0].Issue.Summary,
		"the reviewer must be able to tell what the ticket is ABOUT")
	assert.Equal(t, "Blocked", prompter.seen[0].Issue.StatusName,
		"and its live status, which may differ from what unjira last collected")
}

// TestSession_AsksEvenWhenContextIsUnavailable is the property that keeps this
// from becoming a new failure mode.
//
// Context is an aid to judgment, not a precondition for it. A tracker outage, a
// deleted ticket, or a create action with no issue key at all must still yield a
// reviewable item — degrading to "no context available" rather than aborting a
// session the reviewer is midway through. Losing a half-reviewed batch to a
// failed decoration would be strictly worse than reviewing without the
// decoration.
func TestSession_AsksEvenWhenContextIsUnavailable(t *testing.T) {
	action := store.ActionRow{
		ID: 1, NarrativeID: 7, Type: "create",
		Payload: `{"summary":"new work","description":"d"}`, Status: store.StatusProposed,
	}

	prompter := &recordingPrompter{decisions: []Decision{{Verb: VerbSkip}}}
	// Empty maps: no narrative, no issue, and a create has no key to look up.
	handler := &contextHandler{}

	s := NewSession(t.Context(), []store.ActionRow{action}, prompter, handler)
	require.NoError(t, s.Run())

	require.Len(t, prompter.seen, 1, "the item is still presented")
	assert.Empty(t, prompter.seen[0].Issue.Key)
	assert.Empty(t, prompter.seen[0].Narrative.Title)
}

// TestSession_ResolvesIssueContextOncePerKey: a queue routinely holds several
// actions for one issue (the PAAS-4019 batch had ten). Re-reading the same
// ticket per action would multiply review latency by the batch size for no new
// information, and every one of those is a network round trip a reviewer waits on.
func TestSession_ResolvesIssueContextOncePerKey(t *testing.T) {
	batch := []store.ActionRow{
		{
			ID: 1, NarrativeID: 7, Type: "comment", IssueKey: "PAAS-1",
			Payload: `{"body":"a"}`, Status: store.StatusProposed,
		},
		{
			ID: 2, NarrativeID: 7, Type: "comment", IssueKey: "PAAS-1",
			Payload: `{"body":"b"}`, Status: store.StatusProposed,
		},
		{
			ID: 3, NarrativeID: 7, Type: "comment", IssueKey: "PAAS-1",
			Payload: `{"body":"c"}`, Status: store.StatusProposed,
		},
	}

	prompter := &recordingPrompter{decisions: []Decision{
		{Verb: VerbSkip}, {Verb: VerbSkip}, {Verb: VerbSkip},
	}}
	handler := &contextHandler{
		issues: map[string]tasktracker.Issue{"PAAS-1": {Key: "PAAS-1", Summary: "one ticket"}},
	}

	s := NewSession(t.Context(), batch, prompter, handler)
	require.NoError(t, s.Run())

	assert.Equal(t, 1, handler.issueCalls,
		"three actions on one issue must cost one tracker read, not three")
}

// recordingPrompter captures every Item it was asked about, which is the whole
// point: these tests assert on what the REVIEWER would have seen.
type recordingPrompter struct {
	decisions []Decision
	seen      []Item
	next      int
}

func (p *recordingPrompter) Ask(item Item) (Decision, error) {
	p.seen = append(p.seen, item)

	d := Decision{Verb: VerbQuit}
	if p.next < len(p.decisions) {
		d = p.decisions[p.next]
		p.next++
	}

	return d, nil
}

func (p *recordingPrompter) Confirm(string) (bool, error) { return false, nil }
func (p *recordingPrompter) Notify(string) error          { return nil }

// contextHandler is a Handler that only implements the context lookups, since
// these tests never exercise redraft/retarget/restructure.
type contextHandler struct {
	narratives map[int64]NarrativeContext
	issues     map[string]tasktracker.Issue
	issueCalls int
}

func (h *contextHandler) NarrativeContext(id int64) (NarrativeContext, error) {
	return h.narratives[id], nil
}

func (h *contextHandler) IssueContext(key string) (tasktracker.Issue, error) {
	h.issueCalls++

	return h.issues[key], nil
}

func (h *contextHandler) Redraft(context.Context, store.ActionRow, string) (store.ActionRow, error) {
	return store.ActionRow{}, nil
}

func (h *contextHandler) Retarget(context.Context, store.ActionRow, string) (store.ActionRow, error) {
	return store.ActionRow{}, nil
}

func (h *contextHandler) Restructure(context.Context, Decision, []store.ActionRow) ([]store.ActionRow, error) {
	return nil, nil
}
