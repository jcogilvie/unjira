package tasktracker_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// namedBackend is a TaskTracker that reports which backend answered, so a routing test
// can tell trackers apart.
type namedBackend struct {
	name     string
	comments []string
	created  []string
}

func (b *namedBackend) GetIssue(key string) (tasktracker.Issue, error) {
	return tasktracker.Issue{Key: key, Summary: b.name}, nil
}

func (b *namedBackend) SearchIssues(string, int) ([]tasktracker.Issue, error) { return nil, nil }

func (b *namedBackend) AvailableTransitions(string) ([]tasktracker.Transition, error) {
	return []tasktracker.Transition{{ToStatus: b.name}}, nil
}

func (b *namedBackend) AddComment(key, _ string) error {
	b.comments = append(b.comments, key)

	return nil
}

func (b *namedBackend) SetStatus(string, string) error { return nil }

func (b *namedBackend) CreateIssue(scope, _, _, _ string, _ []string) (string, error) {
	b.created = append(b.created, scope)

	return scope + "-1", nil
}

var _ tasktracker.TaskTracker = (*namedBackend)(nil)

// route builds a Route over b, opening it as both reader and writer.
func route(tracker string, b *namedBackend, scopes, writable []string) tasktracker.Route {
	return tasktracker.Route{
		Tracker:        tracker,
		Scopes:         scopes,
		WritableScopes: writable,
		OpenReader:     func() (tasktracker.TaskReader, error) { return b, nil },
		OpenWriter:     func() (tasktracker.TaskWriter, error) { return b, nil },
	}
}

func newResolver(t *testing.T) (*tasktracker.Resolver, *namedBackend, *namedBackend) {
	t.Helper()

	work, upstream := &namedBackend{name: "work"}, &namedBackend{name: "upstream"}
	ro := tasktracker.Route{
		Tracker:    "upstream",
		Scopes:     []string{"crossplane/crossplane", "crossplane-contrib/*"},
		OpenReader: func() (tasktracker.TaskReader, error) { return upstream, nil },
	}

	return tasktracker.NewResolver(route("work", work, []string{"PAAS", "DEVSBX"}, []string{"DEVSBX"}), ro), work, upstream
}

// TestResolve_RoutesByKeySyntax is routing per key syntax: a project key by its prefix,
// a GitHub key by owner/repo, including a glob scope and a differently-cased spelling.
func TestResolve_RoutesByKeySyntax(t *testing.T) {
	r, _, _ := newResolver(t)

	tests := []struct {
		key, wantTracker, wantScope string
	}{
		{"PAAS-1", "work", "PAAS"},
		{"DEVSBX-9", "work", "DEVSBX"},
		{"crossplane/crossplane#6812", "upstream", "crossplane/crossplane"},
		{"Crossplane/Crossplane#1", "upstream", "crossplane/crossplane"},
		{"crossplane-contrib/provider-aws#3", "upstream", "crossplane-contrib/provider-aws"},
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			res, err := r.Resolve(tt.key)

			require.NoError(t, err)
			assert.Equal(t, tt.wantTracker, res.Tracker)
			assert.Equal(t, tt.wantScope, res.Scope)
			issue, err := res.Reader.GetIssue(tt.key)
			require.NoError(t, err)
			assert.Equal(t, tt.wantTracker, issue.Summary, "the reader must be the routed tracker's")
		})
	}
}

// TestResolve_WriterOnlyForAWritableScope: the resolver hands out a writer for a
// writable scope and for nothing else, including a readable scope on the same tracker.
func TestResolve_WriterOnlyForAWritableScope(t *testing.T) {
	r, _, _ := newResolver(t)

	writable, err := r.Resolve("DEVSBX-1")
	require.NoError(t, err)
	assert.NotNil(t, writable.Writer)

	readable, err := r.Resolve("PAAS-1")
	require.NoError(t, err)
	assert.Nil(t, readable.Writer, "PAAS is readable, not writable")

	upstream, err := r.Resolve("crossplane/crossplane#1")
	require.NoError(t, err)
	assert.Nil(t, upstream.Writer, "a route with no writer never yields one")
}

// TestResolve_NoWriterWithoutOpenWriterEvenIfListedWritable: config refuses this shape,
// and the resolver does not rely on it — a route without a writer has none to give.
func TestResolve_NoWriterWithoutOpenWriterEvenIfListedWritable(t *testing.T) {
	b := &namedBackend{name: "gh"}
	r := tasktracker.NewResolver(tasktracker.Route{
		Tracker: "gh", Scopes: []string{"o/r"}, WritableScopes: []string{"o/r"},
		OpenReader: func() (tasktracker.TaskReader, error) { return b, nil },
	})

	res, err := r.Resolve("o/r#1")

	require.NoError(t, err)
	assert.Nil(t, res.Writer)
}

func TestResolve_UnroutedKeyIsANamedNonTransportError(t *testing.T) {
	r, _, _ := newResolver(t)

	_, err := r.Resolve("SUMO-1")

	unrouted, ok := errors.AsType[*tasktracker.UnroutedError](err)
	require.True(t, ok, "want an UnroutedError, got %v", err)
	assert.Equal(t, "SUMO", unrouted.Scope)
	assert.False(t, tasktracker.IsTransportError(err),
		"no tracker covers this key, which retrying cannot change")
}

