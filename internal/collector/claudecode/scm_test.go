package claudecode

// scm_test.go covers finding F20: the collector read only `type == "text"` content
// blocks, so every ticket key a session named in a git or gh command was discarded.
//
// Measured over 28 helm-charts transcripts, across every SCM surface actually in use
// (`git`, `gh`, `rtk git`, and `mcp__github__*`): 10 of 20 sessions carrying any key
// had one in a tool input that the collected prose did NOT contain. Volume is not the
// constraint — 2,155 Bash calls mention git or gh.
//
// The distinction this file is built around, also measured: keys in AUTHORING
// commands (`git commit`, `git checkout -b`, `gh pr create`) are a developer naming
// the ticket for this work — the same explicit human act that makes ProvenanceBranch
// the strongest inferred tier. Keys in READING commands (`git log --grep=PAAS-1234`,
// `gh pr view`) are an agent investigating something it may not be working on. 8 of
// the keys found appeared ONLY in reading commands, so conflating the two would
// manufacture links from work that never touched those tickets.

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/jcogilvie/unjira/internal/events"
)

// toolLine builds a transcript line carrying one tool_use block, the shape the
// collector previously ignored entirely.
func toolLine(ts, branch, name, command string) map[string]any {
	return map[string]any{
		"timestamp": ts,
		"type":      "assistant",
		"gitBranch": branch,
		"message": map[string]any{
			"content": []any{
				map[string]any{
					"type":  "tool_use",
					"name":  name,
					"input": map[string]any{"command": command},
				},
			},
		},
	}
}

func TestSCMKeys_FindsKeysInAuthoringCommands(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
	}{
		{"git commit", `git commit -m "PAAS-3669: add the thing"`},
		{"branch create", `git checkout -b PAAS-3669-add-the-thing`},
		{"git switch -c", `git switch -c PAAS-3669-work`},
		{"gh pr create", `gh pr create --title "PAAS-3669: add the thing"`},
		{"rtk git commit", `rtk git commit -m "PAAS-3669: add the thing"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := scmKeys(toolLine("2026-09-01T10:00:00Z", "feature", "Bash", tc.command))

			assert.Equal(t, []string{"PAAS-3669"}, got)
		})
	}
}

// TestSCMKeys_IgnoresReadingCommands is the measured guard. An agent inspecting a
// ticket it is not working on must not create a candidate for it.
func TestSCMKeys_IgnoresReadingCommands(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
	}{
		{"git log grep", `git log --all --grep=PAAS-1234 -i`},
		{"git show", `git show PAAS-1234`},
		{"gh pr view", `gh pr view 474 --json title | grep PAAS-1234`},
		{"git diff", `git diff main..PAAS-1234-branch`},
		{"git blame", `git blame -L 10,20 file.go # PAAS-1234`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Empty(t, scmKeys(toolLine("2026-09-01T10:00:00Z", "feature", "Bash", tc.command)),
				"a key read during investigation is not the developer naming this work; 8 of the "+
					"keys measured appeared ONLY in reading commands")
		})
	}
}

// TestSCMKeys_AuthoringWinsInAMixedCommand pins the common real shape: a compound
// command that inspects and then commits.
func TestSCMKeys_AuthoringWinsInAMixedCommand(t *testing.T) {
	got := scmKeys(toolLine("2026-09-01T10:00:00Z", "feature", "Bash",
		`git log --oneline -3 && git commit -m "PAAS-3669: the real change"`))

	assert.Equal(t, []string{"PAAS-3669"}, got,
		"the commit in a compound command is authorship, whatever runs beside it")
}

// TestSCMKeys_IgnoresNonSCMCommands keeps unrelated Bash out. A kubectl invocation
// mentioning a ticket in a comment is not an act of naming work.
func TestSCMKeys_IgnoresNonSCMCommands(t *testing.T) {
	assert.Empty(t, scmKeys(toolLine("2026-09-01T10:00:00Z", "f", "Bash",
		`kubectl get pods -n paas # for PAAS-1234`)))
	assert.Empty(t, scmKeys(toolLine("2026-09-01T10:00:00Z", "f", "Read",
		`/path/to/PAAS-1234-notes.md`)))
}

