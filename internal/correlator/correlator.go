// Package correlator clusters events into narratives — one logical unit of
// work, whatever raw events it took to produce it. Cluster is pure compute:
// it never touches the store (see internal/store's Narrative/ClusterResult
// mirror-shape note below); Persist (a later slice) is what writes results
// to real narratives/narrative_events rows.
//
// See docs/superpowers/specs/2026-08-11-phase1-correlator-design.md for the
// full phase-1 vertical this package is one component of, and
// docs/superpowers/specs/2026-08-12-correlator-cluster-design.md for this
// package's own design (overflow handling, rationale for every non-obvious
// choice below). The prompt/response contract in that doc is superseded by
// docs/superpowers/specs/2026-08-12-correlator-hydrated-context-rework.md,
// which adds each narrative's raw events to the prompt as context.
package correlator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/llm"
	"github.com/jcogilvie/unjira/internal/logging"
	"github.com/jcogilvie/unjira/internal/rules"
	"github.com/jcogilvie/unjira/internal/store"
)

// TimeRange is a half-open [Start, End) window over events.Event.OccurredAt.
type TimeRange struct {
	Start, End time.Time
}

// Event is an alias for events.Event — Cluster operates on the same
// normalized event shape every collector emits.
type Event = events.Event

// Narrative mirrors internal/store's `narratives` table row shape so a
// later slice's Persist doesn't have to reshape this type when it starts
// writing real rows — Cluster reads WindowStart/WindowEnd (for
// overlap/adjacency), ID/Title/Summary, and Events (all for prompt context),
// but the full shape is defined now so nothing here changes when persistence
// lands.
type Narrative struct {
	ID          int64
	WindowStart time.Time
	WindowEnd   time.Time
	Title       string
	Summary     string
	Status      string
	// Events is the narrative's context events, hydrated by the caller
	// (see store.MemberEventsAfterBoundary) before Cluster is called —
	// everything newer than the narrative's compaction boundary; the recap
	// of older events lives in Summary. Cluster reads these for context
	// only and never fetches them itself, keeping Cluster pure compute.
	Events []Event

	// EligibleEvents are this narrative's events that a reviewer-driven
	// re-cluster may reassign: those linked after the narrative's last
	// committed action (see store.EligibleMemberEventIDs). They render in the
	// NUMBERED section alongside in-window events, so the model can move them;
	// Events above stay context-only and cannot be reassigned.
	//
	// Empty for every routine watch pass, because by the time watch runs a
	// prior narrative normally has a committed action. That is why this
	// refines the old "all existing narratives are frozen" rule rather than
	// weakening it: the freeze now ends at each narrative's commit watermark
	// instead of at its existence, and watch's behaviour does not move.
	//
	// A caller must not put the same event in both slices. Doing so would list
	// it twice in one prompt and invite the model to assign a frozen event by
	// index — see pipeline.hydrateContextNarratives, which partitions.
	EligibleEvents []Event

	// ContextEvents are events this narrative holds as CONTEXT links: relevant
	// background that is some other narrative's work (see store.LinkContext and
	// docs/superpowers/specs/2026-10-02-shared-context-design.md). Hydrated by the
	// caller from store.ContextEventsAfterBoundary. Never numbered: a context link
	// is not a member to move. One that is ALSO in the numbered slice (it is
	// another context narrative's eligible member) renders as a back-reference to
	// its number rather than a second copy.
	ContextEvents []Event
}

// ClusterKind distinguishes a brand-new narrative from one extending an
// existing row.
type ClusterKind int

// The two cluster kinds: a brand-new narrative, or one extending an
// existing narrative row (identified by ClusterResult.NarrativeID).
const (
	ClusterNew ClusterKind = iota
	ClusterExtends
)

// ClusterResult is one narrative-shaped grouping of events, held in memory
// only. NarrativeID is set only when Kind == ClusterExtends.
type ClusterResult struct {
	Kind        ClusterKind
	NarrativeID int64
	Title       string
	Summary     string
	// Events are the cluster's MEMBER events: its work. After Cluster returns, an
	// event is a member of at most one result — a multiply-placed event is resolved
	// by the dispute re-ask (cluster_dispute.go) before Cluster returns.
	Events []Event
	// ContextEvents are events that are relevant background for this cluster's work
	// and some other narrative's work. Persist gives each a context link, which
	// nothing downstream of clustering reads in this slice.
	ContextEvents []Event
	// Confidence is the model's stated confidence that every member event is this
	// cluster's work, 0..1. Persist stores it on each member link it writes, unless
	// MemberConfidence overrides it for that event.
	Confidence float64
	// MemberConfidence overrides Confidence for individual member events, keyed by
	// EventKey. Set when a separate judgment placed the event: the dispute re-ask's
	// per-event confidence, an omission re-ask joining events to an existing cluster,
	// or a bisected half's events merged into the other half's cluster.
	MemberConfidence map[string]float64
}

// ConfidenceOf is the confidence e was placed in this cluster with: its override
// when one exists, the cluster's otherwise.
func (r ClusterResult) ConfidenceOf(e Event) float64 {
	if c, ok := r.MemberConfidence[EventKey(e)]; ok {
		return c
	}

	return r.Confidence
}

// EventKey is an event's identity within one pass: (Source, ExternalID), the
// store's dedup key. Exported because internal/pipeline matches cluster results
// against what the store read back.
func EventKey(e Event) string {
	return e.Source + "\x00" + e.ExternalID
}

// absorb adds src's member and context events to dst, deduplicating by EventKey.
// A member src carries at a confidence other than dst's is recorded as an override,
// so a merge never restates a judgment it did not make. A context event dst already
// holds as a member is dropped from context (member wins, as in store.Tx.AddContext).
//
// The one merge primitive for every path that combines two results — bisected
// halves extending one narrative, the same-story check, the omission re-ask — so
// none of them can double a member event or lose its confidence.
func absorb(dst *ClusterResult, srcEvents []Event, srcContext []Event, confidenceOf func(Event) float64) {
	members := make(map[string]bool, len(dst.Events)+len(srcEvents))
	for _, e := range dst.Events {
		members[EventKey(e)] = true
	}

	events := slices.Clone(dst.Events)
	for _, e := range srcEvents {
		k := EventKey(e)
		if members[k] {
			continue
		}
		members[k] = true
		events = append(events, e)
		if c := confidenceOf(e); c != dst.Confidence {
			if dst.MemberConfidence == nil {
				dst.MemberConfidence = make(map[string]float64)
			}
			dst.MemberConfidence[k] = c
		}
	}
	dst.Events = events

	seen := make(map[string]bool, len(dst.ContextEvents)+len(srcContext))
	var context []Event
	for _, e := range slices.Concat(dst.ContextEvents, srcContext) {
		k := EventKey(e)
		if members[k] || seen[k] {
			continue
		}
		seen[k] = true
		context = append(context, e)
	}
	dst.ContextEvents = context
}

// Stats is what one Cluster or Persist call spent, and why. Splits and
// MergeChecks are what separate an expensive-but-correct pass (a wide window
// that legitimately bisected) from a pathological one, which a bare call count
// cannot distinguish.
//
// EstimatedTokens records what estimateTokens guessed for each prompt this
// call built — including every recursion level's own prompt when the pass
// bisected — summed across the whole tree. That means a bisected pass's
// EstimatedTokens is larger than any single prompt's estimate: it is what
// the pass would have had to fit in total, not a top-level estimate, and is
// compared against PromptTokens (the server's actual count) to check whether
// that heuristic is any good.
type Stats struct {
	Calls       int
	Splits      int // window bisections
	MergeChecks int // same-story checks at split seams
	Compactions int // Persist only
	// Truncation is what the per-event summary cap left out, empty when no cap is
	// configured (finding F16). Carried on Stats because that is what already flows
	// from Cluster to internal/pipeline to the rendered pass summary — the same route
	// EstimatedTokens takes, and reporting the cut is the point of having the cap.
	Truncation TruncationReport
	// OmittedEvents counts numbered events a clustering response put in no
	// cluster, and RecoveredEvents how many of those the one follow-up call then
	// assigned. Both sum across a bisected pass. They differ only on a pass that
	// failed, because an event still unassigned after the re-ask is a loud error;
	// on a pass that returned, a non-zero OmittedEvents says the model needed a
	// second call to account for everything, which an operator should see.
	OmittedEvents   int
	RecoveredEvents int
	// DisputedEvents counts events two or more clusters placed in event_indices,
	// which the one dispute pass then gave a single member home; Disputes says how
	// each was resolved. On a pass that returned, every disputed event was resolved,
	// because an unresolved one is a loud error. Reported so the acceptance
	// measurement (shared-context spec §9, M4) can read how often the model claims
	// one event twice and what it decided, without re-deriving it from a prompt.
	DisputedEvents int
	Disputes       []DisputeResolution
	// DisputeCalls is how many calls the dispute pass made: one, unless the disputes
	// together exceeded the context window and were split into batches that each fit.
	// Already counted in Calls; broken out so a report can say which happened.
	DisputeCalls int
	// ContextLinks is how many context links Persist wrote, SharedEvents how many
	// distinct events received at least one, and MaxContextFanOut the most context
	// links one event received in this call. The distribution the spec reports in
	// place of a fan-out cap (§7): no cap exists until a number says one is needed.
	ContextLinks     int
	SharedEvents     int
	MaxContextFanOut int
	// MembersBelowFloor counts member placements Persist wrote at a confidence below
	// correlator.member_confidence_floor — the attributions triage asks a reviewer
	// to confirm. Always zero while the floor is 0 (off, the default).
	MembersBelowFloor int
	// MatchReasks counts matching calls whose response could not be parsed and was
	// asked for again, once, quoting the parser's reason (finding F44). A second
	// refusal fails that narrative loudly and leaves it unmatched for a later pass;
	// the other narratives are unaffected. Already counted in Calls.
	MatchReasks int
	// Emptied lists the narratives Persist moved every remaining member off — a
	// context narrative whose eligible members the model placed in other clusters —
	// and so marked store.StatusSplit (finding F40). Reported because nothing else
	// would show it: the narrative is in no cluster of this pass, and from the next
	// pass on it is no longer context.
	Emptied          []EmptiedNarrative
	PromptTokens     int64
	CompletionTokens int64
	EstimatedTokens  int
}

