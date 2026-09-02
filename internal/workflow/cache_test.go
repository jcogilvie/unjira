package workflow_test

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/workflow"
)

// queueProvider is a workflow.GraphProvider whose WorkflowGraph returns the
// next graph off a queue, sticking on the last one once exhausted. Tests use
// distinguishable graphs (a different status set per slot) so an assertion
// can prove "this result came from mining call N" by checking WHICH graph
// came back, rather than counting how many times WorkflowGraph was called —
// the state-not-call-count style docs/go-conventions.md and this PR's own
// task both call for.
type queueProvider struct {
	graphs []*workflow.Graph
	err    error
	next   int
}

func (p *queueProvider) WorkflowGraph(_ string) (*workflow.Graph, error) {
	if p.err != nil {
		return nil, p.err
	}

	g := p.graphs[p.next]
	if p.next < len(p.graphs)-1 {
		p.next++
	}

	return g, nil
}

// graphNamed builds a one-status graph whose StatusCategories distinguishes
// it from any other graphNamed value, so tests can tell which mine produced
// a given result by inspecting the returned *Graph's content.
func graphNamed(status string) *workflow.Graph {
	g := workflow.NewGraph()
	g.AddStatus(status, "unknown")

	return g
}

// fixedClock returns a workflow.CacheOptions.Now func pinned to t, advancing
// by delta on every call after the first — enough to model "seed the cache
// at t0, then ask again at t0+delta" without a real sleep, per this repo's
// convention of injecting a clock rather than sleeping past a TTL.
func fixedClock(start time.Time) func() time.Time {
	now := start
	return func() time.Time {
		return now
	}
}

func TestCached_NoExistingCacheMinesAndReportsFreshMine(t *testing.T) {
	dir := t.TempDir()
	provider := &queueProvider{graphs: []*workflow.Graph{graphNamed("A")}}

	graph, status, err := workflow.Cached(provider, "PROJ", workflow.CacheOptions{Dir: dir})

	require.NoError(t, err)
	assert.False(t, status.Cached, "nothing was on disk yet; this must be a fresh mine")
	assert.Zero(t, status.Age)
	assert.Equal(t, map[string]string{"A": "unknown"}, graph.StatusCategories())
}

// TestCached_SecondCallWithinTTLServesTheCachedGraph is the headline case:
// a second call, same instant, must be served from disk rather than mining
// again. Proven by state, not a call counter — the provider's second slot
// holds a DIFFERENT graph, so if the second Cached call returned it, the
// cache was bypassed; getting graph "A" back both times is the only way this
// assertion passes.
func TestCached_SecondCallWithinTTLServesTheCachedGraph(t *testing.T) {
	dir := t.TempDir()
	provider := &queueProvider{graphs: []*workflow.Graph{graphNamed("A"), graphNamed("B")}}
	opts := workflow.CacheOptions{Dir: dir, TTL: time.Hour, Now: fixedClock(time.Now())}

	_, first, err := workflow.Cached(provider, "PROJ", opts)
	require.NoError(t, err)
	require.False(t, first.Cached)

	graph, second, err := workflow.Cached(provider, "PROJ", opts)

	require.NoError(t, err)
	assert.True(t, second.Cached, "a fresh cache within its TTL must be served, not re-mined")
	assert.Equal(t, map[string]string{"A": "unknown"}, graph.StatusCategories(),
		"getting graph B here would mean the second call re-mined instead of reading the cache")
}

