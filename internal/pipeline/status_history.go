package pipeline

import "github.com/jcogilvie/unjira/internal/config"

// StatusHistorySource is an optional Collector capability. A collector
// implements it to declare that it can emit events.SetStatusChange-tagged
// events at all — the jira collector does (see internal/collector/jira's
// EventsFromChangelogEntry); internal/collector/claudecode has no concept of
// tracker status and does not implement it.
//
// This is the general seam task #174 needs: deciding whether ANY enabled
// collector can ever supply the reconciler's staleness guard
// (internal/reconciler.suppressStaleTransitions) with collected status
// history must not mean testing for the jira collector by name — that is
// exactly the mistake docs/design-notes.md incident 21 already made once
// (a reconciler that recognized only Jira's own changelog vocabulary, and
// would have silently never fired for a GitHub collector). A marker
// interface lets a future GitHub-Issues collector (or anything else that
// can observe a tracked item's status changing) answer the same question
// unjira asks of jira today, with no change to the caller.
//
// Modeled on workflow.GraphProvider: optional, type-asserted by callers
// (HasEnabledStatusHistorySource), not part of the base Collector interface
// every implementation is forced to have an opinion on.
type StatusHistorySource interface {
	// SuppliesStatusHistory is a pure marker: implementing it IS the signal,
	// and callers type-assert rather than call it.
	//
	// It returns nothing on purpose. A bool would create a state — implement
	// the method, return false — that type-asserts as "supplies history"
	// while claiming it does not, so the assertion and the value could
	// disagree about the same fact. A collector either can observe a tracked
	// item's status changing, and implements this, or cannot, and does not.
	SuppliesStatusHistory()
}

// HasEnabledStatusHistorySource reports whether any collector enabled in cfg
// and present in registry implements StatusHistorySource — i.e. whether
// anything configured to run can ever supply the staleness guard with
// collected status history, as opposed to "nothing has been collected for
// this particular issue yet" (the ordinary, per-issue case
// reconciler.FindUnguardedTransitions reports separately).
//
// Config-only and store-free by design: cmd/unjira calls this once, at
// startup, before any narrative is processed — see task #174's requirement
// that the CONFIGURATION-shaped gap be surfaced once rather than on every
// pass. An unregistered but enabled collector (RunCollect's own "-1"
// sentinel case) is skipped rather than treated as qualifying: a name with
// no factory can supply nothing.
func HasEnabledStatusHistorySource(cfg config.Config, registry map[string]func() Collector) bool {
	for name := range cfg.EnabledCollectors() {
		factory, ok := registry[name]
		if !ok {
			continue
		}

		if _, ok := factory().(StatusHistorySource); ok {
			return true
		}
	}

	return false
}
