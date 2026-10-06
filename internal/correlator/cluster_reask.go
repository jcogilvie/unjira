package correlator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
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
// left out, makes follow-up calls — rounds — asking the model to place exactly the
// events still unplaced, up to a configured budget (llm.max_omission_reasks,
// default 1). Each round shows the clusters as merged so far, so a cluster an
// earlier round created can be joined by position. A round whose response the
// parser refuses merges nothing and still spends the round; the next one quotes
// the refused response and the reason. Anything unplaced once the budget is spent
// is a loud error. Rejected alternatives:
//
//   - Erroring on the first omission, always. Correct but brittle: one forgotten
//     event fails a whole pass the model can usually finish when shown what it
//     missed. A budget of 0 still chooses exactly this, for whoever wants it.
//   - Putting each omitted event in a cluster of its own. That invents a
//     narrative per forgotten event, which is the fragmentation the prompt's
//     grouping criterion exists to prevent, and hides that anything went wrong.
//   - Re-asking until complete. Unbounded cost on a model that keeps omitting, so
//     the rounds are bounded by configuration rather than by the model, and the
//     bound defaults to one: a model still omitting after it was shown what it
//     missed is information, not noise, and an operator raises the bound knowing
//     what each round costs (the full first prompt again, plus the clusters).
//   - Re-clustering the whole window. It throws away the clusters the model
//     already got right and costs the full prompt again for a handful of events.

// reaskRequest is what the omission re-ask rounds need from the clustering call they
// follow up on.
type reaskRequest struct {
	window TimeRange
	// userPrompt is the first call's user prompt, verbatim. Every round repeats it
	// so every index means what it meant the first time — the index space is
	// assignable, shared with the parser, and must not be renumbered.
	userPrompt   string
	assignable   []Event
	first        []ClusterResult
	firstIndices [][]int
	omitted      []int
	rules        []rules.Rule
	instruction  string
	// maxRounds is the budget: how many follow-up calls may be made. 0 (or less)
	// makes any omission an error with no call.
	maxRounds int

	contextWindowTokens int
	log                 *slog.Logger
}

// reaskRound is the state one round's prompt is built from: the clusters as merged
// so far, each one's member indices into assignable, the events still unplaced, and
// the previous round's response when the parser refused it.
type reaskRound struct {
	number   int
	clusters []ClusterResult
	indices  [][]int
	missing  []int
	// refused and reason are the previous round's response and the parser's reason
	// for refusing it. reason is nil when the previous round was accepted or this is
	// the first round (a refused response can itself be empty, so refused cannot
	// mark it).
	refused string
	reason  error
}

// unassignedIndices returns, ascending, every index in [0, n) that appears in no
// cluster's event_indices. An index in several clusters counts as assigned:
// double assignment is not omission, and the dispute re-ask resolves it.
//
// indices are MEMBER placements only. An index named only in some cluster's
// context_indices is unassigned here: "assigned" must mean "has a home", or the
// guarantee would be satisfied by an event that has none — the context-only case the
// shared-context design forbids (spec §4, "Coupling with the re-ask branch").
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

// recoverOmittedEvents spends up to req.maxRounds rounds placing req.omitted, merging
// each accepted answer into the clusters so far. It returns the merged results only
// when every omitted event was placed; otherwise a loud error naming what is still
// missing, and no results.
func recoverOmittedEvents(ctx context.Context, client llm.Client, req reaskRequest) ([]ClusterResult, Stats, error) {
	stats := Stats{OmittedEvents: len(req.omitted)}

	if req.maxRounds <= 0 {
		return nil, stats, fmt.Errorf(
			"clustering events in window [%s, %s): the model left %d event(s) in no cluster (%s), and the "+
				"omission re-ask budget (llm.max_omission_reasks) is 0, so they were not asked about again",
			req.window.Start, req.window.End, len(req.omitted), describeIndices(req.assignable, req.omitted))
	}

	round := reaskRound{clusters: req.first, indices: req.firstIndices, missing: req.omitted}
	for round.number = 1; round.number <= req.maxRounds; round.number++ {
		systemPrompt, userPrompt := buildReaskPrompt(req, round)
		estimated := estimateTokens(systemPrompt + userPrompt)
		stats.EstimatedTokens += estimated

		logging.For(req.log, "correlator").WarnContext(ctx, "cluster response left events unassigned; re-asking",
			"round", round.number,
			"max_rounds", req.maxRounds,
			"omitted_events", len(round.missing),
			"assignable_events", len(req.assignable),
			"omitted", describeIndices(req.assignable, round.missing),
			"previous_round_refused", round.reason != nil,
			"est_tokens", estimated,
		)

		// A round is larger than the prompt that fitted, since it adds the clusters so
		// far (and, after a refusal, the refused response). Sent over budget it would
		// be rejected part-way through the pass; there is nothing to bisect, because
		// the clusters span the whole window.
		if estimated > req.contextWindowTokens {
			return nil, stats, withLastRefusal(fmt.Errorf(
				"clustering events in window [%s, %s): the model left %d event(s) unassigned (%s), and re-ask "+
					"round %d of %d for them is estimated at %d tokens, over the %d-token context window",
				req.window.Start, req.window.End, len(round.missing), describeIndices(req.assignable, round.missing),
				round.number, req.maxRounds, estimated, req.contextWindowTokens), round.reason)
		}

		stats.OmissionReasks++
		raw, usage, err := client.Complete(ctx, systemPrompt, userPrompt)
		if err != nil {
			return nil, stats, fmt.Errorf("re-asking (round %d of %d) for %d unassigned event(s) in window [%s, %s): %w",
				round.number, req.maxRounds, len(round.missing), req.window.Start, req.window.End, err)
		}
		stats.AddUsage(usage)

		merged, stillMissing, err := mergeReaskResponse(raw, req.assignable, round.clusters, round.missing)
		if err != nil {
			// Refused: nothing from it is merged, and the next round quotes it.
			round.refused, round.reason = raw, err

			continue
		}

		indices, err := memberIndices(merged, req.assignable)
		if err != nil {
			return nil, stats, fmt.Errorf("clustering events in window [%s, %s): %w", req.window.Start, req.window.End, err)
		}
		round = reaskRound{number: round.number, clusters: merged, indices: indices, missing: stillMissing}
		stats.RecoveredEvents = len(req.omitted) - len(round.missing)

		if len(round.missing) == 0 {
			logging.For(req.log, "correlator").InfoContext(ctx, "re-ask placed every unassigned event",
				"recovered_events", stats.RecoveredEvents, "rounds", stats.OmissionReasks)

			return merged, stats, nil
		}
	}

	return nil, stats, withLastRefusal(fmt.Errorf(
		"clustering events in window [%s, %s): %d event(s) still in no cluster after %d re-ask round(s) "+
			"(llm.max_omission_reasks): %s",
		req.window.Start, req.window.End, len(round.missing), stats.OmissionReasks,
		describeIndices(req.assignable, round.missing)), round.reason)
}

