package claudecode

// shell.go reads a shell command the way the shell would: which simple commands it
// runs, and with what words.
//
// A substring match cannot tell running a command from mentioning it. Over 244 real
// PR-creating tool calls, "gh pr create" as a substring fired on a plan document
// appended through a heredoc, a python heredoc grepping transcripts for the phrase, and
// echo and grep strings; over 172 commands containing it, 13 only mentioned it (F34).
// So commands are parsed with mvdan.cc/sh, the shell parser behind shfmt, and only a
// simple command the shell would execute counts: at command position, after wrappers
// and environment assignments, inside any pipeline, list, loop, condition, function
// body or command substitution. Quoted heredoc bodies, comments and quoted strings are
// data and never count.
//
// That includes a script the shell does go on to run: one handed to another shell
// (`sh -c '…'`, `bash <<'EOF'`, `docker run … sh -c '…'`), and a Python heredoc that
// shells out. Its commands are invisible here. Known and measured, not overlooked: see
// F48 in docs/architecture-findings.md.
//
// A command the parser rejects runs nothing here. Measured over every distinct shell
// command in the local transcripts: 10 of 42,145 (0.02%) fail to parse, all with an
// unclosed quote or heredoc that bash would reject too, so it ran nothing there either.

import (
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

// shellWord is one word of a simple command. literal is false when the word contained
// a parameter expansion, command substitution or other expansion, so its value at run
// time is not the text the transcript holds; text is then the word's source.
type shellWord struct {
	text    string
	literal bool
}

// simpleCommand is one simple command the shell would run.
type simpleCommand struct {
	// words are the command's words after wrappers and environment assignments
	// (dropCommandPrefixes), so words[0] is the program.
	words []shellWord
	// source is the command's own text: its words as written, plus the body of any
	// here-document redirected into it. It is what a ticket key is read from, so a
	// key belongs to the command that wrote it and not to a neighbour in the same
	// list (`git log --grep PROJ-9 && git commit -m "PROJ-1: x"` names PROJ-1).
	source string
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

// assignmentWord is a leading `NAME=value` environment assignment after a wrapper such
// as `env`, where the parser sees it as an ordinary word.
var assignmentWord = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// commandWrappers run the command that follows them, so they are not the command
// proper: the wrappers measured in real transcripts (`rtk` is the token-proxy every
// git/gh call goes through here). Shell keywords need no entry, since the parser never
// makes them words.
var commandWrappers = map[string]bool{
	"time": true, "command": true, "exec": true, "nohup": true, "env": true,
	"builtin": true, "rtk": true,
}

// simpleCommands returns every simple command in a shell command, in source order, or
// none when the shell could not parse it.
func simpleCommands(command string) []simpleCommand {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
	if err != nil {
		return nil
	}

	var out []simpleCommand
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

		parts := []string{spanOf(command, call)}
		for _, r := range stmt.Redirs {
			if r.Hdoc != nil {
				parts = append(parts, spanOf(command, r.Hdoc))
			}
		}
		source := strings.Join(parts, "\n")

		if words = dropCommandPrefixes(words); len(words) > 0 {
			out = append(out, simpleCommand{words: words, source: source})
		}

		return true
	})

	return out
}

// spanOf is a node's text in the source it was parsed from.
func spanOf(source string, n syntax.Node) string {
	return source[n.Pos().Offset():n.End().Offset()]
}

// wordOf is a parsed word's value, when it has one before run time.
func wordOf(source string, w *syntax.Word) shellWord {
	if !isLiteral(w.Parts) {
		return shellWord{text: spanOf(source, w)}
	}

	value, err := expand.Literal(nil, w)
	if err != nil {
		return shellWord{text: spanOf(source, w)}
	}

	return shellWord{text: value, literal: true}
}

// isLiteral reports whether word parts are plain text and quoting only, with no
// expansion whose value depends on run time.
func isLiteral(parts []syntax.WordPart) bool {
	for _, part := range parts {
		switch p := part.(type) {
		case *syntax.Lit, *syntax.SglQuoted:
		case *syntax.DblQuoted:
			if !isLiteral(p.Parts) {
				return false
			}
		default:
			return false
		}
	}

	return true
}

