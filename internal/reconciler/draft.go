package reconciler

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/llm"
	"github.com/jcogilvie/unjira/internal/rules"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
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
- "transition": move the issue to a different status. ONLY propose a target listed under "legal transitions" for that issue. If the status you believe is correct is not listed, propose a comment instead.

Report confidence honestly in 0.0-1.0. Do not describe work that is not evidenced in the delta.

Return ONLY a bare JSON array, no prose, no markdown fences:
[{"issue_key":"...","type":"comment"|"transition","body":"...","target_status":"todo"|"in_progress"|"done","confidence":0.0-1.0,"rationale":"..."}]`

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
) ([]ProposedAction, correlator.Stats, error) {
	var stats correlator.Stats

	prompt := buildDraftPrompt(narrative, delta, verified)

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

	byKey := make(map[string]verifiedLink, len(verified))
	for _, v := range verified {
		byKey[v.Link.IssueKey] = v
	}

	var out []ProposedAction
	for _, verdict := range verdicts {
		v, ok := byKey[verdict.IssueKey]
		if !ok {
			// Not verified against the live tracker under this pass, so per
			// rules/verify-correlations.md it is not trusted regardless of
			// stated confidence. Log loudly and skip.
			log.Printf(
				"reconciler: narrative %d draft named unrecognized issue_key %q, ignoring",
				narrative.ID, verdict.IssueKey,
			)

			continue
		}

		out = append(out, toProposedAction(verdict, v))
	}

	return out, stats, nil
}

// toProposedAction converts one verdict, flooring its confidence against
// deterministic facts.
func toProposedAction(verdict draftVerdict, v verifiedLink) ProposedAction {
	action := ProposedAction{
		Type:       ActionType(verdict.Type),
		IssueKey:   verdict.IssueKey,
		Body:       verdict.Body,
		Confidence: verdict.Confidence,
		Rationale:  verdict.Rationale,
	}

	if action.Type == ActionTransition {
		action.TargetStatus = tasktracker.StatusCategory(verdict.TargetStatus)
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
// A transition to a category the live issue does not offer is floored to zero
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

	if slices.Contains(v.AvailableStatus, action.TargetStatus) {
		return confidence
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
func buildDraftPrompt(narrative store.NarrativeRow, delta []events.Event, verified []verifiedLink) string {
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

		if len(v.AvailableStatus) == 0 {
			b.WriteString("  legal transitions: none available — propose a comment, not a transition\n")

			continue
		}

		targets := make([]string, 0, len(v.AvailableStatus))
		for _, c := range v.AvailableStatus {
			targets = append(targets, string(c))
		}
		fmt.Fprintf(&b, "  legal transitions: %s\n", strings.Join(targets, ", "))
	}

	return b.String()
}