// TestSCMKeys_ReadsTheGitHubMCP covers the surface that is not a shell command at
// all. 13 create_pull_request and 33 pull_request_read calls were measured, so the
// MCP path is real usage, not hypothetical.
func TestSCMKeys_ReadsTheGitHubMCP(t *testing.T) {
	line := map[string]any{
		"timestamp": "2026-09-01T10:00:00Z",
		"type":      "assistant",
		"message": map[string]any{
			"content": []any{
				map[string]any{
					"type": "tool_use",
					"name": "mcp__github__create_pull_request",
					"input": map[string]any{
						"title": "PAAS-3669: add the thing",
						"body":  "closes PAAS-3669",
					},
				},
			},
		},
	}

	assert.Equal(t, []string{"PAAS-3669"}, scmKeys(line))
}

// TestSCMKeys_MCPReadsAreNotAuthorship mirrors the shell distinction on the MCP
// surface: reading a PR is not naming your work.
func TestSCMKeys_MCPReadsAreNotAuthorship(t *testing.T) {
	line := map[string]any{
		"timestamp": "2026-09-01T10:00:00Z",
		"type":      "assistant",
		"message": map[string]any{
			"content": []any{
				map[string]any{
					"type":  "tool_use",
					"name":  "mcp__github__pull_request_read",
					"input": map[string]any{"pullNumber": 474, "notes": "PAAS-1234 maybe related"},
				},
			},
		},
	}

	assert.Empty(t, scmKeys(line))
}

// TestSCMKeys_DelegatesExtractionToTheSharedExtractor pins WHERE the key-shape
// decision lives, which is the lesson from F20's measurement rather than a behaviour
// claim about noise.
//
// That measurement used an ad-hoc `\b[A-Z][A-Z0-9]+-\d+\b` and reported `AC1-3` and
// `L310-326` as tickets. events.ExtractTicketKeys matches those TOO — deliberately:
// its doc comment says placeholder-shaped keys are matched here and told apart
// downstream by CompileLinkExclusionPatterns / exclude_from_linking. So this file must
// not grow its own filter, or there would be two disagreeing answers to "what is a
// key" and the configured exclusions would silently stop applying to one of them.
//
// An earlier version of this test asserted the noise was dropped here. That asserted
// the wrong layer's responsibility, and the shared extractor was right.
func TestSCMKeys_DelegatesExtractionToTheSharedExtractor(t *testing.T) {
	got := scmKeys(toolLine("2026-09-01T10:00:00Z", "f", "Bash",
		`git commit -m "PAAS-3669: fixes L310-326 per AC1-3"`))

	assert.Equal(t, events.ExtractTicketKeys(`git commit -m "PAAS-3669: fixes L310-326 per AC1-3"`), got,
		"extraction must be exactly the shared extractor's answer, so exclude_from_linking "+
			"governs both prose and SCM keys with one configured list")
	assert.Contains(t, got, "PAAS-3669")
}

// TestSCMKeys_DegenerateLines keeps callers from needing a guard.
func TestSCMKeys_DegenerateLines(t *testing.T) {
	assert.Empty(t, scmKeys(nil))
	assert.Empty(t, scmKeys(map[string]any{}))
	assert.Empty(t, scmKeys(map[string]any{"type": "user", "message": "a string, not blocks"}))
	assert.Empty(t, scmKeys(toolLine("2026-09-01T10:00:00Z", "f", "Bash", "")))
}

// TestSegments_CarriesSCMKeys is the integration point: a session whose PROSE never
// names a ticket must still surface the key it committed under. This is the measured
// case — 10 of 20 sessions.
func TestSegments_CarriesSCMKeys(t *testing.T) {
	lines := []map[string]any{
		line("2026-09-01T10:00:00Z", "feature", "/w/u", "user", "let's get started"),
		toolLine("2026-09-01T10:05:00Z", "feature", "Bash", `git commit -m "PAAS-3669: the work"`),
		line("2026-09-01T10:10:00Z", "feature", "/w/u", "user", "looks good"),
	}

	got := segments(lines, 0)

	assert.Len(t, got, 1)
	assert.Equal(t, []string{"PAAS-3669"}, got[0].scmKeys,
		"the key the session committed under must reach the event, even though no prose names it")
	assert.Empty(t, got[0].orderedKeys,
		"and it must stay DISTINCT from prose keys: they carry different provenance strength")
}

