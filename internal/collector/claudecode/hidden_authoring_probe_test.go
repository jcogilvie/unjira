package claudecode

// hidden_authoring_probe_test.go is F48's tripwire. The collector reads a script handed to
// another shell, and a Python heredoc, as data (shell.go), so a commit or PR made from
// inside one is invisible. Measured at filing: none in 95 shell scripts or 2,082 Python
// heredocs. This re-measures, by ISO week, over local transcripts, and FAILS if any
// appear, which is the signal to build the recursion F48 describes.
//
//	HIDDEN_AUTHORING_PROBE=1 go test ./internal/collector/claudecode/ \
//	  -run TestHiddenAuthoring_Tripwire -v -count=1
//
// HIDDEN_AUTHORING_ROOT overrides the transcript root (default ~/.claude/projects).
//
// Shell scripts are judged by the collector's own recognizers (simpleCommands,
// isAuthoring, ghPRCreateInvocations), so the tripwire and the collector cannot disagree
// about what counts. Python cannot be read that way. A heuristic stands in: a line that
// starts a statement with a process call and names a commit or PR verb is flagged for a
// person to read.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"mvdan.cc/sh/v3/syntax"
)

var (
	shellPrograms = map[string]bool{"sh": true, "bash": true, "zsh": true}
	// pythonShellOut is a process call that STARTS a statement, alone or assigned. A script
	// that writes documentation quotes the very call it describes, mid-line inside a
	// string; the tripwire's first runs flagged exactly that, twice. Stripping strings
	// instead is not robust: a script whose data holds other source's quotes pairs them
	// wrongly, which was the second false positive.
	pythonShellOut = regexp.MustCompile(
		`^\s*(?:[\w.]+\s*=\s*)?(?:subprocess\.(?:run|call|check_call|check_output|Popen)|os\.(?:system|popen))\(`)
	pythonAuthoring = regexp.MustCompile(
		`git commit|gh pr create|["']git["']\s*,\s*["']commit["']|["']pr["']\s*,\s*["']create["']`)
)

// hiddenScripts returns the scripts a command hands to another shell (a literal `-c`
// argument anywhere in a command's words, so `docker run img sh -c '…'` counts, or a
// heredoc fed to a shell program) and the bodies of heredocs fed to Python.
func hiddenScripts(command string) (shell, python []string) {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
	if err != nil {
		return nil, nil
	}

	syntax.Walk(file, func(node syntax.Node) bool {
		stmt, ok := node.(*syntax.Stmt)
		if !ok {
			return true
		}
		call, ok := stmt.Cmd.(*syntax.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}

		words := make([]shellWord, 0, len(call.Args))
		for _, arg := range call.Args {
			words = append(words, wordOf(command, arg))
		}
		for i := 0; i+2 < len(words); i++ {
			if shellPrograms[path.Base(words[i].text)] && words[i+1].text == "-c" && words[i+2].literal {
				shell = append(shell, words[i+2].text)
			}
		}

		program := dropCommandPrefixes(words)
		if len(program) == 0 {
			return true
		}
		for _, r := range stmt.Redirs {
			if r.Hdoc == nil {
				continue
			}
			body := spanOf(command, r.Hdoc)
			switch base := path.Base(program[0].text); {
			case shellPrograms[base]:
				shell = append(shell, body)
			case strings.HasPrefix(base, "python"):
				python = append(python, body)
			}
		}

		return true
	})

	return shell, python
}

// hiddenAuthoring returns the commit or PR commands a command runs only through a hidden
// script, looking into scripts nested in scripts up to a fixed depth.
func hiddenAuthoring(command string, depth int) []string {
	if depth == 0 {
		return nil
	}

	shell, python := hiddenScripts(command)

	var found []string
	for _, script := range shell {
		for _, c := range simpleCommands(script) {
			if isAuthoring(c.words) {
				found = append(found, "shell: "+c.source)
			}
		}
		found = append(found, hiddenAuthoring(script, depth-1)...)
	}
	for _, body := range python {
		for l := range strings.SplitSeq(body, "\n") {
			if pythonShellOut.MatchString(l) && pythonAuthoring.MatchString(l) {
				found = append(found, "python: "+strings.TrimSpace(l))
			}
		}
	}

	return found
}

