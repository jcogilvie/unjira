package events

import (
	"cmp"
	"regexp"
	"slices"
	"strings"
)

// githubRefRE is a qualified GitHub issue reference, <owner>/<repo>#<N>, as written in
// prose or in a pull request body ("Fixes owner/repo#N"). The leading group refuses a
// match inside a longer path (docs/a/b#1): a reference starts at the beginning of the
// text or after a character that cannot be part of one.
var githubRefRE = regexp.MustCompile(
	`(?:^|[^A-Za-z0-9_./-])([A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?)/([A-Za-z0-9._-]+)#(\d+)\b`)

// githubIssueURLRE is a github.com issue URL. Pull request URLs (/pull/N) are not
// matched: a pull request is work evidence, never an issue a narrative is tracked in.
var githubIssueURLRE = regexp.MustCompile(
	`https?://github\.com/([A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?)/([A-Za-z0-9._-]+)/issues/(\d+)\b`)

// ExtractGitHubIssueRefs returns the GitHub issue references in text as
// tasktracker-native keys, <owner>/<repo>#<N>, lower-cased, deduplicated, in order of
// appearance.
//
// Lower-cased because GitHub names are case-insensitive, the way PullRequestRef folds
// a pull request's identity, so two spellings are one candidate.
//
// A bare #N is deliberately not a reference: resolving it needs the repository the
// text was written in, which a key does not carry (internal/correlator/refs is for
// that, finding F1). Issue URLs on hosts other than github.com are not recognized yet.
func ExtractGitHubIssueRefs(text string) []string {
	type found struct {
		at  int
		key string
	}

	var all []found

	for _, re := range []*regexp.Regexp{githubRefRE, githubIssueURLRE} {
		for _, m := range re.FindAllStringSubmatchIndex(text, -1) {
			key := strings.ToLower(text[m[2]:m[3]] + "/" + text[m[4]:m[5]] + "#" + text[m[6]:m[7]])
			all = append(all, found{at: m[2], key: key})
		}
	}

	slices.SortStableFunc(all, func(a, b found) int { return cmp.Compare(a.at, b.at) })

	var keys []string

	for _, f := range all {
		if !slices.Contains(keys, f.key) {
			keys = append(keys, f.key)
		}
	}

	return keys
}
