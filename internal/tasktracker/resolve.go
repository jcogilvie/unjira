package tasktracker

import (
	"errors"
	"fmt"
	"slices"
	"sync"
)

// ErrNotFound is what a backend wraps when the issue genuinely does not exist, as
// opposed to the backend being unreachable. See IsTransportError.
var ErrNotFound = errors.New("issue not found")

// transportClassifier is how a backend's own error type says which side of
// IsTransportError it falls on. Each backend classifies its own errors, so this package
// never learns a backend's error shapes and the correlator never imports a backend.
type transportClassifier interface {
	error
	IsTransport() bool
}

// UnroutedError is a key no configured tracker's scopes cover. It is not a transport
// failure: retrying cannot change the answer, only a config change can.
type UnroutedError struct {
	Key   string
	Scope string
}

func (e *UnroutedError) Error() string {
	return fmt.Sprintf("issue %s: no configured tracker's scopes cover %q", e.Key, e.Scope)
}

// IsTransport reports false: an unrouted key is a config fact, not an outage.
func (e *UnroutedError) IsTransport() bool { return false }

// IsTransportError reports whether err means the tracker itself was unreachable — so
// the caller should fail the work for a retry next pass — rather than that the key
// genuinely does not resolve (ErrNotFound, an UnroutedError, or a backend error whose
// IsTransport reports false).
//
// Getting this backwards in either direction is a real failure mode. Misclassifying an
// outage as "not found" drops a matching candidate permanently once a sibling lets the
// narrative acquire a primary. Misclassifying a deleted ticket as transport fails the
// narrative every pass forever, since the same key is re-derived from the same events.
//
// An error of no recognized shape defaults to transport (true): a spurious retry costs a
// deferred pass, while a spurious drop costs a candidate that never comes back.
func IsTransportError(err error) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, ErrNotFound) {
		return false
	}

	if c, ok := errors.AsType[transportClassifier](err); ok {
		return c.IsTransport()
	}

	return true
}

// SelfIdentifier is a backend that can say who unjira is on it: Jira's accountId, from
// Myself(). Every writer must also be one. When a collector's system is the tracker,
// unjira's own writes come back through that collector on the next pass, and a backend
// that cannot name unjira's identity leaves them indistinguishable from new work, so
// unjira would narrate its own output back at itself (F28).
type SelfIdentifier interface {
	SelfIdentity() (string, error)
}

// Route is one configured tracker as the resolver sees it: what it is called, which
// scopes route to it, which of those it may write, and how to open its backend.
type Route struct {
	Tracker string
	// Scopes and WritableScopes are configured scope patterns, matched by ScopeMatches.
	Scopes         []string
	WritableScopes []string
	// OpenReader builds the backend's reader. Called at most once successfully.
	OpenReader func() (TaskReader, error)
	// OpenWriter builds the backend's writer, or is nil for a backend with none
	// (GitHub). With no OpenWriter a route has no writer to give, whatever
	// WritableScopes lists.
	OpenWriter func() (TaskWriter, error)
}

// Resolution is where one key or scope routes.
type Resolution struct {
	// Tracker is the configured tracker's name.
	Tracker string
	// Scope is the key's scope, as parsed (repository scopes lower-cased).
	Scope  string
	Reader TaskReader
	// Writer is nil unless the scope is writable on a route that has a writer. Holding a
	// Resolution with a Writer is write authority; see TaskWriter.
	Writer TaskWriter
	// Fallback is true when no tracker's scopes cover the key and it was read through
	// the read fallback instead. A fallback resolution never carries a Writer.
	Fallback bool
}

// Resolver routes an issue key to the one tracker that owns it. Routing is a pure
// function of the key and the configured routes; backends are opened lazily, once each,
// so routing itself does no I/O.
//
// The one resolver for matching, the reconciler and the applier.
type Resolver struct {
	mu       sync.Mutex
	routes   []*openRoute
	fallback *openRoute
}

// openRoute memoizes a Route's backends.
type openRoute struct {
	Route

	reader TaskReader
	writer TaskWriter
}