// withLastRefusal adds the last round's refusal to err, when the last round was
// refused: it is why the events are still unplaced.
func withLastRefusal(err, reason error) error {
	if reason == nil {
		return err
	}

	return fmt.Errorf("%w; the last re-ask response was refused: %w", err, reason)
}

// memberIndices recomputes each result's member indices into assignable, so the
// next round can list the merged clusters exactly as the first round listed the
// first response's. assignable holds each event once (assignableEvents refuses a
// repeat), so an event's index is unambiguous; a member not in it would mean a merge
// invented an event, which is a loud error rather than an index to guess.
func memberIndices(results []ClusterResult, assignable []Event) ([][]int, error) {
	index := make(map[string]int, len(assignable))
	for i, e := range assignable {
		index[EventKey(e)] = i
	}

	out := make([][]int, len(results))
	for i, r := range results {
		for _, e := range r.Events {
			idx, ok := index[EventKey(e)]
			if !ok {
				return nil, fmt.Errorf("merged cluster_position=%d holds %s/%s, which is not a numbered event",
					i, e.Source, e.ExternalID)
			}
			out[i] = append(out[i], idx)
		}
	}

	return out, nil
}

// buildReaskPrompt repeats the first call's user prompt verbatim, then lists the
// clusters so far, by position, and the indices still unplaced. After a refused
// round it also quotes the refused response and the parser's reason.
//
// By position because a cluster tagged "new" has no narrative_id yet, so position
// in the list shown is the only name it has — and the list is the MERGED one, so a
// cluster an earlier round created can be joined. Its event_indices are listed too:
// they are what the cluster is, more precisely than a title.
func buildReaskPrompt(req reaskRequest, round reaskRound) (systemPrompt, userPrompt string) {
	systemPrompt = withRulesAndInstruction(clusterReaskSystemPrompt, req.rules, req.instruction)

	var b strings.Builder
	b.WriteString(req.userPrompt)
	b.WriteString("\nClusters you already produced, by cluster_position:\n")
	if len(round.clusters) == 0 {
		b.WriteString("(none)\n")
	}
	for i, r := range round.clusters {
		fmt.Fprintf(&b, "cluster_position=%d kind=%s", i, clusterKindWire(r.Kind))
		if r.Kind == ClusterExtends {
			fmt.Fprintf(&b, " narrative_id=%d", r.NarrativeID)
		} else {
			fmt.Fprintf(&b, " title=%q", r.Title)
		}
		fmt.Fprintf(&b, " summary=%q event_indices=%s\n", r.Summary, formatIndices(round.indices[i]))
	}

	fmt.Fprintf(&b, "\nUnassigned events — your response put these numbered events in no cluster. "+
		"Assign each of them: %s\n", formatIndices(round.missing))

	if round.reason != nil {
		writeRefusal(&b, round.refused, round.reason)
		b.WriteString("\n\nNothing from it was applied. Answer again: return ONLY the JSON array the system " +
			"prompt specifies, placing only the events listed under \"Unassigned events\", no prose and no " +
			"markdown fences.\n")
	}

	return systemPrompt, b.String()
}

