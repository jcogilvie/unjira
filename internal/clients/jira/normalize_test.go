package jira

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// normalizeIssue is unexported, so these tests live in-package rather than
// in tracker_test.go (package jira_test).

func TestNormalizeIssue_CarriesDescription(t *testing.T) {
	raw := map[string]any{
		"key": "PROJ-42",
		"fields": map[string]any{
			"summary":     "Fix the flaky correlator test",
			"description": "The tail-compaction test races the clock; see the boundary tiebreaker.",
		},
	}

	got := normalizeIssue(raw)

	assert.Equal(t, "PROJ-42", got.Key)
	assert.Equal(t, "Fix the flaky correlator test", got.Summary)
	assert.Equal(t, "The tail-compaction test races the clock; see the boundary tiebreaker.",
		got.Description,
		"description is the strongest matching signal; dropping it is why matching needs this field")
}

func TestNormalizeIssue_MissingDescriptionIsEmptyNotError(t *testing.T) {
	raw := map[string]any{
		"key":    "PROJ-42",
		"fields": map[string]any{"summary": "No body on this one"},
	}

	got := normalizeIssue(raw)

	assert.Empty(t, got.Description)
	assert.Equal(t, "No body on this one", got.Summary)
}

func TestNormalizeIssue_FlattensAnADFDescription(t *testing.T) {
	// Jira Cloud v3 returns description as an Atlassian Document Format object,
	// verified against the live instance. This test previously asserted the
	// OPPOSITE — that ADF was "out of scope" and Description was left empty — and
	// that assertion was the bug: normalizeIssue feeds Issue.Description straight
	// into the matching prompt (correlator/match.go), so every v3 issue reached the
	// matcher with `description: ""`.
	//
	// The field exists precisely because a one-line summary often cannot
	// distinguish the ticket a narrative implements from one it merely mentions, so
	// silently emptying it degraded the exact comparison it was added for.
	raw := map[string]any{
		"key": "PROJ-42",
		"fields": map[string]any{
			"summary": "ADF description",
			"description": map[string]any{
				"type":    "doc",
				"version": 1,
				"content": []any{
					map[string]any{"type": "paragraph", "content": []any{
						map[string]any{"type": "text", "text": "PR: Sanyaku/platform-vision#1 merged"},
					}},
					map[string]any{"type": "bulletList", "content": []any{
						map[string]any{"type": "listItem", "content": []any{
							map[string]any{"type": "paragraph", "content": []any{
								map[string]any{"type": "text", "text": "nested detail"},
							}},
						}},
					}},
				},
			},
		},
	}

	got := normalizeIssue(raw)

	assert.Contains(t, got.Description, "PR: Sanyaku/platform-vision#1 merged",
		"a cross-reference in the description is often the only deterministic link between a "+
			"narrative and the issue that tracks it")
	assert.Contains(t, got.Description, "nested detail",
		"and text nested in a list must survive: a flattener that read only top-level content "+
			"would pass a flat fixture and still lose most real descriptions")
}

func TestNormalizeIssue_UnrecognizedDescriptionShapeIsEmpty(t *testing.T) {
	// The degradation that must survive: something that is neither a string nor a
	// document yields "", never a Go-syntax rendering. `map[type:doc]` in a prompt
	// would be read by the model as if it were prose.
	raw := map[string]any{
		"key":    "PROJ-42",
		"fields": map[string]any{"summary": "s", "description": 42},
	}

	assert.Empty(t, normalizeIssue(raw).Description)
}

// TestNormalizeIssue_CarriesResolutionAndUpdateDates: matching shows the model when a
// candidate was resolved and last touched, set against the narrative's own window, so
// a ticket closed long before the work began is visibly that. Jira's own timestamp
// format, as GET /issue returns it.
func TestNormalizeIssue_CarriesResolutionAndUpdateDates(t *testing.T) {
	raw := map[string]any{
		"key": "PAAS-2710",
		"fields": map[string]any{
			"summary":        "CVEs: ArgoCD",
			"resolutiondate": "2025-03-04T10:15:30.000+0000",
			"updated":        "2025-03-05T08:00:00.000-0700",
		},
	}

	got := normalizeIssue(raw)

	assert.True(t, got.Resolved.Equal(time.Date(2025, 3, 4, 10, 15, 30, 0, time.UTC)), "resolved: %v", got.Resolved)
	assert.True(t, got.Updated.Equal(time.Date(2025, 3, 5, 15, 0, 0, 0, time.UTC)), "updated: %v", got.Updated)
}

// TestNormalizeIssue_AnUnresolvedIssueHasNoResolutionDate: Jira sends null for an
// unresolved issue. A field it did not send, or sent in a shape this cannot read, is
// unknown, never a guessed date.
func TestNormalizeIssue_AnUnresolvedIssueHasNoResolutionDate(t *testing.T) {
	raw := map[string]any{
		"key": "PAAS-1",
		"fields": map[string]any{
			"summary":        "open work",
			"resolutiondate": nil,
			"updated":        "yesterday",
		},
	}

	got := normalizeIssue(raw)

	assert.True(t, got.Resolved.IsZero())
	assert.True(t, got.Updated.IsZero())
}
