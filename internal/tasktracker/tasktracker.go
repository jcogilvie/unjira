// Package tasktracker defines the backend-agnostic interface phase-1's
// correlator/reconciler/applier use to read and mutate tracked work, plus
// the normalized types every backend (Jira, GitHub Issues, a local
// no-op-tracker) speaks. It has no imports of internal/clients or
// internal/store — like internal/events, it's a shared contract with
// multiple producers and no single owning consumer.
package tasktracker

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
}

// TaskTracker is the minimal surface the correlator/reconciler/applier need
// against any backend: verify a proposed issue-link resolves, read current
// state before proposing a state-bearing action, and execute the
// comment/transition/create actions.type values. There is no method for
// actions.type=estimate — estimates are unjira's own derived data
// (internal/store), never a native field on any backend by default.
type TaskTracker interface {
	// GetIssue resolves key to its current normalized state.
	GetIssue(key string) (Issue, error)

	// SearchIssues returns issues matching a backend-native query — JQL for
	// Jira, a GitHub search qualifier string for GitHub Issues, a simple
	// substring match for the local backend. Not a portable query language:
	// callers must treat this as backend-flavored.
	SearchIssues(query string, limit int) ([]Issue, error)

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
