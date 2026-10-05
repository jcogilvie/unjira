package claudecode

// shell_test.go pins the shell reading every collector recognizer is built on, against the
// shapes measured in real transcripts. A substring match on "gh pr create" fired on a plan
// document being appended through a heredoc, a python heredoc that greps transcripts for
// the phrase, and echo/grep strings, so an event asserted "a pull request was opened" for
// a PR nobody opened.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGHPRCreateInvocations_RecognizesRealInvocations(t *testing.T) {
	for _, tc := range []struct {
		name, command string
	}{
		{"plain", `gh pr create --title "x" --body "y"`},
		{"after cd", `cd /Users/j/workspace/unjira && gh pr create --base main --head f`},
		{"newline after cd", "cd /Users/j/workspace/unjira\ngh pr create --fill"},
		{"rtk wrapper", `rtk gh pr create --fill`},
		{"absolute path", `/opt/homebrew/bin/gh pr create --fill`},
		{"env assignment", `GH_PROMPT_DISABLED=1 gh pr create --fill`},
		{"command substitution", `URL=$(gh pr create --fill) && echo "$URL"`},
		{"after push", `git push -u origin b 2>&1 | tail -5 && gh pr create -R o/r -H b`},
		{"if condition", `if gh pr create --fill; then echo ok; fi`},
		{"heredoc body arg", "gh pr create --title \"t\" --body \"$(cat <<'EOF'\n## Summary\n\ngh pr create is mentioned here\nEOF\n)\""},
		{
			"for loop (measured: 8 PRs from one call)",
			"for repo in a b c; do\n  cd /w/$repo\n  gh pr create \\\n    --draft \\\n    --title \"PAAS-3691: x\" \\\n    --body-file /tmp/b.md\ndone",
		},
		{
			"shell function (measured: called 4 times)",
			"mk() {\n  git checkout -q -b \"$1\"\n  gh pr create --repo jcogilvie/unjira-sandbox --base main --head \"$1\" --title \"$2\"\n}\nmk a A; mk b B",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Len(t, ghPRCreateInvocations(tc.command), 1, "command: %s", tc.command)
		})
	}
}

func TestGHPRCreateInvocations_IgnoresMentions(t *testing.T) {
	for _, tc := range []struct {
		name, command string
	}{
		{"echo string", `echo "run gh pr create next"`},
		{"grep pattern", `grep -n "gh pr create" internal/collector/claudecode/*.go`},
		{"comment", "# gh pr create --fill\nls"},
		{
			"heredoc appending a plan (measured)",
			"cd /w/u\ncat >> docs/plan.md <<'PLANEOF'\n## Task 7\n\n```sh\ngh pr create --title \"x\"\n```\nPLANEOF",
		},
		{
			"python heredoc grepping transcripts (measured)",
			"cd ~/.claude/projects\npython3 - <<'PY'\nimport re\nWRITE=re.compile(r'git commit|gh pr create')\ngh pr create\nPY",
		},
		{"python -c string", `python3 -c "print('gh pr create')"`},
		{"pr view", `gh pr view 42 --json url`},
		{"pr list piped", `gh pr list | grep create`},
		{"git commit message", `git commit -m "docs: how to gh pr create"`},
		{"unterminated quote", `echo "gh pr create`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Empty(t, ghPRCreateInvocations(tc.command), "command: %s", tc.command)
		})
	}
}

func TestGHPRCreateInvocations_CountsEachStaticInvocation(t *testing.T) {
	got := ghPRCreateInvocations(`gh pr create -R o/a --fill && gh pr create -R o/b --fill`)

	assert.Len(t, got, 2)
}

func TestParsePRCreateArgs(t *testing.T) {
	for _, tc := range []struct {
		name, command    string
		repo, head, titl string
		wantNote         bool
	}{
		{"short flags", `gh pr create -R o/r -H feat -t "PROJ-1: x"`, "o/r", "feat", "PROJ-1: x", false},
		{"long equals", `gh pr create --repo=o/r --head=feat --title=T`, "o/r", "feat", "T", false},
		{"attached short", `gh pr create -Ro/r -Hfeat`, "o/r", "feat", "", false},
		{"host prefix", `gh pr create --repo github.com/o/r`, "o/r", "", "", false},
		{"url", `gh pr create --repo https://github.com/o/r.git`, "o/r", "", "", false},
		{"fork head (measured)", `gh pr create --repo crossplane/cli --head jcogilvie:jco/render-engine`, "crossplane/cli", "jco/render-engine", "", false},
		{"no flags", `gh pr create --fill`, "", "", "", false},
		{"variable repo", `gh pr create --repo "$REPO" --head b`, "", "b", "", true},
		{"variable head", `gh pr create --repo o/r --head "$1"`, "o/r", "", "", true},
		{"malformed repo", `gh pr create --repo justaname`, "", "", "", true},
		{"missing value", `gh pr create --repo`, "", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invs := ghPRCreateInvocations(tc.command)
			require.Len(t, invs, 1)

			got := parsePRCreateArgs(invs[0])

			assert.Equal(t, tc.repo, got.repo)
			assert.Equal(t, tc.head, got.head)
			assert.Equal(t, tc.titl, got.title)
			assert.Equal(t, tc.wantNote, len(got.notes) > 0, "notes: %v", got.notes)
		})
	}
}

// TestSimpleCommands_NeverPanicsOnAnExpansion: expand.Literal with a nil config panics on
// a process substitution, because there is no handler to run it. wordOf calls it only
// for words isLiteral passed, so no expansion reaches it; this pins that for every
// expansion kind, so a change to isLiteral cannot reopen the panic.
func TestSimpleCommands_NeverPanicsOnAnExpansion(t *testing.T) {
	for _, cmd := range []string{
		`diff <(git log) >(cat) && git commit -m x`,
		`echo $((1 + 2)) ${HOME} $(date) ` + "`date`",
		`cd ~/w && git commit -m "~ $x"`,
		`shopt -s extglob; ls !(foo)`,
		`echo {a,b} [ab]* "$@"`,
	} {
		assert.NotPanics(t, func() { simpleCommands(cmd) }, "command %q", cmd)
	}
}
