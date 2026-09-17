package correlator

// summarycap.go bounds how much event text enters a clustering prompt — F16's input
// half.
//
// Attribution, by zeroing each payload site in turn (see design-notes #32 for why that
// came fifth instead of first): 93.7% of a 140k-token prompt was the events hydrated
// under context narratives, not the window's own events and not the narrative
// summaries. The distribution is extreme — 15 events (4.6%) held 52% of the characters,
// the largest a 15,037-char Jira description whose first ~150 chars carry the issue key,
// title and subject.
//
// Measured effect, per window width:
//
//	since | uncapped |  cap5000  cap2000  cap1000   cap500
//	  30d |  139,059 |  110,845   77,002   63,651   55,195
//	  90d |  209,440!|  180,077  143,392  128,541  119,331
//	 365d |  235,512!|  206,150! 169,465  154,614  145,403   (! = over budget, bisects)
//
// So cap2000 fits a full year in one call, where 90 days previously bisected — and
// bisecting measured 1.37x the single call's tokens, because both halves re-hydrate the
// same context.
//
// NOT the only half of F16. A capped prompt still yields ~104 clusters, each emitting a
// title and summary, so llm.max_output_tokens is exhausted before the prompt budget is.
// See the finding.

import "sort"

// truncationMarker ends a truncated summary, so a reader can tell a capped string from
// one that happened to be short. Exported to the test via export_test.go rather than
// duplicated as a literal there.
const truncationMarker = "…"

// TruncationReport is what a capped pass left out.
//
// Reported rather than silent because a cap that says nothing reads as "nothing was
// dropped" — the same defect F25 was about, one layer down — and because an operator
// cannot choose a better value without knowing what the current one is cutting.
// OriginalLengths carries the REAL lengths for exactly that reason: a count alone says
// something was truncated but not whether raising the cap by 100 or by 10,000 recovers
// it.
type TruncationReport struct {
	// Truncated counts EVENTS, not render sites. An event appears in both Events and
	// EligibleEvents, so counting sites would double every number.
	Truncated int
	// OriginalLengths are the pre-truncation character counts, ascending, so the
	// largest — the one that decides the next cap — reads last.
	OriginalLengths []int
	// LongestOriginal is the maximum, lifted out because it is the single number an
	// operator raising the cap actually needs.
	LongestOriginal int
}

// capEventSummaries returns narratives whose event summaries are truncated to maxChars,
// plus a report of what was cut. maxChars <= 0 means unlimited and returns the input
// unchanged.
//
// BOTH RENDER SITES, and that is the whole trick. buildClusterPrompt renders
// EligibleEvents as numbered candidates and Events as context detail; an earlier attempt
// that truncated only Events measured EXACTLY 0.0% of baseline, because with nothing
// applied every event sits in EligibleEvents and Events is empty. That signature —
// precisely 100.0% — is what design-notes #32 records as "suspect the instrument before
// the hypothesis".
//
// Copies rather than mutating in place. Cluster's caller holds the same slice the store
// hydrated, and truncating it there would mean a second pass over the same narratives
// re-truncates an already-truncated string, compounding silently.
//
// Deliberately does NOT drop events or narratives. Dropping a narrative from `existing`
// also removes its events from assignableEvents' index space, so reshuffling would
// silently lose reach — the "never silently drop data" invariant in CLAUDE.md. A
// truncated summary is still a summary; an absent event is a lost one.
func capEventSummaries(in []Narrative, maxChars int) ([]Narrative, TruncationReport) {
	var report TruncationReport
	if maxChars <= 0 || len(in) == 0 {
		return in, report
	}

	// Keyed by (source, external_id) so an event truncated at both render sites counts
	// once. The alternative — counting truncations — would report every event twice.
	seen := make(map[string]int)

	cut := func(evts []Event) []Event {
		if evts == nil {
			return nil
		}

		out := make([]Event, 0, len(evts))
		for _, e := range evts {
			if len(e.Summary) > maxChars {
				seen[e.Source+"\x00"+e.ExternalID] = len(e.Summary)
				e.Summary = e.Summary[:maxChars] + truncationMarker
			}
			out = append(out, e)
		}

		return out
	}

	out := make([]Narrative, 0, len(in))
	for _, n := range in {
		n.Events = cut(n.Events)
		n.EligibleEvents = cut(n.EligibleEvents)
		out = append(out, n)
	}

	report.Truncated = len(seen)
	for _, length := range seen {
		report.OriginalLengths = append(report.OriginalLengths, length)
		if length > report.LongestOriginal {
			report.LongestOriginal = length
		}
	}
	sort.Ints(report.OriginalLengths)

	return out, report
}
