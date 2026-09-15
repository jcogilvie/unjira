package jira

// issuebody.go turns an issue's own summary and description into an event.
//
// Finding F14: every other constructor here derives from something that HAPPENED TO
// an issue — a changelog entry, a comment — so a description written at creation and
// never edited did not exist as far as unjira was concerned. Measured before this
// landed: of 99 collected issues, 29 had description text and 70 had none. The client
// already requests `fields=*all`, so the body was fetched and discarded.
//
// The cost was a misdiagnosis, not just missing data. A `create` was proposed for
// work PAAS-3905 already tracked, and the deterministic cross-reference existed the
// whole time: that issue's description contains
// "PR: Sanyaku/platform-vision#1 (vp-feedback-refactor) — MERGED", and the narrative
// was a session in the `vision` repo. Verified against the live instance.

import (
	"fmt"
	"strings"
	"time"

	"github.com/jcogilvie/unjira/internal/events"
)

// EventFromIssueBody builds one event carrying an issue's current summary and
// description, or reports false when there is nothing worth recording.
//
// occurredAt is the issue's own `updated` time rather than now(): the event describes
// the body as of that revision, and dating it to collection time would make a
// three-month-old description look like today's work to every window-based query.
//
// Not an error return, unlike its sibling constructors. They fail on malformed input —
// a changelog entry with no id is a contract violation worth surfacing. An issue with
// an empty body is ordinary, so this is (Event, bool) and the caller simply skips it.
func EventFromIssueBody(
	ic IssueContext, summary string, description any, occurredAt time.Time,
) (events.Event, bool) {
	summary = strings.TrimSpace(summary)
	body := strings.TrimSpace(adfText(description))

	if summary == "" && body == "" {
		return events.Event{}, false
	}

	text := fmt.Sprintf("%s: %s", ic.Key, summary)
	if body != "" {
		text = fmt.Sprintf("%s\n\n%s", text, body)
	}

	// The ExternalID embeds the issue's `updated` time, and that choice is the
	// idempotence answer F14 asked for. Events are written with INSERT OR IGNORE keyed
	// on (source, external_id), so a FIXED id per issue would freeze the first body
	// ever collected and silently ignore every later revision — the same class of bug
	// as #176, where re-collecting never backfills artifacts onto existing rows.
	//
	// Jira advances `updated` whenever any field changes, so an edited description
	// mints a new id and a fresh row, while a re-collect of unchanged text dedupes
	// exactly as the other constructors do. It over-collects: an unrelated field
	// change also mints a new id. That is the safe direction — a duplicate body event
	// is inert because it is a tracker record and therefore not work evidence, whereas
	// a missed revision is invisible forever.
	evt := events.NewEvent(Name,
		fmt.Sprintf("%s:body:%d", ic.Key, occurredAt.UTC().Unix()), occurredAt, text)

	evt.Artifacts[events.ArtifactIssueKey] = ic.Key
	evt.Artifacts["project_key"] = ic.ProjectKey
	if ic.Connection != "" {
		evt.Artifacts[events.ArtifactConnection] = ic.Connection
	}
	evt.RawRef = ic.browseURL()

	// The tracker describing itself, so NOT work evidence. Without this marker a
	// narrative holding only an issue body would look like observed work, and the
	// reconciler could draft a comment restating text the issue already contains —
	// precisely the paraphrase-Jira-onto-Jira loop PR #39's tracker-echo filter exists
	// to stop.
	//
	// The marker deliberately does not hide it from gatherCandidates, which walks
	// artifacts regardless: the cross-reference is still available for ATTRIBUTION.
	// Usable for matching, unusable for narration.
	events.SetTrackerRecord(&evt)

	return evt, true
}

// adfText flattens Jira's description field to plain text.
//
// Necessary rather than defensive: Jira Cloud v3 returns description as ADF (an
// Atlassian Document Format object), verified live as a map with keys
// {content, type, version}. clients/jira's existing `fields["description"].(string)`
// yields "" against that — silently, which is how F14 went unnoticed while the code
// looked like it handled descriptions.
//
// Both shapes are live in one deployment: the changelog's `toString` values arrive as
// plain wiki markup ("h2. Summary\n\n..."), so a string input is passed through
// rather than treated as an error.
//
// Recursive because the text that matters sits arbitrarily deep — the PR reference
// that motivated this is two levels down, and list items nest three. A flattener that
// read only top-level content would pass a hand-written flat fixture and lose every
// real description, so issuebody_test.go's fixture is deliberately nested.
//
// An unrecognised type yields "" rather than a formatted rendering: "%v" of a
// map would put Go syntax into an event summary, where it would be indexed as if it
// were prose.
func adfText(node any) string {
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
		if child := adfText(n["content"]); child != "" {
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
		if text := adfText(node); text != "" {
			parts = append(parts, text)
		}
	}

	return strings.Join(parts, " ")
}

// stringOf reads a value that should be a string, yielding "" when it is absent or
// some other type. A summary is nominally always a string, but a nil fields map or an
// unexpected shape must degrade to "an issue with no summary" rather than panic
// mid-collection.
func stringOf(v any) string {
	s, _ := v.(string)

	return s
}
