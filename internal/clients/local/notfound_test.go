package local_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// TestTracker_GetIssue_MissingKeyIsNotFoundNotTransport: the backend classifies its own
// error, so matching drops a missing local key as unresolved instead of failing the
// narrative every pass.
func TestTracker_GetIssue_MissingKeyIsNotFoundNotTransport(t *testing.T) {
	tr := openTracker(t)

	_, err := tr.GetIssue("PROJ-404")
	require.Error(t, err)
	require.ErrorIs(t, err, tasktracker.ErrNotFound)
	require.ErrorIs(t, err, store.ErrLocalIssueNotFound, "the store's own error stays in the chain")
	assert.False(t, tasktracker.IsTransportError(err))
}
