package worklog

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/attribution"
	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/store/storetest"
)

const model = "claude-opus-5-5"

func pricing() config.TokenPricing {
	return config.TokenPricing{model: {Input: new(5.0), Output: new(25.0), CacheWrite: new(6.25), CacheRead: new(0.5)}}
}

func msg(id string, h, m int) store.MessageAttribution {
	return store.MessageAttribution{
		MessageUsage: store.MessageUsage{
			MessageID: id, TranscriptID: "tr-1", Model: model, OccurredAt: storetest.UTC(h, m, 0),
			InputTokens: 1000, OutputTokens: 100,
		},
		IssueKey: "PROJ-1", State: store.AttributionTracked,
	}
}

func dctx() attribution.DraftContext {
	return attribution.DraftContext{Tracker: config.Tracker{Name: "jira"}, EnabledAt: storetest.UTC(8, 0, 0)}
}

func payloadOf(t *testing.T, row *store.ActionRow) store.WorklogPayload {
	t.Helper()
	require.NotNil(t, row)
	var p store.WorklogPayload
	require.NoError(t, json.Unmarshal([]byte(row.Payload), &p))
	return p
}

// action builds an existing worklog action on PROJ-1.
func action(t *testing.T, id int64, status string, p store.WorklogPayload) store.ActionRow {
	t.Helper()
	b, err := json.Marshal(p)
	require.NoError(t, err)
	return store.ActionRow{ID: id, Type: store.ActionTypeWorklog, IssueKey: "PROJ-1", Payload: string(b), Status: status}
}

func addKey(ids ...string) string { return idempotencyKey(store.WorklogOpAdd, "PROJ-1", "", ids) }

func TestDraft_Delta(t *testing.T) {
	s := New(config.WorklogConfig{}, pricing())

	t.Run("unlogged at or after the floor become one add", func(t *testing.T) {
		st := attribution.TicketState{IssueKey: "PROJ-1", Unlogged: []store.MessageAttribution{
			msg("m0", 7, 59), msg("m1", 8, 0), msg("m2", 8, 10),
		}}
		drafts, err := s.Draft(dctx(), st)
		require.NoError(t, err)
		require.Len(t, drafts, 1)
		row := drafts[0].Row
		require.NotNil(t, row)
		assert.Equal(t, int64(0), drafts[0].Supersedes)
		assert.Equal(t, "PROJ-1", row.IssueKey)
		assert.Equal(t, int64(0), row.NarrativeID)
		assert.Equal(t, store.ActionTypeWorklog, row.Type)
		assert.Equal(t, store.StatusProposed, row.Status)
		assert.InDelta(t, 1.0, row.Confidence, 0)
		assert.Equal(t, "computed from the token ledger: 2 message(s), 2026-10-06T08:00:00Z to 2026-10-06T08:10:00Z",
			row.Rationale)
		p := payloadOf(t, row)
		assert.Equal(t, store.WorklogOpAdd, p.Op)
		assert.Equal(t, []string{"m1", "m2"}, p.MessageIDs)
		assert.Equal(t, 600, p.TimeSpentSeconds)
		assert.Equal(t, storetest.UTC(8, 0, 0), p.Started)
		assert.Equal(t, addKey("m1", "m2"), p.IdempotencyKey)
		assert.NotEmpty(t, p.Comment)
		assert.NotEmpty(t, p.Property)
	})

	t.Run("work_since excludes earlier messages and overrides enabled_at", func(t *testing.T) {
		var since config.Date
		require.NoError(t, since.UnmarshalText([]byte("2026-10-07")))
		s := New(config.WorklogConfig{WorkSince: since}, pricing())
		late := msg("late", 9, 0)
		late.OccurredAt = since.Time().Add(time.Hour)
		st := attribution.TicketState{IssueKey: "PROJ-1", Unlogged: []store.MessageAttribution{msg("early", 9, 0), late}}
		drafts, err := s.Draft(dctx(), st)
		require.NoError(t, err)
		require.Len(t, drafts, 1)
		assert.Equal(t, []string{"late"}, payloadOf(t, drafts[0].Row).MessageIDs)
	})

	t.Run("nothing at or after the floor and nothing pending is no draft", func(t *testing.T) {
		st := attribution.TicketState{IssueKey: "PROJ-1", Unlogged: []store.MessageAttribution{msg("m0", 7, 0)}}
		drafts, err := s.Draft(dctx(), st)
		require.NoError(t, err)
		assert.Empty(t, drafts)
	})
}

