package reconciler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
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
	// Destinations names the trackers the work should be ticketed in, asked for only
	// when several are allowed (see chooseDestinations).
	Destinations []string `json:"destinations"`
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
// The selection (store.NarrativesAwaitingCreate) already excludes every narrative
// proposeCreateOne would skip without a model call, and the cap applies after that
// exclusion. Applied before it, the narratives a pass had handled kept the first
// max_narratives_per_pass slots on every later pass and starved everything behind
// them. proposeCreateOne's own checks stay as a backstop: a selector bug here costs
// a duplicate ticket, not a wasted call.
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
	opts ...ReconcileOption,
) ([]ReconcileResult, correlator.Stats, error) {
	var o reconcileOptions
	for _, opt := range opts {
		opt(&o)
	}

	var stats correlator.Stats

	limit := cfg.NarrativeLimit()

	narratives, err := s.NarrativesAwaitingCreate(limit + 1)
	if err != nil {
		return nil, stats, fmt.Errorf("listing narratives awaiting a create decision: %w", err)
	}

	if len(narratives) > limit {
		logging.For(log, "reconciler").Warn("untracked narrative cap reached",
			"unmatched_at_least", len(narratives), "examining", limit,
			"config_key", "reconciler.max_narratives_per_pass")
		narratives = narratives[:limit]
	}

	var results []ReconcileResult
	for _, n := range narratives {
		result, oneStats, err := proposeCreateOne(ctx, s, client, n, learnedRules, o.destinations)
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
	policy DestinationPolicy,
) (ReconcileResult, correlator.Stats, error) {
	result := ReconcileResult{NarrativeID: narrative.ID}

	var stats correlator.Stats

	// AllMemberEvents, not DeltaEvents: there is no prior action to compute a
	// delta against, and a create must describe the whole body of work rather
	// than "what's new" — the issue is being opened for all of it at once.
	// Member events only: a create opens a ticket for THIS narrative's work, and a
	// context event is another narrative's (shared-context spec §2).
	evts, err := s.AllMemberEvents(narrative.ID)
	if err != nil {
		return result, stats, fmt.Errorf("loading events for narrative %d: %w", narrative.ID, err)
	}

	evts = dropSelfAuthored(evts)
	if len(evts) == 0 {
		// Every event was unjira's own. Proposing a ticket for unjira's own
		// commentary would be the loop dropSelfAuthored exists to prevent.
		//
		// RECORDED, because nothing else about this outcome is: no draft exists to
		// persist, and store.NarrativesAwaitingCreate cannot see a Go-side filter. An
		// outcome that leaves no trace under the selector's stable order keeps this
		// narrative's slot on every pass — see store.awaitingCreate.
		if err := s.RecordCreateExamined(
			narrative.ID, "every member event is authored_by_unjira"); err != nil {
			return result, stats, fmt.Errorf(
				"recording create examination for narrative %d: %w", narrative.ID, err)
		}

		result.Suppressed = append(result.Suppressed,
			"every event is unjira's own output; nothing to open an issue about")

		return result, stats, nil
	}

	existing, err := s.ActionsForNarrative(narrative.ID)
	if err != nil {
		// A failed lookup must not silently permit a second create — that is the
		// duplicate-ticket case. Refuse and say why.
		return result, stats, fmt.Errorf(
			"checking existing actions for narrative %d: %w", narrative.ID, err)
	}

	// Destinations, decided before any drafting call. Nil policy: the one configured
	// default, which is the behaviour before destinations existed.
	allowed, done, err := allowedDestinations(s, &result, evts, existing, policy)
	if err != nil || done {
		return result, stats, err
	}

	// A previous pass already judged this work not worth tracking. Ask again only
	// if something has changed since — which is exactly what DeltaEvents answers,
	// since it is bounded by the narrative's latest action (by link sequence —
	// actions.created_link_seq) and the decline IS an action
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
			// An EMPTY delta never reaches here through ProposeCreates (the
			// selector's decline clause excludes it), but a delta made only of
			// unjira's own events does: it passes the decline clause, so it is
			// recorded for the same reason as the all-self-authored case above, or
			// it holds its slot from here on.
			if err := s.RecordCreateExamined(
				narrative.ID, "every member event since the decline is authored_by_unjira"); err != nil {
				return result, stats, fmt.Errorf(
					"recording create examination for narrative %d: %w", narrative.ID, err)
			}

			result.Suppressed = append(result.Suppressed,
				"already judged not worth tracking, and nothing new has happened since")

			return result, stats, nil
		}
	}

	systemPrompt := createSystemPrompt
	if rendered := rules.Render(learnedRules); rendered != "" {
		systemPrompt += "\n\n" + rendered
	}

	raw, usage, err := client.Complete(ctx, systemPrompt, buildCreatePrompt(narrative, evts, allowed))
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

	proposal := ProposedAction{
		Type:       ActionCreate,
		Summary:    verdict.Summary,
		Body:       verdict.Description,
		Confidence: clampConfidence(verdict.Confidence),
		Rationale:  verdict.Rationale,
	}

	if policy == nil {
		result.Proposed = append(result.Proposed, proposal)

		return result, stats, nil
	}

	chosen, rejected := chooseDestinations(allowed, verdict.Destinations)
	result.Suppressed = append(result.Suppressed, rejected...)

	for _, d := range chosen {
		p := proposal
		p.Scope = d.Scope
		result.Proposed = append(result.Proposed, p)
	}

	return result, stats, nil
}

