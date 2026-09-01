// Package rules_test exercises Load/ForScope/Render against real files on
// disk (a temp dir per test) rather than any in-memory fixture, since the
// behavior under test is file discovery and frontmatter parsing.
package rules_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/rules"
)

// writeRule writes name (without .md) plus contents into dir, returning the
// full path for callers that want it.
func writeRule(t *testing.T, dir, name, contents string) string {
	t.Helper()

	path := filepath.Join(dir, name+".md")
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))

	return path
}

func TestLoad_ParsesWellFormedRule(t *testing.T) {
	dir := t.TempDir()
	writeRule(t, dir, "auth-epic", `---
scope: correlator
confidence: high
learned: 2026-07-11
source: "review-queue correction on action #42"
---

Commits touching `+"`auth/`"+` belong to the SSO epic (PROJ-88), not new tickets.

A second paragraph continues the body, to prove multi-paragraph prose survives
intact rather than being collapsed to its first line.
`)

	got, err := rules.Load(dir)

	require.NoError(t, err)
	require.Len(t, got, 1)

	r := got[0]
	assert.Equal(t, "auth-epic", r.Name)
	assert.Equal(t, rules.ScopeCorrelator, r.Scope)
	assert.Equal(t, rules.ConfidenceHigh, r.Confidence)
	assert.Equal(t, "2026-07-11", r.Learned)
	assert.Equal(t, "review-queue correction on action #42", r.Source)
	assert.Contains(t, r.Body, "Commits touching `auth/` belong to the SSO epic (PROJ-88), not new tickets.")
	assert.Contains(t, r.Body, "A second paragraph continues the body")
}

func TestLoad_SkipsReadmeWithNoFrontmatter(t *testing.T) {
	dir := t.TempDir()
	writeRule(t, dir, "README", "# Learned rules\n\nNo frontmatter here at all.\n")
	writeRule(t, dir, "real-rule", `---
scope: correlator
confidence: high
learned: 2026-07-16
source: test
---

Body text.
`)

	got, err := rules.Load(dir)

	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "real-rule", got[0].Name)
}

func TestLoad_MissingDirectoryReturnsEmptyNoError(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")

	got, err := rules.Load(dir)

	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestLoad_UnknownScopeErrorsNamingFileAndValue(t *testing.T) {
	dir := t.TempDir()
	writeRule(t, dir, "typo-scope", `---
scope: correlater
confidence: high
learned: 2026-07-16
source: test
---

Body text.
`)

	_, err := rules.Load(dir)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "typo-scope")
	assert.Contains(t, err.Error(), "correlater")
}

func TestLoad_UnknownConfidenceErrorsNamingFileAndValue(t *testing.T) {
	dir := t.TempDir()
	writeRule(t, dir, "typo-confidence", `---
scope: correlator
confidence: maybe
learned: 2026-07-16
source: test
---

Body text.
`)

	_, err := rules.Load(dir)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "typo-confidence")
	assert.Contains(t, err.Error(), "maybe")
}

func TestLoad_DoubleQuotedScalarParsesToBareValue(t *testing.T) {
	dir := t.TempDir()
	writeRule(t, dir, "quoted-scope", `---
scope: "correlator"
confidence: high
learned: 2026-07-16
source: test
---

Body text.
`)

	got, err := rules.Load(dir)

	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, rules.ScopeCorrelator, got[0].Scope, "double-quoted scalar must parse to the bare value, not the literal quotes")
}

func TestLoad_SingleQuotedScalarParsesToBareValue(t *testing.T) {
	dir := t.TempDir()
	writeRule(t, dir, "quoted-scope", `---
scope: 'correlator'
confidence: high
learned: 2026-07-16
source: test
---

Body text.
`)

	got, err := rules.Load(dir)

	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, rules.ScopeCorrelator, got[0].Scope, "single-quoted scalar must parse to the bare value, not the literal quotes")
}

func TestLoad_TrailingInlineCommentIsStripped(t *testing.T) {
	dir := t.TempDir()
	writeRule(t, dir, "commented-scope", `---
scope: correlator  # this is a comment
confidence: high
learned: 2026-07-16
source: test
---

Body text.
`)

	got, err := rules.Load(dir)

	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, rules.ScopeCorrelator, got[0].Scope, "a trailing inline comment must not become part of the value")
}

