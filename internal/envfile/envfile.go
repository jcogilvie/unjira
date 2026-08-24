// Package envfile loads unjira's gitignored .env file into the process
// environment, without ever letting it override a variable the environment
// already set. See internal/config's doc comment: credentials are read from
// the environment, with .env only a convenience for local/dev runs — real
// env vars must always win, or an operator overriding one credential for a
// single command (`UNJIRA_JIRA_TOKEN=x go run ...`) would be silently
// undone by the file.
package envfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/joho/godotenv"
)

// envFileName is the file Load looks for at the repository root.
const envFileName = ".env"

// goModFileName marks the repository root: the directory containing it is
// where Load expects to find .env. go.mod is used rather than, say, .git,
// because it's guaranteed to exist for this module and needs no additional
// assumption about the checkout (e.g. a shallow clone, a vendored copy
// without .git).
const goModFileName = "go.mod"

// Load finds the repository's .env (by walking up from the current working
// directory to the nearest ancestor containing go.mod) and sets any variable
// it declares that is not already present in the environment.
//
// Load is a thin wrapper over LoadFrom(os.Getwd()) — see LoadFrom for the
// walk-up and precedence behavior, both of which are unit-tested against a
// starting directory directly rather than by mutating the test process's
// CWD.
func Load() error {
	wd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("envfile: getting working directory: %w", err)
	}

	return LoadFrom(wd)
}

// LoadFrom walks up from startDir looking for a directory containing
// go.mod, and if found, loads that directory's .env (if present) into the
// process environment.
//
// Three cases are all valid, non-error outcomes: no go.mod anywhere up the
// tree (e.g. running from /tmp), a repo root with no .env (the common case:
// CI, production, a fresh clone), and a .env that declares no new
// variables. Only a .env that exists but fails to parse is an error — see
// the package doc comment and docs/design-notes.md's "error loudly, don't
// silently drop data" principle: a parse failure left ignored would surface
// later as a confusing "credential not configured" error instead of the
// actual misconfigured file.
//
// This must walk up rather than look only in startDir because
// `go test ./internal/live/` (among other packages) runs with the test
// binary's CWD set to the package directory, not the repository root — a
// non-walking loader would silently find nothing there and never load the
// root .env at all.
func LoadFrom(startDir string) error {
	root, ok, err := findRepoRoot(startDir)
	if err != nil {
		return fmt.Errorf("envfile: locating repository root from %s: %w", startDir, err)
	}
	if !ok {
		return nil
	}

	envPath := filepath.Join(root, envFileName)

	fileVars, err := godotenv.Read(envPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}

		return fmt.Errorf("envfile: parsing %s: %w", envPath, err)
	}

	for key, value := range fileVars {
		if _, present := os.LookupEnv(key); present {
			continue
		}

		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("envfile: setting %s from %s: %w", key, envPath, err)
		}
	}

	return nil
}

// findRepoRoot walks up from startDir looking for a directory containing
// go.mod. ok is false (with a nil error) when no ancestor has one — that is
// a valid "not in this repo" outcome, not a failure.
func findRepoRoot(startDir string) (root string, ok bool, err error) {
	dir, err := filepath.Abs(startDir)
	if err != nil {
		return "", false, fmt.Errorf("resolving absolute path of %s: %w", startDir, err)
	}

	for {
		if _, statErr := os.Stat(filepath.Join(dir, goModFileName)); statErr == nil {
			return dir, true, nil
		} else if !errors.Is(statErr, fs.ErrNotExist) {
			return "", false, fmt.Errorf("checking for %s in %s: %w", goModFileName, dir, statErr)
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			// Reached the filesystem root without finding go.mod.
			return "", false, nil
		}

		dir = parent
	}
}
