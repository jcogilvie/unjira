package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/llm"
	"github.com/jcogilvie/unjira/internal/store"
)

// NarrateOptions configures one narration pass.
type NarrateOptions struct {
	// DryRun runs the full pass — including the real LLM calls — but skips
	// Persist, so nothing is written. The reported narratives are exactly what
	// would have been persisted, with zero IDs since no rows were inserted.
	DryRun bool
	// Log is where the stage announces long-running work, notably the clustering call
	// that dominates a pass's wall time (finding F17). Nil is silent — the same
	// contract every other optional dependency here has, so no caller is forced to
	// supply one.
	Log *slog.Logger
}

// NarrateResult is one pass's outcome, carrying enough detail for a human to
// judge the clustering without re-querying the store.
type NarrateResult struct {
	Window            correlator.TimeRange
	UnlinkedEvents    int // clustering candidates considered
	ContextNarratives int // existing narratives passed to Cluster as context
	// ExcludedTrackerRecords counts unlinked events in the window NOT offered to
	// clustering because they are the tracker's own bookkeeping (finding F18).
	//
	// Reported rather than merely dropped: an unreported exclusion reads as "nothing
	// was left out", and an operator looking at an empty pass needs to tell "no work
	// happened" from "all of it was filtered".
	ExcludedTrackerRecords int
	// ExcludedContextNarratives counts existing narratives that overlapped the
	// window but were NOT hydrated as clustering context, because
	// correlator.max_context_narratives bounded the population (finding F16).
	// Zero whenever the bound is unset (the default) or the overlapping
	// population never exceeded it.
	//
	// Reported for the same reason ExcludedTrackerRecords is: an unreported
	// exclusion reads as "nothing was left out", and this bound is deliberately
	// unmeasured (docs/architecture-findings.md F16) — an operator needs the
	// count to tune correlator.max_context_narratives against evidence rather
	// than guessing blind.
	ExcludedContextNarratives int
	// PreAssigned are the unplaced events this pass joined to an existing narrative by
	// exact pull-request identity, BEFORE clustering — so the model never saw them
	// (F43). Under DryRun, what would have been joined; nothing was written.
	//
	// PreAssignFallbacks are the events that carried a pull-request identity and went
	// to the model anyway, each with the reason the identity did not name exactly one
	// open home. Reported, like the exclusions above, because a join that silently
	// declined would read as "the model chose this", and the two-or-more-holders case
	// is F43's damage already persisted, which an operator should see.
	//
	// On NarrateResult rather than correlator.Stats, alongside ExcludedTrackerRecords,
	// its precedent: both are this stage's deterministic pre-filters, decided before
	// the correlator is called, and Stats is the correlator's own accounting.
	PreAssigned        []PRPreAssignment
	PreAssignFallbacks []PRFallback
	// DroppedEmptyClusters names the clusters the model returned holding no event at all,
	// member or context, which this pass discarded (dropEmptyClusters). Reported because a
	// discard nobody sees is how a model regression would go unnoticed.
	DroppedEmptyClusters []string
	DryRun               bool
	Stats                correlator.Stats
	Narratives           []NarratedNarrative
	Compactions          []Compaction
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
	// Events are this pass's MEMBER events for the narrative and ContextEvents its
	// context links. On a persisted pass both are READ BACK FROM THE STORE — this
	// pass's placements as the store holds them, not as the cluster result stated
	// them — because the printout disagreeing with the store is how a double
	// assignment once read as sharing that never persisted (F37's instrument trap).
	// Under DryRun nothing persisted, so they are the cluster result's.
	Events        []correlator.Event
	ContextEvents []correlator.Event
}

