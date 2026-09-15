package pipeline

import (
	"fmt"
	"strings"
	"time"

	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/reconciler"
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

// RenderMatchResult formats one matching pass for a human judging whether
// the attributions are right.
//
// A narrative can end up with no links for three quite different reasons —
// every candidate excluded as a placeholder, every candidate rejected by
// live-tracker verification, or the events simply named no keys at all —
// and "no links" with no explanation is indistinguishable from a silent
// failure. writeUnmatchedNarrative exists to make those three cases
// visually distinct rather than collapsing them into the same blank output.
func RenderMatchResult(r MatchRunResult) string {
	var b strings.Builder

	writeMatchHeader(&b, r)

	if len(r.Matched) == 0 {
		b.WriteString("\nno narratives considered\n")
		writeRemainder(&b, r.Remaining, "unmatched")

		return b.String()
	}

	for _, m := range r.Matched {
		if len(m.Links) > 0 {
			writeMatchedNarrative(&b, m)
		} else {
			writeUnmatchedNarrative(&b, m)
		}
	}

	writeRemainder(&b, r.Remaining, "unmatched")

	return b.String()
}

// writeRemainder reports a backlog this pass did not drain, or writes nothing.
//
// SILENT AT ZERO, deliberately. "0 narratives remaining" on every ordinary pass
// would train an operator to skip the line — which is exactly how the existing
// stderr cap warning came to be ignored, so printing it unconditionally would
// reproduce the bug this exists to fix.
//
// Says what to DO, not just the count. A bare number is a fact; "re-run to
// continue" is the instruction, and the reason this is on stdout at all is that an
// operator reading a pass summary should not have to already know that per-pass
// caps exist.
//
// Written on BOTH paths of RenderMatchResult, including the no-narratives early
// return: a pass that considered nothing while a backlog waits is precisely the
// case worth reporting, and it is the one an early return would have hidden.
func writeRemainder(b *strings.Builder, remaining int, what string) {
	if remaining <= 0 {
		return
	}

	fmt.Fprintf(b, "\n%d narrative(s) still %s — re-run to continue draining "+
		"(per-pass cap; see max_narratives_per_pass)\n", remaining, what)
}

// writeMatchHeader writes the narrative count and LLM call/token stats that
// precede the per-narrative detail, mirroring writeNarrateHeader.
func writeMatchHeader(b *strings.Builder, r MatchRunResult) {
	b.WriteString("== matching pass ==\n")
	fmt.Fprintf(b, "narratives   %d considered\n", len(r.Matched))
	fmt.Fprintf(b, "llm          %d call(s)\n", r.Stats.Calls)
	fmt.Fprintf(b, "tokens       %d prompt + %d completion (estimated %d)\n",
		r.Stats.PromptTokens, r.Stats.CompletionTokens, r.Stats.EstimatedTokens)
}

// writeMatchedNarrative writes one narrative that has at least one surviving
// link: its id, the promoted primary (or an explicit marker when the
// winning candidate's confidence fell below the floor, so a below-floor
// match cannot be mistaken for nothing having been found), then one line per
// link showing role, key, provenance, and confidence — provenance is what
// lets a human tell a stale-branch false positive from a genuine mention.
// Any Excluded/Unresolved keys and the model's rationale, when present,
// follow.
func writeMatchedNarrative(b *strings.Builder, m correlator.MatchResult) {
	b.WriteString("\n")
	fmt.Fprintf(b, "[narrative #%d]", m.NarrativeID)
	if m.Primary != "" {
		fmt.Fprintf(b, " primary: %s\n", m.Primary)
	} else {
		b.WriteString(" primary: (no primary promoted — below confidence floor)\n")
	}

	for _, link := range m.Links {
		fmt.Fprintf(b, "  - %s  key=%s  provenance=%s  confidence=%.2f\n",
			link.Role, link.IssueKey, link.Provenance, link.Confidence)
	}

	writeExcludedAndUnresolved(b, m)

	if m.Rationale != "" {
		fmt.Fprintf(b, "  rationale: %s\n", m.Rationale)
	}
}

// writeUnmatchedNarrative writes one narrative with no surviving links,
// stating which of the three reasons applies: excluded placeholders,
// tracker-rejected keys, or no candidate keys at all in its events. This is
// the branch that keeps "no links" from reading as a silent failure.
func writeUnmatchedNarrative(b *strings.Builder, m correlator.MatchResult) {
	b.WriteString("\n")
	fmt.Fprintf(b, "[narrative #%d] unmatched", m.NarrativeID)

	switch {
	case len(m.Excluded) > 0:
		fmt.Fprintf(b, " — every candidate excluded: %s\n", strings.Join(m.Excluded, ", "))
	case len(m.Unresolved) > 0:
		fmt.Fprintf(b, " — every candidate unresolved against the tracker: %s\n",
			strings.Join(m.Unresolved, ", "))
	default:
		b.WriteString(" — no candidate keys found in its events\n")
	}
}

// writeExcludedAndUnresolved writes m's Excluded/Unresolved keys when a
// matched narrative (one with a surviving link) also had some candidates
// dropped along the way — e.g. a placeholder alongside a real key.
func writeExcludedAndUnresolved(b *strings.Builder, m correlator.MatchResult) {
	if len(m.Excluded) > 0 {
		fmt.Fprintf(b, "  excluded: %s\n", strings.Join(m.Excluded, ", "))
	}
	if len(m.Unresolved) > 0 {
		fmt.Fprintf(b, "  unresolved: %s\n", strings.Join(m.Unresolved, ", "))
	}
}

// RenderReconcileResult formats a reconcile pass for a terminal.
//
// Every narrative appears, including ones that produced nothing: an unchanged
// narrative, an unresolvable link, and a suppressed duplicate are all
// legitimate outcomes, and omitting them would make "nothing proposed"
// indistinguishable from "nothing considered".
func RenderReconcileResult(r ReconcileRunResult) string {
	var b strings.Builder

	b.WriteString("\nreconcile\n")
	if r.DryRun {
		b.WriteString("  (dry run — nothing persisted)\n")
	}
	fmt.Fprintf(&b, "  llm calls: %d  tokens: %d prompt / %d completion\n",
		r.Stats.Calls, r.Stats.PromptTokens, r.Stats.CompletionTokens)

	if len(r.Results) == 0 {
		b.WriteString("\nno linked narratives to reconcile\n")
		writeRemainder(&b, r.Remaining, "carrying unexamined work")
		writeDeferredCreates(&b, r.CreatesDeferred)

		return b.String()
	}

	for _, result := range r.Results {
		writeReconciledNarrative(&b, result)
	}

	writeRemainder(&b, r.Remaining, "carrying unexamined work")
	writeDeferredCreates(&b, r.CreatesDeferred)

	return b.String()
}

// writeDeferredCreates reports a skipped create path, and says nothing when creates
// ran — silent at zero for the same reason writeRemainder is: a line on every
// ordinary pass trains an operator to skip it.
func writeDeferredCreates(b *strings.Builder, deferred int) {
	if deferred <= 0 {
		return
	}

	fmt.Fprintf(b, "\ncreate proposals deferred: %d narrative(s) are still unmatched, so "+
		"\"no linked issue\" does not yet mean \"untracked\" — re-run to let matching catch up\n",
		deferred)
}

// writeReconciledNarrative writes one narrative's header line and every
// outcome it carries — proposed actions, an empty delta, unverified links,
// suppressed duplicates, low-confidence flags, and unguarded transitions —
// since a narrative can legitimately carry any combination of these at once.
func writeReconciledNarrative(b *strings.Builder, result reconciler.ReconcileResult) {
	fmt.Fprintf(b, "\nnarrative %d\n", result.NarrativeID)

	for _, a := range result.Proposed {
		writeProposedAction(b, a)
	}

	if result.SkippedNoDelta {
		b.WriteString("  no new events since the last proposal\n")
	}

	for _, key := range result.Unverified {
		fmt.Fprintf(b, "  unverified: %s (not found on the tracker)\n", key)
	}

	for _, reason := range result.Suppressed {
		fmt.Fprintf(b, "  suppressed: %s\n", reason)
	}

	for _, note := range result.LowConfidence {
		fmt.Fprintf(b, "  low confidence: %s\n", note)
	}

	for _, u := range result.Unguarded {
		fmt.Fprintf(b, "  unguarded: %s\n", u.Reason())
	}
}

// writeProposedAction writes one drafted action's type, target issue,
// confidence, and (when set) its comment body or transition target.
func writeProposedAction(b *strings.Builder, a reconciler.ProposedAction) {
	fmt.Fprintf(b, "  propose %s", a.Type)
	if a.IssueKey != "" {
		fmt.Fprintf(b, " on %s", a.IssueKey)
	}
	fmt.Fprintf(b, " (confidence %.2g)\n", a.Confidence)
	if a.Body != "" {
		fmt.Fprintf(b, "    %s\n", firstLine(a.Body))
	}
	if a.TargetStatus != "" {
		fmt.Fprintf(b, "    -> %s\n", a.TargetStatus)
	}
}

// firstLine returns s up to its first newline, so a multi-paragraph comment
// body renders as one scannable line.
func firstLine(s string) string {
	if before, _, found := strings.Cut(s, "\n"); found {
		return before + " …"
	}

	return s
}
