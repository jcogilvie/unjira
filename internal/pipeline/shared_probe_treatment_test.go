package pipeline

// shared_probe_treatment_test.go holds the acceptance reports only the treatment arm
// can produce: the dispute record (Stats fields this slice added) and M6a, which
// exercises the member-only readers. The baseline arm replaces reportTreatment with a
// no-op stub; see shared_probe_test.go.

import (
	"fmt"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/store"
)

// reportTreatment prints, per pass, the dispute record M4 asks for, then M6a.
func reportTreatment(t *testing.T, s *store.Store, results []NarrateResult, links []probeLink) {
	t.Helper()

	for i, r := range results {
		fmt.Printf("pass %d: M4 %d disputed, %d context link(s) written, %d member placement(s) below the floor\n",
			i+1, r.Stats.DisputedEvents, r.Stats.ContextLinks, r.Stats.MembersBelowFloor)
		for _, d := range r.Stats.Disputes {
			fmt.Printf("      dispute %s: %s chosen from %v at %.2f — %s\n", d.Event, d.Chosen, d.Claimants, d.Confidence, d.Rationale)
		}
	}

	printDeltaExclusivity(t, s, links)
}

// printDeltaExclusivity is M6a, deterministic and model-free: for every event with two
// or more links, how many narratives' reconcile delta, create input and redraft delta
// contain it. Exactly one for each, or context is reaching the reconciler. Unlike the
// other metrics it DOES call the store's readers, because they are what it measures.
func printDeltaExclusivity(t *testing.T, s *store.Store, links []probeLink) {
	t.Helper()

	holders := map[int64][]int64{}
	key := map[int64]string{}
	for _, l := range links {
		holders[l.eventID] = append(holders[l.eventID], l.narrative)
		key[l.eventID] = l.source + "/" + l.externalID
	}

	contains := func(evts []correlator.Event, k string) bool {
		for _, e := range evts {
			if e.Source+"/"+e.ExternalID == k {
				return true
			}
		}

		return false
	}

	checked, bad := 0, []string{}
	for eid, ns := range holders {
		if len(ns) < 2 {
			continue
		}
		checked++
		var inDelta, inAll, inRedraft int
		for _, n := range ns {
			delta, err := s.DeltaEvents(n)
			require.NoError(t, err)
			all, err := s.AllMemberEvents(n)
			require.NoError(t, err)
			redraft, err := s.EligibleMemberEvents(n)
			require.NoError(t, err)
			if contains(delta, key[eid]) {
				inDelta++
			}
			if contains(all, key[eid]) {
				inAll++
			}
			if contains(redraft, key[eid]) {
				inRedraft++
			}
		}
		// Delta and redraft may legitimately be 0 (already drafted, or frozen); more
		// than one is the failure. AllMemberEvents must be exactly one: its home.
		if inDelta > 1 || inRedraft > 1 || inAll != 1 {
			bad = append(bad, fmt.Sprintf("%s: delta %d, all-member %d, redraft %d across %v",
				key[eid], inDelta, inAll, inRedraft, ns))
		}
	}
	sort.Strings(bad)

	fmt.Printf("M6a delta exclusivity: %d multiply-linked event(s) checked, %d violation(s)\n", checked, len(bad))
	for _, b := range bad {
		fmt.Printf("      VIOLATION %s\n", b)
	}
}
