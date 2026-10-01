// Package pipeline runs the collect pass and renders the phase-0 digest.
package pipeline

import (
	"log/slog"
	"regexp"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/credentials"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/store"
)

// CollectContext is everything a collector may need from its caller: the store
// for cursors, the loaded config, resolved credentials, and its own options
// block from config.Collectors[<name>].
//
// It exists as a struct rather than a parameter list because collectors differ
// in what they need — claude_code reads local files and uses only Store and
// Options, while a Jira or GitHub collector needs connection config and
// credentials — and because a future need can be added here without changing
// every implementation's signature again.
//
// Credentials are passed in rather than read from the environment here, so the
// "credentials come from the environment, never config files" rule stays
// enforced at one place (cmd/unjira) instead of in every collector.
type CollectContext struct {
	Store       *store.Store
	Config      config.Config
	Credentials credentials.Set
	// GitHubCredentials is a second, independently-keyed credential.Set: Jira's
	// Credentials is keyed by config connection NAME (a tenant concept — see
	// config.JiraConnection), while GitHub's is keyed by HOST
	// (github.com/a GHES instance), because GitHub is an identity provider
	// rather than a multi-tenant system the way Jira is — one token carries
	// org membership, so there is no per-connection auth surface to name. Both
	// are the same credentials.Set type; only the keyspace differs, which is
	// exactly what that type's generic map[string]Credential shape already
	// supports without a parallel type. See
	// docs/superpowers/specs/2026-09-17-github-collector-design.md §8 and
	// credentials.GitHubEnvVar.
	GitHubCredentials credentials.Set
	// Options is this collector's own block from config.Collectors[<name>],
	// including the "enabled" key that got it selected.
	Options map[string]any
	// Log is where a collector reports degradation. Nil is silent.
	Log *slog.Logger
}

// Collector reads its source since the last cursor, emits normalized
// events via visit, and advances its cursor via the store. Collectors must
// be deterministic and idempotent: re-running one is always safe because
// (source, external_id) dedupes at insert.
type Collector interface {
	Name() string
	Collect(cc CollectContext, visit func(events.Event)) error
}

// RunCollect runs every enabled collector found in registry, persisting new
// events. Returns {collector_name: new_event_count}; a collector enabled in
// config but not present in registry reports -1.
//
// linkExclusions (from config.CompiledLinkExclusions, compiled once by the
// caller) is applied to every event's events.ArtifactTicketKeys artifact
// before insert: a matching key is recorded in a new excluded_ticket_keys
// artifact, never removed from events.ArtifactTicketKeys itself. A nil/empty
// linkExclusions is a no-op.
//
// creds is passed to every collector via CollectContext.Credentials, so a
// remote collector can authenticate without reading the environment itself.
// githubCreds is the same shape but keyed by host rather than by connection
// name — see CollectContext.GitHubCredentials's own doc comment for why this
// is a second Set rather than a second key inside creds.
func RunCollect(
	cfg config.Config,
	s *store.Store,
	registry map[string]func() Collector,
	linkExclusions []*regexp.Regexp,
	creds credentials.Set,
	githubCreds credentials.Set,
	log *slog.Logger,
) (map[string]int, error) {
	results := make(map[string]int)

	for name, options := range cfg.EnabledCollectors() {
		factory, ok := registry[name]
		if !ok {
			results[name] = -1
			continue
		}

		collector := factory()
		inserted := 0
		var collectErr error

		cc := CollectContext{
			Store:             s,
			Config:            cfg,
			Credentials:       creds,
			GitHubCredentials: githubCreds,
			Options:           options,
			Log:               log,
		}

		err := collector.Collect(cc, func(event events.Event) {
			if collectErr != nil {
				return
			}

			annotateExcludedTicketKeys(event, linkExclusions)

			ok, err := s.InsertEvent(event)
			if err != nil {
				collectErr = err
				return
			}
			if ok {
				inserted++
			}
		})
		if err != nil {
			return nil, err
		}
		if collectErr != nil {
			return nil, collectErr
		}

		results[name] = inserted
	}

	return results, nil
}

// annotateExcludedTicketKeys sets event.Artifacts["excluded_ticket_keys"]
// when any of its events.ArtifactTicketKeys match a configured link-exclusion
// pattern. events.ArtifactTicketKeys itself is left untouched — nothing is
// ever removed from it.
func annotateExcludedTicketKeys(event events.Event, linkExclusions []*regexp.Regexp) {
	if len(linkExclusions) == 0 {
		return
	}

	keys := events.TicketKeysOf(event)
	if len(keys) == 0 {
		return
	}

	_, excluded := events.PartitionExcludedKeys(keys, linkExclusions)
	if len(excluded) == 0 {
		return
	}

	excludedAny := make([]any, len(excluded))
	for i, k := range excluded {
		excludedAny[i] = k
	}
	event.Artifacts["excluded_ticket_keys"] = excludedAny
}