// EmptiedNarrative is one narrative a Persist call left holding no member link.
type EmptiedNarrative struct {
	NarrativeID int64
	// Title is the narrative's stored title: the story an operator would recognize.
	Title string
	// ContextLinksDeleted is how many context links it held, all deleted when it was
	// marked split. Each such event keeps its member home elsewhere.
	ContextLinksDeleted int
}

// DisputeResolution is how the dispute re-ask resolved one multiply-placed event.
type DisputeResolution struct {
	// Event is "<source>/<external_id>".
	Event string
	// Claimants describes every cluster that placed the event in event_indices, in
	// response order; Chosen is the one the model made its member home. Each is a
	// cluster label: "narrative <id>" for an extend, "new <title>" for a new one.
	Claimants  []string
	Chosen     string
	Confidence float64
	Rationale  string
}

// Add folds other into s, so a recursive Cluster call's cost rolls up into its
// parent's and a top-level call reports the whole tree. EstimatedTokens is
// summed like everything else: each level estimated its own prompt, and the
// total is what the pass would have had to fit.
//
// Exported because internal/pipeline merges Cluster's and Persist's stats into
// one pass total.
func (s *Stats) Add(other Stats) {
	s.Calls += other.Calls
	s.Splits += other.Splits
	s.MergeChecks += other.MergeChecks
	s.Compactions += other.Compactions
	// Truncation sums across a bisected pass's levels the way every other counter
	// does, so a split pass reports what the whole tree cut rather than one level's
	// share. Lengths concatenate; the longest wins.
	s.Truncation.Truncated += other.Truncation.Truncated
	s.Truncation.OriginalLengths = append(
		s.Truncation.OriginalLengths, other.Truncation.OriginalLengths...)
	if other.Truncation.LongestOriginal > s.Truncation.LongestOriginal {
		s.Truncation.LongestOriginal = other.Truncation.LongestOriginal
	}
	s.OmittedEvents += other.OmittedEvents
	s.RecoveredEvents += other.RecoveredEvents
	s.DisputedEvents += other.DisputedEvents
	s.Disputes = append(s.Disputes, other.Disputes...)
	s.DisputeCalls += other.DisputeCalls
	s.ContextLinks += other.ContextLinks
	s.SharedEvents += other.SharedEvents
	s.MaxContextFanOut = max(s.MaxContextFanOut, other.MaxContextFanOut)
	s.MembersBelowFloor += other.MembersBelowFloor
	s.MatchReasks += other.MatchReasks
	s.Emptied = append(s.Emptied, other.Emptied...)
	s.PromptTokens += other.PromptTokens
	s.CompletionTokens += other.CompletionTokens
	s.EstimatedTokens += other.EstimatedTokens
}

// AddUsage folds one completion's server-reported usage into s and counts the
// call. Exported because internal/reconciler also makes completions and
// folds their usage into the same Stats type.
func (s *Stats) AddUsage(u llm.Usage) {
	s.Calls++
	s.PromptTokens += u.PromptTokens
	s.CompletionTokens += u.CompletionTokens
}

// clusterOptions holds Cluster's optional configuration, threaded through
// ClusterOption so Cluster's positional signature doesn't grow every time a
// new knob is needed — the same pattern Match uses for matchOptions.
type clusterOptions struct {
	rules       []rules.Rule
	instruction string
	log         *slog.Logger
	// maxEventSummaryChars caps each context event's summary. Zero is unlimited,
	// which keeps every existing caller and test unchanged (finding F16).
	maxEventSummaryChars int
}

// ClusterOption configures an optional Cluster behaviour.
type ClusterOption func(*clusterOptions)

// WithClusterRules supplies rules/ entries already filtered to
// rules.ScopeCorrelator (see rules.ForScope) so Cluster can append them to
// its system prompt. Cluster does not load or filter rules itself — like
// Match's WithLinkExclusions, it only ever sees what the caller (internal/
// pipeline) already read from config and resolved; the correlator package
// takes its dependencies as parameters and never reads config on its own.
func WithClusterRules(learnedRules []rules.Rule) ClusterOption {
	return func(o *clusterOptions) {
		o.rules = learnedRules
	}
}

// WithLogger supplies the logger Cluster announces its work on.
//
// An option rather than a parameter, matching WithClusterRules above: a logger is
// optional by construction, so no existing caller or test changes, and nothing here
// reaches for a package-level default. Absent, Cluster logs nowhere.
func WithLogger(log *slog.Logger) ClusterOption {
	return func(o *clusterOptions) {
		o.log = log
	}
}

// WithMaxEventSummaryChars caps each context event's summary in the prompt, reporting
// what it truncated on Stats. Zero — the default — is unlimited.
//
// An option rather than a parameter for the reason WithClusterRules is one: Cluster's
// positional signature should not grow per knob, and every existing caller means
// "unlimited" without being edited to say so.
func WithMaxEventSummaryChars(maxChars int) ClusterOption {
	return func(o *clusterOptions) {
		o.maxEventSummaryChars = maxChars
	}
}

// WithInstruction appends a caller's directive to the system prompt — today,
// triage's [s]plit telling the model that a reviewer has judged these events to be
// more than one story.
//
// Separate from WithClusterRules even though both append to the same prompt,
// because they are different kinds of claim with different lifetimes. A rule is a
// standing constraint distilled from many past corrections and applies to every
// pass; an instruction is one reviewer's judgment about one narrative, right now.
// Passing a split directive through WithClusterRules would file a single
// observation as durable policy, and rules.Render's formatting says "learned
// rules", which this is not.
//
// Appended AFTER the rules so a conflict resolves toward the human present in the
// loop: the reviewer is looking at this narrative, and a distilled rule was written
// about others.
func WithInstruction(instruction string) ClusterOption {
	return func(o *clusterOptions) {
		o.instruction = instruction
	}
}

// Cluster groups evts (filtered to window) plus any Narrative in existing
// whose window overlaps or is adjacent to window, into narratives — each
// tagged new or extending an existing row. Pure compute: no store access.
//
// On return every event is a member of at most one result. An event the model
// placed in two or more clusters' event_indices is resolved by ONE dispute pass
// over the whole pass's results (resolveDisputes) — after any bisection has merged
// its halves, so a double placement no single response contains (finding F36, one
// eligible event numbered in both halves) is resolved the same way as one a single
// response made. The dispute pass is one call, or, when the disputes together exceed
// the context window, one call per batch that fits; each disputed event is asked
// about once either way. Not by response order: membership drives token attribution,
// and which cluster the model happened to write first says nothing about whose work
// an event is.
func Cluster(
	ctx context.Context,
	evts []Event,
	existing []Narrative,
	client llm.Client,
	window TimeRange,
	contextWindowTokens int,
	opts ...ClusterOption,
) ([]ClusterResult, Stats, error) {
	var o clusterOptions
	for _, opt := range opts {
		opt(&o)
	}

	results, stats, err := clusterWindow(ctx, evts, existing, client, window, contextWindowTokens, opts...)
	if err != nil {
		return nil, stats, err
	}

	resolved, disputeStats, err := resolveDisputes(ctx, client, disputeRequest{
		window:              window,
		results:             results,
		rules:               o.rules,
		instruction:         o.instruction,
		contextWindowTokens: contextWindowTokens,
		log:                 o.log,
	})
	stats.Add(disputeStats)
	if err != nil {
		return nil, stats, err
	}

	return resolved, stats, nil
}

