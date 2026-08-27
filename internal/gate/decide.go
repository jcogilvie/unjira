// Package gate is unjira's auto-commit gate — the code that decides whether
// a freshly-proposed action applies to a real tracker immediately or waits
// for a human in triage, and (separately) the code with authority to apply
// it.
//
// This is the first package in unjira that can write to a real tracker.
// Every stage before it (collector, correlator, reconciler) only proposes:
// every action lands in the actions table with status=proposed, and
// internal/reconciler holds a tasktracker.TaskReader, which cannot write —
// that split (commit ba1e369) exists for exactly this moment. Applier is
// this repo's first tasktracker.TaskWriter consumer.
//
// Split into two files, matching the phase-1 spec's own split: decide.go is
// Decide, a pure function of (action, config) with no I/O — table-driven,
// trivially testable, and incapable of surprising anyone by touching a
// network. applier.go is the write-authority half, taking a TaskWriter and
// doing nothing else. Keeping them apart means a reviewer auditing "can this
// possibly write to Jira" only has to read applier.go.
//
// See docs/superpowers/specs/2026-08-26-watch-autocommit-design.md and the
// phase-1 spec's "Auto-commit gate" section
// (docs/superpowers/specs/2026-08-11-phase1-correlator-design.md, ~line 186).
package gate

import (
	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/store"
)

// Decision is Decide's closed result. A bare bool at a call site that goes on
// to mutate a real tracker reads ambiguously (does true mean "apply" or does
// it mean "this was decided"?) — a named type removes the ambiguity at every
// call site, including this package's own applier.
type Decision int

const (
	// DecisionQueue leaves the action at status=proposed for triage. This is
	// the default outcome: an unconfigured action type, a config with no
	// auto_commit block at all, and Graduated=false at any confidence all
	// resolve here.
	DecisionQueue Decision = iota
	// DecisionApply means the action clears both the confidence floor and the
	// human-set Graduated switch for its type, and should be applied now.
	// Deciding DecisionApply does not itself write anything — see Applier.
	DecisionApply
)

// Decide is a pure function of (action, rules): no I/O, no store, no
// context, no clock. rules is the config.Config.AutoCommit map directly
// (not the whole Config) so a caller — and every test — can construct the
// only input that matters without assembling an entire Config.
//
// Applies iff action.Confidence >= rule.ConfidenceFloor && rule.Graduated.
// The floor comparison is inclusive (>=) per the phase-1 spec: a proposal
// scoring exactly at the floor applies, it does not queue.
//
// A nil or empty rules map, or a rule absent for action.Type, produces the
// zero config.AutoCommitRule (Graduated: false, ConfidenceFloor: 0) via Go's
// ordinary map-lookup-miss semantics — Decide does not special-case any of
// these; the safety property ("an unconfigured action type never
// auto-applies") is a consequence of AutoCommitRule's zero value, not of
// branching logic here that could later be edited away.
func Decide(action store.ActionRow, rules map[string]config.AutoCommitRule) Decision {
	rule := rules[action.Type]

	if rule.Graduated && action.Confidence >= rule.ConfidenceFloor {
		return DecisionApply
	}

	return DecisionQueue
}
