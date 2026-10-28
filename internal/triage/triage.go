// Package triage is the interactive review session over the actions queue.
package triage

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/llm"
	"github.com/jcogilvie/unjira/internal/reconciler"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// Verb is one disposition a reviewer chooses for one action.
type Verb string

// The nine dispositions. Single letters at the prompt are a r e m s k t d q,
// all distinct — skip takes k precisely because s belongs to split, and a
// collision would make one disposition unreachable.
const (
	VerbApprove Verb = "approve"
	VerbReject  Verb = "reject"
	VerbEdit    Verb = "edit"
	VerbMerge   Verb = "merge"
	VerbSplit   Verb = "split"
	VerbSkip    Verb = "skip"
	VerbQuit    Verb = "quit"
	VerbTarget  Verb = "target"
	VerbDrop    Verb = "drop"
)

// WorklogRefusal is the fixed message a reviewer is shown when a work verb is
// aimed at a worklog action (R17). Exported so the terminal surface and any
// future prompter can print the same words the Session guards with.
const WorklogRefusal = "a worklog is computed from the token ledger: " +
	"edit, target, merge and split do not apply; approve, reject or skip it"

// DropRefusal is the fixed message a reviewer is shown when drop is aimed at an
// action reviewed alone: drop removes a member from an approval unit, and a
// single action has no unit to remove it from.
const DropRefusal = "drop removes one member of an approval unit; this action is reviewed alone: reject it instead"

// Decision is a reviewer's answer for one presented action.
type Decision struct {
	Verb Verb
	// Text carries reject/edit/drop free-text, or the issue key for target.
	Text string
	// Positions carries merge/split/drop's 1-based batch positions.
	Positions []int
}

// NarrativeContext is the work an action was drafted from, as a reviewer needs to
// see it. A subset of store.NarrativeRow rather than the row itself: a prompter
// needs a human-readable "what work is this?" and nothing more, and passing the
// whole row would invite rendering window boundaries and compaction internals at
// someone who cannot act on them.
type NarrativeContext struct {
	Title   string
	Summary string
	// ToConfirm are the narrative's member events placed below
	// correlator.member_confidence_floor: the model's attributions of work to this
	// narrative that it was least sure of. Surfaced so a reviewer can confirm them,
	// or correct them with merge or split. Empty while the floor is 0 (off).
	ToConfirm []Attribution
}

// Attribution is one member event and the confidence it was placed with, as a
// reviewer reads it.
type Attribution struct {
	Source     string
	Summary    string
	Confidence float64
}

// Item is one action as presented for review, with the context a reviewer needs
// to judge it.
//
// Action alone is not enough, and that was learned the hard way: presented with a
// comment proposal for PAAS-3969, a reviewer said "this doesn't actually tell me
// about the ticket itself, so i really don't have what i need to decide". The body
// and the model's own rationale were all Ask rendered — and the rationale is the
// thing under review, so it must not also be the reviewer's only source of
// context.
//
// Narrative and Issue are both BEST-EFFORT and may be zero. A tracker outage, a
// deleted ticket, or a `create` with no issue key at all must still yield a
// reviewable Item: context aids judgment rather than gating it, and losing a
// half-reviewed batch to a failed decoration would be strictly worse than
// reviewing without the decoration.
type Item struct {
	Action store.ActionRow
	// Narrative is the work this action was drafted from. It was already in the
	// store and simply never reached the prompter.
	Narrative NarrativeContext
	// Issue is the LIVE target issue, read at review time.
	//
	// Live rather than stored because the store has no ticket snapshot to offer:
	// the Jira collector records status transitions and field edits, never the
	// issue itself, so `PAAS-3969 status: Discovery → In Progress` was the most a
	// reviewer could learn locally. Reading now is also the freshest answer — a
	// status changed since collection shows the reviewer the truth rather than
	// unjira's stale belief.
	//
	// Zero for a `create`, which has no issue until it is applied.
	Issue tasktracker.Issue
	// Appliable reports whether gate.Applier would accept this action's target
	// project — asked HERE, at review time, rather than only after approval.
	//
	// Write scope used to be checked exclusively inside gate.Applier, so a reviewer
	// read the issue, read the work, read the drafted prose, judged it, pressed [a],
	// and only then learned it could not be written. Measured on a rebuilt queue: 17
	// of 17 proposed actions were unappliable and nothing said so.
	//
	// True when the Session has no config to consult (see SetWritability): silence is
	// not a refusal, and an unconfigured Session must not claim to know write scope.
	Appliable bool
	// UnappliableReason is operator-facing prose naming the REMEDY, empty when
	// Appliable. The two refusals differ in what a reviewer should do — edit
	// writable_scopes, or retarget an action drafted for work unjira does not
	// track — which is why this carries config.Writability's message rather than a
	// bare flag.
	UnappliableReason string
	Position          int
	Total             int
	// Members is set only for an approval unit (D-18): a comment or transition and
	// every worklog joined to its narrative, in batch order, reviewed with one
	// decision. Each member is decorated and given writability as a single Item would
	// be, and keeps its own flat Position. Action, Position, Narrative and Issue are
	// then the unit's comment or transition, the member the work verbs act on (R7).
	// Appliable is true only when every member is appliable, and UnappliableReason
	// names each member that is not.
	Members []Item
}