func TestDraft_OnePending(t *testing.T) {
	s := New(config.WorklogConfig{}, pricing())
	unlogged := []store.MessageAttribution{msg("m1", 8, 0), msg("m2", 8, 10)}

	for _, status := range []string{store.StatusProposed, store.StatusApproved} {
		t.Run(status+" with the same key is no draft", func(t *testing.T) {
			st := attribution.TicketState{IssueKey: "PROJ-1", Unlogged: unlogged, Actions: []store.ActionRow{
				action(t, 7, status, store.WorklogPayload{Op: store.WorklogOpAdd, IdempotencyKey: addKey("m1", "m2")}),
			}}
			drafts, err := s.Draft(dctx(), st)
			require.NoError(t, err)
			assert.Empty(t, drafts)
		})
	}

	t.Run("a grown delta supersedes the pending add", func(t *testing.T) {
		st := attribution.TicketState{IssueKey: "PROJ-1", Unlogged: unlogged, Actions: []store.ActionRow{
			action(t, 7, store.StatusProposed, store.WorklogPayload{Op: store.WorklogOpAdd, IdempotencyKey: addKey("m1")}),
		}}
		drafts, err := s.Draft(dctx(), st)
		require.NoError(t, err)
		require.Len(t, drafts, 1)
		assert.Equal(t, int64(7), drafts[0].Supersedes)
		assert.Equal(t, []string{"m1", "m2"}, payloadOf(t, drafts[0].Row).MessageIDs)
	})

	t.Run("no unlogged left retires the pending add", func(t *testing.T) {
		st := attribution.TicketState{IssueKey: "PROJ-1", Actions: []store.ActionRow{
			action(t, 7, store.StatusProposed, store.WorklogPayload{Op: store.WorklogOpAdd, IdempotencyKey: addKey("m1")}),
		}}
		drafts, err := s.Draft(dctx(), st)
		require.NoError(t, err)
		assert.Equal(t, []attribution.Draft{{Row: nil, Supersedes: 7}}, drafts)
	})

	t.Run("two pending adds supersede the newest and name the older", func(t *testing.T) {
		st := attribution.TicketState{IssueKey: "PROJ-1", Unlogged: unlogged, Actions: []store.ActionRow{
			action(t, 9, store.StatusProposed, store.WorklogPayload{Op: store.WorklogOpAdd, IdempotencyKey: addKey("m1")}),
			action(t, 4, store.StatusApproved, store.WorklogPayload{Op: store.WorklogOpAdd, IdempotencyKey: addKey("m2")}),
		}}
		drafts, err := s.Draft(dctx(), st)
		require.ErrorContains(t, err, "action 4")
		require.ErrorContains(t, err, "resolve")
		require.Len(t, drafts, 1)
		assert.Equal(t, int64(9), drafts[0].Supersedes)
	})
}

func TestDraft_BlockedByRuling(t *testing.T) {
	s := New(config.WorklogConfig{}, pricing())
	for _, status := range []string{store.StatusRejected, store.StatusFailed} {
		t.Run(status, func(t *testing.T) {
			acts := []store.ActionRow{action(t, 3, status, store.WorklogPayload{Op: store.WorklogOpAdd, IdempotencyKey: addKey("m1")})}
			st := attribution.TicketState{IssueKey: "PROJ-1", Unlogged: []store.MessageAttribution{msg("m1", 8, 0)}, Actions: acts}
			drafts, err := s.Draft(dctx(), st)
			require.NoError(t, err)
			assert.Empty(t, drafts)

			st.Unlogged = append(st.Unlogged, msg("m2", 8, 5))
			drafts, err = s.Draft(dctx(), st)
			require.NoError(t, err)
			require.Len(t, drafts, 1)
			assert.Equal(t, int64(0), drafts[0].Supersedes)
		})
	}
}

func TestDraft_BlockedByRulingRetiresAStalePendingAdd(t *testing.T) {
	s := New(config.WorklogConfig{}, pricing())
	st := attribution.TicketState{IssueKey: "PROJ-1", Unlogged: []store.MessageAttribution{msg("m1", 8, 0)}, Actions: []store.ActionRow{
		action(t, 3, store.StatusRejected, store.WorklogPayload{Op: store.WorklogOpAdd, IdempotencyKey: addKey("m1")}),
		action(t, 5, store.StatusProposed, store.WorklogPayload{Op: store.WorklogOpAdd, IdempotencyKey: addKey("m1", "m2")}),
	}}

	drafts, err := s.Draft(dctx(), st)

	require.NoError(t, err)
	assert.Equal(t, []attribution.Draft{{Row: nil, Supersedes: 5}}, drafts, "m2 left; the pending add over it is stale")
}

