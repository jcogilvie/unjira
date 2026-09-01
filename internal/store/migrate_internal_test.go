package store

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHasColumn distinguishes absence from a failed probe. Treating an error as
// "column not present" would silently skip a migration and leave the database in
// a shape the code no longer expects — a failure that surfaces far from its
// cause.
//
// In-package (not store_test) because hasColumn is unexported and takes the
// Store's own *sql.DB.
func TestHasColumn(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "cols.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	has, err := hasColumn(s.db, "local_issues", "status")
	require.NoError(t, err)
	assert.True(t, has, "the current schema has this column")

	has, err = hasColumn(s.db, "local_issues", "status_category")
	require.NoError(t, err)
	assert.False(t, has, "and no longer has the old one")

	// A table that does not exist yields no rows rather than an error — that is
	// pragma_table_info's own behavior, and asserting it documents that "false"
	// here can mean either "no such column" or "no such table".
	has, err = hasColumn(s.db, "no_such_table", "whatever")
	require.NoError(t, err)
	assert.False(t, has)
}
