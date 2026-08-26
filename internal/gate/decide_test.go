package gate_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/jcogilvie/unjira/internal/config"
	"github.com/jcogilvie/unjira/internal/gate"
	"github.com/jcogilvie/unjira/internal/store"
)

// TestDecide_QueuesWhenAutoCommitIsNilOrEmpty is the gate's safety property in
// the shape Decide actually sees it: a nil map (the zero value; what an
// untouched config produces) or an empty one must queue every action type at
// any confidence, including 1.0. This is the property everything else in the
// gate rests on.
func TestDecide_QueuesWhenAutoCommitIsNilOrEmpty(t *testing.T) {
	action := store.ActionRow{Type: "comment", Confidence: 1.0}

	assert.Equal(t, gate.DecisionQueue, gate.Decide(action, nil))
	assert.Equal(t, gate.DecisionQueue, gate.Decide(action, map[string]config.AutoCommitRule{}))
}

// TestDecide_GraduatedFalseVetoesRegardlessOfConfidence is the second half of
// the safety property: even a fully-configured rule with a rock-bottom floor
// must queue, not apply, while Graduated is false — confidence 1.0 does not
// buy its way past a human's own read-only switch.
func TestDecide_GraduatedFalseVetoesRegardlessOfConfidence(t *testing.T) {
	action := store.ActionRow{Type: "comment", Confidence: 1.0}
	rules := map[string]config.AutoCommitRule{
		"comment": {ConfidenceFloor: 0, Graduated: false},
	}

	assert.Equal(t, gate.DecisionQueue, gate.Decide(action, rules))
}

func TestDecide_TableDriven(t *testing.T) {
	tests := []struct {
		name   string
		action store.ActionRow
		rules  map[string]config.AutoCommitRule
		want   gate.Decision
	}{
		{
			name:   "confidence above floor and graduated applies",
			action: store.ActionRow{Type: "comment", Confidence: 0.9},
			rules: map[string]config.AutoCommitRule{
				"comment": {ConfidenceFloor: 0.8, Graduated: true},
			},
			want: gate.DecisionApply,
		},
		{
			name:   "confidence exactly at floor applies (inclusive per spec)",
			action: store.ActionRow{Type: "comment", Confidence: 0.8},
			rules: map[string]config.AutoCommitRule{
				"comment": {ConfidenceFloor: 0.8, Graduated: true},
			},
			want: gate.DecisionApply,
		},
		{
			name:   "confidence just below floor queues",
			action: store.ActionRow{Type: "comment", Confidence: 0.79},
			rules: map[string]config.AutoCommitRule{
				"comment": {ConfidenceFloor: 0.8, Graduated: true},
			},
			want: gate.DecisionQueue,
		},
		{
			name:   "graduated false at maximum confidence still queues",
			action: store.ActionRow{Type: "transition", Confidence: 1.0},
			rules: map[string]config.AutoCommitRule{
				"transition": {ConfidenceFloor: 0.5, Graduated: false},
			},
			want: gate.DecisionQueue,
		},
		{
			name:   "action type absent from an otherwise-populated map queues",
			action: store.ActionRow{Type: "create", Confidence: 1.0},
			rules: map[string]config.AutoCommitRule{
				"comment": {ConfidenceFloor: 0.1, Graduated: true},
			},
			want: gate.DecisionQueue,
		},
		{
			name:   "a type outside gate's closed set still applies mechanically if configured",
			action: store.ActionRow{Type: "estimate", Confidence: 1.0},
			rules: map[string]config.AutoCommitRule{
				"estimate": {ConfidenceFloor: 0.1, Graduated: true},
			},
			// estimate has no TaskWriter method (tasktracker's doc comment
			// says so) and Applier.Apply below returns an error for it. But
			// Decide is a pure function of (action, config) per the spec, with
			// no knowledge of which types the applier can actually enact — it
			// applies the same map lookup regardless of key. The applier, not
			// Decide, is where "no method for this type" surfaces.
			want: gate.DecisionApply,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := gate.Decide(tt.action, tt.rules)
			assert.Equal(t, tt.want, got)
		})
	}
}