// clusterWindow is Cluster without the dispute pass: one window's clustering call
// (or, over budget, its bisection), with the per-call coverage re-ask. Recursive
// through clusterWithSplit, so it must not resolve disputes itself — a dispute that
// spans two halves only exists once they are merged.
func clusterWindow(
	ctx context.Context,
	evts []Event,
	existing []Narrative,
	client llm.Client,
	window TimeRange,
	contextWindowTokens int,
	opts ...ClusterOption,
) ([]ClusterResult, Stats, error) {
	var o clusterOptions
	for _, opt := range opts {
		opt(&o)
	}

	filtered := filterEventsInWindow(evts, window)
	relevant := filterAdjacentOrOverlapping(existing, window)

	// Bound the prompt's dominant term before building it (finding F16). Applied to
	// the CONTEXT narratives only: the window's own events are the work being
	// clustered, and truncating those would degrade the judgment rather than its cost.
	relevant, truncation := capEventSummaries(relevant, o.maxEventSummaryChars)

	// assignable shares ONE index space between the prompt and the parser:
	// buildClusterPrompt numbers this exact slice, and parseClusterResponse
	// resolves event_indices against it. Passing `filtered` to the parser while
	// the prompt numbered a longer slice would make an eligible event's index
	// resolve to the wrong event, silently.
	var stats Stats
	assignable, err := assignableEvents(filtered, relevant)
	if err != nil {
		return nil, stats, fmt.Errorf("clustering events in window [%s, %s): %w", window.Start, window.End, err)
	}
	systemPrompt, userPrompt := buildClusterPrompt(assignable, relevant, o.rules, o.instruction)

	stats.Truncation = truncation
	estimated := estimateTokens(systemPrompt + userPrompt)
	stats.EstimatedTokens = estimated
	if estimated > contextWindowTokens {
		return clusterWithSplit(ctx, evts, existing, client, window, contextWindowTokens, filtered, stats, opts...)
	}

	// BEFORE the call, not after — finding F17. This is the pipeline's slowest step and
	// it used to print nothing while running, so a pass in progress looked identical to
	// a hung one. The three numbers together are what predict the wait: on a mature
	// store one pass spent 104k prompt tokens to cluster 16 candidates, because 52
	// existing narratives were hydrated as context, and without the breakdown that cost
	// reads as a defect rather than the price of context.
	// "assignable_events", not "candidates": the number is in-window events PLUS the
	// reshufflable events already held by context narratives (assignableEvents above),
	// so it is legitimately larger than the unlinked count the pass summary reports —
	// 30 against 2 on a real run. Calling it candidates invited exactly the "is that a
	// bug?" question this log line exists to prevent.
	logging.For(o.log, "correlator").InfoContext(ctx, "clustering",
		"unlinked_events", len(filtered),
		"assignable_events", len(assignable),
		"context_narratives", len(relevant),
		"est_tokens", estimated,
		"window_start", window.Start,
		"window_end", window.End,
	)

	raw, usage, err := client.Complete(ctx, systemPrompt, userPrompt)
	if err != nil {
		return nil, stats, fmt.Errorf("clustering events in window [%s, %s): %w", window.Start, window.End, err)
	}
	stats.AddUsage(usage)

	results, indices, err := parseClusterResponse(raw, assignable)
	if err != nil {
		return nil, stats, err
	}

	// Coverage is checked HERE, per model call, and not after clusterWithSplit
	// merges two halves: an index means something only against the assignable
	// slice the prompt that produced it numbered, and a re-ask must show the model
	// that same prompt. After a merge there is no single prompt to re-ask against,
	// and rebuilding one for the whole window would be the very prompt that was
	// too big to send. Each half recurses through this function, so each half's
	// call is checked here; mergeSplitResults only unions events, never drops one.
	// indices are MEMBER placements only (event_indices): an event named only in
	// context_indices has no home, which is the context-only case the store forbids,
	// so it counts as omitted and is re-asked for (shared-context spec §4).
	// Only IN-WINDOW events (the first len(filtered) of assignable) must be placed.
	// An eligible context event the model leaves out keeps the link it already
	// has, because Persist only touches events a cluster names, so nothing is
	// lost; an in-window event left out stays unlinked, and watch's rolling
	// window can move past it permanently. Eligible events also dominate the index
	// space (242 of 246 in the F16 measurements), so covering them would spend a
	// model call on omissions that lose nothing.
	omitted := unassignedIndices(indices, len(filtered))
	if len(omitted) == 0 {
		return results, stats, nil
	}

	recovered, reaskStats, err := recoverOmittedEvents(ctx, client, reaskRequest{
		window:              window,
		userPrompt:          userPrompt,
		assignable:          assignable,
		first:               results,
		firstIndices:        indices,
		omitted:             omitted,
		rules:               o.rules,
		instruction:         o.instruction,
		contextWindowTokens: contextWindowTokens,
		log:                 o.log,
	})
	stats.Add(reaskStats)
	if err != nil {
		return nil, stats, err
	}

	return recovered, stats, nil
}

// filterEventsInWindow returns the events in evts whose OccurredAt falls in
// window's half-open [Start, End) range, order-preserving.
func filterEventsInWindow(evts []Event, window TimeRange) []Event {
	var out []Event
	for _, e := range evts {
		if !e.OccurredAt.Before(window.Start) && e.OccurredAt.Before(window.End) {
			out = append(out, e)
		}
	}
	return out
}

// filterAdjacentOrOverlapping returns narratives whose window overlaps
// window or sits immediately adjacent to it (touching at a boundary) —
// temporal proximity is real clustering signal, so this is deliberately
// broader than a strict overlap check.
func filterAdjacentOrOverlapping(existing []Narrative, window TimeRange) []Narrative {
	var out []Narrative
	for _, n := range existing {
		if n.WindowEnd.Before(window.Start) || n.WindowStart.After(window.End) {
			continue
		}
		out = append(out, n)
	}
	return out
}

// charsPerTokenEstimate is how many characters this package assumes one token
// covers. Deliberately pessimistic: it must never exceed the true density, or
// estimateTokens under-counts and Cluster builds a prompt it believes fits.
//
// The familiar "~4 characters per token" figure is calibrated on English
// prose, and these prompts are not prose — they are dense with RFC3339
// timestamps, quoted JSON keys, branch names, and repo paths, all of which
// tokenize much harder. Measured against the server's own count over three
// live passes (litellm-fronted Claude, via `dev narrate --dry-run`, comparing
// Stats.EstimatedTokens with Stats.PromptTokens):
//
//	estimated  actual  implied chars/token
//	      344     548                 2.51
//	      963    1598                 2.41
//	      909    1505                 2.42
//
// 2 rounds that ~2.4 down rather than up, leaving margin for prompts denser
// still (a burst of long branch names, say) without needing a real tokenizer
// dependency. Over-estimating only costs an unnecessary bisection; under-
// estimating costs a rejected request part-way through a pass.
const charsPerTokenEstimate = 2

// estimateTokens gives a pessimistic, non-exact token count — exactness isn't
// the goal, only a margin safe enough to decide whether to split before
// spending a real call. See charsPerTokenEstimate for why the ratio is what
// it is; TestCluster_TokenEstimateIsNotOptimistic pins it against the
// measurements so a future "optimization" back toward 4 fails loudly.
func estimateTokens(text string) int {
	return estimateTokensOfLen(len(text))
}

// estimateTokensOfLen is estimateTokens for text of n bytes, for sizing a prompt
// from the lengths of its parts without concatenating them. The estimate depends on
// length alone, so the two always agree.
func estimateTokensOfLen(n int) int {
	return (n + charsPerTokenEstimate - 1) / charsPerTokenEstimate
}

// buildClusterPrompt renders the system prompt (the fixed clusterSystemPrompt
// plus, when learnedRules is non-empty, a rendered rules section — see
// rules.Render) and a two-section user prompt: the in-window events to
// cluster (numbered 0..N-1, assignable via event_indices), then the
// overlapping/adjacent narratives as CONTEXT ONLY (their events carry no
// index, so the model structurally cannot reassign them). See
// docs/superpowers/specs/2026-08-12-correlator-hydrated-context-rework.md.
// assignableEvents is the single index space Cluster's prompt numbers and its
// response parser resolves against: the in-window events, then every existing
// narrative's eligible events in `existing` order.
//
// One function so the two sides cannot disagree. They were separate call sites
// (buildClusterPrompt(filtered, ...) and parseClusterResponse(raw, filtered))
// before eligible narrative events became assignable, and keeping them separate
// would have meant an eligible event's index resolving to a DIFFERENT event —
// silent misattribution rather than a loud error.
//
// No event may appear twice, and a repeat is an ERROR, not something to
// deduplicate. The slice is collision-free by construction: an in-window event has
// no member link (store.UnlinkedEventsInRange), each event has at most one member
// link (one_member_link_per_event), so it is eligible on at most one narrative, and
// context links are never numbered. A repeat therefore means that invariant broke
// upstream, and deduplicating would hide it — the shape of every assignableEvents
// incident so far, prompt and parser numbering different slices, silently.
func assignableEvents(inWindow []Event, existing []Narrative) ([]Event, error) {
	out := make([]Event, 0, len(inWindow))
	out = append(out, inWindow...)
	for _, n := range existing {
		out = append(out, n.EligibleEvents...)
	}

	seen := make(map[string]int, len(out))
	for i, e := range out {
		if first, ok := seen[EventKey(e)]; ok {
			return nil, fmt.Errorf(
				"event %s/%s would be numbered twice in one clustering prompt (as %d and %d): an event "+
					"must have at most one member home, so it can be in-window or one narrative's eligible "+
					"member, never both or two", e.Source, e.ExternalID, first, i)
		}
		seen[EventKey(e)] = i
	}

	return out, nil
}

