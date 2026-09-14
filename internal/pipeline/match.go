package pipeline

import (
	"context"
	"fmt"
	"log"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/llm"
	"github.com/jcogilvie/unjira/internal/store"
)

// MatchRunResult is one matching pass, shaped for rendering.
type MatchRunResult struct {
	Matched []correlator.MatchResult
	Stats   correlator.Stats
	// Remaining is how many narratives are STILL unmatched after this pass — 0
	// when the backlog drained.
	//
	// Data, not just a log line, for two reasons. The renderer prints it to stdout
	// where an operator actually looks; correlator.Match's own cap warning goes to
	// stderr, which is how a 42-narrative backlog got misread as a clustering
	// defect. And `watch` can act on it: a loop that knows it is behind can drain
	// rather than sleep its interval.
	Remaining int
}

// RunMatch runs one narrative→issue matching pass: validate cfg.Match,
// compile cfg's link exclusions, and hand both to correlator.Match.
//
// It does NOT acquire the pipeline lease. That is the caller's job, for the
// same reason RunNarrate does not: the scope differs per caller (dev match
// wraps this one stage; watch will wrap collect + narrate + match +
// reconcile in a single lease), and acquiring here would make watch contend
// with itself.
//
// Compiling exclusions here rather than inside correlator.Match is
// deliberate: Match takes patterns as a MatchOption and does not read
// config at all, the same split RunCollect uses for the collectors'
// exclusion patterns. Config parsing and compilation is pipeline's job;
// correlator only ever sees compiled regexps. Loading rules/ (see
// loadCorrelatorRules) follows the identical split.
//
// On a partial failure, RunMatch returns both the accumulated MatchRunResult
// and a non-nil error: correlator.Match isolates failures per narrative, so
// the narratives that resolved cleanly are still worth rendering even when
// one narrative's tracker call failed.
// resolve, rather than a single tracker, because a candidate's connection
// decides which backend actually holds it — see correlator.TrackerResolver.
// Resolution stays with the caller (cmd/unjira, which has the config); this
// layer only passes it through, the same split it already uses for exclusion
// patterns and rules.
func RunMatch(
	ctx context.Context,
	s *store.Store,
	resolve correlator.TrackerResolver,
	client llm.Client,
	cfg config.Config,
) (MatchRunResult, error) {
	if err := cfg.Match.Validate(); err != nil {
		return MatchRunResult{}, fmt.Errorf("invalid match config: %w", err)
	}

	compiled, err := cfg.CompiledLinkExclusions()
	if err != nil {
		return MatchRunResult{}, fmt.Errorf("compiling link exclusions: %w", err)
	}

	correlatorRules, err := loadCorrelatorRules(cfg)
	if err != nil {
		return MatchRunResult{}, err
	}

	matched, stats, err := correlator.Match(
		ctx, s, resolve, client, cfg.Match,
		correlator.WithLinkExclusions(compiled), correlator.WithRules(correlatorRules))
	result := MatchRunResult{Matched: matched, Stats: stats}
	if err != nil {
		return result, fmt.Errorf("matching narratives: %w", err)
	}

	// Counted AFTER the pass, here rather than inside correlator.Match, for three
	// reasons. It is EXACT: Match fetches limit+1 to tell "exactly full" from "more
	// waiting", so it could only ever report "21 or more" where an operator wants
	// 47. It respects the layering internal/pipeline already enforces — this layer
	// does config and store I/O, correlator gets resolved values and is not
	// responsible for reporting backlog depth. And post-pass state is the more
	// useful number anyway, since the question is "am I caught up now", not "what
	// did that pass see".
	//
	// A failure here is NOT the pass's failure: the matching happened and its
	// results are worth returning. Remaining stays 0, which reads as "caught up" —
	// the wrong answer, but a quieter one than discarding a completed pass over a
	// COUNT(*).
	remaining, countErr := s.CountNarrativesWithoutPrimaryLink()
	if countErr != nil {
		log.Printf("pipeline: could not count the remaining unmatched narratives (%v); "+
			"this pass's summary will not report a backlog", countErr)
	} else {
		result.Remaining = remaining
	}

	return result, nil
}
