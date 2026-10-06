package tasktracker

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// KeySyntax is the shape of an issue key, which decides how its scope is read.
type KeySyntax int

const (
	// SyntaxProject is <PROJECT>-<NUMBER>: Jira, and the local backend, which mints its
	// keys the same way (store.InsertLocalIssue's fmt.Sprintf("%s-%d", project, n)).
	SyntaxProject KeySyntax = iota + 1
	// SyntaxRepo is <owner>/<repo>#<NUMBER>: a GitHub issue.
	SyntaxRepo
)

// IssueRef is a parsed issue key: the key as written, its syntax, the scope it routes
// by, and its number.
type IssueRef struct {
	Key    string
	Syntax KeySyntax
	// Scope is the project key, or the owner/repo lower-cased. GitHub names are
	// case-insensitive, so two spellings must route as one, the way
	// events.PullRequestRef folds a pull request's identity.
	Scope  string
	Number int
}

// projectKeyRE anchors the whole string to exactly one <PROJECT>-<NUMBER> segment: an
// uppercase-alphanumeric project prefix starting with a letter, a hyphen, and a numeric
// suffix. Anchored, unlike events.TicketKeyRegexp's scan-for-candidates pattern, because
// this validates one already-known key — a stray "PROJ-1-2" must be rejected, not
// truncated to its first match.
var projectKeyRE = regexp.MustCompile(`^([A-Z][A-Z0-9]*)-(\d+)$`)

// repoKeyRE is <owner>/<repo>#<NUMBER>, each name GitHub's character set. A bare #N is
// deliberately not a key: resolving it needs repository context the key does not carry.
var repoKeyRE = regexp.MustCompile(`^([A-Za-z0-9._-]+)/([A-Za-z0-9._-]+)#(\d+)$`)

// ParseIssueKey reads an issue key in its tracker's native syntax. The two syntaxes
// cannot be confused, so a key's scope is a pure function of the key.
//
// Errors, naming the offending key, on anything else, per CLAUDE.md's "don't guess":
// a truncated or empty guess would let a write-scope check silently pass or fail on
// data that was already wrong.
func ParseIssueKey(key string) (IssueRef, error) {
	if m := projectKeyRE.FindStringSubmatch(key); m != nil {
		n, err := strconv.Atoi(m[2])
		if err != nil {
			return IssueRef{}, fmt.Errorf("issue key %q: number: %w", key, err)
		}

		return IssueRef{Key: key, Syntax: SyntaxProject, Scope: m[1], Number: n}, nil
	}

	if m := repoKeyRE.FindStringSubmatch(key); m != nil {
		n, err := strconv.Atoi(m[3])
		if err != nil {
			return IssueRef{}, fmt.Errorf("issue key %q: number: %w", key, err)
		}

		return IssueRef{
			Key: key, Syntax: SyntaxRepo, Scope: strings.ToLower(m[1] + "/" + m[2]), Number: n,
		}, nil
	}

	return IssueRef{}, fmt.Errorf(
		"issue key %q is neither <PROJECT>-<NUMBER> nor <owner>/<repo>#<NUMBER>", key)
}

// ScopeMatches reports whether pattern, a configured scope, covers scope. A repository
// scope (one containing "/") compares segment by segment, case-folded, with "*"
// matching any one whole segment. A project scope compares exactly, since Jira project
// keys are case-sensitive uppercase.
//
// The one matcher for scopes: config's write-scope and routing checks call it, and so
// does Resolver, so the two cannot disagree about which tracker owns a key.
func ScopeMatches(pattern, scope string) bool {
	if !strings.Contains(pattern, "/") {
		return pattern == scope
	}

	ps, ss := strings.Split(pattern, "/"), strings.Split(scope, "/")
	if len(ps) != len(ss) {
		return false
	}

	for i := range ps {
		if ps[i] != "*" && !strings.EqualFold(ps[i], ss[i]) {
			return false
		}
	}

	return true
}

// syntaxOfScope is the key syntax a configured scope routes.
func syntaxOfScope(scope string) KeySyntax {
	if strings.Contains(scope, "/") {
		return SyntaxRepo
	}

	return SyntaxProject
}
