package tasktracker_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// anonymousWriter writes but cannot say who it writes as.
type anonymousWriter struct{}

func (anonymousWriter) AddComment(string, string) error { return nil }
func (anonymousWriter) SetStatus(string, string) error  { return nil }
func (anonymousWriter) CreateIssue(string, string, string, string, []string) (string, error) {
	return "", nil
}

func anonymousRoute(writable ...string) tasktracker.Route {
	b := &namedBackend{name: "anon"}

	return tasktracker.Route{
		Tracker: "anon", Scopes: []string{"PROJ"}, WritableScopes: writable,
		OpenReader: func() (tasktracker.TaskReader, error) { return b, nil },
		OpenWriter: func() (tasktracker.TaskWriter, error) { return anonymousWriter{}, nil },
	}
}

// TestCheckWriters_RefusesAWritableTrackerThatCannotReportItsIdentity: a writer whose
// backend cannot say who unjira is would have its own writes collected back as new work,
// so a writable tracker on one is refused at startup.
func TestCheckWriters_RefusesAWritableTrackerThatCannotReportItsIdentity(t *testing.T) {
	err := tasktracker.NewResolver(anonymousRoute("PROJ")).CheckWriters()

	require.Error(t, err)
	assert.Contains(t, err.Error(), `"anon"`)
	assert.Contains(t, err.Error(), "identity")
}

// TestCheckWriters_AReadOnlyTrackerNeedsNoIdentity: it has no writes to echo.
func TestCheckWriters_AReadOnlyTrackerNeedsNoIdentity(t *testing.T) {
	require.NoError(t, tasktracker.NewResolver(anonymousRoute()).CheckWriters())
}

func TestCheckWriters_AcceptsAWriterThatReportsItsIdentity(t *testing.T) {
	r, _, _ := newResolver(t)

	require.NoError(t, r.CheckWriters())
}

// TestResolve_NeverHandsOutAnAnonymousWriter: the same requirement holds without the
// startup check, so a command that skips it still cannot write anonymously.
func TestResolve_NeverHandsOutAnAnonymousWriter(t *testing.T) {
	_, err := tasktracker.NewResolver(anonymousRoute("PROJ")).Resolve("PROJ-1")

	require.ErrorContains(t, err, "identity")
}
