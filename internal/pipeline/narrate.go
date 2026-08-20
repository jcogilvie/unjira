package pipeline

import (
	"context"
	"fmt"
	"time"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/llm"
	"github.com/jcogilvie/unjira/internal/store"
)

// NarrateOptions configures one narration pass.
type NarrateOptions struct {
	// DryRun runs the full pass — including the real LLM calls — but skips
	// Persist, so nothing is written. The reported narratives are exactly what
	// would have been persisted, with zero IDs since no rows were inserted.
	DryRun bool
}

// NarrateResult is one pass's outcome, carrying enough detail for a human to
// judge the clustering without re-querying the store.
type NarrateResult struct {
	Window            correlator.TimeRange
	UnlinkedEvents    int // clustering candidates considered
	ContextNarratives int // existing narratives passed to Cluster as context
	DryRun            bool
	Stats             correlator.Stats
	Narratives        []NarratedNarrative
	Compactions       []Compaction
}

// NarratedNarrative is one narrative this pass produced, with the member
// events that justify its grouping — the detail that makes the clustering
// judgeable rather than merely reportable.
type NarratedNarrative struct {
	Kind correlator.ClusterKind
	// ID is the persisted narrative id, or 0 under DryRun.
	ID int64
	// PriorWindowEnd is what window_end was before this pass extended it;
	// zero when Kind is ClusterNew.
	PriorWindowEnd time.Time
	WindowStart    time.Time
	WindowEnd      time.Time
	Title          string
	Summary        string
	Events         []correlator.Event
}

// Compaction records one tail-summarization, so the lossy step is visible in
// the pass output rather than only in logs.
type Compaction struct {
	NarrativeID int64
	// EventsFolded is how many events this pass's compaction folded into the
	// recap — not the narrative's lifetime total. narrative_events rows are
	// never deleted (see store.NarrativeEventsForContext), so a narrative
	// compacted more than once accumulates events that are linked but not
	// "visible" from any earlier pass too; EventsFolded must not include
	// those. See collectCompactions for the arithmetic and why a naive
	// linked-minus-visible subtraction overcounts on a second compaction.
	EventsFolded int
	Boundary     time.Time
}

// RunNarrate runs one narration pass over window: assemble Cluster's inputs
// from the store, cluster them, and (unless DryRun) persist the results.
//
// It does NOT acquire the pipeline lease. That is the caller's job, because
// the scope differs per caller: dev narrate wraps this one stage, while watch
// will wrap collect + narrate + reconcile in a single lease. Acquiring here
// would make watch contend with itself.
//
// Inputs are fetched once for the whole window and handed to Cluster as-is.
// Cluster re-derives its own in-window and adjacency filters at every
// bisection level (clusterWithSplit recurses with the full slices, narrowing
// only the window), so re-querying per sub-window would be both wrong and
// impossible — Cluster has no store by design.
func RunNarrate(
	ctx context.Context,
	s *store.Store,
	client llm.Client,
	cfg config.Config,
	window correlator.TimeRange,
	opts NarrateOptions,
) (NarrateResult, error) {
	result := NarrateResult{Window: window, DryRun: opts.DryRun}

	candidates, err := s.UnlinkedEventsInRange(window.Start, window.End)
	if err != nil {
		return NarrateResult{}, fmt.Errorf("assembling clustering candidates: %w", err)
	}
	result.UnlinkedEvents = len(candidates)

	if len(candidates) == 0 {
		// Nothing to narrate is a normal outcome. Return before spending a
		// call so an idle window costs nothing.
		return result, nil
	}

	existing, err := hydrateContextNarratives(s, window)
	if err != nil {
		return NarrateResult{}, err
	}
	result.ContextNarratives = len(existing)

	clustered, clusterStats, err := correlator.Cluster(
		ctx, candidates, existing, client, window, cfg.LLM.ContextWindowTokens)
	result.Stats.Add(clusterStats)
	if err != nil {
		return NarrateResult{}, fmt.Errorf("clustering: %w", err)
	}
	if err := requireNonEmptyClusters(clustered); err != nil {
		return NarrateResult{}, err
	}

	if opts.DryRun {
		result.Narratives = describeUnpersisted(clustered)

		return result, nil
	}

	return finishNarrate(ctx, s, client, cfg, existing, clustered, result)
}

