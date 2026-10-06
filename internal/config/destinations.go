package config

import (
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// Host is the host a connection serves, which is also the key of its
// UNJIRA_GITHUB_CREDENTIALS entry for a github connection: https://api.github.com serves
// github.com, and any other endpoint (GHES at https://<host>/api/v3) serves its own host.
// Empty for a connection with no endpoint, or one that does not parse.
func (c Connection) Host() string {
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Host == "" {
		return ""
	}

	if c.Kind == KindGitHub && u.Host == "api.github.com" {
		return "github.com"
	}

	return u.Host
}

// Destination is a tracker a create may land in, at that tracker's default_scope.
type Destination struct {
	Tracker string
	Scope   string
}

// DestinationPlan is where untracked work may be ticketed, decided deterministically
// before any drafting call. See docs/superpowers/specs/2026-10-06-tracker-model-design.md,
// "Destinations".
type DestinationPlan struct {
	// Destinations is the allowed set, in config order. Empty means propose nothing.
	Destinations []Destination
	// Location is the tracker whose scope the work happened in, or "" when no
	// configured scope covers it.
	Location string
	// Ambiguous names the trackers the work's repositories routed to when there was
	// more than one. Treated as no known scope, never guessed.
	Ambiguous []string
	// Reason explains an empty Destinations, for the suppression it is recorded as.
	Reason string
}

// UntrackedDestinations decides where untracked work may be ticketed, from the
// repositories it happened in ("<host>/<owner>/<repo>", case-folded; see
// events.ArtifactWorkRepos and events.ArtifactCwdRemotes):
//
//   - in the scope of exactly one tracker T: T itself if it is writable and has a
//     default_scope, plus the trackers T.mirror_to names. Mirroring is deny by default:
//     unlisted means nowhere else;
//   - in the scopes of several trackers: ambiguous, reported, and treated as below;
//   - in no tracker's scope (a fork, a repository nobody tracks, or nothing known):
//     default_ticket_in.
//
// Every repository is matched, not only one, so a contributor fork beside its upstream
// matches no scope and drops out. A repository matches a tracker's scope only on the host
// that tracker's connection serves. Decided here, at reconcile time, so a config change
// applies without re-collecting.
func (c Config) UntrackedDestinations(repos []string) DestinationPlan {
	var located []string

	for _, repo := range repos {
		if name, ok := c.trackerForRepo(repo); ok && !slices.Contains(located, name) {
			located = append(located, name)
		}
	}

	slices.Sort(located)

	var plan DestinationPlan

	switch len(located) {
	case 1:
		t, _ := c.TrackerByName(located[0])
		plan.Location = t.Name

		if len(t.WritableScopes) > 0 && t.DefaultScope != "" {
			plan.Destinations = append(plan.Destinations, Destination{Tracker: t.Name, Scope: t.DefaultScope})
		}

		plan.Destinations = append(plan.Destinations, c.destinationsNamed(t.MirrorTo)...)
		if len(plan.Destinations) == 0 {
			plan.Reason = fmt.Sprintf(
				"the work happened in tracker %q's scope (%s); that tracker is read-only and its mirror_to "+
					"names no other tracker, so the work is ticketed nowhere else",
				t.Name, strings.Join(repos, ", "))
		}

		return plan
	case 0:
	default:
		plan.Ambiguous = located
	}

	plan.Destinations = c.destinationsNamed(c.DefaultTicketIn)
	if len(plan.Destinations) == 0 {
		plan.Reason = "the work happened in no tracker's scope, and default_ticket_in is empty, so untracked " +
			"work outside every scope is ticketed nowhere"
		if len(plan.Ambiguous) > 0 {
			plan.Reason = fmt.Sprintf("the work's repositories route to several trackers (%s), which is "+
				"ambiguous and treated as no known scope; default_ticket_in is empty, so it is ticketed nowhere",
				strings.Join(plan.Ambiguous, ", "))
		}
	}

	return plan
}

// trackerForRepo finds the github-kind tracker whose scopes cover a host-qualified
// repository, on that tracker's own host.
func (c Config) trackerForRepo(repo string) (string, bool) {
	host, ownerRepo, ok := strings.Cut(repo, "/")
	if !ok {
		return "", false
	}

	for _, t := range c.Trackers {
		conn, ok := c.ConnectionOf(t)
		if !ok || conn.Kind.scopeSyntax() != syntaxRepo || !strings.EqualFold(conn.Host(), host) {
			continue
		}

		if t.containsScope(ownerRepo) {
			return t.Name, true
		}
	}

	return "", false
}

// destinationsNamed resolves tracker names to destinations, in the order given, skipping
// a name that resolves to nothing (ValidateTrackers has refused those at load).
func (c Config) destinationsNamed(names []string) []Destination {
	var out []Destination

	for _, name := range names {
		if t, ok := c.TrackerByName(name); ok && t.DefaultScope != "" &&
			!slices.ContainsFunc(out, func(d Destination) bool { return d.Tracker == name }) {
			out = append(out, Destination{Tracker: t.Name, Scope: t.DefaultScope})
		}
	}

	return out
}

// IssueAcceptsWrites reports whether a linked issue is a destination for comments and
// transitions at all. A tracker on a connection kind with no writer is not one: nothing
// drafted for it could ever be applied, so nothing is drafted. Every other issue is a
// destination, gated by writable_scopes at apply time exactly as before, so a reviewer
// still sees a readable-but-unwritable proposal with its remedy.
func (c Config) IssueAcceptsWrites(issueKey string) (bool, string) {
	ref, err := tasktracker.ParseIssueKey(issueKey)
	if err != nil {
		return true, ""
	}

	t, ok := c.TrackerForScope(ref.Scope)
	if !ok {
		return true, ""
	}

	conn, ok := c.ConnectionOf(t)
	if !ok || conn.Kind.hasWriter() {
		return true, ""
	}

	return false, fmt.Sprintf("%s is tracked read-only by tracker %q (a %s tracker): unjira never writes to it, "+
		"so nothing is drafted for it", issueKey, t.Name, conn.Kind)
}