func TestDraft_Corrections(t *testing.T) {
	s := New(config.WorklogConfig{}, pricing())
	// Before the enabled_at floor: corrections ignore it.
	kept, moved := msg("k1", 7, 0), msg("x1", 7, 5)
	kept.WorklogID, kept.WorklogIssueKey = "10001", "PROJ-1"
	moved.WorklogID, moved.WorklogIssueKey, moved.IssueKey = "10001", "PROJ-1", "PROJ-2"

	t.Run("shrink", func(t *testing.T) {
		st := attribution.TicketState{
			IssueKey: "PROJ-1",
			Logged:   map[string][]store.MessageAttribution{"10001": {kept}},
			Moved:    map[string][]store.MessageAttribution{"10001": {moved}},
		}
		drafts, err := s.Draft(dctx(), st)
		require.NoError(t, err)
		require.Len(t, drafts, 1)
		row := drafts[0].Row
		assert.Equal(t, "correction: 1 message(s) logged here are now attributed elsewhere", row.Rationale)
		p := payloadOf(t, row)
		assert.Equal(t, store.WorklogOpUpdate, p.Op)
		assert.Equal(t, "10001", p.WorklogID)
		assert.Equal(t, []string{"k1"}, p.MessageIDs)
		assert.Equal(t, minWorklogSeconds, p.TimeSpentSeconds)
		assert.Equal(t, storetest.UTC(7, 0, 0), p.Started)
		assert.Equal(t, idempotencyKey(store.WorklogOpUpdate, "PROJ-1", "10001", []string{"k1"}), p.IdempotencyKey)
		assert.NotEmpty(t, p.Comment)
		assert.NotEmpty(t, p.Property)
	})

	t.Run("delete", func(t *testing.T) {
		st := attribution.TicketState{
			IssueKey: "PROJ-1",
			Moved:    map[string][]store.MessageAttribution{"10001": {moved}},
		}
		drafts, err := s.Draft(dctx(), st)
		require.NoError(t, err)
		require.Len(t, drafts, 1)
		p := payloadOf(t, drafts[0].Row)
		assert.Equal(t, store.WorklogOpDelete, p.Op)
		assert.Equal(t, "10001", p.WorklogID)
		assert.Empty(t, p.MessageIDs)
		assert.Equal(t, 0, p.TimeSpentSeconds)
	})

	delKey := idempotencyKey(store.WorklogOpDelete, "PROJ-1", "10001", nil)
	t.Run("pending, blocked and supersede apply per worklog id", func(t *testing.T) {
		base := attribution.TicketState{
			IssueKey: "PROJ-1",
			Moved:    map[string][]store.MessageAttribution{"10001": {moved}},
		}

		for _, status := range []string{store.StatusProposed, store.StatusRejected} {
			st := base
			st.Actions = []store.ActionRow{action(t, 5, status, store.WorklogPayload{Op: store.WorklogOpDelete, WorklogID: "10001", IdempotencyKey: delKey})}
			drafts, err := s.Draft(dctx(), st)
			require.NoError(t, err)
			assert.Empty(t, drafts, status)
		}

		// A pending correction on another worklog does not count.
		st := base
		st.Actions = []store.ActionRow{action(t, 5, store.StatusProposed, store.WorklogPayload{Op: store.WorklogOpDelete, WorklogID: "10002", IdempotencyKey: "other"})}
		st.Moved = map[string][]store.MessageAttribution{"10001": {moved}}
		drafts, err := s.Draft(dctx(), st)
		require.NoError(t, err)
		var for10001 []attribution.Draft
		for _, d := range drafts {
			if d.Row != nil && payloadOf(t, d.Row).WorklogID == "10001" {
				for10001 = append(for10001, d)
			}
		}
		require.Len(t, for10001, 1)
		assert.Equal(t, int64(0), for10001[0].Supersedes)

		// A shrink pending, then everything moved: the delete supersedes it.
		st = base
		st.Actions = []store.ActionRow{action(t, 6, store.StatusProposed, store.WorklogPayload{Op: store.WorklogOpUpdate, WorklogID: "10001", IdempotencyKey: "old"})}
		drafts, err = s.Draft(dctx(), st)
		require.NoError(t, err)
		require.Len(t, drafts, 1)
		assert.Equal(t, int64(6), drafts[0].Supersedes)
		assert.Equal(t, store.WorklogOpDelete, payloadOf(t, drafts[0].Row).Op)
	})

	t.Run("a correction and an add coexist", func(t *testing.T) {
		st := attribution.TicketState{
			IssueKey: "PROJ-1",
			Unlogged: []store.MessageAttribution{msg("m1", 8, 0)},
			Moved:    map[string][]store.MessageAttribution{"10001": {moved}},
		}
		drafts, err := s.Draft(dctx(), st)
		require.NoError(t, err)
		require.Len(t, drafts, 2)
		ops := []string{payloadOf(t, drafts[0].Row).Op, payloadOf(t, drafts[1].Row).Op}
		assert.ElementsMatch(t, []string{store.WorklogOpAdd, store.WorklogOpDelete}, ops)
	})
}

func TestDraft_UnpricedModelErrors(t *testing.T) {
	s := New(config.WorklogConfig{}, config.TokenPricing{})
	st := attribution.TicketState{IssueKey: "PROJ-1", Unlogged: []store.MessageAttribution{msg("m1", 8, 0)}}
	drafts, err := s.Draft(dctx(), st)
	require.ErrorContains(t, err, model)
	require.ErrorContains(t, err, "PROJ-1")
	assert.Empty(t, drafts)
}

func TestSink_Name(t *testing.T) {
	assert.Equal(t, config.SinkWorklog, New(config.WorklogConfig{}, nil).Name())
}