// dropCommandPrefixes removes leading wrappers, the options a wrapper takes before the
// command it runs, and the environment assignments `env` takes. `rtk proxy <cmd>` runs
// the command unfiltered, so `proxy` after `rtk` goes too. Other wrappers' options are
// not skipped, so `command -v go` leaves `-v` as the program, which matches nothing: a
// lookup is not a run.
func dropCommandPrefixes(words []shellWord) []shellWord {
	for len(words) > 0 {
		w := words[0].text

		switch {
		case w == "env":
			words = dropEnvOptions(words[1:])
		case w == "proxy" && len(words) > 1:
			words = words[1:]
		case commandWrappers[w], assignmentWord.MatchString(w):
			words = words[1:]
		default:
			return words
		}
	}

	return words
}

// dropEnvOptions removes env's options from the words after `env`: `-u NAME`, `-C DIR`
// and their long forms take the next word as their value, and every other option stands
// alone. Measured: `env -u GOROOT -u GOPATH go test` is the dominant way this repo's
// own transcripts run its tests.
func dropEnvOptions(words []shellWord) []shellWord {
	for len(words) > 0 && strings.HasPrefix(words[0].text, "-") {
		option := words[0].text
		words = words[1:]
		if envOptionsWithValue[option] && len(words) > 0 {
			words = words[1:]
		}
	}

	return words
}

// envOptionsWithValue are env's options that take their value as the next word.
var envOptionsWithValue = map[string]bool{"-u": true, "--unset": true, "-C": true, "--chdir": true}

// isProgram reports whether a command's program is name, by base name, so an absolute
// path such as /opt/homebrew/bin/gh counts.
func isProgram(words []shellWord, name string) bool {
	return len(words) > 0 && words[0].literal && path.Base(words[0].text) == name
}

// hasWords reports whether a command's words, from the second, begin with want.
func hasWords(words []shellWord, want ...string) bool {
	if len(words) < len(want)+1 {
		return false
	}
	for i, w := range want {
		if words[i+1].text != w {
			return false
		}
	}

	return true
}

// gitSubcommand returns a git command's subcommand and the words after it, skipping
// git's global options (`git -C <dir> commit`), and reports whether it is a git command.
func gitSubcommand(words []shellWord) (string, []shellWord, bool) {
	if !isProgram(words, "git") {
		return "", nil, false
	}

	rest := words[1:]
	for len(rest) > 0 && strings.HasPrefix(rest[0].text, "-") {
		option := rest[0].text
		rest = rest[1:]
		if gitOptionsWithValue[option] && len(rest) > 0 {
			rest = rest[1:]
		}
	}
	if len(rest) == 0 {
		return "", nil, false
	}

	return rest[0].text, rest[1:], true
}

// gitOptionsWithValue are git's global options that take their value as the next word.
var gitOptionsWithValue = map[string]bool{
	"-C": true, "-c": true, "--git-dir": true, "--work-tree": true, "--namespace": true,
	"--exec-path": true, "--config-env": true,
}

// hasAnyWord reports whether any of words is one of want.
func hasAnyWord(words []shellWord, want ...string) bool {
	for _, w := range words {
		if slices.Contains(want, w.text) {
			return true
		}
	}

	return false
}

// hasPositional reports whether words hold an argument that is not an option.
func hasPositional(words []shellWord) bool {
	for _, w := range words {
		if !strings.HasPrefix(w.text, "-") {
			return true
		}
	}

	return false
}

// ghPRCreateInvocations returns the argument words following `gh pr create` for each
// place the command invokes it.
//
// Static sites, not executions: a `for` loop or a shell function that runs one site
// several times returns one entry. The caller must therefore not read the count as the
// number of PRs created — the tool result is what says that.
func ghPRCreateInvocations(command string) [][]shellWord {
	var out [][]shellWord

	for _, c := range simpleCommands(command) {
		if isProgram(c.words, "gh") && hasWords(c.words, "pr", "create") {
			out = append(out, c.words[3:])
		}
	}

	return out
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
