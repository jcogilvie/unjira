package reconciler

import (
	"context"

	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/events"
	"github.com/jcogilvie/unjira/internal/llm"
	"github.com/jcogilvie/unjira/internal/store"
)

// draft is stubbed in this task; Task 6 implements the LLM call. Returning no
// proposals keeps the deterministic pass testable in isolation.
func draft(
	_ context.Context,
	_ llm.Client,
	_ store.NarrativeRow,
	_ []events.Event,
	_ []verifiedLink,
) ([]ProposedAction, correlator.Stats, error) {
	return nil, correlator.Stats{}, nil
}
