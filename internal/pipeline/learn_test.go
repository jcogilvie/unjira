package pipeline_test

// learn_test.go covers RunLearn: the pass that makes rules.Distill reachable and
// persists what it produces.
//
// Distill and store.CorrectionsSince landed without a caller, which the reviewer
// correctly called out — a distiller nobody can invoke is a library, not a feature. This
// is the stage that closes the loop: read corrections since the last learn, draft
// candidates, and (only when told to) write the rule files the correlator's and
// reconciler's prompts already know how to read.
//
// TWO PROPERTIES CARRY THE SAFETY HERE. Writing is opt-in per candidate, because a rule
// shapes every future prompt on every narrative. And the watermark advances only over
// corrections that were actually distilled, or a failed pass would silently skip the
// feedback it never saw.

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/llm"
	"github.com/jcogilvie/unjira/internal/pipeline"
	"github.com/jcogilvie/unjira/internal/rules"
	"github.com/jcogilvie/unjira/internal/store"
)

// learnLLM returns one canned distillation reply.
type learnLLM struct {
	response string
	err      error
	calls    int
}

func (f *learnLLM) Complete(_ context.Context, _, _ string) (string, llm.Usage, error) {
	f.calls++

	return f.response, llm.Usage{PromptTokens: 90, CompletionTokens: 40}, f.err
}

// learnFixture builds a store holding one rejected action with feedback, plus a rules dir.
func learnFixture(t *testing.T) (*store.Store, config.Config, int64) {
	t.Helper()

	return learnFixtureAt(t, filepath.Join(t.TempDir(), "learn.db"))
}

// learnFixtureAt is learnFixture at a known path, for a test that must reach the
// database directly to set a timestamp the store API stamps itself.
func learnFixtureAt(t *testing.T, path string) (*store.Store, config.Config, int64) {
	t.Helper()

	s, err := store.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	nid, err := s.InsertNarrative(time.Now().Add(-time.Hour), time.Now(), "work", "s")
	require.NoError(t, err)

	actionID, err := s.InsertAction(store.ActionRow{
		NarrativeID: nid, Type: "comment", IssueKey: "DEVSBX-1",
		Payload: `{"body":"drafted prose"}`, Status: store.StatusProposed,
	})
	require.NoError(t, err)
	// Rule on it, because only the status-update path stamps decided_at — which the
	// watermark filters on.
	require.NoError(t, s.UpdateActionStatusAndFeedback(actionID, store.StatusRejected,
		"comments must say what changed"))

	cfg := config.Config{Rules: config.RulesConfig{Dir: t.TempDir()}}

	return s, cfg, actionID
}

const oneCandidate = `[{
	"name": "say-what-changed", "scope": "reconciler",
	"confidence": "provisional", "body": "A comment must say what changed."
}]`

// TestRunLearn_DraftsWithoutWriting is the default, and it is the safety default. A learn
// pass that wrote rule files on its own authority would change agent behaviour with no
// human in the loop — the same thing gate.Applier exists to prevent for tracker writes.
func TestRunLearn_DraftsWithoutWriting(t *testing.T) {
	s, cfg, _ := learnFixture(t)
	client := &learnLLM{response: oneCandidate}

	got, err := pipeline.RunLearn(t.Context(), s, client, cfg, pipeline.LearnOptions{})
	require.NoError(t, err)

	require.Len(t, got.Candidates, 1)
	assert.Equal(t, "say-what-changed", got.Candidates[0].Name)
	assert.Empty(t, got.Written, "nothing is written unless a candidate is named")

	entries, err := os.ReadDir(cfg.RulesDir())
	require.NoError(t, err)
	assert.Empty(t, entries, "the rules directory must be untouched by a draft-only pass")
}

// TestRunLearn_WritesOnlyNamedCandidates is the opt-in. A reviewer keeps the ones they
// agree with, by name, and the rest are simply not written.
func TestRunLearn_WritesOnlyNamedCandidates(t *testing.T) {
	s, cfg, _ := learnFixture(t)
	client := &learnLLM{response: `[
		{"name":"keep-me","scope":"reconciler","body":"kept norm"},
		{"name":"skip-me","scope":"reconciler","body":"skipped norm"}
	]`}

	got, err := pipeline.RunLearn(t.Context(), s, client, cfg,
		pipeline.LearnOptions{Keep: []string{"keep-me"}})
	require.NoError(t, err)

	require.Len(t, got.Written, 1)
	assert.Equal(t, "keep-me", got.Written[0])

	_, err = os.Stat(filepath.Join(cfg.RulesDir(), "keep-me.md"))
	require.NoError(t, err, "the kept rule must be on disk")

	_, err = os.Stat(filepath.Join(cfg.RulesDir(), "skip-me.md"))
	require.Error(t, err, "and the one nobody kept must not be")
}

