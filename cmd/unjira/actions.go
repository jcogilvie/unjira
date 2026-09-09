package main

// actions.go implements `unjira actions list|decide` — the machine-facing
// primitives over the actions table that slice 6's design doc
// (docs/superpowers/specs/2026-08-27-actions-primitives-design.md) calls the
// "urgent half": the review queue (26 proposed actions in the real local DB
// as of this slice) was previously visible only via sqlite3, and a `failed`
// auto-commit (internal/gate) was invisible unless someone went looking for
// it there. `triage` (a later slice) is the human-facing surface built ON
// these primitives, per the phase-1 spec's own layering
// (docs/superpowers/specs/2026-08-11-phase1-correlator-design.md, ~line 32).

import (
	"encoding/json"
	"fmt"

	"github.com/jcogilvie/unjira/internal/gate"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// actionsCmd groups the two `unjira actions` subcommands, matching devCmd's
// own grouping-struct precedent for a multi-verb command family.
type actionsCmd struct {
	List   actionsListCmd   `cmd:"" help:"List actions, defaulting to the review queue (status=proposed)."`
	Decide actionsDecideCmd `cmd:"" help:"Approve, reject, or edit one action by id."`
}

// actionsListCmd is `unjira actions list`. Default --status is "proposed"
// (the review queue), but any status is accepted — surfacing "failed" is
// half the point of building this now: watch's auto-commit gate
// (internal/gate) can silently leave an action at status=failed, and this
// command is the first thing in the repo able to show that to a human
// without sqlite3.
type actionsListCmd struct {
	JSON   bool   `help:"Emit JSON (one array, every column) instead of plain text."`
	Status string `default:"proposed" help:"Filter by status: proposed, approved, edited, rejected, applied, failed."`
}

func (c *actionsListCmd) Run(app *appContext) error {
	rows, err := app.store.ActionsByStatus(c.Status)
	if err != nil {
		return err
	}

	if c.JSON {
		// json.Marshal, not MarshalIndent: this is the scripting form
		// (--json is explicitly "for scripting" per the design doc), and a
		// jq/grep consumer has no use for indentation it will just discard.
		encoded, err := json.Marshal(rows)
		if err != nil {
			return fmt.Errorf("encoding %d action(s) as json: %w", len(rows), err)
		}

		fmt.Println(string(encoded))

		return nil
	}

	renderActionsList(rows, c.Status)

	return nil
}

// renderActionsList writes one scannable line per action: id, narrative,
// type, issue key, confidence, status — the same fields the design doc
// requires of both render forms, so a reviewer switching between --json and
// plain text never loses information depending on which they picked. A
// non-empty Error is appended to the line: this command's own doc comment
// calls surfacing status=failed "half the point of building this now", and a
// reason-less "status=failed" is exactly the gap
// docs/superpowers/specs/2026-08-27-failure-reason-capture-design.md closes —
// this is the surface that "still works tomorrow" (the design doc's words),
// so it matters more than RenderAutoCommitResult's live-pass printout.
func renderActionsList(rows []store.ActionRow, status string) {
	if len(rows) == 0 {
		fmt.Printf("no actions with status %q\n", status)

		return
	}

	for _, a := range rows {
		issueKey := a.IssueKey
		if issueKey == "" {
			issueKey = "-"
		}

		fmt.Printf("#%-5d narrative=%-6d %-10s %-12s confidence=%.2f status=%s",
			a.ID, a.NarrativeID, a.Type, issueKey, a.Confidence, a.Status)

		if a.Error != "" {
			fmt.Printf(" error=%q", a.Error)
		}

		fmt.Println()
	}
}

// actionsDecideCmd is `unjira actions decide <id> --approve|--reject|--edit
// <text>`. The three verbs are mutually exclusive and one is required (Kong's
// xor group, matching TestXorRequiredMany's pattern) — a caller cannot ask
// for two dispositions at once, and cannot ask for none.
//
// Approve is the ONLY verb that can reach a tracker: it applies the action
// for real via gate.Applier, the same write path watch's auto-commit gate
// uses. That makes this command the second place in unjira — after
// runWatchPass's own auto-commit call — that can mutate a real Jira. Reject
// and Edit are deliberately incapable of it: see Run below.
type actionsDecideCmd struct {
	ID      int64  `arg:"" help:"Action id, as printed by 'actions list'."`
	Approve bool   `xor:"decision" required:"" help:"Apply the action now via the auto-commit gate's Applier."`
	Reject  bool   `xor:"decision" required:"" help:"Mark the action rejected. Never calls the tracker."`
	Edit    string `xor:"decision" required:"" help:"Mark the action edited and persist this text as reviewer feedback. Never calls the tracker."`
}

func (c *actionsDecideCmd) Run(app *appContext) error {
	switch {
	case c.Approve:
		return app.approveAction(c.ID)
	case c.Reject:
		return app.store.UpdateActionStatus(c.ID, store.StatusRejected)
	default:
		// Kong's xor+required guarantee (see the struct's own doc comment)
		// means reaching here implies Edit was the flag actually supplied,
		// even if its value happens to be the empty string — an operator
		// clearing feedback back to nothing is a legitimate, if unusual,
		// edit, not a parse ambiguity to guess at.
		return app.store.UpdateActionStatusAndFeedback(c.ID, store.StatusEdited, c.Edit)
	}
}

// approveAction is the double-post guard the design doc names as "the test
// that stops a real-world duplicate": an action already at status=applied
// has already mutated Jira once, and re-approving it would post the same
// comment/transition/create a second time. Guarding here — before
// gate.Applier is ever consulted — means Applier itself never has to know
// this command exists; it just applies whatever ActionRow it's handed, same
// as runWatchPass's auto-commit call.
//
// failed -> approve IS allowed, deliberately: runWatchPass's auto-commit
// gate never retries a failed write on its own (internal/gate/applier.go's
// own doc comment — a retry loop would burn API quota on a write that will
// fail identically every time the underlying cause hasn't changed). A human
// explicitly re-approving after fixing that cause (a since-restored issue, a
// corrected payload via a future edit-then-approve flow) is a deliberate,
// one-shot act, not a loop — so it gets a different answer than the
// automatic path did.
//
// rejected/edited -> approve is also allowed: a reviewer who initially
// rejected or edited an action can change their mind before anything has
// been applied, and nothing about either status implies Jira has already
// been touched (proposed/rejected/edited all precede any tracker write —
// only applied/failed follow one). Refusing those would make a reviewer's
// own prior "actions decide" call irreversible for no safety reason; only
// "already applied" carries the risk this guard exists to close.
func (a *appContext) approveAction(id int64) error {
	action, err := a.store.GetAction(id)
	if err != nil {
		return err
	}

	if action.Status == store.StatusApplied {
		return fmt.Errorf(
			"action %d is already applied: approving it again would repeat its tracker write "+
				"(comment/transition/create) a second time", id,
		)
	}

	writer, err := a.approveWriter()
	if err != nil {
		return err
	}

	applier := gate.NewApplier(a.store, writer, a.config.Tracker.DefaultProject, a.config.Jira)

	return applier.Apply(action)
}

// approveWriter resolves the tasktracker.TaskWriter `actions decide
// --approve` hands to gate.Applier — deliberately typed as the narrow
// TaskWriter interface, not the full TaskTracker appContext.taskTracker
// returns, so the compiler (not a reviewer re-reading this file later)
// enforces that this command's approve path cannot also read-and-decide on
// its own. Obtaining the full TaskTracker here is fine — approveAction just
// never widens what it hands onward to Applier.
func (a *appContext) approveWriter() (tasktracker.TaskWriter, error) {
	project, err := a.projectKey("")
	if err != nil {
		return nil, err
	}

	return a.taskTracker(project)
}
