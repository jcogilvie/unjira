package main

// learn.go is `unjira learn` — the terminal surface over rules distillation.
//
// It exists because rules.Distill and store.CorrectionsSince landed without a caller,
// which makes them a library rather than a feature: a reviewer's corrections would sit in
// actions.feedback and never become a rule. This is thin by design, in the same way
// triage.go is: parse flags, print candidates, hand the decision back. Every judgment —
// what generalises, whether the watermark advances — lives in internal/pipeline and
// internal/rules, so the pass is testable without a terminal.
//
// TWO-STEP BY DEFAULT, and that is the point. Running it bare drafts and prints; writing
// requires naming the rules you want with --keep. A rule shapes every future prompt on
// every narrative, a wider blast radius than any single tracker write, so the same
// propose-then-commit separation gate.Applier enforces applies here.

import (
	"context"
	"fmt"
	"strings"

	"github.com/jcogilvie/unjira/internal/pipeline"
)

// learnCmd is `unjira learn`.
type learnCmd struct {
	Keep []string `help:"Names of drafted rules to write into the rules directory. Repeatable. Omit to draft only."`
	All  bool     `help:"Write every drafted rule. Convenience for a reviewer who has read them all — still a deliberate flag, never a default."`
}

// Run drafts candidate rules from reviewer corrections, writing only what was named.
func (c *learnCmd) Run(app *appContext) error {
	client, err := app.llmClient()
	if err != nil {
		return err
	}

	ctx := context.Background()

	// Drafting always happens first, even under --all: the reviewer sees what was
	// written, and --all needs the candidate names before it can name them.
	drafted, err := pipeline.RunLearn(ctx, app.store, client, app.config,
		pipeline.LearnOptions{Log: app.log})
	if err != nil {
		return err
	}

	fmt.Print(renderLearnResult(drafted))

	keep := c.Keep
	if c.All {
		for _, candidate := range drafted.Candidates {
			keep = append(keep, candidate.Name)
		}
	}
	if len(keep) == 0 {
		if len(drafted.Candidates) > 0 {
			fmt.Printf("\nNothing written. Re-run with --keep <name> (repeatable) or --all.\n")
		}

		return nil
	}

	// Persist the candidates ALREADY DRAFTED rather than re-running the pass. An earlier
	// version re-distilled with Keep set, which cannot work: the model does not return
	// the same rule names twice, so --all named "skip-no-op-progress-comments" from the
	// first draft and the second offered "comment-must-add-information" instead. It also
	// means a reviewer can only ever keep prose they actually read.
	written, advanced, err := pipeline.KeepCandidates(
		app.store, app.config, drafted.Candidates, keep)
	for _, name := range written {
		fmt.Printf("  wrote %s/%s.md\n", app.config.RulesDir(), name)
	}
	if err != nil {
		return err
	}
	if advanced {
		fmt.Printf("\n%d correction(s) distilled; they will not be offered again.\n",
			drafted.CorrectionsRead)
	}

	return nil
}

// renderLearnResult prints the drafted candidates in full.
//
// Full bodies, never truncated, for the reason triage renders full action bodies: a
// reviewer must not keep a rule they have not read, and a rule body is the norm itself.
func renderLearnResult(r pipeline.LearnResult) string {
	var b strings.Builder

	b.WriteString("== learn pass ==\n")
	fmt.Fprintf(&b, "corrections  %d reviewer ruling(s) with feedback\n", r.CorrectionsRead)
	fmt.Fprintf(&b, "tokens       %d prompt + %d completion\n",
		r.Stats.PromptTokens, r.Stats.CompletionTokens)

	if r.CorrectionsRead == 0 {
		b.WriteString("\nNo corrections since the last learn pass. Nothing to distil.\n")

		return b.String()
	}

	if len(r.Candidates) == 0 {
		// Distinct from "no corrections", and worth saying: the model read real feedback
		// and judged that none of it generalises, which is frequently the right answer
		// for a one-off wording fix.
		b.WriteString("\nNo rules drafted — these corrections did not generalise into a norm.\n")

		return b.String()
	}

	for i, candidate := range r.Candidates {
		fmt.Fprintf(&b, "\n[%d/%d] %s  scope=%s  confidence=%s\n",
			i+1, len(r.Candidates), candidate.Name, candidate.Scope, candidate.Confidence)
		fmt.Fprintf(&b, "  from action(s) %s\n", joinActionIDs(candidate.SourceActionIDs))
		fmt.Fprintf(&b, "%s\n", indent(candidate.Body, "  "))
	}

	return b.String()
}

// joinActionIDs formats the provenance line, so a reviewer can trace a proposed rule back
// to the ruling that produced it.
func joinActionIDs(ids []int64) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, fmt.Sprintf("%d", id))
	}

	return strings.Join(parts, ", ")
}