// NewResolver builds a resolver over routes. Overlapping scopes are config's to refuse
// (config.ValidateTrackers); the resolver takes the first route that matches.
func NewResolver(routes ...Route) *Resolver {
	r := &Resolver{}
	for _, route := range routes {
		r.routes = append(r.routes, &openRoute{Route: route})
	}

	return r
}

// WithReadFallback names the route that READS a key no route's scopes cover, when the
// key has that route's syntax. It is never a writer.
//
// It exists to keep the behaviour from before routing: a ticket in a project no tracker
// lists is still verified, so the work on it stays linked rather than reaching the create
// path as untracked work, which would propose a duplicate of a ticket that exists. A
// write to it is refused as untracked, as it was. An unknown name leaves no fallback.
func (r *Resolver) WithReadFallback(tracker string) *Resolver {
	r.mu.Lock()
	defer r.mu.Unlock()

	i := slices.IndexFunc(r.routes, func(o *openRoute) bool { return o.Tracker == tracker })
	if i >= 0 {
		r.fallback = r.routes[i]
	}

	return r
}

// Location is where a key routes, decided without opening any backend.
type Location struct {
	Tracker string
	Scope   string
	// Writable is whether a Resolve would hand out a writer.
	Writable bool
	// Fallback is true when only the read fallback covers the key.
	Fallback bool
}

