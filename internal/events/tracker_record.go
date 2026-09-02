package events

// ArtifactTrackerRecord marks an event as a record the tracker itself produced
// about a work item it already tracks — a changelog entry, a field edit, a
// comment posted on the issue.
//
// It answers a question no other artifact does: "if unjira writes prose sourced
// only from events like this one, is it telling anybody anything they don't have?"
// The answer is no. A tracker record is the tracker's own account of itself, so a
// comment derived from nothing else paraphrases the issue back onto the issue.
// Measured on the 2026-09-02 triage pass: 18 of 21 proposed comments were exactly
// that (see docs/design-notes.md incident 24).
//
// Why a declared marker and not an inference:
//
//   - NOT the source name. "Is this the jira collector?" is incident 21 exactly —
//     the reconciler once recognized only Jira's own changelog vocabulary and
//     would have silently never fired for a GitHub collector. A GitHub-Issues
//     collector's timeline events are tracker records too, and must be recognized
//     without editing any consumer.
//   - NOT the presence of ArtifactIssueKey. That means "this event concerns issue
//     X", which a CI run, a Slack thread or a commit can all say truthfully while
//     being real work evidence. Conflating the two would make any future collector
//     that resolves an issue key look like tracker bookkeeping.
//
// So the producing collector declares it, because the producer is the only party
// that knows whether it read a tracker's own bookkeeping or observed work.
const ArtifactTrackerRecord = "tracker_record"

// SetTrackerRecord marks evt as a record the tracker produced about itself.
//
// Collectors call this for events they mine from a tracker's changelog, field
// history or comment stream. A collector observing WORK — a session transcript, a
// CI result, a commit — must not call it, even when it can name the issue the work
// concerns.
func SetTrackerRecord(evt *Event) {
	evt.Artifacts[ArtifactTrackerRecord] = true
}

// IsTrackerRecord reports whether evt is one.
//
// Absence means "not a tracker record", which is the safe default for the
// consumers that exist: an unmarked event counts as work evidence, so a collector
// that forgets to mark its records makes unjira too talkative rather than silent.
// That is the right direction to fail — an over-eager proposal reaches a human,
// while a suppression does not.
//
// A wrongly-typed artifact reads as absence rather than panicking: Artifacts
// round-trips through JSON in the store, so any value can arrive as any type.
func IsTrackerRecord(evt Event) bool {
	marked, _ := evt.Artifacts[ArtifactTrackerRecord].(bool)

	return marked
}

// AnyWorkEvidence reports whether evts holds at least one event that is not a
// tracker record — i.e. whether unjira observed anything the tracker has no
// account of.
//
// This is the precondition for saying anything on an issue: unjira's job is to
// close the gap between what you did and what the tracker knows, so with no
// evidence of what you did there is no gap, and the "patch" would be a paraphrase
// of the thing being patched.
//
// An empty slice is false. No events means no demonstrated gap, and defaulting to
// true here would make an empty delta indistinguishable from a real observation.
func AnyWorkEvidence(evts []Event) bool {
	for _, e := range evts {
		if !IsTrackerRecord(e) {
			return true
		}
	}

	return false
}
