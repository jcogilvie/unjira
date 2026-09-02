package jira_test

// status_history_test.go pins the jira collector's declared capability to
// supply status-change history — the general seam
// pipeline.HasEnabledStatusHistorySource type-asserts for (task #174). If
// this regressed to false (or the interface stopped being implemented at
// all), cmd/unjira's startup check would warn about a missing status-event
// source even with the jira collector configured and enabled — the exact
// false-positive that would make operators distrust the warning.

import (
	"testing"

	"github.com/stretchr/testify/assert"

	collectorjira "github.com/jcogilvie/unjira/internal/collector/jira"
	"github.com/jcogilvie/unjira/internal/pipeline"
)

func TestCollector_ImplementsStatusHistorySource(t *testing.T) {
	c := collectorjira.New()

	// The type assertion IS the assertion: StatusHistorySource is a pure
	// marker, so there is no return value to check and no way for the
	// declaration to disagree with itself.
	_, ok := any(c).(pipeline.StatusHistorySource)
	assert.True(t, ok, "the jira collector emits events.SetStatusChange-tagged events "+
		"(see EventsFromChangelogEntry) and must declare that capability")
}
