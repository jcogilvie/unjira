package events_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/jcogilvie/unjira/internal/events"
)

var eventTime = time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)

// TestSetTicketKeys_RoundTripsInOrder is the accessor pair's basic contract:
// what SetTicketKeys writes, TicketKeysOf must read back, in the same order.
func TestSetTicketKeys_RoundTripsInOrder(t *testing.T) {
	e := events.NewEvent("claude_code", "s1:1", eventTime, "session summary")

	events.SetTicketKeys(&e, []string{"PROJ-1", "PROJ-2"})

	assert.Equal(t, []string{"PROJ-1", "PROJ-2"}, events.TicketKeysOf(e))
}

// TestSetTicketKeys_StoresAnySliceNotStringSlice: the map value must survive a
// JSON round trip through the store, and json.Unmarshal into map[string]any
// always produces []interface{} for a JSON array. A []string value here would
// look right in memory and then silently stop matching after one store
// round-trip, since a fresh read would never produce a []string.
func TestSetTicketKeys_StoresAnySliceNotStringSlice(t *testing.T) {
	e := events.NewEvent("claude_code", "s1:1", eventTime, "session summary")

	events.SetTicketKeys(&e, []string{"PROJ-1"})

	raw, ok := e.Artifacts[events.ArtifactTicketKeys].([]any)
	assert.True(t, ok, "must be stored as []any, matching what a JSON round trip through the store produces")
	assert.Equal(t, []any{"PROJ-1"}, raw)
}

// TestTicketKeysOf_AbsentArtifactIsNilNotError: no keys is the common case for
// a Jira-sourced event, not a malformed one.
func TestTicketKeysOf_AbsentArtifactIsNilNotError(t *testing.T) {
	e := events.NewEvent("jira", "PROJ-1:status:1", eventTime, "status change")

	assert.Nil(t, events.TicketKeysOf(e))
}

// TestTicketKeysOf_ToleratesWrongTypeWithoutPanicking: Artifacts survives a
// JSON round trip and can be written by any future collector, a migration, or
// a hand-seeded row. A reader whose only protection is its writer is
// unprotected the moment something else writes — the same reasoning
// StatusChangeOf's own tests establish for this package.
func TestTicketKeysOf_ToleratesWrongTypeWithoutPanicking(t *testing.T) {
	e := events.NewEvent("claude_code", "s1:1", eventTime, "session summary")
	e.Artifacts[events.ArtifactTicketKeys] = "not a slice"

	assert.Nil(t, events.TicketKeysOf(e))
}

// TestTicketKeysOf_DropsMalformedElements: a corrupt or partially-written
// artifact degrades to fewer candidates rather than failing the whole read —
// matching gatherCandidates' existing tolerance, which this accessor now
// backs.
func TestTicketKeysOf_DropsMalformedElements(t *testing.T) {
	e := events.NewEvent("claude_code", "s1:1", eventTime, "session summary")
	e.Artifacts[events.ArtifactTicketKeys] = []any{"PROJ-1", 42, "", "PROJ-2", nil}

	assert.Equal(t, []string{"PROJ-1", "PROJ-2"}, events.TicketKeysOf(e))
}
