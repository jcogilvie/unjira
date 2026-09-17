package rules_test

// disabled_test.go covers the `disabled` frontmatter flag.
//
// The reviewer's argument for it: writing a rule file is not a one-way door, since the
// undo is deleting the file — but deletion loses WHY the rule existed. A flag keeps the
// record and the off switch, which matters most for the rules slice 7 distils: a
// provisional norm that turns out to be wrong should be recorded as tried-and-rejected,
// not vanish as though nobody ever proposed it.
//
// It also composes with distillation. A candidate a reviewer is unsure about can land
// disabled rather than not landing at all, which is strictly more informative than a
// decision nobody wrote down.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/rules"
)

// writeRuleWithFrontmatter builds a rule file from its frontmatter and body, on top of
// rules_test.go's writeRule (which takes whole contents).
func writeRuleWithFrontmatter(t *testing.T, dir, name, frontmatter, body string) {
	t.Helper()

	writeRule(t, dir, name, "---\n"+frontmatter+"\n---\n\n"+body+"\n")
}

// TestLoad_SkipsADisabledRule is the flag. A disabled rule must not reach any prompt.
func TestLoad_SkipsADisabledRule(t *testing.T) {
	dir := t.TempDir()
	writeRuleWithFrontmatter(t, dir, "kept", "scope: reconciler\nconfidence: high\nlearned: 2026-09-17", "keep me")
	writeRuleWithFrontmatter(t, dir, "turned-off",
		"scope: reconciler\nconfidence: high\nlearned: 2026-09-17\ndisabled: true", "skip me")

	got, err := rules.Load(dir)
	require.NoError(t, err)

	require.Len(t, got, 1, "a disabled rule must not load")
	assert.Equal(t, "kept", got[0].Name)
}

// TestLoad_AbsentDisabledMeansEnabled keeps every existing rule file working unchanged.
// The flag is opt-out, so a file that never heard of it behaves exactly as before.
func TestLoad_AbsentDisabledMeansEnabled(t *testing.T) {
	dir := t.TempDir()
	writeRuleWithFrontmatter(t, dir, "no-flag", "scope: correlator\nconfidence: provisional\nlearned: 2026-09-17", "b")

	got, err := rules.Load(dir)
	require.NoError(t, err)

	require.Len(t, got, 1, "absent must mean enabled: every rule written before this flag existed")
}

// TestLoad_DisabledFalseLoads pins the explicit-on case, so a reviewer can re-enable by
// flipping the value rather than by deleting the line and hoping.
func TestLoad_DisabledFalseLoads(t *testing.T) {
	dir := t.TempDir()
	writeRuleWithFrontmatter(t, dir, "explicitly-on",
		"scope: correlator\nconfidence: high\nlearned: 2026-09-17\ndisabled: false", "b")

	got, err := rules.Load(dir)
	require.NoError(t, err)

	assert.Len(t, got, 1)
}

// TestLoad_ADisabledRuleStillValidatesItsScope is the property that keeps the flag from
// becoming a way to smuggle broken files in. Disabling a rule turns it off; it does not
// exempt it from being a well-formed rule, or a reviewer re-enabling it later would
// discover the breakage at that moment instead of now.
func TestLoad_ADisabledRuleStillValidatesItsScope(t *testing.T) {
	dir := t.TempDir()
	writeRuleWithFrontmatter(t, dir, "broken-but-off",
		"scope: nonsense\nconfidence: high\nlearned: 2026-09-17\ndisabled: true", "b")

	_, err := rules.Load(dir)

	require.Error(t, err,
		"a disabled rule must still parse: off is not the same as exempt, and deferring the error "+
			"to whenever somebody re-enables it hides it")
	assert.Contains(t, err.Error(), "nonsense")
}
