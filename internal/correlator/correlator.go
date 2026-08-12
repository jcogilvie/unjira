// Package correlator clusters events into narratives — one logical unit of
// work, whatever raw events it took to produce it. Cluster is pure compute:
// it never touches the store (see internal/store's Narrative/ClusterResult
// mirror-shape note below); Persist (a later slice) is what writes results
// to real narratives/narrative_events rows.
//
// See docs/superpowers/specs/2026-08-11-phase1-correlator-design.md for the
// full phase-1 vertical this package is one component of, and
// docs/superpowers/specs/2026-08-12-correlator-cluster-design.md for this
// package's own design (prompt/response contract, overflow handling,
// rationale for every non-obvious choice below).
package correlator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jcogilvie/unjira/internal/events"
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
// writing real rows — Cluster only reads WindowStart/WindowEnd (for
// overlap/adjacency) and ID/Title/Summary (for prompt context), but the
// full shape is defined now so nothing here changes when persistence
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
}

// ClusterKind distinguishes a brand-new narrative from one extending an
// existing row.
type ClusterKind int

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

// llmClient is the narrow capability Cluster needs from an LLM backend — a
// consumer-owned interface (same pattern as internal/workflow's
// projectMiner) so tests use a fake with no real HTTP server.
// *openai.Client (internal/clients/openai) already satisfies this with
// zero adapter code; this package deliberately never imports
// internal/clients/openai to keep that decoupling real, not incidental.
type llmClient interface {
	Complete(ctx context.Context, systemPrompt, userPrompt string) (string, error)
}

// Cluster groups evts (filtered to window) plus any Narrative in existing
// whose window overlaps or is adjacent to window, into narratives — each
// tagged new or extending an existing row. Pure compute: no store access.
func Cluster(
	ctx context.Context,
	evts []Event,
	existing []Narrative,
	llm llmClient,
	window TimeRange,
	contextWindowTokens int,
) ([]ClusterResult, error) {
	filtered := filterEventsInWindow(evts, window)
	relevant := filterAdjacentOrOverlapping(existing, window)

	systemPrompt, userPrompt := buildClusterPrompt(filtered, relevant)

	estimated := estimateTokens(systemPrompt + userPrompt)
	if estimated > contextWindowTokens {
		return clusterWithSplit(ctx, evts, existing, llm, window, contextWindowTokens, filtered)
	}

	raw, err := llm.Complete(ctx, systemPrompt, userPrompt)
	if err != nil {
		return nil, fmt.Errorf("clustering events in window [%s, %s): %w", window.Start, window.End, err)
	}

	return parseClusterResponse(raw, filtered)
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

// estimateTokens gives a conservative, non-exact token count estimate —
// exactness isn't the goal, only a margin safe enough to decide whether to
// split before spending a real call. ~4 characters per token is the
// standard rough ratio for English text against OpenAI-family tokenizers.
func estimateTokens(text string) int {
	return (len(text) + 3) / 4
}

// buildClusterPrompt renders the fixed system prompt and a user prompt
// listing evts (numbered 0..N-1) and existing narratives, per this
// package's documented prompt/response contract (see
// docs/superpowers/specs/2026-08-12-correlator-cluster-design.md).
func buildClusterPrompt(evts []Event, existing []Narrative) (systemPrompt, userPrompt string) {
	systemPrompt = clusterSystemPrompt

	var b strings.Builder
	b.WriteString("Events:\n")
	for i, e := range evts {
		// %q on Summary (not %s): event summaries come from arbitrary
		// upstream session/commit text, so an embedded newline or a
		// fabricated "N. [source] ..." line could otherwise inject a
		// spurious entry into this numbered list as the model reads it.
		// Quoting escapes those, matching the %q already used for the
		// narrative fields below.
		fmt.Fprintf(&b, "%d. [%s] %q (occurred_at=%s)\n", i, e.Source, e.Summary, e.OccurredAt.Format(time.RFC3339))
	}
	b.WriteString("\nExisting narratives:\n")
	if len(existing) == 0 {
		b.WriteString("(none)\n")
	}
	for _, n := range existing {
		fmt.Fprintf(&b, "narrative_id=%d title=%q summary=%q window=[%s, %s)\n",
			n.ID, n.Title, n.Summary, n.WindowStart.Format(time.RFC3339), n.WindowEnd.Format(time.RFC3339))
	}

	return systemPrompt, b.String()
}

const clusterSystemPrompt = `Cluster the given events into narratives. Every event index belongs to exactly one cluster. Tag each cluster "new" or "extends"; if "extends", include the narrative_id of the existing narrative it continues. Return ONLY a JSON array matching this shape, no prose, no markdown fences:
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
func parseClusterResponse(raw string, evts []Event) ([]ClusterResult, error) {
	var items []clusterResponseItem
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
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

// clusterWithSplit is implemented in Task 5 (window bisection + merge).
// This task only needs it to exist so Cluster compiles; it errors loudly
// rather than silently proceeding with an oversized request.
func clusterWithSplit(
	_ context.Context,
	_ []Event,
	_ []Narrative,
	_ llmClient,
	window TimeRange,
	_ int,
	filtered []Event,
) ([]ClusterResult, error) {
	return nil, fmt.Errorf(
		"context budget exceeded for window [%s, %s) with %d event(s): split-and-merge not yet implemented",
		window.Start, window.End, len(filtered),
	)
}