func TestLoad_QuotedHashInSourceIsPreserved(t *testing.T) {
	dir := t.TempDir()
	writeRule(t, dir, "hash-source", `---
scope: correlator
confidence: high
learned: 2026-07-16
source: "review-queue correction on action #42"
---

Body text.
`)

	got, err := rules.Load(dir)

	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "review-queue correction on action #42", got[0].Source, "a quoted source containing a space-hash must round-trip to the literal value, not be truncated at the comment")
}

func TestLoad_UnquotedDateStaysString(t *testing.T) {
	dir := t.TempDir()
	writeRule(t, dir, "dated-rule", `---
scope: correlator
confidence: high
learned: 2026-07-16
source: test
---

Body text.
`)

	got, err := rules.Load(dir)

	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.IsType(t, "", got[0].Learned)
	assert.Equal(t, "2026-07-16", got[0].Learned, "an unquoted YAML date-shaped scalar must still land as the string it was written as")
}

func TestLoad_MalformedFrontmatterNoClosingDelimiterErrors(t *testing.T) {
	dir := t.TempDir()
	writeRule(t, dir, "unterminated", `---
scope: correlator
confidence: high
learned: 2026-07-16
source: test

Body text, but the frontmatter never closed.
`)

	_, err := rules.Load(dir)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "unterminated")
}

func TestLoad_RealRulesDirectoryLoadsWithoutError(t *testing.T) {
	// Catches drift between rules/README.md's declared format and the actual
	// seeded files — the one check that exercises real content, not a fixture
	// this test file controls.
	got, err := rules.Load("../../rules")
	require.NoError(t, err)

	// The expected count is DERIVED from the directory rather than hardcoded.
	// A literal ("the five seeded rule files") makes every future rule a test
	// failure, which trains whoever adds one to bump the number without reading
	// why it was five — and the number was never the property under test. What
	// matters is that every .md file except README.md parses.
	entries, err := os.ReadDir("../../rules")
	require.NoError(t, err)

	want := 0
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".md" || e.Name() == "README.md" {
			continue
		}
		want++
	}

	require.Positive(t, want, "precondition: the repo seeds at least one rule")
	assert.Len(t, got, want,
		"every seeded rule file must parse; README.md is skipped (it has no frontmatter)")
}

func TestForScope_FiltersAndDoesNotMutateInput(t *testing.T) {
	all := []rules.Rule{
		{Name: "a", Scope: rules.ScopeCorrelator},
		{Name: "b", Scope: rules.ScopeReconciler},
		{Name: "c", Scope: rules.ScopeCorrelator},
	}

	got := rules.ForScope(all, rules.ScopeCorrelator)

	require.Len(t, got, 2)
	assert.Equal(t, "a", got[0].Name)
	assert.Equal(t, "c", got[1].Name)

	require.Len(t, all, 3, "ForScope must not mutate its input")
	assert.Equal(t, rules.ScopeReconciler, all[1].Scope)
}

func TestRender_EmptyInputReturnsEmptyString(t *testing.T) {
	assert.Empty(t, rules.Render(nil))
	assert.Empty(t, rules.Render([]rules.Rule{}))
}

func TestRender_MarksProvisionalDistinctlyFromHighConfidence(t *testing.T) {
	got := rules.Render([]rules.Rule{
		{Name: "high-one", Scope: rules.ScopeCorrelator, Confidence: rules.ConfidenceHigh, Body: "Do X."},
		{Name: "provisional-one", Scope: rules.ScopeCorrelator, Confidence: rules.ConfidenceProvisional, Body: "Maybe do Y."},
	})

	assert.Contains(t, got, "high-one")
	assert.Contains(t, got, "Do X.")
	assert.Contains(t, got, "provisional-one")
	assert.Contains(t, got, "Maybe do Y.")
	assert.Contains(t, got, "provisional", "provisional rules must be labelled, not indistinguishable from high-confidence ones")
}
