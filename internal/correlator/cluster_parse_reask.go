package correlator

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jcogilvie/unjira/internal/llm"
	"github.com/jcogilvie/unjira/internal/logging"
)

// A clustering response the parser refuses — an out-of-range index, an unknown kind,
// a missing confidence, prose where the array belongs — is not read best-effort,
// because a best-effort reading is how events get silently misattributed. Before this
// re-ask existed, one such response cost the whole multi-minute pass, and under
// `unjira watch` the window could move on (finding F44). So the response is asked
// about again, quoting it and the parser's reason, up to llm.max_cluster_reasks
// follow-up calls per clustering call, then fails loudly naming every refusal: the
// shape matching, the omission re-ask and the dispute re-ask already have.
//
// Each re-ask repeats the clustering call's own system and user prompts verbatim, so
// every index means what it meant the first time; only the refusal is appended. That
// makes a re-ask cost about what the first call did (the whole window's prompt), where
// a matching re-ask costs one narrative's. The default is still one follow-up, as at
// every other use: the alternative to a re-ask is a failed pass, and retrying that
// pass re-sends the same prompt plus everything else the pass had already paid for.

// parseReaskRequest is what reaskRefusedCluster needs from the clustering call whose
// response the parser refused.
type parseReaskRequest struct {
	window TimeRange
	// systemPrompt and userPrompt are the refused call's prompts, verbatim. The user
	// prompt numbers assignable, the index space the parser resolves against, so it
	// must not be rebuilt.
	systemPrompt string
	userPrompt   string
	assignable   []Event
	// refused and reason are the first response and the parser's reason for refusing
	// it.
	refused string
	reason  error
	// maxReasks is the budget: how many follow-up calls may be made. 0 (or less)
	// makes the first refusal an error with no call.
	maxReasks int

	contextWindowTokens int
	log                 *slog.Logger
}

// reaskRefusedCluster asks the refused clustering call again, up to req.maxReasks
// times, and returns the first response the parser accepts, with its member indices
// (as parseClusterResponse returns them, so coverage is then checked on it exactly
// as on a first answer). Spending the budget is a loud error naming every refusal.
//
// Each re-ask is checked against the context window before it is sent: it is larger
// than the prompt that fitted, by the refusal it quotes. One over the window fails the
// pass rather than bisecting. Bisecting would throw away the refusal the re-ask exists
// to show, re-cluster two halves the model was never asked about, and could not shrink
// the part that overflowed, which is the quoted response, not the window's events. A
// transport error is not a refusal and ends the pass at once.
func reaskRefusedCluster(ctx context.Context, client llm.Client, req parseReaskRequest) ([]ClusterResult, [][]int, Stats, error) {
	var stats Stats

	refusals := []error{req.reason}
	refused := req.refused

	for attempt := 1; attempt <= req.maxReasks; attempt++ {
		latest := refusals[len(refusals)-1]
		userPrompt := buildClusterParseReaskPrompt(req.userPrompt, refused, latest)
		estimated := estimateTokens(req.systemPrompt + userPrompt)
		stats.EstimatedTokens += estimated

		logging.For(req.log, "correlator").WarnContext(ctx, "cluster response refused; re-asking",
			"reask", attempt, "max_reasks", req.maxReasks,
			"assignable_events", len(req.assignable),
			"reason", latest.Error(), "est_tokens", estimated,
			"window_start", req.window.Start, "window_end", req.window.End)

		if estimated > req.contextWindowTokens {
			return nil, nil, stats, fmt.Errorf(
				"clustering events in window [%s, %s): re-ask %d of %d (llm.max_cluster_reasks) for the refused "+
					"cluster response is estimated at %d tokens, over the %d-token context window, so it was not "+
					"sent; the refusal was: %w",
				req.window.Start, req.window.End, attempt, req.maxReasks, estimated, req.contextWindowTokens, latest)
		}

		stats.ClusterReasks++
		raw, usage, err := client.Complete(ctx, req.systemPrompt, userPrompt)
		if err != nil {
			return nil, nil, stats, fmt.Errorf(
				"clustering events in window [%s, %s): re-asking (re-ask %d of %d) after %w: %w",
				req.window.Start, req.window.End, attempt, req.maxReasks, latest, err)
		}
		stats.AddUsage(usage)

		results, indices, err := parseClusterResponse(raw, req.assignable)
		if err == nil {
			logging.For(req.log, "correlator").InfoContext(ctx, "cluster re-ask accepted",
				"reasks", stats.ClusterReasks)

			return results, indices, stats, nil
		}
		refusals = append(refusals, err)
		refused = raw
	}

	return nil, nil, stats, fmt.Errorf("clustering events in window [%s, %s): %w", req.window.Start, req.window.End,
		&refusalsError{
			what: "cluster response", key: "llm.max_cluster_reasks", budget: max(req.maxReasks, 0), reasons: refusals,
		})
}

// buildClusterParseReaskPrompt repeats the clustering call's user prompt verbatim, so
// every index keeps its meaning, then quotes the latest refused response and the
// parser's reason.
func buildClusterParseReaskPrompt(userPrompt, refused string, reason error) string {
	var b strings.Builder

	b.WriteString(userPrompt)
	writeRefusal(&b, refused, reason)
	b.WriteString("\n\nNothing from it was applied. Answer again: return ONLY the JSON array the system prompt " +
		"specifies, placing every numbered event, no prose and no markdown fences.\n")

	return b.String()
}
