package config

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// ConnectionKind is the system a connection talks to. It decides which backend reads
// and writes the trackers on that connection, and which key syntax their scopes use.
type ConnectionKind string

// The connection kinds unjira has a backend for.
const (
	// KindJira is a Jira Cloud site. It has a reader and a writer.
	KindJira ConnectionKind = "jira"
	// KindGitHub is a GitHub API endpoint. It is read-only: unjira never writes to a
	// public tracker, and no GitHub writer exists.
	KindGitHub ConnectionKind = "github"
	// KindLocal is unjira's own store-backed tracker, for tests and dry operation.
	KindLocal ConnectionKind = "local"
)

// hasWriter reports whether unjira has a tasktracker.TaskWriter for this kind. A
// writable_scopes entry on a kind without one is refused at load, since it would arm
// nothing while telling the operator writes are possible.
func (k ConnectionKind) hasWriter() bool {
	return k == KindJira || k == KindLocal
}

// scopeSyntax names the key space a kind's scopes live in. Jira and local trackers
// both mint <PROJECT>-<N> keys, so they share one key space, and a project cannot be
// claimed by one tracker of each.
func (k ConnectionKind) scopeSyntax() scopeSyntax {
	if k == KindGitHub {
		return syntaxRepo
	}

	return syntaxProject
}

type scopeSyntax int

const (
	// syntaxProject is a Jira-style project key: PAAS, DEVSBX.
	syntaxProject scopeSyntax = iota
	// syntaxRepo is an owner/repo path, matched case-insensitively, with a whole-segment
	// "*" allowed as the repository: crossplane-contrib/*.
	syntaxRepo
)

// projectScopeRE matches a project key, the same shape tasktracker.ParseIssueKey
// accepts as an issue key's prefix.
var projectScopeRE = regexp.MustCompile(`^[A-Z][A-Z0-9]*$`)

// repoSegmentRE matches one GitHub owner or repository name.
var repoSegmentRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Connection is WHO and WHERE: which system, at which endpoint. It says nothing about
// scope — that is a tracker's concern — so one credential serves every tracker and
// collector on it.
//
// The credential is looked up in the environment, never here: a jira connection by its
// Name in UNJIRA_JIRA_CREDENTIALS, a github connection by its endpoint's host in
// UNJIRA_GITHUB_CREDENTIALS.
type Connection struct {
	Name     string         `json:"name"`
	Kind     ConnectionKind `json:"kind"`
	Endpoint string         `json:"endpoint"`
}

// Tracker is WHAT: the scopes one tracker owns on a connection, which of them unjira may
// write to, and what the Jira collector reads from it.
type Tracker struct {
	Name       string `json:"name"`
	Connection string `json:"connection"`
	// Scopes is the read scope: which issues route to this tracker. A Jira project key,
	// or a GitHub owner/repo (with owner/* matching every repository of that owner).
	// Scopes never overlap across trackers, so every issue routes to exactly one.
	Scopes []string `json:"scopes"`
	// WritableScopes is which Scopes unjira may WRITE to — the choke point gate.Applier
	// consults before any write. Declared independently of Scopes.
	//
	// Absent or empty means NOTHING is writable — deny by default. Deliberately NOT
	// "defaults to Scopes when unset": that would arm every scope unjira reads for
	// writes the moment a tracker is configured, which is the exact bug write scope
	// exists to prevent. See docs/superpowers/specs/2026-08-27-write-scope-design.md.
	//
	// Must be a subset of Scopes, and is refused on a connection kind with no writer.
	WritableScopes []string `json:"writable_scopes"`
	// DefaultScope is where this tracker's creates land. Must be writable, and is
	// required when the tracker is writable and named in a destination list
	// (default_ticket_in or another tracker's mirror_to).
	DefaultScope string `json:"default_scope"`
	// MirrorTo names the trackers that work tracked or done in this tracker's scope may
	// ALSO be ticketed in. Absent means nowhere else: deny by default. The policy is the
	// operator's; unjira only encodes the mechanism.
	MirrorTo []string `json:"mirror_to"`
	// Queries are the named JQL views the Jira collector reads, each scoped to Scopes
	// (see EffectiveJQL). Only meaningful on a jira connection. Empty means the tracker
	// routes keys but collects nothing.
	Queries []JiraQuery `json:"queries"`
	// MaxIssuesPerQuery bounds one query's issue count per pass. Zero means
	// DefaultMaxIssuesPerQuery.
	MaxIssuesPerQuery int `json:"max_issues_per_query"`
}

