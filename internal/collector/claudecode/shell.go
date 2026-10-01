package claudecode

// shell.go recognizes `gh pr create` as an INVOCATION, not as a substring.
//
// authoringVerbs (scm.go) matches "gh pr create" anywhere in a command, which is right
// for what it feeds — a ticket KEY found near an authoring verb is still a key. An
// anchor event asserts something stronger, that a pull request was created, and over
// 244 real PR-creating tool calls the substring fired on commands that only mentioned
// the phrase: a plan document appended through a heredoc, a python heredoc grepping
// transcripts for it, echo and grep strings.
//
// So this is a deliberately small shell lexer, sized to the real shapes rather than to
// the shell grammar: it drops heredoc bodies and comments, honours single and double
// quotes, splits on the control operators, and then asks whether a simple command's
// words are `gh pr create`. Anything it cannot parse degrades to "not an invocation"
// or to an argument it reports as unusable — never to a guess and never to a panic.

import (
	"net/url"
	"path"
	"regexp"
	"strings"
)

// shellWord is one word of a simple command. literal is false when the word contained
// a parameter expansion or command substitution, so its value at run time is not the
// text the transcript holds.
type shellWord struct {
	text    string
	literal bool
}

// prArgs is what a `gh pr create` command line says about the PR it creates. Each
// field is a literal from the command, or empty; notes say why a present flag was not
// usable.
type prArgs struct {
	// repo is `owner/repo`, with any host or URL scheme removed.
	repo string
	// head is the head branch, with gh's `owner:` fork prefix removed (git refnames
	// cannot contain ':', so the split is unambiguous).
	head  string
	title string
	notes []string
}

// heredocOperator matches `<<WORD`, `<<-WORD`, `<<'WORD'` and `<<"WORD"`. A here-string
// (`<<<`) is excluded by the caller, since RE2 has no lookbehind.
var heredocOperator = regexp.MustCompile(`<<(-?)[ \t]*(?:'([^']+)'|"([^"]+)"|\\?([A-Za-z_][A-Za-z0-9_]*))`)

// assignmentWord is a leading `NAME=value` environment assignment.
var assignmentWord = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// commandPrefixes are words that can precede the command proper without being it:
// shell keywords that open a body or condition, and the wrappers measured in real
// transcripts (`rtk` is the token-proxy every git/gh call goes through here).
var commandPrefixes = map[string]bool{
	"do": true, "then": true, "else": true, "elif": true, "if": true, "while": true,
	"until": true, "!": true, "{": true, "}": true, "time": true, "command": true,
	"exec": true, "nohup": true, "env": true, "builtin": true, "rtk": true,
}

// ghPRCreateInvocations returns the argument words following `gh pr create` for each
// place the command invokes it.
//
// Static sites, not executions: a `for` loop or a shell function that runs one site
// several times returns one entry. The caller must therefore not read the count as the
// number of PRs created — the tool result is what says that.
func ghPRCreateInvocations(command string) [][]shellWord {
	var out [][]shellWord

	for _, words := range simpleCommands(stripHeredocBodies(command)) {
		words = dropCommandPrefixes(words)
		if len(words) < 3 {
			continue
		}
		if path.Base(words[0].text) != "gh" || words[1].text != "pr" || words[2].text != "create" {
			continue
		}

		out = append(out, words[3:])
	}

	return out
}

// stripHeredocBodies removes the body lines of every heredoc, keeping the line that
// opens it. A body is data, not commands, and real bodies quote `gh pr create` in
// documentation and scripts.
//
// An unterminated heredoc consumes the rest of the command, as the shell itself would.
func stripHeredocBodies(command string) string {
	lines := strings.Split(command, "\n")
	out := make([]string, 0, len(lines))

	var pending []string

	for _, l := range lines {
		if len(pending) > 0 {
			if strings.TrimSpace(l) == pending[0] {
				pending = pending[1:]
			}

			continue
		}

		out = append(out, l)

		for _, loc := range heredocOperator.FindAllStringSubmatchIndex(l, -1) {
			if loc[0] > 0 && l[loc[0]-1] == '<' {
				continue // `<<<` here-string: no body follows
			}

			for g := 2; g <= 4; g++ {
				if start := loc[2*g+0]; start >= 0 {
					pending = append(pending, l[start:loc[2*g+1]])

					break
				}
			}
		}
	}

	return strings.Join(out, "\n")
}

// simpleCommands splits a command into simple commands: word lists separated by the
// control operators (`&&`, `||`, `;`, `|`, `&`, newline), subshell and grouping
// parentheses, and the start of an unquoted command substitution.
func simpleCommands(s string) [][]shellWord {
	lx := lexer{}

	for i := 0; i < len(s); i++ {
		c := s[i]

		switch {
		case c == '\\':
			if i+1 < len(s) {
				i++
				if s[i] != '\n' { // backslash-newline is a line continuation
					lx.add(s[i])
				}
			}
		case c == '\'':
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				end = len(s) - i - 1
			}
			lx.addString(s[i+1 : i+1+end])
			i += end + 1
		case c == '"':
			i = lx.doubleQuoted(s, i)
		case c == '$' && i+1 < len(s) && s[i+1] == '(':
			lx.endCommand()
			i++
		case c == '`':
			lx.endCommand()
		case c == '$':
			lx.literal = false
			lx.add(c)
		case c == '#' && !lx.inWord:
			if end := strings.IndexByte(s[i:], '\n'); end >= 0 {
				i += end - 1
			} else {
				i = len(s)
			}
		case c == ' ' || c == '\t':
			lx.endWord()
		case strings.IndexByte(";&|\n()", c) >= 0:
			lx.endCommand()
		default:
			lx.add(c)
		}
	}

	lx.endCommand()

	return lx.commands
}

