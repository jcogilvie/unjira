// Package correlator clusters events into narratives — one logical unit of
// work, whatever raw events it took to produce it. Cluster is pure compute:
// it never touches the store (see internal/store's Narrative/ClusterResult
// mirror-shape note below); Persist (a later slice) is what writes results
// to real narratives/narrative_events rows.
//
// See docs/superpowers/specs/2026-08-11-phase1-correlator-design.md for the
// full phase-1 vertical this package is one component of, and
// docs/superpowers/specs/2026-08-12-correlator-cluster-design.md for this
// package's own design (prompt/response contract, overflow handling,
// rationale for every non-obvious choice below).
package correlator

import (
	"context"
	"time"

	"github.com/jcogilvie/unjira/internal/events"
)

// TimeRange is a half-open [Start, End) window over events.Event.OccurredAt.
type TimeRange struct {
	Start, End time.Time
}

// Event is an alias for events.Event — Cluster operates on the same
// normalized event shape every collector emits.
type Event = events.Event

// Narrative mirrors internal/store's `narratives` table row shape so a
// later slice's Persist doesn't have to reshape this type when it starts
// writing real rows — Cluster only reads WindowStart/WindowEnd (for
// overlap/adjacency) and ID/Title/Summary (for prompt context), but the
// full shape is defined now so nothing here changes when persistence
// lands.
type Narrative struct {
	ID          int64
	WindowStart time.Time
	WindowEnd   time.Time
	Title       string
	Summary     string
	IssueKey    string
	Confidence  float64
	Status      string
}

// ClusterKind distinguishes a brand-new narrative from one extending an
// existing row.
type ClusterKind int

const (
	ClusterNew ClusterKind = iota
	ClusterExtends
)

// ClusterResult is one narrative-shaped grouping of events, held in memory
// only. NarrativeID is set only when Kind == ClusterExtends.
type ClusterResult struct {
	Kind        ClusterKind
	NarrativeID int64
	Title       string
	Summary     string
	Events      []Event
}

// llmClient is the narrow capability Cluster needs from an LLM backend — a
// consumer-owned interface (same pattern as internal/workflow's
// projectMiner) so tests use a fake with no real HTTP server.
// *openai.Client (internal/clients/openai) already satisfies this with
// zero adapter code; this package deliberately never imports
// internal/clients/openai to keep that decoupling real, not incidental.
type llmClient interface {
	Complete(ctx context.Context, systemPrompt, userPrompt string) (string, error)
}

// Cluster groups evts (filtered to window) plus any Narrative in existing
// whose window overlaps or is adjacent to window, into narratives — each
// tagged new or extending an existing row. Pure compute: no store access.
func Cluster(
	_ context.Context,
	_ []Event,
	_ []Narrative,
	_ llmClient,
	_ TimeRange,
	_ int,
) ([]ClusterResult, error) {
	return nil, nil
}
