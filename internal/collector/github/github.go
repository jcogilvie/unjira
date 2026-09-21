// Package github collects GitHub pull-request lifecycle events into the
// event log, as a second work-evidence source alongside claude_code — never
// a tracker record in the deployments this slice supports, because a pull
// request is a thing somebody did, not a work item in a tracker. See
// docs/superpowers/specs/2026-09-17-github-collector-design.md.
//
// PR lifecycle only, in this slice: :opened once at creation, and one
// :merged/:closed event per matching issue-events timeline entry once a PR
// leaves the open state (see events.go's CompletionEvents for why a merge
// legitimately produces both a :merged and a :closed event). No reviews, no
// CI, no releases, no direct-push commits — the design's §2 says why, per
// artifact.
package github

import (
	"errors"
	"fmt"
	"time"

	ghclient "github.com/jcogilvie/unjira/internal/clients/github"
	"github.com/jcogilvie/unjira/internal/credentials"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/pipeline"
)

// Name is this collector's registration name, the cursors-table collector
// key, and the Source on every event it emits.
const Name = "github"

// DefaultBackfillDays is how far back a repo is scanned on its first-ever
// collection pass (no existing cursor row), mirroring collector/claudecode's
// own option name and default shape. A GitHub-specific default of 30 (vs.
// claudecode's 14) is proposed because PRs often stay open longer than a
// Claude Code session backfill window needs to reach — but this is genuinely
// a guess, not a measurement, exactly as the design doc's own open questions
// section says: no real repo's PR-lifetime distribution has been measured
// against it.
const DefaultBackfillDays = 30

// API is the subset of *clients/github.Client this collector needs,
// declared as an interface so a test can substitute a fake without a real
// HTTP server — the design's own instruction (§9): "Collector unit tests —
// a fake github.Client (canned []PullRequest per call, no subprocess)".
type API interface {
	ListPullRequests(ref ghclient.RepoRef, since time.Time) ([]ghclient.PullRequest, error)
	ListIssueEvents(ref ghclient.RepoRef, number int) ([]ghclient.IssueEvent, error)
}

// Compile-time proof the real client satisfies API.
var _ API = (*ghclient.Client)(nil)

// ClientFactory builds an API for one host, given its base URL (see
// ghclient.BaseURL) and the credential token registered for that host.
type ClientFactory func(base, token string) (API, error)

// defaultClientFactory wraps clients/github.New, returning it as the API
// interface.
func defaultClientFactory(base, token string) (API, error) {
	return ghclient.New(base, token)
}

// Collector reads GitHub pull-request activity for every configured repo.
type Collector struct {
	newClient ClientFactory
}

// New builds the collector against the real GitHub client. Otherwise
// stateless, like collector/jira's: everything it needs arrives on the
// CollectContext.
func New() *Collector {
	return &Collector{newClient: defaultClientFactory}
}

// NewWithClientFactory builds a Collector using a custom ClientFactory —
// constructor injection per docs/go-conventions.md, for a test substituting
// a fake API without any network.
func NewWithClientFactory(factory ClientFactory) *Collector {
	return &Collector{newClient: factory}
}

// Name identifies this collector in the registry, the cursors table, and the
// Source field of every event it emits.
func (c *Collector) Name() string { return Name }

// Collect walks every configured repo.
//
// Failure is per repo, not per pass, mirroring collector/jira's per-query
// isolation: a transient failure or a revoked credential on one repo must
// not stop an unrelated repo's collection from progressing. Failed repos are
// accumulated and returned together, and their watermarks stay put so the
// next run retries that range automatically.
func (c *Collector) Collect(cc pipeline.CollectContext, visit func(events.Event)) error {
	repos := stringSliceOption(cc.Options, "repos")
	backfillDays := intOption(cc.Options, "backfill_days", DefaultBackfillDays)

	var failures []error

	for _, raw := range repos {
		if err := c.collectRepo(cc, raw, backfillDays, visit); err != nil {
			failures = append(failures, fmt.Errorf("repo %q: %w", raw, err))
		}
	}

	return errors.Join(failures...)
}

