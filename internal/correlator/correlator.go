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
	"log"
	"slices"
	"strings"
	"time"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/llm"
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
	IssueKey    string
	Confidence  float64
	Status      string
	// Events is the narrative's context events, hydrated by the caller
	// (see store.NarrativeEventsForContext) before Cluster is called —
	// everything newer than the narrative's compaction boundary; the recap
	// of older events lives in Summary. Cluster reads these for context
	// only and never fetches them itself, keeping Cluster pure compute.
	Events []Event
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
	Events      []Event
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
	Calls            int
	Splits           int // window bisections
	MergeChecks      int // same-story checks at split seams
	Compactions      int // Persist only
	PromptTokens     int64
	CompletionTokens int64
	EstimatedTokens  int
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
	s.PromptTokens += other.PromptTokens
	s.CompletionTokens += other.CompletionTokens
	s.EstimatedTokens += other.EstimatedTokens
}

// addUsage folds one completion's server-reported usage into s and counts the
// call. Unexported: only this package makes completions, so nothing outside it
// has a Usage to fold.
func (s *Stats) addUsage(u llm.Usage) {
	s.Calls++
	s.PromptTokens += u.PromptTokens
	s.CompletionTokens += u.CompletionTokens
}

// Cluster groups evts (filtered to window) plus any Narrative in existing
// whose window overlaps or is adjacent to window, into narratives — each
// tagged new or extending an existing row. Pure compute: no store access.
func Cluster(
	ctx context.Context,
	evts []Event,
	existing []Narrative,
	client llm.Client,
	window TimeRange,
	contextWindowTokens int,
) ([]ClusterResult, Stats, error) {
	filtered := filterEventsInWindow(evts, window)
	relevant := filterAdjacentOrOverlapping(existing, window)

	systemPrompt, userPrompt := buildClusterPrompt(filtered, relevant)

	var stats Stats
	estimated := estimateTokens(systemPrompt + userPrompt)
	stats.EstimatedTokens = estimated
	if estimated > contextWindowTokens {
		return clusterWithSplit(ctx, evts, existing, client, window, contextWindowTokens, filtered, stats)
	}

	raw, usage, err := client.Complete(ctx, systemPrompt, userPrompt)
	if err != nil {
		return nil, stats, fmt.Errorf("clustering events in window [%s, %s): %w", window.Start, window.End, err)
	}
	stats.addUsage(usage)

	results, err := parseClusterResponse(raw, filtered)
	if err != nil {
		return nil, stats, err
	}

	return results, stats, nil
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
	return (len(text) + charsPerTokenEstimate - 1) / charsPerTokenEstimate
}

// buildClusterPrompt renders the fixed system prompt and a two-section user
// prompt: the in-window events to cluster (numbered 0..N-1, assignable via
// event_indices), then the overlapping/adjacent narratives as CONTEXT ONLY
// (their events carry no index, so the model structurally cannot reassign
// them). See docs/superpowers/specs/2026-08-12-correlator-hydrated-context-rework.md.
func buildClusterPrompt(evts []Event, existing []Narrative) (systemPrompt, userPrompt string) {
	systemPrompt = clusterSystemPrompt

	var b strings.Builder
	b.WriteString("Events to cluster:\n")
	for i, e := range evts {
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
	}

	return systemPrompt, b.String()
}

const clusterSystemPrompt = `Cluster the given events into narratives. "Events to cluster" are numbered; assign each to exactly one cluster via event_indices. "Existing narratives" are CONTEXT ONLY — never put their events in event_indices; use them only to decide whether a numbered event extends one of them. Tag each cluster "new" or "extends" (include narrative_id when extending). Return ONLY a JSON array matching this shape, no prose, no markdown fences:
[{"kind":"new"|"extends","narrative_id":<int, only if extends>,"title":"...","summary":"...","event_indices":[0,2,5]}]`

// clusterResponseItem is the wire shape of one element in the model's JSON
// array response.
type clusterResponseItem struct {
	Kind         string `json:"kind"`
	NarrativeID  int64  `json:"narrative_id"`
	Title        string `json:"title"`
	Summary      string `json:"summary"`
	EventIndices []int  `json:"event_indices"`
}

