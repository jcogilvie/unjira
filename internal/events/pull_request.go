package events

import (
	"slices"
	"strconv"
	"strings"
)

// PullRequestRef renders the ArtifactPullRequest value for pull request number on
// host/owner/repo: "<host>/<owner>/<repo>#<number>", host, owner and repo lower-cased.
//
// The ONE spelling both writers use, so the github collector and the claudecode
// collector cannot drift apart (design-notes #21): each used to format its own
// "<owner>/<repo>#<N>", and the contract test was all that kept them equal.
//
// Host-qualified because without the host, acme/infra#12 on github.com and on a GHES
// instance are one key, and the clustering pre-filter that joins on this value would
// file one PR's merge under the other's narrative (F43). Case-folded because GitHub
// resolves all three case-insensitively, while the two writers read them from
// different places — a hand-typed config entry and the URL gh printed — so an exact
// comparison would silently miss a match the identity guarantees. Folding cannot
// collide: no host holds two owners, or one owner two repositories, differing only in
// case.
func PullRequestRef(host, owner, repo string, number int) string {
	return strings.ToLower(host+"/"+owner+"/"+repo) + "#" + strconv.Itoa(number)
}

// PullRequestOf reads evt's ArtifactPullRequest, returning it only when it is a value
// PullRequestRef writes — "" otherwise, including for an absent or wrongly-typed
// artifact.
//
// Strict rather than tolerant, unlike TicketKeysOf, because its reader is a JOIN: a
// value that is not exactly PullRequestRef's shape must join nothing rather than
// something. The case that matters is the pre-host-qualification "<owner>/<repo>#<N>"
// still on events collected before it: two such values would compare equal across
// hosts, which is the collision host qualification exists to prevent. Such an event is
// not lost — it simply reaches clustering the way every event did before the join.
func PullRequestOf(evt Event) string {
	s, ok := evt.Artifacts[ArtifactPullRequest].(string)
	if !ok {
		return ""
	}

	path, num, ok := strings.Cut(s, "#")
	if !ok {
		return ""
	}

	n, err := strconv.Atoi(num)
	if err != nil || n <= 0 {
		return ""
	}

	segments := strings.Split(path, "/")
	if len(segments) != 3 || slices.Contains(segments, "") {
		return ""
	}

	// Round-trips through the writer: rejects a leading zero, upper case, and anything
	// else PullRequestRef would have spelled differently.
	if PullRequestRef(segments[0], segments[1], segments[2], n) != s {
		return ""
	}

	return s
}