func buildClusterPrompt(
	evts []Event, existing []Narrative, learnedRules []rules.Rule, instruction string,
) (systemPrompt, userPrompt string) {
	systemPrompt = withRulesAndInstruction(clusterSystemPrompt, learnedRules, instruction)

	// Built off the SAME slice the events are numbered from, so a context event's
	// back-reference and its numbered entry cannot disagree (shared-context spec §5).
	number := make(map[string]int, len(evts))

	var b strings.Builder
	b.WriteString("Events to cluster:\n")
	for i, e := range evts {
		number[EventKey(e)] = i
		// %q on Summary (not %s): event summaries come from arbitrary
		// upstream session/commit text, so an embedded newline or a
		// fabricated "N. [source] ..." line could otherwise inject a
		// spurious entry into this numbered list as the model reads it.
		// Quoting escapes those, matching the %q already used for the
		// narrative fields below.
		fmt.Fprintf(&b, "%d. [%s] %q (occurred_at=%s)\n", i, e.Source, e.Summary, e.OccurredAt.Format(time.RFC3339))
	}

	b.WriteString("\nExisting narratives (CONTEXT ONLY):\n")
	if len(existing) == 0 {
		b.WriteString("(none)\n")
	}
	for _, n := range existing {
		fmt.Fprintf(&b, "narrative_id=%d title=%q window=[%s, %s)\n",
			n.ID, n.Title, n.WindowStart.Format(time.RFC3339), n.WindowEnd.Format(time.RFC3339))
		fmt.Fprintf(&b, "  summary: %q\n", n.Summary)
		if len(n.Events) > 0 {
			b.WriteString("  events:\n")
			for _, e := range n.Events {
				fmt.Fprintf(&b, "    - [%s] %q (occurred_at=%s)\n", e.Source, e.Summary, e.OccurredAt.Format(time.RFC3339))
			}
		}
		writeContextSection(&b, n.ContextEvents, number)
	}

	return systemPrompt, b.String()
}

// writeContextSection renders a context narrative's context links under their own
// heading. An event that is also numbered renders as "-> #N", a back-reference to its
// entry: it ties the two renderings deterministically and costs a few tokens instead
// of a repeated summary.
func writeContextSection(b *strings.Builder, contextEvents []Event, number map[string]int) {
	if len(contextEvents) == 0 {
		return
	}

	b.WriteString("  background (another narrative's work, linked here as context):\n")
	for _, e := range contextEvents {
		if i, ok := number[EventKey(e)]; ok {
			fmt.Fprintf(b, "    - -> #%d\n", i)

			continue
		}
		fmt.Fprintf(b, "    - [%s] %q (occurred_at=%s)\n", e.Source, e.Summary, e.OccurredAt.Format(time.RFC3339))
	}
}

// withRulesAndInstruction appends the rendered learned rules and then the
// caller's instruction to a base system prompt. Shared by the first clustering
// call and its re-ask (cluster_reask.go), so a learned rule or a reviewer's
// split directive cannot govern where events go on one call and be absent from
// the other.
func withRulesAndInstruction(base string, learnedRules []rules.Rule, instruction string) string {
	systemPrompt := base
	if rendered := rules.Render(learnedRules); rendered != "" {
		systemPrompt += "\n\n" + rendered
	}
	// After the rules deliberately — see WithInstruction. A reviewer's judgment
	// about the narrative in front of them outranks a rule distilled from others.
	if instruction != "" {
		systemPrompt += "\n\n" + instruction
	}

	return systemPrompt
}

const clusterSystemPrompt = `Cluster the given events into narratives. "Events to cluster" are numbered. Put every numbered event in exactly one cluster's event_indices: that placement is the event's home, the one narrative whose work it is. "Existing narratives" are CONTEXT ONLY — never put their events in event_indices; use them only to decide whether a numbered event extends one of them. Tag each cluster "new" or "extends" (include narrative_id when extending). Return ONLY a JSON array matching this shape, no prose, no markdown fences:
[{"kind":"new"|"extends","narrative_id":<int, only if extends>,"title":"..., only if new","summary":"...","confidence":<0.0-1.0>,"event_indices":[0,2,5],"context_indices":[3]}]

Omit title when extending: an extended narrative keeps the title it already has, so a title supplied there is discarded unread.

confidence is how sure you are that every event in this cluster's event_indices is this cluster's work, from 0 to 1.

` + clusterContextRule + `

` + clusterSummaryRule + `

` + clusterGroupingCriterion + `

Default to the coarsest grouping that is still accurate. Do not create a separate cluster for every event: a numbered list of N events should very rarely produce N clusters. Before emitting a cluster tagged "new", check the clusters you have ALREADY emitted in this response: if one covers the same ticket/branch/PR/topic, put these events there instead of opening a new one. You cannot revise a cluster once emitted. Only leave two events in separate clusters when they are genuinely unrelated work.`

// clusterContextRule is what context_indices means. Its own constant, like
// clusterGroupingCriterion, because the omission re-ask accepts context_indices too
// and must mean the same thing by them.
const clusterContextRule = `context_indices is optional. A numbered event may ALSO appear in other clusters' context_indices when it is genuinely relevant background for that cluster's work without being that cluster's work — for example, one investigation that led to several separate fixes is the work of one cluster and background for the others. context_indices never replaces a home: the event must still be in exactly one cluster's event_indices. Do not list an event in both lists of one cluster. An "extends" cluster may carry only context_indices, adding background to an existing narrative without new work; omit its summary, because the narrative's summary stays as it is.`

// clusterSummaryRule is the shared-context spec's summary rule (§2): a summary is the
// retrieval key later passes match new events against, so one that RE-TELLS another
// stream's work would attract that stream's future events as members — misattributed
// work, and a token-attribution error. A phrasing rule, expected to leak; the spec's
// two-pass measurement M7 measures whether it does, and gates this slice on it. Shared
// with the omission re-ask, which writes summaries too.
const clusterSummaryRule = `A summary describes the cluster's OWN work, the events in its event_indices. Refer to background by reference, never by re-telling it: write "Fixed log flooding in the logger (discovered while debugging the cache-eviction work)", naming the other work, not describing what that work did.`

// clusterGroupingCriterion is what makes events one narrative. Its own constant
// because the re-ask (clusterReaskSystemPrompt) must judge by the same criterion
// as the call it follows up on, and two copies would drift.
const clusterGroupingCriterion = `Grouping criterion: a narrative is one logical unit of work — the same underlying piece of work, whatever raw events it took to produce it. Group events into the SAME cluster when they are steps toward the same outcome: the same ticket, branch, PR, or topic; a sequence of commits/comments/status-changes that tell one story end to end. Do NOT group events only because they are close in time, from the same source, or from the same author — those are weak signals, not a reason to merge unrelated work. Conversely, do not split one continuous piece of work into several clusters just because it produced several events.`

// The response schema's spellings of ClusterKind. Shared by parseClusterResponse,
// the re-ask's parser, and the re-ask prompt's listing of first-response clusters,
// so the three cannot spell a kind differently.
const (
	wireKindNew     = "new"
	wireKindExtends = "extends"
)

// clusterResponseItem is the wire shape of one element in the model's JSON
// array response. Confidence is a pointer so an omitted one is an error rather
// than a silent zero.
type clusterResponseItem struct {
	Kind           string   `json:"kind"`
	NarrativeID    int64    `json:"narrative_id"`
	Title          string   `json:"title"`
	Summary        string   `json:"summary"`
	Confidence     *float64 `json:"confidence"`
	EventIndices   []int    `json:"event_indices"`
	ContextIndices []int    `json:"context_indices"`
}

// resolvedPlacement is one response item's indices resolved against the numbered
// slice: its member events (deduplicated, in first-mention order) and their indices,
// its context events (deduplicated, minus any that are also members of THIS
// cluster — member wins), and its confidence.
type resolvedPlacement struct {
	members       []Event
	memberIndices []int
	context       []Event
	confidence    float64
}

// resolvePlacement range-checks and resolves one item's event_indices and
// context_indices against evts, and validates its confidence. what names the
// response in errors ("cluster response", "cluster re-ask response"), and raw is the
// response body, quoted in every error as parseClusterResponse always has.
//
// A repeated index within one list, or an index in both lists of one cluster, is
// normalized rather than rejected: neither loses or misplaces anything (the event
// keeps one placement in this cluster, as a member when it was named as one), and
// failing a pass for it would be brittle for no protection.
func resolvePlacement(what, raw string, eventIndices, contextIndices []int, confidence *float64, evts []Event) (resolvedPlacement, error) {
	var p resolvedPlacement

	if len(eventIndices) > 0 {
		if confidence == nil {
			return p, fmt.Errorf("parsing %s %q: a cluster with event_indices %v has no confidence", what, raw, eventIndices)
		}
		if *confidence < 0 || *confidence > 1 {
			return p, fmt.Errorf("parsing %s %q: confidence %v outside [0, 1]", what, raw, *confidence)
		}
		p.confidence = *confidence
	}

	memberSet := make(map[int]bool, len(eventIndices))
	for _, idx := range eventIndices {
		if idx < 0 || idx >= len(evts) {
			return p, fmt.Errorf("parsing %s %q: event_indices value %d out of range [0,%d)", what, raw, idx, len(evts))
		}
		if memberSet[idx] {
			continue
		}
		memberSet[idx] = true
		p.members = append(p.members, evts[idx])
		p.memberIndices = append(p.memberIndices, idx)
	}

	contextSet := make(map[int]bool, len(contextIndices))
	for _, idx := range contextIndices {
		if idx < 0 || idx >= len(evts) {
			return p, fmt.Errorf("parsing %s %q: context_indices value %d out of range [0,%d)", what, raw, idx, len(evts))
		}
		if memberSet[idx] || contextSet[idx] {
			continue
		}
		contextSet[idx] = true
		p.context = append(p.context, evts[idx])
	}

	return p, nil
}