// TestRunLearn_WrittenRuleIsLoadable is the property that makes the whole slice worth
// anything: a rule the pass writes must be one the prompts can read. Load fails a whole
// file on bad frontmatter, so an unloadable write would take every other rule out of both
// prompts with it.
func TestRunLearn_WrittenRuleIsLoadable(t *testing.T) {
	s, cfg, _ := learnFixture(t)
	client := &learnLLM{response: oneCandidate}

	_, err := pipeline.RunLearn(t.Context(), s, client, cfg,
		pipeline.LearnOptions{Keep: []string{"say-what-changed"}})
	require.NoError(t, err)

	loaded, err := rules.Load(cfg.RulesDir())
	require.NoError(t, err, "a written rule MUST parse, or it breaks rule loading for everything")
	require.Len(t, loaded, 1)
	assert.Equal(t, rules.ScopeReconciler, loaded[0].Scope)
	assert.Contains(t, loaded[0].Body, "say what changed")
}

// TestRunLearn_RefusesToOverwriteAnExistingRule protects a rule a human already edited. A
// distilled candidate colliding on name must not silently replace hand-tuned prose.
func TestRunLearn_RefusesToOverwriteAnExistingRule(t *testing.T) {
	s, cfg, _ := learnFixture(t)
	existing := filepath.Join(cfg.RulesDir(), "say-what-changed.md")
	require.NoError(t, os.WriteFile(existing, []byte("---\nscope: reconciler\nconfidence: high\n"+
		"learned: 2026-01-01\n---\n\nhand-written, do not clobber\n"), 0o600))

	client := &learnLLM{response: oneCandidate}

	_, err := pipeline.RunLearn(t.Context(), s, client, cfg,
		pipeline.LearnOptions{Keep: []string{"say-what-changed"}})
	require.Error(t, err, "a name collision must fail loudly rather than overwrite")

	contents, readErr := os.ReadFile(existing)
	require.NoError(t, readErr)
	assert.Contains(t, string(contents), "do not clobber",
		"and the human's file must survive intact")
}

// TestRunLearn_AdvancesTheWatermarkOnlyWhenWriting is the watermark's contract. A
// draft-only pass must NOT advance it, or the next pass would skip corrections nobody
// ever turned into rules — the outcome-leaves-no-trace shape from F12, F22 and F26,
// inverted: here the danger is advancing past work that was never done.
func TestRunLearn_AdvancesTheWatermarkOnlyWhenWriting(t *testing.T) {
	s, cfg, _ := learnFixture(t)

	first, err := pipeline.RunLearn(t.Context(), s, &learnLLM{response: oneCandidate}, cfg,
		pipeline.LearnOptions{})
	require.NoError(t, err)
	require.Len(t, first.Candidates, 1)

	// Draft-only: the same correction must still be offered.
	second, err := pipeline.RunLearn(t.Context(), s, &learnLLM{response: oneCandidate}, cfg,
		pipeline.LearnOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, second.CorrectionsRead,
		"a draft-only pass must not advance the watermark: the reviewer has not ruled yet")

	// Keeping a rule advances it.
	_, err = pipeline.RunLearn(t.Context(), s, &learnLLM{response: oneCandidate}, cfg,
		pipeline.LearnOptions{Keep: []string{"say-what-changed"}})
	require.NoError(t, err)

	after, err := pipeline.RunLearn(t.Context(), s, &learnLLM{response: `[]`}, cfg,
		pipeline.LearnOptions{})
	require.NoError(t, err)
	assert.Zero(t, after.CorrectionsRead,
		"once distilled and kept, a correction must not be re-offered forever")
}

