package pipeline

import (
	"fmt"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/rules"
)

// loadCorrelatorRules reads cfg.RulesDir and returns only the
// rules.ScopeCorrelator subset, ready to hand to correlator.WithClusterRules/
// correlator.WithRules. Loading and scope-filtering happen here, in
// pipeline, not in internal/correlator — the same split RunMatch already
// uses for compiling link-exclusion patterns (see match.go's doc comment):
// config parsing/loading is pipeline's job, and correlator only ever sees
// resolved values passed in as parameters.
//
// A missing rules directory is not an error (see rules.Load's doc comment):
// this only wraps a genuine read/parse failure with the config path that
// caused it.
func loadCorrelatorRules(cfg config.Config) ([]rules.Rule, error) {
	all, err := rules.Load(cfg.RulesDir())
	if err != nil {
		return nil, fmt.Errorf("loading rules from %s: %w", cfg.RulesDir(), err)
	}

	return rules.ForScope(all, rules.ScopeCorrelator), nil
}