// finishNarrate is RunNarrate's persisting tail, split out to keep
// RunNarrate's cognitive complexity down: persist, describe what was
// persisted, and collect any compactions this pass triggered.
func finishNarrate(
	ctx context.Context,
	s *store.Store,
	client llm.Client,
	cfg config.Config,
	existing []correlator.Narrative,
	clustered []correlator.ClusterResult,
	result NarrateResult,
) (NarrateResult, error) {
	// Capture pre-pass window ends so the output can show what an extend moved.
	priorEnds := make(map[int64]time.Time, len(existing))
	for _, n := range existing {
		priorEnds[n.ID] = n.WindowEnd
	}

	persisted, persistStats, err := correlator.Persist(ctx, s, client, clustered, cfg.Correlator)
	result.Stats.Add(persistStats)
	if err != nil {
		return NarrateResult{}, fmt.Errorf("persisting narratives: %w", err)
	}

	result.Narratives = describePersisted(clustered, persisted, priorEnds)

	result.Compactions, err = collectCompactions(s, existing, clustered, persisted, persistStats)
	if err != nil {
		return NarrateResult{}, err
	}

	return result, nil
}

// requireNonEmptyClusters rejects any ClusterResult with no member events.
// parseClusterResponse (internal/correlator) does not itself reject an empty
// event_indices array — a model could return one — and an empty cluster
// would otherwise reach eventWindow (dry-run path) or prepareOneResult
// (persist path) and silently produce a zero-width, zero-time window rather
// than a loud failure. Per this repo's "never silently drop data" invariant,
// that must be an error, not a quietly wrong narrative.
func requireNonEmptyClusters(clustered []correlator.ClusterResult) error {
	for _, r := range clustered {
		if len(r.Events) == 0 {
			return fmt.Errorf("clustering produced narrative %q with no member events", r.Title)
		}
	}

	return nil
}

// hydrateContextNarratives loads the narratives overlapping or touching window
// and fills each one's Events from the store, which is what makes Cluster's
// context section carry raw events rather than just summaries (Path B).
func hydrateContextNarratives(s *store.Store, window correlator.TimeRange) ([]correlator.Narrative, error) {
	rows, err := s.NarrativesOverlapping(window.Start, window.End)
	if err != nil {
		return nil, fmt.Errorf("assembling context narratives: %w", err)
	}

	out := make([]correlator.Narrative, 0, len(rows))
	for _, row := range rows {
		contextEvents, err := s.NarrativeEventsForContext(row.ID)
		if err != nil {
			return nil, fmt.Errorf("hydrating context events for narrative %d: %w", row.ID, err)
		}

		out = append(out, correlator.Narrative{
			ID:          row.ID,
			WindowStart: row.WindowStart,
			WindowEnd:   row.WindowEnd,
			Title:       row.Title,
			Summary:     row.Summary,
			IssueKey:    row.IssueKey,
			Confidence:  row.Confidence,
			Status:      row.Status,
			Events:      contextEvents,
		})
	}

	return out, nil
}

// describeUnpersisted renders dry-run results, which have no ids because
// nothing was written. Window bounds come from the clustered events
// themselves, matching what Persist would have computed.
func describeUnpersisted(clustered []correlator.ClusterResult) []NarratedNarrative {
	out := make([]NarratedNarrative, 0, len(clustered))
	for _, r := range clustered {
		lo, hi := eventWindow(r.Events)
		out = append(out, NarratedNarrative{
			Kind:        r.Kind,
			ID:          0,
			WindowStart: lo,
			WindowEnd:   hi,
			Title:       r.Title,
			Summary:     r.Summary,
			Events:      r.Events,
		})
	}

	return out
}

