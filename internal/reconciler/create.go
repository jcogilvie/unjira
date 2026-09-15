package reconciler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/llm"
	"github.com/jcogilvie/unjira/internal/logging"
	"github.com/jcogilvie/unjira/internal/rules"
	"github.com/jcogilvie/unjira/internal/store"
)

// createSystemPrompt asks for an issue to open, or for an explicit refusal.
//
// A separate prompt from draftSystemPrompt rather than a fourth action type added
// to it, because the two answer different questions. Drafting asks "given this
// issue and its live state, what should we say about it"; this asks "does this
// work warrant a ticket nobody has filed." Merging them would put an issue_key
// and a legal-transitions list in front of a model that has neither.
//
// The refusal path is the important half. Most unmatched narratives should NOT
// become tickets — a five-minute investigation that went nowhere, a dependency
// bump, a local experiment. If the model cannot decline, unjira turns every
// unrecognized fragment of work into a ticket somebody has to close, which is
// worse than proposing nothing. So "worth_tracking": false is a first-class
// answer, and the prompt names the cases that deserve it.
const createSystemPrompt = `You are deciding whether a body of work that has NO tracker issue deserves one, and if so, drafting it.

You will be shown a narrative: a title, a summary, and the events that make it up. No issue exists for it.

Most work like this should NOT get a new issue. Answer "worth_tracking": false when the work is:
- exploratory or investigative with no lasting outcome
- routine maintenance (dependency bumps, formatting, config tweaks)
- too small to plan around, or already obviously part of something else
- a false start, or an experiment that was abandoned

Answer "worth_tracking": true only when the work is substantial, has a lasting outcome, and a team planning their next cycle would want it visible. An issue nobody will act on is worse than no issue.

When true, write a summary line an engineer would recognize (not a restatement of the title) and a description covering what was done and why it matters.

Return ONLY a bare JSON object, no prose, no markdown fences:
{"worth_tracking":true|false,"summary":"...","description":"...","confidence":0.0-1.0,"rationale":"..."}`

// StatusDeclined marks a create the MODEL judged not worth tracking.
//
// A distinct status from store.StatusRejected, which is a HUMAN's ruling:
// conflating them would let slice 7's rules.Distill learn from unjira's own
// refusals as though a reviewer had made them, teaching the model from its
// own output. It also keeps `actions list --status rejected` meaning "a
// person said no".
//
// The row exists to be a memory. Without it a declined narrative is re-judged on
// every pass forever — measured before this existed, one LLM call per untracked
// narrative per watch tick:
//
//	pass 1: proposed=0  cumulative LLM calls=1
//	pass 2: proposed=0  cumulative LLM calls=2
//	pass 3: proposed=0  cumulative LLM calls=3
//
// with no memory of the refusal. Remembering the answer is the whole fix: a
// narrative unjira has already judged costs nothing until something about it
// changes.
//
// An alias for store.StatusDeclined (see actionstatus.go), for the same
// ergonomic reason as StatusProposed above: this package already spells
// StatusDeclined unqualified in several places, including its own tests.
const StatusDeclined = store.StatusDeclined

// createVerdict is the model's answer.
type createVerdict struct {
	WorthTracking bool    `json:"worth_tracking"`
	Summary       string  `json:"summary"`
	Description   string  `json:"description"`
	Confidence    float64 `json:"confidence"`
	Rationale     string  `json:"rationale"`
}

