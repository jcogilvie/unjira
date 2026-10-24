package config_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/config"
)

func TestLoad_Worklog(t *testing.T) {
	tests := []struct {
		name          string
		body          string
		wantErr       []string // substrings; nil means valid
		wantWorkSince time.Time
		wantIdleGap   time.Duration
	}{
		{
			name:          "date and idle gap parse",
			body:          "worklog: {work_since: 2026-10-01, idle_gap: 20m}\n",
			wantWorkSince: time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local).UTC(), //nolint:gosmopolitan // Date reads local midnight
			wantIdleGap:   20 * time.Minute,
		},
		{
			name: "absent block loads",
			body: "db_path: x.db\n",
		},
		{
			name:    "timestamp is refused",
			body:    "worklog: {work_since: 2026-10-01T00:00:00Z}\n",
			wantErr: []string{"worklog.work_since", "YYYY-MM-DD"},
		},
		{
			name:    "impossible date is refused",
			body:    "worklog: {work_since: 2026-13-01}\n",
			wantErr: []string{"worklog.work_since", "YYYY-MM-DD"},
		},
		{
			name:    "zero idle gap is refused",
			body:    "worklog: {idle_gap: 0s}\n",
			wantErr: []string{"must be positive"},
		},
		{
			name:    "unknown sub-key is refused",
			body:    "worklog: {idle_gaps: 20m}\n",
			wantErr: []string{"idle_gaps"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := loadYAML(t, tt.body)

			if tt.wantErr != nil {
				require.Error(t, err)

				for _, want := range tt.wantErr {
					require.ErrorContains(t, err, want)
				}

				return
			}

			require.NoError(t, err)

			if tt.wantWorkSince.IsZero() {
				assert.True(t, cfg.Worklog.WorkSince.IsZero())
			} else {
				assert.Equal(t, tt.wantWorkSince, cfg.Worklog.WorkSince.Time())
			}

			assert.Equal(t, tt.wantIdleGap, cfg.Worklog.IdleGap.Duration())
		})
	}
}

func TestValidateTrackers_AttributionSinks(t *testing.T) {
	const conns = `
connections:
  - {name: j, kind: jira, endpoint: "https://x.atlassian.net"}
  - {name: gh, kind: github, endpoint: "https://api.github.com"}
  - {name: l, kind: local}
`

	tests := []struct {
		name      string
		tracker   string
		wantSinks []string
		wantErr   string
	}{
		{
			name:      "writable jira defaults to worklog",
			tracker:   `{name: t, connection: j, scopes: ["PROJ"], writable_scopes: ["PROJ"]}`,
			wantSinks: []string{config.SinkWorklog},
		},
		{
			name:      "empty list disables",
			tracker:   `{name: t, connection: j, scopes: ["PROJ"], writable_scopes: ["PROJ"], attribution_sinks: []}`,
			wantSinks: []string{},
		},
		{
			name:    "read-only jira has none",
			tracker: `{name: t, connection: j, scopes: ["PROJ"]}`,
		},
		{
			name:    "sinks on a read-only tracker",
			tracker: `{name: t, connection: j, scopes: ["PROJ"], attribution_sinks: [worklog]}`,
			wantErr: `tracker "t": attribution_sinks is set, but writable_scopes is empty: a sink only drafts writes, so it would draft nothing`,
		},
		{
			name:    "sinks on a local tracker",
			tracker: `{name: t, connection: l, scopes: ["PROJ"], writable_scopes: ["PROJ"], attribution_sinks: [worklog]}`,
			wantErr: `tracker "t": attribution_sinks is set on a local tracker: the only sink, worklog, writes Jira worklogs`,
		},
		{
			name:    "sinks on a github tracker",
			tracker: `{name: t, connection: gh, scopes: ["o/r"], attribution_sinks: [worklog]}`,
			wantErr: `tracker "t": attribution_sinks is set on a github tracker: the only sink, worklog, writes Jira worklogs`,
		},
		{
			name:    "unknown sink",
			tracker: `{name: t, connection: j, scopes: ["PROJ"], writable_scopes: ["PROJ"], attribution_sinks: [timesheet]}`,
			wantErr: `tracker "t": attribution_sinks names unknown sink "timesheet" (known: worklog)`,
		},
		{
			name:    "duplicate sink",
			tracker: `{name: t, connection: j, scopes: ["PROJ"], writable_scopes: ["PROJ"], attribution_sinks: [worklog, worklog]}`,
			wantErr: `tracker "t": attribution_sinks lists "worklog" twice`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := loadYAML(t, conns+"trackers: ["+tt.tracker+"]\n")

			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)

				return
			}

			require.NoError(t, err)

			tr, ok := cfg.TrackerByName("t")
			require.True(t, ok)
			assert.Equal(t, tt.wantSinks, cfg.SinksFor(tr))
		})
	}
}