// describePersisted pairs each persisted narrative with the clustered result
// that produced it, so the output can show member events (which Persist's
// return does not carry) alongside real ids and window bounds (which the
// cluster result does not carry).
//
// The pairing is by index: clustered[i] <-> persisted[i]. This is verified,
// not assumed, against Persist's implementation (internal/correlator/
// correlator.go): Persist's prepareResults builds preps by ranging over
// results in order and appending each in turn, and the write transaction
// then ranges over preps in that same order, appending each touched
// Narrative in turn. Both stages preserve input order start to finish, so
// persisted[i] is always the outcome of clustered[i]. clusterKindAt still
// guards the bound rather than trusting it blindly, in case that invariant
// ever slips.
func describePersisted(
	clustered []correlator.ClusterResult,
	persisted []correlator.Narrative,
	priorEnds map[int64]time.Time,
) []NarratedNarrative {
	out := make([]NarratedNarrative, 0, len(persisted))
	for i, n := range persisted {
		narrated := NarratedNarrative{
			Kind:        clusterKindAt(clustered, i),
			ID:          n.ID,
			WindowStart: n.WindowStart,
			WindowEnd:   n.WindowEnd,
			Title:       n.Title,
			Summary:     n.Summary,
		}
		if i < len(clustered) {
			narrated.Events = clustered[i].Events
		}
		if narrated.Kind == correlator.ClusterExtends {
			narrated.PriorWindowEnd = priorEnds[n.ID]
		}
		out = append(out, narrated)
	}

	return out
}

// clusterKindAt reports the kind of the i-th cluster result. Persist returns
// narratives in the same order it received results, so index alignment holds;
// this guards the bound rather than assuming it.
func clusterKindAt(clustered []correlator.ClusterResult, i int) correlator.ClusterKind {
	if i < len(clustered) {
		return clustered[i].Kind
	}

	return correlator.ClusterNew
}

// eventWindow returns the earliest and latest OccurredAt among evts. Callers
// only reach this with a non-empty evts (requireNonEmptyClusters rejects
// empty clusters before either caller runs), so the zero-time result for an
// empty slice is unreachable in practice; the loop still degrades to that
// rather than panicking if that invariant ever slips.
func eventWindow(evts []correlator.Event) (lo, hi time.Time) {
	for i, e := range evts {
		if i == 0 || e.OccurredAt.Before(lo) {
			lo = e.OccurredAt
		}
		if i == 0 || e.OccurredAt.After(hi) {
			hi = e.OccurredAt
		}
	}

	return lo, hi
}

// collectCompactions reports which narratives this pass compacted and how
// many events each compaction folded, by reading back the boundary Persist
// just wrote.
//
// EventsFolded is deliberately not "total linked minus currently visible":
// narrative_events rows are never deleted (store.NarrativeEventsForContext),
// so a narrative already compacted once, then extended and compacted again,
// would have that subtraction count every event ever folded across every
// past compaction, not just this pass's. Instead this computes, per
// narrative: (events visible as context *before* this pass, from `existing`
// — this pass's V0) + (events this pass linked to it, from the matching
// ClusterExtends result — K) - (events visible *after* this pass — V1,
// read fresh from the store). Persist's incoming events are always
// previously-unlinked (they come from UnlinkedEventsInRange), so V0 and the
// incoming K never overlap and this sum equals exactly the count of events
// this pass's compaction moved out of the visible tail — regardless of how
// many earlier compactions already happened to this narrative. Compaction
// only ever applies to ClusterExtends (prepareOneResult in
// internal/correlator never compacts a ClusterNew, since there is no prior
// row to have a boundary), so only extends are matched against `clustered`.
func collectCompactions(
	s *store.Store,
	existing []correlator.Narrative,
	clustered []correlator.ClusterResult,
	persisted []correlator.Narrative,
	stats correlator.Stats,
) ([]Compaction, error) {
	if stats.Compactions == 0 {
		return nil, nil
	}

	priorVisible := make(map[int64]int, len(existing))
	for _, n := range existing {
		priorVisible[n.ID] = len(n.Events)
	}

	incomingCount := make(map[int64]int, len(clustered))
	for _, r := range clustered {
		if r.Kind == correlator.ClusterExtends {
			incomingCount[r.NarrativeID] += len(r.Events)
		}
	}

	var out []Compaction
	for _, n := range persisted {
		row, err := s.GetNarrative(n.ID)
		if err != nil {
			return nil, fmt.Errorf("reading compaction boundary for narrative %d: %w", n.ID, err)
		}
		if row.CompactionBoundary == nil {
			continue
		}

		visible, err := s.NarrativeEventsForContext(n.ID)
		if err != nil {
			return nil, fmt.Errorf("counting context events for narrative %d: %w", n.ID, err)
		}

		out = append(out, Compaction{
			NarrativeID:  n.ID,
			EventsFolded: priorVisible[n.ID] + incomingCount[n.ID] - len(visible),
			Boundary:     *row.CompactionBoundary,
		})
	}

	return out, nil
}