// Prompter is how a Session asks a human. cmd/unjira implements it against a
// terminal; tests implement it as a script. The Session never touches stdin.
type Prompter interface {
	Ask(item Item) (Decision, error)
	Confirm(summary string) (bool, error)
	// Notify reports a recoverable problem — a refused merge, a failed
	// redraft — without ending the session.
	Notify(message string) error
}

// Session holds one review pass in memory.
type Session struct {
	batch     []store.ActionRow
	decisions map[int64]Decision
	prompter  Prompter
	handler   Handler
	// ctx bounds the LLM calls the handler makes on this pass. Held here rather
	// than on the handler because a Session IS one pass — its lifetime and the
	// context's are the same — whereas a handler is a long-lived dependency.
	ctx context.Context //nolint:containedctx // a Session is one pass; see above
	// issueCache memoizes IssueContext per key for this pass. A queue routinely
	// holds many actions on one issue (one real batch had ten on PAAS-4019), and
	// re-reading the same ticket per action would multiply the reviewer's wait by
	// the batch size for no new information. Per-pass, not longer-lived: within one
	// review the ticket is not expected to change, but across passes it may.
	issueCache map[string]tasktracker.Issue
	// writability answers "may unjira write to this action's project" per item, with
	// no I/O. Nil means the Session was given no config, in which case every action
	// reads as appliable — an unconfigured Session must not claim to know write scope,
	// and every existing caller and test constructs one that way.
	writability *config.Config
	// units maps a worklog action id to the narrative whose comment or transition it
	// is reviewed with (JoinWorklogs). Nil means no units: every action is asked alone.
	units map[int64]int64
}

// SetUnits gives the Session the worklog joins that group actions into approval
// units (D-18). A setter for SetWritability's reason: every existing caller and test
// would otherwise pass a map it does not care about. Nil means no units.
func (s *Session) SetUnits(joins map[int64]int64) {
	s.units = joins
}

// SetWritability gives the Session the config it needs to tell a reviewer, before they
// spend judgment, that an action cannot be applied.
//
// A setter rather than a NewSession parameter for the reason WithLogger is an option
// elsewhere in this tree: every existing caller and test would otherwise need editing
// to pass something they do not care about, and the zero behaviour (report everything
// appliable) is the correct default for a Session that was told nothing.
func (s *Session) SetWritability(cfg config.Config) {
	s.writability = &cfg
}

