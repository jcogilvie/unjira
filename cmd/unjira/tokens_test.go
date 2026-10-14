package main

import (
	"testing"

	"github.com/alecthomas/kong"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTokensCmd_ParsesAndReportsAnEmptyLedger(t *testing.T) {
	// The real cli, so this fails if the command is ever unregistered.
	parser, err := kong.New(&cli)
	require.NoError(t, err)
	ctx, err := parser.Parse([]string{"tokens"})
	require.NoError(t, err)
	assert.Equal(t, "tokens", ctx.Command())

	app := &appContext{store: openTestStore(t)}
	out := captureStdout(t, func() {
		require.NoError(t, cli.Tokens.Run(app))
	})

	assert.Contains(t, out, "No ledger rows yet. Run: unjira collect")
}
