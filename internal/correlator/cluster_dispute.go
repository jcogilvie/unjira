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

	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/llm"
	"github.com/jcogilvie/unjira/internal/logging"
	"github.com/jcogilvie/unjira/internal/rules"
)

// The dispute re-ask: what happens when the model places one event in two or more
// clusters' event_indices (docs/superpowers/specs/2026-10-02-shared-context-design.md
// §4, decided 2026-10-02).
//
// Every event has exactly one MEMBER home, because member links are what a reconcile
// pass drafts from and what token attribution charges. Intentional sharing has its
// own channel, context_indices. A second member placement is therefore a contract
// violation, and it must be resolved CORRECTLY rather than consistently: the model is
// asked, in one follow-up pass, which workstream each disputed event is primarily the
// work of. Rejected alternatives:
//
//   - Last writer wins — the old relinkEvents. Order-dependent, and on
//     [NEW{E}, EXTENDS 5{E, F}] it left the new narrative open and empty, with a
//     title and summary describing an event it did not hold (finding F37).
//   - First in response order, the downgrade an earlier draft of the spec
//     recommended. Consistent, cheap, and wrong as often as the model happens to
//     write the eventual home second: response order has no bearing on whose work an
//     event is.
//   - Deterministic evidence applied by code (the event carries a claimant's PR, or
//     its branch). Relevance is the model's call (CLAUDE.md); the evidence is
//     PRESENTED to the model, never applied.
//   - One call per disputed event. One call for the whole pass when the disputes fit
//     the context window together: a dispute is usually a handful of events, and
//     each call re-pays the system prompt.
//
// Run once per Cluster call, after any bisection has merged its halves, so an
// eligible event numbered in both halves and placed differently by each (F36) is a
// dispute like any other. Each loser keeps the event as CONTEXT: it claimed the
// event as its work, which is at least a claim that the event is relevant to it.
//
// The prompt lists every claimant's other member events in full, per dispute, so it
// grows with disputes × claimants × members and can exceed the context window even
// when every clustering call fitted — most likely after a bisection, whose claimants
// span both halves. When the disputes do not fit together, the pass splits them, in
// order, into batches that each fit, one call per batch. Still ONE dispute pass per
// Cluster call: every disputed event is asked about exactly once, and no answer is
// applied until every batch has answered. Splitting is sound because each dispute is
// an independent question — "which of THESE claimants is this event the work of" —
// and nothing in the prompt or the parser relates one dispute to another: each block
// names its claimants by their position in the pass's results (global, so the same
// cluster has the same name in every batch) and describes them in full; each batch
// numbers and parses its own event_index space; and the one cross-dispute rule, a
// NEW cluster must keep a member, is checked by applyDisputeAnswers over all the
// answers at once, exactly as for a single call. Rejected bounds:
//
//   - Refusing whenever the whole set does not fit. Correct but brittle: a pass whose
//     every clustering call fitted would die at a step whose questions each fit.
//   - Capping each listed member summary, or listing at most N members per claimant.
//     Trades the model's view of a claimant for the call fitting, the trade F16
//     measured for context narratives; deciding it needs real dispute sizes first.
//
// A single dispute too large on its own is the one case batching cannot fit, and it
// refuses loudly rather than cut a claimant's description (finding F52).

// disputeRequest is what one dispute pass needs.
type disputeRequest struct {
	window      TimeRange
	results     []ClusterResult
	rules       []rules.Rule
	instruction string

	contextWindowTokens int
	log                 *slog.Logger
}

// dispute is one event with more than one member placement: the event, and the
// result positions that claim it, ascending.
type dispute struct {
	event     Event
	claimants []int
}

