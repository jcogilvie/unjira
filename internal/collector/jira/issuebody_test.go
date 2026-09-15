package jira

// issuebody_test.go covers finding F14: an issue's own summary and description were
// never ingested, so 70 of 99 collected issues had no description text.
//
// The collector derived events only from things that HAPPENED TO an issue —
// changelog entries and comments — so a description written at creation and never
// edited did not exist as far as unjira was concerned. The client already requests
// `fields=*all`; the body was fetched and discarded before reaching the event
// builders.
//
// It cost a real misdiagnosis. A create was proposed for narrative 32
// ("Architectural vision & 3-year roadmap document") even though PAAS-3905 tracked
// that work, and the deterministic cross-reference existed all along — PAAS-3905's
// description contains "PR: Sanyaku/platform-vision#1" and the session ran in the
// `vision` repo. Verified against the live instance before this was written.
//
// Two shapes matter, and both are covered here because the second is what makes the
// first useless if missed:
//   - the event must exist at all, for an issue with no changelog and no comments
//   - `fields.description` is ADF on Jira Cloud v3 (verified live: a
//     map[string]any with keys {content, type, version}), so a `.(string)`
//     assertion silently yields empty. Flattening is the load-bearing half.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/events"
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

// TestEventFromIssueBody_CarriesSummaryAndDescription is the finding itself: the
// event must exist for an issue whose body was never edited.
func TestEventFromIssueBody_CarriesSummaryAndDescription(t *testing.T) {
	ic := IssueContext{
		Key: "PAAS-3905", ProjectKey: "PAAS", Connection: "dev",
		Site: "https://example.atlassian.net",
	}
	updated := time.Date(2026, 7, 13, 10, 0, 16, 0, time.UTC)

	evt, ok := EventFromIssueBody(ic,
		"Platform vision + roadmap — VP feedback refactor", adfFixture(t), updated)

	require.True(t, ok)
	assert.Contains(t, evt.Summary, "Platform vision + roadmap",
		"the summary line is often the most searchable text an issue has")
	assert.Contains(t, evt.Summary, "PR: Sanyaku/platform-vision#1",
		"and the description must be in the same event, since that is what carries the reference")
	assert.Equal(t, "PAAS-3905", evt.Artifacts[events.ArtifactIssueKey])
	assert.Equal(t, "PAAS", evt.Artifacts["project_key"])
	assert.Equal(t, "dev", evt.Artifacts[events.ArtifactConnection])
}

// TestEventFromIssueBody_IsATrackerRecord is the interaction with PR #39's
// suppression. This event is the tracker's own text, so it is NOT evidence that work
// happened — commenting on an issue because we read its description back would be
// exactly the paraphrase-Jira-onto-Jira loop tracker-echo exists to stop.
//
// It stays readable as a CANDIDATE source: gatherCandidates walks artifacts
// regardless of the marker, so the cross-reference is still available for matching.
// That asymmetry is the point — usable for attribution, not for narration.
func TestEventFromIssueBody_IsATrackerRecord(t *testing.T) {
	ic := IssueContext{Key: "PAAS-3905", ProjectKey: "PAAS"}

	evt, ok := EventFromIssueBody(ic, "s", adfFixture(t), time.Now())
	require.True(t, ok)

	assert.True(t, events.IsTrackerRecord(evt),
		"an issue's own body is the tracker describing itself; treating it as work evidence would "+
			"let a comment be drafted from text the issue already contains")
	assert.False(t, events.AnyWorkEvidence([]events.Event{evt}),
		"so a narrative holding only this must not be considered to have work evidence")
}