// itemWritability is the per-action verdict, defaulting to appliable when the Session
// holds no config.
//
// The project is derived from the issue key's prefix, matching how gate.Applier
// resolves it for comment and transition actions. A `create` has no issue key yet, so
// it reads as appliable here and gate.Applier checks it against defaultProject at
// apply time — deliberately NOT duplicated, since this surface has no way to know
// which project a create would land in.
func (s *Session) itemWritability(a store.ActionRow) (bool, string) {
	if s.writability == nil || a.IssueKey == "" {
		return true, ""
	}

	w := s.writability.IssueWritability(a.IssueKey)

	return w.Writable, w.Reason
}

// NewSession builds a review session. handler may be nil: the verbs that need
// it (edit and the three restructures) then report that they are unavailable
// and the reviewer is re-prompted, which is exactly what --dry-run wants. The
// handler's LLM calls are recorded under the triage stage.
func NewSession(ctx context.Context, batch []store.ActionRow, p Prompter, h Handler) *Session {
	return &Session{
		batch:     batch,
		decisions: make(map[int64]Decision, len(batch)),
		prompter:  p,
		handler:   h,
		ctx:       llm.WithStage(ctx, "triage"),

		issueCache: make(map[string]tasktracker.Issue),
	}
}

// Run walks the batch, collecting a decision per action. It applies nothing.
func (s *Session) Run() error {
	for i := 0; i < len(s.batch); i++ {
		// A unit presented at an earlier member already decided this one. Nothing
		// else is ever decided when the walk reaches it.
		if _, decided := s.decisions[s.batch[i].ID]; decided {
			continue
		}

		// ai is the index of item.Action: i for a single action, the unit's comment
		// or transition for a unit. The work verbs act on it, never on a worklog.
		item, members, ai := s.present(i)
		a := item.Action

		d, err := s.prompter.Ask(item)
		if err != nil {
			return fmt.Errorf("prompting for action %d: %w", a.ID, err)
		}

		// Refuse approve on an unappliable action rather than trusting the prompter to
		// have hidden the verb. A Prompter is an interface — a script, a future TUI, a
		// Slack surface — so the display is a courtesy and this is the guard. Approving
		// here would only defer bad news gate.Applier is going to deliver anyway, and
		// leave a `failed` row where a reviewer expected an `applied` one.
		//
		// Only APPROVE is gated. Reject stays available because rejecting an
		// unappliable action is exactly the right disposition, and target stays
		// available because it is the documented remedy for the untracked case —
		// blocking either would leave rows nobody can dispose of.
		if d.Verb == VerbApprove && !item.Appliable {
			if err := s.prompter.Notify(
				"cannot apply: " + item.UnappliableReason); err != nil {
				return fmt.Errorf("notifying unappliable action %d: %w", a.ID, err)
			}

			i--

			continue
		}

		switch d.Verb {
		case VerbApprove, VerbReject, VerbSkip:
			// Recorded below; nothing to do but advance.
		case VerbQuit:
			// Discard every decision, not just this one. Nothing has been
			// applied yet — that is the whole point of batch apply — so a
			// reviewer who quits owes nothing and expects nothing to have
			// happened. Keeping earlier approvals would make quit a partial
			// commit, which is the surprise batch apply exists to avoid.
			s.decisions = make(map[int64]Decision, len(s.batch))

			return ErrAbandoned
		case VerbDrop:
			if perr := s.drop(d, members); perr != nil {
				return perr
			}

			// Re-present what is left: the dropped member is decided, so the
			// unit re-forms without it, or dissolves to a single action.
			i--

			continue
		case VerbEdit, VerbMerge, VerbSplit, VerbTarget:
			// A worklog has no narrative text to rework and carries narrative 0
			// (R2), so edit and target would hand Redraft/Retarget a narrative id
			// that is not one, and merge/split would route it through ReworkOne
			// the same way (R17). Refused before the handler is ever consulted,
			// so a nil-handler dry run reports the same refusal a live one does.
			if s.worklogTouched(d, ai) {
				if perr := s.prompter.Notify(WorklogRefusal); perr != nil {
					return fmt.Errorf("notifying worklog action %d: %w", a.ID, perr)
				}

				i--

				continue
			}

			// These do work rather than merely recording a disposition. The
			// result replaces the affected actions and is re-presented, since
			// the reviewer has not seen the new text — approving text nobody
			// read is the mistake this whole surface exists to prevent.
			// Collected before handle, which may reorder the batch.
			touched := s.restructured(d, ai)

			replaced, herr := s.handle(d, ai)
			if herr != nil {
				// Re-prompt rather than abort: a failed redraft or a refused
				// merge should cost one keystroke, not a half-reviewed batch.
				if perr := s.prompter.Notify(herr.Error()); perr != nil {
					return perr
				}

				i--

				continue
			}

			s.dissolveUnits(touched)
			s.spliceBatch(ai, replaced)

			// Re-present from i, which is at most ai, so a unit whose worklog comes
			// first is re-presented whole.
			i--

			continue
		}

		s.record(d, a, members)
	}

	return nil
}