// findDisputes returns, in first-appearance order, every event that two or more of
// results hold as a member.
func findDisputes(results []ClusterResult) []dispute {
	positions := make(map[string][]int)
	var order []string
	first := make(map[string]Event)

	for pos, r := range results {
		seenHere := make(map[string]bool, len(r.Events))
		for _, e := range r.Events {
			k := EventKey(e)
			if seenHere[k] {
				continue
			}
			seenHere[k] = true
			if _, ok := positions[k]; !ok {
				order = append(order, k)
				first[k] = e
			}
			positions[k] = append(positions[k], pos)
		}
	}

	var out []dispute
	for _, k := range order {
		if len(positions[k]) > 1 {
			out = append(out, dispute{event: first[k], claimants: positions[k]})
		}
	}

	return out
}

// resolveDisputes makes the dispute pass, if any event has more than one member
// placement, and applies the model's answers: the chosen cluster keeps the member,
// with the answer's per-event confidence; every other claimant gets it as context.
// One call when the disputes fit the context window together, otherwise one per
// batch that fits (batchDisputes). Returns results unchanged, with no call, when
// nothing is disputed.
func resolveDisputes(ctx context.Context, client llm.Client, req disputeRequest) ([]ClusterResult, Stats, error) {
	disputes := findDisputes(req.results)
	if len(disputes) == 0 {
		return req.results, Stats{}, nil
	}

	stats := Stats{DisputedEvents: len(disputes)}

	batches := batchDisputes(req, disputes)
	prompts := make([]disputePrompt, len(batches))
	for i, batch := range batches {
		systemPrompt, userPrompt := buildDisputePrompt(req, batch)
		prompts[i] = disputePrompt{system: systemPrompt, user: userPrompt}
		stats.EstimatedTokens += estimateTokens(systemPrompt + userPrompt)
	}

	logging.For(req.log, "correlator").WarnContext(ctx, "clusters placed one event in several; asking which owns it",
		"disputed_events", len(disputes),
		"disputed", describeDisputes(req.results, disputes),
		"calls", len(batches),
		"est_tokens", stats.EstimatedTokens,
	)

	// Every batch is checked before any is sent, so a pass that cannot finish spends
	// nothing. batchDisputes only leaves a batch over budget when it is one dispute
	// that does not fit alone; the check covers every batch regardless, so no prompt
	// is ever sent over budget whatever the packing believed.
	for i, p := range prompts {
		if err := requireDisputeFits(req, batches[i], p, i, len(batches)); err != nil {
			return nil, stats, err
		}
	}

	answers := make([]disputeAnswer, 0, len(disputes))
	for i, batch := range batches {
		systemPrompt, userPrompt := prompts[i].system, prompts[i].user

		// A batched pass names which call failed; a single call's errors read as they
		// always have.
		inCall := func(err error) error {
			if len(batches) == 1 {
				return err
			}

			return fmt.Errorf("%s: %w", disputeCallLabel(i, len(batches)), err)
		}

		raw, usage, err := client.Complete(ctx, systemPrompt, userPrompt)
		if err != nil {
			return nil, stats, inCall(fmt.Errorf("re-asking which cluster owns %d disputed event(s) in window [%s, %s): %w",
				len(batch), req.window.Start, req.window.End, err))
		}
		stats.AddUsage(usage)
		stats.DisputeCalls++

		batchAnswers, err := parseDisputeResponse(raw, batch)
		if err != nil {
			return nil, stats, inCall(err)
		}
		answers = append(answers, batchAnswers...)
	}

	resolved, resolutions, err := applyDisputeAnswers(req.results, disputes, answers)
	if err != nil {
		return nil, stats, err
	}
	stats.Disputes = resolutions

	logging.For(req.log, "correlator").InfoContext(ctx, "dispute re-ask gave every disputed event one home",
		"disputed_events", len(disputes))

	return resolved, stats, nil
}