type lexer struct {
	commands [][]shellWord
	cur      []shellWord
	word     strings.Builder
	inWord   bool
	literal  bool
}

func (lx *lexer) add(c byte) {
	if !lx.inWord {
		lx.inWord, lx.literal = true, true
	}
	lx.word.WriteByte(c)
}

func (lx *lexer) addString(s string) {
	if !lx.inWord {
		lx.inWord, lx.literal = true, true
	}
	lx.word.WriteString(s)
}

// doubleQuoted consumes a double-quoted span starting at s[start] and returns the index
// of its closing quote (or the end of input). Expansions inside it make the word
// non-literal; a command substitution inside quotes is not recursed into.
func (lx *lexer) doubleQuoted(s string, start int) int {
	if !lx.inWord {
		lx.inWord, lx.literal = true, true
	}

	i := start + 1
	for ; i < len(s) && s[i] != '"'; i++ {
		switch s[i] {
		case '\\':
			if i+1 < len(s) {
				i++
			}
		case '$', '`':
			lx.literal = false
		}
		lx.word.WriteByte(s[i])
	}

	return i
}

func (lx *lexer) endWord() {
	if !lx.inWord {
		return
	}

	lx.cur = append(lx.cur, shellWord{text: lx.word.String(), literal: lx.literal})
	lx.word.Reset()
	lx.inWord = false
}

func (lx *lexer) endCommand() {
	lx.endWord()
	if len(lx.cur) > 0 {
		lx.commands = append(lx.commands, lx.cur)
	}
	lx.cur = nil
}

// dropCommandPrefixes removes leading keywords, wrappers and environment assignments.
// `rtk proxy <cmd>` runs the command unfiltered, so `proxy` after `rtk` goes too.
func dropCommandPrefixes(words []shellWord) []shellWord {
	for len(words) > 0 {
		w := words[0].text

		switch {
		case w == "proxy" && len(words) > 1:
			words = words[1:]
		case commandPrefixes[w], assignmentWord.MatchString(w):
			words = words[1:]
		default:
			return words
		}
	}

	return words
}

// parsePRCreateArgs reads --repo/-R, --head/-H and --title/-t from a `gh pr create`
// invocation's argument words, in each of the forms gh's flag parser accepts.
func parsePRCreateArgs(words []shellWord) prArgs {
	var out prArgs

	for i := 0; i < len(words); i++ {
		w := words[i].text

		for _, f := range []struct {
			long, short string
			set         func(value string)
		}{
			{"--repo", "-R", func(v string) { out.repo = out.normalizeRepo(v) }},
			{"--head", "-H", func(v string) { out.head = headBranch(v) }},
			{"--title", "-t", func(v string) { out.title = v }},
		} {
			var value shellWord

			switch {
			case w == f.long || w == f.short:
				if i+1 >= len(words) {
					out.notes = append(out.notes, f.long+" has no value")

					continue
				}
				i++
				value = words[i]
			case strings.HasPrefix(w, f.long+"="):
				value = shellWord{text: strings.TrimPrefix(w, f.long+"="), literal: words[i].literal}
			case strings.HasPrefix(w, f.short) && len(w) > len(f.short) && !strings.HasPrefix(w, "--"):
				value = shellWord{text: strings.TrimPrefix(w, f.short), literal: words[i].literal}
			default:
				continue
			}

			if !value.literal {
				out.notes = append(out.notes, f.long+" is a shell expansion, not a literal")

				continue
			}

			f.set(value.text)
		}
	}

	return out
}

// normalizeRepo reduces gh's `[HOST/]OWNER/REPO` or a repository URL to `owner/repo`,
// recording a note and returning "" for anything else.
func (a *prArgs) normalizeRepo(value string) string {
	p := value
	if strings.Contains(value, "://") {
		u, err := url.Parse(value)
		if err != nil {
			a.notes = append(a.notes, "--repo is not a parseable URL: "+value)

			return ""
		}
		p = u.Path
	}

	parts := strings.Split(strings.Trim(p, "/"), "/")
	if len(parts) == 3 {
		parts = parts[1:] // HOST/OWNER/REPO
	}
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		a.notes = append(a.notes, "--repo is not OWNER/REPO: "+value)

		return ""
	}

	return parts[0] + "/" + strings.TrimSuffix(parts[1], ".git")
}

// headBranch strips gh's `owner:` fork prefix from a --head value.
func headBranch(value string) string {
	if _, branch, ok := strings.Cut(value, ":"); ok {
		return branch
	}

	return value
}