// parseClusterResponse unmarshals raw (the model's response body) against
// evts (the exact slice sent in the prompt, so indices map back correctly).
// Any malformed shape — invalid JSON, an out-of-range index, an unknown
// kind — is a loud error including the raw response, never a partial or
// best-effort result.
//
// It also returns each result's member indices (event_indices, deduplicated),
// aligned with the results. Well-formed is not the same as complete: a response
// can leave a numbered event in no cluster, and only the indices can show that, so
// Cluster checks coverage on them (unassignedIndices) and the re-ask lists them
// back to the model. context_indices are deliberately not returned there: a
// context mention is not a home, so it must not satisfy coverage.
//
// An index in two clusters' event_indices is accepted HERE and resolved later, by
// the dispute re-ask over the whole pass (resolveDisputes): a parser sees one
// response, and the same event can be placed twice across two bisected halves
// (F36), which no single response contains.
//
// raw is run through llm.JSONArrayPayload first, which strips a markdown fence
// and wraps a lone object into a one-element array — see that function for why
// each is treated as a property of the interface, not a prompt bug.
func parseClusterResponse(raw string, evts []Event) ([]ClusterResult, [][]int, error) {
	var items []clusterResponseItem
	if err := json.Unmarshal([]byte(llm.JSONArrayPayload(raw)), &items); err != nil {
		return nil, nil, fmt.Errorf("parsing cluster response %q: %w", raw, err)
	}

	results := make([]ClusterResult, 0, len(items))
	indices := make([][]int, 0, len(items))
	for _, item := range items {
		var kind ClusterKind
		switch item.Kind {
		case wireKindNew:
			kind = ClusterNew
		case wireKindExtends:
			kind = ClusterExtends
		default:
			return nil, nil, fmt.Errorf("parsing cluster response %q: unknown kind %q", raw, item.Kind)
		}

		p, err := resolvePlacement("cluster response", raw, item.EventIndices, item.ContextIndices, item.Confidence, evts)
		if err != nil {
			return nil, nil, err
		}

		results = append(results, ClusterResult{
			Kind:          kind,
			NarrativeID:   item.NarrativeID,
			Title:         item.Title,
			Summary:       item.Summary,
			Events:        p.members,
			ContextEvents: p.context,
			Confidence:    p.confidence,
		})
		indices = append(indices, p.memberIndices)
	}

	return results, indices, nil
}

// clusterWithSplit bisects window in half by time and recurses on each
// half, then merges the results (see mergeSplitResults). If bisection
// cannot make progress — the filtered event count doesn't decrease from
// the parent window to at least one non-empty half smaller than the whole
// — the window is irreducible: return a loud error naming what didn't fit
// rather than looping forever or silently truncating.
func clusterWithSplit(
	ctx context.Context,
	evts []Event,
	existing []Narrative,
	client llm.Client,
	window TimeRange,
	contextWindowTokens int,
	filtered []Event,
	stats Stats,
	opts ...ClusterOption,
) ([]ClusterResult, Stats, error) {
	if len(filtered) <= 1 {
		return nil, stats, irreducibleUnitError(window, filtered)
	}

	mid := window.Start.Add(window.End.Sub(window.Start) / 2)
	firstHalf := TimeRange{Start: window.Start, End: mid}
	secondHalf := TimeRange{Start: mid, End: window.End}

	firstFiltered := filterEventsInWindow(filtered, firstHalf)
	secondFiltered := filterEventsInWindow(filtered, secondHalf)

	if len(firstFiltered) == len(filtered) || len(secondFiltered) == len(filtered) {
		// Bisection made no progress (e.g. every remaining event shares the
		// same timestamp) — recursing again would loop forever on an
		// unchanged set. Treat as irreducible now.
		return nil, stats, irreducibleUnitError(window, filtered)
	}

	stats.Splits++

	// clusterWindow, not Cluster: disputes are resolved once, by the top-level call,
	// over the merged halves. Resolving per half would miss an eligible event both
	// halves numbered and placed differently (F36), and spend a call per level.
	firstResults, firstStats, err := clusterWindow(ctx, evts, existing, client, firstHalf, contextWindowTokens, opts...)
	stats.Add(firstStats)
	if err != nil {
		return nil, stats, err
	}

	secondResults, secondStats, err := clusterWindow(ctx, evts, existing, client, secondHalf, contextWindowTokens, opts...)
	stats.Add(secondStats)
	if err != nil {
		return nil, stats, err
	}

	merged, mergeStats, err := mergeSplitResults(ctx, client, firstResults, secondResults)
	stats.Add(mergeStats)
	if err != nil {
		return nil, stats, err
	}

	return merged, stats, nil
}

// irreducibleUnitError reports a window that cannot be split further and
// still doesn't fit the configured budget — the "error loudly rather than
// silently drop" floor for this overflow path.
func irreducibleUnitError(window TimeRange, filtered []Event) error {
	if len(filtered) == 0 {
		return fmt.Errorf(
			"context budget exceeded for window [%s, %s) with existing-narrative context alone (no events): cannot split further",
			window.Start, window.End,
		)
	}

	return fmt.Errorf(
		"context budget exceeded for irreducible event %q (source=%s) in window [%s, %s): cannot split further",
		filtered[0].ExternalID, filtered[0].Source, window.Start, window.End,
	)
}

// mergeSplitResults combines two split halves' results: ClusterExtends
// results sharing a NarrativeID merge deterministically (union events, keep
// the earlier half's title/summary); at most one adjacent-boundary pair of
// ClusterNew results (the last of first, the first of second) gets one
// extra LLM call asking whether they're the same emerging story, merging
// on yes.
//
// Unions go through absorb, which deduplicates by event and keeps each half's
// stated confidence for the members it placed. An event the two halves placed in
// two DIFFERENT clusters stays in both here; the top-level Cluster's dispute re-ask
// resolves it (F36).
func mergeSplitResults(ctx context.Context, client llm.Client, first, second []ClusterResult) ([]ClusterResult, Stats, error) {
	var stats Stats
	merged := make([]ClusterResult, 0, len(first)+len(second))
	usedFromSecond := make(map[int]bool)

	for _, f := range first {
		if f.Kind != ClusterExtends {
			merged = append(merged, f)
			continue
		}

		mergedWithSecond := false
		for j, s := range second {
			if usedFromSecond[j] || s.Kind != ClusterExtends || s.NarrativeID != f.NarrativeID {
				continue
			}
			combined := f
			combined.MemberConfidence = maps.Clone(f.MemberConfidence)
			absorb(&combined, s.Events, s.ContextEvents, s.ConfidenceOf)
			merged = append(merged, combined)
			usedFromSecond[j] = true
			mergedWithSecond = true
			break
		}
		if !mergedWithSecond {
			merged = append(merged, f)
		}
	}

	var remainingSecond []ClusterResult
	for j, s := range second {
		if !usedFromSecond[j] {
			remainingSecond = append(remainingSecond, s)
		}
	}

	// Adjacent-boundary same-story check: last ClusterNew of `merged` (from
	// first) vs first ClusterNew of remainingSecond.
	lastNewIdx := lastNewIndex(merged)
	firstNewIdx := firstNewIndex(remainingSecond)

	if lastNewIdx == -1 || firstNewIdx == -1 {
		return append(merged, remainingSecond...), stats, nil
	}

	a, b := merged[lastNewIdx], remainingSecond[firstNewIdx]

	sameStory, mergedResult, checkStats, err := checkSameStory(ctx, client, a, b)
	stats.Add(checkStats)
	if err != nil {
		return nil, stats, err
	}

	if !sameStory {
		return append(merged, remainingSecond...), stats, nil
	}

	merged[lastNewIdx] = mergedResult
	remainingSecond = append(remainingSecond[:firstNewIdx], remainingSecond[firstNewIdx+1:]...)

	return append(merged, remainingSecond...), stats, nil
}

func lastNewIndex(results []ClusterResult) int {
	for i, r := range slices.Backward(results) {
		if r.Kind == ClusterNew {
			return i
		}
	}
	return -1
}

func firstNewIndex(results []ClusterResult) int {
	for i, r := range results {
		if r.Kind == ClusterNew {
			return i
		}
	}
	return -1
}

// sameStoryResponse is the wire shape of the merge-boundary judgment call.
type sameStoryResponse struct {
	SameStory bool   `json:"same_story"`
	Title     string `json:"title"`
	Summary   string `json:"summary"`
}

const sameStorySystemPrompt = `You will be shown two narrative clusters that sit on either side of a time-window split. Decide whether they describe the same emerging story (the split point fell in the middle of one continuous narrative) or two distinct stories. Return ONLY a JSON object, no prose, no markdown fences: {"same_story": true|false, "title": "...", "summary": "..."} — title and summary are only meaningful when same_story is true (the unified title/summary for the merged cluster); omit or ignore them when false.`

