package correlator_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/correlator"
)

func TestParseRole_AcceptsTheClosedSet(t *testing.T) {
	for _, want := range []correlator.Role{
		correlator.RolePrimary, correlator.RoleSameWork, correlator.RoleMentioned,
	} {
		got, err := correlator.ParseRoleForTest(string(want))

		require.NoError(t, err)
		assert.Equal(t, want, got)
	}
}

func TestParseRole_RejectsAnythingElse(t *testing.T) {
	// An open vocabulary would rot: if the model emits "relates", "related-to",
	// "blocked-by" and "blocker" as synonyms, every consumer must pattern-match
	// a set that grows without bound. Rejecting loudly is the alternative.
	tests := []string{"", "gating", "caused_by", "discovered_while", "relates", "PRIMARY", "primary "}

	for _, in := range tests {
		t.Run(fmt.Sprintf("%q", in), func(t *testing.T) {
			_, err := correlator.ParseRoleForTest(in)

			require.Error(t, err, "an unrecognized role must be a parse error, never coerced")
			assert.Contains(t, err.Error(), "role")
		})
	}
}

func TestParseMatchResponse_HappyPath(t *testing.T) {
	raw := `[
	  {"issue_key":"PAAS-1","role":"primary","confidence":0.92,"rationale":"the branch names it and the description matches"},
	  {"issue_key":"SUMO-2","role":"same_work","confidence":0.80,"rationale":"change-management record for the same deploy"},
	  {"issue_key":"NOPE-3","role":"mentioned","confidence":0.10,"rationale":"cited as prior art only"}
	]`

	got, err := correlator.ParseMatchResponseForTest(raw)

	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, "PAAS-1", got[0].IssueKey)
	assert.Equal(t, correlator.RolePrimary, got[0].Role)
	assert.InDelta(t, 0.92, got[0].Confidence, 1e-9)
	assert.Contains(t, got[0].Rationale, "branch")
	assert.Equal(t, correlator.RoleSameWork, got[1].Role)
}

func TestParseMatchResponse_StripsMarkdownFence(t *testing.T) {
	// Models add fences despite being told not to. A previous slice hit exactly
	// this against a real model, so every parser in this package runs raw output
	// through stripJSONFence first.
	fenced := "```json\n[{\"issue_key\":\"PAAS-1\",\"role\":\"primary\",\"confidence\":0.9,\"rationale\":\"x\"}]\n```"

	got, err := correlator.ParseMatchResponseForTest(fenced)

	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "PAAS-1", got[0].IssueKey)
}

func TestParseMatchResponse_ErrorCases(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantMsg string
	}{
		{name: "not json", raw: "I think it's PAAS-1", wantMsg: "parsing"},
		{name: "not an array", raw: `{"issue_key":"PAAS-1"}`, wantMsg: "parsing"},
		{
			name:    "unknown role",
			raw:     `[{"issue_key":"PAAS-1","role":"gating","confidence":0.9,"rationale":"x"}]`,
			wantMsg: "role",
		},
		{
			name:    "missing issue key",
			raw:     `[{"role":"primary","confidence":0.9,"rationale":"x"}]`,
			wantMsg: "issue_key",
		},
		{
			name:    "two primaries",
			raw:     `[{"issue_key":"A-1","role":"primary","confidence":0.9,"rationale":"x"},{"issue_key":"B-2","role":"primary","confidence":0.8,"rationale":"y"}]`,
			wantMsg: "primary",
		},
		{
			name:    "confidence out of range",
			raw:     `[{"issue_key":"A-1","role":"primary","confidence":1.5,"rationale":"x"}]`,
			wantMsg: "confidence",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := correlator.ParseMatchResponseForTest(tt.raw)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantMsg)
		})
	}
}
