package jira

import (
	"testing"

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

func TestNormalizeIssue_NonStringDescriptionIsIgnored(t *testing.T) {
	// Jira Cloud's v3 API can return an Atlassian Document Format object here
	// rather than a string. Coercing that to text is out of scope; the type
	// assertion must simply not panic and must leave Description empty.
	raw := map[string]any{
		"key": "PROJ-42",
		"fields": map[string]any{
			"summary":     "ADF description",
			"description": map[string]any{"type": "doc", "version": 1},
		},
	}

	got := normalizeIssue(raw)

	assert.Empty(t, got.Description, "an ADF object must not be stringified into garbage")
}