// TestRunLearn_NoCorrectionsCostsNoCall is the cost floor for a scheduled learn pass.
func TestRunLearn_NoCorrectionsCostsNoCall(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "empty.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	client := &learnLLM{response: `[]`}
	got, err := pipeline.RunLearn(t.Context(), s, client,
		config.Config{Rules: config.RulesConfig{Dir: t.TempDir()}}, pipeline.LearnOptions{})

	require.NoError(t, err)
	assert.Zero(t, client.calls, "an unreviewed queue must not cost a model call")
	assert.Zero(t, got.CorrectionsRead)
	assert.Empty(t, got.Candidates)
}

// TestRunLearn_KeepingAnUnknownNameIsAnError catches a typo before it reads as "the
// reviewer declined everything". Silently writing nothing would be indistinguishable
// from a deliberate skip.
func TestRunLearn_KeepingAnUnknownNameIsAnError(t *testing.T) {
	s, cfg, _ := learnFixture(t)
	client := &learnLLM{response: oneCandidate}

	_, err := pipeline.RunLearn(t.Context(), s, client, cfg,
		pipeline.LearnOptions{Keep: []string{"typoed-name"}})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "typoed-name")
	assert.Contains(t, err.Error(), "say-what-changed", "and the message must name what WAS offered")
}

// TestRunLearn_SurfacesADistillError keeps a failed learn from looking like "nothing to
// learn", and must not advance the watermark past corrections it never processed.
func TestRunLearn_SurfacesADistillError(t *testing.T) {
	s, cfg, _ := learnFixture(t)

	_, err := pipeline.RunLearn(t.Context(), s, &learnLLM{err: assert.AnError}, cfg,
		pipeline.LearnOptions{})
	require.Error(t, err)

	after, err := pipeline.RunLearn(t.Context(), s, &learnLLM{response: oneCandidate}, cfg,
		pipeline.LearnOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, after.CorrectionsRead,
		"a failed pass must leave the correction to be retried, not skip it")
}

// TestKeepCandidates_PersistsWhatWasAlreadyDrafted is the fix for a real design flaw,
// found by running `unjira learn --all` rather than by review.
//
// The first version re-ran RunLearn with Keep set, on the theory that the watermark had
// not moved so the same corrections would be re-read. They were — but the model does not
// return the same rule NAMES twice. The draft offered "skip-no-op-progress-comments", the
// second pass offered "comment-must-add-information", and --all failed with "no drafted
// rule named ...". Both names were fine; the assumption that they would be stable was not.
//
// So keeping takes the candidates a caller already holds. That also halves the cost and
// guarantees a reviewer can only keep prose they actually read.
func TestKeepCandidates_PersistsWhatWasAlreadyDrafted(t *testing.T) {
	s, cfg, _ := learnFixture(t)

	drafted, err := pipeline.RunLearn(t.Context(), s, &learnLLM{response: oneCandidate}, cfg,
		pipeline.LearnOptions{})
	require.NoError(t, err)
	require.Len(t, drafted.Candidates, 1)

	// No client, no second distillation: the candidates in hand are what gets written.
	written, advanced, err := pipeline.KeepCandidates(s, cfg, drafted,
		[]string{drafted.Candidates[0].Name})
	require.NoError(t, err)

	assert.Equal(t, []string{"say-what-changed"}, written)
	assert.True(t, advanced, "keeping a rule advances the watermark")

	loaded, err := rules.Load(cfg.RulesDir())
	require.NoError(t, err)
	require.Len(t, loaded, 1, "and the written rule is loadable by the prompts")
}

// TestKeepCandidates_DoesNotAdvanceOnAWriteFailure keeps a collision from costing the
// corrections. If the watermark advanced before the write, a refused overwrite would lose
// the feedback entirely — worse than the collision it was protecting against.
func TestKeepCandidates_DoesNotAdvanceOnAWriteFailure(t *testing.T) {
	s, cfg, _ := learnFixture(t)

	drafted, err := pipeline.RunLearn(t.Context(), s, &learnLLM{response: oneCandidate}, cfg,
		pipeline.LearnOptions{})
	require.NoError(t, err)

	// Pre-create the file so the write is refused.
	require.NoError(t, os.WriteFile(
		filepath.Join(cfg.RulesDir(), "say-what-changed.md"), []byte("existing"), 0o600))

	_, advanced, err := pipeline.KeepCandidates(s, cfg, drafted,
		[]string{"say-what-changed"})
	require.Error(t, err)
	assert.False(t, advanced, "a failed write must not advance the watermark")

	again, err := pipeline.RunLearn(t.Context(), s, &learnLLM{response: oneCandidate}, cfg,
		pipeline.LearnOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, again.CorrectionsRead,
		"the correction must still be offered, or the feedback is lost to a name collision")
}

