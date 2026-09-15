package jira

// adf_test.go covers ADF flattening, which both the collector and the read path need.
//
// The read path is why this matters most: normalizeIssue feeds Issue.Description into
// the matching prompt, and a bare `.(string)` against Jira Cloud v3's ADF object
// yielded "" for every issue — silently. The collector hit the same wall separately
// (F14), which is why the flattener lives here, in the package both sides import.

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// adfFixture is the real shape returned by Jira Cloud v3 for PAAS-3905, reduced to
// the nodes that matter. Nested on purpose: a flattener that only reads top-level
// content would pass a flat fixture and still lose every real description.
func adfFixture(t *testing.T) map[string]any {
	t.Helper()

	const raw = `{
	  "type": "doc",
	  "version": 1,
	  "content": [
	    {"type": "heading", "content": [{"type": "text", "text": "Vision doc"}]},
	    {"type": "paragraph", "content": [
	      {"type": "text", "text": "PR: Sanyaku/platform-vision#1 (vp-feedback-refactor) "},
	      {"type": "text", "text": "— MERGED 2026-07-09T21:38:18Z to main"}
	    ]},
	    {"type": "bulletList", "content": [
	      {"type": "listItem", "content": [
	        {"type": "paragraph", "content": [{"type": "text", "text": "three-year roadmap"}]}
	      ]}
	    ]}
	  ]
	}`

	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &out))

	return out
}

// TestADFText_FlattensNestedTextNodes is the half that decides whether the fix works
// at all. Jira Cloud v3 returns ADF, and clients/jira's existing
// `fields["description"].(string)` yields "" against it — silently, which is how this
// went unnoticed.
func TestADFText_FlattensNestedTextNodes(t *testing.T) {
	got := ADFText(adfFixture(t))

	assert.Contains(t, got, "PR: Sanyaku/platform-vision#1",
		"the cross-reference is the whole point: this exact string is what links a narrative in the "+
			"vision repo to the issue that tracks it, and it sits two levels down in the document")
	assert.Contains(t, got, "three-year roadmap",
		"text inside a bulletList/listItem/paragraph chain must survive; a flattener that reads only "+
			"top-level content would pass a flat fixture and lose every real description")
	assert.Contains(t, got, "Vision doc", "heading text counts as description text")
}

// TestADFText_AcceptsAPlainString keeps the fix working on a server that returns
// wiki markup rather than ADF. The changelog already does exactly this — its
// `toString` values are plain text — so both shapes are live in one deployment.
func TestADFText_AcceptsAPlainString(t *testing.T) {
	assert.Equal(t, "h2. Summary\n\nplain wiki markup",
		ADFText("h2. Summary\n\nplain wiki markup"))
}

// TestADFText_EmptyForNothingUsable pins the degradation. An absent description and
// an unrecognised shape must both yield "", not a Go-syntax rendering of the value —
// `map[content:...]` in an event summary would be indexed as if it were prose.
func TestADFText_EmptyForNothingUsable(t *testing.T) {
	assert.Empty(t, ADFText(nil))
	assert.Empty(t, ADFText(map[string]any{"type": "doc"}), "a doc with no content is empty")
	assert.Empty(t, ADFText(42), "an unexpected type degrades to empty, never to %!v(int=42)")
}