const clusterDisputeSystemPrompt = `You clustered events into narratives, and placed some events in the event_indices of more than one cluster. An event's event_indices placement is its home: the ONE narrative whose work it is, which is where its effort is attributed and what that narrative's ticket will describe. An event can be relevant background to several narratives, but it is the work of only one.

For each disputed event below, decide which ONE of the clusters that claimed it the event is primarily the work of. Every other claimant will keep the event as background (a context link), so choosing one loses nothing. Judge by what the event did, not by where it was listed first. Deterministic evidence, where any was found, is listed under the event; weigh it, but it is evidence, not a rule.

Return ONLY a JSON array with one element per disputed event, no prose, no markdown fences. Write the rationale FIRST, then the decision that follows from it:
[{"rationale":"...","event_index":<the disputed event's event_index>,"member":{"cluster_position":<one of that event's claimants>},"confidence":<0.0-1.0>}]

confidence is how sure you are that the event is the chosen cluster's work.

` + clusterGroupingCriterion

// buildDisputePrompt lists each disputed event under its own event_index — the
// dispute's index space, numbered here and resolved by parseDisputeResponse against
// the same disputes slice — with every claimant's cluster_position, title or
// narrative_id, summary and other member events, and any deterministic evidence.
//
// Its own index space rather than the clustering prompt's numbers, deliberately: after
// a bisection there is no single clustering prompt, and the same event carries a
// different number in each half (F36). One slice numbers the prompt and resolves the
// answer, which is the property assignableEvents exists to keep.
func buildDisputePrompt(req disputeRequest, disputes []dispute) (systemPrompt, userPrompt string) {
	systemPrompt = withRulesAndInstruction(clusterDisputeSystemPrompt, req.rules, req.instruction)

	var b strings.Builder
	b.WriteString(disputePromptHeader)
	for i, d := range disputes {
		writeDispute(&b, i, d, req.results)
	}

	return systemPrompt, b.String()
}

const disputePromptHeader = "Disputed events — each was placed in the event_indices of more than one cluster:\n"

// writeDispute renders one disputed event under event_index, with every claimant and
// any evidence. Self-contained: nothing in it depends on any other dispute, which is
// what lets batchDisputes size a batch as the sum of its blocks.
func writeDispute(b *strings.Builder, index int, d dispute, results []ClusterResult) {
	fmt.Fprintf(b, "\nevent_index=%d [%s] %q (occurred_at=%s)\n",
		index, d.event.Source, d.event.Summary, d.event.OccurredAt.Format(time.RFC3339))
	b.WriteString("  claimed by:\n")
	for _, pos := range d.claimants {
		writeClaimant(b, pos, results[pos], d.event)
	}
	writeEvidence(b, d, results)
}

// disputePrompt is one dispute call's prompt pair.
type disputePrompt struct {
	system, user string
}

// batchDisputes splits disputes, in order, into contiguous batches whose dispute
// prompt each fits req.contextWindowTokens: one batch, the whole set, whenever it
// fits. Greedy: a dispute joins the current batch unless that would take it over
// budget, and then starts the next. Never drops or splits a dispute — a dispute too
// large on its own still gets a batch of its own, which requireDisputeFits refuses.
//
// Sized from the lengths of buildDisputePrompt's own parts, rendered with the
// event_index each dispute will carry in its batch, so the arithmetic is the prompt's
// length exactly (estimateTokens depends on length alone) without re-rendering a
// growing batch for every candidate.
func batchDisputes(req disputeRequest, disputes []dispute) [][]dispute {
	systemPrompt := withRulesAndInstruction(clusterDisputeSystemPrompt, req.rules, req.instruction)
	fixed := len(systemPrompt) + len(disputePromptHeader)
	blockLen := func(index int, d dispute) int {
		var b strings.Builder
		writeDispute(&b, index, d, req.results)

		return b.Len()
	}

	// Each batch is disputes[start:i]: contiguous, so concatenating the batches is
	// disputes again, in order, and the answers line up with it.
	var batches [][]dispute
	start, size := 0, fixed
	for i, d := range disputes {
		n := blockLen(i-start, d)
		if i > start && estimateTokensOfLen(size+n) > req.contextWindowTokens {
			batches = append(batches, disputes[start:i])
			start, size = i, fixed
			n = blockLen(0, d)
		}
		size += n
	}

	return append(batches, disputes[start:])
}

