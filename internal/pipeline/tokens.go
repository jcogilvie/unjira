package pipeline

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/store"
)

// TokenAmount is a token count of each of the four kinds and what they cost in dollars.
type TokenAmount struct {
	Input, Output, CacheWrite, CacheRead int64
	Cost                                 float64
}

func (a *TokenAmount) add(b TokenAmount) {
	a.Input += b.Input
	a.Output += b.Output
	a.CacheWrite += b.CacheWrite
	a.CacheRead += b.CacheRead
	a.Cost += b.Cost
}

// ModelTokens is one model's share of a bucket. Logged is the share already logged
// in a Jira worklog on this ticket; Pending is the share named in a proposed worklog
// add; Unlogged is everything else (R12).
type ModelTokens struct {
	Model                     string
	Unlogged, Logged, Pending TokenAmount
}

// TicketTokens is what the messages charged to one tracked issue cost, per model.
type TicketTokens struct {
	Connection, IssueKey string
	Models               []ModelTokens
}

// UntrackedTokens is one untracked narrative's cost. CandidateKey is its below-floor
// primary, or empty when it has none.
type UntrackedTokens struct {
	NarrativeID  int64
	CandidateKey string
	Models       []ModelTokens
}

// UnattributedTokens is the cost of the messages in one state charged nowhere:
// store.AttributionUnattributed or store.AttributionNoEvent.
type UnattributedTokens struct {
	State    store.AttributionState
	Messages int
	Models   []ModelTokens
}

// OwnSpendWindow is unjira's own LLM spend over one window. UpperBound is set when
// some call in it reported no cached count: those are priced as all uncached input,
// so Cost is then an upper bound.
type OwnSpendWindow struct {
	Window                                 string
	Calls                                  int
	PromptTokens, CompletionTokens, Cached int64
	Cost                                   float64
	UpperBound                             bool
}

// TokenReport is what `unjira tokens` prints.
type TokenReport struct {
	Messages     int
	Tickets      []TicketTokens
	Untracked    []UntrackedTokens
	Unattributed []UnattributedTokens
	OwnSpend     []OwnSpendWindow
}

// BuildTokenReport attributes every ledgered message as of now (confidenceFloor is
// match.confidence_floor) and windows unjira's own calls ending at now: 30d, 7d and
// 1d are rolling, and today starts at midnight in now's location. Every model seen,
// in a transcript or an own call, must be in pricing, or it errors naming the model.
func BuildTokenReport(s *store.Store, pricing config.TokenPricing, confidenceFloor float64, now time.Time) (TokenReport, error) {
	msgs, err := s.MessageAttributions(confidenceFloor)
	if err != nil {
		return TokenReport{}, err
	}
	pendingIDs, err := pendingWorklogMessages(s)
	if err != nil {
		return TokenReport{}, err
	}

	type ticketKey struct{ connection, issue string }
	tickets := map[ticketKey]map[string]*ticketBuckets{}
	untracked := map[int64]map[string]*TokenAmount{}
	candidates := map[int64]string{}
	unattributed := map[store.AttributionState]map[string]*TokenAmount{}
	unattributedCount := map[store.AttributionState]int{}

	for _, m := range msgs {
		price, err := pricing.Lookup(m.Model)
		if err != nil {
			return TokenReport{}, err
		}
		amount := TokenAmount{
			Input: m.InputTokens, Output: m.OutputTokens,
			CacheWrite: m.CacheWriteTokens, CacheRead: m.CacheReadTokens,
			Cost: (float64(m.InputTokens)**price.Input + float64(m.OutputTokens)**price.Output +
				float64(m.CacheWriteTokens)**price.CacheWrite + float64(m.CacheReadTokens)**price.CacheRead) / 1e6,
		}
		switch m.State {
		case store.AttributionTracked:
			k := ticketKey{m.Connection, m.IssueKey}
			if tickets[k] == nil {
				tickets[k] = map[string]*ticketBuckets{}
			}
			if tickets[k][m.Model] == nil {
				tickets[k][m.Model] = &ticketBuckets{}
			}
			switch {
			case m.WorklogID != "" && m.WorklogIssueKey == m.IssueKey:
				tickets[k][m.Model].logged.add(amount)
			case pendingIDs[m.MessageID]:
				tickets[k][m.Model].pending.add(amount)
			default:
				tickets[k][m.Model].unlogged.add(amount)
			}
		case store.AttributionUntracked:
			addTo(untracked, m.NarrativeID, m.Model, amount)
			candidates[m.NarrativeID] = m.IssueKey
		case store.AttributionUnattributed, store.AttributionNoEvent:
			addTo(unattributed, m.State, m.Model, amount)
			unattributedCount[m.State]++
		default:
			return TokenReport{}, fmt.Errorf("message %s: attribution state %q has no report bucket", m.MessageID, m.State)
		}
	}

	report := TokenReport{Messages: len(msgs)}
	for _, k := range slices.SortedFunc(maps.Keys(tickets), func(a, b ticketKey) int {
		return cmp.Or(cmp.Compare(a.connection, b.connection), cmp.Compare(a.issue, b.issue))
	}) {
		report.Tickets = append(report.Tickets, TicketTokens{
			Connection: k.connection, IssueKey: k.issue, Models: ticketModels(tickets[k]),
		})
	}
	for _, id := range slices.SortedFunc(maps.Keys(untracked), cmp.Compare[int64]) {
		report.Untracked = append(report.Untracked, UntrackedTokens{
			NarrativeID: id, CandidateKey: candidates[id], Models: modelTokens(untracked[id]),
		})
	}
	for _, st := range slices.SortedFunc(maps.Keys(unattributed), cmp.Compare[store.AttributionState]) {
		report.Unattributed = append(report.Unattributed, UnattributedTokens{
			State: st, Messages: unattributedCount[st], Models: modelTokens(unattributed[st]),
		})
	}

	report.OwnSpend, err = ownSpend(s, pricing, now)
	if err != nil {
		return TokenReport{}, err
	}

	return report, nil
}

