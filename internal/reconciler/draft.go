package reconciler

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/llm"
	"github.com/jcogilvie/unjira/internal/rules"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/workflow"
)

// draftSystemPrompt instructs the model to produce one action per actionable
// link. Worded to match types.go's ActionType doc comments so the two cannot
// drift apart.
const draftSystemPrompt = `You are drafting proposed updates to tracker issues from a record of work that was actually done.

You will be shown WHAT'S NEW since a reviewer last saw this work (the delta — never the full history), and every issue the work is attributed to, each with its role and its CURRENT live state.

Draft exactly one action per issue shown. Roles:
- "primary": the record the work is principally tracked in. Write for an engineering audience.
- "same_work": the same body of work recorded again for a different audience (for example a change-management ticket paired with an engineering one). Write for THAT audience — do not repeat the primary's text.

Action types:
- "comment": post prose describing what changed. The default.
- "transition": move the issue to a different status. ONLY propose a target listed under "reachable statuses" for that issue, spelled EXACTLY as listed. If the status you believe is correct is not listed, propose a comment instead. Some listed statuses are several workflow steps away; propose the one the evidence supports and unjira will walk the intermediate steps itself.

When to propose a transition. A transition needs evidence in the delta that the WORK reached a new stage:
- the delta shows substantive work on the issue's subject, and the issue is not yet in a working status -> propose the working status (e.g. "In Progress")
- the delta shows a pull request opened or marked ready for review -> propose the review status (e.g. "In Review")
- the delta shows the work blocked on something external -> propose the blocked status, if one is listed

Evidence of work is the ONLY basis for a transition. The issue's current status is never itself a reason to move it: current status tells you a move is unnecessary, not that one is due. In particular, do not propose a transition merely because the current status looks early or looks stale.

Do not restate a status change somebody else already made. If the delta contains a status change, that move is already done; treat it as context for what to say, never as the subject of what you say, and never propose repeating or reversing it.

Report confidence honestly in 0.0-1.0. Do not describe work that is not evidenced in the delta.

Return ONLY a bare JSON array, no prose, no markdown fences:
[{"issue_key":"...","type":"comment"|"transition","body":"...","target_status":"<exact status name from legal transitions>","confidence":0.0-1.0,"rationale":"..."}]`

// draftVerdict is one entry of the model's response.
type draftVerdict struct {
	IssueKey     string  `json:"issue_key"`
	Type         string  `json:"type"`
	Body         string  `json:"body"`
	TargetStatus string  `json:"target_status"`
	Confidence   float64 `json:"confidence"`
	Rationale    string  `json:"rationale"`
}

// draft makes ONE LLM call for the whole narrative and returns one
// ProposedAction per recognized, actionable link.
//
// One call rather than one per link is deliberate: the same_work case requires
// each draft to be written in awareness of the others (so the two audiences get
// genuinely different text), which a per-link call cannot do.
//
// learnedRules is appended to the system prompt when non-empty (see
// rules.Render), following internal/correlator's classifyCandidates pattern
// exactly: draft trusts that its caller (Reconcile, ultimately internal/
// pipeline) already filtered to rules.ScopeReconciler via rules.ForScope —
// it does not filter or re-check scope itself, the same way classifyCandidates
// does not re-check rules.ScopeCorrelator on what WithRules handed it.
func draft(
	ctx context.Context,
	client llm.Client,
	narrative store.NarrativeRow,
	delta []events.Event,
	verified []verifiedLink,
	learnedRules []rules.Rule,
	graph *workflow.Graph,
) ([]ProposedAction, correlator.Stats, error) {
	var stats correlator.Stats

	prompt := buildDraftPrompt(narrative, delta, verified, graph)

	systemPrompt := draftSystemPrompt
	if rendered := rules.Render(learnedRules); rendered != "" {
		systemPrompt += "\n\n" + rendered
	}

	raw, usage, err := client.Complete(ctx, systemPrompt, prompt)
	if err != nil {
		return nil, stats, fmt.Errorf("drafting actions for narrative %d: %w", narrative.ID, err)
	}
	// AddUsage already counts the call (s.Calls++ is inside it) — an
	// additional stats.Calls++ here would double-count a single completion.
	stats.AddUsage(usage)

	verdicts, err := parseDraftResponse(raw)
	if err != nil {
		return nil, stats, err
	}

	return actionsFromVerdicts(narrative.ID, verdicts, verified, "draft", graph), stats, nil
}

// toProposedAction converts one verdict, flooring its confidence against
// deterministic facts.
func toProposedAction(verdict draftVerdict, v verifiedLink, graph *workflow.Graph) ProposedAction {
	action := ProposedAction{
		Type:       ActionType(verdict.Type),
		IssueKey:   verdict.IssueKey,
		Body:       verdict.Body,
		Confidence: verdict.Confidence,
		Rationale:  verdict.Rationale,
	}

	if action.Type == ActionTransition {
		action.TargetStatus = strings.TrimSpace(verdict.TargetStatus)

		// The route is resolved HERE, before flooring, because floorConfidence's
		// verdict depends on it: a multi-hop target is by definition not in the
		// live-legal set, so flooring against that set alone zeroes every
		// legitimate multi-hop action.
		//
		// Resolving it in a later pass and flooring here is the bug this ordering
		// prevents — and it is invisible to a unit test that calls either function
		// directly, since each is correct in isolation. Only the end-to-end path
		// shows it.
		if route, ok := resolveRoute(v, graph, action.TargetStatus); ok {
			action.Route = route
			// The route's last hop IS the target, in the spelling the backend
			// agrees with, which may differ in casing from what the model said.
			action.TargetStatus = route[len(route)-1]
		}
	}

	action.Confidence = floorConfidence(action, v)

	return action
}