// allowedDestinations computes where this untracked narrative may be ticketed, and
// whether that already settles the narrative (done): an empty set, or every allowed
// scope already holding an open or applied create, is recorded on result and needs no
// drafting call. With no policy it applies the pre-destination rule: one create, unless
// one is already open or applied.
func allowedDestinations(
	s *store.Store, result *ReconcileResult, evts []events.Event, existing []store.ActionRow, policy DestinationPolicy,
) ([]config.Destination, bool, error) {
	if policy == nil {
		if reason, found := openOrAppliedCreate(existing); found {
			// An already-proposed or already-applied create must not be proposed
			// again: the first would double the review queue, the second would
			// open a duplicate ticket.
			result.Suppressed = append(result.Suppressed, reason)

			return nil, true, nil
		}

		return nil, false, nil
	}

	plan := policy.UntrackedDestinations(workLocation(evts))
	if len(plan.Ambiguous) > 0 {
		result.Notes = append(result.Notes, fmt.Sprintf(
			"work location is ambiguous: its repositories route to trackers %s, so it is treated as "+
				"in no tracker's scope", strings.Join(plan.Ambiguous, ", ")))
	}

	if len(plan.Destinations) == 0 {
		// Recorded as a suppression, which is also the watermark DeltaEvents reads;
		// re-recorded only once something new has happened, or every pass would write
		// another row for a narrative nothing changed.
		fresh, err := somethingNewSinceLastAction(s, result.NarrativeID, existing)
		if err != nil {
			return nil, true, err
		}

		if fresh {
			result.Suppressed = append(result.Suppressed, "no allowed destination: "+plan.Reason)
		}

		return nil, true, nil
	}

	// The open-or-applied rule per destination: a create already open or applied in a
	// scope blocks that scope, not the others.
	taken, blocksAll := openCreateScopes(existing)
	allowed := slices.DeleteFunc(plan.Destinations, func(d config.Destination) bool {
		return blocksAll || slices.Contains(taken, d.Scope)
	})

	if len(allowed) == 0 {
		reason, _ := openOrAppliedCreate(existing)
		result.Suppressed = append(result.Suppressed, reason)

		return nil, true, nil
	}

	return allowed, false, nil
}

// somethingNewSinceLastAction reports whether a narrative has work evidence newer than
// its latest action, or has no action at all — the condition for recording a fresh
// suppression rather than repeating one.
func somethingNewSinceLastAction(s *store.Store, narrativeID int64, existing []store.ActionRow) (bool, error) {
	if len(existing) == 0 {
		return true, nil
	}

	delta, err := s.DeltaEvents(narrativeID)
	if err != nil {
		return false, fmt.Errorf("computing delta for narrative %d: %w", narrativeID, err)
	}

	return len(dropSelfAuthored(delta)) > 0, nil
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
	// JSONObjectPayload, matching correlator.checkSameStory — the repo's other
	// object-shaped response. Deliberately NOT JSONArrayPayload: that wraps a bare
	// object INTO an array, which is the opposite of what this prompt asks for.
	cleaned := llm.JSONObjectPayload(raw)

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

// buildCreatePrompt renders the narrative and its full event list, and, when several
// destinations are allowed, asks the model to name the ones the work belongs in.
func buildCreatePrompt(narrative store.NarrativeRow, evts []events.Event, allowed []config.Destination) string {
	var b strings.Builder

	if len(allowed) > 1 {
		fmt.Fprintf(&b, "Allowed destinations: %s. If the work is worth tracking, add "+
			"\"destinations\": [...] to your JSON, naming each tracker it should be ticketed in. "+
			"Name only these.\n\n", destinationNames(allowed))
	}

	fmt.Fprintf(&b, "Narrative: title=%q\nsummary: %q\n\n", narrative.Title, narrative.Summary)
	b.WriteString("Every event in this work (no tracker issue exists for any of it):\n")
	for _, e := range evts {
		fmt.Fprintf(&b, "- [%s] %s: %s\n",
			e.OccurredAt.Format("2006-01-02 15:04"), e.Source, e.Summary)
	}

	return b.String()
}
