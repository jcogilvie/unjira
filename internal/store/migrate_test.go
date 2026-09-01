package store_test

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"

	"github.com/jcogilvie/unjira/internal/store"
)

// TestMigrate_RenamesLocalIssueStatusCategory is the only proof the rename
// works on a database that already exists.
//
// The schema is `CREATE TABLE IF NOT EXISTS`, so it is a no-op against an old
// database — renaming the column in that string alone would leave every existing
// dev database with `status_category` while every query asked for `status`. That
// fails at runtime, on the first local-backend call, not at build time.
//
// The old shape is constructed by hand rather than by checking out old code,
// which is the only way to test a migration honestly.
func TestMigrate_RenamesLocalIssueStatusCategory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")

	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)", path))
	require.NoError(t, err)

	// The pre-rename shape, with a row in it: a migration that drops data is
	// worse than one that never ran.
	_, err = db.Exec(`
		CREATE TABLE local_issues (
			key             TEXT PRIMARY KEY,
			project         TEXT NOT NULL,
			summary         TEXT NOT NULL,
			description     TEXT,
			issue_type      TEXT NOT NULL,
			status_category TEXT NOT NULL DEFAULT 'todo',
			labels          TEXT NOT NULL DEFAULT '[]',
			created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
			updated_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
		)`)
	require.NoError(t, err)

	_, err = db.Exec(
		`INSERT INTO local_issues (key, project, summary, issue_type, status_category)
		 VALUES ('OLD-1', 'OLD', 'a pre-existing issue', 'Task', 'in_progress')`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	// Open is what every command calls, so the migration must run from there
	// rather than needing a separate step somebody could forget.
	s, err := store.Open(path)
	require.NoError(t, err, "an existing database must still open")
	t.Cleanup(func() { _ = s.Close() })

	// The accessors now name `status`, so this failing means the rename did not
	// happen.
	issue, err := s.GetLocalIssue("OLD-1")
	require.NoError(t, err)
	assert.Equal(t, "a pre-existing issue", issue.Summary, "the row survived")
	assert.Equal(t, "in_progress", issue.Status,
		"the recorded value is preserved verbatim: it is a usable status name for a "+
			"backend that does not validate them, and inventing a display string "+
			"would rewrite state no caller ever set")

	// Writing through the renamed column works, which the read alone does not
	// prove (a rename that left an old index or trigger behind can read fine
	// and fail on write).
	require.NoError(t, s.SetLocalIssueStatus("OLD-1", "In Review"))
	issue, err = s.GetLocalIssue("OLD-1")
	require.NoError(t, err)
	assert.Equal(t, "In Review", issue.Status)
}

// TestMigrate_IsIdempotent: Open runs on every single command invocation, so a
// migration that only survives its first run would break the second one.
func TestMigrate_IsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "repeat.db")

	for pass := 1; pass <= 3; pass++ {
		s, err := store.Open(path)
		require.NoError(t, err, "pass %d must open cleanly", pass)
		require.NoError(t, s.Close())
	}
}
