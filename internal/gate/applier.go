package gate

import (
	"encoding/json"
	"fmt"

	"github.com/jcogilvie/unjira/internal/config"
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
//
// It is also, deliberately, the write-scope choke point (see
// docs/superpowers/specs/2026-08-27-write-scope-design.md): every path that
// can reach a.writer — RunAutoCommit's automatic route and
// `actions decide --approve`'s human one — constructs an Applier and calls
// Apply, and gate.Decide is NOT on the approve path at all. So a
// writable-project check placed anywhere other than here would be a hole,
// not a second layer.
type Applier struct {
	store          *store.Store
	writer         tasktracker.TaskWriter
	defaultProject string
	// jiraConnections backs the write-scope check: which project a write
	// targets is resolved per call (from action.IssueKey for comment/
	// transition, or from defaultProject for create — see checkWritable),
	// then checked against JiraConnection.IsProjectWritable. Held as the
	// plain connection slice rather than a whole config.Config, matching
	// defaultProject's own precedent as a plain runtime field: project
	// writability is a runtime, data-dependent fact this same Applier must
	// answer differently per call (see the design doc's "Not
	// compiler-enforced, and that is deliberate").
	jiraConnections []config.JiraConnection
}

// NewApplier constructs an Applier. defaultProject routes a `create` action,
// which has no existing issue to anchor a project from — see
// config.Config.DefaultProjectConnection, whose Name identifies the same
// project this parameter expects. An empty defaultProject is not rejected
// here (Apply rejects it only when a `create` action actually needs it), so
// a caller wiring only comment/transition auto-commit need not resolve a
// default-project connection it will never use.
//
// jiraConnections is config.Config.Jira, unmodified — passed directly rather
// than resolved down to a smaller shape, since Apply must resolve a
// DIFFERENT project per call (see checkWritable) and JiraConnectionForProject
// already knows how to do that lookup; duplicating it here would risk the
// two falling out of sync.
func NewApplier(
	s *store.Store, writer tasktracker.TaskWriter, defaultProject string, jiraConnections []config.JiraConnection,
) *Applier {
	return &Applier{store: s, writer: writer, defaultProject: defaultProject, jiraConnections: jiraConnections}
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

	if err := a.checkWritable(action.IssueKey); err != nil {
		return fmt.Errorf("action %d: %w", action.ID, err)
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

	if err := a.checkWritable(action.IssueKey); err != nil {
		return fmt.Errorf("action %d: %w", action.ID, err)
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

// applyCreate opens a new issue AND links it back to the narrative that
// motivated it.
//
// The link-back is the load-bearing half, and it was missing: this function used
// to discard CreateIssue's returned key (`if _, err := ...`). Probed before
// fixing:
//
//	CreateIssue called 1 time(s), returned DEVSBX-100
//	narrative_issues rows for narrative 1: 0
//
// With no link, the narrative still has zero narrative_issues rows, so it still
// reads as untracked work — and the next pass proposes a create for it again.
// That is a loop that manufactures DUPLICATE JIRA TICKETS, each one an object
// other people reference and none of which unjira can un-create. Of the three
// mutation types this is the least recoverable, so it is also the one that most
// needed closing.
//
// The link is written even if CreateIssue succeeded but the link fails: that case
// returns an error naming the created key, so a human can attach it by hand. The
// alternative — swallowing the link error — would recreate the duplicate loop
// while reporting success.
func (a *Applier) applyCreate(action store.ActionRow) error {
	if a.defaultProject == "" {
		return fmt.Errorf(
			"action %d: create action needs a default project, but none is configured "+
				"(tracker.default_project)", action.ID,
		)
	}

	if err := a.checkProjectWritable(a.defaultProject); err != nil {
		return fmt.Errorf("action %d: %w", action.ID, err)
	}

	var p createPayload
	if err := json.Unmarshal([]byte(action.Payload), &p); err != nil {
		return fmt.Errorf("action %d: decoding create payload %q: %w", action.ID, action.Payload, err)
	}

	key, err := a.writer.CreateIssue(a.defaultProject, p.Summary, defaultIssueType, p.Description, nil)
	if err != nil {
		return fmt.Errorf("creating issue in %s: %w", a.defaultProject, err)
	}

	if key == "" {
		// A tracker that created something but told us nothing leaves us unable to
		// link it, which is the duplicate-ticket condition. Loud, because the
		// issue DOES exist and a human needs to know it is orphaned.
		return fmt.Errorf(
			"created an issue in %s but the tracker returned no key, so it cannot be linked to "+
				"narrative %d; find it and link it by hand before the next pass proposes another",
			a.defaultProject, action.NarrativeID)
	}

	if err := a.linkCreatedIssue(action.NarrativeID, key); err != nil {
		return fmt.Errorf("created %s but could not link it to narrative %d (%w); link it by hand "+
			"before the next pass proposes another", key, action.NarrativeID, err)
	}

	return nil
}

// linkCreatedIssue records the new issue as the narrative's primary.
//
// provenance is "unjira_created": distinct from every matching provenance because
// this is not an inference about where work belongs — unjira put it there.
// Confidence 1.0 for the same reason.
func (a *Applier) linkCreatedIssue(narrativeID int64, key string) error {
	return a.store.WithTx(func(tx *store.Tx) error {
		return tx.AddNarrativeIssues(narrativeID, []store.NarrativeIssue{{
			IssueKey:   key,
			Role:       "primary",
			Provenance: "unjira_created",
			Confidence: 1.0,
		}})
	})
}

// checkWritable is applyComment/applyTransition's entry into the write-scope
// choke point: it derives the project from issueKey via
// tasktracker.ProjectFromIssueKey (the only place in the repo that parses one
// — see that function's own doc comment) and defers to checkProjectWritable.
// Neither comment nor transition has a project as an input field on its own
// — only an issue key — so deriving it is this method's whole job.
func (a *Applier) checkWritable(issueKey string) error {
	project, err := tasktracker.ProjectFromIssueKey(issueKey)
	if err != nil {
		return fmt.Errorf("determining write scope for %s: %w", issueKey, err)
	}

	return a.checkProjectWritable(project)
}

// checkProjectWritable is the write-scope choke point itself: a project must
// resolve to a configured jira connection AND that connection must list it in
// writable_project_keys, or the write is refused with a message naming both
// the project and the config key — per the design doc's required error shape
// ("action N: project %q is not writable (jira[].writable_project_keys does
// not include it for connection %q)"). This is the ONLY place in unjira that
// makes this check; Apply calls it (via checkWritable/applyCreate) before
// every AddComment/SetStatus/CreateIssue call, and there is no other
// TaskWriter holder in the repo to route around it.
//
// An unresolvable project (no connection covers it at all) is reported as not
// writable too, rather than as a different kind of error: from a write-
// authorization standpoint "no connection says yes" and "a connection says no"
// are the same outcome, and a caller checking for the write-scope message
// shape should not need to distinguish them.
func (a *Applier) checkProjectWritable(project string) error {
	conn, ok := config.Config{Jira: a.jiraConnections}.JiraConnectionForProject(project)
	if !ok || !conn.IsProjectWritable(project) {
		name := "none"
		if ok {
			name = conn.Name
		}

		return fmt.Errorf(
			"project %q is not writable (jira[].writable_project_keys does not include it "+
				"for connection %q)", project, name,
		)
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
