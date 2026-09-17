package pipeline

// learn.go is the pass that makes rules.Distill reachable and persists what it produces.
//
// Distill and store.CorrectionsSince landed without a caller, which is a fair thing to
// call a library rather than a feature: a reviewer could correct the same mistake every
// pass, the correction would be read by nothing, and no rule would ever exist. This stage
// closes the loop — read corrections since the last learn, draft candidates, and write the
// ones a human keeps into the directory the correlator's and reconciler's prompts already
// load from.
//
// TWO PROPERTIES CARRY THE SAFETY.
//
// Writing is OPT-IN PER CANDIDATE. A rule shapes every future prompt on every narrative,
// which is a bigger blast radius than any single tracker write, so a learn pass drafts by
// default and writes only what it is told to. That is the same separation gate.Applier
// embodies: proposing is free, committing needs a human.
//
// The WATERMARK ADVANCES ONLY WHEN SOMETHING WAS KEPT. A draft-only pass leaves the
// corrections in place, because the reviewer has not ruled yet — advancing there would
// skip feedback nobody ever turned into a rule. That is the inverse of the
// outcome-leaves-no-trace bug this codebase has now hit three times (F12, F22, F26): here
// the danger is a watermark that moves past work that never happened.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/llm"
	"github.com/jcogilvie/unjira/internal/logging"
	"github.com/jcogilvie/unjira/internal/rules"
	"github.com/jcogilvie/unjira/internal/store"
)

// learnCursor names the watermark row in the cursors table.
//
// `cursors` rather than a new table: it is already a keyed (collector, resource) →
// position store with an updated_at, which is exactly a watermark. The phase-1 spec warned
// against reusing it for the pipeline LOCK, and that warning was specific — "locking is a
// different concern from watermarking — a lock needs expires_at/steal semantics cursors
// was never designed for". Watermarking is the concern cursors was designed for.
//
// The "collector" column holds a non-collector name here, which is a small abuse of the
// schema's vocabulary and cheaper than a second table that would do the same thing.
const (
	learnCursorCollector = "learn"
	learnCursorResource  = "corrections"
)

// LearnOptions configures one learn pass.
type LearnOptions struct {
	// Keep names the candidates to write, by Candidate.Name. Empty means draft only —
	// the default, so an accidental invocation cannot change agent behaviour.
	Keep []string
	// Log is where the pass announces the model call, which is its slow step. Nil is
	// silent, matching every other optional dependency in this package.
	Log *slog.Logger
}

// LearnResult is one learn pass's outcome.
type LearnResult struct {
	// CorrectionsRead is how many reviewer rulings the pass considered. Reported even
	// when zero, because "nobody has corrected anything" and "the watermark skipped
	// past them" are different situations and only one is fine.
	CorrectionsRead int
	// Candidates are the drafted rules, whether or not they were written.
	Candidates []rules.Candidate
	// Written are the names actually persisted, so a caller can report what changed on
	// disk rather than what was offered.
	Written []string
	// WatermarkAdvanced records whether the pass moved the cursor, since that is the
	// difference between a pass a reviewer can safely repeat and one they cannot.
	WatermarkAdvanced bool
	Stats             llm.Usage
}

// RunLearn distils reviewer corrections into candidate rules, writing only those named in
// opts.Keep.
//
// No corrections means no model call: a scheduled learn pass over an unreviewed queue must
// cost nothing.
func RunLearn(
	ctx context.Context,
	s *store.Store,
	client llm.Client,
	cfg config.Config,
	opts LearnOptions,
) (LearnResult, error) {
	var result LearnResult

	since, err := readLearnWatermark(s)
	if err != nil {
		return LearnResult{}, err
	}

	corrections, err := s.CorrectionsSince(since)
	if err != nil {
		return LearnResult{}, fmt.Errorf("reading corrections since %s: %w", since, err)
	}
	result.CorrectionsRead = len(corrections)

	if len(corrections) == 0 {
		return result, nil
	}

	logging.For(opts.Log, "learn").InfoContext(ctx, "distilling corrections",
		"corrections", len(corrections), "since", since, "keep", len(opts.Keep))

	candidates, usage, err := rules.Distill(ctx, client, toRulesCorrections(corrections))
	result.Stats = usage
	if err != nil {
		// Deliberately NOT advancing the watermark: a failed pass must leave its
		// corrections to be retried, not skip them.
		return result, fmt.Errorf("distilling corrections: %w", err)
	}
	result.Candidates = candidates

	if len(opts.Keep) == 0 {
		return result, nil
	}

	written, advanced, err := KeepCandidates(s, cfg, candidates, opts.Keep)
	result.Written = written
	result.WatermarkAdvanced = advanced

	return result, err
}