// Compaction records one tail-summarization, so the lossy step is visible in
// the pass output rather than only in logs.
type Compaction struct {
	NarrativeID int64
	// EventsFolded is how many events this pass's compaction folded into the
	// recap — not the narrative's lifetime total. narrative_events rows are
	// never deleted (see store.MemberEventsAfterBoundary), so a narrative
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

	unlinked, err := s.UnlinkedEventsInRange(window.Start, window.End)
	if err != nil {
		return NarrateResult{}, fmt.Errorf("assembling clustering candidates: %w", err)
	}

	// Only work evidence is clusterable (finding F18). A tracker record is the OTHER
	// side of the diff unjira computes — the tracker's own account of itself — so
	// narrating one produces a story that restates a ticket, matches it to the ticket
	// it came from, and proposes a comment that suppressTrackerEcho then refuses.
	// Measured before this filter: 82% of the prompt's characters, and 29 of 68
	// narratives containing nothing else.
	//
	// The excluded half is NOT linked to anything, so the next pass sees it again.
	// That is deliberate and cheap: the filter is a pure function running before any
	// model call, so a repeat costs a map lookup rather than tokens — which is what
	// makes it unlike design-notes #29's livelock, where narratives were re-examined
	// at full model cost. Do not "fix" this with a watermark.
	candidates, trackerRecords := events.PartitionByTrackerRecord(unlinked)
	result.ExcludedTrackerRecords = len(trackerRecords)

	// The second pre-filter (F43): an event carrying the exact pull request a member of
	// one open narrative carries joins it here, written before clustering, and never
	// reaches the model. See preassign.go for the line it draws — exact identity, never
	// issue keys — and every case that falls back to the model.
	candidates, result.PreAssigned, result.PreAssignFallbacks, err = preassignByPullRequest(s, candidates, opts.DryRun)
	if err != nil {
		return NarrateResult{}, err
	}
	result.UnlinkedEvents = len(candidates)

	if len(candidates) == 0 {
		// Nothing to narrate is a normal outcome. Return before spending a
		// call so an idle window costs nothing.
		return result, nil
	}

	existing, excludedContext, err := hydrateContextNarratives(s, window, candidates, cfg.Correlator.MaxContextNarratives)
	if err != nil {
		return NarrateResult{}, err
	}
	existing, hiddenMembers := hidePreAssigned(existing, result.PreAssigned)
	result.ContextNarratives = len(existing)
	result.ExcludedContextNarratives = excludedContext

	correlatorRules, err := loadCorrelatorRules(cfg)
	if err != nil {
		return NarrateResult{}, err
	}

	clustered, clusterStats, err := correlator.Cluster(
		ctx, candidates, existing, client, window, cfg.LLM.ContextWindowTokens,
		correlator.WithClusterRules(correlatorRules),
		correlator.WithMaxEventSummaryChars(cfg.Correlator.MaxEventSummaryChars),
		correlator.WithLogger(opts.Log))
	result.Stats.Add(clusterStats)
	if err != nil {
		return NarrateResult{}, fmt.Errorf("clustering: %w", err)
	}
	clustered, result.DroppedEmptyClusters = dropEmptyClusters(clustered)
	if err := requireNonEmptyClusters(clustered); err != nil {
		return NarrateResult{}, err
	}

	if opts.DryRun {
		result.Narratives = describeUnpersisted(clustered, existing)

		return result, nil
	}

	return finishNarrate(ctx, s, client, cfg, existing, hiddenMembers, clustered, result)
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
	hiddenMembers map[int64]int,
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

	result.Narratives, err = describePersisted(s, clustered, persisted, priorEnds)
	if err != nil {
		return NarrateResult{}, err
	}

	result.Compactions, err = collectCompactions(s, existing, hiddenMembers, clustered, persisted, persistStats)
	if err != nil {
		return NarrateResult{}, err
	}

	return result, nil
}

