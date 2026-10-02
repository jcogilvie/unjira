package correlator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"github.com/jcogilvie/unjira/internal/llm"
	"github.com/jcogilvie/unjira/internal/logging"
	"github.com/jcogilvie/unjira/internal/rules"
)

// A clustering response can be well-formed and still incomplete: on three real
// 116-event passes, one rendered 111 events, every event of one PR in no cluster
// and no error raised. Under `unjira watch` that loss is permanent, because an
// unlinked event is only ever offered again while it is inside the rolling window,
// and the window moves on.
//
// So Cluster checks coverage after every clustering call and, when something was
// left out, makes ONE follow-up call asking the model to place exactly those
// events. Anything still unplaced is a loud error. Rejected alternatives:
//
//   - Erroring on the first omission. Correct but brittle: one forgotten event
//     fails a whole pass the model can usually finish when shown what it missed.
//   - Putting each omitted event in a cluster of its own. That invents a
//     narrative per forgotten event, which is the fragmentation the prompt's
//     grouping criterion exists to prevent, and hides that anything went wrong.
//   - Re-asking until complete. Unbounded cost on a model that keeps omitting; one
//     re-ask is the budget, and a second failure is information, not noise.
//   - Re-clustering the whole window. It throws away the clusters the model
//     already got right and costs the full prompt again for a handful of events.

// reaskRequest is what one re-ask needs from the clustering call it follows up on.
type reaskRequest struct {
	window TimeRange
	// userPrompt is the first call's user prompt, verbatim. The re-ask repeats it
	// so every index means what it meant the first time — the index space is
	// assignable, shared with the parser, and must not be renumbered.
	userPrompt   string
	assignable   []Event
	first        []ClusterResult
	firstIndices [][]int
	omitted      []int
	rules        []rules.Rule
	instruction  string

	contextWindowTokens int
	log                 *slog.Logger
}

// unassignedIndices returns, ascending, every index in [0, n) that appears in no
// cluster's event_indices. An index in several clusters counts as assigned:
// double assignment is not omission, and its contract is not this check's to
// change.
//
// n is the number of indices that MUST be placed — the in-window events, which
// assignableEvents numbers first — not the size of the whole index space. Indices
// at or beyond n are eligible context events: valid in a response
// (parseClusterResponse range-checked them against the full slice) and simply not
// subject to coverage, so they are skipped rather than indexed.
func unassignedIndices(indices [][]int, n int) []int {
	assigned := make([]bool, n)
	for _, cluster := range indices {
		for _, idx := range cluster {
			if idx < n {
				assigned[idx] = true
			}
		}
	}

	var out []int
	for idx, ok := range assigned {
		if !ok {
			out = append(out, idx)
		}
	}

	return out
}

// recoverOmittedEvents makes the one re-ask for req.omitted and merges its answer
// into req.first. It returns the merged results only when every omitted event was
// placed; otherwise a loud error naming what is still missing, and no results.
func recoverOmittedEvents(ctx context.Context, client llm.Client, req reaskRequest) ([]ClusterResult, Stats, error) {
	stats := Stats{OmittedEvents: len(req.omitted)}

	systemPrompt, userPrompt := buildReaskPrompt(req)
	estimated := estimateTokens(systemPrompt + userPrompt)
	stats.EstimatedTokens = estimated

	logging.For(req.log, "correlator").WarnContext(ctx, "cluster response left events unassigned; re-asking once",
		"omitted_events", len(req.omitted),
		"assignable_events", len(req.assignable),
		"omitted", describeIndices(req.assignable, req.omitted),
		"est_tokens", estimated,
	)

	// The re-ask is larger than the prompt that fitted, since it adds the clusters
	// already produced. Sent over budget it would be rejected part-way through the
	// pass; there is nothing to bisect, because the first response's clusters span
	// the whole window.
	if estimated > req.contextWindowTokens {
		return nil, stats, fmt.Errorf(
			"clustering events in window [%s, %s): the model left %d event(s) unassigned (%s), and the re-ask "+
				"for them is estimated at %d tokens, over the %d-token context window",
			req.window.Start, req.window.End, len(req.omitted), describeIndices(req.assignable, req.omitted),
			estimated, req.contextWindowTokens)
	}

	raw, usage, err := client.Complete(ctx, systemPrompt, userPrompt)
	if err != nil {
		return nil, stats, fmt.Errorf("re-asking for %d unassigned event(s) in window [%s, %s): %w",
			len(req.omitted), req.window.Start, req.window.End, err)
	}
	stats.AddUsage(usage)

	merged, stillMissing, err := mergeReaskResponse(raw, req.assignable, req.first, req.omitted)
	if err != nil {
		return nil, stats, err
	}
	stats.RecoveredEvents = len(req.omitted) - len(stillMissing)

	if len(stillMissing) > 0 {
		return nil, stats, fmt.Errorf(
			"clustering events in window [%s, %s): %d event(s) still in no cluster after one re-ask: %s",
			req.window.Start, req.window.End, len(stillMissing), describeIndices(req.assignable, stillMissing))
	}

	logging.For(req.log, "correlator").InfoContext(ctx, "re-ask placed every unassigned event",
		"recovered_events", stats.RecoveredEvents)

	return merged, stats, nil
}

