package claudecode

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRemoteReader_ReadsEachCwdOncePerPass: dozens of segments share one working
// directory, and the read must not repeat per segment. Removing the repository after the
// first read proves the second answer came from the pass's cache.
func TestRemoteReader_ReadsEachCwdOncePerPass(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	repo, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	_, err = repo.CreateRemote(&gitconfig.RemoteConfig{Name: "origin", URLs: []string{"https://github.com/o/r"}})
	require.NoError(t, err)

	reader := newRemoteReader()
	first := reader.read(dir)
	require.NoError(t, os.RemoveAll(dir))
	second := reader.read(dir)

	assert.Equal(t, []string{"github.com/o/r"}, first.repos)
	assert.Equal(t, first, second)

	assert.Equal(t, cwdGoneReason, newRemoteReader().read(dir).omitted, "a fresh pass reads the directory again")
}