// ProposeCreates drafts a create action for each narrative that matched no issue
// at all.
//
// This is the actual fix for "the drafting prompt never offers create". That
// framing was wrong, and the probe is worth keeping because the wrong fix looked
// obvious: an unlinked narrative is EXCLUDED by NarrativesWithActionableLinks, so
// reconcileOne is never invoked for it —
//
//	NarrativesWithActionableLinks (reconciler's backlog) -> 0 rows
//	NarrativesWithoutPrimaryLink (matching's backlog)    -> 1 rows
//
// meaning adding "create" to draftSystemPrompt would have changed nothing at all.
// The missing piece was a selection path, not a prompt option.
//
// Deliberately NOT behind a config flag. A create reaches the review queue like
// any other action, and the queue is what gates it: gate.Decide refuses to
// auto-apply a create unless a human set auto_commit["create"].graduated, which
// holds in every configuration a reader might expect to slip through —
//
//	Decide(create @ confidence 1.0, nil)          -> DecisionQueue
//	Decide(create, {comment: graduated=true})     -> DecisionQueue
//	Decide(create, {create: graduated=false})     -> DecisionQueue
//
// A flag guarding PROPOSING would add no protection on top of that, and would
// conflate "unjira suggests something" with "unjira acts" — the distinction the
// review queue exists to draw. The recurring cost that might otherwise justify
// one is handled where it belongs, by remembering declines: see StatusDeclined.
//
// No tracker call anywhere in here, unlike Reconcile: there is no issue to verify.
// That is why this takes no TaskReader — the parameter's absence is the guarantee.
// The verification invariant is not weakened, because it governs proposing against
// state unjira inferred; here there is no prior state to be wrong about, and
// gate.Applier still resolves and checks the target project before writing.
func ProposeCreates(
	ctx context.Context,
	s *store.Store,
	client llm.Client,
	cfg config.ReconcilerConfig,
	learnedRules []rules.Rule,
	log *slog.Logger,
) ([]ReconcileResult, correlator.Stats, error) {
	var stats correlator.Stats

	limit := cfg.NarrativeLimit()

	narratives, err := s.NarrativesWithNoIssueLink(limit + 1)
	if err != nil {
		return nil, stats, fmt.Errorf("listing narratives with no issue link: %w", err)
	}

	if len(narratives) > limit {
		logging.For(log, "reconciler").Warn("untracked narrative cap reached",
			"unmatched_at_least", len(narratives), "examining", limit,
			"config_key", "reconciler.max_narratives_per_pass")
		narratives = narratives[:limit]
	}

	var results []ReconcileResult
	for _, n := range narratives {
		result, oneStats, err := proposeCreateOne(ctx, s, client, n, learnedRules)
		stats.Add(oneStats)
		results = append(results, result)
		if err != nil {
			// Per narrative, matching Reconcile: one narrative's failure must not
			// cost the rest their pass, since "no proposal" is a valid resting
			// state.
			logging.For(log, "reconciler").Warn("proposing a create failed",
				"narrative_id", n.ID, "err", err)
		}
	}

	return results, stats, nil
}