// rule inserts one more action and rules on it with feedback, which is what makes it a
// correction.
func rule(t *testing.T, s *store.Store, feedback string) int64 {
	t.Helper()

	nid, err := s.InsertNarrative(time.Now().Add(-time.Hour), time.Now(), "more work", "s")
	require.NoError(t, err)
	id, err := s.InsertAction(store.ActionRow{
		NarrativeID: nid, Type: "comment", IssueKey: "DEVSBX-2",
		Payload: `{"body":"other prose"}`, Status: store.StatusProposed,
	})
	require.NoError(t, err)
	require.NoError(t, s.UpdateActionStatusAndFeedback(id, store.StatusRejected, feedback))

	return id
}

// TestKeepCandidates_ACorrectionDecidedAfterTheDraftIsOfferedNextPass is F31's second
// half. `unjira learn` drafts, which includes the model call, and keeps afterwards. A
// ruling made in between was never drafted from, so the watermark must not cover it — it
// must be the high-water mark of what the draft READ, not the clock at keep.
func TestKeepCandidates_ACorrectionDecidedAfterTheDraftIsOfferedNextPass(t *testing.T) {
	path := filepath.Join(t.TempDir(), "learn.db")
	s, cfg, readID := learnFixtureAt(t, path)

	drafted, err := pipeline.RunLearn(t.Context(), s, &learnLLM{response: oneCandidate}, cfg,
		pipeline.LearnOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, drafted.CorrectionsRead)

	lateID := rule(t, s, "ruled while the draft was with the model")

	// The late ruling is given EXACTLY the decided_at of the correction the draft read,
	// written directly because the store API stamps decided_at itself. That makes this
	// test discriminate deterministically, with no sleep (design-notes #40 is about
	// exactly that trap):
	//   - the read-through cursor offers it: same instant, but its id was not read;
	//   - a clock reading taken at keep cannot be earlier than that instant, so a
	//     regression to "watermark = now at keep" skips it, every run, not only when
	//     the ruling and the keep happen to share a millisecond.
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`UPDATE actions SET decided_at = (SELECT decided_at FROM actions WHERE id = ?)
		WHERE id = ?`, readID, lateID)
	require.NoError(t, err)

	_, advanced, err := pipeline.KeepCandidates(s, cfg, drafted, []string{"say-what-changed"})
	require.NoError(t, err)
	require.True(t, advanced)

	next, err := pipeline.RunLearn(t.Context(), s, &learnLLM{response: `[]`}, cfg,
		pipeline.LearnOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, next.CorrectionsRead,
		"the late correction was never drafted from, so it must be offered now")
}

// TestKeepCandidates_NeverMovesTheWatermarkBackwards covers a two-draft interleaving: a
// later draft kept first, then an earlier one. Moving the cursor back to the earlier
// draft's read would re-offer corrections already distilled.
func TestKeepCandidates_NeverMovesTheWatermarkBackwards(t *testing.T) {
	s, cfg, _ := learnFixture(t)

	early, err := pipeline.RunLearn(t.Context(), s, &learnLLM{response: oneCandidate}, cfg,
		pipeline.LearnOptions{})
	require.NoError(t, err)

	rule(t, s, "a second correction")
	late, err := pipeline.RunLearn(t.Context(), s, &learnLLM{response: `[
		{"name":"second-rule","scope":"reconciler","body":"second norm"}
	]`}, cfg, pipeline.LearnOptions{})
	require.NoError(t, err)
	require.Equal(t, 2, late.CorrectionsRead)

	_, _, err = pipeline.KeepCandidates(s, cfg, late, []string{"second-rule"})
	require.NoError(t, err)
	_, advanced, err := pipeline.KeepCandidates(s, cfg, early, []string{"say-what-changed"})
	require.NoError(t, err)
	assert.True(t, advanced, "keeping still counts as advancing, even when the stored position stands")

	after, err := pipeline.RunLearn(t.Context(), s, &learnLLM{response: `[]`}, cfg,
		pipeline.LearnOptions{})
	require.NoError(t, err)
	assert.Zero(t, after.CorrectionsRead, "both corrections were distilled and kept")
}

