package jira_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	collectorjira "github.com/jcogilvie/unjira/internal/collector/jira"
)

// testIssue is rebuilt per test rather than shared, so one test mutating a map
// cannot affect another.
func testIssue() collectorjira.IssueContext {
	return collectorjira.IssueContext{
		Key:           "PROJ-42",
		ProjectKey:    "PROJ",
		Connection:    "corp",
		Site:          "https://corp.atlassian.net",
		SelfAccountID: "acct-unjira",
	}
}

func TestEventsFromChangelogEntry_EmitsTrackedFields(t *testing.T) {
	tests := []struct {
		name        string
		field       string
		toString    string
		wantEvents  int
		wantExtID   string
		wantSummary string
	}{
		{
			name: "status", field: "status", toString: "In Progress", wantEvents: 1,
			wantExtID: "PROJ-42:status:10001", wantSummary: "PROJ-42 status: To Do → In Progress",
		},
		{
			name: "description", field: "description", toString: "investigate the flaky test",
			wantEvents: 1, wantExtID: "PROJ-42:description:10001",
			wantSummary: "PROJ-42 description: investigate the flaky test",
		},
		{
			name: "summary", field: "summary", toString: "Fix flaky correlator test",
			wantEvents: 1, wantExtID: "PROJ-42:summary:10001",
			wantSummary: "PROJ-42 summary: Fix flaky correlator test",
		},
		{name: "assignee is not tracked", field: "assignee", toString: "Bob", wantEvents: 0},
		{name: "priority is not tracked", field: "priority", toString: "High", wantEvents: 0},
		{name: "labels are not tracked", field: "labels", toString: "backend", wantEvents: 0},
		{name: "sprint is not tracked", field: "Sprint", toString: "Sprint 9", wantEvents: 0},
		{name: "resolution is not tracked", field: "resolution", toString: "Done", wantEvents: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry := map[string]any{
				"id":      "10001",
				"created": "2026-08-20T14:30:00.000+0000",
				"author":  map[string]any{"accountId": "acct-alice", "displayName": "Alice"},
				"items": []any{
					map[string]any{"field": tt.field, "fromString": "To Do", "toString": tt.toString},
				},
			}

			got, err := collectorjira.EventsFromChangelogEntry(testIssue(), entry)

			require.NoError(t, err)
			require.Len(t, got, tt.wantEvents,
				"only status/description/summary are actionable in phase 1")
			if tt.wantEvents == 0 {
				return
			}
			assert.Equal(t, "jira", got[0].Source)
			assert.Equal(t, tt.wantExtID, got[0].ExternalID)
			assert.Equal(t, tt.wantSummary, got[0].Summary)
			assert.Equal(t, "Alice", got[0].Actor)
			assert.Equal(t, "https://corp.atlassian.net/browse/PROJ-42", got[0].RawRef)
			assert.Equal(t,
				time.Date(2026, 8, 20, 14, 30, 0, 0, time.UTC),
				got[0].OccurredAt.UTC(),
				"OccurredAt is when the change happened, not when we collected it")
			assert.Equal(t, "PROJ-42", got[0].Artifacts["issue_key"])
			assert.Equal(t, "PROJ", got[0].Artifacts["project_key"])
			assert.Equal(t, "corp", got[0].Artifacts["connection"])
			assert.Equal(t, tt.field, got[0].Artifacts["field"])
			assert.Equal(t, false, got[0].Artifacts["authored_by_unjira"])
		})
	}
}

func TestEventsFromChangelogEntry_MultipleItemsInOneEntry(t *testing.T) {
	// Jira batches simultaneous field changes into one changelog entry. Both
	// tracked fields must be emitted, and their ExternalIDs must differ or the
	// second silently dedupes away against the first.
	entry := map[string]any{
		"id":      "10007",
		"created": "2026-08-20T14:30:00.000+0000",
		"author":  map[string]any{"accountId": "acct-alice", "displayName": "Alice"},
		"items": []any{
			map[string]any{"field": "status", "fromString": "To Do", "toString": "Done"},
			map[string]any{"field": "assignee", "fromString": "", "toString": "Bob"},
			map[string]any{"field": "summary", "fromString": "old", "toString": "new"},
		},
	}

	got, err := collectorjira.EventsFromChangelogEntry(testIssue(), entry)

	require.NoError(t, err)
	require.Len(t, got, 2, "status and summary tracked, assignee skipped")
	ids := []string{got[0].ExternalID, got[1].ExternalID}
	assert.ElementsMatch(t, []string{"PROJ-42:status:10007", "PROJ-42:summary:10007"}, ids,
		"the field must be in the ExternalID or co-occurring changes collide")
}

func TestEventsFromChangelogEntry_SelfAuthoredIsTaggedNotDropped(t *testing.T) {
	entry := map[string]any{
		"id":      "10002",
		"created": "2026-08-20T14:30:00.000+0000",
		"author":  map[string]any{"accountId": "acct-unjira", "displayName": "unjira"},
		"items": []any{
			map[string]any{"field": "status", "fromString": "To Do", "toString": "Done"},
		},
	}

	got, err := collectorjira.EventsFromChangelogEntry(testIssue(), entry)

	require.NoError(t, err)
	require.Len(t, got, 1,
		"our own changes are evidence a drift was closed; dropping them makes the loop propose it again")
	assert.Equal(t, true, got[0].Artifacts["authored_by_unjira"])
}

func TestEventsFromChangelogEntry_MalformedEntryErrorsNaming(t *testing.T) {
	tests := []struct {
		name    string
		entry   map[string]any
		wantMsg string
	}{
		{
			name:    "missing id",
			entry:   map[string]any{"created": "2026-08-20T14:30:00.000+0000", "items": []any{}},
			wantMsg: "id",
		},
		{
			name:    "unparseable created",
			entry:   map[string]any{"id": "1", "created": "yesterday", "items": []any{}},
			wantMsg: "created",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := collectorjira.EventsFromChangelogEntry(testIssue(), tt.entry)

			require.Error(t, err, "a shape we cannot read must be loud, not skipped")
			assert.Contains(t, err.Error(), tt.wantMsg)
			assert.Contains(t, err.Error(), "PROJ-42", "the error must name the issue")
		})
	}
}

func TestEventFromComment_ShapeAndFullText(t *testing.T) {
	long := strings.Repeat("x", 3000)
	comment := map[string]any{
		"id":      "9001",
		"created": "2026-08-20T15:00:00.000+0000",
		"body":    long,
		"author":  map[string]any{"accountId": "acct-alice", "displayName": "Alice"},
	}

	got, err := collectorjira.EventFromComment(testIssue(), comment)

	require.NoError(t, err)
	assert.Equal(t, "PROJ-42:comment:9001", got.ExternalID)
	assert.Equal(t, "Alice", got.Actor)
	assert.Contains(t, got.Summary, long,
		"comment text is carried in full; truncation would discard the richest statement of intent")
	assert.Equal(t, false, got.Artifacts["authored_by_unjira"])
	assert.NotContains(t, got.Artifacts, "field", "comments are not a changelog field")
}

func TestEventFromComment_SelfAuthoredIsTagged(t *testing.T) {
	comment := map[string]any{
		"id":      "9002",
		"created": "2026-08-20T15:00:00.000+0000",
		"body":    "Narrated by unjira.",
		"author":  map[string]any{"accountId": "acct-unjira", "displayName": "unjira"},
	}

	got, err := collectorjira.EventFromComment(testIssue(), comment)

	require.NoError(t, err)
	assert.Equal(t, true, got.Artifacts["authored_by_unjira"],
		"this tag is what stops the reconciler proposing a comment we already posted")
}