// TestSegments_SCMKeysAreScopedToTheirRun mirrors the prose-key scoping rule. A key
// committed while on one branch must not become a candidate for another segment.
func TestSegments_SCMKeysAreScopedToTheirRun(t *testing.T) {
	lines := []map[string]any{
		line("2026-09-01T10:00:00Z", "alpha", "/w/u", "user", "one"),
		toolLine("2026-09-01T10:01:00Z", "alpha", "Bash", `git commit -m "PROJ-1: a"`),
		line("2026-09-01T11:00:00Z", "beta", "/w/u", "user", "two"),
		toolLine("2026-09-01T11:01:00Z", "beta", "Bash", `git commit -m "PROJ-2: b"`),
	}

	got := segments(lines, 0)

	assert.Len(t, got, 2)
	assert.Equal(t, []string{"PROJ-1"}, got[0].scmKeys)
	assert.Equal(t, []string{"PROJ-2"}, got[1].scmKeys)
}

// TestSCMKeys_AMentionIsNotAuthorship is F34. Measured: 13 of 172 commands containing
// `gh pr create` only mentioned it, and the keys in such a command became SCM-command
// provenance, the tier just below a branch name. Each case names a verb without running it.
func TestSCMKeys_AMentionIsNotAuthorship(t *testing.T) {
	for _, tc := range []struct{ name, command string }{
		{"plan appended through a heredoc", "cat >> docs/plan.md <<'EOF'\ngit commit -m \"PROJ-5: x\"\nEOF"},
		{"echo", `echo "next: git commit -m PROJ-5"`},
		{"grep pattern", `grep -rn "gh pr create.*PROJ-5" .`},
		{"python heredoc", "python3 - <<'PY'\nprint('git commit -m PROJ-5')\nPY"},
		{"comment body", `gh pr comment 42 --body "please git commit PROJ-5 first"`},
		{"listing branches", `git branch --list "PROJ-5*"`},
		{"deleting a tag", `git tag -d PROJ-5-rc1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Empty(t, scmKeys(toolLine("2026-09-01T10:00:00Z", "f", "Bash", tc.command)))
		})
	}
}

// TestSCMKeys_AKeyBelongsToTheCommandThatWroteIt: in a compound command only the authoring
// command's own text is read, so a key a neighbouring read named is not authorship.
func TestSCMKeys_AKeyBelongsToTheCommandThatWroteIt(t *testing.T) {
	got := scmKeys(toolLine("2026-09-01T10:00:00Z", "f", "Bash",
		`git log --grep PROJ-9 --oneline && git commit -m "PROJ-1: the change"`))

	assert.Equal(t, []string{"PROJ-1"}, got)
}

// TestSCMKeys_ReadsTheShapesClaudeCodeActuallyCommitsWith: the message in a heredoc fed to
// the commit, or in a command substitution in its argument, is the commit's own text.
func TestSCMKeys_ReadsTheShapesClaudeCodeActuallyCommitsWith(t *testing.T) {
	for _, tc := range []struct{ name, command string }{
		{"-m with a heredoc substitution", "git commit -m \"$(cat <<'EOF'\nPROJ-1: the change\n\nbody\nEOF\n)\""},
		{"-F - from a heredoc", "git commit -F - <<'EOF'\nPROJ-1: the change\nEOF"},
		{"git -C <dir>", `git -C /w/u commit -m "PROJ-1: x"`},
		{"checkout with -q before -b", `git checkout -q -b PROJ-1-work`},
		{"creating a branch", `git branch PROJ-1-work`},
		{"tagging", `git tag -a PROJ-1-rc1 -m "rc"`},
		{"worktree on a new branch (measured)", `git -C /w/u worktree add -b PROJ-1-audit /w/u-wt origin/main`},
		{"checkout -B (measured)", `git checkout -B PROJ-1-sweep origin/PROJ-1-sweep`},
		{"absolute path", `/usr/bin/git commit -m "PROJ-1: x"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, []string{"PROJ-1"}, scmKeys(toolLine("2026-09-01T10:00:00Z", "f", "Bash", tc.command)))
		})
	}
}