// parseClusterResponse unmarshals raw (the model's response body) against
// evts (the exact slice sent in the prompt, so indices map back correctly).
// Any malformed shape — invalid JSON, an out-of-range index, an unknown
// kind — is a loud error including the raw response, never a partial or
// best-effort result.
// stripJSONFence removes a Markdown code fence wrapping an LLM's JSON reply,
// returning the payload unchanged when there is no fence.
//
// Both system prompts say "no markdown fences", and models emit them anyway —
// a live litellm-fronted Claude model returned "```json\n[]\n```" for a
// prompt that forbade exactly that. Fencing is a property of the interface,
// not a prompt bug, so the parsers tolerate it rather than failing a pass over
// formatting. Everything past the fence stays strict: malformed JSON inside
// one is still a loud error naming the raw response.
func stripJSONFence(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if !strings.HasPrefix(trimmed, "```") {
		return raw
	}

	// Drop the opening fence and its optional language tag ("```json"), which
	// runs to the end of that first line.
	if newline := strings.IndexByte(trimmed, '\n'); newline >= 0 {
		trimmed = trimmed[newline+1:]
	} else {
		// A fence with no newline carries no payload to parse; let the caller
		// report the original text rather than inventing a valid-looking one.
		return raw
	}

	if closing := strings.LastIndex(trimmed, "```"); closing >= 0 {
		trimmed = trimmed[:closing]
	}

	return strings.TrimSpace(trimmed)
}

func parseClusterResponse(raw string, evts []Event) ([]ClusterResult, error) {
	var items []clusterResponseItem
	if err := json.Unmarshal([]byte(stripJSONFence(raw)), &items); err != nil {
		return nil, fmt.Errorf("parsing cluster response %q: %w", raw, err)
	}

	results := make([]ClusterResult, 0, len(items))
	for _, item := range items {
		var kind ClusterKind
		switch item.Kind {
		case "new":
			kind = ClusterNew
		case "extends":
			kind = ClusterExtends
		default:
			return nil, fmt.Errorf("parsing cluster response %q: unknown kind %q", raw, item.Kind)
		}

		clusterEvents := make([]Event, 0, len(item.EventIndices))
		for _, idx := range item.EventIndices {
			if idx < 0 || idx >= len(evts) {
				return nil, fmt.Errorf("parsing cluster response %q: event_indices value %d out of range [0,%d)", raw, idx, len(evts))
			}
			clusterEvents = append(clusterEvents, evts[idx])
		}

		results = append(results, ClusterResult{
			Kind:        kind,
			NarrativeID: item.NarrativeID,
			Title:       item.Title,
			Summary:     item.Summary,
			Events:      clusterEvents,
		})
	}

	return results, nil
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

	firstResults, firstStats, err := Cluster(ctx, evts, existing, client, firstHalf, contextWindowTokens)
	stats.Add(firstStats)
	if err != nil {
		return nil, stats, err
	}

	secondResults, secondStats, err := Cluster(ctx, evts, existing, client, secondHalf, contextWindowTokens)
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
			merged = append(merged, ClusterResult{
				Kind:        ClusterExtends,
				NarrativeID: f.NarrativeID,
				Title:       f.Title,
				Summary:     f.Summary,
				Events:      append(append([]Event{}, f.Events...), s.Events...),
			})
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
	stats.addUsage(usage)

	var resp sameStoryResponse
	if err := json.Unmarshal([]byte(stripJSONFence(raw)), &resp); err != nil {
		return false, ClusterResult{}, stats, fmt.Errorf("parsing same-story response %q: %w", raw, err)
	}

	if !resp.SameStory {
		return false, ClusterResult{}, stats, nil
	}

	return true, ClusterResult{
		Kind:    ClusterNew,
		Title:   resp.Title,
		Summary: resp.Summary,
		Events:  append(append([]Event{}, a.Events...), b.Events...),
	}, stats, nil
}

