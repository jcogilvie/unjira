// Package reconciler turns each touched narrative into a proposed action — a
// comment, a transition, or a new issue — written to the actions table with
// status=proposed.
//
// This package proposes and never applies. It holds no authorization to write
// to a tracker: per CLAUDE.md's architecture invariants, "writes are gated by
// the pipeline, not the client." Its only tracker calls are reads (GetIssue,
// AvailableStatusCategories) used to verify current state before drafting
// anything, per rules/intent-not-outcome.md.
//
// See docs/superpowers/specs/2026-08-25-reconciler-design.md.
package reconciler

import (
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// ActionType is the closed set of actions this slice can propose.
//
// estimate is deliberately absent: tasktracker has no method for it, so it is
// phase 2+ (see the spec's "What this slice does NOT do").
type ActionType string

const (
	// ActionComment posts narrative prose to an existing issue.
	ActionComment ActionType = "comment"
	// ActionTransition moves an existing issue to a new status category.
	ActionTransition ActionType = "transition"
	// ActionCreate opens a new issue. Proposed only when a narrative has no
	// verified link at all — never alongside one, or unjira would manufacture
	// duplicate tickets for work already tracked.
	ActionCreate ActionType = "create"
)

// ProposedAction is one drafted, not-yet-persisted action.
type ProposedAction struct {
	Type ActionType
	// IssueKey is empty for ActionCreate.
	IssueKey string
	// Body is the comment text for ActionComment and the description for
	// ActionCreate; empty for ActionTransition.
	Body string
	// Summary is the issue title, set only for ActionCreate.
	Summary string
	// TargetStatus is set only for ActionTransition.
	TargetStatus tasktracker.StatusCategory
	// Confidence is the model's self-reported score AFTER deterministic
	// flooring (see floorConfidence). Never the raw model number.
	Confidence float64
	Rationale  string
}

// verifiedLink pairs a stored narrative→issue link with the live issue read
// while verifying it, so drafting never needs a second GetIssue for the same
// key. AvailableStatus is the live transition legality for this issue,
// populated only when a transition is plausible.
type verifiedLink struct {
	Link            store.NarrativeIssue
	Issue           tasktracker.Issue
	AvailableStatus []tasktracker.StatusCategory
}

// ReconcileResult is one narrative's outcome, for rendering and tests.
type ReconcileResult struct {
	NarrativeID int64
	Proposed    []ProposedAction
	// Unverified holds link keys the tracker reported as not-found. They are
	// recorded and not acted on (rules/verify-correlations.md) rather than
	// failing the narrative.
	Unverified []string
	// Suppressed explains why a plausible action was NOT proposed — an empty
	// delta, a duplicate open proposal on a shared issue, an illegal
	// transition. Kept because "nothing proposed" is otherwise
	// indistinguishable from "nothing considered."
	Suppressed []string
	// SkippedNoDelta is true when every linked event predates the last
	// action, which is the steady state for an unchanged narrative.
	SkippedNoDelta bool
	// LowConfidence names proposals scoring below
	// ReconcilerConfig.MinConfidenceToPropose. They are still in Proposed and
	// still persisted — the threshold governs what unjira asserts, not what it
	// records. See noteLowConfidence.
	LowConfidence []string
}
