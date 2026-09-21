package github

import (
	"fmt"
	"strings"
)

// DefaultHost is the well-known host for github.com, and the host a
// two-segment "owner/repo" config entry elides to.
const DefaultHost = "github.com"

// RepoRef is a fully-resolved (host, owner, repo) triple, parsed from one
// config.Collectors["github"]["repos"] entry.
//
// Host determines both the credential lookup key (UNJIRA_GITHUB_CREDENTIALS is
// keyed by host, not by a config connection name — GitHub is an identity
// provider, not a tenant) and the API base URL. See BaseURL.
type RepoRef struct {
	Host  string
	Owner string
	Repo  string
}

// String renders ref as "host/owner/repo", the canonical, unambiguous form.
func (ref RepoRef) String() string {
	return ref.Host + "/" + ref.OwnerRepo()
}

// OwnerRepo renders ref as "owner/repo", without the host — the form GitHub's
// own REST paths use.
func (ref RepoRef) OwnerRepo() string {
	return ref.Owner + "/" + ref.Repo
}

// ParseRepoRef parses one repos[] config entry.
//
// Parsing rule, stated so a future editor does not improvise: split on '/'; if
// the first segment contains a '.', it is the host, otherwise the host is
// DefaultHost. Exactly two segments must remain after the host is removed —
// anything else is a config error naming the offending entry, because a
// three-segment string whose first segment has no dot is ambiguous (is it
// host-qualified with a dot-less host, or an extra path segment?), and
// guessing is how a repo gets silently collected from the wrong instance.
func ParseRepoRef(raw string) (RepoRef, error) {
	if raw == "" {
		return RepoRef{}, fmt.Errorf("github repos[] entry is empty")
	}

	segments := strings.Split(raw, "/")

	host := DefaultHost
	if strings.Contains(segments[0], ".") {
		host = segments[0]
		segments = segments[1:]
	}

	if len(segments) != 2 || segments[0] == "" || segments[1] == "" {
		return RepoRef{}, fmt.Errorf(
			"github repos[] entry %q: want \"owner/repo\" or \"host/owner/repo\" (host segment "+
				"identified by containing a '.'), got %d segment(s) after host resolution",
			raw, len(segments),
		)
	}

	return RepoRef{Host: host, Owner: segments[0], Repo: segments[1]}, nil
}

// BaseURL returns the REST API base URL for host: github.com resolves to the
// well-known api.github.com; anything else is treated as a GitHub Enterprise
// Server instance, whose documented REST prefix is /api/v3 on the instance's
// own host (never a separate api.<host> the way github.com has one).
func BaseURL(host string) string {
	if host == DefaultHost {
		return "https://api.github.com"
	}

	return "https://" + host + "/api/v3"
}