// checkSameStory asks the model whether a and b (both ClusterNew) are the
// same emerging story. On yes, returns the merged ClusterResult (unioned
// events, model-provided title/summary). On no, mergedResult is the zero
// value and must be ignored.
func checkSameStory(ctx context.Context, client llm.Client, a, b ClusterResult) (bool, ClusterResult, Stats, error) {
	var stats Stats
	stats.MergeChecks++

	userPrompt := fmt.Sprintf(
		"Cluster A: title=%q summary=%q\nCluster B: title=%q summary=%q",
		a.Title, a.Summary, b.Title, b.Summary,
	)

	raw, usage, err := client.Complete(ctx, sameStorySystemPrompt, userPrompt)
	if err != nil {
		return false, ClusterResult{}, stats, fmt.Errorf("checking same-story merge for split boundary: %w", err)
	}
	stats.AddUsage(usage)

	var resp sameStoryResponse
	if err := json.Unmarshal([]byte(llm.JSONObjectPayload(raw)), &resp); err != nil {
		return false, ClusterResult{}, stats, fmt.Errorf("parsing same-story response %q: %w", raw, err)
	}

	if !resp.SameStory {
		return false, ClusterResult{}, stats, nil
	}

	// a's events, context and confidence carry over; b's members keep the
	// confidence b's half stated for them (absorb records the override).
	merged := ClusterResult{
		Kind:             ClusterNew,
		Title:            resp.Title,
		Summary:          resp.Summary,
		Events:           slices.Clone(a.Events),
		ContextEvents:    slices.Clone(a.ContextEvents),
		Confidence:       a.Confidence,
		MemberConfidence: maps.Clone(a.MemberConfidence),
	}
	absorb(&merged, b.Events, b.ContextEvents, b.ConfidenceOf)

	return true, merged, stats, nil
}

// preparedResult is one ClusterResult after phase-1 (pre-transaction)
// preparation: its events resolved to store row ids, its window bounds
// computed, and — for an extending result whose post-boundary history
// crosses the compaction threshold — the compaction recap/boundary already
// computed via the (at most one) LLM call this result needs. See Persist.
//
// eventIDs/confidences are the MEMBER events, aligned; contextIDs the context
// events. The window is computed from members only (shared-context spec §4): a
// context event dated weeks earlier would otherwise widen every narrative it
// supports, NarrativesOverlapping would return them for more windows, and context
// hydration — F16's cost — would grow with sharing.
type preparedResult struct {
	result          ClusterResult
	eventIDs        []int64
	confidences     []float64
	contextIDs      []int64
	windowLo        time.Time
	windowHi        time.Time
	doCompact       bool
	recap           string
	boundary        time.Time
	boundaryEventID int64
}

// hasMembers reports whether p places any member event. An EXTENDS without one only
// adds context, and must leave the narrative's summary and window as they are: there
// is no new work to summarize.
func (p preparedResult) hasMembers() bool { return len(p.eventIDs) > 0 }

// Persist writes Cluster's results to the narratives/narrative_events
// tables: ClusterNew inserts a fresh narrative row, ClusterExtends updates
// an existing one (extends window_end, overwrites the cumulative summary,
// links new events). After an extend, if the narrative's post-boundary
// history (recap + raw tail) crosses cfg.TailSummarizeThresholdTokens,
// older events are compacted into the summary's recap prefix via one LLM
// call (their narrative_events rows are never deleted). Returns the
// narratives touched this run, plus Stats for every compaction call made
// while preparing them (Cluster's own calls are not included — that Stats
// comes back from Cluster itself). All-or-nothing per call: any failure
// aborts the whole pass with a loud error and, via a transaction, persists
// nothing.
//
// Two kinds of link (docs/superpowers/specs/2026-10-02-shared-context-design.md
// §4). A result's Events become MEMBER links via store.Tx.MoveMember — the event's
// one home moves here — and its ContextEvents become context links via
// store.Tx.AddContext, which never deletes anything. Windows, summaries and
// compaction come from members only; an EXTENDS carrying only context leaves the
// narrative's summary and window_end untouched. Before the transaction commits,
// every event given a context link must hold a member link somewhere, or the pass
// fails (requireMemberHomes).
//
// Every result's compaction recap (the only LLM calls Persist makes) is
// computed before the write transaction opens — read current state, decide,
// call the LLM, and only then apply links+summary+boundary in one
// transaction — so no SQLite transaction is held open across an LLM
// round-trip. The pipeline_lock lease (see store.TryAcquire/Acquire)
// serializes passes so there is no concurrent-writer race despite that gap
// between the pre-transaction read and the transaction itself; Persist does
// not acquire that lock itself (a later slice's caller does).
func Persist(
	ctx context.Context,
	s *store.Store,
	client llm.Client,
	results []ClusterResult,
	cfg config.CorrelatorConfig,
	opts ...ClusterOption,
) ([]Narrative, Stats, error) {
	if len(results) == 0 {
		return nil, Stats{}, nil
	}

	var o clusterOptions
	for _, opt := range opts {
		opt(&o)
	}

	preps, stats, err := prepareResults(ctx, s, client, results, cfg, o.log)
	if err != nil {
		return nil, stats, err
	}

	var (
		touched   []Narrative
		linkStats Stats
	)
	err = s.WithTx(func(tx *store.Tx) error {
		touched = nil // WithTx may retry fn in principle; keep this idempotent.
		linkStats = Stats{}

		// Read before anything moves: the narratives this pass takes a member from are
		// the only ones it can empty.
		holders, err := tx.MemberHolders(memberEventIDs(preps))
		if err != nil {
			return err
		}

		// Every member placement first, every context link after. The order is what
		// makes the outcome independent of response order: a context link added to A
		// for an event that is still A's member is a no-op (member wins), so if A's
		// member were moved elsewhere LATER in the transaction, A would end up holding
		// nothing. Moving members first means each AddContext sees the event's final
		// home.
		for _, p := range preps {
			n, err := applyPrepared(tx, p)
			if err != nil {
				return err
			}
			touched = append(touched, n)
		}

		linkStats, err = applyContextLinks(tx, preps, touched)
		if err != nil {
			return err
		}

		// After the context links, so a context-only extend of a narrative this pass
		// emptied is judged on what the narrative finally holds: background for no
		// work is still no work.
		linkStats.Emptied, err = markEmptied(tx, holders, touched)
		if err != nil {
			return err
		}

		return requireMemberHomes(tx, preps)
	})
	if err != nil {
		return nil, stats, err
	}

	for _, e := range linkStats.Emptied {
		logging.For(o.log, "correlator").InfoContext(ctx, "narrative emptied: every member moved to another narrative",
			"narrative_id", e.NarrativeID, "status", store.StatusSplit, "context_links_deleted", e.ContextLinksDeleted)
	}

	stats.Add(linkStats)
	stats.MembersBelowFloor = countBelowFloor(results, cfg.MemberConfidenceFloor)

	return touched, stats, nil
}

// applyContextLinks writes every result's context links, onto the narrative
// applyPrepared wrote for it (touched is aligned with preps), and reports the
// sharing distribution on Stats.
func applyContextLinks(tx *store.Tx, preps []preparedResult, touched []Narrative) (Stats, error) {
	var stats Stats
	fanOut := make(map[int64]int)

	for i, p := range preps {
		for _, eid := range p.contextIDs {
			inserted, err := tx.AddContext(touched[i].ID, eid)
			if err != nil {
				return Stats{}, err
			}
			if inserted {
				stats.ContextLinks++
				fanOut[eid]++
			}
		}
	}

	stats.SharedEvents = len(fanOut)
	for _, n := range fanOut {
		stats.MaxContextFanOut = max(stats.MaxContextFanOut, n)
	}

	return stats, nil
}

// memberEventIDs is every member event id the prepared results place.
func memberEventIDs(preps []preparedResult) []int64 {
	var out []int64
	for _, p := range preps {
		out = append(out, p.eventIDs...)
	}

	return out
}

// markEmptied marks split each of holders — the narratives this pass moved a member
// off — that no longer holds any member (finding F40), deleting its context links,
// and reports them. A context narrative's eligible members are numbered in the
// clustering prompt, so the model may place every one of them elsewhere; left open,
// the narrative would keep a title and summary describing work it no longer holds and
// ride into every later prompt as context. touched is updated in place, so a narrative
// this pass also extended with context only is returned as split, as stored.
//
// A narrative with an applied action is not exempt, and need not be: MoveMember never
// moves a frozen member, so a reshuffle can empty a committed narrative only if it
// held no member at its last commit. Its actions and issue links stay on the row.
func markEmptied(tx *store.Tx, holders []int64, touched []Narrative) ([]EmptiedNarrative, error) {
	var out []EmptiedNarrative
	for _, id := range holders {
		contextLinks, emptied, err := tx.MarkSplitIfEmptied(id)
		if err != nil {
			return nil, err
		}
		if !emptied {
			continue
		}

		row, err := tx.GetNarrative(id)
		if err != nil {
			return nil, fmt.Errorf("reading emptied narrative %d: %w", id, err)
		}
		out = append(out, EmptiedNarrative{NarrativeID: id, Title: row.Title, ContextLinksDeleted: contextLinks})
		for i := range touched {
			if touched[i].ID == id {
				touched[i].Status = store.StatusSplit
			}
		}
	}

	return out, nil
}

