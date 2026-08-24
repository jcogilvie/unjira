package envfile_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/envfile"
)

// writeRepoFixture builds a fake repo root (a go.mod marker plus a .env) and
// returns the root and a nested subdirectory beneath it, mirroring the real
// shape: go test's CWD lands in a package directory, not the repo root.
func writeRepoFixture(t *testing.T, envBody string) (root, nested string) {
	t.Helper()

	root = t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "go.mod"), []byte("module fixture\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".env"), []byte(envBody), 0o600))

	nested = filepath.Join(root, "internal", "somepkg")
	require.NoError(t, os.MkdirAll(nested, 0o750))

	return root, nested
}

func TestLoadFrom_SetsVariableFromEnvFile(t *testing.T) {
	_, nested := writeRepoFixture(t, "UNJIRA_ENVFILE_TEST_ONLY_A=from-file\n")
	os.Unsetenv("UNJIRA_ENVFILE_TEST_ONLY_A")
	t.Cleanup(func() { os.Unsetenv("UNJIRA_ENVFILE_TEST_ONLY_A") })

	err := envfile.LoadFrom(nested)

	require.NoError(t, err)
	assert.Equal(t, "from-file", os.Getenv("UNJIRA_ENVFILE_TEST_ONLY_A"))
}

func TestLoadFrom_RealEnvVarWinsOverFile(t *testing.T) {
	_, nested := writeRepoFixture(t, "UNJIRA_ENVFILE_TEST_ONLY_B=from-file\n")
	t.Setenv("UNJIRA_ENVFILE_TEST_ONLY_B", "from-environment")

	err := envfile.LoadFrom(nested)

	require.NoError(t, err)
	assert.Equal(t, "from-environment", os.Getenv("UNJIRA_ENVFILE_TEST_ONLY_B"),
		"an already-set env var must never be overwritten by .env")
}

func TestLoadFrom_WalksUpToRepoRootEnv(t *testing.T) {
	_, nested := writeRepoFixture(t, "UNJIRA_ENVFILE_TEST_ONLY_C=from-root-env\n")
	os.Unsetenv("UNJIRA_ENVFILE_TEST_ONLY_C")
	t.Cleanup(func() { os.Unsetenv("UNJIRA_ENVFILE_TEST_ONLY_C") })

	// nested has no .env of its own; only the repo root (marked by go.mod)
	// does. LoadFrom must walk up to find it.
	err := envfile.LoadFrom(nested)

	require.NoError(t, err)
	assert.Equal(t, "from-root-env", os.Getenv("UNJIRA_ENVFILE_TEST_ONLY_C"),
		"LoadFrom must walk up from startDir to the go.mod root to find .env")
}

func TestLoadFrom_MissingEnvFileIsNoOp(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "go.mod"), []byte("module fixture\n"), 0o600))
	nested := filepath.Join(root, "internal", "somepkg")
	require.NoError(t, os.MkdirAll(nested, 0o750))

	err := envfile.LoadFrom(nested)

	assert.NoError(t, err, "a repo with no .env at all must be a silent no-op")
}

func TestLoadFrom_NoGoModAnywhereIsNoOp(t *testing.T) {
	// A directory tree with no go.mod anywhere up to the filesystem root
	// (e.g. running from /tmp) must not error - it just has nothing to load.
	dir := t.TempDir()
	nested := filepath.Join(dir, "a", "b", "c")
	require.NoError(t, os.MkdirAll(nested, 0o750))

	err := envfile.LoadFrom(nested)

	assert.NoError(t, err, "no go.mod anywhere up the tree must be a no-op, not an error")
}

func TestLoadFrom_MalformedEnvFileErrorsLoudly(t *testing.T) {
	root, nested := writeRepoFixture(t, "")
	// Overwrite with genuinely malformed content: an unterminated quoted
	// value, which godotenv's parser rejects.
	require.NoError(t, os.WriteFile(filepath.Join(root, ".env"), []byte(`UNJIRA_ENVFILE_TEST_ONLY_D="unterminated`), 0o600))

	err := envfile.LoadFrom(nested)

	require.Error(t, err, "a malformed .env must error loudly, not be silently ignored")
}

func TestLoad_UsesCurrentWorkingDirectory(t *testing.T) {
	root, nested := writeRepoFixture(t, "UNJIRA_ENVFILE_TEST_ONLY_E=from-file-via-load\n")
	os.Unsetenv("UNJIRA_ENVFILE_TEST_ONLY_E")
	t.Cleanup(func() { os.Unsetenv("UNJIRA_ENVFILE_TEST_ONLY_E") })

	t.Chdir(nested)

	err := envfile.Load()

	require.NoError(t, err)
	assert.Equal(t, "from-file-via-load", os.Getenv("UNJIRA_ENVFILE_TEST_ONLY_E"))

	// avoid the unused root identifier - root is used only to assert the
	// fixture layout is as expected.
	_, statErr := os.Stat(filepath.Join(root, ".env"))
	require.NoError(t, statErr)
}
