package events

import (
	"net/url"
	"strings"
)

// Work-location artifacts: where untracked work happened, as host-qualified repositories
// ("<host>/<owner>/<repo>", case-folded). The reconciler matches them against tracker
// scopes at reconcile time, so a config change applies without re-collecting.
// Both use the []any storage contract of ArtifactTicketKeys — write with SetRepos, read
// with ReposOf.
const (
	// ArtifactWorkRepos is where the transcript's own SCM actions sent the work: the
	// remote a `git push` named, a pull request a call created, a `gh -R/--repo` on an
	// authoring command, a GitHub MCP write call's owner/repo. It names where work WENT,
	// so it wins over ArtifactCwdRemotes wherever both exist: measured over 1,296 local
	// segments, the two disagreed in 5 of 123 root and 33 of 104 subagent segments.
	ArtifactWorkRepos = "work_repos"

	// ArtifactCwdRemotes is every remote of the segment's working directory's
	// repository, sorted — the fallback when the transcript carries no SCM action. Every
	// remote, not only origin: a contributor fork matches no tracker scope and drops out
	// at reconcile time, which is the point of keeping them all.
	ArtifactCwdRemotes = "cwd_remotes"

	// ArtifactCwdRemotesOmitted says why ArtifactCwdRemotes is absent — the working
	// directory no longer exists, or is not a repository — as the other *_omitted
	// artifacts do, so a missing location reads as a reason, not as an oversight.
	ArtifactCwdRemotesOmitted = "cwd_remotes_omitted"
)

// SetRepos records repos on evt under key ([]any, the store's round-trip shape).
func SetRepos(evt *Event, key string, repos []string) {
	out := make([]any, len(repos))
	for i, r := range repos {
		out[i] = r
	}

	evt.Artifacts[key] = out
}

// ReposOf reads evt's key as a []string, degrading to fewer (or no) repositories on a
// missing or malformed artifact, like TicketKeysOf.
func ReposOf(evt Event, key string) []string {
	raw, ok := evt.Artifacts[key].([]any)
	if !ok {
		return nil
	}

	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok && s != "" {
			out = append(out, s)
		}
	}

	return out
}

// NormalizeRepo reads a git remote URL — https, ssh://, scp-like (git@host:owner/repo),
// with or without .git — as "<host>/<owner>/<repo>", case-folded. A local path, a file://
// URL, or anything that does not name exactly one owner and repository is not a hosted
// repository, and reports false.
func NormalizeRepo(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}

	var host, path string

	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || u.Scheme == "file" {
			return "", false
		}

		host, path = u.Hostname(), u.Path
	} else {
		// scp-like: [user@]host:owner/repo. A leading "/" or "." is a local path.
		if strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, ".") {
			return "", false
		}

		before, after, ok := strings.Cut(raw, ":")
		if !ok {
			return "", false
		}

		_, host, _ = strings.Cut(before, "@")
		if host == "" {
			host = before
		}

		// A one-letter "host" is a Windows drive (C:/repos/o/r), not a remote.
		if len(host) == 1 {
			return "", false
		}

		path = after
	}

	segments := strings.Split(strings.Trim(strings.TrimSuffix(strings.Trim(path, "/"), ".git"), "/"), "/")
	if host == "" || len(segments) != 2 || segments[0] == "" || segments[1] == "" {
		return "", false
	}

	return strings.ToLower(host + "/" + segments[0] + "/" + segments[1]), true
}
