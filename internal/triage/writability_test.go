package triage_test

// writability_test.go covers surfacing write scope at REVIEW time.
//
// The finding: every write gate lived in gate.Applier, so a reviewer read the issue,
// read the work, read the drafted prose, judged it, pressed [a], and only then learned
// the project was not writable. Measured on a real rebuilt queue: 17 proposed actions
// targeting PAAS/RE/SUMO/SIA against a writable_project_keys of ["DEVSBX"] — every one
// unappliable, with nothing saying so.
//
// The intended UX, in the reviewer's words: "I would do this, but I can't; add a
// writable project key or tell me where else to put it." So [a]pprove is not offered
// for an unappliable action, and the reason names the remedy.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/triage"
)

// capturingPrompter records the Items it was asked about and the notices it received,
// then answers with a scripted verb.
type capturingPrompter struct {
	items   []triage.Item
	notices []string
	verbs   []triage.Verb
	calls   int
}

func (p *capturingPrompter) Ask(item triage.Item) (triage.Decision, error) {
	p.items = append(p.items, item)

	verb := triage.VerbQuit
	if p.calls < len(p.verbs) {
		verb = p.verbs[p.calls]
	}
	p.calls++

	return triage.Decision{Verb: verb}, nil
}

func (p *capturingPrompter) Confirm(string) (bool, error) { return false, nil }

func (p *capturingPrompter) Notify(message string) error {
	p.notices = append(p.notices, message)

	return nil
}

// writabilityCfg tracks PAAS and DEVSBX, but only permits writes to DEVSBX — the
// real shape of the dev configuration this was measured against.
func writabilityCfg() config.Config {
	return config.Config{Jira: []config.JiraConnection{{
		Name:                "dev",
		ProjectKeys:         []string{"PAAS", "DEVSBX"},
		WritableProjectKeys: []string{"DEVSBX"},
	}}}
}

func action(id int64, issueKey string) store.ActionRow {
	return store.ActionRow{
		ID: id, NarrativeID: 1, Type: "comment", IssueKey: issueKey,
		Payload: `{"body":"some drafted prose"}`, Confidence: 0.5, Status: store.StatusProposed,
	}
}

// TestSession_MarksAnUnappliableActionBeforeAsking is the finding. The Item must
// carry the verdict, because the prompter is what decides which verbs to offer.
func TestSession_MarksAnUnappliableActionBeforeAsking(t *testing.T) {
	p := &capturingPrompter{verbs: []triage.Verb{triage.VerbSkip}}

	s := triage.NewSession(t.Context(), []store.ActionRow{action(1, "PAAS-123")}, p, nil)
	s.SetWritability(writabilityCfg())
	require.NoError(t, s.Run())

	require.Len(t, p.items, 1)
	got := p.items[0]

	assert.False(t, got.Appliable,
		"a reviewer must learn this BEFORE spending judgment, not after pressing [a]")
	assert.Contains(t, got.UnappliableReason, "writable_project_keys",
		"and the reason must name the config key to edit")
	assert.Contains(t, got.UnappliableReason, "dev",
		"and the connection, since that is the address of the fix")
}

// TestSession_AWritableActionIsAppliable is the other half: the marker must not fire
// for an action that can actually be applied.
func TestSession_AWritableActionIsAppliable(t *testing.T) {
	p := &capturingPrompter{verbs: []triage.Verb{triage.VerbSkip}}

	s := triage.NewSession(t.Context(), []store.ActionRow{action(1, "DEVSBX-1")}, p, nil)
	s.SetWritability(writabilityCfg())
	require.NoError(t, s.Run())

	require.Len(t, p.items, 1)
	assert.True(t, p.items[0].Appliable)
	assert.Empty(t, p.items[0].UnappliableReason)
}

// TestSession_RefusesApproveOnAnUnappliableAction is the guard behind the display. A
// prompter is free to offer whatever it likes — a scripted one, a future TUI — so the
// Session must refuse rather than trusting the UI to have hidden the verb.
func TestSession_RefusesApproveOnAnUnappliableAction(t *testing.T) {
	p := &capturingPrompter{verbs: []triage.Verb{triage.VerbApprove, triage.VerbSkip}}

	s := triage.NewSession(t.Context(), []store.ActionRow{action(1, "PAAS-123")}, p, nil)
	s.SetWritability(writabilityCfg())
	require.NoError(t, s.Run())

	require.NotEmpty(t, p.notices, "the refusal must be explained, not silent")
	assert.Contains(t, p.notices[0], "writable_project_keys")

	assert.Empty(t, s.Approved(),
		"and nothing may be approved: the gate would refuse it at apply time anyway, so approving "+
			"here only defers the bad news")
	assert.Equal(t, 2, p.calls, "the reviewer is re-asked rather than losing the batch")
}

// TestSession_UntrackedProjectIsAlsoUnappliable covers the second refusal, whose
// remedy differs: unjira does not track the project at all, which is a signal about
// the correlator's attribution rather than a config gap.
func TestSession_UntrackedProjectIsAlsoUnappliable(t *testing.T) {
	p := &capturingPrompter{verbs: []triage.Verb{triage.VerbSkip}}

	s := triage.NewSession(t.Context(), []store.ActionRow{action(1, "NOPE-1")}, p, nil)
	s.SetWritability(writabilityCfg())
	require.NoError(t, s.Run())

	require.Len(t, p.items, 1)
	assert.False(t, p.items[0].Appliable)
	assert.Contains(t, p.items[0].UnappliableReason, "retarget",
		"the remedy here is [t]arget, not a config edit, and the message must say which")
}

// TestSession_WithoutWritabilityEverythingIsAppliable keeps the seam optional. Every
// existing caller and test constructs a Session without config, and none of them
// should start reporting actions as unappliable.
func TestSession_WithoutWritabilityEverythingIsAppliable(t *testing.T) {
	p := &capturingPrompter{verbs: []triage.Verb{triage.VerbSkip}}

	s := triage.NewSession(t.Context(), []store.ActionRow{action(1, "PAAS-123")}, p, nil)
	require.NoError(t, s.Run())

	require.Len(t, p.items, 1)
	assert.True(t, p.items[0].Appliable,
		"an unconfigured Session must not claim to know write scope: silence is not a refusal")
}

// TestSession_RejectAndTargetStayAvailable pins that only APPROVE is gated. Rejecting
// an unappliable action is exactly what a reviewer should be able to do, and
// retargeting it is the documented remedy for the untracked case — blocking either
// would leave the queue with rows nobody can dispose of.
func TestSession_RejectAndTargetStayAvailable(t *testing.T) {
	p := &capturingPrompter{verbs: []triage.Verb{triage.VerbReject}}

	s := triage.NewSession(t.Context(), []store.ActionRow{action(1, "PAAS-123")}, p, nil)
	s.SetWritability(writabilityCfg())
	require.NoError(t, s.Run())

	assert.Empty(t, p.notices, "reject is not refused")
	assert.Len(t, s.Rulings(), 1, "and the rejection is recorded")
}