// dropEmptyClusters removes the clusters holding no event at all, member or context,
// and returns them described, for the pass summary.
//
// parseClusterResponse (internal/correlator) does not reject an empty event_indices
// array, and a model does return one: a real 30-day pass died after 37 clustering calls
// on a NEW cluster titled "meta-claude TODO-refresh cron" with no event in it, and every
// cluster the pass had produced was lost with it. Such a cluster carries no data. By the
// time Cluster returns, its omission re-ask has given every in-window event a member home,
// so discarding it loses nothing. It is the same class as a trailing comma: punctuation,
// not content. A cluster holding only CONTEXT is a different case, and requireNonEmptyClusters
// still refuses it.
func dropEmptyClusters(clustered []correlator.ClusterResult) ([]correlator.ClusterResult, []string) {
	kept := make([]correlator.ClusterResult, 0, len(clustered))
	var dropped []string

	for _, r := range clustered {
		if len(r.Events) > 0 || len(r.ContextEvents) > 0 {
			kept = append(kept, r)

			continue
		}
		if r.Kind == correlator.ClusterNew {
			dropped = append(dropped, fmt.Sprintf("new %q", r.Title))
		} else {
			dropped = append(dropped, fmt.Sprintf("extends narrative %d", r.NarrativeID))
		}
	}

	return kept, dropped
}

// requireNonEmptyClusters rejects a NEW cluster with no member events. A new narrative
// IS its member work, so context alone cannot make one: it would be an empty narrative
// whose title describes somebody else's events (F37, shared-context spec §4). Left
// unrejected it would reach eventWindow (dry-run path) or prepareOneResult (persist path)
// and silently produce a zero-width window. An extend may legitimately add only background
// to a narrative that already exists. Clusters with no event at all are removed first, by
// dropEmptyClusters.
func requireNonEmptyClusters(clustered []correlator.ClusterResult) error {
	for _, r := range clustered {
		if r.Kind == correlator.ClusterNew && len(r.Events) == 0 {
			return fmt.Errorf("clustering produced new narrative %q with no member events (%d context event(s)); "+
				"a new narrative must hold at least one event as its own work", r.Title, len(r.ContextEvents))
		}
	}

	return nil
}

// hydrateContextNarratives loads the narratives overlapping or touching window
// and fills each one's Events from the store, which is what makes Cluster's
// context section carry raw events rather than just summaries (Path B).
//
// maxContext bounds how many of the overlapping rows are hydrated at all
// (finding F16, config.CorrelatorConfig.MaxContextNarratives) — applied BEFORE
// the per-row event/eligibility queries below, not after, so a bound that
// excludes a row also excludes its cost: hydrating first and discarding
// narratives afterward would keep paying the queries this bound exists to
// avoid. candidates is this pass's own work-evidence events (post-
// PartitionByTrackerRecord), read for the issue keys they name — the shared-key
// tier of selectContextNarratives' ranking. The second return is how many
// overlapping rows were excluded, which the caller must report (never
// silently drop data).
func hydrateContextNarratives(
	s *store.Store, window correlator.TimeRange, candidates []events.Event, maxContext int,
) ([]correlator.Narrative, int, error) {
	rows, err := s.NarrativesOverlapping(window.Start, window.End)
	if err != nil {
		return nil, 0, fmt.Errorf("assembling context narratives: %w", err)
	}

	rows, excluded, err := boundContextNarratives(s, rows, candidates, maxContext)
	if err != nil {
		return nil, 0, err
	}

	out := make([]correlator.Narrative, 0, len(rows))
	for _, row := range rows {
		memberEvents, err := s.MemberEventsAfterBoundary(row.ID)
		if err != nil {
			return nil, 0, fmt.Errorf("hydrating member events for narrative %d: %w", row.ID, err)
		}

		// Context links are a third slice, never numbered (shared-context spec §5): a
		// context link is not a member to move. Bounded by the same compaction
		// boundary as members, so accumulated background is not re-sent forever.
		background, err := s.ContextEventsAfterBoundary(row.ID)
		if err != nil {
			return nil, 0, fmt.Errorf("hydrating context links for narrative %d: %w", row.ID, err)
		}

		// Partition by the commit watermark: uncommitted events stay eligible
		// for re-clustering, frozen ones do not.
		//
		// watch runs are discrete, and the total floating (uncommitted) work is
		// legitimately reshufflable until something commits — new events can
		// change where a boundary belongs, and refusing to revise would make
		// every early mis-clustering permanent until a human fixed it by hand.
		// What cannot move is work a tracker mutation already describes; that is
		// what store.EligibleMemberEventIDs draws the line at.
		//
		// An event must land in exactly one slice. In both would list it twice
		// in one prompt and invite assigning a frozen event by index.
		eligibleIDs, err := s.EligibleMemberEventIDs(row.ID)
		if err != nil {
			return nil, 0, fmt.Errorf("resolving eligible events for narrative %d: %w", row.ID, err)
		}

		eligible := make(map[int64]bool, len(eligibleIDs))
		for _, id := range eligibleIDs {
			eligible[id] = true
		}

		var frozen, assignable []correlator.Event
		for _, e := range memberEvents {
			id, err := s.EventIDByExternalID(e.Source, e.ExternalID)
			if err != nil {
				return nil, 0, fmt.Errorf("resolving event id for %s/%s: %w", e.Source, e.ExternalID, err)
			}

			if eligible[id] {
				assignable = append(assignable, e)
			} else {
				frozen = append(frozen, e)
			}
		}

		out = append(out, correlator.Narrative{
			ID:             row.ID,
			WindowStart:    row.WindowStart,
			WindowEnd:      row.WindowEnd,
			Title:          row.Title,
			Summary:        row.Summary,
			Status:         row.Status,
			Events:         frozen,
			EligibleEvents: assignable,
			ContextEvents:  background,
		})
	}

	return out, excluded, nil
}

