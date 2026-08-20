package pipeline

import (
	"fmt"
	"strings"
	"time"

	"github.com/jcogilvie/unjira/internal/correlator"
)

// RenderNarrateResult formats one pass for a human deciding whether the
// clustering is any good. A pure function of its input, so it can be tested
// without a store or an LLM.
//
// Member events are included per narrative deliberately: a title and a count
// cannot show whether a grouping is defensible, and judging that is the whole
// point of the command.
func RenderNarrateResult(r NarrateResult) string {
	var b strings.Builder

	writeNarrateHeader(&b, r)

	if len(r.Narratives) == 0 {
		b.WriteString("\nno narratives produced\n")

		return b.String()
	}

	for _, n := range r.Narratives {
		writeNarratedNarrative(&b, n)
	}

	return b.String()
}

// writeNarrateHeader writes the window, candidate counts, LLM call/token
// stats, any compactions, and (under a dry run) the disclaimer that nothing
// was persisted — everything that precedes the per-narrative detail.
func writeNarrateHeader(b *strings.Builder, r NarrateResult) {
	b.WriteString("== narration pass ==\n")
	fmt.Fprintf(b, "window   %s .. %s\n",
		r.Window.Start.Format(time.RFC3339), r.Window.End.Format(time.RFC3339))
	fmt.Fprintf(b, "events   %d unlinked candidate(s), %d narrative(s) as context\n",
		r.UnlinkedEvents, r.ContextNarratives)
	fmt.Fprintf(b, "llm      %d call(s), %d split(s), %d merge check(s)\n",
		r.Stats.Calls, r.Stats.Splits, r.Stats.MergeChecks)
	fmt.Fprintf(b, "tokens   %d prompt + %d completion (estimated %d)\n",
		r.Stats.PromptTokens, r.Stats.CompletionTokens, r.Stats.EstimatedTokens)

	for _, c := range r.Compactions {
		fmt.Fprintf(b, "compact  narrative %d: folded %d event(s) up to %s\n",
			c.NarrativeID, c.EventsFolded, c.Boundary.Format(time.RFC3339))
	}

	if r.DryRun {
		b.WriteString("dry run: nothing persisted\n")
	}
}

// writeNarratedNarrative writes one narrative's header line (kind, id,
// window, and — for an extend — what window_end moved from), its title and
// summary, and its member events.
func writeNarratedNarrative(b *strings.Builder, n NarratedNarrative) {
	b.WriteString("\n")
	fmt.Fprintf(b, "[%s #%s] %s .. %s", narrativeKindLabel(n.Kind), narrativeIDLabel(n.ID),
		n.WindowStart.Format(time.RFC3339), n.WindowEnd.Format(time.RFC3339))
	if n.Kind == correlator.ClusterExtends && !n.PriorWindowEnd.IsZero() {
		fmt.Fprintf(b, "  (window_end was %s)", n.PriorWindowEnd.Format(time.RFC3339))
	}
	b.WriteString("\n")
	fmt.Fprintf(b, "  %q\n", n.Title)
	fmt.Fprintf(b, "  %s\n", n.Summary)

	if len(n.Events) == 0 {
		return
	}
	fmt.Fprintf(b, "  events (%d):\n", len(n.Events))
	for _, e := range n.Events {
		fmt.Fprintf(b, "    - [%s] %s  %s\n", e.Source, e.Summary, e.OccurredAt.Format(time.RFC3339))
	}
}

// narrativeKindLabel renders a cluster kind for the output's leading tag.
func narrativeKindLabel(k correlator.ClusterKind) string {
	if k == correlator.ClusterExtends {
		return "EXTENDS"
	}

	return "NEW"
}

// narrativeIDLabel renders a narrative id, or "-" when there isn't one — a
// dry run persists nothing, and printing "#0" would look like a real row.
func narrativeIDLabel(id int64) string {
	if id == 0 {
		return "-"
	}

	return fmt.Sprintf("%d", id)
}
