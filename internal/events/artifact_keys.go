package events

// Cross-package artifact keys that need no accessor beyond the constant
// itself: every reader today does a single type assertion
// (evt.Artifacts[ArtifactX].(T)) and tolerates absence or the wrong type by
// falling back to the zero value, the same defensive shape StatusChangeOf and
// IsTrackerRecord use for their own artifact reads. Grouped in one file
// because none of them, alone, is a state machine worth its own file the way
// a status change or a tracker-record marker is — they are declared together
// only for that reason, not because they are one concept.
const (
	// ArtifactConnection is the configured Jira connection name that resolved
	// an event's issue key, when the event came from a jira-source collector.
	// Read by the correlator when gathering candidates (a candidate must be
	// verified against the same connection that produced it) and recorded on
	// every stored link so the reconciler knows which site to write back to.
	ArtifactConnection = "connection"

	// ArtifactAuthoredByUnjira reports whether the account that produced a
	// jira-source event was unjira's own configured account. The reconciler
	// drops self-authored events before drafting — without it, a comment
	// unjira itself posted would be collected back and proposed again,
	// closing a loop that never terminates.
	ArtifactAuthoredByUnjira = "authored_by_unjira"

	// ArtifactGitBranch is the git branch a claude_code session transcript
	// recorded, when one was present. The correlator re-derives ticket keys
	// from it independently of ArtifactTicketKeys (see that constant's own
	// comment for why re-deriving rather than trusting a flattened list
	// matters here): a branch name is an explicit human act of naming the
	// ticket for this work, which is the strongest attribution signal
	// available today.
	ArtifactGitBranch = "git_branch"

	// ArtifactTicketKeys is every candidate ticket key a claude_code session
	// mentioned, in first-mention order — from message text and, appended
	// after, the branch name. Use SetTicketKeys/TicketKeysOf rather than a
	// bare type assertion: the artifact's on-the-wire type is always []any
	// (json.Unmarshal into map[string]any produces that for a JSON array, and
	// Artifacts round-trips through the store as JSON), never []string, so a
	// caller writing or reading with the wrong shape silently loses every key
	// on the first store round trip rather than failing loudly.
	ArtifactTicketKeys = "ticket_keys"
)

// SetTicketKeys records keys on evt as the ArtifactTicketKeys artifact, for a
// collector emitting a session's mentioned tickets.
//
// Stores as []any rather than []string: that is what a JSON round trip
// through the store always produces on read (json.Unmarshal has no way to
// know the element type of a map[string]any value), so writing []string here
// would only work until the first store round trip, at which point every
// reader doing the same []any assertion this function performs would see
// nothing.
func SetTicketKeys(evt *Event, keys []string) {
	out := make([]any, len(keys))
	for i, k := range keys {
		out[i] = k
	}

	evt.Artifacts[ArtifactTicketKeys] = out
}

// TicketKeysOf reads evt's ArtifactTicketKeys artifact as a []string,
// tolerating absence, the wrong container type, and malformed elements by
// degrading to fewer keys rather than erroring — a corrupted or
// hand-seeded artifact here should read as "fewer candidates", not fail the
// caller gathering them (mirroring the tolerance StatusChangeOf and
// IsTrackerRecord already document for a wrongly-typed artifact: absence, not
// a panic).
func TicketKeysOf(evt Event) []string {
	raw, ok := evt.Artifacts[ArtifactTicketKeys].([]any)
	if !ok {
		return nil
	}

	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok && s != "" {
			out = append(out, s)
		}
	}

	return out
}