func ownSpend(s *store.Store, pricing config.TokenPricing, now time.Time) ([]OwnSpendWindow, error) {
	windows := []struct {
		name  string
		start time.Time
	}{
		{"30d", now.Add(-30 * 24 * time.Hour)},
		{"7d", now.Add(-7 * 24 * time.Hour)},
		{"1d", now.Add(-24 * time.Hour)},
		{"today", time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())},
	}
	// 30d starts earliest.
	calls, err := s.LLMCallsSince(windows[0].start)
	if err != nil {
		return nil, err
	}

	out := make([]OwnSpendWindow, len(windows))
	for i, w := range windows {
		out[i].Window = w.name
	}
	for _, c := range calls {
		if c.CalledAt.After(now) {
			continue
		}
		price, err := pricing.Lookup(c.Model)
		if err != nil {
			return nil, err
		}
		// Ruling: a call that reported no cached count is priced as all uncached input.
		var cached int64
		if c.CachedTokens != nil {
			cached = *c.CachedTokens
		}
		// prompt_tokens is assumed to include cached_tokens (OpenAI semantics). A gateway
		// passing through Anthropic's, where input excludes cache reads, would break that,
		// and pricing it anyway would make the cost shrink with no signal.
		if cached > c.PromptTokens {
			return nil, fmt.Errorf(
				"llm call %s at %s reports cached_tokens %d above prompt_tokens %d: "+
					"own-spend pricing assumes prompt_tokens includes cached_tokens",
				c.Stage, c.CalledAt.Format(time.RFC3339), cached, c.PromptTokens)
		}
		cost := (float64(c.PromptTokens-cached)**price.Input + float64(cached)**price.CacheRead +
			float64(c.CompletionTokens)**price.Output) / 1e6
		for i, w := range windows {
			if c.CalledAt.Before(w.start) {
				continue
			}
			out[i].Calls++
			out[i].PromptTokens += c.PromptTokens
			out[i].CompletionTokens += c.CompletionTokens
			out[i].Cached += cached
			out[i].Cost += cost
			out[i].UpperBound = out[i].UpperBound || c.CachedTokens == nil
		}
	}

	return out, nil
}

// ticketBuckets holds one model's share of a ticket in the three report columns.
type ticketBuckets struct{ unlogged, logged, pending TokenAmount }

