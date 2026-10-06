package tasktracker_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// TestLocate_RoutesWithoutOpeningABackend: a startup report checks every stored key,
// so locating one must not build a client.
func TestLocate_RoutesWithoutOpeningABackend(t *testing.T) {
	opened := 0
	open := func() (tasktracker.TaskReader, error) {
		opened++

		return nil, errors.New("must not be opened")
	}
	r := tasktracker.NewResolver(
		tasktracker.Route{
			Tracker: "work", Scopes: []string{"PAAS", "DEVSBX"}, WritableScopes: []string{"DEVSBX"},
			OpenReader: open,
			OpenWriter: func() (tasktracker.TaskWriter, error) { return nil, errors.New("must not be opened") },
		},
		tasktracker.Route{Tracker: "upstream", Scopes: []string{"o/*"}, OpenReader: open},
	).WithReadFallback("work")

	writable, err := r.Locate("DEVSBX-1")
	require.NoError(t, err)
	assert.Equal(t, tasktracker.Location{Tracker: "work", Scope: "DEVSBX", Writable: true}, writable)

	readOnly, err := r.Locate("O/Repo#4")
	require.NoError(t, err)
	assert.Equal(t, tasktracker.Location{Tracker: "upstream", Scope: "o/repo"}, readOnly)

	fallback, err := r.Locate("SUMO-1")
	require.NoError(t, err)
	assert.Equal(t, tasktracker.Location{Tracker: "work", Scope: "SUMO", Fallback: true}, fallback)

	_, err = r.Locate("x/y#1")
	_, unrouted := errors.AsType[*tasktracker.UnroutedError](err)
	assert.True(t, unrouted)

	assert.Zero(t, opened)
}