// TestCached_AgeReflectsElapsedTimeSinceTheMine asserts the exact reported
// age, using an injected clock (not time.Sleep) so the test is deterministic
// and fast regardless of TTL size — this repo's convention for anything
// TTL-shaped (see internal/store.TryAcquire's now-as-parameter tests).
func TestCached_AgeReflectsElapsedTimeSinceTheMine(t *testing.T) {
	dir := t.TempDir()
	provider := &queueProvider{graphs: []*workflow.Graph{graphNamed("A")}}
	start := time.Now()

	callAt := start
	now := func() time.Time { return callAt }

	_, _, err := workflow.Cached(provider, "PROJ", workflow.CacheOptions{Dir: dir, TTL: time.Hour, Now: now})
	require.NoError(t, err)

	callAt = start.Add(30 * time.Minute)
	_, status, err := workflow.Cached(provider, "PROJ", workflow.CacheOptions{Dir: dir, TTL: time.Hour, Now: now})

	require.NoError(t, err)
	assert.True(t, status.Cached)
	assert.Equal(t, 30*time.Minute, status.Age)
}

// TestCached_PastTTLReMines is the other half of the TTL trigger: once the
// cached graph is older than TTL, Cached must mine again rather than keep
// serving stale data forever.
func TestCached_PastTTLReMines(t *testing.T) {
	dir := t.TempDir()
	provider := &queueProvider{graphs: []*workflow.Graph{graphNamed("A"), graphNamed("B")}}
	start := time.Now()
	callAt := start
	now := func() time.Time { return callAt }

	_, _, err := workflow.Cached(provider, "PROJ", workflow.CacheOptions{Dir: dir, TTL: time.Hour, Now: now})
	require.NoError(t, err)

	callAt = start.Add(2 * time.Hour)
	graph, status, err := workflow.Cached(provider, "PROJ", workflow.CacheOptions{Dir: dir, TTL: time.Hour, Now: now})

	require.NoError(t, err)
	assert.False(t, status.Cached, "a cache older than its TTL must trigger a re-mine")
	assert.Equal(t, map[string]string{"B": "unknown"}, graph.StatusCategories(),
		"graph B is what proves the second mine actually ran")
}

// TestCached_RefreshForcesAReMineEvenWithinTTL covers the explicit
// invalidation trigger (`dev workflow --refresh` in cmd/unjira): even a
// brand-new, well-within-TTL cache must be bypassed when the caller asks for
// a forced refresh.
func TestCached_RefreshForcesAReMineEvenWithinTTL(t *testing.T) {
	dir := t.TempDir()
	provider := &queueProvider{graphs: []*workflow.Graph{graphNamed("A"), graphNamed("B")}}
	opts := workflow.CacheOptions{Dir: dir, TTL: time.Hour}

	_, _, err := workflow.Cached(provider, "PROJ", opts)
	require.NoError(t, err)

	opts.Refresh = true
	graph, status, err := workflow.Cached(provider, "PROJ", opts)

	require.NoError(t, err)
	assert.False(t, status.Cached, "--refresh must bypass a fresh cache, not just an expired one")
	assert.Equal(t, map[string]string{"B": "unknown"}, graph.StatusCategories())
}

// TestCached_CorruptCacheFileFallsBackToMiningAndLogsIt is the "never fail a
// caller because the cache is unusable" requirement: a cache is an
// optimization, so garbage on disk must degrade to a mine, not an error —
// but silently swallowing that fact would violate the repo's "never
// silently drop data" invariant, so it must reach the log too.
func TestCached_CorruptCacheFileFallsBackToMiningAndLogsIt(t *testing.T) {
	dir := t.TempDir()
	path := workflow.CachePath(dir, "PROJ")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte("not json at all"), 0o600))

	var logged strings.Builder
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	provider := &queueProvider{graphs: []*workflow.Graph{graphNamed("A")}}

	graph, status, err := workflow.Cached(provider, "PROJ", workflow.CacheOptions{Dir: dir})

	require.NoError(t, err, "a corrupt cache must never fail the caller")
	assert.False(t, status.Cached)
	assert.Equal(t, map[string]string{"A": "unknown"}, graph.StatusCategories())
	assert.Contains(t, logged.String(), "corrupt",
		"the fallback must be visible in the log, per the never-silently-drop-data invariant")
}