// buildReaskPrompt repeats the first call's user prompt verbatim, then lists the
// clusters that call produced, by position, and the omitted indices.
//
// By position because a cluster tagged "new" has no narrative_id yet, so position
// in the first response is the only name it has. Its event_indices are listed too:
// they are what the cluster is, more precisely than a title.
func buildReaskPrompt(req reaskRequest) (systemPrompt, userPrompt string) {
	systemPrompt = withRulesAndInstruction(clusterReaskSystemPrompt, req.rules, req.instruction)

	var b strings.Builder
	b.WriteString(req.userPrompt)
	b.WriteString("\nClusters you already produced, by cluster_position:\n")
	if len(req.first) == 0 {
		b.WriteString("(none)\n")
	}
	for i, r := range req.first {
		fmt.Fprintf(&b, "cluster_position=%d kind=%s", i, clusterKindWire(r.Kind))
		if r.Kind == ClusterExtends {
			fmt.Fprintf(&b, " narrative_id=%d", r.NarrativeID)
		} else {
			fmt.Fprintf(&b, " title=%q", r.Title)
		}
		fmt.Fprintf(&b, " summary=%q event_indices=%s\n", r.Summary, formatIndices(req.firstIndices[i]))
	}

	fmt.Fprintf(&b, "\nUnassigned events — your response put these numbered events in no cluster. "+
		"Assign each of them: %s\n", formatIndices(req.omitted))

	return systemPrompt, b.String()
}

const clusterReaskSystemPrompt = `You already clustered the numbered "Events to cluster" below, but your response put some of them in no cluster. Every numbered event must belong to a cluster. Assign each event listed under "Unassigned events", and use no other index: every event you already assigned stays where you put it. "Existing narratives" are CONTEXT ONLY — never put their events in event_indices; use them only to decide whether an unassigned event extends one of them.

Each element of your response places some unassigned events in one of three places:
- "earlier": a cluster you already produced, named by its cluster_position. This is the only way to add to one you tagged "new", since it has no narrative_id yet. Give that cluster's updated summary covering the added events, or omit summary to keep it unchanged.
- "extends": an existing narrative, by narrative_id, with the narrative's updated summary.
- "new": a brand-new cluster, with a title and summary.

Return ONLY a JSON array matching this shape, no prose, no markdown fences:
[{"kind":"earlier"|"extends"|"new","cluster_position":<int, only if earlier>,"narrative_id":<int, only if extends>,"title":"..., only if new","summary":"...","event_indices":[3]}]

` + clusterGroupingCriterion + `

Prefer joining a cluster that covers the same ticket/branch/PR/topic over opening a new one.`

// reaskResponseItem is the wire shape of one element of a re-ask response. It is
// clusterResponseItem plus cluster_position, which only "earlier" uses. A pointer
// so an "earlier" without one is an error, not a silent join to position 0.
type reaskResponseItem struct {
	Kind            string `json:"kind"`
	ClusterPosition *int   `json:"cluster_position"`
	NarrativeID     int64  `json:"narrative_id"`
	Title           string `json:"title"`
	Summary         string `json:"summary"`
	EventIndices    []int  `json:"event_indices"`
}