// floorConfidence caps the model's self-reported score using facts this
// package established.
//
// The model's number is evidence, not a verdict — rules/verify-correlations.md:
// "Confidence scores are not enough; a hallucinated match can be
// high-confidence." So deterministic checks can only ever LOWER it, never
// raise it.
//
// A transition to a status the live issue does not offer is floored to zero
// rather than merely reduced: the backend would refuse it outright, so there is
// no confidence level at which proposing it is correct.
func floorConfidence(action ProposedAction, v verifiedLink) float64 {
	confidence := action.Confidence

	// Clamp a model that returned something outside the documented range.
	if confidence < 0 {
		confidence = 0
	}
	if confidence > 1 {
		confidence = 1
	}

	if action.Type != ActionTransition {
		return confidence
	}

	// A resolved route is strictly stronger evidence than the check below: it
	// confirms a route to the target exists AND that its first hop is live-legal
	// (see resolveRoute's two rules). Re-testing the target against the live set
	// alone would floor every legitimate multi-hop action to zero, since a
	// multi-hop target is by definition not directly offered.
	if len(action.Route) > 0 {
		return confidence
	}

	// No route resolved, which with a graph present means the target is
	// unreachable. Fall back to the live set anyway: without a graph (the Redraft
	// path passes nil) that IS the whole answer. Compared case-insensitively for
	// the same reason the Jira backend matches that way — the name round-trips
	// through a model response, and refusing a legal move over casing would be a
	// self-inflicted false negative.
	for _, name := range v.targetNames() {
		if strings.EqualFold(strings.TrimSpace(name), action.TargetStatus) {
			return confidence
		}
	}

	return 0
}

// parseDraftResponse decodes the model's JSON array, tolerating both a markdown
// fence and a lone object where an array was requested — see
// llm.JSONArrayPayload for why each counts as a property of the interface rather
// than a prompt bug. A narrative with exactly one actionable link is precisely
// the case that provokes the unwrapped form, and that is the common case here.
func parseDraftResponse(raw string) ([]draftVerdict, error) {
	cleaned := llm.JSONArrayPayload(raw)

	var verdicts []draftVerdict
	if err := json.Unmarshal([]byte(cleaned), &verdicts); err != nil {
		return nil, fmt.Errorf("parsing draft response %q: %w", truncateForError(cleaned), err)
	}

	for i, v := range verdicts {
		switch ActionType(v.Type) {
		case ActionComment, ActionTransition, ActionCreate:
		default:
			return nil, fmt.Errorf(
				"draft response entry %d has unknown action type %q: must be one of comment, transition, create",
				i, v.Type,
			)
		}
		if v.IssueKey == "" && ActionType(v.Type) != ActionCreate {
			return nil, fmt.Errorf("draft response entry %d has no issue_key", i)
		}
	}

	return verdicts, nil
}

// truncateForError bounds a model response embedded in an error message so a
// pathological or runaway completion can't blow up a log line or a returned
// error's size. Caps at maxErrorSnippet runes (not bytes) and slices on a
// rune boundary via utf8.RuneCountInString/range, so truncating cannot split
// a multi-byte UTF-8 sequence and produce invalid UTF-8 in the message.
func truncateForError(s string) string {
	const maxErrorSnippet = 300

	if utf8.RuneCountInString(s) <= maxErrorSnippet {
		return s
	}

	var b strings.Builder
	for i, r := range s {
		if i >= maxErrorSnippet {
			break
		}
		b.WriteRune(r)
	}

	return b.String() + "...(truncated)"
}

// buildDraftPrompt renders the delta and each link's live state.
//
// The delta only — never the full cumulative narrative — so a reviewer sees
// "what's new since you last saw this" rather than a repeated history.
func buildDraftPrompt(
	narrative store.NarrativeRow, delta []events.Event, verified []verifiedLink, graph *workflow.Graph,
) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Narrative: title=%q\nsummary: %q\n\n", narrative.Title, narrative.Summary)

	b.WriteString("What's new since the last proposal (the delta):\n")
	for _, e := range delta {
		fmt.Fprintf(&b, "- [%s] %s: %s\n",
			e.OccurredAt.Format("2006-01-02 15:04"), e.Source, e.Summary)
	}

	b.WriteString("\nIssues this work is attributed to:\n")
	for _, v := range verified {
		fmt.Fprintf(&b, "- issue_key=%s role=%s\n", v.Link.IssueKey, v.Link.Role)
		fmt.Fprintf(&b, "  current status: %s\n", v.Issue.StatusName)
		fmt.Fprintf(&b, "  summary: %q\n", v.Issue.Summary)
		fmt.Fprintf(&b, "  description: %q\n", v.Issue.Description)

		// Reachable, not merely live-legal: the live set names only the NEXT hop,
		// and unjira has no guaranteed run cadence, so one delta routinely holds
		// evidence for a journey across several statuses. See reachableTargets.
		reachable := reachableTargets(v, graph)
		if len(reachable) == 0 {
			b.WriteString("  reachable statuses: none — propose a comment, not a transition\n")

			continue
		}

		// Names, quoted, because they contain spaces and must be reproduced
		// exactly: the applier passes the string straight through to the
		// backend, which matches on it.
		targets := make([]string, 0, len(reachable))
		for _, name := range reachable {
			targets = append(targets, strconv.Quote(name))
		}
		fmt.Fprintf(&b, "  reachable statuses: %s\n", strings.Join(targets, ", "))
	}

	return b.String()
}

