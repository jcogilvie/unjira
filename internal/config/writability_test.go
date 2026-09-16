package config_test

// writability_test.go covers ProjectWritability, the shared answer to "may unjira
// write to this project" — lifted out of gate.Applier so triage can ask the same
// question at review time.
//
// The finding: every write gate lived in gate.Applier, so a reviewer worked through
// an action, judged it, approved it, and only THEN learned it could not be applied.
// Measured on a real queue: 17 proposed actions targeting PAAS/RE/SUMO/SIA against a
// writable_project_keys of ["DEVSBX"] — every one unappliable, and nothing said so.
//
// The two refusals stay DISTINCT, which is the whole reason this is a typed result
// rather than a bool. gate/applier.go's own comment argues it: "not in
// writable_project_keys" is a scope decision whose remedy is a config edit, while
// "no connection tracks this project" means unjira drafted an action for work outside
// what it manages — a signal about the correlator's attribution, whose remedy is
// triage's [t]arget.

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/jcogilvie/unjira/internal/config"
)

func writabilityConfig() config.Config {
	return config.Config{Jira: []config.JiraConnection{{
		Name:                "dev",
		ProjectKeys:         []string{"PAAS", "DEVSBX"},
		WritableProjectKeys: []string{"DEVSBX"},
	}}}
}

func TestProjectWritability_Writable(t *testing.T) {
	got := writabilityConfig().ProjectWritability("DEVSBX")

	assert.True(t, got.Writable)
	assert.Equal(t, "dev", got.Connection)
	assert.Empty(t, got.Reason, "a writable project needs no explanation")
	assert.False(t, got.Untracked)
}

// TestProjectWritability_ReadableButNotWritable is the scope-decision case: unjira
// tracks the project, someone chose not to allow writes.
func TestProjectWritability_ReadableButNotWritable(t *testing.T) {
	got := writabilityConfig().ProjectWritability("PAAS")

	assert.False(t, got.Writable)
	assert.False(t, got.Untracked,
		"tracked-but-not-writable must be distinguishable from untracked: the remedies differ")
	assert.Equal(t, "dev", got.Connection,
		"and the connection must be named, since that is where the fix goes")
	assert.Contains(t, got.Reason, "writable_project_keys",
		"the reason must name the key to edit")
	assert.Contains(t, got.Reason, "dev")
}

// TestProjectWritability_Untracked is the attribution case. The old message sent a
// reader to edit writable_project_keys for connection "none" — advice impossible to
// follow.
func TestProjectWritability_Untracked(t *testing.T) {
	got := writabilityConfig().ProjectWritability("NOPE")

	assert.False(t, got.Writable)
	assert.True(t, got.Untracked,
		"an untracked project is a correlator-attribution signal, not a config gap")
	assert.Empty(t, got.Connection, "no connection covers it, so none can be named")
	assert.Contains(t, got.Reason, "retarget",
		"the remedy is triage's [t]arget, and the message must say so")
	assert.NotContains(t, got.Reason, "connection \"\"",
		"and it must never name an empty connection")
}

// TestProjectWritability_NoConnectionsConfigured is the fresh-clone case: a clone
// with no jira block writes nothing, and the message must still be actionable.
func TestProjectWritability_NoConnectionsConfigured(t *testing.T) {
	got := config.Config{}.ProjectWritability("PAAS")

	assert.False(t, got.Writable)
	assert.True(t, got.Untracked)
	assert.NotEmpty(t, got.Reason)
}

// TestProjectWritability_EmptyWritableListDeniesEverything pins the deny-by-default
// property the README states: absent means nothing on that connection is writable.
func TestProjectWritability_EmptyWritableListDeniesEverything(t *testing.T) {
	cfg := config.Config{Jira: []config.JiraConnection{{
		Name: "dev", ProjectKeys: []string{"PAAS"},
	}}}

	got := cfg.ProjectWritability("PAAS")

	assert.False(t, got.Writable,
		"an absent writable_project_keys must deny, not permit: a fresh clone applies nothing")
	assert.False(t, got.Untracked, "but the project IS tracked, so the remedy is still a config edit")
}

// TestProjectWritability_EmptyProjectIsNotWritable guards the degenerate input. An
// action with no resolvable project must not slip through as writable.
func TestProjectWritability_EmptyProjectIsNotWritable(t *testing.T) {
	got := writabilityConfig().ProjectWritability("")

	assert.False(t, got.Writable)
	assert.True(t, got.Untracked)
}