// IsScopeWritable reports whether scope is in WritableScopes (matched by
// tasktracker.ScopeMatches) — the single question gate.Applier asks before every tracker
// write. Empty WritableScopes answers false for
// every scope, including the ones the tracker reads.
func (t Tracker) IsScopeWritable(scope string) bool {
	return slices.ContainsFunc(t.WritableScopes, func(w string) bool { return tasktracker.ScopeMatches(w, scope) })
}

// containsScope reports whether scope routes to this tracker.
func (t Tracker) containsScope(scope string) bool {
	return slices.ContainsFunc(t.Scopes, func(s string) bool { return tasktracker.ScopeMatches(s, scope) })
}

// EffectiveJQL returns query's JQL scoped to this tracker's Scopes.
//
// The scope is added rather than left to the operator because an unscoped JQL
// (assignee = currentUser(), say) spans a whole site, and collecting an issue from a
// project no tracker covers would narrate work the reconciler can never act on.
//
// Empty Scopes is an error rather than "collect everything", for the same reason.
func (t Tracker) EffectiveJQL(query JiraQuery) (string, error) {
	if len(t.Scopes) == 0 {
		return "", fmt.Errorf(
			"tracker %q has no scopes: cannot scope collector query %q, and an unscoped query "+
				"would collect issues no tracker routes",
			t.Name, query.Name,
		)
	}

	quoted := make([]string, 0, len(t.Scopes))
	for _, key := range t.Scopes {
		quoted = append(quoted, fmt.Sprintf("%q", key))
	}

	return fmt.Sprintf("(%s) AND project IN (%s)", query.JQL, strings.Join(quoted, ", ")), nil
}

// IssueLimit returns the per-query issue cap, defaulting when unset. A negative value is
// a configuration error rather than silently coerced: it most likely means someone
// intended "no limit", which the collector deliberately does not offer.
func (t Tracker) IssueLimit() (int, error) {
	switch {
	case t.MaxIssuesPerQuery < 0:
		return 0, fmt.Errorf(
			"tracker %q has max_issues_per_query %d: must be positive, or omitted for the default of %d",
			t.Name, t.MaxIssuesPerQuery, DefaultMaxIssuesPerQuery,
		)
	case t.MaxIssuesPerQuery == 0:
		return DefaultMaxIssuesPerQuery, nil
	default:
		return t.MaxIssuesPerQuery, nil
	}
}

// scopesOverlap reports whether some issue could route to both a and b. Only "*" is a
// wildcard, and only as a whole segment, so two scopes overlap exactly when every
// segment is equal or either side is "*".
func scopesOverlap(a, b string) bool {
	if strings.Contains(a, "/") != strings.Contains(b, "/") {
		return false
	}

	if !strings.Contains(a, "/") {
		return a == b
	}

	as, bs := strings.Split(a, "/"), strings.Split(b, "/")
	if len(as) != len(bs) {
		return false
	}

	for i := range as {
		if as[i] != "*" && bs[i] != "*" && !strings.EqualFold(as[i], bs[i]) {
			return false
		}
	}

	return true
}

// yamlBooleanHint explains a "true"/"false" where a key was expected. YAML 1.1 reads an
// unquoted NO, ON, YES or OFF as a boolean, and sigs.k8s.io/yaml renders that boolean
// into a string field without an error — so the only place to catch it is here.
func yamlBooleanHint(value string) string {
	if value != "true" && value != "false" {
		return ""
	}

	return fmt.Sprintf(" (YAML reads an unquoted NO, ON, YES or OFF as a boolean, which arrives "+
		"here as %q: quote the value, e.g. \"NO\")", value)
}

