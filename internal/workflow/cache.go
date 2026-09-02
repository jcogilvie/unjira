package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// DefaultCacheDir is where per-project graph caches live when CacheOptions
// leaves Dir unset — a "data/"-relative location, mirroring
// config.DBPath's own "data/unjira.db" default. "data/" is already
// gitignored, and putting a derived, regenerable cache next to the SQLite
// event log rather than inventing a second top-level directory keeps
// "where does unjira keep its local state" answerable in one place.
const DefaultCacheDir = "data/workflow-cache"

// DefaultCacheTTL bounds how long a mined graph is trusted before Cached
// re-mines it, absent a configured workflow.cache_ttl (see
// internal/config.WorkflowConfig).
//
// Set generously long on purpose. Jira workflows are admin-configured and
// observed to change on the order of months, while mining took ~40s for 200
// issues in real measurement against the PAAS project — cheap once, but
// wasted work every pass if re-paid on a timer far shorter than the thing
// being cached actually changes. A too-long TTL is not a correctness risk
// here the way it would be for, say, a credential cache: the dirty flag
// (MarkDirty) is the real staleness detector, reacting to an observed
// contradiction rather than a clock, and every hop a cached graph plans is
// re-validated against the live per-issue transitions endpoint before it
// executes (see tasktracker.TaskReader.AvailableStatusCategories's own doc
// comment) — so a stale graph can misdirect planning but can never license
// an illegal write. 24h means at most one re-mine per calendar day of
// `watch` ticks, which is the number of "wasted" 40s mines this constant is
// actually trying to avoid.
const DefaultCacheTTL = 24 * time.Hour

// cacheKeyHashLength is how much of the project key's digest goes into its
// cache filename — enough to make an accidental collision between two
// sanitized-to-the-same-string keys astronomically unlikely, not a security
// boundary (matching internal/collector/jira's own jqlHashLength precedent
// and rationale).
const cacheKeyHashLength = 12

// cacheUnsafeChars matches everything CachePath does not treat as safe to
// place directly in a filename: path separators (so a GitHub-shaped
// "owner/repo" key, which workflow.GraphProvider's own doc names as a real
// future case, can't be misread as a subdirectory or escape the cache dir),
// and anything else outside a conservative alphanumeric-plus-punctuation
// set. Jira Cloud project keys are uppercase-alphanumeric only today, but
// classic/Data-Center Jira permits underscores (e.g. "PRODUCT_2013"), and
// nothing in workflow.GraphProvider's contract limits projectOrRepo to
// Jira's own rules — so this sanitizes defensively rather than assuming the
// narrower Cloud format.
var cacheUnsafeChars = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

// CachePath returns the on-disk path Cached and MarkDirty use for
// projectKey's cache, under dir (DefaultCacheDir if empty).
//
// The filename is a sanitized, human-legible prefix (mostly for a curious
// operator poking around data/workflow-cache/) plus a hash suffix of the
// UNSANITIZED key. The hash suffix is load-bearing, not decorative: two
// distinct keys that sanitize to the same safe string (e.g. "owner/repo" and
// "owner-repo") must not silently share one cache file, and the sanitized
// prefix alone cannot guarantee that. Exported so a caller wiring the CLI
// (or a test) can show or seed the exact file Cached will read.
func CachePath(dir, projectKey string) string {
	if dir == "" {
		dir = DefaultCacheDir
	}

	return filepath.Join(dir, cacheFileName(projectKey))
}

func cacheFileName(projectKey string) string {
	safe := cacheUnsafeChars.ReplaceAllString(projectKey, "_")
	if safe == "" {
		safe = "_"
	}

	sum := sha256.Sum256([]byte(projectKey))

	return fmt.Sprintf("%s-%s.json", safe, hex.EncodeToString(sum[:])[:cacheKeyHashLength])
}

// cacheEntry is the on-disk shape Cached/MarkDirty read and write: the mined
// graph (Graph.ToMap's own representation, embedded rather than
// re-invented) plus the two pieces of metadata Save/Load never needed to
// carry — when it was mined, and whether a caller has since flagged it
// stale.
type cacheEntry struct {
	MinedAt time.Time      `json:"mined_at"`
	Dirty   bool           `json:"dirty"`
	Graph   map[string]any `json:"graph"`
}

// CacheOptions configures Cached.
type CacheOptions struct {
	// Dir is the cache directory. Empty means DefaultCacheDir.
	Dir string
	// TTL bounds how long a cached graph is trusted. Zero or negative means
	// DefaultCacheTTL — the single source of truth for that default, so a
	// caller (internal/config.WorkflowConfig.CacheTTL is Span-typed and
	// zero-valued when unset) does not need to know or duplicate it.
	TTL time.Duration
	// Refresh forces a re-mine and overwrite, bypassing the cache
	// unconditionally — the `dev workflow --refresh` path.
	Refresh bool
	// Now returns the current time. Nil means time.Now. Tests inject a fixed
	// or stepped clock here rather than sleeping past a real TTL.
	Now func() time.Time
}

// CacheStatus reports how Cached obtained its graph — an operator-facing
// detail, not just an implementation curiosity: `dev workflow` prints it
// because a cache an operator can't observe is a cache they can't debug.
type CacheStatus struct {
	// Cached is true when the graph was read from disk rather than mined.
	Cached bool
	// Age is how old the cached graph was when read. Zero when Cached is
	// false.
	Age time.Duration
	// Reason names why a fresh mine happened, when Cached is false: "no
	// cache yet", "expired", "marked dirty", "refresh requested", or a
	// "cache unreadable: ..." / "cache corrupt: ..." detail. Empty when
	// Cached is true.
	Reason string
}