func TestResolve_MalformedKeyErrors(t *testing.T) {
	r, _, _ := newResolver(t)

	_, err := r.Resolve("not a key")

	require.ErrorContains(t, err, "not a key")
}

// TestResolve_FallbackReadsAnUnroutedKeyButNeverWrites: an unrouted project key is read
// through the fallback route, flagged as such, and given no writer.
func TestResolve_FallbackReadsAnUnroutedKeyButNeverWrites(t *testing.T) {
	work := &namedBackend{name: "work"}
	r := tasktracker.NewResolver(route("work", work, []string{"DEVSBX"}, []string{"DEVSBX"})).
		WithReadFallback("work")

	res, err := r.Resolve("SUMO-1")

	require.NoError(t, err)
	assert.True(t, res.Fallback)
	assert.Equal(t, "work", res.Tracker)
	assert.Nil(t, res.Writer, "a fallback read is never a write authority")
	require.NotNil(t, res.Reader)

	_, err = r.Resolve("o/r#1")
	require.Error(t, err, "the fallback serves its own key syntax only")
}

func TestResolve_OpensEachBackendOnce(t *testing.T) {
	opened := 0
	b := &namedBackend{name: "work"}
	r := tasktracker.NewResolver(tasktracker.Route{
		Tracker: "work", Scopes: []string{"PAAS"},
		OpenReader: func() (tasktracker.TaskReader, error) {
			opened++

			return b, nil
		},
	})

	for range 3 {
		_, err := r.Resolve("PAAS-1")
		require.NoError(t, err)
	}

	assert.Equal(t, 1, opened, "a backend is an HTTP client with credentials: build it once")
}

func TestResolve_OpenErrorNamesTheTracker(t *testing.T) {
	r := tasktracker.NewResolver(tasktracker.Route{
		Tracker: "work", Scopes: []string{"PAAS"},
		OpenReader: func() (tasktracker.TaskReader, error) { return nil, errors.New("no credentials") },
	})

	_, err := r.Resolve("PAAS-1")

	require.Error(t, err)
	assert.Contains(t, err.Error(), `"work"`)
	assert.Contains(t, err.Error(), "no credentials")
}

// TestRouted_ReadsAndWritesPerKey: the adapter lets a TaskReader/TaskWriter consumer
// route per call without learning about trackers.
func TestRouted_ReadsAndWritesPerKey(t *testing.T) {
	r, work, _ := newResolver(t)
	routed := tasktracker.Routed(r)

	issue, err := routed.GetIssue("crossplane/crossplane#2")
	require.NoError(t, err)
	assert.Equal(t, "upstream", issue.Summary)

	transitions, err := routed.AvailableTransitions("PAAS-1")
	require.NoError(t, err)
	assert.Equal(t, "work", transitions[0].ToStatus)

	require.NoError(t, routed.AddComment("DEVSBX-1", "done"))
	assert.Equal(t, []string{"DEVSBX-1"}, work.comments)

	key, err := routed.CreateIssue("DEVSBX", "s", "Task", "d", nil)
	require.NoError(t, err)
	assert.Equal(t, "DEVSBX-1", key)
}

func TestRouted_RefusesAWriteToAScopeWithNoWriter(t *testing.T) {
	r, work, _ := newResolver(t)
	routed := tasktracker.Routed(r)

	err := routed.AddComment("PAAS-1", "done")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"work"`)
	assert.Contains(t, err.Error(), "PAAS")

	require.Error(t, routed.SetStatus("crossplane/crossplane#1", "closed"))
	_, err = routed.CreateIssue("PAAS", "s", "Task", "d", nil)
	require.Error(t, err)
	assert.Empty(t, work.comments)
	assert.Empty(t, work.created)
}

func TestRouted_SearchIsNotRoutable(t *testing.T) {
	r, _, _ := newResolver(t)

	_, err := tasktracker.Routed(r).SearchIssues("anything", 10)

	require.Error(t, err)
}

// transportish lets a test build an error that classifies itself.
type transportish struct{ transport bool }

func (e transportish) Error() string     { return fmt.Sprintf("transport=%v", e.transport) }
func (e transportish) IsTransport() bool { return e.transport }

func TestIsTransportError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"not found", fmt.Errorf("getting X-1: %w", tasktracker.ErrNotFound), false},
		{"unrouted", &tasktracker.UnroutedError{Key: "X-1", Scope: "X"}, false},
		{"backend says transport", fmt.Errorf("wrapped: %w", transportish{transport: true}), true},
		{"backend says not transport", fmt.Errorf("wrapped: %w", transportish{transport: false}), false},
		// Unrecognized errors lean transport: a spurious retry costs a deferred pass,
		// a spurious drop costs a candidate that never comes back.
		{"unrecognized", errors.New("boom"), true},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, tasktracker.IsTransportError(tt.err), tt.name)
	}
}
