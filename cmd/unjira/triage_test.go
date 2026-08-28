package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/triage"
)

func TestParseDecision(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		wantVerb triage.Verb
		wantText string
		wantPos  []int
		wantErr  string
	}{
		{name: "approve", input: "a", wantVerb: triage.VerbApprove},
		{name: "reject with reason", input: "r not worth posting", wantVerb: triage.VerbReject, wantText: "not worth posting"},
		{name: "edit with correction", input: "e name the actual PR", wantVerb: triage.VerbEdit, wantText: "name the actual PR"},
		{name: "skip", input: "k", wantVerb: triage.VerbSkip},
		{name: "quit", input: "q", wantVerb: triage.VerbQuit},
		{name: "merge two positions", input: "m 1 3", wantVerb: triage.VerbMerge, wantPos: []int{1, 3}},
		{name: "split one position", input: "s 4", wantVerb: triage.VerbSplit, wantPos: []int{4}},
		{name: "target an issue key", input: "t PAAS-4010", wantVerb: triage.VerbTarget, wantText: "PAAS-4010"},
		{name: "whitespace tolerated", input: "  a  ", wantVerb: triage.VerbApprove},
		{name: "unknown verb", input: "z", wantErr: "unknown"},
		{name: "empty line", input: "", wantErr: "unknown"},
		{
			name:    "edit with no text is an error, not an empty correction",
			input:   "e",
			wantErr: "requires text",
		},
		{
			name:    "merge with one position is an error",
			input:   "m 1",
			wantErr: "two positions",
		},
		{
			name:    "merge with a non-numeric position",
			input:   "m 1 x",
			wantErr: "position",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseDecision(tc.input)

			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.wantVerb, got.Verb)
			assert.Equal(t, tc.wantText, got.Text)
			assert.Equal(t, tc.wantPos, got.Positions)
		})
	}
}
