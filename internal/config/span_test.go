package config_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/config"
)

func TestSpan_UnmarshalText(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    time.Duration
		wantErr string
	}{
		{name: "days", input: "7d", want: 168 * time.Hour},
		{name: "weeks", input: "2w", want: 336 * time.Hour},
		{name: "compound day and hour", input: "7d12h", want: 180 * time.Hour},
		{name: "compound week day hour minute", input: "1w2d3h4m", want: 219*time.Hour + 4*time.Minute},
		{name: "stdlib hours still work", input: "36h", want: 36 * time.Hour},
		{name: "stdlib minutes still work", input: "90m", want: 90 * time.Minute},
		{name: "years unsupported", input: "1y", wantErr: "1y"},
		{name: "uppercase unit rejected", input: "7D", wantErr: "7D"},
		{name: "empty rejected", input: "", wantErr: "empty"},
		{name: "negative rejected", input: "-3d", wantErr: "must be positive"},
		{name: "zero rejected", input: "0s", wantErr: "must be positive"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var s config.Span
			err := s.UnmarshalText([]byte(tt.input))

			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, s.Duration())
		})
	}
}

func TestSpan_UnmarshalsFromJSON(t *testing.T) {
	// TextUnmarshaler is honored by encoding/json too, so a Span works as a
	// config field, not just a CLI flag.
	var payload struct {
		Interval config.Span `json:"interval"`
	}

	require.NoError(t, json.Unmarshal([]byte(`{"interval":"7d"}`), &payload))
	assert.Equal(t, 168*time.Hour, payload.Interval.Duration())
}