// proposeCreateOne asks whether one untracked narrative deserves an issue.
func proposeCreateOne(
	ctx context.Context,
	s *store.Store,
	client llm.Client,
	narrative store.NarrativeRow,
	learnedRules []rules.Rule,
) (ReconcileResult, correlator.Stats, error) {
	result := ReconcileResult{NarrativeID: narrative.ID}

	var stats correlator.Stats

	// AllNarrativeEvents, not DeltaEvents: there is no prior action to compute a
	// delta against, and a create must describe the whole body of work rather
	// than "what's new" — the issue is being opened for all of it at once.
	evts, err := s.AllNarrativeEvents(narrative.ID)
	if err != nil {
		return result, stats, fmt.Errorf("loading events for narrative %d: %w", narrative.ID, err)
	}

	evts = dropSelfAuthored(evts)
	if len(evts) == 0 {
		// Every event was unjira's own. Proposing a ticket for unjira's own
		// commentary would be the loop dropSelfAuthored exists to prevent.
		result.Suppressed = append(result.Suppressed,
			"every event is unjira's own output; nothing to open an issue about")

		return result, stats, nil
	}

	if existing, err := s.ActionsForNarrative(narrative.ID); err == nil {
		if reason, found := openOrAppliedCreate(existing); found {
			// An already-proposed or already-applied create must not be proposed
			// again: the first would double the review queue, the second would
			// open a duplicate ticket.
			result.Suppressed = append(result.Suppressed, reason)

			return result, stats, nil
		}
	} else {
		// A failed lookup must not silently permit a second create — that is the
		// duplicate-ticket case. Refuse and say why.
		return result, stats, fmt.Errorf(
			"checking existing actions for narrative %d: %w", narrative.ID, err)
	}

	// A previous pass already judged this work not worth tracking. Ask again only
	// if something has changed since — which is exactly what DeltaEvents answers,
	// since it is bounded by max(actions.created_at) and the decline IS an action
	// row. Probed:
	//
	//	delta BEFORE any decline row:   1
	//	delta AFTER the decline row:    0   <- nothing new, do not re-ask
	//	delta AFTER new events arrived: 1   <- ask again
	//
	// So no new column and no new accessor: the mechanism that stops the
	// reconciler re-proposing an unchanged narrative stops this too.
	//
	// Re-asking on change rather than never is the point. A narrative that looked
	// like a ten-minute investigation can become substantial by its fourth day,
	// and a permanent veto would make the first judgment final for work that grew.
	if declined, err := hasDecline(s, narrative.ID); err != nil {
		return result, stats, err
	} else if declined {
		delta, err := s.DeltaEvents(narrative.ID)
		if err != nil {
			return result, stats, fmt.Errorf(
				"computing delta for declined narrative %d: %w", narrative.ID, err)
		}

		if len(dropSelfAuthored(delta)) == 0 {
			result.Suppressed = append(result.Suppressed,
				"already judged not worth tracking, and nothing new has happened since")

			return result, stats, nil
		}
	}

	systemPrompt := createSystemPrompt
	if rendered := rules.Render(learnedRules); rendered != "" {
		systemPrompt += "\n\n" + rendered
	}

	raw, usage, err := client.Complete(ctx, systemPrompt, buildCreatePrompt(narrative, evts))
	if err != nil {
		return result, stats, fmt.Errorf("proposing a create for narrative %d: %w", narrative.ID, err)
	}
	stats.AddUsage(usage)

	verdict, err := parseCreateResponse(raw)
	if err != nil {
		return result, stats, fmt.Errorf("narrative %d: %w", narrative.ID, err)
	}

	if !verdict.WorthTracking {
		// PERSISTED, not merely annotated. The Suppressed line is for this pass's
		// output; the row is what stops the next pass paying to ask again. See
		// StatusDeclined.
		if err := recordDecline(s, narrative.ID, verdict.Rationale); err != nil {
			// Loud: a lost decline is not cosmetic, it restores the
			// re-ask-every-pass cost this row exists to remove.
			return result, stats, err
		}

		result.Suppressed = append(result.Suppressed, fmt.Sprintf(
			"not worth tracking: %s", verdict.Rationale))

		return result, stats, nil
	}

	result.Proposed = append(result.Proposed, ProposedAction{
		Type:       ActionCreate,
		Summary:    verdict.Summary,
		Body:       verdict.Description,
		Confidence: clampConfidence(verdict.Confidence),
		Rationale:  verdict.Rationale,
	})

	return result, stats, nil
}

// recordDecline persists the model's refusal as an action row.
//
// Payload carries the rationale rather than being empty: `actions list` renders
// payloads, so a human asking "why did unjira not file anything for this" gets an
// answer without reading logs. rationale also lands in the Rationale column,
// which is where every other action's reasoning lives.
func recordDecline(s *store.Store, narrativeID int64, rationale string) error {
	payload, err := ActionPayload(ProposedAction{
		Type:    ActionCreate,
		Summary: "declined: not worth tracking",
		Body:    rationale,
	})
	if err != nil {
		return fmt.Errorf("encoding decline for narrative %d: %w", narrativeID, err)
	}

	if _, err := s.InsertAction(store.ActionRow{
		NarrativeID: narrativeID,
		Type:        string(ActionCreate),
		Payload:     payload,
		Rationale:   rationale,
		Status:      StatusDeclined,
	}); err != nil {
		return fmt.Errorf("recording decline for narrative %d: %w", narrativeID, err)
	}

	return nil
}

