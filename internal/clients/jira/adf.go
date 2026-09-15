package jira

// adf.go flattens Atlassian Document Format to plain text.

import "strings"

// ADFText flattens Jira's description field to plain text.
//
// Necessary rather than defensive: Jira Cloud v3 returns description as ADF (an
// Atlassian Document Format object), verified live as a map with keys
// {content, type, version}. A bare `fields["description"].(string)` yields ""
// against that — silently, which is how this went unnoticed twice: once in the
// collector (F14) and once here on the read path, where normalizeIssue feeds
// Issue.Description into the matching prompt.
//
// Lives in this package rather than in collector/jira because collector/jira already
// imports it; the dependency only runs one way. Both callers need the same
// flattening, and two copies would drift.
//
// Both shapes are live in one deployment: the changelog's `toString` values arrive as
// plain wiki markup ("h2. Summary\n\n..."), so a string input is passed through
// rather than treated as an error.
//
// Recursive because the text that matters sits arbitrarily deep — the PR reference
// that motivated this is two levels down, and list items nest three. A flattener that
// read only top-level content would pass a hand-written flat fixture and lose every
// real description, so both callers' fixtures are deliberately nested.
//
// An unrecognised type yields "" rather than a formatted rendering: "%v" of a
// map would put Go syntax into an event summary, where it would be indexed as if it
// were prose.
func ADFText(node any) string {
	switch n := node.(type) {
	case string:
		return n
	case []any:
		return joinNonEmpty(n)
	case map[string]any:
		// A text node's own text, then whatever its children hold. Both are checked
		// because a node can carry text AND content, and taking only the first would
		// truncate at the first leaf.
		var parts []string
		if text, ok := n["text"].(string); ok && text != "" {
			parts = append(parts, text)
		}
		if child := ADFText(n["content"]); child != "" {
			parts = append(parts, child)
		}

		return strings.Join(parts, "")
	default:
		return ""
	}
}

// joinNonEmpty flattens a list of ADF nodes, separating block-level results with a
// space so adjacent paragraphs do not run their words together.
func joinNonEmpty(nodes []any) string {
	parts := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if text := ADFText(node); text != "" {
			parts = append(parts, text)
		}
	}

	return strings.Join(parts, " ")
}

// adfText is the in-package spelling, so normalizeIssue and its sibling readers do
// not qualify a call to their own package.
func adfText(node any) string { return ADFText(node) }