// TestCached_UnreadableCacheFileFallsBackToMiningAndLogsIt covers the sibling
// failure mode to corrupt JSON: a path that cannot even be read (here, a
// directory sitting where a file is expected) must degrade the same way —
// fall back, log, never fail the caller.
func TestCached_UnreadableCacheFileFallsBackToMiningAndLogsIt(t *testing.T) {
	dir := t.TempDir()
	path := workflow.CachePath(dir, "PROJ")
	require.NoError(t, os.MkdirAll(path, 0o750)) // a directory, not a file, at the cache path

	var logged strings.Builder
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	provider := &queueProvider{graphs: []*workflow.Graph{graphNamed("A")}}

	graph, status, err := workflow.Cached(provider, "PROJ", workflow.CacheOptions{Dir: dir})

	require.NoError(t, err, "an unreadable cache must never fail the caller")
	assert.False(t, status.Cached)
	assert.Equal(t, map[string]string{"A": "unknown"}, graph.StatusCategories())
	assert.Contains(t, logged.String(), "unreadable",
		"the fallback must be visible in the log, per the never-silently-drop-data invariant")
}

// TestCached_UnwritableCacheDirDoesNotFailTheCaller is the write-side of the
// same "a cache is an optimization" rule: if persisting the freshly-mined
// graph fails (here, because a file already occupies the directory slot
// MkdirAll needs), the caller must still get the graph it asked for — it
// only loses the speedup on the next call, never the result of this one.
func TestCached_UnwritableCacheDirDoesNotFailTheCaller(t *testing.T) {
	dir := t.TempDir()
	// Occupy the cache directory's own path with a plain file, so
	// os.MkdirAll(dir, ...) fails when Cached tries to persist.
	blocked := filepath.Join(dir, "blocked")
	require.NoError(t, os.WriteFile(blocked, []byte("x"), 0o600))

	provider := &queueProvider{graphs: []*workflow.Graph{graphNamed("A")}}

	graph, status, err := workflow.Cached(provider, "PROJ", workflow.CacheOptions{Dir: filepath.Join(blocked, "nested")})

	require.NoError(t, err, "a failure to persist the cache must not fail the caller")
	assert.False(t, status.Cached)
	assert.Equal(t, map[string]string{"A": "unknown"}, graph.StatusCategories())
}

// TestCached_MiningFailurePropagatesWrapped is the case none of the
// tolerance above should apply to: if there is no usable cache AND mining
// itself fails, that failure is real and must reach the caller, named to the
// project it was mining, per this repo's error-wrapping convention.
func TestCached_MiningFailurePropagatesWrapped(t *testing.T) {
	dir := t.TempDir()
	provider := &queueProvider{err: assert.AnError}

	_, _, err := workflow.Cached(provider, "PAAS", workflow.CacheOptions{Dir: dir})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "PAAS")
	assert.ErrorIs(t, err, assert.AnError)
}

// TestMarkDirty_NoExistingCacheIsANoOp: nothing is stale if nothing has been
// mined yet, so marking a never-cached project dirty must not be an error —
// a future caller (the per-hop live-transition check landing with the
// named-status-transition work) may call this speculatively without first
// checking whether a cache exists.
func TestMarkDirty_NoExistingCacheIsANoOp(t *testing.T) {
	dir := t.TempDir()

	err := workflow.MarkDirty(dir, "PROJ")

	assert.NoError(t, err)
}

// TestMarkDirty_ForcesAReMineOnTheNextCachedCallRegardlessOfTTL is the
// rejection-triggered invalidation trigger from the design spec: "when a
// live transition attempt is refused for a target the graph predicted was
// reachable, the graph is wrong. Mark dirty, re-mine on the next pass." This
// asserts on the dirty flag's OBSERABLE EFFECT — the next Cached call
// re-mines even though the cache is brand new and well within TTL — rather
// than on any re-mine call count, so the test doesn't encode WHEN re-mining
// happens, only THAT a dirty cache is never served.
func TestMarkDirty_ForcesAReMineOnTheNextCachedCallRegardlessOfTTL(t *testing.T) {
	dir := t.TempDir()
	provider := &queueProvider{graphs: []*workflow.Graph{graphNamed("A"), graphNamed("B")}}
	opts := workflow.CacheOptions{Dir: dir, TTL: time.Hour}

	_, seeded, err := workflow.Cached(provider, "PROJ", opts)
	require.NoError(t, err)
	require.False(t, seeded.Cached)

	require.NoError(t, workflow.MarkDirty(dir, "PROJ"))

	graph, status, err := workflow.Cached(provider, "PROJ", opts)

	require.NoError(t, err)
	assert.False(t, status.Cached, "a dirty cache must never be served, even seconds after being mined")
	assert.Equal(t, map[string]string{"B": "unknown"}, graph.StatusCategories())
}