// validateScope checks one scope's syntax for the tracker's connection kind.
func validateScope(field, scope string, syntax scopeSyntax) error {
	switch syntax {
	case syntaxProject:
		if projectScopeRE.MatchString(scope) {
			return nil
		}

		return fmt.Errorf("%s: %q is not a project key (uppercase letters and digits, starting "+
			"with a letter)%s", field, scope, yamlBooleanHint(scope))
	case syntaxRepo:
		owner, repo, ok := strings.Cut(scope, "/")
		if ok && repoSegmentRE.MatchString(owner) && (repo == "*" || repoSegmentRE.MatchString(repo)) {
			return nil
		}

		return fmt.Errorf("%s: %q is not an owner/repo scope (a literal owner, then a repository "+
			"name or *)%s", field, scope, yamlBooleanHint(scope))
	}

	return fmt.Errorf("%s: unknown scope syntax", field)
}

// ConnectionByName finds a connection by its configured name.
func (c Config) ConnectionByName(name string) (Connection, bool) {
	i := slices.IndexFunc(c.Connections, func(conn Connection) bool { return conn.Name == name })
	if i < 0 {
		return Connection{}, false
	}

	return c.Connections[i], true
}

// TrackerByName finds a tracker by its configured name.
func (c Config) TrackerByName(name string) (Tracker, bool) {
	i := slices.IndexFunc(c.Trackers, func(t Tracker) bool { return t.Name == name })
	if i < 0 {
		return Tracker{}, false
	}

	return c.Trackers[i], true
}

// ConnectionOf returns the connection tracker t sits on.
func (c Config) ConnectionOf(t Tracker) (Connection, bool) {
	return c.ConnectionByName(t.Connection)
}

// TrackerForScope finds the one tracker whose Scopes cover scope. ValidateTrackers
// refuses overlapping scopes, so at most one tracker matches.
func (c Config) TrackerForScope(scope string) (Tracker, bool) {
	if scope == "" {
		return Tracker{}, false
	}

	i := slices.IndexFunc(c.Trackers, func(t Tracker) bool { return t.containsScope(scope) })
	if i < 0 {
		return Tracker{}, false
	}

	return c.Trackers[i], true
}

// FirstProjectScope returns the first project-key scope configured, in tracker order —
// the fallback for commands that take a --project. Repository scopes are skipped, since
// a repository is not a project key.
func (c Config) FirstProjectScope() (string, bool) {
	for _, t := range c.Trackers {
		for _, s := range t.Scopes {
			if projectScopeRE.MatchString(s) {
				return s, true
			}
		}
	}

	return "", false
}

// DefaultCreateTarget resolves the first default_ticket_in tracker, erroring loudly when
// unset. Its DefaultScope is where a create that names no scope of its own lands — every
// create persisted before destinations existed; a create proposed since carries its
// destination's scope (UntrackedDestinations). ValidateTrackers has already checked that
// the tracker is writable and has a DefaultScope.
func (c Config) DefaultCreateTarget() (Tracker, error) {
	if len(c.DefaultTicketIn) == 0 {
		return Tracker{}, errors.New(
			"no default_ticket_in configured for new-issue creation: name a writable tracker with a default_scope")
	}

	t, ok := c.TrackerByName(c.DefaultTicketIn[0])
	if !ok {
		return Tracker{}, fmt.Errorf("default_ticket_in names %q, which is not a configured tracker",
			c.DefaultTicketIn[0])
	}

	return t, nil
}

// DefaultCreateScope is DefaultCreateTarget's scope, or "" when no default is
// configured. gate.Applier refuses a create with an empty default, at apply time.
func (c Config) DefaultCreateScope() string {
	t, err := c.DefaultCreateTarget()
	if err != nil {
		return ""
	}

	return t.DefaultScope
}