// restructured returns the narrative ids a target, merge or split decision on
// the action at index ai touches: the action's own and every d.Positions row's.
// These verbs change which narrative holds which work, so the message->narrative
// attribution a unit's join was computed from no longer holds for any of them.
// Edit only rewords text and keeps the unit, so it touches none.
func (s *Session) restructured(d Decision, ai int) []int64 {
	if d.Verb == VerbEdit {
		return nil
	}

	touched := []int64{s.batch[ai].NarrativeID}
	for _, p := range d.Positions {
		if p >= 1 && p <= len(s.batch) {
			touched = append(touched, s.batch[p-1].NarrativeID)
		}
	}

	return touched
}

// dissolveUnits removes every unit joined to one of narratives, so its members
// are presented alone from then on.
func (s *Session) dissolveUnits(narratives []int64) {
	for id, n := range s.units {
		if slices.Contains(narratives, n) {
			delete(s.units, id)
		}
	}
}

// drop records the one member d.Positions names as rejected with d.Text (R5): a
// drop IS a reject of that member. Outside a unit, or naming anything but exactly
// one member, it notifies the reviewer and records nothing.
func (s *Session) drop(d Decision, members []int) error {
	if members == nil {
		if err := s.prompter.Notify(DropRefusal); err != nil {
			return fmt.Errorf("notifying drop refusal: %w", err)
		}

		return nil
	}

	positions := make([]string, 0, len(members))
	named := -1

	for _, j := range members {
		positions = append(positions, fmt.Sprint(j+1))

		if len(d.Positions) == 1 && d.Positions[0] == j+1 {
			named = j
		}
	}

	if named < 0 {
		msg := "drop names one member of this unit: positions " + strings.Join(positions, ", ")
		if err := s.prompter.Notify(msg); err != nil {
			return fmt.Errorf("notifying drop refusal: %w", err)
		}

		return nil
	}

	s.decisions[s.batch[named].ID] = Decision{Verb: VerbReject, Text: d.Text}

	return nil
}

// record writes an approve, reject or skip: on action a alone, or on every member
// of a's unit when members is non-nil. A unit's one decision is each member's own,
// so Approved() and Rulings() see exactly what per-action decisions produce (D-21):
// a reject copies its feedback onto each member.
func (s *Session) record(d Decision, a store.ActionRow, members []int) {
	if members == nil {
		s.decisions[a.ID] = d

		return
	}

	for _, j := range members {
		s.decisions[s.batch[j].ID] = d
	}
}

// present builds the Item for batch index i, returning the batch indices of its
// unit's members (nil for a single action) and the index of Item.Action.
func (s *Session) present(i int) (Item, []int, int) {
	members := s.unitAt(i)
	if members == nil {
		return s.itemAt(i), nil, i
	}

	var (
		unit    Item
		ai      int
		reasons []string
		items   = make([]Item, 0, len(members))
	)

	for _, j := range members {
		m := s.itemAt(j)
		items = append(items, m)

		if m.Action.Type != store.ActionTypeWorklog {
			unit, ai = m, j
		}

		if !m.Appliable {
			reasons = append(reasons, fmt.Sprintf("position %d (%s on %s): %s",
				m.Position, m.Action.Type, m.Action.IssueKey, m.UnappliableReason))
		}
	}

	unit.Members = items
	unit.Appliable, unit.UnappliableReason = true, ""

	if len(reasons) > 0 {
		reasons = append(reasons, "drop each member that cannot be applied (d <n>), or reject the unit")
		unit.Appliable, unit.UnappliableReason = false, strings.Join(reasons, "\n")
	}

	return unit, members, ai
}

