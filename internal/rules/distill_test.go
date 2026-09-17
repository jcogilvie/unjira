package rules_test

// distill_test.go covers phase 1's slice 7: turning a reviewer's free-text corrections
// into candidate rule files.
//
// The loop this closes: triage persists actions.feedback on reject/edit, and until now
// nothing read it. A reviewer could say "progress comments that only restate 'committed
// and ran tests' are not worth posting" and the reconciler would propose the same shape
// again next pass, because the correction lived in a column no prompt consulted.
//
// Distill drafts candidates and returns them. It writes NOTHING — the spec is explicit
// that files land in triage, only on keep/modify — because a rule file changes agent
// behaviour for every future pass, and that is a decision a human makes rather than
// something a distillation pass does on its own.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/llm"
	"github.com/jcogilvie/unjira/internal/rules"
)

// distillLLM returns a canned response and records the prompt it was given.
type distillLLM struct {
	response string
	err      error
	system   string
	user     string
	calls    int
}

func (f *distillLLM) Complete(_ context.Context, system, user string) (string, llm.Usage, error) {
	f.calls++
	f.system, f.user = system, user

	return f.response, llm.Usage{PromptTokens: 100, CompletionTokens: 50}, f.err
}

func correction(id int64, actionType, feedback string) rules.Correction {
	return rules.Correction{
		ActionID: id, ActionType: actionType, IssueKey: "DEVSBX-1",
		Body: "some drafted prose", Feedback: feedback,
	}
}

// TestDistill_DraftsARuleFromACorrection is the slice. One reviewer correction in, one
// candidate rule out.
func TestDistill_DraftsARuleFromACorrection(t *testing.T) {
	client := &distillLLM{response: `[{
		"name": "comment-must-say-what-changed",
		"scope": "reconciler",
		"confidence": "provisional",
		"body": "A comment must say what CHANGED, not that work occurred."
	}]`}

	got, stats, err := rules.Distill(t.Context(), client, []rules.Correction{
		correction(2, "comment", "progress comments that only restate committed and ran tests are not worth posting"),
	})
	require.NoError(t, err)

	require.Len(t, got, 1)
	assert.Equal(t, "comment-must-say-what-changed", got[0].Name)
	assert.Equal(t, rules.ScopeReconciler, got[0].Scope)
	assert.Equal(t, rules.ConfidenceProvisional, got[0].Confidence)
	assert.Contains(t, got[0].Body, "what CHANGED")
	assert.Equal(t, 1, client.calls)
	assert.Equal(t, int64(100), stats.PromptTokens)
}

// TestDistill_DefaultsToProvisional is a safety property, not a convenience. A rule
// distilled from ONE correction has one data point behind it, and ConfidenceHigh's own
// doc comment reserves that for norms that have held up. A model that omits the field —
// or asks for high on its first sighting — must not silently get more weight than the
// evidence supports.
func TestDistill_DefaultsToProvisional(t *testing.T) {
	client := &distillLLM{response: `[{
		"name": "r", "scope": "reconciler", "body": "b"
	}]`}

	got, _, err := rules.Distill(t.Context(), client,
		[]rules.Correction{correction(1, "comment", "f")})
	require.NoError(t, err)

	require.Len(t, got, 1)
	assert.Equal(t, rules.ConfidenceProvisional, got[0].Confidence,
		"an omitted confidence must not become high: one correction is one data point")
}

// TestDistill_RejectsAnUnknownScope keeps a candidate from being unloadable. Load's
// parseScope fails a whole file on a bad scope, so a distilled rule with scope "cluster"
// would be written by triage and then take every other rule out of the prompt with it.
func TestDistill_RejectsAnUnknownScope(t *testing.T) {
	client := &distillLLM{response: `[{
		"name": "r", "scope": "cluster", "confidence": "high", "body": "b"
	}]`}

	_, _, err := rules.Distill(t.Context(), client,
		[]rules.Correction{correction(1, "comment", "f")})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "cluster", "the message must name the rejected value")
	assert.Contains(t, err.Error(), "correlator", "and what is available")
}

// TestDistill_RejectsAnEmptyBody guards the one field that carries the norm. A rule with
// a name and no body renders as a heading with nothing under it — worse than no rule,
// because it looks like guidance.
func TestDistill_RejectsAnEmptyBody(t *testing.T) {
	client := &distillLLM{response: `[{"name": "r", "scope": "reconciler", "body": "  "}]`}

	_, _, err := rules.Distill(t.Context(), client,
		[]rules.Correction{correction(1, "comment", "f")})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "body")
}

// TestDistill_RejectsAnUnusableName is about the filename triage will write. Load derives
// Name from the basename, and a name with a slash or spaces either escapes the rules
// directory or produces a file nothing can reference.
func TestDistill_RejectsAnUnusableName(t *testing.T) {
	for _, name := range []string{"", "../escape", "has spaces", "has/slash", "Upper_Case"} {
		client := &distillLLM{response: `[{
			"name": "` + name + `", "scope": "reconciler", "body": "b"
		}]`}

		_, _, err := rules.Distill(t.Context(), client,
			[]rules.Correction{correction(1, "comment", "f")})

		require.Error(t, err, "name %q must be rejected", name)
	}
}