// Locate routes issueKey without opening any backend: pure, so a startup report can
// check every stored key without building a client. An unrouted key is an
// *UnroutedError, as with Resolve.
func (r *Resolver) Locate(issueKey string) (Location, error) {
	ref, err := ParseIssueKey(issueKey)
	if err != nil {
		return Location{}, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	_, loc, err := r.locate(issueKey, ref.Scope, ref.Syntax)

	return loc, err
}

// Resolve routes issueKey: parse its scope by syntax, then find the route whose scopes
// contain it. A malformed key is an error; a key no route covers is an *UnroutedError,
// unless the read fallback serves it.
func (r *Resolver) Resolve(issueKey string) (Resolution, error) {
	ref, err := ParseIssueKey(issueKey)
	if err != nil {
		return Resolution{}, err
	}

	return r.resolve(issueKey, ref.Scope, ref.Syntax)
}

// ResolveScope routes a scope directly, for a create, which has no key yet. The read
// fallback never serves a scope: a create in an untracked scope is refused.
func (r *Resolver) ResolveScope(scope string) (Resolution, error) {
	return r.resolve(scope, scope, 0)
}

// locate finds the route for scope. syntax 0 means "a bare scope", which no route has,
// so the fallback never serves one. Callers hold r.mu.
func (r *Resolver) locate(key, scope string, syntax KeySyntax) (*openRoute, Location, error) {
	i := slices.IndexFunc(r.routes, func(o *openRoute) bool {
		return slices.ContainsFunc(o.Scopes, func(p string) bool { return ScopeMatches(p, scope) })
	})

	if i < 0 {
		if r.fallback == nil || !r.fallback.hasSyntax(syntax) {
			return nil, Location{}, &UnroutedError{Key: key, Scope: scope}
		}

		return r.fallback, Location{Tracker: r.fallback.Tracker, Scope: scope, Fallback: true}, nil
	}

	route := r.routes[i]
	writable := route.OpenWriter != nil &&
		slices.ContainsFunc(route.WritableScopes, func(p string) bool { return ScopeMatches(p, scope) })

	return route, Location{Tracker: route.Tracker, Scope: scope, Writable: writable}, nil
}

func (r *Resolver) resolve(key, scope string, syntax KeySyntax) (Resolution, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	route, loc, err := r.locate(key, scope, syntax)
	if err != nil {
		return Resolution{}, err
	}

	reader, err := route.openReader()
	if err != nil {
		return Resolution{}, err
	}

	res := Resolution{Tracker: loc.Tracker, Scope: scope, Reader: reader, Fallback: loc.Fallback}

	if loc.Writable {
		writer, err := route.openWriter()
		if err != nil {
			return Resolution{}, err
		}

		res.Writer = writer
	}

	return res, nil
}

// CheckWriters refuses any route with writable scopes whose writer cannot report
// unjira's own identity (SelfIdentifier). Run at startup, before any pass, so the
// misconfiguration fails loudly rather than when unjira first reads its own writes
// back as work. A read-only route has no writes to echo and is not checked.
//
// It opens each writable route's writer, which builds a client but makes no request:
// the requirement is the capability, checked by type.
func (r *Resolver) CheckWriters() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	var errs []error

	for _, route := range r.routes {
		if len(route.WritableScopes) == 0 || route.OpenWriter == nil {
			continue
		}

		if _, err := route.openWriter(); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// hasSyntax reports whether any of the route's scopes routes keys of syntax.
func (o *openRoute) hasSyntax(syntax KeySyntax) bool {
	return slices.ContainsFunc(o.Scopes, func(s string) bool { return syntaxOfScope(s) == syntax })
}

func (o *openRoute) openReader() (TaskReader, error) {
	if o.reader != nil {
		return o.reader, nil
	}

	reader, err := o.OpenReader()
	if err != nil {
		return nil, fmt.Errorf("opening tracker %q: %w", o.Tracker, err)
	}

	o.reader = reader

	return reader, nil
}

func (o *openRoute) openWriter() (TaskWriter, error) {
	if o.writer != nil {
		return o.writer, nil
	}

	writer, err := o.OpenWriter()
	if err != nil {
		return nil, fmt.Errorf("opening tracker %q for writing: %w", o.Tracker, err)
	}

	// Never hand out a writer that cannot say who it writes as, even to a command
	// that skipped CheckWriters.
	if _, ok := writer.(SelfIdentifier); !ok {
		return nil, fmt.Errorf("tracker %q is writable, but its backend cannot report unjira's own "+
			"identity, so unjira's writes would be collected back as new work: remove writable_scopes, "+
			"or use a backend that can", o.Tracker)
	}

	o.writer = writer

	return writer, nil
}

// Routed adapts a Resolver to a TaskTracker that routes every call by its key, so a
// consumer written against TaskReader or TaskWriter routes per issue without learning
// what a tracker is.
//
// Its write methods refuse a key whose scope has no writer, naming the tracker. That is a
// second, structural layer under gate.Applier's write-scope check, not a replacement:
// the check gives the operator-facing reason, this makes the write impossible regardless.
func Routed(r *Resolver) TaskTracker {
	return routed{r: r}
}

type routed struct{ r *Resolver }

func (t routed) GetIssue(key string) (Issue, error) {
	res, err := t.r.Resolve(key)
	if err != nil {
		return Issue{}, err
	}

	return res.Reader.GetIssue(key)
}

// SearchIssues cannot be routed: a backend-flavored query names no scope.
func (t routed) SearchIssues(query string, _ int) ([]Issue, error) {
	return nil, fmt.Errorf("cannot search %q across trackers: a query names no scope to route by", query)
}

func (t routed) AvailableTransitions(key string) ([]Transition, error) {
	res, err := t.r.Resolve(key)
	if err != nil {
		return nil, err
	}

	return res.Reader.AvailableTransitions(key)
}

func (t routed) AddComment(key, text string) error {
	w, err := t.writer(key)
	if err != nil {
		return err
	}

	return w.AddComment(key, text)
}

func (t routed) SetStatus(key, targetStatus string) error {
	w, err := t.writer(key)
	if err != nil {
		return err
	}

	return w.SetStatus(key, targetStatus)
}

func (t routed) CreateIssue(scope, summary, issueType, description string, labels []string) (string, error) {
	res, err := t.r.ResolveScope(scope)
	if err != nil {
		return "", err
	}

	if res.Writer == nil {
		return "", fmt.Errorf("cannot create in %q: tracker %q does not allow writes to it", scope, res.Tracker)
	}

	return res.Writer.CreateIssue(scope, summary, issueType, description, labels)
}

func (t routed) writer(key string) (TaskWriter, error) {
	res, err := t.r.Resolve(key)
	if err != nil {
		return nil, err
	}

	if res.Writer == nil {
		return nil, fmt.Errorf("cannot write %s: tracker %q does not allow writes to %s", key, res.Tracker, res.Scope)
	}

	return res.Writer, nil
}