// ValidateTrackers checks the tracker model, returning every problem found rather than
// the first, each naming its field. Load runs it, so a misconfigured tracker fails
// before any command does work.
func (c Config) ValidateTrackers() error {
	var errs []error

	kinds := make(map[string]ConnectionKind, len(c.Connections))

	for i, conn := range c.Connections {
		errs = append(errs, validateConnection(i, conn, kinds)...)
		kinds[conn.Name] = conn.Kind
	}

	trackerNames := make(map[string]bool, len(c.Trackers))
	for i, t := range c.Trackers {
		if t.Name == "" {
			errs = append(errs, fmt.Errorf("trackers[%d].name is required", i))
		} else if trackerNames[t.Name] {
			errs = append(errs, fmt.Errorf("duplicate tracker name %q%s", t.Name, yamlBooleanHint(t.Name)))
		}

		trackerNames[t.Name] = true
	}

	for i, t := range c.Trackers {
		kind, ok := kinds[t.Connection]
		if !ok {
			errs = append(errs, fmt.Errorf("trackers[%d].connection %q is not a configured connection%s",
				i, t.Connection, yamlBooleanHint(t.Connection)))

			continue
		}

		errs = append(errs, validateTracker(i, t, kind)...)
		errs = append(errs, c.validateMirrorTo(i, t)...)
	}

	errs = append(errs, c.validateScopeOverlap(kinds)...)
	errs = append(errs, c.validateQueryNames(kinds)...)
	errs = append(errs, c.validateDefaultTicketIn()...)

	return errors.Join(errs...)
}

func validateConnection(i int, conn Connection, seen map[string]ConnectionKind) []error {
	var errs []error

	if conn.Name == "" {
		errs = append(errs, fmt.Errorf("connections[%d].name is required", i))
	} else if _, dup := seen[conn.Name]; dup {
		errs = append(errs, fmt.Errorf("duplicate connection name %q", conn.Name))
	}

	switch conn.Kind {
	case KindJira, KindGitHub:
		if conn.Endpoint == "" {
			errs = append(errs, fmt.Errorf("connections[%d].endpoint is required for kind %q", i, conn.Kind))
		} else if conn.Host() == "" {
			errs = append(errs, fmt.Errorf("connections[%d].endpoint %q is not an absolute URL", i, conn.Endpoint))
		}
	case KindLocal:
		if conn.Endpoint != "" {
			errs = append(errs, fmt.Errorf(
				"connections[%d].endpoint is set, but a %q connection has no endpoint: remove it", i, conn.Kind))
		}
	default:
		errs = append(errs, fmt.Errorf("connections[%d] has kind %q: must be one of %q, %q or %q",
			i, conn.Kind, KindJira, KindGitHub, KindLocal))
	}

	return errs
}

func validateTracker(i int, t Tracker, kind ConnectionKind) []error {
	var errs []error

	if len(t.Scopes) == 0 {
		errs = append(errs, fmt.Errorf(
			"trackers[%d].scopes is empty: tracker %q would route no issue", i, t.Name))
	}

	for _, s := range t.Scopes {
		if err := validateScope(fmt.Sprintf("trackers[%d].scopes", i), s, kind.scopeSyntax()); err != nil {
			errs = append(errs, err)
		}
	}

	if len(t.WritableScopes) > 0 && !kind.hasWriter() {
		errs = append(errs, fmt.Errorf(
			"tracker %q: writable_scopes is set, but connection %q is kind %q, which has no writer in "+
				"unjira: a %s tracker is read-only, so remove writable_scopes",
			t.Name, t.Connection, kind, kind))
	}

	for _, w := range t.WritableScopes {
		if !slices.Contains(t.Scopes, w) {
			errs = append(errs, fmt.Errorf(
				"tracker %q: writable_scopes includes %q, which is not in scopes — a scope must be readable "+
					"by this tracker before it can be declared writable%s",
				t.Name, w, yamlBooleanHint(w)))
		}
	}

	if t.DefaultScope != "" && !slices.Contains(t.WritableScopes, t.DefaultScope) {
		errs = append(errs, fmt.Errorf(
			"tracker %q: default_scope %q is not in writable_scopes, so every create there would be refused%s",
			t.Name, t.DefaultScope, yamlBooleanHint(t.DefaultScope)))
	}

	if len(t.Queries) > 0 && kind != KindJira {
		errs = append(errs, fmt.Errorf(
			"trackers[%d].queries is set on tracker %q, whose connection is kind %q: queries are JQL, "+
				"read only by the jira collector", i, t.Name, kind))
	}

	return errs
}

