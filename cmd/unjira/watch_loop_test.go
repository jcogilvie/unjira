package main

// watch_loop_test.go exercises appContext.watchLoop's mechanics — lease
// acquire/release per tick, --once's single-pass exit, and "a pass failure
// must not exit the loop" — against a fake runOnePass, independent of the
// real collect->narrate->match->reconcile->auto-commit composition that
// watch_pass_test.go covers separately. Splitting the two is what lets each
// be tested without needing both working at once (per watchLoop's own doc
// comment).

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWatchLoop_OnceRunsExactlyOnePass(t *testing.T) {
	app := &appContext{store: openTestStore(t)}

	var calls atomic.Int32
	err := app.watchLoop(t.Context(), time.Hour, true, func(_ context.Context, runID string) error {
		calls.Add(1)
		assert.NotEmpty(t, runID, "watchLoop must hand the pass a real runID for the lease it already holds")

		return nil
	})

	require.NoError(t, err)
	assert.Equal(t, int32(1), calls.Load())
}

func TestWatchLoop_OnceAcquiresAndReleasesTheLease(t *testing.T) {
	app := &appContext{store: openTestStore(t)}

	var seenRunID string
	err := app.watchLoop(t.Context(), time.Hour, true, func(_ context.Context, runID string) error {
		seenRunID = runID

		// The lease must be HELD while the pass runs: a second, unrelated
		// runID must not be able to acquire it mid-pass.
		ok, tryErr := app.store.TryAcquire("someone-else", time.Now(), time.Hour)
		require.NoError(t, tryErr)
		assert.False(t, ok, "the lease must still be held by this pass's runID while it runs")

		return nil
	})
	require.NoError(t, err)
	require.NotEmpty(t, seenRunID)

	// Released after the pass returns: now a fresh runID can acquire it.
	ok, err := app.store.TryAcquire("after-the-pass", time.Now(), time.Hour)
	require.NoError(t, err)
	assert.True(t, ok, "watchLoop must release the lease after the pass returns")
}

// TestWatchLoop_FailureThenSuccessThenCancelRunsBothPasses is the property
// the design doc calls out explicitly: a transient failure (a 503, an
// expired credential) is exactly the condition watch exists to survive.
// --once can't observe "the loop kept going" directly (there's only one
// tick to observe), so this uses a first-pass-fails-second-succeeds seam
// instead — the technique the task brief names for this exact test — with
// the second pass canceling the loop's context so the test terminates with
// a real, checkable assertion: pass 1's failure must not have stopped the
// loop before pass 2 ran.
func TestWatchLoop_FailureThenSuccessThenCancelRunsBothPasses(t *testing.T) {
	app := &appContext{store: openTestStore(t)}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var calls int
	err := app.watchLoop(ctx, time.Millisecond, false, func(_ context.Context, _ string) error {
		calls++
		if calls == 1 {
			return fmt.Errorf("transient jira 503")
		}
		cancel()

		return nil
	})

	require.NoError(t, err, "watchLoop itself returns nil on a canceled context, not the pass's own errors")
	assert.Equal(t, 2, calls, "the first pass's failure must not have stopped the loop before the second pass ran")
}

func TestWatchLoop_ContextCanceledBeforeFirstTickRunsNoPasses(t *testing.T) {
	app := &appContext{store: openTestStore(t)}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var calls int
	err := app.watchLoop(ctx, time.Hour, false, func(_ context.Context, _ string) error {
		calls++

		return nil
	})

	require.NoError(t, err)
	assert.Zero(t, calls, "a context canceled before the loop starts must run nothing")
}
