package triage

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/jcogilvie/unjira/internal/reconciler"
	"github.com/jcogilvie/unjira/internal/store"
)

// JoinWorklogs joins each proposed worklog in batch to the one narrative whose
// comment or transition it is reviewed with (D-18, D-36), and reports every
// worklog it could not join unambiguously. joins maps worklog action id to
// narrative id; a worklog absent from it is reviewed alone.
//
// A worklog is drafted per ticket, not per narrative, so its messages may span
// several narratives (R2). When more than one of them, or one narrative with
// several pending comments or transitions, could claim it, picking one would be
// a guess presented as a fact: the worklog is reviewed alone and the
// ambiguity is reported instead. Reports come out in batch order.
func JoinWorklogs(batch []store.ActionRow, msgs []store.MessageAttribution) (joins map[int64]int64, reports []string) {
	joins = map[int64]int64{}

	pending := map[int64]int{} // narrative id -> pending comment/transition count
	for _, row := range batch {
		if row.Type == string(reconciler.ActionComment) || row.Type == string(reconciler.ActionTransition) {
			pending[row.NarrativeID]++
		}
	}
	byMessage := make(map[string]store.MessageAttribution, len(msgs))
	for _, m := range msgs {
		byMessage[m.MessageID] = m
	}

	for _, row := range batch {
		if row.Type != store.ActionTypeWorklog {
			continue
		}
		var p store.WorklogPayload
		if err := json.Unmarshal([]byte(row.Payload), &p); err != nil {
			reports = append(reports, fmt.Sprintf("worklog action %d on %s: payload does not decode (%v): reviewed alone", row.ID, row.IssueKey, err))
			continue
		}
		var candidates []int64
		for _, id := range p.MessageIDs {
			m, ok := byMessage[id]
			if !ok || m.State != store.AttributionTracked || m.IssueKey != row.IssueKey {
				continue
			}
			if pending[m.NarrativeID] > 0 && !slices.Contains(candidates, m.NarrativeID) {
				candidates = append(candidates, m.NarrativeID)
			}
		}
		switch {
		case len(candidates) == 0:
		case len(candidates) == 1 && pending[candidates[0]] == 1:
			joins[row.ID] = candidates[0]
		case len(candidates) == 1:
			reports = append(reports, fmt.Sprintf("worklog action %d on %s: narrative %d has %d pending comment/transition actions: reviewed alone",
				row.ID, row.IssueKey, candidates[0], pending[candidates[0]]))
		default:
			slices.Sort(candidates)
			ids := make([]string, len(candidates))
			for i, n := range candidates {
				ids[i] = strconv.FormatInt(n, 10)
			}
			reports = append(reports, fmt.Sprintf("worklog action %d on %s covers work from narratives %s, each with a pending action: reviewed alone",
				row.ID, row.IssueKey, strings.Join(ids, ", ")))
		}
	}
	return joins, reports
}