// TestDistill_NoCorrectionsMakesNoCall is the cost floor. A learn-check on a queue nobody
// corrected must not spend a model call to conclude there is nothing to learn.
func TestDistill_NoCorrectionsMakesNoCall(t *testing.T) {
	client := &distillLLM{response: `[]`}

	got, stats, err := rules.Distill(t.Context(), client, nil)

	require.NoError(t, err)
	assert.Empty(t, got)
	assert.Zero(t, client.calls, "no corrections means nothing to distill")
	assert.Zero(t, stats.PromptTokens, "and no tokens spent")
}

// TestDistill_AnEmptyArrayIsNotAnError pins the model's right to decline. "These
// corrections do not generalise" is a legitimate answer, and treating it as a failure
// would push the model toward inventing norms from one-off fixes.
func TestDistill_AnEmptyArrayIsNotAnError(t *testing.T) {
	client := &distillLLM{response: `[]`}

	got, _, err := rules.Distill(t.Context(), client,
		[]rules.Correction{correction(1, "comment", "fix this typo")})

	require.NoError(t, err, "declining to generalise is a valid outcome, not a failure")
	assert.Empty(t, got)
}

// TestDistill_PromptCarriesEveryCorrection is what makes clustering possible. The spec
// says candidates may be drafted per correction OR per cluster of related corrections,
// and a model cannot cluster what it is not shown.
func TestDistill_PromptCarriesEveryCorrection(t *testing.T) {
	client := &distillLLM{response: `[]`}

	_, _, err := rules.Distill(t.Context(), client, []rules.Correction{
		correction(1, "comment", "first correction text"),
		correction(2, "transition", "second correction text"),
	})
	require.NoError(t, err)

	assert.Contains(t, client.user, "first correction text")
	assert.Contains(t, client.user, "second correction text")
	assert.Contains(t, client.user, "transition",
		"the action type belongs in the prompt: a norm about comments is not a norm about transitions")
}

// TestDistill_PromptForbidsFabrication mirrors the constraint every other unjira prompt
// carries. A distilled rule invented from nothing would be an agent norm nobody agreed
// to, and it would then shape every future pass.
func TestDistill_PromptForbidsFabrication(t *testing.T) {
	client := &distillLLM{response: `[]`}

	_, _, err := rules.Distill(t.Context(), client,
		[]rules.Correction{correction(1, "comment", "f")})
	require.NoError(t, err)

	lowered := strings.ToLower(client.system)
	assert.Contains(t, lowered, "do not invent",
		"the system prompt must forbid inventing norms the corrections do not support")
}

// TestDistill_SurfacesAModelError keeps a failed learn-check from looking like "nothing
// to learn". Those are different outcomes and only one of them is fine.
func TestDistill_SurfacesAModelError(t *testing.T) {
	client := &distillLLM{err: assert.AnError}

	_, _, err := rules.Distill(t.Context(), client,
		[]rules.Correction{correction(1, "comment", "f")})

	require.Error(t, err)
}

// TestDistill_RejectsUnparseableJSON is the same argument as the reconciler's own parse
// guards: a reply that does not decode must fail loudly rather than yield zero rules,
// which would read as "the model found nothing".
func TestDistill_RejectsUnparseableJSON(t *testing.T) {
	client := &distillLLM{response: `not json at all`}

	_, _, err := rules.Distill(t.Context(), client,
		[]rules.Correction{correction(1, "comment", "f")})

	require.Error(t, err)
}

// TestDistill_CandidateRoundTripsThroughLoad is the property that matters most: a
// candidate is only useful if the file triage writes can be read back. This asserts
// against the real parser rather than a hand-rolled expectation of its format.
func TestDistill_CandidateRoundTripsThroughLoad(t *testing.T) {
	client := &distillLLM{response: `[{
		"name": "comment-must-say-what-changed",
		"scope": "reconciler",
		"confidence": "provisional",
		"body": "A comment must say what CHANGED.\n\nSecond paragraph survives."
	}]`}

	got, _, err := rules.Distill(t.Context(), client,
		[]rules.Correction{correction(1, "comment", "f")})
	require.NoError(t, err)
	require.Len(t, got, 1)

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, got[0].Name+".md"), []byte(got[0].FileContents()), 0o600))

	loaded, err := rules.Load(dir)
	require.NoError(t, err, "a candidate triage writes MUST parse: Load fails a whole file on bad "+
		"frontmatter, so an unloadable rule takes every other rule out of the prompt with it")
	require.Len(t, loaded, 1)

	assert.Equal(t, got[0].Name, loaded[0].Name)
	assert.Equal(t, rules.ScopeReconciler, loaded[0].Scope)
	assert.Equal(t, rules.ConfidenceProvisional, loaded[0].Confidence)
	assert.Contains(t, loaded[0].Body, "Second paragraph survives.")
}