func TestHiddenAuthoring_FindsEachHiddenShape(t *testing.T) {
	for _, tc := range []struct{ name, command string }{
		{"bash -c", `bash -c 'cd /w/u && git commit -m "PROJ-1: x"'`},
		{"sh -c in docker", `docker run --rm img sh -c 'gh pr create --fill'`},
		{"heredoc to bash", "bash <<'EOF'\ngit commit -am x\nEOF"},
		{"nested", `bash -c "sh -c 'git commit -m x'"`},
		{"python subprocess", "python3 - <<'PY'\nimport subprocess\nsubprocess.run(['git', 'commit', '-m', 'x'])\nPY"},
		{"python assigned subprocess", "python3 - <<'PY'\nimport subprocess\n    r = subprocess.run([\"gh\", \"pr\", \"create\", \"--fill\"], check=True)\nPY"},
		{"python os.system", "python3 - <<'PY'\nimport os\nos.system('git commit -am x')\nPY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.NotEmpty(t, hiddenAuthoring(tc.command, 3))
		})
	}
}

func TestHiddenAuthoring_IgnoresWhatRunsNothing(t *testing.T) {
	for _, tc := range []struct{ name, command string }{
		{"visible commit", `git commit -m x`},
		{"heredoc to cat", "cat > notes.md <<'EOF'\ngit commit -m x\nEOF"},
		{"bash -c that only reads", `bash -c 'git log --oneline'`},
		// Measured: the tripwire's first run flagged the heredoc that wrote F48's own text,
		// which describes `subprocess.run(["git", "commit", …])` inside a triple-quoted string.
		{"python writing a doc that describes a subprocess commit", "python3 - <<'EOF'\n" +
			"new = '''\nshells out (`subprocess.run([\"git\", \"commit\"])`) and `gh pr create`\n'''\n" +
			"open('f.md', 'w').write(new)\nEOF"},
		{"python with the call and the verb on different lines", "python3 - <<'PY'\nimport subprocess\n" +
			"subprocess.run(['ls'])\nprint('git commit')\nPY"},
		{"python quoting the call in a one-line string", "python3 - <<'PY'\n" +
			"msg = \"shells out: subprocess.run(['git', 'commit'])\"\nprint(msg)\nPY"},
		{"python that names git but runs nothing", "python3 - <<'PY'\nprint('git commit')\nPY"},
		{"python that shells out to something else", "python3 - <<'PY'\nimport subprocess\nsubprocess.run(['ls'])\nPY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Empty(t, hiddenAuthoring(tc.command, 3))
		})
	}
}

type hiddenWeek struct {
	commands, shellScripts, pythonHeredocs int
	found                                  []string
}

func TestHiddenAuthoring_Tripwire(t *testing.T) {
	if os.Getenv("HIDDEN_AUTHORING_PROBE") == "" {
		t.Skip("set HIDDEN_AUTHORING_PROBE=1 to re-measure F48 over local transcripts")
	}

	root := os.Getenv("HIDDEN_AUTHORING_ROOT")
	if root == "" {
		home, err := os.UserHomeDir()
		require.NoError(t, err)
		root = filepath.Join(home, ".claude", "projects")
	}

	weeks := map[string]*hiddenWeek{}
	seen := map[string]bool{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".jsonl") {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()

		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<20), 64<<20)
		for sc.Scan() {
			var line map[string]any
			if json.Unmarshal(sc.Bytes(), &line) != nil {
				continue
			}
			ts, _ := line["timestamp"].(string)
			at, err := time.Parse(time.RFC3339, ts)
			if err != nil {
				continue
			}
			year, week := at.ISOWeek()
			key := fmt.Sprintf("%d-W%02d", year, week)
			for _, command := range toolCommands(line) {
				if seen[command] {
					continue
				}
				seen[command] = true
				if weeks[key] == nil {
					weeks[key] = &hiddenWeek{}
				}
				w := weeks[key]
				w.commands++
				shell, python := hiddenScripts(command)
				w.shellScripts += len(shell)
				w.pythonHeredocs += len(python)
				w.found = append(w.found, hiddenAuthoring(command, 3)...)
			}
		}

		return sc.Err()
	})
	require.NoError(t, err)

	keys := make([]string, 0, len(weeks))
	for k := range weeks {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var found []string
	t.Logf("%-9s %7s %13s %15s %7s", "week", "cmds", "shell-scripts", "python-heredocs", "hidden")
	for _, k := range keys {
		w := weeks[k]
		t.Logf("%-9s %7d %13d %15d %7d", k, w.commands, w.shellScripts, w.pythonHeredocs, len(w.found))
		for _, f := range w.found {
			found = append(found, k+" "+f)
		}
	}

	assert.Empty(t, found, "commits or PRs made from inside a script the collector reads as data (F48): "+
		"build the recursion F48 describes, or file what these are")
}
