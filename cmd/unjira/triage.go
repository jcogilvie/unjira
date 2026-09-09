package main

// triage.go is `unjira triage` — the interactive review surface. It is
// deliberately thin: parse a keystroke, print an action, hand the answer to
// internal/triage. Every decision (what a merge means, which narrative is the
// target, what gets applied) lives in that package, so the session is testable
// without a terminal. See
// docs/superpowers/specs/2026-08-28-triage-design.md.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jcogilvie/unjira/internal/gate"
	"github.com/jcogilvie/unjira/internal/pipeline"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/triage"
)

// triageCmd is `unjira triage`.
type triageCmd struct {
	AutoApprove bool `help:"Approve every action without prompting. Bypasses the prompt AND auto_commit.graduated (the approve path never consults gate.Decide) — only jira[].writable_project_keys still applies."`
	Refresh     bool `help:"Block until any in-flight watch pass finishes, so the batch is not mid-change."`
	DryRun      bool `help:"Walk the batch and show dispositions, but never write to the store or a tracker."`
}

// parseDecision turns one line of reviewer input into a triage.Decision.
//
// Single letters, all distinct: a r e m s k t q. `skip` takes k precisely
// because s belongs to split — a collision here would make one disposition
// unreachable.
//
// Verbs that need an argument error when it is missing rather than defaulting.
// An `e` with no text would reach reconciler.Redraft, which errors on empty
// feedback (by design — blank text means the correction was lost), so catching
// it here turns a wasted LLM round-trip into an immediate re-prompt.
func parseDecision(line string) (triage.Decision, error) {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) == 0 {
		return triage.Decision{}, fmt.Errorf("unknown verb %q: expected one of a r e m s k t q", line)
	}

	verb, rest := fields[0], fields[1:]

	switch verb {
	case "a":
		return triage.Decision{Verb: triage.VerbApprove}, nil
	case "k":
		return triage.Decision{Verb: triage.VerbSkip}, nil
	case "q":
		return triage.Decision{Verb: triage.VerbQuit}, nil
	case "r":
		// Reject takes optional free text: a reviewer may simply not want the
		// action, with nothing to teach slice 7's rules.Distill.
		return triage.Decision{Verb: triage.VerbReject, Text: strings.Join(rest, " ")}, nil
	case "e":
		if len(rest) == 0 {
			return triage.Decision{}, fmt.Errorf("edit requires text: e <what to change>")
		}

		return triage.Decision{Verb: triage.VerbEdit, Text: strings.Join(rest, " ")}, nil
	case "t":
		if len(rest) != 1 {
			return triage.Decision{}, fmt.Errorf("target requires exactly one issue key: t <KEY>")
		}

		return triage.Decision{Verb: triage.VerbTarget, Text: rest[0]}, nil
	case "m":
		if len(rest) != 2 {
			return triage.Decision{}, fmt.Errorf("merge requires two positions: m <n> <n>")
		}
		positions, err := parsePositions(rest)
		if err != nil {
			return triage.Decision{}, err
		}

		return triage.Decision{Verb: triage.VerbMerge, Positions: positions}, nil
	case "s":
		if len(rest) != 1 {
			return triage.Decision{}, fmt.Errorf("split requires one position: s <n>")
		}
		positions, err := parsePositions(rest)
		if err != nil {
			return triage.Decision{}, err
		}

		return triage.Decision{Verb: triage.VerbSplit, Positions: positions}, nil
	default:
		return triage.Decision{}, fmt.Errorf("unknown verb %q: expected one of a r e m s k t q", verb)
	}
}

func parsePositions(fields []string) ([]int, error) {
	out := make([]int, 0, len(fields))
	for _, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil {
			return nil, fmt.Errorf("position %q is not a number", f)
		}
		if n < 1 {
			return nil, fmt.Errorf("position %d is not valid: batch positions start at 1", n)
		}
		out = append(out, n)
	}

	return out, nil
}

// terminalPrompter implements triage.Prompter against stdin/stdout. It is the
// only thing in this slice that touches a terminal.
type terminalPrompter struct {
	in *bufio.Scanner
}

func newTerminalPrompter() *terminalPrompter {
	return &terminalPrompter{in: bufio.NewScanner(os.Stdin)}
}

// Ask prints one action in full — body and rationale, never truncated — then
// reads a disposition. Full bodies are deliberate: a reviewer must not approve
// text they did not read, and real drafted bodies run past 200 characters, so a
// one-line summary would invite exactly that.
func (p *terminalPrompter) Ask(item triage.Item) (triage.Decision, error) {
	a := item.Action

	fmt.Printf("\n[%d/%d] %s  %s  confidence %.2f\n",
		item.Position, item.Total, a.Type, a.IssueKey, a.Confidence)
	fmt.Printf("%s\n", indent(bodyOf(a), "  "))
	if a.Rationale != "" {
		fmt.Printf("\n  why: %s\n", a.Rationale)
	}

	for {
		fmt.Print("\n  [a]pprove [r]eject [e]dit [m]erge [s]plit s[k]ip [t]arget [q]uit > ")

		if !p.in.Scan() {
			if err := p.in.Err(); err != nil {
				return triage.Decision{}, fmt.Errorf("reading disposition: %w", err)
			}
			// EOF (piped input exhausted, or Ctrl-D): treat as quit rather than
			// looping forever on a closed stdin.
			return triage.Decision{Verb: triage.VerbQuit}, nil
		}

		d, err := parseDecision(p.in.Text())
		if err != nil {
			// A typo re-prompts rather than aborting: losing a half-reviewed
			// batch to a fat finger would be worse than the noise.
			fmt.Printf("  %v\n", err)

			continue
		}

		return d, nil
	}
}

