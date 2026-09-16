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

	// ArtifactSCMKeys is every ticket key a claude_code session named while
	// AUTHORING in source control — a commit message, a branch creation, a PR
	// title — in first-seen order.
	//
	// Separate from ArtifactTicketKeys rather than folded into it, for the same
	// reason that constant's comment gives for the correlator re-deriving branch
	// keys: flattening loses the distinction that matters most for attribution.
	// A prose mention is someone talking about a ticket; a commit message is
	// someone naming the ticket for this work, which is the same explicit human
	// act that makes a branch name the strongest inferred signal.
	//
	// Written by the claudecode collector for finding F20: messageText reads only
	// text blocks, so every key named in a tool call was discarded. Measured over
	// 28 transcripts, 10 of 20 sessions carrying any key had one only here.
	//
	// Deliberately excludes keys from READING commands (`git log --grep=`, `gh pr
	// view`): 8 of the keys measured appeared only in those, where an agent is
	// investigating a ticket it may have nothing to do with.
	//
	// Same []any storage contract as ArtifactTicketKeys — use SetSCMKeys/SCMKeysOf.
	ArtifactSCMKeys = "scm_keys"
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

// SetSCMKeys records keys on evt as the ArtifactSCMKeys artifact, for a collector
// emitting the tickets a session named while authoring in source control.
//
// Same []any storage contract as SetTicketKeys, and for the same reason: a JSON
// round trip through the store produces []any on read, so writing []string would
// work until the first round trip and then silently read as nothing.
func SetSCMKeys(evt *Event, keys []string) {
	out := make([]any, len(keys))
	for i, k := range keys {
		out[i] = k
	}

	evt.Artifacts[ArtifactSCMKeys] = out
}

// SCMKeysOf reads evt's ArtifactSCMKeys artifact as a []string, with the same
// tolerance TicketKeysOf documents: absence, a wrong container type, or malformed
// elements degrade to fewer keys rather than erroring.
func SCMKeysOf(evt Event) []string {
	raw, ok := evt.Artifacts[ArtifactSCMKeys].([]any)
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
