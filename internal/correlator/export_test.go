package correlator

// This file exposes internals to the correlator_test package. It compiles only
// under `go test`, so nothing here widens the package's real API.

// EstimateTokensForTest lets tests size contextWindowTokens budgets from the
// same estimator Cluster uses, instead of hard-coding numbers that silently
// encode the current chars-per-token ratio.
//
// Hard-coded budgets are a trap here: when the ratio changes, every one of
// them breaks at once, and the only way to repair a bare number is to try
// values until the test goes green — which is exactly how a test that pinned
// "this window must bisect" quietly becomes one that pins nothing.
func EstimateTokensForTest(text string) int {
	return estimateTokens(text)
}

// ClusterSystemPromptForTest is the fixed system prompt Cluster sends. Tests
// budgeting for a prompt have to account for it: it is the larger half of a
// small prompt, so sizing against the user half alone is misleading.
func ClusterSystemPromptForTest() string {
	return clusterSystemPrompt
}

// BuildClusterPromptForTest renders the prompt pair Cluster would send for
// these inputs, so a test can budget against the real thing rather than an
// approximation of it.
func BuildClusterPromptForTest(evts []Event, existing []Narrative) (systemPrompt, userPrompt string) {
	return buildClusterPrompt(evts, existing)
}
