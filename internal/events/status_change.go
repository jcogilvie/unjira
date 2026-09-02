package events

// Artifact keys with meaning to consumers outside the collector that wrote them.
//
// Artifacts is a bare map[string]any, so any key is otherwise a private
// convention between one collector and whichever consumer happened to hardcode
// the same string literal. Naming the cross-package ones here makes the contract
// findable and the compiler a participant: a consumer reading ArtifactIssueKey is
// reading a declared contract, where one reading "issue_key" is guessing.
//
// Only keys read outside their producing collector belong here. A key a collector
// writes purely for its own bookkeeping stays private to it — this is the shared
// vocabulary, not a registry of everything anyone ever put in the map.
const (
	// ArtifactIssueKey is the tracked-work item this event concerns, in the
	// backend's own key syntax ("PAAS-4038", or "owner/repo#7" for a backend
	// that shapes keys that way). Read by the correlator when gathering
	// candidates and by the reconciler when judging staleness.
	ArtifactIssueKey = "issue_key"

	// ArtifactStatusFrom and ArtifactStatusTo record a status change's
	// endpoints, in the backend's own status vocabulary.
	//
	// Presence of ArtifactStatusTo is what makes an event a status change — see
	// StatusChangeOf. Deliberately NOT a backend-specific marker like Jira's
	// `field: "status"` changelog vocabulary: GitHub Issues has no `field`
	// concept at all (open/closed arrives as a timeline event), so a consumer
	// testing for one could never see a GitHub status change, and a GitHub
	// collector would have to emit a fake Jira-shaped artifact to be noticed.
	ArtifactStatusFrom = "status_from"
	ArtifactStatusTo   = "status_to"
)

// StatusChange is one observed change of a tracked item's status, as some
// collector recorded it.
//
// The backend's own status names, not a normalized category: several distinct
// statuses routinely share a category, so a category cannot say which move
// happened (see tasktracker.StatusCategory's doc comment for the measurement
// that settled this).
type StatusChange struct {
	// IssueKey is the item that moved.
	IssueKey string
	// From is the status it left. Empty when the backend does not report one —
	// an item's first status has no predecessor, and GitHub's "closed" timeline
	// event names no prior state. Informational only.
	From string
	// To is the status it reached. Never empty: StatusChangeOf reports an event
	// with no destination as not-a-status-change rather than returning one, and
	// SetStatusChange refuses to record one.
	To string
}

// SetStatusChange records a status change on evt, for a collector emitting one.
//
// A no-op when to is empty. A status change with no destination is not a status
// change, and recording one would produce an event present enough to look like
// evidence and empty enough to be useless — which a consumer then has to defend
// against.
func SetStatusChange(evt *Event, issueKey, from, to string) {
	if to == "" {
		return
	}

	evt.Artifacts[ArtifactIssueKey] = issueKey
	evt.Artifacts[ArtifactStatusFrom] = from
	evt.Artifacts[ArtifactStatusTo] = to
}

// StatusChangeOf reports whether evt records a status change, and what it was.
//
// Source-agnostic on purpose: a Jira changelog transition and a GitHub
// closed-timeline event are the same fact to a consumer, and a consumer that
// switched on Source would need editing for every new collector. The test is
// simply whether a destination was recorded.
//
// A wrongly-typed artifact reads as absence rather than panicking: Artifacts
// round-trips through JSON in the store, so any value can arrive as any type.
func StatusChangeOf(evt Event) (StatusChange, bool) {
	to, ok := evt.Artifacts[ArtifactStatusTo].(string)
	if !ok || to == "" {
		return StatusChange{}, false
	}

	issueKey, _ := evt.Artifacts[ArtifactIssueKey].(string)
	from, _ := evt.Artifacts[ArtifactStatusFrom].(string)

	return StatusChange{IssueKey: issueKey, From: from, To: to}, true
}