// requireDisputeFits refuses a dispute call whose prompt is over the context window,
// before anything is sent. After batchDisputes that is one dispute whose claimants'
// member events, listed in full, exceed the window alone: nothing smaller can be
// asked without cutting a claimant's description, which would trade the model's view
// of a claimant for the call fitting (finding F52).
func requireDisputeFits(req disputeRequest, batch []dispute, p disputePrompt, call, calls int) error {
	estimated := estimateTokens(p.system + p.user)
	if estimated <= req.contextWindowTokens {
		return nil
	}

	if len(batch) == 1 {
		return fmt.Errorf(
			"clustering events in window [%s, %s): an event was placed in more than one cluster (%s), and the "+
				"re-ask resolving it alone is estimated at %d tokens, over the %d-token context window. The re-ask "+
				"lists each claimant's member events in full and never truncates them (finding F52)",
			req.window.Start, req.window.End, describeDisputes(req.results, batch),
			estimated, req.contextWindowTokens)
	}

	return fmt.Errorf(
		"clustering events in window [%s, %s): %s: %d event(s) were placed in more than one cluster (%s), and the "+
			"re-ask resolving them is estimated at %d tokens, over the %d-token context window",
		req.window.Start, req.window.End, disputeCallLabel(call, calls), len(batch),
		describeDisputes(req.results, batch), estimated, req.contextWindowTokens)
}

// disputeCallLabel names one call of the dispute pass in an error, 1-based.
func disputeCallLabel(call, calls int) string {
	return fmt.Sprintf("dispute re-ask call %d of %d", call+1, calls)
}

// writeClaimant renders one claiming cluster by position, with its other members.
func writeClaimant(b *strings.Builder, pos int, r ClusterResult, disputed Event) {
	fmt.Fprintf(b, "    cluster_position=%d kind=%s", pos, clusterKindWire(r.Kind))
	if r.Kind == ClusterExtends {
		fmt.Fprintf(b, " narrative_id=%d", r.NarrativeID)
	} else {
		fmt.Fprintf(b, " title=%q", r.Title)
	}
	fmt.Fprintf(b, " summary=%q\n", r.Summary)

	others := 0
	for _, e := range r.Events {
		if EventKey(e) == EventKey(disputed) {
			continue
		}
		if others == 0 {
			b.WriteString("      its other member events:\n")
		}
		others++
		fmt.Fprintf(b, "        - [%s] %q (occurred_at=%s)\n", e.Source, e.Summary, e.OccurredAt.Format(time.RFC3339))
	}
	if others == 0 {
		b.WriteString("      (no other member events)\n")
	}
}

// writeEvidence lists the deterministic connections the event's own artifacts make to
// a claimant: the pull request it carries (a PR-creation anchor names the PR its tool
// call created), or the git branch it recorded matching a claimant's pull request's
// head branch. Presented, never applied: the model weighs it.
//
// Lineage — a subagent's dispatching root segment — is not here. It is the shared-
// context design's second slice, which gives the subagent artifacts (F6) their
// reader; this slice deliberately reads none of them.
func writeEvidence(b *strings.Builder, d dispute, results []ClusterResult) {
	var lines []string
	pr := events.PullRequestOf(d.event)
	branch := stringArtifact(d.event, events.ArtifactGitBranch)

	for _, pos := range d.claimants {
		for _, e := range results[pos].Events {
			if EventKey(e) == EventKey(d.event) {
				continue
			}
			otherPR := events.PullRequestOf(e)
			if otherPR == "" {
				continue
			}
			if pr != "" && pr == otherPR {
				lines = append(lines, fmt.Sprintf(
					"this event carries pull request %s, and so does cluster_position=%d's [%s] event %q",
					pr, pos, e.Source, e.ExternalID))
			}
			if branch != "" && branch == stringArtifact(e, events.ArtifactGitBranch) {
				lines = append(lines, fmt.Sprintf(
					"this event recorded git branch %q, the head branch of pull request %s in cluster_position=%d",
					branch, otherPR, pos))
			}
		}
	}

	slices.Sort(lines)
	lines = slices.Compact(lines)

	b.WriteString("  evidence:\n")
	if len(lines) == 0 {
		b.WriteString("    (none found)\n")

		return
	}
	for _, l := range lines {
		fmt.Fprintf(b, "    - %s\n", l)
	}
}

