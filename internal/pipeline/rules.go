package pipeline

import (
	"fmt"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/rules"
)

// loadRulesForScope reads cfg.RulesDir once and returns only the rules whose
// Scope matches scope. Both loadCorrelatorRules and loadReconcilerRules
// delegate here so the read-and-wrap-error logic exists in exactly one
// place — the two scopes differ only in which ForScope filter they need, not
// in how a load failure should be reported.
//
// A missing rules directory is not an error (see rules.Load's doc comment):
// this only wraps a genuine read/parse failure, naming the config path that
// caused it so the error is actionable without a caller-added prefix.
func loadRulesForScope(cfg config.Config, scope rules.Scope) ([]rules.Rule, error) {
	all, err := rules.Load(cfg.RulesDir())
	if err != nil {
		return nil, fmt.Errorf("loading rules from %s: %w", cfg.RulesDir(), err)
	}

	return rules.ForScope(all, scope), nil
}

// loadCorrelatorRules returns the rules.ScopeCorrelator subset, ready to
// hand to correlator.WithClusterRules/correlator.WithRules. Loading and
// scope-filtering happen here, in pipeline, not in internal/correlator — the
// same split RunMatch already uses for compiling link-exclusion patterns
// (see match.go's doc comment): config parsing/loading is pipeline's job,
// and correlator only ever sees resolved values passed in as parameters.
func loadCorrelatorRules(cfg config.Config) ([]rules.Rule, error) {
	return loadRulesForScope(cfg, rules.ScopeCorrelator)
}

// loadReconcilerRules returns the rules.ScopeReconciler subset, ready to
// hand to reconciler.WithRules. Same split as loadCorrelatorRules: pipeline
// loads and filters, internal/reconciler only ever sees the resolved slice.
func loadReconcilerRules(cfg config.Config) ([]rules.Rule, error) {
	return loadRulesForScope(cfg, rules.ScopeReconciler)
}