// requireMemberHomes is the commit-time half of the one-member-home invariant: every
// event this pass gave a context link must hold a member link as the transaction
// closes. The partial unique index enforces "at most one"; nothing else can enforce
// "at least one". A context-only event would never be in any narrative's delta, so
// the work it records would never be reconciled, and the create path could never
// propose it as untracked work — a silent drop.
func requireMemberHomes(tx *store.Tx, preps []preparedResult) error {
	var (
		ids   []int64
		label = make(map[int64]string)
	)
	for _, p := range preps {
		for i, eid := range p.contextIDs {
			if _, seen := label[eid]; seen {
				continue
			}
			e := p.result.ContextEvents[i]
			label[eid] = e.Source + "/" + e.ExternalID
			ids = append(ids, eid)
		}
	}

	homeless, err := tx.EventsWithoutMemberHome(ids)
	if err != nil {
		return err
	}
	if len(homeless) == 0 {
		return nil
	}

	names := make([]string, 0, len(homeless))
	for _, id := range homeless {
		names = append(names, label[id])
	}

	return fmt.Errorf(
		"persisting narratives: %d event(s) were linked only as context, with no member home: %s. Every linked "+
			"event must be exactly one narrative's work; a context-only event would never reach any delta",
		len(homeless), strings.Join(names, ", "))
}

// countBelowFloor counts member placements stated at a confidence below floor. Zero
// while the floor is off.
func countBelowFloor(results []ClusterResult, floor float64) int {
	if floor <= 0 {
		return 0
	}

	n := 0
	for _, r := range results {
		for _, e := range r.Events {
			if r.ConfidenceOf(e) < floor {
				n++
			}
		}
	}

	return n
}

// prepareResults is Persist's pre-transaction phase: resolve each result's
// event ids, validate ClusterExtends targets exist (a hallucinated
// narrative_id fails here, before any LLM spend), compute window bounds,
// and run any compaction LLM calls. Nothing is written to the store here —
// GetNarrative reads are the only store access, and the store's own
// consistency at write time is re-checked inside the transaction (see
// applyPrepared).
func prepareResults(
	ctx context.Context,
	s *store.Store,
	client llm.Client,
	results []ClusterResult,
	cfg config.CorrelatorConfig,
	log *slog.Logger,
) ([]preparedResult, Stats, error) {
	var stats Stats
	preps := make([]preparedResult, 0, len(results))

	// One member home per event, checked before any spend. Cluster's dispute re-ask
	// guarantees it for its own output; this is the backstop for every caller, because
	// without it two results naming one member would be resolved by MoveMember in
	// result order — last writer wins, the order-dependent rule F37 came from.
	claimedBy := make(map[string]string)
	for _, r := range results {
		for _, e := range r.Events {
			k := EventKey(e)
			if prior, ok := claimedBy[k]; ok {
				return nil, stats, fmt.Errorf(
					"persisting narratives: event %s/%s is a member of two results (%s and %s); every event "+
						"must have exactly one member home, and Cluster's dispute re-ask resolves a double "+
						"placement before Persist", e.Source, e.ExternalID, prior, clusterLabel(r))
			}
			claimedBy[k] = clusterLabel(r)
		}
	}

	for _, r := range results {
		p, oneStats, err := prepareOneResult(ctx, s, client, r, cfg, log)
		stats.Add(oneStats)
		if err != nil {
			return nil, stats, err
		}
		preps = append(preps, p)
	}

	return preps, stats, nil
}

// prepareOneResult is prepareResults' per-ClusterResult body, split out to
// keep prepareResults' cognitive complexity in check: resolve r's events to
// store row ids and its window bounds, then (ClusterExtends only) validate
// the target narrative exists and run compaction if its post-boundary
// history is over threshold.
func prepareOneResult(
	ctx context.Context,
	s *store.Store,
	client llm.Client,
	r ClusterResult,
	cfg config.CorrelatorConfig,
	log *slog.Logger,
) (preparedResult, Stats, error) {
	p := preparedResult{result: r}

	for i, e := range r.Events {
		eid, err := s.EventIDByExternalID(e.Source, e.ExternalID)
		if err != nil {
			return preparedResult{}, Stats{}, fmt.Errorf("resolving event %s/%s for narrative %q: %w", e.Source, e.ExternalID, r.Title, err)
		}
		p.eventIDs = append(p.eventIDs, eid)
		p.confidences = append(p.confidences, r.ConfidenceOf(e))
		if i == 0 || e.OccurredAt.Before(p.windowLo) {
			p.windowLo = e.OccurredAt
		}
		if i == 0 || e.OccurredAt.After(p.windowHi) {
			p.windowHi = e.OccurredAt
		}
	}

	for _, e := range r.ContextEvents {
		eid, err := s.EventIDByExternalID(e.Source, e.ExternalID)
		if err != nil {
			return preparedResult{}, Stats{}, fmt.Errorf("resolving context event %s/%s for narrative %q: %w", e.Source, e.ExternalID, r.Title, err)
		}
		p.contextIDs = append(p.contextIDs, eid)
	}

	switch r.Kind {
	case ClusterNew:
		// A NEW narrative is its member work; with none it would be an empty open
		// narrative whose title and summary describe events it does not hold — F37's
		// shape, riding along as context in every later pass. Cluster's dispute
		// re-ask refuses to produce one and pipeline.requireNonEmptyClusters rejects
		// one; this is the backstop for every other caller (triage's split).
		if !p.hasMembers() {
			return preparedResult{}, Stats{}, fmt.Errorf(
				"persisting new narrative %q: it has no member events (%d context event(s)); a new narrative "+
					"must hold at least one event as its own work", r.Title, len(r.ContextEvents))
		}

		// Nothing further to prepare: no existing row to validate, no
		// compaction possible for a narrative that doesn't exist yet.
		return p, Stats{}, nil
	case ClusterExtends:
		if !p.hasMembers() && len(p.contextIDs) == 0 {
			return preparedResult{}, Stats{}, fmt.Errorf(
				"extending narrative %d: the cluster names no member and no context event", r.NarrativeID)
		}

		return prepareExtend(ctx, s, client, r, cfg, p, log)
	default:
		return preparedResult{}, Stats{}, fmt.Errorf("persisting narrative %q: unknown ClusterKind %v", r.Title, r.Kind)
	}
}

// prepareExtend fills in p's compaction fields for a ClusterExtends result:
// validate the target narrative exists (fail fast, before any LLM spend —
// TestPersist_ExtendUnknownNarrativeIDErrorsLoudly and the all-or-nothing
// test both rely on this erroring here, not on the second, transaction-
// scoped read in applyPrepared, which exists for a different reason — see
// its own comment), then compact its tail if its post-boundary history is
// over threshold.
func prepareExtend(
	ctx context.Context,
	s *store.Store,
	client llm.Client,
	r ClusterResult,
	cfg config.CorrelatorConfig,
	p preparedResult,
	log *slog.Logger,
) (preparedResult, Stats, error) {
	if _, err := s.GetNarrative(r.NarrativeID); err != nil {
		return preparedResult{}, Stats{}, fmt.Errorf("extending narrative %d: %w", r.NarrativeID, err)
	}

	// Compaction folds MEMBER history (shared-context spec §2): a context-only extend
	// adds no work, so it cannot have pushed the narrative's history over threshold,
	// and its summary is not written anyway.
	if !p.hasMembers() {
		return p, Stats{}, nil
	}

	// Per docs/superpowers/specs/2026-08-12-correlator-persist-design.md
	// ("Tail-summarization ... compute the narrative's post-boundary
	// history size (recap prefix already in summary, plus the raw events
	// newer than compaction_boundary)"), the threshold measures the
	// narrative's history *after* this extend applies: the new cumulative
	// summary r.Summary (which already carries forward any existing recap —
	// Cluster's caller hydrates the old summary back into the model's
	// context) plus every raw post-boundary event, both already-linked
	// (ctxEvents) and about-to-be-linked (r.Events). This deliberately
	// differs from the implementation plan's draft, which measured only
	// row.Summary+r.Summary — string-only, ignoring raw event volume
	// entirely, and using the stale pre-extend summary. That undercounts a
	// narrative whose bulk is many small raw events rather than one large
	// summary, exactly the case tail-summarization exists to catch.
	ctxEvents, err := s.MemberEventsAfterBoundary(r.NarrativeID)
	if err != nil {
		return preparedResult{}, Stats{}, fmt.Errorf("loading post-boundary member events for narrative %d: %w", r.NarrativeID, err)
	}
	postBoundary := mergePostBoundaryEvents(ctxEvents, r.Events)

	var stats Stats
	if estimateTokens(r.Summary+renderEventsForEstimate(postBoundary)) > cfg.TailSummarizeThresholdTokens {
		recap, boundary, boundaryEventID, compactStats, err := compactNarrativeTail(
			ctx, s, client, r.NarrativeID, r.Summary, postBoundary, cfg.RecentEventsKept, log)
		stats.Add(compactStats)
		if err != nil {
			return preparedResult{}, stats, err
		}
		// A zero boundary means compactNarrativeTail found nothing beyond
		// the recent tail worth compacting (post-boundary history is short
		// even though the threshold estimate tripped) — treat that as "no
		// compaction happened."
		if !boundary.IsZero() {
			p.doCompact = true
			p.recap = recap
			p.boundary = boundary
			p.boundaryEventID = boundaryEventID
		}
	}

	return p, stats, nil
}