// boundContextNarratives applies config.CorrelatorConfig.MaxContextNarratives to
// rows before any per-row hydration query runs, so an excluded narrative's cost
// is excluded too. Loads NarrativeIssueKeysByNarrative only when maxContext will
// actually bind (selectContextNarratives is itself a no-op past that point) —
// skipping a store round trip that a zero or oversized bound would throw away.
func boundContextNarratives(
	s *store.Store, rows []store.NarrativeRow, candidates []events.Event, maxContext int,
) ([]store.NarrativeRow, int, error) {
	if maxContext <= 0 || len(rows) <= maxContext {
		return rows, 0, nil
	}

	ids := make([]int64, len(rows))
	for i, row := range rows {
		ids[i] = row.ID
	}

	linkedKeys, err := s.NarrativeIssueKeysByNarrative(ids)
	if err != nil {
		return nil, 0, fmt.Errorf("loading linked issue keys for context-narrative ranking: %w", err)
	}

	kept, excluded := selectContextNarratives(rows, linkedKeys, candidateIssueKeys(candidates), maxContext)

	return kept, excluded, nil
}

// describeUnpersisted renders dry-run results, which have no ids because
// nothing was written. Window bounds come from the clustered events
// themselves, matching what Persist would have computed.
//
// A context-only extend moves no window (Persist leaves it alone), so it reports the
// narrative's existing one rather than a zero window computed from no members.
func describeUnpersisted(clustered []correlator.ClusterResult, existing []correlator.Narrative) []NarratedNarrative {
	windows := make(map[int64]correlator.Narrative, len(existing))
	for _, n := range existing {
		windows[n.ID] = n
	}

	out := make([]NarratedNarrative, 0, len(clustered))
	for _, r := range clustered {
		lo, hi := eventWindow(r.Events)
		if n, ok := windows[r.NarrativeID]; ok && r.Kind == correlator.ClusterExtends && len(r.Events) == 0 {
			lo, hi = n.WindowStart, n.WindowEnd
		}
		out = append(out, NarratedNarrative{
			Kind:          r.Kind,
			ID:            0,
			WindowStart:   lo,
			WindowEnd:     hi,
			Title:         r.Title,
			Summary:       r.Summary,
			Events:        r.Events,
			ContextEvents: r.ContextEvents,
		})
	}

	return out
}