// collectRepo builds a client for one repo's host and collects it.
func (c *Collector) collectRepo(
	cc pipeline.CollectContext, raw string, backfillDays int, visit func(events.Event),
) error {
	ref, err := ghclient.ParseRepoRef(raw)
	if err != nil {
		return err
	}

	cred, ok := cc.GitHubCredentials.For(ref.Host)
	if !ok {
		return fmt.Errorf(
			"no credential for host %q: set it in %s", ref.Host, credentials.GitHubEnvVar,
		)
	}

	client, err := c.newClient(ghclient.BaseURL(ref.Host), cred.Token)
	if err != nil {
		return fmt.Errorf("building github client for host %q: %w", ref.Host, err)
	}

	resource := CursorResource(ref)

	position, err := cc.Store.GetCursor(Name, resource)
	if err != nil {
		return err
	}

	since, ok := DecodePosition(position)
	if !ok {
		// No usable cursor (first-ever pass for this repo, or a corrupted
		// row): backfill from backfillDays rather than collecting the repo's
		// entire history.
		since = time.Now().UTC().AddDate(0, 0, -backfillDays)
	}

	prs, err := client.ListPullRequests(ref, since)
	if err != nil {
		return err
	}

	var highest time.Time

	for _, pr := range prs {
		if err := c.collectPR(client, ref, pr, visit); err != nil {
			// One PR's failure fails its repo. Advancing past a PR we could
			// not fully read would step over it permanently — the same
			// all-or-nothing-per-scope discipline collector/jira's
			// collectQuery uses for one issue's failure.
			return fmt.Errorf("PR %s#%d: %w", ref.OwnerRepo(), pr.Number, err)
		}

		if pr.UpdatedAt.After(highest) {
			highest = pr.UpdatedAt
		}
	}

	if highest.IsZero() {
		// Nothing matched; leave the existing watermark alone rather than
		// writing a zero one.
		return nil
	}

	// The watermark advances only after this repo's list-and-fetch completes
	// without error, to the max updated_at observed — mirroring the Jira
	// collector's per-query advance, scoped here to per-repo (this design has
	// no query-hash component: there is no operator-authored query to
	// invalidate, only a fixed repos[] list).
	return cc.Store.SetCursor(Name, resource, EncodePosition(highest))
}

// collectPR emits every event for one PR: its :opened event (always — a PR
// this collector has never seen mints one regardless of how old it is,
// matching the Jira collector's "the watermark bounds which issues are
// fetched, never which events are emitted for one that matched" discipline),
// then its :merged/:closed events once it has left the open state.
func (c *Collector) collectPR(client API, ref ghclient.RepoRef, pr ghclient.PullRequest, visit func(events.Event)) error {
	visit(OpenedEvent(ref, pr))

	if !pr.IsClosed() {
		return nil
	}

	timeline, err := client.ListIssueEvents(ref, pr.Number)
	if err != nil {
		return fmt.Errorf("issue events: %w", err)
	}

	completion, err := CompletionEvents(ref, pr, timeline)
	if err != nil {
		return err
	}

	for _, evt := range completion {
		visit(evt)
	}

	return nil
}

// stringSliceOption and intOption mirror collector/claudecode's own
// identically-named, identically-shaped helpers for reading a collector's
// options map (a collector's own config block is unstructured
// map[string]any, decoded from JSON — see config.Config.Collectors). Not
// shared between the two collector packages: docs/go-conventions.md's
// package-layout section keeps each client/collector package self-contained,
// and this is four lines each, not worth an internal/optutil seam for two
// callers.
func stringSliceOption(options map[string]any, key string) []string {
	raw, ok := options[key]
	if !ok {
		return nil
	}

	switch v := raw.(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}

		return out
	default:
		return nil
	}
}

func intOption(options map[string]any, key string, fallback int) int {
	switch v := options[key].(type) {
	case int:
		return v
	case float64:
		return int(v)
	default:
		return fallback
	}
}