// Notify reports a recoverable problem — a refused merge, an unavailable verb —
// without ending the session. The reviewer sees it and is asked again, so a
// mistake costs one keystroke rather than a half-reviewed batch.
func (p *terminalPrompter) Notify(message string) error {
	fmt.Printf("  %s\n", message)

	return nil
}

// Confirm is the single gate before anything is written.
func (p *terminalPrompter) Confirm(summary string) (bool, error) {
	fmt.Printf("\n== %s ==\napply? [y/N] ", summary)

	if !p.in.Scan() {
		return false, p.in.Err()
	}

	answer := strings.ToLower(strings.TrimSpace(p.in.Text()))

	return answer == "y" || answer == "yes", nil
}

// bodyOf extracts the human-readable text from an action's JSON payload for
// display. A payload that will not decode is shown raw rather than hidden: the
// reviewer needs to see that it is malformed, since gate.Applier would fail on
// it too.
func bodyOf(a store.ActionRow) string {
	var p struct {
		Body    string `json:"body"`
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal([]byte(a.Payload), &p); err != nil {
		return a.Payload
	}
	if p.Body != "" {
		return p.Body
	}
	if p.Summary != "" {
		return p.Summary
	}

	return a.Payload
}

func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}

	return strings.Join(lines, "\n")
}

// Run loads the review queue, walks it, and applies only what the reviewer
// confirms at the end.
//
// Nothing reaches a tracker mid-session: gate.Applier is called once, after
// Confirm. That is what makes every restructure free while the reviewer is
// still deciding — a merge cannot invalidate an action that has already been
// posted, because none have.
func (c *triageCmd) Run(app *appContext) error {
	if c.Refresh {
		// Block on any in-flight watch pass rather than reviewing a batch it is
		// midway through changing. The lease is TTL-bounded, so a crashed pass
		// cannot hold this forever.
		runID := fmt.Sprintf("triage-%d", os.Getpid())
		if err := app.store.Acquire(
			context.Background(), runID, time.Now, leaseTTL, leasePoll,
		); err != nil {
			return fmt.Errorf("waiting for an in-flight pass to finish: %w", err)
		}
		defer func() {
			if err := app.store.ReleaseLock(runID); err != nil {
				log.Printf("triage: releasing lease: %v", err)
			}
		}()
	}

	batch, err := app.store.ActionsByStatus(store.StatusProposed)
	if err != nil {
		return err
	}

	if len(batch) == 0 {
		fmt.Println("nothing to review: no actions at status=proposed")

		return nil
	}

	prompter := newTerminalPrompter()

	// --dry-run passes no Handler, so edit/target/merge report themselves
	// unavailable and re-prompt. That is deliberate: those verbs persist
	// narrative and action changes, and a dry run must not.
	var handler triage.Handler
	if !c.DryRun {
		h, err := app.triageHandler()
		if err != nil {
			return err
		}
		handler = h
	}

	session := triage.NewSession(context.Background(), batch, prompter, handler)

	fmt.Printf("%d proposed action(s) to review.\n", len(batch))

	if c.AutoApprove {
		session.ApproveAll()
	} else if err := session.Run(); err != nil {
		if errors.Is(err, triage.ErrAbandoned) {
			fmt.Println("\nabandoned: nothing was applied")

			return nil
		}

		return err
	}

	approved := session.Approved()

	if c.DryRun {
		fmt.Printf("\ndry run: would apply %d action(s); skipped Persist and the tracker\n", len(approved))
		for _, a := range approved {
			fmt.Printf("  would apply %s on %s\n", a.Type, a.IssueKey)
		}

		return nil
	}

	// Rulings are persisted BEFORE the apply confirmation, and independently of
	// it. A reject writes nothing to a tracker, so it is not the confirmation's
	// business — and gating it on "apply?" would mean a reviewer who rejects
	// several actions and then answers N loses every ruling. Nothing in PR #26
	// wrote these at all, so a rejected action came back in the next session and
	// slice 7's rules.Distill had no input; see Session.Rulings.
	if err := app.recordRulings(session.Rulings()); err != nil {
		return err
	}

	if len(approved) == 0 {
		fmt.Println("\nnothing approved; nothing applied")

		return nil
	}

	ok, err := prompter.Confirm(session.Summary())
	if err != nil {
		return err
	}
	if !ok {
		fmt.Println("not applied")

		return nil
	}

	return app.applyApproved(approved)
}

