package pipeline_test

// remainder_test.go covers finding F10: a pass that hit its per-pass narrative
// cap must say so in its RENDERED OUTPUT, not only in a log line.
//
// Both caps already logged, and both named their config key — the code was fine.
// The problem was where the message went. log.Printf writes to stderr while the
// rendered summary goes to stdout, so a truncated pass ENDS with a clean-looking
// summary and the warning is easy to lose. That is not hypothetical: a
// 42-narrative backlog was misdiagnosed as a clustering defect, and the
// misdiagnosis survived three drain passes because the diagnosing session piped
// output through `tail` and discarded the very line that explained it.
//
// The remainder is returned as DATA rather than only printed, for two reasons:
//   - a renderer that had to query the store to learn it would gain I/O, and
//     these renderers are pure functions over a result struct
//   - `watch` can act on it. A loop that knows it is behind can drain rather than
//     sleep its interval, which is the difference between "re-run this by hand N
//     times" and "it catches up".
//
// Deliberately NOT on correlator.Stats, which is where it first looked like it
// belonged: Stats.Add folds stage totals together (internal/pipeline calls it for
// cluster + persist, and reconcile + create), so a remainder there would be
// SUMMED across stages. Two stages each with 27 narratives left does not mean 54.

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/pipeline"
)

// TestRenderMatchResult_SurfacesTheRemainder is the point of the finding: the
// operator reads stdout, so stdout is where "you are not caught up" has to appear.
func TestRenderMatchResult_SurfacesTheRemainder(t *testing.T) {
	out := pipeline.RenderMatchResult(pipeline.MatchRunResult{
		Matched:   []correlator.MatchResult{},
		Remaining: 27,
	})

	assert.Contains(t, out, "27",
		"the count must be in the rendered output, not only in a stderr log line")
	assert.Contains(t, strings.ToLower(out), "re-run",
		"and it must say what to DO about it — a bare number is a fact, not an instruction")
}

// TestRenderMatchResult_SaysNothingWhenCaughtUp is what stops this becoming noise.
//
// A pass that drained its backlog must not print a remainder line at all. "0
// narratives remaining" on every ordinary pass would train an operator to skip
// the line, which is exactly how the stderr warning got ignored in the first
// place — the fix would then have reproduced the bug it was written for.
func TestRenderMatchResult_SaysNothingWhenCaughtUp(t *testing.T) {
	out := pipeline.RenderMatchResult(pipeline.MatchRunResult{
		Matched:   []correlator.MatchResult{},
		Remaining: 0,
	})

	assert.NotContains(t, strings.ToLower(out), "remaining",
		"a drained pass must be silent about the remainder")
	assert.NotContains(t, strings.ToLower(out), "re-run")
}

// TestRenderReconcileResult_SurfacesTheRemainder: the reconciler has TWO caps
// (eligible narratives, and untracked narratives for creates), and either can
// truncate. A reviewer whose queue is short needs to know which pass was bounded.
func TestRenderReconcileResult_SurfacesTheRemainder(t *testing.T) {
	out := pipeline.RenderReconcileResult(pipeline.ReconcileRunResult{
		Remaining: 15,
	})

	assert.Contains(t, out, "15")
	assert.Contains(t, strings.ToLower(out), "re-run")
}

func TestRenderReconcileResult_SaysNothingWhenCaughtUp(t *testing.T) {
	out := pipeline.RenderReconcileResult(pipeline.ReconcileRunResult{Remaining: 0})

	assert.NotContains(t, strings.ToLower(out), "remaining")
}
