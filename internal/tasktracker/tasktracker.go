// Package tasktracker defines the backend-agnostic interface phase-1's
// correlator/reconciler/applier use to read and mutate tracked work, plus
// the normalized types every backend (Jira, GitHub Issues, a local
// no-op-tracker) speaks. It has no imports of internal/clients or
// internal/store — like internal/events, it's a shared contract with
// multiple producers and no single owning consumer.
package tasktracker

import (
	"fmt"
	"regexp"
)

// StatusCategory is a normalized status bucket every backend maps into.
// Deliberately coarse: GitHub Issues has no named-status concept at all,
// only open/closed, so a category-based target is the common denominator
// across backends.
type StatusCategory string

// The three normalized status categories every backend maps into.
const (
	StatusTodo       StatusCategory = "todo"
	StatusInProgress StatusCategory = "in_progress"
	StatusDone       StatusCategory = "done"
)

// Issue is a backend-normalized view of one tracked work item.
type Issue struct {
	Key            string
	Summary        string
	StatusCategory StatusCategory
	StatusName     string // native display value; "" if the backend has none
	Labels         []string

	// Description is the issue body. Populated on the live read path only —
	// the Jira collector deliberately does not emit description snapshots as
	// events, so this is the only place a never-edited ticket's body is
	// available. Narrative→issue matching compares against it, since a
	// one-line summary frequently cannot distinguish the ticket a narrative
	// implements from one it merely mentions.
	//
	// Empty when the backend has none, or when Jira returns an Atlassian
	// Document Format object rather than a string (v3 API): rendering ADF to
	// text is deliberately out of scope, and an empty description degrades
	// matching rather than breaking it.
	Description string
}

// TaskReader is the read-only surface: everything needed to verify a proposed
// issue-link resolves and to establish current state before proposing a
// state-bearing action. Nothing here mutates the backend.
//
// Kept separate from TaskWriter so code that must not write can say so in its
// signature. The reconciler takes a TaskReader and is therefore structurally
// incapable of applying an action — the invariant docs/design-notes.md and
// rules/intent-not-outcome.md both turn on: authority to write belongs to the
// review queue, never to the code that drafts proposals.
type TaskReader interface {
	// GetIssue resolves key to its current normalized state.
	GetIssue(key string) (Issue, error)

	// SearchIssues returns issues matching a backend-native query — JQL for
	// Jira, a GitHub search qualifier string for GitHub Issues, a simple
	// substring match for the local backend. Not a portable query language:
	// callers must treat this as backend-flavored.
	SearchIssues(query string, limit int) ([]Issue, error)

	// AvailableStatusCategories reports which normalized status categories the
	// issue can legally move to right now, per the live backend.
	//
	// Normalized categories rather than backend-native transition identifiers,
	// for the same reason SetStatus takes a category: Jira's named,
	// admin-configurable transitions have no counterpart in GitHub Issues'
	// open/closed model, and callers only need to know whether a target is
	// reachable.
	//
	// This must be a live per-issue read, not a lookup against a mined
	// workflow.Graph. The graph is statistically observed changelog history, so
	// HasEdge answers "has this ever been seen" — a proxy for legality, not
	// ground truth (see internal/workflow's package doc on its three tiers).
	// Proposing a transition the backend will refuse is the failure this
	// prevents.
	//
	// A read that describes a write: it reports what a write *could* do
	// without performing one, which is why it sits here and not beside
	// SetStatus.
	AvailableStatusCategories(key string) ([]StatusCategory, error)
}

// TaskWriter is the mutating surface — the executable counterparts of the
// comment/transition/create actions.type values.
//
// There is no method for actions.type=estimate: estimates are unjira's own
// derived data (internal/store), never a native field on any backend by
// default.
//
// Holding one of these is authority to change the org's view of reality, so
// take it only in code that runs after the review gate. Drafting code takes a
// TaskReader instead.
type TaskWriter interface {
	// AddComment posts a comment, gated upstream by the narrative-worthiness
	// test.
	AddComment(key, text string) error

	// SetStatus moves an issue toward a normalized target category —
	// deliberately coarser than Jira's named-transition model, since
	// GitHub Issues only has open/closed.
	SetStatus(key string, target StatusCategory) error

	// CreateIssue creates an issue and returns its key.
	CreateIssue(projectOrRepo, summary, issueType, description string, labels []string) (string, error)
}

// TaskTracker is the full backend surface, for the applier and for wiring that
// must hand out both halves.
//
// Prefer the narrowest of TaskReader/TaskWriter a consumer actually needs.
// Every backend implements all of it, so the split constrains callers rather
// than implementations.
type TaskTracker interface {
	TaskReader
	TaskWriter
}

// issueKeyRE anchors the whole string to exactly one <PROJECT>-<NUMBER>
// segment: an uppercase-alphanumeric project prefix starting with a letter
// (matching how every backend that mints its own keys shapes them — see
// internal/store.InsertLocalIssue's `fmt.Sprintf("%s-%d", project, n)` and the
// same convention on a real Jira site), a literal hyphen, and a numeric
// suffix. Anchored (not internal/events.TicketKeyRegexp's unanchored
// scan-for-candidates pattern) because this validates one already-known key
// rather than searching free text for candidates — an extra "-2" segment
// (e.g. a stray "PROJ-1-2") must be rejected, not silently truncated to its
// first match.
var issueKeyRE = regexp.MustCompile(`^([A-Z][A-Z0-9]*)-(\d+)$`)

// ProjectFromIssueKey extracts the project key from an issue key of the form
// <PROJECT>-<NUMBER> (e.g. "PAAS-4036" -> "PAAS").
//
// A backend-shape fact, not gate business logic — every backend unjira talks
// to (Jira, the local tracker) mints keys in this shape, so this lives here
// rather than in internal/gate. Nothing in the repo parsed this before write
// scope needed it (internal/correlator/refs.go's regex is for owner/repo#N PR
// references, a different syntax entirely).
//
// Errors, naming the offending key, on anything that doesn't match — per
// CLAUDE.md's "don't guess" convention. A malformed key here means a caller
// handed this an issue key that was never valid for any backend this package
// knows about; returning a truncated or empty guess would let a
// write-authorization check silently pass or fail on data that was already
// wrong.
func ProjectFromIssueKey(key string) (string, error) {
	m := issueKeyRE.FindStringSubmatch(key)
	if m == nil {
		return "", fmt.Errorf(
			"issue key %q does not match the <PROJECT>-<NUMBER> shape every tasktracker backend uses",
			key,
		)
	}

	return m[1], nil
}
