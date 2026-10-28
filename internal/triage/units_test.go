package triage_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/triage"
)

func worklogRow(t *testing.T, id int64, issue, op string, msgIDs ...string) store.ActionRow {
	t.Helper()
	p, err := json.Marshal(store.WorklogPayload{Op: op, MessageIDs: msgIDs})
	require.NoError(t, err)
	return store.ActionRow{ID: id, Type: store.ActionTypeWorklog, IssueKey: issue, Payload: string(p)}
}

func actionRow(id, narrative int64, typ, issue string) store.ActionRow {
	return store.ActionRow{ID: id, NarrativeID: narrative, Type: typ, IssueKey: issue}
}

func attr(msgID string, narrative int64, issue string, state store.AttributionState) store.MessageAttribution {
	return store.MessageAttribution{
		MessageUsage: store.MessageUsage{MessageID: msgID},
		NarrativeID:  narrative, IssueKey: issue, State: state,
	}
}

func TestJoinWorklogs(t *testing.T) {
	// The undecodable-payload report quotes encoding/json's own error, whose
	// text is not a compatibility promise, so the expectation decodes it too.
	var v store.WorklogPayload
	decErr := json.Unmarshal([]byte("{"), &v)
	require.Error(t, decErr)

	tracked := store.AttributionTracked
	tests := []struct {
		name        string
		batch       []store.ActionRow
		msgs        []store.MessageAttribution
		wantJoins   map[int64]int64
		wantReports []string
	}{
		{
			name:      "joins one worklog to the narrative of one pending comment",
			batch:     []store.ActionRow{actionRow(1, 3, "comment", "PROJ-1"), worklogRow(t, 2, "PROJ-1", store.WorklogOpAdd, "m1")},
			msgs:      []store.MessageAttribution{attr("m1", 3, "PROJ-1", tracked)},
			wantJoins: map[int64]int64{2: 3},
		},
		{
			name:      "joins to a transition",
			batch:     []store.ActionRow{actionRow(1, 3, "transition", "PROJ-1"), worklogRow(t, 2, "PROJ-1", store.WorklogOpAdd, "m1")},
			msgs:      []store.MessageAttribution{attr("m1", 3, "PROJ-1", tracked)},
			wantJoins: map[int64]int64{2: 3},
		},
		{
			name:      "message tracked to another issue: no join, no report",
			batch:     []store.ActionRow{actionRow(1, 3, "comment", "PROJ-2"), worklogRow(t, 2, "PROJ-1", store.WorklogOpAdd, "m1")},
			msgs:      []store.MessageAttribution{attr("m1", 3, "PROJ-2", tracked)},
			wantJoins: map[int64]int64{},
		},
		{
			name:  "untracked or unattributed message: no join",
			batch: []store.ActionRow{actionRow(1, 3, "comment", "PROJ-1"), worklogRow(t, 2, "PROJ-1", store.WorklogOpAdd, "m1", "m2")},
			msgs: []store.MessageAttribution{
				attr("m1", 3, "PROJ-1", store.AttributionUntracked),
				attr("m2", 3, "PROJ-1", store.AttributionUnattributed),
			},
			wantJoins: map[int64]int64{},
		},
		{
			name:      "narrative has no pending comment or transition: no join, no report",
			batch:     []store.ActionRow{worklogRow(t, 2, "PROJ-1", store.WorklogOpAdd, "m1")},
			msgs:      []store.MessageAttribution{attr("m1", 3, "PROJ-1", tracked)},
			wantJoins: map[int64]int64{},
		},
		{
			name: "two narratives each with a pending comment: reported",
			batch: []store.ActionRow{
				actionRow(1, 7, "comment", "PROJ-1"), actionRow(4, 3, "comment", "PROJ-1"),
				worklogRow(t, 2, "PROJ-1", store.WorklogOpAdd, "m1", "m2"),
			},
			msgs:        []store.MessageAttribution{attr("m1", 7, "PROJ-1", tracked), attr("m2", 3, "PROJ-1", tracked)},
			wantJoins:   map[int64]int64{},
			wantReports: []string{"worklog action 2 on PROJ-1 covers work from narratives 3, 7, each with a pending action: reviewed alone"},
		},
		{
			name:      "two narratives, only one with a pending comment: joins that one",
			batch:     []store.ActionRow{actionRow(1, 3, "comment", "PROJ-1"), worklogRow(t, 2, "PROJ-1", store.WorklogOpAdd, "m1", "m2")},
			msgs:      []store.MessageAttribution{attr("m1", 7, "PROJ-1", tracked), attr("m2", 3, "PROJ-1", tracked)},
			wantJoins: map[int64]int64{2: 3},
		},
		{
			name: "one narrative with two pending comments: reported",
			batch: []store.ActionRow{
				actionRow(1, 3, "comment", "PROJ-1"), actionRow(4, 3, "comment", "PROJ-1"),
				worklogRow(t, 2, "PROJ-1", store.WorklogOpAdd, "m1"),
			},
			msgs:        []store.MessageAttribution{attr("m1", 3, "PROJ-1", tracked)},
			wantJoins:   map[int64]int64{},
			wantReports: []string{"worklog action 2 on PROJ-1: narrative 3 has 2 pending comment/transition actions: reviewed alone"},
		},
		{
			name:      "delete worklog carries no message ids: no join",
			batch:     []store.ActionRow{actionRow(1, 3, "comment", "PROJ-1"), worklogRow(t, 2, "PROJ-1", store.WorklogOpDelete)},
			msgs:      []store.MessageAttribution{attr("m1", 3, "PROJ-1", tracked)},
			wantJoins: map[int64]int64{},
		},
		{
			name: "undecodable payload: reported",
			batch: []store.ActionRow{
				actionRow(1, 3, "comment", "PROJ-1"),
				{ID: 2, Type: store.ActionTypeWorklog, IssueKey: "PROJ-1", Payload: "{"},
			},
			msgs:        []store.MessageAttribution{attr("m1", 3, "PROJ-1", tracked)},
			wantJoins:   map[int64]int64{},
			wantReports: []string{fmt.Sprintf("worklog action 2 on PROJ-1: payload does not decode (%v): reviewed alone", decErr)},
		},
		{
			name: "an add and an update covering one narrative both join it",
			batch: []store.ActionRow{
				actionRow(1, 3, "comment", "PROJ-1"),
				worklogRow(t, 2, "PROJ-1", store.WorklogOpAdd, "m1"),
				worklogRow(t, 5, "PROJ-1", store.WorklogOpUpdate, "m2"),
			},
			msgs:      []store.MessageAttribution{attr("m1", 3, "PROJ-1", tracked), attr("m2", 3, "PROJ-1", tracked)},
			wantJoins: map[int64]int64{2: 3, 5: 3},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			joins, reports := triage.JoinWorklogs(tt.batch, tt.msgs)
			if diff := cmp.Diff(tt.wantJoins, joins); diff != "" {
				t.Errorf("joins (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tt.wantReports, reports); diff != "" {
				t.Errorf("reports (-want +got):\n%s", diff)
			}
		})
	}
}