// applyPrepared writes one preparedResult inside the caller's transaction,
// returning the touched Narrative. GetNarrative is re-read here (inside the
// transaction) rather than trusting prepareResults' earlier read, so the
// write is against the row's current state as of the transaction — the
// pipeline_lock lease is what actually prevents a concurrent second writer,
// but re-reading under the transaction costs nothing and avoids relying on
// that lease being the *only* thing standing between the two reads.
//
// It writes MEMBER placements only, via store.Tx.MoveMember; Persist adds every
// result's context links afterwards (applyContextLinks), once every member is in its
// final home. MoveMember replaced relinkEvents, which deleted an event's link to
// every other narrative before inserting — last-writer-wins, the order-dependent
// rule that let a double-assigned event leave an empty narrative behind (F37), and
// that would delete context links if it survived.
func moveMembers(tx *store.Tx, narrativeID int64, p preparedResult) error {
	for i, eventID := range p.eventIDs {
		if err := tx.MoveMember(narrativeID, eventID, p.confidences[i], store.PlacedByModel); err != nil {
			return err
		}
	}

	return nil
}

func applyPrepared(tx *store.Tx, p preparedResult) (Narrative, error) {
	r := p.result

	switch r.Kind {
	case ClusterNew:
		id, err := tx.InsertNarrative(p.windowLo, p.windowHi, r.Title, r.Summary)
		if err != nil {
			return Narrative{}, err
		}
		if err := moveMembers(tx, id, p); err != nil {
			return Narrative{}, err
		}
		return Narrative{
			ID: id, WindowStart: p.windowLo, WindowEnd: p.windowHi,
			Title: r.Title, Summary: r.Summary, Status: "open",
		}, nil

	case ClusterExtends:
		row, err := tx.GetNarrative(r.NarrativeID)
		if err != nil {
			return Narrative{}, fmt.Errorf("extending narrative %d: %w", r.NarrativeID, err)
		}

		// Context only: no new work, so neither the summary nor window_end moves
		// (shared-context spec §2). Writing the cluster's summary here would let a
		// narrative's recap be rewritten around another stream's events.
		if !p.hasMembers() {
			return Narrative{
				ID: r.NarrativeID, WindowStart: row.WindowStart, WindowEnd: row.WindowEnd,
				Title: row.Title, Summary: row.Summary, Status: row.Status,
			}, nil
		}

		newEnd := row.WindowEnd
		if p.windowHi.After(newEnd) {
			newEnd = p.windowHi
		}
		if err := tx.ExtendNarrative(r.NarrativeID, newEnd, r.Summary); err != nil {
			return Narrative{}, err
		}
		if err := moveMembers(tx, r.NarrativeID, p); err != nil {
			return Narrative{}, err
		}
		if p.doCompact {
			if err := tx.SetCompactionBoundary(r.NarrativeID, p.boundary, p.boundaryEventID, p.recap); err != nil {
				return Narrative{}, err
			}
		}

		summary := r.Summary
		if p.doCompact {
			summary = p.recap
		}

		// row.Title, not r.Title: the prompt asks for a title only on "new" (F27),
		// because ExtendNarrative's UPDATE has no title column and a supplied one
		// would be discarded unread. So r.Title is empty here, and returning it
		// would render `""` in the pass output — a narrative that visibly lost its
		// name. The stored title is the one that is actually true of this row.
		return Narrative{
			ID: r.NarrativeID, WindowStart: row.WindowStart, WindowEnd: newEnd,
			Title: row.Title, Summary: summary, Status: row.Status,
		}, nil

	default:
		// Unreachable: prepareResults already rejects unknown kinds before
		// any transaction opens. Kept as a loud error rather than a silent
		// no-op in case that invariant ever slips.
		return Narrative{}, fmt.Errorf("persisting narrative %q: unknown ClusterKind %v", r.Title, r.Kind)
	}
}

// mergePostBoundaryEvents combines a narrative's already-linked
// post-boundary events (ctxEvents, from store.MemberEventsAfterBoundary)
// with the events this Persist call is about to link (incoming), dedupes by
// (Source, ExternalID) — incoming may re-list an event ctxEvents already
// has, e.g. on a retried pass — and returns the union sorted by OccurredAt.
// This is the actual raw-event population the compaction-threshold estimate
// and, if triggered, the compaction call itself operate on.
func mergePostBoundaryEvents(ctxEvents, incoming []Event) []Event {
	seen := make(map[string]bool, len(ctxEvents)+len(incoming))
	merged := make([]Event, 0, len(ctxEvents)+len(incoming))

	for _, e := range slices.Concat(ctxEvents, incoming) {
		key := e.Source + "/" + e.ExternalID
		if seen[key] {
			continue
		}
		seen[key] = true
		merged = append(merged, e)
	}

	slices.SortFunc(merged, func(a, b Event) int {
		return a.OccurredAt.Compare(b.OccurredAt)
	})

	return merged
}

// renderEventsForEstimate renders evts the same way compactNarrativeTail
// would present them to the LLM, so estimateTokens sees the same text
// volume that a real compaction call would send — the threshold check and
// the call it may trigger must judge the same thing.
func renderEventsForEstimate(evts []Event) string {
	var b strings.Builder
	for _, e := range evts {
		fmt.Fprintf(&b, "- [%s] %q (occurred_at=%s)\n", e.Source, e.Summary, e.OccurredAt.Format(time.RFC3339))
	}
	return b.String()
}

// compactNarrativeTail summarizes a narrative's older post-boundary events
// (postBoundary, everything but the newest recentEventsKept of them) into a
// recap via one LLM call, and returns the recap plus the new compaction
// boundary: both the occurred_at of the newest compacted event and that
// event's store row id. The row id is required alongside the timestamp —
// see store.MemberEventsAfterBoundary's doc comment for why a bare
// timestamp cannot uniquely order events (occurred_at is stored at
// whole-second granularity, so two events in the same second are
// indistinguishable by timestamp alone, and a cut landing between them
// would otherwise silently drop the tied event from future context).
// correlator.Event carries no row id of its own (it's events.Event, the
// same shape every collector emits), so this resolves it via the same
// EventIDByExternalID accessor prepareOneResult already uses, rather than
// adding a second lookup path.
//
// compactNarrativeTail does not write anything — the caller applies the
// recap/boundary inside Persist's transaction. A zero boundary return means
// there weren't enough post-boundary events to compact (recentEventsKept or
// fewer); the caller must treat that as "no compaction happened," even
// though the size estimate that triggered this call was over threshold.
// Logs what it folded, for auditability — per this repo's "never silently
// drop data" invariant, a lossy compaction step must leave a trace of what
// it discarded even though narrative_events itself keeps the raw rows
// forever.
func compactNarrativeTail(
	ctx context.Context,
	s *store.Store,
	client llm.Client,
	narrativeID int64,
	existingSummary string,
	postBoundary []Event,
	recentEventsKept int,
	log *slog.Logger,
) (recap string, boundary time.Time, boundaryEventID int64, stats Stats, err error) {
	if len(postBoundary) <= recentEventsKept {
		return "", time.Time{}, 0, Stats{}, nil
	}

	toCompact := postBoundary[:len(postBoundary)-recentEventsKept]
	newest := toCompact[len(toCompact)-1]
	boundary = newest.OccurredAt

	boundaryEventID, err = s.EventIDByExternalID(newest.Source, newest.ExternalID)
	if err != nil {
		return "", time.Time{}, 0, Stats{}, fmt.Errorf(
			"resolving compaction boundary event %s/%s for narrative %d: %w",
			newest.Source, newest.ExternalID, narrativeID, err,
		)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Existing recap/summary:\n%s\n\nOlder events to fold into a concise recap:\n%s",
		existingSummary, renderEventsForEstimate(toCompact))

	var usage llm.Usage
	recap, usage, err = client.Complete(ctx, compactionSystemPrompt, b.String())
	if err != nil {
		return "", time.Time{}, 0, Stats{}, fmt.Errorf("compacting narrative %d tail: %w", narrativeID, err)
	}
	stats.Compactions = 1
	stats.AddUsage(usage)

	logging.For(log, "correlator").InfoContext(ctx, "compacted narrative tail",
		"narrative_id", narrativeID, "events_folded", len(toCompact),
		"boundary", boundary.Format(time.RFC3339), "boundary_event_id", boundaryEventID)

	return recap, boundary, boundaryEventID, stats, nil
}

// compactionSystemPrompt instructs the model to fold a narrative's older
// events into a single recap paragraph that becomes the new leading portion
// of its summary.
const compactionSystemPrompt = `You are compacting the older history of an ongoing work narrative. Given the existing recap/summary and a list of older events, produce a single concise recap paragraph that preserves the decisions, problems, and outcomes a future reader would need. Return ONLY the recap text, no preamble, no markdown.`