// hasDecline reports whether a previous pass declined to file for this narrative.
func hasDecline(s *store.Store, narrativeID int64) (bool, error) {
	actions, err := s.ActionsForNarrative(narrativeID)
	if err != nil {
		return false, fmt.Errorf("checking declines for narrative %d: %w", narrativeID, err)
	}

	for _, a := range actions {
		if a.Type == string(ActionCreate) && a.Status == StatusDeclined {
			return true, nil
		}
	}

	return false, nil
}

// openOrAppliedCreate reports whether this narrative already has a create action
// that must not be duplicated, and why.
//
// store.StatusRejected and store.StatusFailed are deliberately NOT blockers. A
// rejected create means a human said no — but they said no to that draft, and
// a later pass with more events may be right; the reviewer can reject again
// cheaply. A failed create wrote nothing, so there is nothing to duplicate.
// Only a live proposal or a successful write blocks.
func openOrAppliedCreate(actions []store.ActionRow) (reason string, found bool) {
	for _, a := range actions {
		if a.Type != string(ActionCreate) {
			continue
		}

		switch a.Status {
		case store.StatusApplied:
			return fmt.Sprintf(
				"action %d already created an issue for this narrative; proposing another would "+
					"open a duplicate", a.ID), true
		case StatusProposed, store.StatusApproved:
			return fmt.Sprintf(
				"action %d already proposes creating an issue for this narrative and is awaiting "+
					"review", a.ID), true
		}
	}

	return "", false
}

// clampConfidence bounds a model-reported score to 0..1.
//
// No floorConfidence equivalent here, and the absence is the point: that function
// lowers a score using deterministic facts about a live issue (is this transition
// legal), and a create has no live issue to check against. Inventing a floor would
// be fabricating rigor. Clamping a malformed number is all that is warranted.
func clampConfidence(c float64) float64 {
	if c < 0 {
		return 0
	}
	if c > 1 {
		return 1
	}

	return c
}

// parseCreateResponse decodes the model's object, requiring a summary when it
// claims the work is worth tracking.
//
// A "worth_tracking": true with no summary is rejected rather than defaulted to
// the narrative title: gate.Applier would send an empty summary to CreateIssue and
// open a blank ticket, and substituting the title would silently produce an issue
// whose text no model actually wrote.
func parseCreateResponse(raw string) (createVerdict, error) {
	// StripJSONFence, matching correlator.checkSameStory — the repo's other
	// object-shaped response. Deliberately NOT JSONArrayPayload: that wraps a bare
	// object INTO an array, which is the opposite of what this prompt asks for.
	cleaned := llm.StripJSONFence(raw)

	var v createVerdict
	if err := json.Unmarshal([]byte(cleaned), &v); err != nil {
		return createVerdict{}, fmt.Errorf(
			"parsing create response %q: %w", truncateForError(cleaned), err)
	}

	if v.WorthTracking && strings.TrimSpace(v.Summary) == "" {
		return createVerdict{}, fmt.Errorf(
			"create response says the work is worth tracking but gives no summary; refusing to "+
				"open a blank issue (response was %q)", truncateForError(cleaned))
	}

	return v, nil
}

// buildCreatePrompt renders the narrative and its full event list.
func buildCreatePrompt(narrative store.NarrativeRow, evts []events.Event) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Narrative: title=%q\nsummary: %q\n\n", narrative.Title, narrative.Summary)
	b.WriteString("Every event in this work (no tracker issue exists for any of it):\n")
	for _, e := range evts {
		fmt.Fprintf(&b, "- [%s] %s: %s\n",
			e.OccurredAt.Format("2006-01-02 15:04"), e.Source, e.Summary)
	}

	return b.String()
}