// describePersisted pairs each persisted narrative with the clustered result
// that produced it, so the output can show this pass's member and context events
// (which Persist's return does not carry) alongside real ids and window bounds
// (which the cluster result does not carry).
//
// The events are READ BACK FROM THE STORE: the narrative's member links and context
// links, restricted to the ones this result named. A printout built from the cluster
// result alone once showed an event under two narratives while the store held it
// under one (F37), so any measurement read off the output counted sharing that never
// persisted. Restricting to this result's events keeps the output about this pass
// rather than the narrative's whole history; reading from the store keeps it true.
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
	s *store.Store,
	clustered []correlator.ClusterResult,
	persisted []correlator.Narrative,
	priorEnds map[int64]time.Time,
) ([]NarratedNarrative, error) {
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
			members, err := s.AllMemberEvents(n.ID)
			if err != nil {
				return nil, fmt.Errorf("reading back member events of narrative %d: %w", n.ID, err)
			}
			background, err := s.ContextEvents(n.ID)
			if err != nil {
				return nil, fmt.Errorf("reading back context links of narrative %d: %w", n.ID, err)
			}
			narrated.Events = restrictTo(members, clustered[i].Events)
			narrated.ContextEvents = restrictTo(background, clustered[i].ContextEvents)
		}
		if narrated.Kind == correlator.ClusterExtends {
			narrated.PriorWindowEnd = priorEnds[n.ID]
		}
		out = append(out, narrated)
	}

	return out, nil
}

// restrictTo returns the events of stored that named also holds, in stored's order.
func restrictTo(stored, named []correlator.Event) []correlator.Event {
	want := make(map[string]bool, len(named))
	for _, e := range named {
		want[correlator.EventKey(e)] = true
	}

	var out []correlator.Event
	for _, e := range stored {
		if want[correlator.EventKey(e)] {
			out = append(out, e)
		}
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
// narrative_events rows are never deleted (store.MemberEventsAfterBoundary),
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
	hiddenMembers map[int64]int,
	clustered []correlator.ClusterResult,
	persisted []correlator.Narrative,
	stats correlator.Stats,
) ([]Compaction, error) {
	if stats.Compactions == 0 {
		return nil, nil
	}

	priorVisible := make(map[int64]int, len(existing))
	for _, n := range existing {
		// BOTH halves: hydrateContextNarratives splits a narrative's visible
		// events into Events (frozen by the commit watermark) and
		// EligibleEvents (still reshufflable). "Visible as context before this
		// pass" is the union — counting only Events under-reports V0 and makes
		// EventsFolded too small, which is exactly what
		// TestRunNarrate_EventsFoldedCountsOnlyThisPassNotLifetimeTotal caught
		// when eligibility was introduced.
		//
		// hiddenMembers adds back the members this pass placed by pull-request identity
		// (hidePreAssigned): kept out of the prompt, but in the store, so in what
		// Persist's compaction saw and in V1.
		priorVisible[n.ID] = len(n.Events) + len(n.EligibleEvents) + hiddenMembers[n.ID]
	}

	// K counts only events NOT already visible on that narrative. Persist's
	// incoming events used to be exclusively previously-unlinked, so V0 and K
	// could never overlap; with eligibility, a narrative's own eligible event
	// can come back assigned to itself, appearing in both V0 and r.Events.
	// Counting it twice would inflate EventsFolded.
	alreadyVisible := make(map[int64]map[string]bool, len(existing))
	for _, n := range existing {
		seen := make(map[string]bool, len(n.Events)+len(n.EligibleEvents))
		for _, e := range n.Events {
			seen[e.Source+"\x00"+e.ExternalID] = true
		}
		for _, e := range n.EligibleEvents {
			seen[e.Source+"\x00"+e.ExternalID] = true
		}
		alreadyVisible[n.ID] = seen
	}

	incomingCount := make(map[int64]int, len(clustered))
	for _, r := range clustered {
		if r.Kind != correlator.ClusterExtends {
			continue
		}
		for _, e := range r.Events {
			if !alreadyVisible[r.NarrativeID][e.Source+"\x00"+e.ExternalID] {
				incomingCount[r.NarrativeID]++
			}
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

		visible, err := s.MemberEventsAfterBoundary(n.ID)
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
