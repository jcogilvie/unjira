// Package correlator_test exercises Cluster against a hand-written fake
// llmClient, not a real HTTP server — Cluster's own logic (window/adjacency
// filtering, prompt construction, response parsing, split/merge) is what's
// under test here, not any wire protocol. Compare
// internal/workflow/workflow_test.go's fakeMiner, which decouples
// MineProject from a concrete *jira.Client the same way.
package correlator_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/correlator"
)

// fakeLLM satisfies correlator's llmClient interface without making any
// real call. responses is consumed in call order; a test that only cares
// about one canned response can set a single-element slice.
type fakeLLM struct {
	responses []string
	prompts   []string // captured user prompts, in call order, for assertions
	err       error
}

func (f *fakeLLM) Complete(_ context.Context, _ string, userPrompt string) (string, error) {
	f.prompts = append(f.prompts, userPrompt)
	if f.err != nil {
		return "", f.err
	}

	idx := len(f.prompts) - 1
	if idx >= len(f.responses) {
		idx = len(f.responses) - 1
	}

	return f.responses[idx], nil
}

func TestCluster_EmptyEventsReturnsEmptyResult(t *testing.T) {
	llm := &fakeLLM{responses: []string{"[]"}}

	results, err := correlator.Cluster(t.Context(), nil, nil, llm, correlator.TimeRange{}, 128000)

	require.NoError(t, err)
	require.Empty(t, results)
}
