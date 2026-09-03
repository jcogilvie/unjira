package store

// The seven values actions.status may hold, per the schema comment in
// store.go. Declared here — not in reconciler, triage, or gate, all of which
// write or compare at least one of them — because store is the only
// cycle-free home.
//
// store imports only internal/events (verified: no import of reconciler,
// triage, or gate anywhere in this package). reconciler, triage, and gate all
// already import store for ActionRow and its accessors, so declaring the
// enum here adds no new import edge to any of them. The alternative
// considered was reconciler, which already declares StatusProposed and
// StatusDeclined (see the aliases below) — but gate does not import
// reconciler today, and gate is meant to stay the minimal write-authority
// package described in docs/architecture.md §2 ("no read methods, no
// decision logic"); making it depend on reconciler, a large LLM-drafting
// package, for two string constants would be a real new dependency for no
// return. Putting the enum in store instead means gate's existing store
// import is enough, and it matches this package's own precedent:
// StatusOpen/StatusSplit (narrativestatus.go) already live here for the
// narrative-status enum, which is schema-owned in exactly the same way.
//
// This is the same reasoning internal/correlator/match.go documents for
// IsTransportError: name the cycle-free option and say why the "more
// natural" home was rejected, rather than silently picking somewhere that
// merely happens to compile.
const (
	// StatusProposed is the actions.status value every freshly drafted action
	// lands at.
	StatusProposed = "proposed"
	// StatusApproved marks a human's approval, recorded via `actions decide
	// --approve` before gate.Applier is invoked. Transient in practice: the
	// same call chain that sets it goes on to call Applier.Apply, which
	// immediately moves the row to StatusApplied or StatusFailed. No
	// production code path leaves a row at StatusApproved on purpose, but it
	// is a legal value the schema and updateActionStatusImpl both name.
	StatusApproved = "approved"
	// StatusEdited marks the OLD row a redraft superseded — triage's [e]dit.
	// The replacement row it produced is a fresh StatusProposed row.
	StatusEdited = "edited"
	// StatusRejected marks a human's refusal — triage's [r]eject, or the old
	// row a retarget ([t]arget) superseded, since the old row named the wrong
	// issue rather than merely needing different wording.
	StatusRejected = "rejected"
	// StatusApplied marks an action that reached the tracker successfully.
	StatusApplied = "applied"
	// StatusFailed marks an action gate.Applier attempted and the tracker
	// refused; actions.error records how far it got. Never retried
	// automatically — see gate.Applier's own doc comment.
	StatusFailed = "failed"
	// StatusDeclined marks a create the MODEL judged not worth tracking —
	// distinct from StatusRejected, which is a human's ruling. See
	// reconciler.StatusDeclined, which aliases this constant.
	StatusDeclined = "declined"
)
