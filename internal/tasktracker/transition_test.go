package tasktracker_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// realPAASTransitions is the shape a live GET /transitions returns for a ticket
// mid-flight in the PAAS project, mined 2026-09-01 with `dev workflow --project
// PAAS` over 200 issues. Every one of these lands in the SAME status category.
//
// This fixture is the whole argument for named targets, so it is real data
// rather than invented names: under tasktracker.StatusCategory these five
// distinct destinations were one value, and "move to In Review" was literally
// the same request as "move to Blocked".
var realPAASTransitions = []tasktracker.Transition{
	{ToStatus: "In Progress", ToCategory: tasktracker.StatusInProgress},
	{ToStatus: "In Review", ToCategory: tasktracker.StatusInProgress},
	{ToStatus: "Blocked", ToCategory: tasktracker.StatusInProgress},
	{ToStatus: "In Test", ToCategory: tasktracker.StatusInProgress},
	{ToStatus: "Waiting for customer", ToCategory: tasktracker.StatusInProgress},
}

// TestTransition_DistinguishesTargetsSharingACategory is the regression guard
// for the entire named-status change, and it fails by construction under the
// old category-only type: there is no assertion you could write against
// []StatusCategory that separates "In Review" from "Blocked", because
// deduplicating those five transitions by category yields exactly one element.
//
// If this test is ever weakened to compare categories, the type has regressed to
// the projection that could not express either transition unjira exists to
// propose.
func TestTransition_DistinguishesTargetsSharingACategory(t *testing.T) {
	byName := make(map[string]tasktracker.Transition, len(realPAASTransitions))
	categories := make(map[tasktracker.StatusCategory]bool)

	for _, transition := range realPAASTransitions {
		byName[transition.ToStatus] = transition
		categories[transition.ToCategory] = true
	}

	require.Len(t, categories, 1,
		"precondition: these five real destinations collapse to ONE category")
	assert.Len(t, byName, 5,
		"all five must remain distinguishable by name, or a transition target is unstatable")

	// The specific pair from the spec: proposing one must never authorize the
	// other.
	assert.NotEqual(t, byName["In Review"].ToStatus, byName["Blocked"].ToStatus)
	assert.Equal(t, byName["In Review"].ToCategory, byName["Blocked"].ToCategory,
		"and they genuinely do share a category — that is why the category cannot be the target")
}

// TestTransition_RetainsCategoryForDirectionChecks: the category is not deleted,
// only demoted. floorConfidence still wants a cheap "is this a suspicious
// reopen" check, which needs the coarse bucket — a done -> new move is worth
// noticing regardless of what the two statuses are named.
func TestTransition_RetainsCategoryForDirectionChecks(t *testing.T) {
	reopen := tasktracker.Transition{ToStatus: "Backlog", ToCategory: tasktracker.StatusTodo}

	assert.Equal(t, tasktracker.StatusTodo, reopen.ToCategory,
		"a named target must still carry its bucket, or direction checks lose their input")
}
