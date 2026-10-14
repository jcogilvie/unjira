package main

import (
	"testing"

	"github.com/alecthomas/kong"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTokensCmd_ParsesAndReportsAnEmptyLedger(t *testing.T) {
	var parsed struct {
		Tokens tokensCmd `cmd:""`
	}
	parser, err := kong.New(&parsed)
	require.NoError(t, err)
	_, err = parser.Parse([]string{"tokens"})
	require.NoError(t, err)

	app := &appContext{store: openTestStore(t)}
	out := captureStdout(t, func() {
		require.NoError(t, parsed.Tokens.Run(app))
	})

	assert.Contains(t, out, "No ledger rows yet. Run: unjira collect")
}
