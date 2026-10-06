package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// anonymousBackend is a full TaskTracker that cannot report who unjira is.
type anonymousBackend struct{}

func (anonymousBackend) GetIssue(string) (tasktracker.Issue, error)            { return tasktracker.Issue{}, nil }
func (anonymousBackend) SearchIssues(string, int) ([]tasktracker.Issue, error) { return nil, nil }
func (anonymousBackend) AvailableTransitions(string) ([]tasktracker.Transition, error) {
	return nil, nil
}
func (anonymousBackend) AddComment(string, string) error { return nil }
func (anonymousBackend) SetStatus(string, string) error  { return nil }
func (anonymousBackend) CreateIssue(string, string, string, string, []string) (string, error) {
	return "", nil
}

// TestWatchCmd_RefusesAWritableTrackerWhoseBackendCannotNameUnjira: the startup check
// that a writer can report unjira's identity runs in the real watch wiring, before the
// store is touched — store is nil here, so a check that ran too late would panic.
func TestWatchCmd_RefusesAWritableTrackerWhoseBackendCannotNameUnjira(t *testing.T) {
	app := &appContext{
		config: config.Config{
			LLM:         config.LLMConfig{Model: "test-model", ContextWindowTokens: 128000, BaseURL: "http://localhost:4000/v1"},
			Correlator:  config.CorrelatorConfig{TailSummarizeThresholdTokens: 1_000_000, RecentEventsKept: 20},
			Reconciler:  config.ReconcilerConfig{MaxNarrativesPerPass: 10, MinConfidenceToPropose: 0.5},
			Connections: []config.Connection{{Name: "local", Kind: config.KindLocal}},
			Trackers: []config.Tracker{{
				Name: "dev", Connection: "local", Scopes: []string{"PROJ"}, WritableScopes: []string{"PROJ"},
			}},
		},
		llmAPIKey: "test-key",
		backends:  map[string]tasktracker.TaskTracker{"local": anonymousBackend{}},
	}

	err := (&watchCmd{}).Run(app)

	require.Error(t, err)
	assert.Contains(t, err.Error(), `"dev"`)
	assert.Contains(t, err.Error(), "identity")
}
