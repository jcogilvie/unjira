package config

import "fmt"

// DefaultMaxReasks is the re-ask budget when no tier sets one: one follow-up call
// per problem, at every use.
const DefaultMaxReasks = 1

// LLMDefaultsConfig is the llm_defaults block: settings that apply across every
// model block unless a model block overrides them.
type LLMDefaultsConfig struct {
	// MaxReasks is the re-ask budget every model inherits when its own block sets
	// none. A pointer for the reason LLMConfig's budgets are: unset and 0 differ.
	MaxReasks *int `json:"max_reasks"`
}

// ReaskBudgets is how many re-asks each model-call site may make for one problem,
// resolved from config. A re-ask is any model call after the original one, for the
// same problem, so 0 means "never ask again" at every site:
//
//   - Match: follow-ups to a matching call whose response the parser refused.
//   - Omission: rounds asking a clustering call to place the in-window events it
//     left in no cluster.
//   - Dispute: calls per batch asking which cluster owns an event placed in two.
//     The first dispute call is itself the first re-ask of the clustering response,
//     so 0 makes any double placement a loud error.
//
// Exhausting a budget is always a loud error, never a partial result.
type ReaskBudgets struct {
	Match    int
	Omission int
	Dispute  int
}

// ReaskBudgets resolves each use's budget from the most specific tier that is SET:
//
//  1. per use, in the model block (llm.max_match_reasks, llm.max_omission_reasks,
//     llm.max_dispute_reasks);
//  2. per model (llm.max_reasks);
//  3. across all models (llm_defaults.max_reasks);
//  4. DefaultMaxReasks.
//
// Resolved here, once, so the packages that make the calls receive plain ints and
// never see the tiers.
//
// A negative value at any tier is an error naming its key, even when a more specific
// tier shadows it for every use: it is a typo wherever it sits (most likely someone
// reaching for "unlimited", which deliberately does not exist, because an unbounded
// re-ask loop on a model that keeps failing is the cost a budget exists to cap), and
// accepting it would leave it to surface only when the key above it is removed.
func (c Config) ReaskBudgets() (ReaskBudgets, error) {
	tiers := []struct {
		key string
		val *int
	}{
		{"llm_defaults.max_reasks", c.LLMDefaults.MaxReasks},
		{"llm.max_reasks", c.LLM.MaxReasks},
		{"llm.max_match_reasks", c.LLM.MaxMatchReasks},
		{"llm.max_omission_reasks", c.LLM.MaxOmissionReasks},
		{"llm.max_dispute_reasks", c.LLM.MaxDisputeReasks},
	}
	for _, t := range tiers {
		if t.val != nil && *t.val < 0 {
			return ReaskBudgets{}, fmt.Errorf(
				"%s is %d: must be zero or positive (0 never re-asks), or omitted to inherit the tier below",
				t.key, *t.val)
		}
	}

	perModel, global := c.LLM.MaxReasks, c.LLMDefaults.MaxReasks

	return ReaskBudgets{
		Match:    firstSet(c.LLM.MaxMatchReasks, perModel, global),
		Omission: firstSet(c.LLM.MaxOmissionReasks, perModel, global),
		Dispute:  firstSet(c.LLM.MaxDisputeReasks, perModel, global),
	}, nil
}

// firstSet returns the value of the first non-nil candidate, most specific first,
// or DefaultMaxReasks when none is set.
func firstSet(candidates ...*int) int {
	for _, c := range candidates {
		if c != nil {
			return *c
		}
	}

	return DefaultMaxReasks
}