// TestMarkDirty_UnrelatedProjectsCacheIsUntouched proves the dirty flag is
// scoped per project — a real hazard given every project's cache lives in
// one shared directory: marking PROJ dirty must not affect PAAS's own fresh
// cache.
func TestMarkDirty_UnrelatedProjectsCacheIsUntouched(t *testing.T) {
	dir := t.TempDir()
	provider := &queueProvider{graphs: []*workflow.Graph{graphNamed("A"), graphNamed("SHOULD-NOT-APPEAR")}}
	opts := workflow.CacheOptions{Dir: dir, TTL: time.Hour}

	_, _, err := workflow.Cached(provider, "PAAS", opts)
	require.NoError(t, err)

	require.NoError(t, workflow.MarkDirty(dir, "PROJ")) // a different project, never cached

	graph, status, err := workflow.Cached(provider, "PAAS", opts)

	require.NoError(t, err)
	assert.True(t, status.Cached, "marking a different project dirty must not evict PAAS's own cache")
	assert.Equal(t, map[string]string{"A": "unknown"}, graph.StatusCategories())
}

// TestMarkDirty_CorruptCacheIsANoOpNotAnError: there is nothing coherent to
// mark dirty in a file that isn't valid cache JSON, and Cached's own
// tolerance already re-mines on the very next read regardless — so this
// must degrade gracefully rather than surface a confusing error about a
// cache the caller (a future transition-validation path) doesn't otherwise
// care about the internals of.
func TestMarkDirty_CorruptCacheIsANoOpNotAnError(t *testing.T) {
	dir := t.TempDir()
	path := workflow.CachePath(dir, "PROJ")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte("not json"), 0o600))

	err := workflow.MarkDirty(dir, "PROJ")

	assert.NoError(t, err)
}

// TestCachePath_DistinctProjectKeysNeverCollideEvenWhenSanitizedTheSame
// covers the requirement that a project/repo key containing filesystem-
// hostile characters (workflow.GraphProvider's own doc names GitHub-style
// "owner/repo" keys as a real future case, and Jira itself allows
// underscores on server/DC) must not silently collide with a different key
// that sanitizes to the same safe string, and must never let the key escape
// the cache directory via a path-traversal-shaped value.
func TestCachePath_DistinctProjectKeysNeverCollideEvenWhenSanitizedTheSame(t *testing.T) {
	dir := t.TempDir()

	a := workflow.CachePath(dir, "owner/repo")
	b := workflow.CachePath(dir, "owner-repo") // sanitizes to the same safe prefix as "owner/repo"

	assert.NotEqual(t, a, b, "two different keys sanitizing to the same string must not share a cache file")
	assert.Equal(t, dir, filepath.Dir(a))
	assert.Equal(t, dir, filepath.Dir(b))
}

// TestCachePath_TraversalShapedKeyStaysInsideTheCacheDir is the security-
// adjacent half of the same requirement: a key built to look like a path
// traversal must not let CachePath escape dir.
func TestCachePath_TraversalShapedKeyStaysInsideTheCacheDir(t *testing.T) {
	dir := t.TempDir()

	path := workflow.CachePath(dir, "../../etc/passwd")

	assert.Equal(t, dir, filepath.Dir(path), "a hostile key must resolve to a plain file directly inside dir")
}
