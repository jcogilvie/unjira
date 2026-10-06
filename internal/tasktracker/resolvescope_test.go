package tasktracker_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// TestResolveScope_NeverUsesTheFallback: a create names a scope, not a key, and a create
// in a scope no tracker owns must be refused — the fallback exists to keep reads of an
// unlisted project working, never to give it a place to write.
func TestResolveScope_NeverUsesTheFallback(t *testing.T) {
	work := &namedBackend{name: "work"}
	r := tasktracker.NewResolver(route("work", work, []string{"DEVSBX"}, []string{"DEVSBX"})).
		WithReadFallback("work")

	_, err := r.ResolveScope("SUMO")

	_, unrouted := errors.AsType[*tasktracker.UnroutedError](err)
	assert.True(t, unrouted, "want UnroutedError, got %v", err)

	res, err := r.ResolveScope("DEVSBX")
	require.NoError(t, err)
	assert.NotNil(t, res.Writer)
}