// KeepCandidates writes the named candidates and advances the watermark.
//
// SEPARATE FROM RunLearn because a two-step reviewer flow — draft, read, then keep — must
// persist the candidates the reviewer ACTUALLY READ. Re-running RunLearn with Keep set
// would re-distil, and a model does not return the same rule names twice: an early version
// of `unjira learn --all` drafted "skip-no-op-progress-comments", then re-distilled and was
// offered "comment-must-add-information", and failed with "no drafted rule named ...". The
// names were both fine; the assumption that they would be stable was not.
//
// So the CLI holds the candidates from its one draft and hands them back here. That also
// halves the cost of keeping, and removes any chance of a reviewer approving prose they did
// not see.
func KeepCandidates(
	s *store.Store, cfg config.Config, candidates []rules.Candidate, keep []string,
) (written []string, advanced bool, err error) {
	written, err = writeKept(cfg.RulesDir(), candidates, keep)
	if err != nil {
		return written, false, err
	}

	// Only after every kept rule is on disk. Advancing first and failing to write would
	// lose the corrections entirely.
	if err := s.SetCursor(learnCursorCollector, learnCursorResource,
		time.Now().UTC().Format("2006-01-02T15:04:05Z")); err != nil {
		return written, false, fmt.Errorf("advancing the learn watermark: %w", err)
	}

	return written, true, nil
}

// readLearnWatermark reads the cursor, treating absence as "everything".
//
// A malformed stored value is an error rather than a silent reset to zero: resetting would
// re-distil every correction ever made and offer a reviewer rules they already ruled on,
// which reads as the tool having forgotten their decisions.
func readLearnWatermark(s *store.Store) (time.Time, error) {
	raw, err := s.GetCursor(learnCursorCollector, learnCursorResource)
	if err != nil {
		return time.Time{}, fmt.Errorf("reading the learn watermark: %w", err)
	}
	if raw == "" {
		return time.Time{}, nil
	}

	parsed, err := time.Parse("2006-01-02T15:04:05Z", raw)
	if err != nil {
		return time.Time{}, fmt.Errorf(
			"the learn watermark %q is not a timestamp: refusing to reset it, which would "+
				"re-offer every correction a reviewer has already ruled on: %w", raw, err)
	}

	return parsed, nil
}

// toRulesCorrections maps the store's shape to the rules package's.
//
// Two identical structs in two packages, mapped here, because internal/store must not
// import internal/rules — the same boundary narrativeissues.go keeps by storing provenance
// as a plain string rather than importing the correlator's typed one.
func toRulesCorrections(in []store.Correction) []rules.Correction {
	out := make([]rules.Correction, 0, len(in))
	for _, c := range in {
		out = append(out, rules.Correction{
			ActionID: c.ActionID, ActionType: c.ActionType, IssueKey: c.IssueKey,
			Body: c.Body, Feedback: c.Feedback,
		})
	}

	return out
}

// writeKept writes the named candidates, refusing to overwrite an existing file.
//
// Every name must match a drafted candidate. A typo that silently wrote nothing would be
// indistinguishable from a reviewer deliberately keeping none, and the reviewer would
// believe a rule exists that does not.
//
// O_EXCL rather than a stat-then-write, so a collision cannot slip through between the
// check and the write. Refusing is right even though the candidate may be better prose: a
// name collision means a human has already written a rule about this, possibly by hand,
// and clobbering it would discard a decision nobody recorded elsewhere.
func writeKept(dir string, candidates []rules.Candidate, keep []string) ([]string, error) {
	byName := make(map[string]rules.Candidate, len(candidates))
	offered := make([]string, 0, len(candidates))
	for _, c := range candidates {
		byName[c.Name] = c
		offered = append(offered, c.Name)
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating rules directory %s: %w", dir, err)
	}

	var written []string
	for _, name := range keep {
		candidate, ok := byName[name]
		if !ok {
			return written, fmt.Errorf(
				"no drafted rule named %q; this pass offered: %s",
				name, strings.Join(offered, ", "))
		}

		path := filepath.Join(dir, candidate.Name+".md")

		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				return written, fmt.Errorf(
					"a rule named %q already exists at %s: refusing to overwrite, since a "+
						"collision means somebody has already written about this — delete or "+
						"rename it first if the distilled version should replace it",
					candidate.Name, path)
			}

			return written, fmt.Errorf("creating rule file %s: %w", path, err)
		}

		_, writeErr := f.WriteString(candidate.FileContents())
		closeErr := f.Close()
		if writeErr != nil {
			return written, fmt.Errorf("writing rule file %s: %w", path, writeErr)
		}
		if closeErr != nil {
			return written, fmt.Errorf("closing rule file %s: %w", path, closeErr)
		}

		written = append(written, candidate.Name)
	}

	return written, nil
}