// Redraft re-runs drafting for one narrative with a reviewer's free-text
// correction added to the prompt. It is `triage`'s [e]dit disposition.
//
// It deliberately reuses the caller's already-verified links rather than
// re-verifying: live tracker state was confirmed earlier in this same pass, so
// the "never propose without verifying" invariant (rules/intent-not-outcome.md)
// still holds, and a second round-trip would cost a Jira call to learn what we
// just learned. The tradeoff is a narrow staleness window — if the issue
// changed in the seconds since verification, this redraft is against slightly
// old state. That is the same window every action in the batch already has
// between propose and apply, so Redraft introduces no new exposure.
//
// Empty feedback is an error, not a no-op: Redraft exists to carry a
// correction, so blank text means the caller dropped the reviewer's words
// somewhere upstream. Failing loudly beats spending an LLM call to regenerate
// the draft the reviewer just rejected.
func Redraft(
	ctx context.Context,
	client llm.Client,
	narrative store.NarrativeRow,
	delta []events.Event,
	verified []verifiedLink,
	learnedRules []rules.Rule,
	feedback string,
) ([]ProposedAction, correlator.Stats, error) {
	if strings.TrimSpace(feedback) == "" {
		return nil, correlator.Stats{}, fmt.Errorf(
			"redrafting narrative %d: reviewer feedback is empty", narrative.ID)
	}

	var stats correlator.Stats

	// nil graph: Redraft is triage's [e]dit, where a human is correcting the action
	// in front of them. Offering statuses several hops away would answer a specific
	// request by widening its scope, and the reviewer has the live state on screen.
	// Single-hop here is the right answer, not a missing feature.
	prompt := buildDraftPrompt(narrative, delta, verified, nil) +
		"\n\nThe reviewer rejected the previous draft with this correction. " +
		"Address it directly:\n" + feedback

	systemPrompt := draftSystemPrompt
	if rendered := rules.Render(learnedRules); rendered != "" {
		systemPrompt += "\n\n" + rendered
	}

	raw, usage, err := client.Complete(ctx, systemPrompt, prompt)
	if err != nil {
		return nil, stats, fmt.Errorf("redrafting narrative %d: %w", narrative.ID, err)
	}
	stats.AddUsage(usage)

	verdicts, err := parseDraftResponse(raw)
	if err != nil {
		return nil, stats, fmt.Errorf("redrafting narrative %d: %w", narrative.ID, err)
	}

	// Same verdict->action mapping draft() uses, including the
	// unrecognized-issue_key skip and toProposedAction's confidence flooring.
	// Extract draft()'s loop into a shared helper and call it from both rather
	// than copying it here — a divergence would mean triage's redrafts floor
	// confidence differently from watch's drafts, which nothing would catch.
	return actionsFromVerdicts(narrative.ID, verdicts, verified, "redraft", nil), stats, nil
}

// actionsFromVerdicts maps the model's verdicts onto ProposedActions, dropping
// any that names an issue not verified against the live tracker under this pass.
//
// Shared by draft and Redraft deliberately. Both need the identical mapping,
// including toProposedAction's confidence flooring, and two copies would drift
// — a redraft flooring confidence differently from a draft is the kind of
// divergence no test would notice until a graduated action applied at the wrong
// confidence.
//
// An unrecognized issue_key is logged and skipped rather than failing the whole
// narrative: per rules/verify-correlations.md it is not trusted regardless of
// stated confidence, but one hallucinated key should not discard the verdicts
// that were fine.
func actionsFromVerdicts(
	narrativeID int64, verdicts []draftVerdict, verified []verifiedLink, what string,
	graph *workflow.Graph,
) []ProposedAction {
	byKey := make(map[string]verifiedLink, len(verified))
	for _, v := range verified {
		byKey[v.Link.IssueKey] = v
	}

	var out []ProposedAction
	for _, verdict := range verdicts {
		v, ok := byKey[verdict.IssueKey]
		if !ok {
			log.Printf(
				"reconciler: narrative %d %s named unrecognized issue_key %q, ignoring",
				narrativeID, what, verdict.IssueKey,
			)

			continue
		}

		out = append(out, toProposedAction(verdict, v, graph))
	}

	return out
}