// triageHandler builds the production Handler, resolving the tracker and LLM
// client that edit and target need.
//
// A resolution failure is reported and the session continues WITHOUT those verbs
// rather than aborting: approve/reject/skip need neither dependency, and refusing
// to open a review queue because no default project is configured would be a
// worse outcome than a session where three verbs say they are unavailable.
//
// The tracker is passed as a TaskReader. taskTracker returns the full
// TaskTracker, so this narrowing is what makes a write from the handler fail to
// compile — the same guarantee RunReconcile relies on.
func (a *appContext) triageHandler() (triage.Handler, error) {
	project, err := a.projectKey("")
	if err != nil {
		noteUnavailable("no default project is configured (tracker.default_project)")

		return triage.NewStoreHandler(
			a.store, nil, nil, nil, a.config.Correlator, a.config.LLM.ContextWindowTokens), nil
	}

	tracker, err := a.taskTracker(project)
	if err != nil {
		noteUnavailable("the tracker could not be resolved (check tracker.backend and credentials)")

		return triage.NewStoreHandler(
			a.store, nil, nil, nil, a.config.Correlator, a.config.LLM.ContextWindowTokens), nil
	}

	client, err := a.llmClient()
	if err != nil {
		noteUnavailable("no LLM client could be built (check the llm block and its credential)")

		return triage.NewStoreHandler(
			a.store, tracker, nil, nil, a.config.Correlator, a.config.LLM.ContextWindowTokens), nil
	}

	// The SAME rules a watch pass would have used. A redraft prompted without
	// them would silently ignore everything slice 7 taught, so the reviewer's
	// correction would land while a rule they wrote earlier was dropped.
	learnedRules, err := pipeline.ReconcilerRules(a.config)
	if err != nil {
		return nil, err
	}

	return triage.NewStoreHandler(
		a.store, tracker, client, learnedRules,
		a.config.Correlator, a.config.LLM.ContextWindowTokens), nil
}

// noteUnavailable tells the reviewer that edit and target will not work this
// session, and WHY in terms of what to configure.
//
// It prints a fixed, hand-written cause rather than the underlying error, and
// that is a deliberate narrowing rather than laziness. The first draft printed
// `%v` of the error, which CodeQL flagged as high-severity
// `go/clear-text-logging`: "Sensitive data returned by an access to APIKeyHelper
// flows to a logging call." The taint is real — llmCredential's failure paths
// reach ResolvedAPIKeyHelper, whose error quotes llm.api_key_helper with %q. That
// value is a command line, so a helper written as `sh -c 'print-token --key=...'`
// would put a credential on stdout, and unlike a log file stdout is what a
// reviewer pastes into a bug report.
//
// Every other caller of llmClient RETURNS its error rather than printing it, so
// this function was the only place in the repo introducing that flow. Suppressing
// the alert would have kept a real (if unlikely) leak for the sake of a
// diagnostic; a fixed string names the config key to check, which is the actually
// useful half of the message anyway.
func noteUnavailable(cause string) {
	fmt.Printf("  note: edit and target are unavailable this session: %s\n", cause)
}

// recordRulings persists the reviewer's non-tracker dispositions.
//
// One failure does not abort the rest, matching applyApproved: each ruling
// concerns its own action, and losing four rulings because the fifth hit a
// constraint would discard review work for an unrelated reason.
func (a *appContext) recordRulings(rulings []triage.Ruling) error {
	var errs error
	for _, r := range rulings {
		if err := a.store.RecordRuling(r.ActionID, r.Status, r.Feedback); err != nil {
			fmt.Printf("  failed to record %s on action %d: %v\n", r.Status, r.ActionID, err)
			errs = errors.Join(errs, err)

			continue
		}

		fmt.Printf("  recorded %s on action %d\n", r.Status, r.ActionID)
	}

	return errs
}

// applyApproved hands the confirmed set to gate.Applier — the only code in
// unjira that writes to a tracker. Constructed exactly as `actions decide
// --approve` does, including the jira connections that carry
// writable_project_keys: triage must not become a second, differently-gated
// write path.
func (a *appContext) applyApproved(approved []store.ActionRow) error {
	writer, err := a.approveWriter()
	if err != nil {
		return err
	}

	applier := gate.NewApplier(a.store, writer, a.config.Tracker.DefaultProject, a.config.Jira)

	var errs error
	for _, action := range approved {
		if err := applier.Apply(action); err != nil {
			// One refusal does not abort the rest: each action targets its own
			// issue, and Applier already records the reason on that row (see
			// actions.error). Aborting would leave later actions unattempted for
			// a reason unrelated to their own validity.
			fmt.Printf("  failed %s on %s: %v\n", action.Type, action.IssueKey, err)
			errs = errors.Join(errs, err)

			continue
		}

		fmt.Printf("  applied %s on %s\n", action.Type, action.IssueKey)
	}

	return errs
}

// leaseTTL and leasePoll mirror watch's own lease settings so --refresh waits
// on the same terms the pass it is waiting for holds.
const (
	leaseTTL  = 15 * time.Minute
	leasePoll = 2 * time.Second
)
