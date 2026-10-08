package correlator

// context_fit.go fits each clustering call's context narratives to the prompt budget
// (finding F16).
//
// THE FAILURE. Every narrative overlapping the window is shown to the model as
// context, and a narrative's cost is its whole rendered block: summary, frozen
// events, eligible (numbered) events and background. On a real 30-day store that is
// 163 narratives and an estimated ~740,000-token prompt for 71 candidate events.
// Bisection sheds most of it, because a half sees only the narratives near its own
// events, but not all of it: a window that cannot be split further (one event, or
// events sharing a timestamp) still carries every narrative overlapping it, and
// before this fit that was a failed pass, however much context was the cause.
//
// THE FIT. Context narratives are added in the caller's order, which is its ranking
// (internal/pipeline ranks shared issue key first, then recency), while the prompt
// stays within the budget. A narrative that does not fit is left out whole and
// reported; a smaller, lower-ranked one after it may still fit. Never cut to fit:
// F16 measured that trade and a truncated story is still the wrong story.
//
// THE ORDER OF RESORT. clusterWindow bisects first and fits second. Leaving a
// narrative out is the damage F16 measured (a bound of 12 on a 7-day window more than
// doubled NEW clusters, each one duplicating a narrative the model could not see),
// while bisecting costs tokens and seam checks and loses no story. So a window over
// budget bisects while bisection makes progress, and only a window that cannot be
// split leaves narratives out. Measured on a real store at a 200,000-token window
// and a 32,000-token ceiling: over 7 days (71 overlapping narratives), fitting the
// whole window in one call leaves out 35, bisecting first leaves out 1; over 30 days
// (163), 123 against 85, where the code before this fit failed the pass outright (a
// 15-day half holding 2 events and ~359,000 estimated tokens, irreducible). The 30-day
// remainder is bisection's own limit: a half whose events all fall in one of its own
// halves cannot be split by time (bisectable), so it is fitted instead.
//
// THE BUDGET. llm.context_window_tokens less the reply's reserve
// (WithResponseReserve, llm.max_output_tokens): a server sizes prompt and max_tokens
// together, and F16's 365-day pass used the full 32,000-token ceiling
// (finish_reason=length), so reserving less than the whole ceiling is reserving less
// than a real reply has taken.
//
// THE HEADROOM. Each kept narrative is also charged followupTokensPerContextNarrative.
// A filled prompt has a follow-up: the omission re-ask resends the prompt verbatim
// plus every cluster the first reply produced, and F16 measured one EXTENDS cluster
// per context narrative shown (32 of 32 in nine runs). Without the charge, a prompt
// fitted to the budget leaves its own re-ask no room, and the re-ask fails loudly.

import (
	"fmt"
	"strings"
)

// followupTokensPerContextNarrative is the estimated tokens one context narrative's
// cluster adds to an omission re-ask, which quotes every cluster of the first reply.
//
// Derived, not tuned. The most completion F16 measured per cluster on the shipped
// prompt is 334 tokens (12,009 across 36 clusters; the mean across 110 clusters was
// 274, and run-to-run spread is 34%, so the maximum, not the mean). A quoted cluster
// is the reply's own text re-rendered, and estimateTokens counts 2 characters per
// token where the server measured ~2.4 (charsPerTokenEstimate), so 334 server tokens
// estimate at 334 × 2.4 / 2 ≈ 401.
const followupTokensPerContextNarrative = 401

// WithResponseReserve sets how many tokens of the context window are kept for the
// reply: the prompt budget every clustering-family call is checked against is
// contextWindowTokens less this. The caller passes llm.max_output_tokens, the
// ceiling a server reserves alongside the prompt. Zero, the default, reserves
// nothing, because with no ceiling configured unjira sends none and cannot know what
// the gateway will reserve.
func WithResponseReserve(tokens int) ClusterOption {
	return func(o *clusterOptions) {
		o.responseReserve = tokens
	}
}

// promptBudget is the most tokens a clustering-family prompt may be estimated at.
func promptBudget(contextWindowTokens, responseReserve int) (int, error) {
	budget := contextWindowTokens - max(responseReserve, 0)
	if budget <= 0 {
		return 0, fmt.Errorf(
			"no room for a clustering prompt: llm.max_output_tokens (%d) reserves the whole of "+
				"llm.context_window_tokens (%d) for the reply; lower max_output_tokens below the context window",
			responseReserve, contextWindowTokens)
	}

	return budget, nil
}

// contextFit is one call's fitted context set.
type contextFit struct {
	// kept are the narratives the prompt carries, in the caller's order.
	kept []Narrative
	// unfitted are the ids of the narratives left out, in the caller's order.
	unfitted []int64
	// baseTokens is the estimated prompt with no context narrative at all: what this
	// call costs before any context. Over budget, the window's own events do not fit.
	baseTokens int
	// fullTokens is the estimated prompt with every relevant narrative, unfitted: what
	// the call would have cost without the fit, recorded on Stats when it bisects.
	fullTokens int
}

// fitContextNarratives fits relevant to budget alongside inWindow, as
// buildClusterPrompt would render them under systemPrompt.
//
// Sizes are measured by rendering each narrative's pieces with the same helpers
// buildClusterPrompt uses, so the two cannot drift. Adding a narrative appends its
// eligible events to the end of the numbered list and its block to the end of the
// context section, so nothing already measured moves. The one thing that can change
// is a kept narrative's background line turning into a "-> #N" back-reference once a
// later narrative numbers that event, which only shortens the prompt: the running
// total is an upper bound on what is sent.
func fitContextNarratives(systemPrompt string, inWindow []Event, relevant []Narrative, budget int) contextFit {
	number := make(map[string]int, len(inWindow))
	var events strings.Builder
	for i, e := range inWindow {
		number[EventKey(e)] = i
		writeNumberedEvent(&events, i, e)
	}

	used := len(systemPrompt) + len(eventsHeading) + events.Len() + len(contextHeading)
	fit := contextFit{baseTokens: estimateTokensOfLen(used + len(noContextNarratives))}

	next, all := len(inWindow), used
	for _, n := range relevant {
		var piece strings.Builder
		var numbered []string
		for j, e := range n.EligibleEvents {
			writeNumberedEvent(&piece, next+j, e)
			// Never overwrite: an event numbered twice is assignableEvents' loud error
			// to raise, and the fit must not change which number it already has.
			if _, ok := number[EventKey(e)]; !ok {
				number[EventKey(e)] = next + j
				numbered = append(numbered, EventKey(e))
			}
		}
		writeNarrativeBlock(&piece, n, number)
		all += piece.Len()

		followup := (len(fit.kept) + 1) * followupTokensPerContextNarrative
		if estimateTokensOfLen(used+piece.Len())+followup > budget {
			// Its events were tentatively numbered; an excluded narrative's are not.
			for _, k := range numbered {
				delete(number, k)
			}
			fit.unfitted = append(fit.unfitted, n.ID)

			continue
		}

		used += piece.Len()
		next += len(n.EligibleEvents)
		fit.kept = append(fit.kept, n)
	}

	if len(relevant) == 0 {
		all += len(noContextNarratives)
	}
	fit.fullTokens = estimateTokensOfLen(all)

	return fit
}

// mergeNarrativeIDs returns the union of a and b, each id once, a's order first.
func mergeNarrativeIDs(a, b []int64) []int64 {
	if len(b) == 0 {
		return a
	}

	seen := make(map[int64]bool, len(a)+len(b))
	out := make([]int64, 0, len(a)+len(b))
	for _, id := range append(append([]int64(nil), a...), b...) {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}

	return out
}