// itemAt builds the single, decorated Item for batch index i.
func (s *Session) itemAt(i int) Item {
	a := s.batch[i]

	item := Item{Action: a, Position: i + 1, Total: len(s.batch)}
	item.Appliable, item.UnappliableReason = s.itemWritability(a)
	s.decorate(&item)

	return item
}

// unitAt returns the batch indices of i's approval unit in batch order, or nil
// when i is in none. A unit is the undecided actions of one narrative: exactly one
// comment or transition and at least one worklog joined to it (R1). Computed at
// walk time from the batch and the decisions, so a splice reshapes the unit with
// no bookkeeping of its own.
func (s *Session) unitAt(i int) []int {
	if len(s.units) == 0 {
		return nil
	}

	n, ok := s.unitNarrative(s.batch[i])
	if !ok {
		return nil
	}

	var (
		members           []int
		actions, worklogs int
	)

	for j, a := range s.batch {
		if _, decided := s.decisions[a.ID]; decided {
			continue
		}

		if m, ok := s.unitNarrative(a); !ok || m != n {
			continue
		}

		members = append(members, j)

		if a.Type == store.ActionTypeWorklog {
			worklogs++
		} else {
			actions++
		}
	}

	if actions != 1 || worklogs == 0 {
		return nil
	}

	return members
}

// unitNarrative is the narrative an action would share a unit through: its join
// for a worklog, its own narrative for a comment or transition.
func (s *Session) unitNarrative(a store.ActionRow) (int64, bool) {
	switch a.Type {
	case store.ActionTypeWorklog:
		n, ok := s.units[a.ID]

		return n, ok
	case string(reconciler.ActionComment), string(reconciler.ActionTransition):
		return a.NarrativeID, true
	}

	return 0, false
}

// decorate attaches the narrative and live issue a reviewer needs, best-effort.
//
// EVERY failure here is swallowed deliberately, and that is the load-bearing
// property: context is an aid to judgment, not a precondition for it. A tracker
// outage or a deleted ticket must not end a review the human is midway through,
// because losing a half-reviewed batch is strictly worse than reviewing one item
// without its decoration. The reviewer sees an empty summary and can still decide
// — or skip, which is the honest move when context is missing.
//
// Not surfaced through Notify either: a per-item warning on every action of a
// batch during a tracker outage would bury the actions themselves, and the empty
// field already says "unknown" as clearly as a message would.
func (s *Session) decorate(item *Item) {
	if s.handler == nil {
		return
	}

	if item.Action.NarrativeID != 0 {
		if n, err := s.handler.NarrativeContext(item.Action.NarrativeID); err == nil {
			item.Narrative = n
		}
	}

	// A create has no issue key until it is applied, so there is nothing to read.
	if item.Action.IssueKey == "" {
		return
	}

	if cached, ok := s.issueCache[item.Action.IssueKey]; ok {
		item.Issue = cached

		return
	}

	issue, err := s.handler.IssueContext(item.Action.IssueKey)
	if err != nil {
		return
	}

	// Cached even when zero-valued, so a key that resolves to nothing is not
	// re-read once per action for the rest of the pass.
	s.issueCache[item.Action.IssueKey] = issue
	item.Issue = issue
}

// Approved returns the actions the reviewer approved, in batch order.
func (s *Session) Approved() []store.ActionRow {
	var out []store.ActionRow
	for _, a := range s.batch {
		if s.decisions[a.ID].Verb == VerbApprove {
			out = append(out, a)
		}
	}

	return out
}

