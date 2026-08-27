package gate

import (
	"encoding/json"
	"fmt"

	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// defaultIssueType is what a `create` action opens when applied. There is no
// per-narrative signal for issue type today — reconciler.ProposedAction
// carries no field for it (see internal/reconciler/types.go) — so this
// mirrors internal/devtools.Seed's own hardcoded "Task", the only other
// unjira code path that calls CreateIssue. Worth revisiting once triage or
// the reconciler has an actual signal to route on; noted rather than silently
// assumed.
const defaultIssueType = "Task"

// Applier holds the authority to write to a real tracker. It is the only
// type in this package — and, as of this slice, in unjira — that holds a
// tasktracker.TaskWriter rather than a TaskReader or a TaskTracker. Nothing
// else about it: no read methods, no decision logic (that is Decide's job),
// nothing that could let a caller accidentally ask it "is this legal" and get
// back a state-bearing side effect.
//
// See tasktracker.TaskWriter's own doc comment: "Holding one of these is
// authority to change the org's view of reality, so take it only in code
// that runs after the review gate." Applier is that code.
type Applier struct {
	store          *store.Store
	writer         tasktracker.TaskWriter
	defaultProject string
}

// NewApplier constructs an Applier. defaultProject routes a `create` action,
// which has no existing issue to anchor a project from — see
// config.Config.DefaultProjectConnection, whose Name identifies the same
// project this parameter expects. An empty defaultProject is not rejected
// here (Apply rejects it only when a `create` action actually needs it), so
// a caller wiring only comment/transition auto-commit need not resolve a
// default-project connection it will never use.
func NewApplier(s *store.Store, writer tasktracker.TaskWriter, defaultProject string) *Applier {
	return &Applier{store: s, writer: writer, defaultProject: defaultProject}
}

// commentPayload/transitionPayload/createPayload mirror
// internal/reconciler/persist.go's actionPayload encoding exactly — same
// field names, same JSON shape — so this package decodes symmetrically with
// what wrote the row. Decoding via typed structs (rather than a bare
// map[string]any) means a payload with the wrong shape for its declared type
// is a decode error, not a silent zero-value guess at what to send the
// tracker.
type commentPayload struct {
	Body string `json:"body"`
}

type transitionPayload struct {
	TargetStatus string `json:"target_status"`
}

type createPayload struct {
	Summary     string `json:"summary"`
	Description string `json:"description"`
}

// Apply performs exactly one action's tracker write and records the outcome
// via store.UpdateActionStatusAndError — applied on success, failed on any
// error (decode, routing, or the tracker call itself). Every path sets a
// terminal status: this method never leaves action's row at status=proposed
// once called, and never deletes it — a failure must stay visible for slice
// 6's triage to surface, per the phase-1 spec ("never silently dropped").
//
// The reason is persisted alongside the status, not just returned: a
// gate.Decide-driven auto-commit failure is otherwise only ever seen once, in
// a terminal nobody may be watching (see
// docs/superpowers/specs/2026-08-27-failure-reason-capture-design.md's "the
// gap, traced") — `actions list --status failed` is the surface that must be
// able to answer WHY, not just THAT. A success clears any reason a previous
// failed attempt left behind (a pointer to "", not nil): a human who fixes
// the underlying cause and re-approves via `actions decide --approve` must
// not have this row keep reporting the stale reason from before the fix.
//
// Not retried, on any path. A write that fails because the underlying issue
// was deleted, or because the payload was never valid for its type, will
// fail identically on a second attempt — looping here would only spend more
// of the tracker's API quota for the same outcome. Failed actions are a
// human-triage problem (slice 6), not a "try again automatically" one.
func (a *Applier) Apply(action store.ActionRow) error {
	err := a.write(action)

	status := "applied"
	reason := ""
	if err != nil {
		status = "failed"
		reason = err.Error()
	}

	// UpdateActionStatusAndError stamps executed_at for both applied and
	// failed (see its own doc comment: "only a write that actually reached
	// the tracker sets executed_at") — a failed attempt still attempted a
	// write, which is exactly the distinction that column exists to record.
	if updateErr := a.store.UpdateActionStatusAndError(action.ID, status, &reason); updateErr != nil {
		if err != nil {
			return fmt.Errorf("marking action %d failed (write error was %w): %w", action.ID, err, updateErr)
		}

		return fmt.Errorf("marking action %d applied: %w", action.ID, updateErr)
	}

	if err != nil {
		return fmt.Errorf("applying action %d: %w", action.ID, err)
	}

	return nil
}

// write performs the type-specific tracker call. A payload that doesn't
// decode for its declared type, a target status this package doesn't
// recognize, or an action type with no TaskWriter method are all loud errors
// here — none of them reach the tracker.
func (a *Applier) write(action store.ActionRow) error {
	switch action.Type {
	case "comment":
		return a.applyComment(action)
	case "transition":
		return a.applyTransition(action)
	case "create":
		return a.applyCreate(action)
	default:
		// Covers "estimate" (a real actions.type value with no TaskWriter
		// method — see tasktracker's package doc) and anything future/unknown.
		// Decide does not know about this closed set (it is a mechanical
		// (action, config) lookup — see decide.go), so this is where an
		// action type outside what unjira can actually enact surfaces.
		return fmt.Errorf("action %d: no applier for action type %q", action.ID, action.Type)
	}
}

func (a *Applier) applyComment(action store.ActionRow) error {
	if action.IssueKey == "" {
		return fmt.Errorf("action %d: comment action has no issue_key", action.ID)
	}

	var p commentPayload
	if err := json.Unmarshal([]byte(action.Payload), &p); err != nil {
		return fmt.Errorf("action %d: decoding comment payload %q: %w", action.ID, action.Payload, err)
	}

	if err := a.writer.AddComment(action.IssueKey, p.Body); err != nil {
		return fmt.Errorf("posting comment to %s: %w", action.IssueKey, err)
	}

	return nil
}

func (a *Applier) applyTransition(action store.ActionRow) error {
	if action.IssueKey == "" {
		return fmt.Errorf("action %d: transition action has no issue_key", action.ID)
	}

	var p transitionPayload
	if err := json.Unmarshal([]byte(action.Payload), &p); err != nil {
		return fmt.Errorf("action %d: decoding transition payload %q: %w", action.ID, action.Payload, err)
	}

	target, err := normalizeStatusCategory(p.TargetStatus)
	if err != nil {
		return fmt.Errorf("action %d: %w", action.ID, err)
	}

	if err := a.writer.SetStatus(action.IssueKey, target); err != nil {
		return fmt.Errorf("transitioning %s to %s: %w", action.IssueKey, target, err)
	}

	return nil
}

func (a *Applier) applyCreate(action store.ActionRow) error {
	if a.defaultProject == "" {
		return fmt.Errorf(
			"action %d: create action needs a default project, but none is configured "+
				"(tracker.default_project)", action.ID,
		)
	}

	var p createPayload
	if err := json.Unmarshal([]byte(action.Payload), &p); err != nil {
		return fmt.Errorf("action %d: decoding create payload %q: %w", action.ID, action.Payload, err)
	}

	if _, err := a.writer.CreateIssue(a.defaultProject, p.Summary, defaultIssueType, p.Description, nil); err != nil {
		return fmt.Errorf("creating issue in %s: %w", a.defaultProject, err)
	}

	return nil
}

// normalizeStatusCategory validates a decoded target_status string against
// the three normalized categories tasktracker.StatusCategory offers. Unlike
// a live legality check (that already happened in the reconciler, before
// this action was ever persisted — see AvailableStatusCategories), this is
// only a syntactic check: is the string one of the three values persist.go
// could have written at all. An unrecognized string means the payload was
// never valid for its declared type, which is this package's "malformed
// payload" case, not a tracker call to attempt and let fail.
func normalizeStatusCategory(raw string) (tasktracker.StatusCategory, error) {
	switch tasktracker.StatusCategory(raw) {
	case tasktracker.StatusTodo, tasktracker.StatusInProgress, tasktracker.StatusDone:
		return tasktracker.StatusCategory(raw), nil
	default:
		return "", fmt.Errorf("unrecognized target_status %q", raw)
	}
}