// preparedResult is one ClusterResult after phase-1 (pre-transaction)
// preparation: its events resolved to store row ids, its window bounds
// computed, and — for an extending result whose post-boundary history
// crosses the compaction threshold — the compaction recap/boundary already
// computed via the (at most one) LLM call this result needs. See Persist.
type preparedResult struct {
	result          ClusterResult
	eventIDs        []int64
	windowLo        time.Time
	windowHi        time.Time
	doCompact       bool
	recap           string
	boundary        time.Time
	boundaryEventID int64
}

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
) ([]Narrative, Stats, error) {
	if len(results) == 0 {
		return nil, Stats{}, nil
	}

	preps, stats, err := prepareResults(ctx, s, client, results, cfg)
	if err != nil {
		return nil, stats, err
	}

	var touched []Narrative
	err = s.WithTx(func(tx *store.Tx) error {
		touched = nil // WithTx may retry fn in principle; keep this idempotent.
		for _, p := range preps {
			n, err := applyPrepared(tx, p)
			if err != nil {
				return err
			}
			touched = append(touched, n)
		}
		return nil
	})
	if err != nil {
		return nil, stats, err
	}

	return touched, stats, nil
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
) ([]preparedResult, Stats, error) {
	var stats Stats
	preps := make([]preparedResult, 0, len(results))

	for _, r := range results {
		p, oneStats, err := prepareOneResult(ctx, s, client, r, cfg)
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
) (preparedResult, Stats, error) {
	p := preparedResult{result: r}

	for i, e := range r.Events {
		eid, err := s.EventIDByExternalID(e.Source, e.ExternalID)
		if err != nil {
			return preparedResult{}, Stats{}, fmt.Errorf("resolving event %s/%s for narrative %q: %w", e.Source, e.ExternalID, r.Title, err)
		}
		p.eventIDs = append(p.eventIDs, eid)
		if i == 0 || e.OccurredAt.Before(p.windowLo) {
			p.windowLo = e.OccurredAt
		}
		if i == 0 || e.OccurredAt.After(p.windowHi) {
			p.windowHi = e.OccurredAt
		}
	}

	switch r.Kind {
	case ClusterNew:
		// Nothing further to prepare: no existing row to validate, no
		// compaction possible for a narrative that doesn't exist yet.
		return p, Stats{}, nil
	case ClusterExtends:
		return prepareExtend(ctx, s, client, r, cfg, p)
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
) (preparedResult, Stats, error) {
	if _, err := s.GetNarrative(r.NarrativeID); err != nil {
		return preparedResult{}, Stats{}, fmt.Errorf("extending narrative %d: %w", r.NarrativeID, err)
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
	ctxEvents, err := s.NarrativeEventsForContext(r.NarrativeID)
	if err != nil {
		return preparedResult{}, Stats{}, fmt.Errorf("loading context events for narrative %d: %w", r.NarrativeID, err)
	}
	postBoundary := mergePostBoundaryEvents(ctxEvents, r.Events)

	var stats Stats
	if estimateTokens(r.Summary+renderEventsForEstimate(postBoundary)) > cfg.TailSummarizeThresholdTokens {
		recap, boundary, boundaryEventID, compactStats, err := compactNarrativeTail(ctx, s, client, r.NarrativeID, r.Summary, postBoundary, cfg.RecentEventsKept)
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
func applyPrepared(tx *store.Tx, p preparedResult) (Narrative, error) {
	r := p.result

	switch r.Kind {
	case ClusterNew:
		id, err := tx.InsertNarrative(p.windowLo, p.windowHi, r.Title, r.Summary)
		if err != nil {
			return Narrative{}, err
		}
		if err := tx.AddNarrativeEvents(id, p.eventIDs); err != nil {
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

		newEnd := row.WindowEnd
		if p.windowHi.After(newEnd) {
			newEnd = p.windowHi
		}
		if err := tx.ExtendNarrative(r.NarrativeID, newEnd, r.Summary); err != nil {
			return Narrative{}, err
		}
		if err := tx.AddNarrativeEvents(r.NarrativeID, p.eventIDs); err != nil {
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
		return Narrative{
			ID: r.NarrativeID, WindowStart: row.WindowStart, WindowEnd: newEnd,
			Title: r.Title, Summary: summary, Status: row.Status,
		}, nil

	default:
		// Unreachable: prepareResults already rejects unknown kinds before
		// any transaction opens. Kept as a loud error rather than a silent
		// no-op in case that invariant ever slips.
		return Narrative{}, fmt.Errorf("persisting narrative %q: unknown ClusterKind %v", r.Title, r.Kind)
	}
}

// mergePostBoundaryEvents combines a narrative's already-linked
// post-boundary events (ctxEvents, from store.NarrativeEventsForContext)
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
// see store.NarrativeEventsForContext's doc comment for why a bare
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
	stats.addUsage(usage)

	log.Printf("correlator: compacted narrative %d — folded %d event(s) up to %s (event id %d) into recap",
		narrativeID, len(toCompact), boundary.Format(time.RFC3339), boundaryEventID)

	return recap, boundary, boundaryEventID, stats, nil
}

// compactionSystemPrompt instructs the model to fold a narrative's older
// events into a single recap paragraph that becomes the new leading portion
// of its summary.
const compactionSystemPrompt = `You are compacting the older history of an ongoing work narrative. Given the existing recap/summary and a list of older events, produce a single concise recap paragraph that preserves the decisions, problems, and outcomes a future reader would need. Return ONLY the recap text, no preamble, no markdown.`