// Ruling is one recorded disposition that must be written to the store.
type Ruling struct {
	ActionID int64
	// Status is the actions.status value to record: store.StatusRejected today.
	Status string
	// Feedback is the reviewer's free text, read later by slice 7's
	// rules.Distill.
	Feedback string
}

// Rulings returns the dispositions that must be PERSISTED but do not write to a
// tracker — today, every reject.
//
// This exists because PR #26 shipped without it and thereby lost every ruling. A
// Session recorded reject text in memory and cmd/unjira only ever called
// Approved(), so a rejected action stayed at status=proposed, came back in the
// next session, and actions.feedback stayed NULL. The triage spec asserted the
// opposite — "actions.feedback is already persisted by [r]eject/[e]dit, so slice
// 7 will have its input waiting" — which was simply untrue, and untrue in a way
// nothing surfaced: rejecting looked like it worked.
//
// Kept separate from Approved() rather than folded into one "everything to
// write" method, because the two have different stakes. Approved() feeds
// gate.Applier and can mutate someone's Jira; this only ever writes to the local
// store. A caller must not be able to confuse them, and a reader of
// cmd/unjira/triage.go should be able to see which is which.
//
// Skip is deliberately absent: leaving an action at proposed for a later session
// IS the outcome, so there is nothing to record. Edit and target are absent too,
// because StoreHandler already persisted them via SupersedeAction — they had to
// be written during the session to have an id at all.
func (s *Session) Rulings() []Ruling {
	var out []Ruling
	for _, a := range s.batch {
		d := s.decisions[a.ID]
		if d.Verb == VerbReject {
			out = append(out, Ruling{ActionID: a.ID, Status: store.StatusRejected, Feedback: d.Text})
		}
	}

	return out
}

// Summary renders the pending dispositions for the final confirmation.
func (s *Session) Summary() string {
	counts := map[Verb]int{}
	for _, d := range s.decisions {
		counts[d.Verb]++
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d approve, %d reject, %d edit, %d skip",
		counts[VerbApprove], counts[VerbReject], counts[VerbEdit], counts[VerbSkip])

	return b.String()
}

// ErrAbandoned is returned when the reviewer quits: nothing is applied.
var ErrAbandoned = fmt.Errorf("triage abandoned by reviewer")

// Handler performs the verbs that do more than record a disposition. Session
// calls it and re-presents whatever comes back; it knows nothing about
// clustering, drafting, or the store.
//
// Separate from Prompter because these are two different axes: Prompter is how
// we ask a human, Handler is what happens when the answer requires work. A test
// can script one and stub the other.
type Handler interface {
	// NarrativeContext returns the work an action was drafted from, for display.
	//
	// Read-only and display-only, which is why it sits on Handler rather than
	// Session taking a *store.Store: Session deliberately touches no storage, and
	// giving it one to satisfy a display concern would widen it past "collect
	// decisions" for no gain.
	NarrativeContext(narrativeID int64) (NarrativeContext, error)
	// IssueContext returns the LIVE issue an action targets, for display.
	//
	// A live read because the store holds no ticket snapshot — see Item.Issue.
	// Errors are the caller's to swallow: Session degrades to no context rather
	// than aborting a review the human is midway through.
	IssueContext(issueKey string) (tasktracker.Issue, error)
	// Redraft returns a replacement action for an edit.
	//
	// Takes a context because it makes an LLM call. Session holds one for exactly
	// this reason rather than the handler storing one: a stored context outlives
	// the call it was made for and cannot be cancelled per-operation, which is
	// what golangci-lint's containedctx check objects to and it is right to.
	Redraft(ctx context.Context, action store.ActionRow, feedback string) (store.ActionRow, error)
	// Retarget moves this action's work to a different issue and returns the
	// replacement.
	//
	// Separate from Restructure despite the design calling both "restructures",
	// because the shapes genuinely differ: retarget concerns ONE action and yields
	// exactly one replacement, while merge and split operate across batch
	// positions and may yield zero or many. Folding it in would mean routing an
	// issue key through Decision.Positions, which has no position to carry, and
	// giving up the compiler's guarantee that exactly one action comes back.
	Retarget(ctx context.Context, action store.ActionRow, issueKey string) (store.ActionRow, error)
	// Restructure performs a merge/split and returns the actions that replace the
	// affected ones. Returning a slice (not one action) is why re-presentation
	// exists: a split yields more actions than it consumed.
	Restructure(ctx context.Context, d Decision, batch []store.ActionRow) ([]store.ActionRow, error)
}