// TestEventFromIssueBody_ExternalIDTracksTheUpdatedTime is the idempotence question
// F14 raised. Events are written with INSERT OR IGNORE keyed on
// (source, external_id), so a FIXED id per issue would freeze the first body ever
// collected and silently ignore every later revision.
//
// The issue's own `updated` timestamp is the available discriminator: Jira advances
// it whenever any field changes, so an edited description yields a new id and a fresh
// row, while a re-collect of unchanged text dedupes exactly as the other constructors
// do. It over-collects — an unrelated field change also mints a new id — which is the
// safe direction: a duplicate body event is inert (it is a tracker record, so it is
// not work evidence), whereas a missed revision is invisible forever.
func TestEventFromIssueBody_ExternalIDTracksTheUpdatedTime(t *testing.T) {
	ic := IssueContext{Key: "PAAS-3905", ProjectKey: "PAAS"}
	first := time.Date(2026, 7, 13, 10, 0, 16, 0, time.UTC)
	later := first.Add(time.Hour)

	before, ok := EventFromIssueBody(ic, "s", "body one", first)
	require.True(t, ok)
	after, ok := EventFromIssueBody(ic, "s", "body two", later)
	require.True(t, ok)

	assert.NotEqual(t, before.ExternalID, after.ExternalID,
		"an edited body must not dedupe against the old one under INSERT OR IGNORE")

	again, ok := EventFromIssueBody(ic, "s", "body one", first)
	require.True(t, ok)
	assert.Equal(t, before.ExternalID, again.ExternalID,
		"but re-collecting unchanged text must dedupe, or every pass grows the table")

	assert.True(t, strings.HasPrefix(before.ExternalID, "PAAS-3905:body:"),
		"and it must follow the <KEY>:<kind>:<id> convention the other constructors use, got %q",
		before.ExternalID)
}

// TestEventFromIssueBody_RefusesAnEmptyBody stops the store filling with noise. An
// issue with neither summary nor usable description has nothing to contribute, and an
// event whose summary is just the key would be indexed as if it were content.
func TestEventFromIssueBody_RefusesAnEmptyBody(t *testing.T) {
	ic := IssueContext{Key: "PAAS-1", ProjectKey: "PAAS"}

	_, ok := EventFromIssueBody(ic, "", nil, time.Now())
	assert.False(t, ok, "no summary and no description is nothing to record")

	_, ok = EventFromIssueBody(ic, "", map[string]any{"type": "doc"}, time.Now())
	assert.False(t, ok, "an ADF document with no text is equally empty")
}

// TestEventFromIssueBody_SummaryAloneIsEnough is the common case for a freshly-filed
// ticket: a one-line summary and no description yet. That summary is still the most
// searchable text the issue has, so it must be recorded.
func TestEventFromIssueBody_SummaryAloneIsEnough(t *testing.T) {
	ic := IssueContext{Key: "PAAS-1", ProjectKey: "PAAS"}

	evt, ok := EventFromIssueBody(ic, "parameterGroup IaC translator", nil, time.Now())

	require.True(t, ok)
	assert.Contains(t, evt.Summary, "parameterGroup IaC translator")
}

// TestSearchFields_RequestTheBody is the test whose absence let F14 ship inert.
//
// EventFromIssueBody was correct, its unit tests were green, and collectIssue called
// it — but searchFields asked Jira for only {key, project, updated}, so the
// constructor received "" for both summary and description on every issue and
// returned false every time. A full re-collect produced ZERO body events.
//
// Nothing connected the two facts: the constructor's tests supply their arguments
// directly, and the collector's tests do not assert what the search requested. This
// asserts the seam rather than either side of it.
func TestSearchFields_RequestTheBody(t *testing.T) {
	assert.Contains(t, searchFields, fieldDescription,
		"EventFromIssueBody cannot see a field the search did not ask for; F14 was wired, "+
			"tested, and starved by this list")
	assert.Contains(t, searchFields, fieldSummary,
		"a freshly-filed ticket often has a summary and no description, and the summary is "+
			"then the only searchable text the issue has")
	assert.Contains(t, searchFields, "updated",
		"and `updated` must stay: it is both the watermark the query advances on and the "+
			"body event's idempotence key")
}

// TestTrackedFieldsAndSearchFieldsAgree pins the duality that hid the bug: the same two
// field names are needed as CHANGELOG field names (an edit produces an event) and as
// SEARCH field names (the search must request them). Two lists, one fact.
func TestTrackedFieldsAndSearchFieldsAgree(t *testing.T) {
	for _, f := range []string{fieldSummary, fieldDescription} {
		assert.Contains(t, trackedFields, f,
			"%s must be tracked so an EDIT to it produces an event", f)
		assert.Contains(t, searchFields, f,
			"...and requested, so the issue's CURRENT value is available to EventFromIssueBody; "+
				"F14 shipped with the first half only", f)
	}
}
