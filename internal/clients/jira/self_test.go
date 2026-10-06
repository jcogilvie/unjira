package jira_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/clients/jira"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

var _ tasktracker.SelfIdentifier = (*jira.Tracker)(nil)

// TestTracker_SelfIdentity_IsTheAccountID: the identity the collector compares an
// author against, so unjira's own writes are tagged self-authored when they come back.
func TestTracker_SelfIdentity_IsTheAccountID(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Contains(t, r.URL.Path, "/myself")
		writeJSON(t, w, http.StatusOK, map[string]any{"accountId": "acct-unjira", "displayName": "unjira"})
	})

	got, err := jira.NewTracker(client).SelfIdentity()

	require.NoError(t, err)
	assert.Equal(t, "acct-unjira", got)
}

func TestTracker_SelfIdentity_EmptyAccountIDIsAnError(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{"displayName": "unjira"})
	})

	_, err := jira.NewTracker(client).SelfIdentity()

	require.ErrorContains(t, err, "accountId")
}