// worklogTouched reports whether a work-verb decision touches any worklog
// action in the batch: the one at index i (edit, target, merge and split all
// act on the item being prompted), plus each 1-based position in
// d.Positions (merge and split name further batch members). A position out of
// range reads as not touching a worklog — Restructure is the surface that
// validates positions, and duplicating its error here would only fork the
// message a reviewer sees.
func (s *Session) worklogTouched(d Decision, i int) bool {
	if s.batch[i].Type == store.ActionTypeWorklog {
		return true
	}

	for _, p := range d.Positions {
		if p >= 1 && p <= len(s.batch) && s.batch[p-1].Type == store.ActionTypeWorklog {
			return true
		}
	}

	return false
}

// handle runs the verbs that require work, returning the actions that replace
// the one at index i.
func (s *Session) handle(d Decision, i int) ([]store.ActionRow, error) {
	if s.handler == nil {
		return nil, fmt.Errorf("%s is not available in this session", d.Verb)
	}

	switch d.Verb {
	case VerbEdit:
		replacement, err := s.handler.Redraft(s.ctx, s.batch[i], d.Text)
		if err != nil {
			return nil, err
		}

		return []store.ActionRow{replacement}, nil

	case VerbTarget:
		replacement, err := s.handler.Retarget(s.ctx, s.batch[i], d.Text)
		if err != nil {
			return nil, err
		}

		return []store.ActionRow{replacement}, nil

	case VerbMerge, VerbSplit:
		return s.handler.Restructure(s.ctx, d, s.batch)

	case VerbApprove, VerbReject, VerbSkip, VerbQuit, VerbDrop:
		// Unreachable: Run only calls handle for the four work verbs. Named
		// explicitly rather than left to a default, so that adding a Verb forces a
		// decision here — and loud rather than falling through to Restructure,
		// which would silently treat an approve as a merge if Run's dispatch ever
		// drifted from this switch.
		return nil, fmt.Errorf("%s does not require a handler", d.Verb)
	}

	return nil, fmt.Errorf("unknown verb %q", d.Verb)
}

// spliceBatch replaces the action at index i with replacements, dropping any
// decision recorded for the action being replaced.
//
// Dispositions for OTHER actions survive untouched — that is what batch apply
// buys: a restructure late in the batch does not discard the reviewer's earlier
// judgments, because none of them have been applied yet.
func (s *Session) spliceBatch(i int, replacements []store.ActionRow) {
	delete(s.decisions, s.batch[i].ID)

	out := make([]store.ActionRow, 0, len(s.batch)-1+len(replacements))
	out = append(out, s.batch[:i]...)
	out = append(out, replacements...)
	out = append(out, s.batch[i+1:]...)
	s.batch = out
}

// ApproveAll records approve for every action without prompting, for
// --auto-approve.
//
// Still goes through the same Approved()/Commit path as an interactive session,
// so the flag skips the PROMPT and nothing else. Note what that does and does
// not mean: gate.Applier enforces writable_scopes, but auto_commit's
// Graduated and ConfidenceFloor live in gate.Decide, which the approve path
// never consults — deliberately, since that gate governs unattended writes and
// a human typing this flag is attending.
func (s *Session) ApproveAll() {
	for _, a := range s.batch {
		s.decisions[a.ID] = Decision{Verb: VerbApprove}
	}
}
