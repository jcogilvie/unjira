package local_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/clients/local"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

func openTracker(t *testing.T) *local.Tracker {
	t.Helper()

	_, tr := openTrackerWithStore(t)
	return tr
}

func openTrackerWithStore(t *testing.T) (*store.Store, *local.Tracker) {
	t.Helper()

	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	return s, local.New(s)
}

func TestTracker_CreateThenGetIssue_RoundTrips(t *testing.T) {
	tr := openTracker(t)

	key, err := tr.CreateIssue("PROJ", "Do the thing", "Task", "a description", []string{"bug"})
	require.NoError(t, err)
	assert.Equal(t, "PROJ-1", key)

	issue, err := tr.GetIssue(key)
	require.NoError(t, err)
	assert.Equal(t, tasktracker.Issue{
		Key:            key,
		Summary:        "Do the thing",
		StatusCategory: tasktracker.StatusTodo,
		StatusName:     "todo",
		Labels:         []string{"bug"},
	}, issue)
}

func TestTracker_GetIssue_MissingReturnsError(t *testing.T) {
	tr := openTracker(t)

	_, err := tr.GetIssue("PROJ-1")

	require.Error(t, err)
}

func TestTracker_SetStatus_UpdatesCategory(t *testing.T) {
	tr := openTracker(t)
	key, err := tr.CreateIssue("PROJ", "Do the thing", "Task", "", nil)
	require.NoError(t, err)

	err = tr.SetStatus(key, tasktracker.StatusInProgress)
	require.NoError(t, err)

	issue, err := tr.GetIssue(key)
	require.NoError(t, err)
	assert.Equal(t, tasktracker.StatusInProgress, issue.StatusCategory)
}

func TestTracker_SetStatus_MissingReturnsError(t *testing.T) {
	tr := openTracker(t)

	err := tr.SetStatus("PROJ-1", tasktracker.StatusDone)

	require.Error(t, err)
}

func TestTracker_AddComment_PersistsAgainstIssue(t *testing.T) {
	s, tr := openTrackerWithStore(t)
	key, err := tr.CreateIssue("PROJ", "Do the thing", "Task", "", nil)
	require.NoError(t, err)

	err = tr.AddComment(key, "investigation notes")
	require.NoError(t, err)

	comments, err := s.LocalIssueComments(key)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	assert.Equal(t, "investigation notes", comments[0])
}

func TestTracker_AddComment_MissingReturnsError(t *testing.T) {
	tr := openTracker(t)

	err := tr.AddComment("PROJ-1", "investigation notes")

	require.Error(t, err)
}

func TestTracker_SearchIssues_NoQueryReturnsAll(t *testing.T) {
	tr := openTracker(t)
	_, err := tr.CreateIssue("PROJ", "Fix the bug", "Task", "", nil)
	require.NoError(t, err)
	_, err = tr.CreateIssue("PROJ", "Write the docs", "Task", "", nil)
	require.NoError(t, err)

	issues, err := tr.SearchIssues("", 10)
	require.NoError(t, err)
	assert.Len(t, issues, 2)
}

func TestTracker_SearchIssues_SubstringMatchesSubset(t *testing.T) {
	tr := openTracker(t)
	_, err := tr.CreateIssue("PROJ", "Fix the bug", "Task", "", nil)
	require.NoError(t, err)
	_, err = tr.CreateIssue("PROJ", "Write the docs", "Task", "", nil)
	require.NoError(t, err)

	issues, err := tr.SearchIssues("bug", 10)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, "Fix the bug", issues[0].Summary)
}
