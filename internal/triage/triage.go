// Package triage is the interactive review session over the actions queue.
package triage

import (
	"fmt"
	"strings"

	"github.com/jcogilvie/unjira/internal/store"
)

// Verb is one disposition a reviewer chooses for one action.
type Verb string

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
}

// NewSession builds a review session. handler may be nil: the verbs that need
// it (edit and the three restructures) then report that they are unavailable
// and the reviewer is re-prompted, which is exactly what --dry-run wants.
func NewSession(batch []store.ActionRow, p Prompter, h Handler) *Session {
	return &Session{
		batch:     batch,
		decisions: make(map[int64]Decision, len(batch)),
		prompter:  p,
		handler:   h,
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
	Redraft(action store.ActionRow, feedback string) (store.ActionRow, error)
	// Restructure performs a merge/split/retarget and returns the actions that
	// replace the affected ones. Returning a slice (not one action) is why
	// re-presentation exists: a split yields more actions than it consumed.
	Restructure(d Decision, batch []store.ActionRow) ([]store.ActionRow, error)
}

// handle runs the verbs that require work, returning the actions that replace
// the one at index i.
func (s *Session) handle(d Decision, i int) ([]store.ActionRow, error) {
	if s.handler == nil {
		return nil, fmt.Errorf("%s is not available in this session", d.Verb)
	}

	if d.Verb == VerbEdit {
		replacement, err := s.handler.Redraft(s.batch[i], d.Text)
		if err != nil {
			return nil, err
		}

		return []store.ActionRow{replacement}, nil
	}

	return s.handler.Restructure(d, s.batch)
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
