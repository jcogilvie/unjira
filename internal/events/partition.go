package events

// PartitionByTrackerRecord splits evts into work evidence and tracker records,
// preserving each half's input order.
//
// This is the entrance-side counterpart to AnyWorkEvidence. unjira diffs reality
// (what you did) against the tracker (what the org thinks you did), and those are
// two sides of one comparison — but they arrive as one Event type through one door,
// so nothing downstream could tell them apart without asking. Three separate exit
// filters grew to compensate: suppressTrackerEcho, AnyWorkEvidence, and
// dropSelfAuthored.
//
// This does NOT make those redundant, and they must not be removed. Two reasons:
//
//   - Narratives linked BEFORE this filter existed still hold tracker records (96 in
//     the dev store), and narrative_events rows are never deleted. Their deltas carry
//     tracker records for as long as those narratives live.
//   - The exclusion is opt-in by the producing collector. A collector that forgets to
//     mark its records — the failure IsTrackerRecord deliberately tolerates — feeds
//     them straight through, and the exit filters are what catch that.
//
// So this is defense in depth: it stops the pipeline PAYING to narrate bookkeeping,
// while the exit filters stop it SAYING anything derived from bookkeeping. Different
// failures, both real.
//
// Measured on the dev store before this existed: 96 Jira events carried 210,350
// characters into the clustering prompt against the session events' 45,550 — 82% of
// the payload — and 29 of 68 narratives contained nothing but tracker bookkeeping. 26
// of those held exactly one issue key and linked to the very ticket they were derived
// from. All 26 were correctly refused downstream, so nothing wrong was ever written;
// the entire cluster-name-persist-rehydrate-propose-suppress cycle was simply spent
// to arrive at "no".
//
// BOTH halves are returned rather than the work half alone. Two reasons, and the
// second is the load-bearing one:
//
//   - The tracker half is a real input to other decisions (a transition into a
//     working state is a human declaring intent to work), so a caller may want it.
//   - The COUNT is needed. Excluding events without reporting how many reads as
//     "nothing was left out", which is exactly the silent-data-loss failure this
//     codebase prefers to error over.
//
// A pure function, not a store query: the correlator invariant says pre-filters are
// pure functions of their input, and a SQL `WHERE json_extract(...) IS NULL` would
// hide the exclusion from anyone reading the pipeline. It also means the split costs
// a map lookup per event per pass — which is why excluded events being re-offered
// every pass is fine and needs no watermark. That is NOT design-notes #29's livelock:
// that failure re-examined narratives at full MODEL cost, where nothing here reaches
// the model at all.
//
// Classification comes from IsTrackerRecord, so a wrongly-typed or absent marker
// reads as work evidence — see its doc comment for why that is the right direction to
// fail.
func PartitionByTrackerRecord(evts []Event) (work, trackerRecords []Event) {
	for _, e := range evts {
		if IsTrackerRecord(e) {
			trackerRecords = append(trackerRecords, e)

			continue
		}
		work = append(work, e)
	}

	return work, trackerRecords
}
