package pipeline

// f16_probe_test.go attributes and bounds clustering's prompt cost — the F16
// measurement harness, kept because the numbers it produces are what a future
// "optimization" would silently regress.
//
// Every test here is env-gated and skipped by default: they read a COPY of a real
// store (F16_DB), which CI does not have, and one makes real LLM calls. Nothing
// here writes.
//
//	F16_PROBE=1 F16_DB=/path/to/copy.db go test ./internal/pipeline/ -run TestF16 -v
//	F16_PROBE=1 F16_DECISIONS=1 F16_MAX_OUTPUT=64000 F16_DB=... go test ... -run DecisionDelta
//
// WHY ATTRIBUTION COMES FIRST. Four candidate fixes were proposed and falsified
// before TestF16_WhereAreTheTokens existed; see design-notes.md #32. The sharpest:
// truncating Narrative.Events measured EXACTLY 0.0%, because
// hydrateContextNarratives puts frozen events in Events and assignable ones in
// EligibleEvents — and with nothing applied, Events is empty for every narrative.
// Zeroing one payload site at a time settles in one run what argument could not.

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/clients/openai"
	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/correlator"
	"github.com/jcogilvie/unjira/internal/llm"
	"github.com/jcogilvie/unjira/internal/store"
)

// budgetTokens is the configured llm.context_window_tokens these measurements are
// read against: above it, Cluster bisects rather than making one call.
const budgetTokens = 200000

// probeClient returns an empty clustering: the prompt gets built and measured,
// nothing is parsed, no network is touched. Cluster's prompt builder and token
// estimator are pure, which is what makes this harness free to run.
type probeClient struct{}

func (p *probeClient) Complete(_ context.Context, _, _ string) (string, llm.Usage, error) {
	return "[]", llm.Usage{}, nil
}