// pendingWorklogMessages returns the message ids named by proposed worklog adds.
// Only an add marks messages as about to be logged; a correction (update or delete)
// proposes no new logging.
func pendingWorklogMessages(s *store.Store) (map[string]bool, error) {
	actions, err := s.ActionsByStatus(store.StatusProposed)
	if err != nil {
		return nil, err
	}
	pending := map[string]bool{}
	for _, a := range actions {
		if a.Type != store.ActionTypeWorklog {
			continue
		}
		var payload store.WorklogPayload
		if err := json.Unmarshal([]byte(a.Payload), &payload); err != nil {
			return nil, fmt.Errorf("action %d: unmarshalling worklog payload: %w", a.ID, err)
		}
		if payload.Op != store.WorklogOpAdd {
			continue
		}
		for _, id := range payload.MessageIDs {
			pending[id] = true
		}
	}
	return pending, nil
}

func ticketModels(byModel map[string]*ticketBuckets) []ModelTokens {
	out := make([]ModelTokens, 0, len(byModel))
	for _, model := range slices.SortedFunc(maps.Keys(byModel), strings.Compare) {
		b := byModel[model]
		out = append(out, ModelTokens{Model: model, Unlogged: b.unlogged, Logged: b.logged, Pending: b.pending})
	}
	return out
}

func addTo[K comparable](m map[K]map[string]*TokenAmount, k K, model string, a TokenAmount) {
	if m[k] == nil {
		m[k] = map[string]*TokenAmount{}
	}
	if m[k][model] == nil {
		m[k][model] = &TokenAmount{}
	}
	m[k][model].add(a)
}

func modelTokens(byModel map[string]*TokenAmount) []ModelTokens {
	out := make([]ModelTokens, 0, len(byModel))
	for _, model := range slices.SortedFunc(maps.Keys(byModel), strings.Compare) {
		out = append(out, ModelTokens{Model: model, Unlogged: *byModel[model]})
	}
	return out
}

// RenderTokens renders a TokenReport as text.
func RenderTokens(r TokenReport) string {
	var b strings.Builder
	b.WriteString("# unjira tokens\n\n")

	if r.Messages == 0 {
		b.WriteString("No ledger rows yet. Run: unjira collect\n")
	} else {
		b.WriteString("## Per ticket\n")
		if len(r.Tickets) == 0 {
			b.WriteString("  (none)\n")
		}
		for _, t := range r.Tickets {
			fmt.Fprintf(&b, "%s (%s)\n", t.IssueKey, t.Connection)
			writeModels(&b, t.Models)
		}
		b.WriteString("\n## Untracked work, by narrative id\n")
		if len(r.Untracked) == 0 {
			b.WriteString("  (none)\n")
		}
		for _, u := range r.Untracked {
			candidate := "no primary"
			if u.CandidateKey != "" {
				candidate = "candidate " + u.CandidateKey + ", below the confidence floor"
			}
			fmt.Fprintf(&b, "narrative %d (%s)\n", u.NarrativeID, candidate)
			writeModels(&b, u.Models)
		}
		b.WriteString("\n## Unattributed messages\n")
		if len(r.Unattributed) == 0 {
			b.WriteString("  (none)\n")
		}
		for _, u := range r.Unattributed {
			fmt.Fprintf(&b, "%s: %d message(s)\n", u.State, u.Messages)
			writeModels(&b, u.Models)
		}
	}

	b.WriteString("\n## unjira's own LLM spend\n")
	upperBound := false
	for _, w := range r.OwnSpend {
		mark := ""
		if w.UpperBound {
			mark, upperBound = " (upper bound)", true
		}
		fmt.Fprintf(&b, "  %-5s  %d call(s)  prompt %d (cached %d)  completion %d  $%.4f%s\n",
			w.Window, w.Calls, w.PromptTokens, w.Cached, w.CompletionTokens, w.Cost, mark)
	}
	if upperBound {
		b.WriteString("  An upper bound: some calls reported no cached count, so all their prompt tokens are priced at the input rate.\n")
	}

	return b.String()
}

func writeModels(b *strings.Builder, models []ModelTokens) {
	for _, m := range models {
		u := m.Unlogged
		fmt.Fprintf(b, "  %s  unlogged: in %d, out %d, cache write %d, cache read %d, $%.4f  logged: $%.4f  pending: $%.4f\n",
			m.Model, u.Input, u.Output, u.CacheWrite, u.CacheRead, u.Cost, m.Logged.Cost, m.Pending.Cost)
	}
}