// TestKeepCandidates_RefusesADraftThatReadNothing guards the cursor rather than the files:
// a draft with no read position has nothing to advance to, and writing to "" would reset
// the watermark and re-offer everything.
func TestKeepCandidates_RefusesADraftThatReadNothing(t *testing.T) {
	s, cfg, _ := learnFixture(t)

	_, advanced, err := pipeline.KeepCandidates(s, cfg, pipeline.LearnResult{
		Candidates: []rules.Candidate{{Name: "orphan", Scope: rules.ScopeReconciler, Body: "b"}},
	}, []string{"orphan"})
	require.ErrorContains(t, err, "read no corrections")
	assert.False(t, advanced)

	entries, readErr := os.ReadDir(cfg.RulesDir())
	require.NoError(t, readErr)
	assert.Empty(t, entries, "refused before any rule reached disk")
}

// TestRunLearn_RefusesAMalformedWatermark keeps a corrupt cursor from reading as "never
// learned", which would re-offer every correction a reviewer has already ruled on.
func TestRunLearn_RefusesAMalformedWatermark(t *testing.T) {
	s, cfg, _ := learnFixture(t)
	require.NoError(t, s.SetCursor("learn", "corrections", "garbage"))

	client := &learnLLM{response: oneCandidate}
	_, err := pipeline.RunLearn(t.Context(), s, client, cfg, pipeline.LearnOptions{})
	require.ErrorContains(t, err, `"garbage"`)
	require.ErrorContains(t, err, "refusing to reset it")
	assert.Zero(t, client.calls, "and it fails before spending a model call")
}

// TestRunLearn_RefusesAnEarlierCursorForm: a whole-second timestamp (pre-F31) and the F31
// decided_at JSON can only be in a store old enough for Open to refuse, but a hand-edited
// cursor could hold one. Neither says what it covered in sequence terms, so both are refused
// rather than read as "never learned".
func TestRunLearn_RefusesAnEarlierCursorForm(t *testing.T) {
	for _, raw := range []string{
		"2026-01-01T12:00:05Z",
		`{"decided_at":"2026-01-01T12:00:05.000Z","ids":[]}`,
	} {
		s, cfg, _ := learnFixture(t)
		require.NoError(t, s.SetCursor("learn", "corrections", raw))

		client := &learnLLM{response: oneCandidate}
		_, err := pipeline.RunLearn(t.Context(), s, client, cfg, pipeline.LearnOptions{})
		require.ErrorContains(t, err, "refusing to reset it", "cursor %q", raw)
		assert.Zero(t, client.calls)
	}
}

// TestKeepCandidates_ALessonAddedToAnEarlierRejectionIsOfferedNextPass is F42 end to end:
// a silent rejection, a learn pass that keeps a rule from a different correction, then
// `actions decide --edit` adds the lesson to the first. The next pass must offer it.
func TestKeepCandidates_ALessonAddedToAnEarlierRejectionIsOfferedNextPass(t *testing.T) {
	s, cfg, _ := learnFixture(t)
	nid, err := s.InsertNarrative(time.Now().Add(-time.Hour), time.Now(), "other work", "s")
	require.NoError(t, err)
	silent, err := s.InsertAction(store.ActionRow{
		NarrativeID: nid, Type: "comment", IssueKey: "DEVSBX-2",
		Payload: `{"body":"rejected at first without a word"}`, Status: store.StatusProposed,
	})
	require.NoError(t, err)
	require.NoError(t, s.UpdateActionStatusAndFeedback(silent, store.StatusRejected, ""))

	drafted, err := pipeline.RunLearn(t.Context(), s, &learnLLM{response: oneCandidate}, cfg,
		pipeline.LearnOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, drafted.CorrectionsRead, "the silent rejection is not a correction yet")
	_, advanced, err := pipeline.KeepCandidates(s, cfg, drafted, []string{drafted.Candidates[0].Name})
	require.NoError(t, err)
	require.True(t, advanced)

	require.NoError(t, s.UpdateActionStatusAndFeedback(silent, store.StatusEdited, "say which tickets"))

	next, err := pipeline.RunLearn(t.Context(), s, &learnLLM{response: "[]"}, cfg, pipeline.LearnOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, next.CorrectionsRead, "the lesson added later must be offered")
}