// openProbeStore opens the store copy, or skips.
func openProbeStore(t *testing.T) *store.Store {
	t.Helper()

	if os.Getenv("F16_PROBE") == "" {
		t.Skip("set F16_PROBE=1 and F16_DB=<copy of a real store> to run the F16 measurements")
	}

	s, err := store.Open(os.Getenv("F16_DB"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	return s
}

// probeInputs loads one window's real clustering inputs.
func probeInputs(t *testing.T, s *store.Store, days int) (
	[]correlator.Event, []correlator.Narrative, correlator.TimeRange,
) {
	t.Helper()

	now := time.Now().UTC()
	window := correlator.TimeRange{
		Start: now.Add(-time.Duration(days) * 24 * time.Hour),
		End:   now,
	}

	cands, err := s.UnlinkedEventsInRange(window.Start, window.End)
	require.NoError(t, err)

	// Unbounded (maxContext=0): these measurements are about attributing and
	// capping the CONTENTS of the prompt, not about the new context-narrative
	// COUNT bound this file's own doc comment names as the untried lever — see
	// TestF16_WhereAreTheTokens's own comment.
	ctxNarr, _, err := hydrateContextNarratives(s, window, cands, 0)
	require.NoError(t, err)

	return cands, ctxNarr, window
}

// tokensFor measures one shaping of the context set through the real Cluster path.
// A budget of 1<<30 keeps it from bisecting, so the number is one prompt's cost.
func tokensFor(t *testing.T, cands []correlator.Event, ctxNarr []correlator.Narrative,
	window correlator.TimeRange,
) int {
	t.Helper()

	_, stats, _ := correlator.Cluster(
		t.Context(), cands, ctxNarr, &probeClient{}, window, 1<<30)

	return stats.EstimatedTokens
}

// truncateContextEvents caps every context event's Summary at BOTH render sites —
// EligibleEvents (numbered candidates, correlator.go:380) and Events (context
// detail, :393). Both matter: an earlier version touched only Events and measured
// 0.0%, which is how design-notes #32 got written.
//
// Deliberately does NOT drop events or narratives. Dropping a narrative from
// `existing` also removes its events from assignableEvents' index space, so
// reshuffling would silently lose reach — the "never silently drop data"
// invariant in CLAUDE.md.
func truncateContextEvents(in []correlator.Narrative, maxChars int) []correlator.Narrative {
	cut := func(evs []correlator.Event) []correlator.Event {
		if evs == nil {
			return nil
		}

		out := make([]correlator.Event, 0, len(evs))
		for _, e := range evs {
			ce := e
			if len(ce.Summary) > maxChars {
				ce.Summary = ce.Summary[:maxChars] + "…"
			}
			out = append(out, ce)
		}

		return out
	}

	out := make([]correlator.Narrative, 0, len(in))
	for _, nar := range in {
		cp := nar
		cp.Events = cut(nar.Events)
		cp.EligibleEvents = cut(nar.EligibleEvents)
		out = append(out, cp)
	}

	return out
}

// TestF16_WhereAreTheTokens attributes the prompt by zeroing one payload site at a
// time. This is the test that should have been written first.
func TestF16_WhereAreTheTokens(t *testing.T) {
	s := openProbeStore(t)
	cands, ctxNarr, window := probeInputs(t, s, 30)

	nEvents, nEligible := 0, 0
	for _, nar := range ctxNarr {
		nEvents += len(nar.Events)
		nEligible += len(nar.EligibleEvents)
	}

	base := tokensFor(t, cands, ctxNarr, window)
	fmt.Printf("\ncands=%d narr=%d Events=%d EligibleEvents=%d\nBASE %7d tok\n\n",
		len(cands), len(ctxNarr), nEvents, nEligible, base)

	zeroed := func(mut func(*correlator.Narrative)) []correlator.Narrative {
		out := make([]correlator.Narrative, len(ctxNarr))
		for i, nar := range ctxNarr {
			cp := nar
			mut(&cp)
			out[i] = cp
		}

		return out
	}

	for _, tc := range []struct {
		name string
		in   []correlator.Narrative
	}{
		{"Events=nil", zeroed(func(n *correlator.Narrative) { n.Events = nil })},
		{"EligibleEvents=nil", zeroed(func(n *correlator.Narrative) { n.EligibleEvents = nil })},
		{"Summary=\"\"", zeroed(func(n *correlator.Narrative) { n.Summary = "" })},
		{"no narratives at all", nil},
	} {
		got := tokensFor(t, cands, tc.in, window)
		fmt.Printf("  %-22s %7d tok | costs %7d tok (%5.1f%%)\n",
			tc.name, got, base-got, 100*float64(base-got)/float64(base))
	}
}

// TestF16_WideWindowsWithCap is the finding's payoff: does capping let a WIDE
// window fit in ONE call? Uncapped, 90d and beyond exceed the budget and bisect —
// which measured 1.37x the single call, since both halves re-hydrate the same
// context narratives.
func TestF16_WideWindowsWithCap(t *testing.T) {
	s := openProbeStore(t)

	fmt.Printf("\n%5s | %8s | %8s %8s %8s %8s   (! = over %d, bisects)\n",
		"since", "uncapped", "cap5000", "cap2000", "cap1000", "cap500", budgetTokens)

	for _, days := range []int{30, 60, 90, 180, 365} {
		cands, ctxNarr, window := probeInputs(t, s, days)

		mark := func(v int) string {
			if v < budgetTokens {
				return fmt.Sprintf("%7d ", v)
			}

			return fmt.Sprintf("%7d!", v)
		}

		capped := func(n int) string {
			return mark(tokensFor(t, cands, truncateContextEvents(ctxNarr, n), window))
		}

		fmt.Printf("%4dd | %s | %s %s %s %s   (events=%d narr=%d)\n",
			days, mark(tokensFor(t, cands, ctxNarr, window)),
			capped(5000), capped(2000), capped(1000), capped(500),
			len(cands), len(ctxNarr))
	}
}

// realClient builds the configured LLM client for the decision-delta test only,
// mirroring cmd/unjira's llmClient(). Reads unjira.config.json rather than
// hardcoding an endpoint, so it cannot send prompts somewhere unintended.
func realClient() (llm.Client, error) {
	cfg, err := config.Load("../../unjira.config.json")
	if err != nil {
		return nil, err
	}
	if err := cfg.LLM.Validate(); err != nil {
		return nil, err
	}
	if cfg.LLM.BaseURL == "" {
		return nil, fmt.Errorf("llm.base_url is required")
	}

	helper, err := cfg.LLM.ResolvedAPIKeyHelper()
	if err != nil {
		return nil, err
	}

	// Output ceiling is overridable because the first delta run DIED on it: cap2000
	// built a smaller prompt and then exceeded 32,000 completion tokens, since ~104
	// clusters each emit a title and summary. That is F16's other half — the response
	// ceiling binds before the prompt budget. Overridden by env rather than by editing
	// unjira.config.json, so a measurement leaves config alone.
	maxOut := cfg.LLM.MaxOutputTokens
	if v := os.Getenv("F16_MAX_OUTPUT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("F16_MAX_OUTPUT=%q: %w", v, err)
		}
		maxOut = n
	}

	return openai.New(cfg.LLM.BaseURL,
		llm.NewHelperCredential(helper), cfg.LLM.Model, maxOut), nil
}

// TestF16_DecisionDelta is the correctness half, and the only part that costs real
// calls. Capping is cheap and effective on tokens; the question is whether it
// changes the model's JUDGMENT. Clusters the same window twice and diffs the
// EXTENDS/NEW verdicts.
//
// Measured at cap2000 over 60 days: 100 of 110 clusters identical (91%), the 10
// differences being regroupings at the margin (8 NEW, 2 EXTENDS) — coarser, not
// collapsed. Prompt fell 143,229 -> 89,193 actual tokens.
func TestF16_DecisionDelta(t *testing.T) {
	if os.Getenv("F16_DECISIONS") == "" {
		t.Skip("set F16_DECISIONS=1 as well; this test makes real LLM calls")
	}

	s := openProbeStore(t)
	cands, ctxNarr, window := probeInputs(t, s, 60)

	client, err := realClient()
	require.NoError(t, err)

	fmt.Printf("\nclustering %d candidates against %d narratives, twice\n", len(cands), len(ctxNarr))

	// Key each cluster by its member events, so the runs compare even when titles
	// differ in wording.
	describe := func(label string, in []correlator.Narrative) map[string]string {
		res, stats, err := correlator.Cluster(t.Context(), cands, in, client, window, budgetTokens)
		if err != nil {
			// Report and continue: one arm failing must not discard the other arm's
			// expensive result, which is what happened on the first run.
			fmt.Printf("  %-9s FAILED after %d tok est: %v\n", label, stats.EstimatedTokens, err)

			return nil
		}

		out := map[string]string{}
		for _, r := range res {
			key := ""
			for _, e := range r.Events {
				key += e.ExternalID + "|"
			}

			verdict := "NEW"
			if r.NarrativeID != 0 {
				verdict = fmt.Sprintf("EXTENDS %d", r.NarrativeID)
			}
			out[key] = verdict
		}

		fmt.Printf("  %-9s %d clusters | %d tok est | %d prompt tok actual\n",
			label, len(res), stats.EstimatedTokens, stats.PromptTokens)

		return out
	}

	full := describe("uncapped", ctxNarr)
	capped := describe("cap2000", truncateContextEvents(ctxNarr, 2000))

	if full == nil || capped == nil {
		fmt.Println("\n  one arm failed; no delta to report")

		return
	}

	agree, differ := 0, 0
	for key, v := range full {
		if capped[key] == v {
			agree++

			continue
		}
		differ++
		fmt.Printf("    DIFFER: uncapped=%q capped=%q\n", v, capped[key])
	}

	fmt.Printf("\n  same cluster+verdict: %d | differing: %d | capped-only clusters: %d\n",
		agree, differ, len(capped)-agree)
}