// stringArtifact reads a string artifact, tolerating absence and the wrong type as
// "not present", the same degradation every other artifact reader here takes.
func stringArtifact(e Event, key string) string {
	s, _ := e.Artifacts[key].(string)

	return s
}

// disputeAnswer is one resolved dispute: which claimant owns the event.
type disputeAnswer struct {
	position   int
	confidence float64
	rationale  string
}

// disputeResponseItem is the wire shape of one dispute answer. Pointers so an omitted
// field is an error rather than a silent zero — a missing cluster_position must not
// quietly mean position 0.
type disputeResponseItem struct {
	Rationale  string `json:"rationale"`
	EventIndex *int   `json:"event_index"`
	Member     *struct {
		ClusterPosition *int `json:"cluster_position"`
	} `json:"member"`
	Confidence *float64 `json:"confidence"`
}

// parseDisputeResponse resolves raw against disputes, the slice the prompt numbered.
// Every malformation is a loud error quoting raw: invalid JSON, an event_index out of
// range or answered twice, a member that did not claim the event, a missing rationale
// or confidence, and any disputed event left unanswered. Never best-effort: an
// unanswered dispute has no correct default, which is the reason this call exists.
func parseDisputeResponse(raw string, disputes []dispute) ([]disputeAnswer, error) {
	var items []disputeResponseItem
	if err := json.Unmarshal([]byte(llm.JSONArrayPayload(raw)), &items); err != nil {
		return nil, fmt.Errorf("parsing cluster dispute response %q: %w", raw, err)
	}

	answers := make([]disputeAnswer, len(disputes))
	answered := make([]bool, len(disputes))

	for _, item := range items {
		if item.EventIndex == nil {
			return nil, fmt.Errorf("parsing cluster dispute response %q: an answer has no event_index", raw)
		}
		idx := *item.EventIndex
		if idx < 0 || idx >= len(disputes) {
			return nil, fmt.Errorf("parsing cluster dispute response %q: event_index %d out of range [0,%d)",
				raw, idx, len(disputes))
		}
		if answered[idx] {
			return nil, fmt.Errorf("parsing cluster dispute response %q: event_index %d answered twice", raw, idx)
		}
		if item.Member == nil || item.Member.ClusterPosition == nil {
			return nil, fmt.Errorf("parsing cluster dispute response %q: event_index %d has no member.cluster_position",
				raw, idx)
		}
		pos := *item.Member.ClusterPosition
		if !slices.Contains(disputes[idx].claimants, pos) {
			return nil, fmt.Errorf(
				"parsing cluster dispute response %q: event_index %d names cluster_position %d, which did not "+
					"claim it (claimants %v)", raw, idx, pos, disputes[idx].claimants)
		}
		if item.Confidence == nil || *item.Confidence < 0 || *item.Confidence > 1 {
			return nil, fmt.Errorf("parsing cluster dispute response %q: event_index %d needs a confidence in [0, 1]",
				raw, idx)
		}
		if strings.TrimSpace(item.Rationale) == "" {
			return nil, fmt.Errorf("parsing cluster dispute response %q: event_index %d has no rationale", raw, idx)
		}

		answers[idx] = disputeAnswer{position: pos, confidence: *item.Confidence, rationale: item.Rationale}
		answered[idx] = true
	}

	var missing []string
	for i, ok := range answered {
		if !ok {
			e := disputes[i].event
			missing = append(missing, fmt.Sprintf("%d (%s/%s)", i, e.Source, e.ExternalID))
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("parsing cluster dispute response %q: %d disputed event(s) left unresolved: %s",
			raw, len(missing), strings.Join(missing, ", "))
	}

	return answers, nil
}

// applyDisputeAnswers moves each disputed event out of every losing claimant's members
// into its context, and records the winner's per-event confidence. Copies results
// rather than mutating them.
//
// A NEW cluster left with no member events is an error naming it and the clusters that
// won its events: an empty new narrative is F37's failure exactly, and the model's
// answers produced it, so the response is malformed rather than something to repair
// by dropping the cluster (which would discard its context links and summary) or by
// overruling the model.
func applyDisputeAnswers(
	results []ClusterResult, disputes []dispute, answers []disputeAnswer,
) ([]ClusterResult, []DisputeResolution, error) {
	out := make([]ClusterResult, len(results))
	for i, r := range results {
		r.Events = slices.Clone(r.Events)
		r.ContextEvents = slices.Clone(r.ContextEvents)
		r.MemberConfidence = maps.Clone(r.MemberConfidence)
		out[i] = r
	}

	resolutions := make([]DisputeResolution, 0, len(disputes))
	wonFrom := make(map[int][]int) // loser position -> winner positions
	for i, d := range disputes {
		a := answers[i]
		k := EventKey(d.event)

		for _, pos := range d.claimants {
			if pos == a.position {
				continue
			}
			loser := &out[pos]
			loser.Events = slices.DeleteFunc(loser.Events, func(e Event) bool { return EventKey(e) == k })
			delete(loser.MemberConfidence, k)
			if !slices.ContainsFunc(loser.ContextEvents, func(e Event) bool { return EventKey(e) == k }) {
				loser.ContextEvents = append(loser.ContextEvents, d.event)
			}
			wonFrom[pos] = append(wonFrom[pos], a.position)
		}

		winner := &out[a.position]
		if winner.MemberConfidence == nil {
			winner.MemberConfidence = make(map[string]float64)
		}
		winner.MemberConfidence[k] = a.confidence

		claimants := make([]string, 0, len(d.claimants))
		for _, pos := range d.claimants {
			claimants = append(claimants, clusterLabel(results[pos]))
		}
		resolutions = append(resolutions, DisputeResolution{
			Event:      d.event.Source + "/" + d.event.ExternalID,
			Claimants:  claimants,
			Chosen:     clusterLabel(results[a.position]),
			Confidence: a.confidence,
			Rationale:  a.rationale,
		})
	}

	for pos, r := range out {
		if r.Kind != ClusterNew || len(r.Events) > 0 {
			continue
		}
		winners := slices.Compact(slices.Sorted(slices.Values(wonFrom[pos])))
		labels := make([]string, 0, len(winners))
		for _, w := range winners {
			labels = append(labels, fmt.Sprintf("cluster_position=%d (%s)", w, clusterLabel(results[w])))
		}

		return nil, nil, fmt.Errorf(
			"clustering: resolving disputed events left new cluster_position=%d (%s) with no member events — "+
				"every event it claimed went to %s. A new narrative must hold at least one event as its own work "+
				"(finding F37)", pos, clusterLabel(r), strings.Join(labels, ", "))
	}

	return out, resolutions, nil
}

// clusterLabel names a cluster for a person reading a report or an error: by
// narrative id when it extends one, by title when it is new.
func clusterLabel(r ClusterResult) string {
	if r.Kind == ClusterExtends {
		return fmt.Sprintf("narrative %d", r.NarrativeID)
	}

	return fmt.Sprintf("new %q", r.Title)
}

// describeDisputes names each disputed event and its claimants, for a log line or an
// error a person reads afterwards.
func describeDisputes(results []ClusterResult, disputes []dispute) string {
	parts := make([]string, 0, len(disputes))
	for _, d := range disputes {
		claimants := make([]string, 0, len(d.claimants))
		for _, pos := range d.claimants {
			claimants = append(claimants, clusterLabel(results[pos]))
		}
		parts = append(parts, fmt.Sprintf("%s/%s claimed by %s",
			d.event.Source, d.event.ExternalID, strings.Join(claimants, " and ")))
	}

	return strings.Join(parts, "; ")
}
