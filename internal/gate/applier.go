package gate

import (
	"encoding/json"
	"fmt"
	"strings"

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
	// Route is the ordered hops to TargetStatus, present only when reaching it
	// takes more than one. Absent means a single hop to TargetStatus, which is
	// what every action persisted before multi-hop existed looks like.
	Route []string `json:"route,omitempty"`
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

	status := store.StatusApplied
	reason := ""
	if err != nil {
		status = store.StatusFailed
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

	target := strings.TrimSpace(p.TargetStatus)
	if target == "" {
		return fmt.Errorf("action %d: transition action has no target_status", action.ID)
	}

	// One row, one approval, N writes. The route is the whole journey the work
	// crossed between collects; the reviewer approved reaching its end, and the
	// intermediate hops are mechanical consequences of that — see the spec's
	// "coalescing is presentation-only."
	//
	// Each hop is validated live by SetStatus immediately before it executes (it
	// reads the issue's own transitions and errors naming what was available), so
	// no separate per-hop check belongs here — a second one would only add a window
	// between check and write. See docs/design-notes.md incident 16.
	route := p.Route
	if len(route) == 0 {
		route = []string{target}
	}

	for i, hop := range route {
		if err := a.writer.SetStatus(action.IssueKey, hop); err != nil {
			// Name how far it got. A partial application is a real state and must
			// be legible as one: without the reached-status prefix a reviewer
			// cannot tell a route that never started from one that stopped
			// halfway, and would have to infer position from the ticket. The next
			// reconcile pass sees the new current status, computes a shorter
			// route, and proposes the remainder — retry is the normal path, not
			// machinery here.
			if i == 0 {
				return fmt.Errorf("transitioning %s to %q: %w", action.IssueKey, hop, err)
			}

			return fmt.Errorf(
				"transitioning %s to %q: reached %q, then %q refused: %w",
				action.IssueKey, target, route[i-1], hop, err,
			)
		}
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

	// Re-check that the work is still untracked. A create is proposed from a
	// SNAPSHOT — at propose time the narrative had no link — and matching can link it
	// afterwards, in the observed case three hours later at confidence 1.0, to an
	// issue that was already Done (finding F13). Nothing between proposal and write
	// re-checked: openOrAppliedCreate inspects only other ACTIONS, never links.
	//
	// Here rather than as a queue-expiry rule because the applier is the only code
	// that writes, so "is this still true?" belongs at the write. A duplicate ticket
	// cannot be un-opened, which makes this the one check whose absence is
	// irreversible.
	//
	// Only a PRIMARY link disqualifies. A `mentioned` link is a citation, not an
	// attribution, so that narrative's work is still untracked and a create is still
	// correct; refusing on any link would make unjira unable to open a ticket for
	// work that merely references another issue.
	if key, err := a.primaryLinkFor(action.NarrativeID); err != nil {
		return fmt.Errorf("action %d: %w", action.ID, err)
	} else if key != "" {
		return fmt.Errorf(
			"action %d: narrative %d is already tracked by %s, so creating an issue would "+
				"duplicate it; the link was made after this action was proposed (reject it rather "+
				"than retrying)", action.ID, action.NarrativeID, key,
		)
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
// The two refusals are reported DIFFERENTLY, which an earlier version of this
// function deliberately did not do — it argued that "no connection says yes" and
// "a connection says no" are the same outcome from a write-authorization
// standpoint. That is true of the outcome and false of the remedy, which is what
// the message is for.
//
// A project inside a connection's project_keys but absent from
// writable_project_keys is a scope decision someone made: the fix is to add it,
// and naming the connection tells them where. A project no connection covers at
// all is not a scope decision — unjira does not track it, and the old message
// sent the reader to edit `writable_project_keys` for connection "none", a
// connection that does not exist. Following that advice is impossible.
//
// The distinction matters most for a reviewer in triage. Both refusals look
// identical in the output, and only one of them means "you can allow this if you
// want to". The other means "unjira drafted an action for work outside what it
// tracks", which is a signal about the correlator's attribution — the mention
// that produced this candidate probably should not have become a link, and
// triage's [t]arget is the remedy rather than a config edit.
func (a *Applier) checkProjectWritable(project string) error {
	conn, ok := config.Config{Jira: a.jiraConnections}.JiraConnectionForProject(project)
	if !ok {
		return fmt.Errorf(
			"project %q is not tracked by unjira: no jira connection lists it in project_keys, so "+
				"this action targets work outside what unjira manages. If %q should be tracked, add "+
				"it to a connection's project_keys (and writable_project_keys to allow writes); "+
				"otherwise retarget the action to a tracked issue",
			project, project,
		)
	}

	if !conn.IsProjectWritable(project) {
		return fmt.Errorf(
			"project %q is readable but not writable (connection %q lists it in project_keys but "+
				"not in writable_project_keys); add it there to allow writes",
			project, conn.Name,
		)
	}

	return nil
}

// There is deliberately no name-validating counterpart to the old
// normalizeStatusCategory here.
//
// That function checked target_status against the three StatusCategory values,
// which was possible because the target was drawn from a closed enum. A status
// NAME has no closed set — it is whatever the project's admin configured — so the
// only real check is "does this issue currently offer a transition to this name,"
// which is a live per-issue read.
//
// TaskWriter.SetStatus already performs exactly that read and errors, naming what
// was available, when nothing matches. Repeating it here would issue a second
// identical request and open a window between the check and the write in which
// the answer can change — while preventing nothing SetStatus does not already
// prevent. See docs/design-notes.md incident 16: a gate is only a gate if it
// stops something no other gate stops.
//
// What survives is the malformed-payload check inline in applyTransition: an
// empty target_status means the payload was never valid for its declared type,
// and is this package's business rather than a tracker round trip to spend.

// primaryLinkFor returns the issue key of narrativeID's primary link, or "" when it
// has none. A read, on the write-authority package's one narrow exception: refusing
// a duplicate needs to know what already tracks the work, and the alternative —
// passing the answer in from the caller — would put the check somewhere that does not
// write, where it could be bypassed by a second write path.
func (a *Applier) primaryLinkFor(narrativeID int64) (string, error) {
	links, err := a.store.NarrativeIssues(narrativeID)
	if err != nil {
		// Refuse rather than assume untracked: guessing wrong here opens a duplicate
		// ticket, and the failure mode of guessing the other way is one unapplied
		// action with a stated reason.
		return "", fmt.Errorf("checking existing links for narrative %d: %w", narrativeID, err)
	}

	for _, l := range links {
		if l.Role == store.RolePrimary {
			return l.IssueKey, nil
		}
	}

	return "", nil
}