const clusterReaskSystemPrompt = `You already clustered the numbered "Events to cluster" below, but your response put some of them in no cluster. Every numbered event must belong to a cluster. Assign each event listed under "Unassigned events", and use no other index: every event you already assigned stays where you put it. "Existing narratives" are CONTEXT ONLY — never put their events in event_indices; use them only to decide whether an unassigned event extends one of them.

Each element of your response places some unassigned events in one of three places:
- "earlier": a cluster you already produced, named by its cluster_position. This is the only way to add to one you tagged "new", since it has no narrative_id yet. Give that cluster's updated summary covering the added events, or omit summary to keep it unchanged.
- "extends": an existing narrative, by narrative_id, with the narrative's updated summary.
- "new": a brand-new cluster, with a title and summary.

An event listed only in a cluster's context_indices still has no home: it must be placed in event_indices here. confidence is how sure you are that the events in this element's event_indices are that cluster's work, from 0 to 1.

Return ONLY a JSON array matching this shape, no prose, no markdown fences:
[{"kind":"earlier"|"extends"|"new","cluster_position":<int, only if earlier>,"narrative_id":<int, only if extends>,"title":"..., only if new","summary":"...","confidence":<0.0-1.0>,"event_indices":[3],"context_indices":[1]}]

` + clusterContextRule + `

` + clusterSummaryRule + `

` + clusterGroupingCriterion + `

Prefer joining a cluster that covers the same ticket/branch/PR/topic over opening a new one.`

// reaskResponseItem is the wire shape of one element of a re-ask response. It is
// clusterResponseItem plus cluster_position, which only "earlier" uses. A pointer
// so an "earlier" without one is an error, not a silent join to position 0.
type reaskResponseItem struct {
	Kind            string   `json:"kind"`
	ClusterPosition *int     `json:"cluster_position"`
	NarrativeID     int64    `json:"narrative_id"`
	Title           string   `json:"title"`
	Summary         string   `json:"summary"`
	Confidence      *float64 `json:"confidence"`
	EventIndices    []int    `json:"event_indices"`
	ContextIndices  []int    `json:"context_indices"`
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
		if err := requireOmitted(raw, item.EventIndices, evts, omitted); err != nil {
			return nil, nil, err
		}

		// context_indices are not restricted to the omitted events: a context link
		// moves nothing, so naming an already-placed event as background revises no
		// answer the model was told stands.
		p, err := resolvePlacement("cluster re-ask response", raw, item.EventIndices, item.ContextIndices, item.Confidence, evts)
		if err != nil {
			return nil, nil, err
		}

		merged, err = placeReaskItem(raw, merged, len(first), item, p)
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

// requireOmitted rejects any re-ask event_indices value out of range or not among
// those omitted.
func requireOmitted(raw string, indices []int, evts []Event, omitted []int) error {
	for _, idx := range indices {
		if idx < 0 || idx >= len(evts) {
			return fmt.Errorf("parsing cluster re-ask response %q: event_indices value %d out of range [0,%d)",
				raw, idx, len(evts))
		}
		if !slices.Contains(omitted, idx) {
			return fmt.Errorf("parsing cluster re-ask response %q: event_indices value %d was not one of the omitted indices %v",
				raw, idx, omitted)
		}
	}

	return nil
}

// placeReaskItem puts one re-ask item's events where it says, returning the
// updated results. firstLen bounds cluster_position to the first response: the
// model was shown those positions and no others.
func placeReaskItem(raw string, merged []ClusterResult, firstLen int, item reaskResponseItem, p resolvedPlacement) ([]ClusterResult, error) {
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
		joinCluster(&merged[pos], p, item.Summary)

		return merged, nil

	case wireKindExtends:
		for i := range merged {
			if merged[i].Kind == ClusterExtends && merged[i].NarrativeID == item.NarrativeID {
				joinCluster(&merged[i], p, item.Summary)

				return merged, nil
			}
		}

		return append(merged, ClusterResult{
			Kind: ClusterExtends, NarrativeID: item.NarrativeID,
			Title: item.Title, Summary: item.Summary, Events: p.members,
			ContextEvents: p.context, Confidence: p.confidence,
		}), nil

	case wireKindNew:
		return append(merged, ClusterResult{
			Kind: ClusterNew, Title: item.Title, Summary: item.Summary, Events: p.members,
			ContextEvents: p.context, Confidence: p.confidence,
		}), nil

	default:
		return nil, fmt.Errorf("parsing cluster re-ask response %q: unknown kind %q", raw, item.Kind)
	}
}

// joinCluster adds a re-ask item's events to r, replacing r's summary only when one
// was supplied. The joined members keep the confidence the RE-ASK stated for them,
// recorded as per-event overrides: it is a separate judgment from the one r's own
// confidence describes. absorb copies rather than appends in place, so a merged
// result never shares a backing array with the first response's.
func joinCluster(r *ClusterResult, p resolvedPlacement, summary string) {
	r.MemberConfidence = maps.Clone(r.MemberConfidence)
	absorb(r, p.members, p.context, func(Event) float64 { return p.confidence })
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