// Cached returns projectKey's workflow graph, mining it via provider only
// when there is no usable cache: none exists yet, it has aged past TTL, it
// was marked dirty (see MarkDirty), or opts.Refresh forces it regardless.
//
// A failure to read or persist the cache is never returned to the caller —
// a cache is an optimization, so its own unavailability degrades to mining
// rather than an error. It is still logged: per this repo's "never silently
// drop data" invariant, an operator debugging "why is this always slow"
// needs to see that the cache is unreadable, not just that mining ran.
//
// Only a genuine mining failure (provider.WorkflowGraph erroring with no
// usable cache to fall back to) reaches the caller, wrapped with
// projectKey.
func Cached(provider GraphProvider, projectKey string, opts CacheOptions) (*Graph, CacheStatus, error) {
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	ttl := opts.TTL
	if ttl <= 0 {
		ttl = DefaultCacheTTL
	}

	path := CachePath(opts.Dir, projectKey)

	reason := "refresh requested"
	if !opts.Refresh {
		graph, age, freshReason := loadIfFresh(path, ttl, now())
		if freshReason == "" {
			return graph, CacheStatus{Cached: true, Age: age}, nil
		}

		reason = freshReason
	}

	graph, err := provider.WorkflowGraph(projectKey)
	if err != nil {
		return nil, CacheStatus{}, fmt.Errorf("mining workflow graph for %s: %w", projectKey, err)
	}

	entry := cacheEntry{MinedAt: now(), Graph: graph.ToMap()}
	if err := writeCacheEntry(path, entry); err != nil {
		log.Printf("workflow: could not save cache for project %s at %s (%v); continuing without a cache",
			projectKey, path, err)
	}

	return graph, CacheStatus{Cached: false, Reason: reason}, nil
}

// loadIfFresh attempts to read a usable, non-stale graph from path. An empty
// reason means success; any non-empty reason means the caller should mine —
// "no cache yet" is the ordinary first-run case (not logged, since there is
// nothing noteworthy about a cache that was never expected to exist yet);
// an unreadable or corrupt cache IS logged, since those are real operational
// surprises an operator would otherwise never see evidence of.
func loadIfFresh(path string, ttl time.Duration, now time.Time) (*Graph, time.Duration, string) {
	body, err := os.ReadFile(path) //nolint:gosec // cache path is derived, not directly user-supplied
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, "no cache yet"
		}

		log.Printf("workflow: cache at %s is unreadable (%v); re-mining", path, err)

		return nil, 0, fmt.Sprintf("cache unreadable: %v", err)
	}

	var entry cacheEntry
	if err := json.Unmarshal(body, &entry); err != nil {
		log.Printf("workflow: cache at %s is corrupt (%v); re-mining", path, err)

		return nil, 0, fmt.Sprintf("cache corrupt: %v", err)
	}

	graph, err := GraphFromMap(entry.Graph)
	if err != nil {
		log.Printf("workflow: cache at %s has a corrupt graph payload (%v); re-mining", path, err)

		return nil, 0, fmt.Sprintf("cache corrupt: %v", err)
	}

	if entry.Dirty {
		return nil, 0, "marked dirty"
	}

	age := now.Sub(entry.MinedAt)
	if age < 0 || age >= ttl {
		return nil, 0, "expired"
	}

	return graph, age, ""
}

// MarkDirty flags projectKey's cached graph as stale, so the next Cached
// call re-mines regardless of TTL. This is the package doc's third
// invalidation trigger: "when the live transitions endpoint returns an edge
// the graph doesn't predict, mark it dirty and re-mine." A rejection means
// the graph's plan was wrong; a cache that only expires on a timer would
// stay wrong for the rest of its TTL, while this makes it self-heal on the
// very next pass.
//
// Nothing in unjira calls this yet — the caller is the per-hop live
// transition check that lands with
// docs/superpowers/specs/2026-09-01-named-status-transitions-design.md's
// SetStatus/AvailableTransitions work, not this PR. It is exercised
// directly in this package's tests so the mechanism has coverage before it
// has a caller, rather than shipping untested and unreachable.
//
// A no-op, not an error, when there is no cache yet to mark (nothing is
// stale if nothing has been mined) or when the existing cache is already
// unreadable/corrupt (Cached's own tolerance re-mines on the very next read
// regardless of the dirty flag, so there is nothing useful to persist).
func MarkDirty(dir, projectKey string) error {
	path := CachePath(dir, projectKey)

	entry, err := readCacheEntry(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}

		log.Printf("workflow: cache at %s cannot be marked dirty (%v); the next read re-mines anyway",
			path, err)

		return nil
	}

	entry.Dirty = true

	if err := writeCacheEntry(path, entry); err != nil {
		return fmt.Errorf("marking workflow cache dirty at %s: %w", path, err)
	}

	return nil
}

func readCacheEntry(path string) (cacheEntry, error) {
	body, err := os.ReadFile(path) //nolint:gosec // cache path is derived, not directly user-supplied
	if err != nil {
		return cacheEntry{}, err
	}

	var entry cacheEntry
	if err := json.Unmarshal(body, &entry); err != nil {
		return cacheEntry{}, err
	}

	return entry, nil
}

func writeCacheEntry(path string, entry cacheEntry) error {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("creating directory for %s: %w", path, err)
		}
	}

	body, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling cache entry: %w", err)
	}

	if err := os.WriteFile(path, body, 0o600); err != nil {
		return fmt.Errorf("writing workflow cache to %s: %w", path, err)
	}

	return nil
}
