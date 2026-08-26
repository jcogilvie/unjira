package reconciler

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/llm"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// selectionRoles is every role a narrative_issues row can carry. Reconcile
// selects on this superset — NOT the narrower actionable set actionableLinks
// filters to — so a `mentioned`-only narrative still gets a ReconcileResult
// row documenting "considered, nothing to do", distinguishing it from a
// narrative with no link at all. A genuinely unlinked narrative has no
// narrative_issues row of any role, so NarrativesWithActionableLinks excludes
// it regardless of which roles are passed: it is matching's backlog, not the
// reconciler's, and must not appear in results at all.
//
// actionableLinks (below) is the actual, narrower "may receive an action"
// definition — primary and same_work only, never mentioned — applied
// per-narrative once a candidate has already been selected.
var selectionRoles = []store.Role{
	correlator.RolePrimary,
	correlator.RoleSameWork,
	correlator.RoleMentioned,
}

// Reconcile drafts proposed actions for narratives with at least one
// actionable issue link and at least one event newer than their last action.
//
// Acquires no lease; the caller holds one, matching RunNarrate's convention
// for Cluster/Persist/Match. Failures are per narrative via errors.Join: one
// narrative's tracker outage leaves it unreconciled and the rest still get a
// chance, since "no proposal" is a valid resting state, so a failed pass costs
// a retry and nothing else.
func Reconcile(
	ctx context.Context,
	s *store.Store,
	tracker tasktracker.TaskReader,
	client llm.Client,
	cfg config.ReconcilerConfig,
) ([]ReconcileResult, correlator.Stats, error) {
	limit := cfg.NarrativeLimit()

	narratives, err := s.NarrativesWithActionableLinks(limit+1, selectionRoles)
	if err != nil {
		return nil, correlator.Stats{}, fmt.Errorf("listing narratives with actionable links: %w", err)
	}

	// Reaching the cap is logged, never silent: a silent cap presents as a
	// clean pass that quietly ignored work.
	if len(narratives) > limit {
		log.Printf(
			"reconciler: %d or more narratives are eligible but this pass examines %d "+
				"(reconciler.max_narratives_per_pass); the remainder wait for the next pass",
			len(narratives), limit,
		)
		narratives = narratives[:limit]
	}

	var (
		results []ReconcileResult
		stats   correlator.Stats
		errs    error
	)

	for _, n := range narratives {
		result, oneStats, err := reconcileOne(ctx, s, tracker, client, n, cfg)
		stats.Add(oneStats)
		results = append(results, result)
		if err != nil {
			errs = errors.Join(errs, err)
		}
	}

	return results, stats, errs
}

// reconcileOne handles a single narrative: load links, compute the delta,
// verify every link against the live tracker, then draft.
//
// Order matters and is load-bearing. The delta check precedes verification,
// which precedes drafting: an unchanged narrative must cost neither a tracker
// read nor an LLM call, and nothing may be drafted against state this pass has
// not confirmed (rules/intent-not-outcome.md, rules/verify-correlations.md).
func reconcileOne(
	ctx context.Context,
	s *store.Store,
	tracker tasktracker.TaskReader,
	client llm.Client,
	narrative store.NarrativeRow,
	cfg config.ReconcilerConfig,
) (ReconcileResult, correlator.Stats, error) {
	result := ReconcileResult{NarrativeID: narrative.ID}

	links, err := s.NarrativeIssues(narrative.ID)
	if err != nil {
		return result, correlator.Stats{}, fmt.Errorf("loading links for narrative %d: %w", narrative.ID, err)
	}

	actionable := actionableLinks(links)
	if len(actionable) == 0 {
		// Either no links at all (matching's concern) or only `mentioned`
		// links, which by definition get nothing.
		return result, correlator.Stats{}, nil
	}

	delta, err := s.DeltaEvents(narrative.ID)
	if err != nil {
		return result, correlator.Stats{}, fmt.Errorf("computing delta for narrative %d: %w", narrative.ID, err)
	}

	delta = dropSelfAuthored(delta)
	if len(delta) == 0 {
		result.SkippedNoDelta = true

		return result, correlator.Stats{}, nil
	}

	verified, unverified, err := verifyLinks(tracker, narrative.ID, actionable)
	result.Unverified = unverified
	if err != nil {
		return result, correlator.Stats{}, err
	}

	if len(verified) == 0 {
		// Every link was unresolvable. A `create` is NOT proposed here: the
		// narrative does have links, they just did not resolve this pass, and
		// manufacturing a ticket for work that is probably already tracked is
		// worse than proposing nothing.
		result.Suppressed = append(result.Suppressed,
			"no link verified against the live tracker; nothing drafted")

		return result, correlator.Stats{}, nil
	}

	drafted, stats, err := draft(ctx, client, narrative, delta, verified)
	if err != nil {
		return result, stats, fmt.Errorf("drafting for narrative %d: %w", narrative.ID, err)
	}

	kept, suppressed := suppressDuplicates(s, narrative.ID, drafted)
	result.Proposed = kept
	result.Suppressed = append(result.Suppressed, suppressed...)
	noteLowConfidence(&result, cfg.MinConfidenceToPropose)

	return result, stats, nil
}