func (c Config) validateMirrorTo(i int, t Tracker) []error {
	var errs []error

	for _, name := range t.MirrorTo {
		if name == t.Name {
			errs = append(errs, fmt.Errorf("tracker %q: mirror_to names itself", t.Name))

			continue
		}

		target, ok := c.TrackerByName(name)
		if !ok {
			errs = append(errs, fmt.Errorf("trackers[%d].mirror_to names %q, which is not a configured tracker%s",
				i, name, yamlBooleanHint(name)))

			continue
		}

		if err := destinationReady(target, fmt.Sprintf("tracker %q's mirror_to", t.Name)); err != nil {
			errs = append(errs, err)
		}
	}

	return errs
}

// destinationReady checks that a tracker named in a destination list can take a create:
// it is writable, and says where creates land.
//
// A read-only tracker as a destination is refused rather than ignored: the operator who
// named it believes work will be ticketed there, and nothing ever would be.
func destinationReady(t Tracker, listedIn string) error {
	if len(t.WritableScopes) == 0 {
		return fmt.Errorf("tracker %q is a destination (named in %s), but has no writable_scopes, "+
			"so nothing could be ticketed there", t.Name, listedIn)
	}

	if t.DefaultScope == "" {
		return fmt.Errorf("tracker %q: default_scope is required, since it is writable and named in %s",
			t.Name, listedIn)
	}

	return nil
}

func (c Config) validateDefaultTicketIn() []error {
	var errs []error

	for _, name := range c.DefaultTicketIn {
		t, ok := c.TrackerByName(name)
		if !ok {
			errs = append(errs, fmt.Errorf("default_ticket_in names %q, which is not a configured tracker%s",
				name, yamlBooleanHint(name)))

			continue
		}

		if err := destinationReady(t, "default_ticket_in"); err != nil {
			errs = append(errs, err)
		}
	}

	return errs
}

// validateScopeOverlap refuses two trackers claiming one scope, so every issue routes to
// exactly one tracker. Kinds that share a key syntax share a key space.
func (c Config) validateScopeOverlap(kinds map[string]ConnectionKind) []error {
	var errs []error

	for i, a := range c.Trackers {
		for _, b := range c.Trackers[i+1:] {
			ka, okA := kinds[a.Connection]
			kb, okB := kinds[b.Connection]
			if !okA || !okB || ka.scopeSyntax() != kb.scopeSyntax() {
				continue
			}

			for _, sa := range a.Scopes {
				for _, sb := range b.Scopes {
					if scopesOverlap(sa, sb) {
						errs = append(errs, fmt.Errorf(
							"trackers %q and %q overlap: scope %q and scope %q can match the same issue, and "+
								"every issue must route to exactly one tracker", a.Name, b.Name, sa, sb))
					}
				}
			}
		}
	}

	return errs
}

// validateQueryNames refuses one query name used twice on a connection. The Jira
// collector's cursor is keyed by (connection, query), so two such queries would share a
// watermark and each would reset the other's.
func (c Config) validateQueryNames(kinds map[string]ConnectionKind) []error {
	var errs []error

	seen := make(map[[2]string]string)

	for _, t := range c.Trackers {
		if kinds[t.Connection] != KindJira {
			continue
		}

		for _, q := range t.Queries {
			key := [2]string{t.Connection, q.Name}
			if owner, dup := seen[key]; dup {
				errs = append(errs, fmt.Errorf(
					"query name %q is used twice on connection %q (trackers %q and %q): the collector's "+
						"cursor is keyed by connection and query name, so rename one", q.Name, t.Connection, owner, t.Name))

				continue
			}

			seen[key] = t.Name
		}
	}

	return errs
}
