// Package triage is the interactive review session over the actions queue.
package triage

import (
	"context"
	"fmt"
	"strings"

	"github.com/jcogilvie/unjira/internal/store"
)

// Verb is one disposition a reviewer chooses for one action.
type Verb string

// The eight dispositions. Single letters at the prompt are a r e m s k t q,
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
)

// Decision is a reviewer's answer for one presented action.
type Decision struct {
	Verb Verb
	// Text carries reject/edit free-text, or the issue key for target.
	Text string
	// Positions carries merge/split's 1-based batch positions.
	Positions []int
}

// Item is one action as presented for review.
type Item struct {
	Action   store.ActionRow
	Position int
	Total    int
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
}

// NewSession builds a review session. handler may be nil: the verbs that need
// it (edit and the three restructures) then report that they are unavailable
// and the reviewer is re-prompted, which is exactly what --dry-run wants.
func NewSession(ctx context.Context, batch []store.ActionRow, p Prompter, h Handler) *Session {
	return &Session{
		batch:     batch,
		decisions: make(map[int64]Decision, len(batch)),
		prompter:  p,
		handler:   h,
		ctx:       ctx,
	}
}

// Run walks the batch, collecting a decision per action. It applies nothing.
func (s *Session) Run() error {
	for i := 0; i < len(s.batch); i++ {
		a := s.batch[i]

		d, err := s.prompter.Ask(Item{Action: a, Position: i + 1, Total: len(s.batch)})
		if err != nil {
			return fmt.Errorf("prompting for action %d: %w", a.ID, err)
		}
		switch d.Verb {
		case VerbApprove, VerbReject, VerbSkip, VerbQuit:
			// Recorded below; nothing to do but advance.
		case VerbEdit, VerbMerge, VerbSplit, VerbTarget:
			// These do work rather than merely recording a disposition. The
			// result replaces the affected actions and is re-presented, since
			// the reviewer has not seen the new text — approving text nobody
			// read is the mistake this whole surface exists to prevent.
			replaced, herr := s.handle(d, i)
			if herr != nil {
				// Re-prompt rather than abort: a failed redraft or a refused
				// merge should cost one keystroke, not a half-reviewed batch.
				if perr := s.prompter.Notify(herr.Error()); perr != nil {
					return perr
				}

				i--

				continue
			}

			s.spliceBatch(i, replaced)

			i--

			continue
		}

		if d.Verb == VerbQuit {
			// Discard every decision, not just this one. Nothing has been
			// applied yet — that is the whole point of batch apply — so a
			// reviewer who quits owes nothing and expects nothing to have
			// happened. Keeping earlier approvals would make quit a partial
			// commit, which is the surprise batch apply exists to avoid.
			s.decisions = make(map[int64]Decision, len(s.batch))

			return ErrAbandoned
		}
		s.decisions[a.ID] = d
	}

	return nil
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
	// Status is the actions.status value to record: "rejected" today.
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
			out = append(out, Ruling{ActionID: a.ID, Status: "rejected", Feedback: d.Text})
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

	case VerbApprove, VerbReject, VerbSkip, VerbQuit:
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
// not mean: gate.Applier enforces writable_project_keys, but auto_commit's
// Graduated and ConfidenceFloor live in gate.Decide, which the approve path
// never consults — deliberately, since that gate governs unattended writes and
// a human typing this flag is attending.
func (s *Session) ApproveAll() {
	for _, a := range s.batch {
		s.decisions[a.ID] = Decision{Verb: VerbApprove}
	}
}