// noteLowConfidence records which proposals fell below the configured
// threshold, WITHOUT dropping them.
//
// The threshold governs what unjira *asserts*, not what it *records*: slice 6's
// triage must be able to see a weak proposal in order to judge it, and a
// dropped proposal is indistinguishable from "nothing to do." Same reasoning as
// MatchConfig.ConfidenceFloor gating promotion rather than recording.
//
// So this only annotates the result, for rendering. Every action is still
// returned and still persisted carrying its low score. Slice 5's auto-commit
// gate is what will consult the score to decide whether to apply without
// review — which is why the threshold has to be visible somewhere now rather
// than silently unused.
func noteLowConfidence(result *ReconcileResult, threshold float64) {
	for _, a := range result.Proposed {
		if a.Confidence < threshold {
			result.LowConfidence = append(result.LowConfidence, fmt.Sprintf(
				"%s %s at confidence %.2f is below reconciler.min_confidence_to_propose %.2f "+
					"(recorded, not fit to auto-apply)",
				a.Type, a.IssueKey, a.Confidence, threshold))
		}
	}
}

// actionableLinks returns the links that may receive an action: the primary
// and every same_work co-representation.
//
// `mentioned` links get nothing — that is what the role means (a citation, a
// "caused by", a "discovered while"). Each returned link gets its OWN action,
// drafted separately: the motivating case is one body of work recorded twice
// for two audiences (an engineering ticket plus a paired change-management
// ticket), where identical text on both would defeat the point.
func actionableLinks(links []store.NarrativeIssue) []store.NarrativeIssue {
	var out []store.NarrativeIssue
	for _, l := range links {
		if l.Role == correlator.RolePrimary ||
			l.Role == correlator.RoleSameWork {
			out = append(out, l)
		}
	}

	return out
}

// dropSelfAuthored removes events unjira itself produced.
//
// The Jira collector sets artifacts["authored_by_unjira"], and its doc comment
// states exactly why: "without it the reconciler proposes the same comment
// every pass." That artifact has been written since the collector landed and
// read by nothing — this is its first consumer. Without this step the loop is
// unstable: unjira comments, collects its own comment, and proposes commenting
// about it.
func dropSelfAuthored(evts []events.Event) []events.Event {
	var out []events.Event
	for _, e := range evts {
		if authored, _ := e.Artifacts["authored_by_unjira"].(bool); authored {
			continue
		}
		out = append(out, e)
	}

	return out
}

// verifyLinks confirms every link's issue still exists, and reads its legal
// transitions in the same pass.
//
// A not-found key is recorded as unverified and skipped; a transport error
// aborts this narrative so the next pass retries it. correlator.IsTransportError
// makes that distinction and is reused rather than reimplemented — getting it
// backwards in either direction is a real failure mode documented at length on
// that function.
func verifyLinks(
	tracker tasktracker.TaskReader,
	narrativeID int64,
	links []store.NarrativeIssue,
) (verified []verifiedLink, unverified []string, err error) {
	for _, l := range links {
		issue, getErr := tracker.GetIssue(l.IssueKey)
		if getErr != nil {
			if correlator.IsTransportError(getErr) {
				return nil, unverified, fmt.Errorf(
					"verifying link %s for narrative %d: %w", l.IssueKey, narrativeID, getErr,
				)
			}
			unverified = append(unverified, l.IssueKey)

			continue
		}

		categories, catErr := tracker.AvailableStatusCategories(l.IssueKey)
		if catErr != nil {
			if correlator.IsTransportError(catErr) {
				return nil, unverified, fmt.Errorf(
					"reading transitions for %s on narrative %d: %w", l.IssueKey, narrativeID, catErr,
				)
			}
			// Not-found here is odd (GetIssue just succeeded) but survivable:
			// no known-legal transition means no transition is proposed, which
			// floorConfidence enforces.
			categories = nil
		}

		verified = append(verified, verifiedLink{
			Link: l, Issue: issue, AvailableStatus: categories,
		})
	}

	return verified, unverified, nil
}

// suppressDuplicates drops any action whose issue already has an open
// proposal from a DIFFERENT narrative, via store.NarrativesForIssue.
//
// The shared-ticket case: two narratives both linked to one change-management
// ticket should not stack two comments on it. That accessor was built for this
// slice (its doc comment says so) and has had no caller until now.
func suppressDuplicates(
	s *store.Store,
	narrativeID int64,
	drafted []ProposedAction,
) (kept []ProposedAction, suppressed []string) {
	for _, a := range drafted {
		if a.Type == ActionCreate {
			kept = append(kept, a)

			continue
		}

		refs, err := s.NarrativesForIssue(a.IssueKey)
		if err != nil {
			// A failed reverse lookup must not silently drop a proposal:
			// keep it and say so. A duplicate comment is recoverable; a
			// silently-vanished proposal is not.
			log.Printf(
				"reconciler: narrative %d: checking other narratives on %s failed (%v); "+
					"keeping the proposal rather than dropping it",
				narrativeID, a.IssueKey, err,
			)
			kept = append(kept, a)

			continue
		}

		if openProposalFromAnother(s, refs, narrativeID, a.IssueKey) {
			suppressed = append(suppressed, fmt.Sprintf(
				"%s: another narrative already has an open proposal on this issue", a.IssueKey))

			continue
		}

		kept = append(kept, a)
	}

	return kept, suppressed
}

// openProposalFromAnother reports whether a narrative other than narrativeID
// already has a status=proposed action on issueKey.
func openProposalFromAnother(
	s *store.Store,
	refs []store.NarrativeIssueRef,
	narrativeID int64,
	issueKey string,
) bool {
	for _, ref := range refs {
		if ref.NarrativeID == narrativeID {
			continue
		}

		actions, err := s.ActionsForNarrative(ref.NarrativeID)
		if err != nil {
			log.Printf("reconciler: reading actions for narrative %d: %v", ref.NarrativeID, err)

			continue
		}

		for _, a := range actions {
			if a.IssueKey == issueKey && a.Status == "proposed" {
				return true
			}
		}
	}

	return false
}
