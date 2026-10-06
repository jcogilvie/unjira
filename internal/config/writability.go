package config

import "fmt"

// Writability is whether unjira may write to a project, and if not, why — the
// single answer both gate.Applier (at apply time) and triage (at review time) read.
//
// WHY THIS TYPE EXISTS. Write scope was checked only inside gate.Applier, so a
// reviewer worked through an action, judged it, approved it, and only then learned it
// could not be applied. Measured on a real queue: 17 proposed actions targeting
// PAAS/RE/SUMO/SIA against a writable scope of ["DEVSBX"] — every one unappliable,
// with nothing saying so until approval.
//
// It is a struct rather than an error because the two refusals need DIFFERENT
// remedies, and a caller has to be able to tell them apart without parsing prose:
//
//   - Untracked false, Writable false — a scope decision someone made. The project is
//     readable; the fix is adding it to writable_scopes on the named tracker.
//   - Untracked true — unjira does not track this project at all, so it drafted an
//     action for work outside what it manages. That is a signal about the correlator's
//     attribution: the mention that produced the link probably should not have, and
//     the remedy is retargeting the action, not editing config.
//
// Pure, so triage can consult it per action with no I/O and no tracker call.
type Writability struct {
	// Writable is the only field a caller needs when the answer is yes.
	Writable bool
	// Untracked distinguishes "no tracker's scopes cover this project" from "covered
	// but not writable". See the type comment for why the difference decides what a
	// reviewer should do.
	Untracked bool
	// Tracker names the tracker whose scopes cover the project, so a config fix has an
	// address. Empty when Untracked, because naming tracker "" is the bug an earlier
	// version of this message actually shipped.
	Tracker string
	// Reason is operator-facing prose, empty when Writable. Never parsed.
	Reason string
}

// ProjectWritability answers whether projectKey may be written to.
//
// Deny-by-default at both layers: a project no tracker covers is untracked, and a
// covered project absent from writable_scopes is refused. A fresh clone configures
// neither, so a fresh clone writes nothing — the property the README's Status section
// states, and the reason `config/unjira.example.yaml` arms no writable scope.
func (c Config) ProjectWritability(projectKey string) Writability {
	tracker, ok := c.TrackerForScope(projectKey)
	if !ok {
		return Writability{
			Untracked: true,
			Reason: fmt.Sprintf(
				"project %q is not tracked by unjira: no tracker lists it in scopes, so this action "+
					"targets work outside what unjira manages. If %q should be tracked, add it to a "+
					"tracker's scopes (and writable_scopes to allow writes); otherwise retarget the "+
					"action to a tracked issue",
				projectKey, projectKey),
		}
	}

	if !tracker.IsScopeWritable(projectKey) {
		return Writability{
			Tracker: tracker.Name,
			Reason: fmt.Sprintf(
				"project %q is readable but not writable (tracker %q lists it in scopes but not in "+
					"writable_scopes); add it there to allow writes",
				projectKey, tracker.Name),
		}
	}

	return Writability{Writable: true, Tracker: tracker.Name}
}
