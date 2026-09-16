package claudecode

// scm.go recovers the ticket keys a session named while running git, gh, or the
// GitHub MCP — finding F20.
//
// messageText reads only `type == "text"` content blocks, and its comment is right
// that tool RESULTS are plumbing: a 3,000-line kubectl dump is noise. But it also
// dropped tool INPUTS, and `git commit -m "PAAS-3669: …"` is not plumbing. It is a
// developer naming the ticket for this work — the same explicit human act that makes
// ProvenanceBranch the strongest inferred tier in the correlator.
//
// Measured over 28 helm-charts transcripts across every SCM surface actually in use:
// 10 of 20 sessions carrying any ticket key had one in a tool input that the
// collected prose did NOT contain. unjira's primary interface with git IS Claude
// Code, so the transcript already holds the commit messages and PR titles the
// correlator is otherwise reduced to inferring.

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"

	"github.com/jcogilvie/unjira/internal/events"
)

// authoringVerbs are the SCM operations that CREATE a reference: the developer is
// naming this ticket for this work.
//
// Kept deliberately narrow. The complementary set — `git log --grep=`, `git show`,
// `gh pr view` — is an agent INVESTIGATING a ticket it may have nothing to do with,
// and 8 of the keys found in the measurement appeared only in those. Treating a read
// as authorship would manufacture links from work that never touched the ticket,
// which is the failure mode CLAUDE.md's "keep the review queue signal-rich" invariant
// exists to prevent.
//
// `rtk git commit` matches by containing `git commit`, so the token-proxy wrapper
// needs no separate pattern.
var authoringVerbs = regexp.MustCompile(
	`git commit|git checkout -b|git switch -c|git branch |git tag |` +
		`gh pr create|gh pr edit|gh issue create|gh release create`)

// scmToolNames are the non-shell surfaces. The GitHub MCP is real usage rather than
// hypothetical: 13 create_pull_request and 33 pull_request_read calls were measured
// in the same corpus.
//
// Only the WRITING half is listed, for the same reason authoringVerbs is narrow —
// mcp__github__pull_request_read is investigation.
var scmToolNames = []string{
	"mcp__github__create_pull_request",
	"mcp__github__update_pull_request",
	"mcp__github__create_branch",
	"mcp__github__issue_write",
	"mcp__github__push_files",
	"mcp__github__create_or_update_file",
}

// scmKeys returns the ticket keys a transcript line's tool call named while
// AUTHORING something in the SCM, order-preserving and deduplicated.
//
// Extraction uses events.ExtractTicketKeys rather than a local pattern. The
// measurement behind F20 initially used an ad-hoc `\b[A-Z][A-Z0-9]+-\d+\b` and
// matched `AC1-3`, `L310-326` and `PAAS-0` as tickets; the shared extractor is the
// one place this codebase decides what a key looks like, and its placeholder handling
// (CompileLinkExclusionPatterns) is downstream of it.
//
// Returns nil for anything that is not an authoring SCM call, so a caller needs no
// guard.
func scmKeys(line map[string]any) []string {
	message, _ := line["message"].(map[string]any)
	if message == nil {
		return nil
	}

	blocks, _ := message["content"].([]any)

	var keys []string
	for _, block := range blocks {
		b, ok := block.(map[string]any)
		if !ok || b["type"] != "tool_use" {
			continue
		}

		text, ok := authoringText(b)
		if !ok {
			continue
		}

		for _, key := range events.ExtractTicketKeys(text) {
			if !slices.Contains(keys, key) {
				keys = append(keys, key)
			}
		}
	}

	return keys
}

// authoringText returns the searchable text of a tool_use block when that block is an
// SCM authoring call, and reports whether it is one.
//
// For a shell call the text is the command itself; an authoring verb ANYWHERE in it
// qualifies the whole command, because the real shape is compound — `git log
// --oneline -3 && git commit -m "PAAS-1: …"` inspects and then commits, and the key
// belongs to the commit.
//
// For an MCP call the whole input is searched: the key may be in a title, a body, or
// a branch name, and enumerating those field names per tool would break silently
// whenever a tool's schema changed.
func authoringText(block map[string]any) (string, bool) {
	name, _ := block["name"].(string)

	input, ok := block["input"].(map[string]any)
	if !ok {
		return "", false
	}

	if slices.Contains(scmToolNames, name) {
		encoded, err := json.Marshal(input)
		if err != nil {
			// A tool input that will not marshal carries nothing this can read.
			// Skipping is right: the alternative is failing a whole collection pass
			// over one unreadable line, and scanLines already treats malformed input
			// as skippable.
			return "", false
		}

		return string(encoded), true
	}

	command, _ := input["command"].(string)
	if command == "" || !authoringVerbs.MatchString(command) {
		return "", false
	}

	// Guard against a bare mention with no SCM at all — `echo "git commit"` in a
	// comment, say. Requiring the verb AND a git/gh invocation keeps the gate tight
	// without enumerating shell syntax.
	if !strings.Contains(command, "git") && !strings.Contains(command, "gh ") {
		return "", false
	}

	return command, true
}