// mergeReaskResponse parses raw against the same assignable slice the first
// response was parsed against and merges it into first, deterministically:
//
//   - "earlier" appends to first[cluster_position], keeping its kind and id.
//   - "extends" appends to the result already extending that narrative_id, if
//     any — the precedent is mergeSplitResults, which never leaves two results
//     extending one narrative — and otherwise adds a new ClusterExtends.
//   - "new" adds a new ClusterNew.
//
// A non-empty summary replaces the joined cluster's: Persist writes an extend's
// summary as the narrative's cumulative one, and only the re-ask has seen the
// added events. An empty one keeps the summary the first response wrote.
//
// Any malformed shape is a loud error including raw, as in parseClusterResponse.
// So is an index that was not omitted: the re-ask was asked about exactly those
// events, and a reply that moves others is revising answers it was told stand.
// It returns the omitted indices the response still did not place.
func mergeReaskResponse(raw string, evts []Event, first []ClusterResult, omitted []int) ([]ClusterResult, []int, error) {
	var items []reaskResponseItem
	if err := json.Unmarshal([]byte(llm.JSONArrayPayload(raw)), &items); err != nil {
		return nil, nil, fmt.Errorf("parsing cluster re-ask response %q: %w", raw, err)
	}

	merged := slices.Clone(first)
	placed := make(map[int]bool, len(omitted))

	for _, item := range items {
		itemEvents, err := resolveReaskIndices(raw, item.EventIndices, evts, omitted)
		if err != nil {
			return nil, nil, err
		}

		merged, err = placeReaskItem(raw, merged, len(first), item, itemEvents)
		if err != nil {
			return nil, nil, err
		}

		for _, idx := range item.EventIndices {
			placed[idx] = true
		}
	}

	var stillMissing []int
	for _, idx := range omitted {
		if !placed[idx] {
			stillMissing = append(stillMissing, idx)
		}
	}

	return merged, stillMissing, nil
}

// resolveReaskIndices maps one re-ask item's indices to events, rejecting any
// index out of range or not among those omitted.
func resolveReaskIndices(raw string, indices []int, evts []Event, omitted []int) ([]Event, error) {
	out := make([]Event, 0, len(indices))
	for _, idx := range indices {
		if idx < 0 || idx >= len(evts) {
			return nil, fmt.Errorf("parsing cluster re-ask response %q: event_indices value %d out of range [0,%d)",
				raw, idx, len(evts))
		}
		if !slices.Contains(omitted, idx) {
			return nil, fmt.Errorf("parsing cluster re-ask response %q: event_indices value %d was not one of the omitted indices %v",
				raw, idx, omitted)
		}
		out = append(out, evts[idx])
	}

	return out, nil
}

// placeReaskItem puts one re-ask item's events where it says, returning the
// updated results. firstLen bounds cluster_position to the first response: the
// model was shown those positions and no others.
func placeReaskItem(raw string, merged []ClusterResult, firstLen int, item reaskResponseItem, itemEvents []Event) ([]ClusterResult, error) {
	switch item.Kind {
	case "earlier":
		if item.ClusterPosition == nil {
			return nil, fmt.Errorf("parsing cluster re-ask response %q: kind \"earlier\" without cluster_position", raw)
		}
		pos := *item.ClusterPosition
		if pos < 0 || pos >= firstLen {
			return nil, fmt.Errorf("parsing cluster re-ask response %q: cluster_position %d out of range [0,%d)",
				raw, pos, firstLen)
		}
		joinCluster(&merged[pos], itemEvents, item.Summary)

		return merged, nil

	case wireKindExtends:
		for i := range merged {
			if merged[i].Kind == ClusterExtends && merged[i].NarrativeID == item.NarrativeID {
				joinCluster(&merged[i], itemEvents, item.Summary)

				return merged, nil
			}
		}

		return append(merged, ClusterResult{
			Kind: ClusterExtends, NarrativeID: item.NarrativeID,
			Title: item.Title, Summary: item.Summary, Events: itemEvents,
		}), nil

	case wireKindNew:
		return append(merged, ClusterResult{
			Kind: ClusterNew, Title: item.Title, Summary: item.Summary, Events: itemEvents,
		}), nil

	default:
		return nil, fmt.Errorf("parsing cluster re-ask response %q: unknown kind %q", raw, item.Kind)
	}
}

// joinCluster adds evts to r, replacing its summary only when one was supplied.
// The events slice is copied rather than appended in place, so a merged result
// never shares a backing array with the first response's.
func joinCluster(r *ClusterResult, evts []Event, summary string) {
	r.Events = slices.Concat(r.Events, evts)
	if summary != "" {
		r.Summary = summary
	}
}

// clusterKindWire renders a kind the way the response schema spells it.
func clusterKindWire(k ClusterKind) string {
	if k == ClusterExtends {
		return wireKindExtends
	}

	return wireKindNew
}

// formatIndices renders indices as "[2, 3]", the same shape the response
// schema's own example uses.
func formatIndices(indices []int) string {
	parts := make([]string, len(indices))
	for i, idx := range indices {
		parts[i] = strconv.Itoa(idx)
	}

	return "[" + strings.Join(parts, ", ") + "]"
}

// describeIndices names each index with its event's source and external id, so
// an error or log line identifies the events rather than positions in a prompt
// nobody can see afterwards.
func describeIndices(evts []Event, indices []int) string {
	parts := make([]string, len(indices))
	for i, idx := range indices {
		parts[i] = fmt.Sprintf("%d (%s/%s)", idx, evts[idx].Source, evts[idx].ExternalID)
	}

	return strings.Join(parts, ", ")
}
