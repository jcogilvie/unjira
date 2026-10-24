//go:build live

package live

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/clients/jira"
	"github.com/jcogilvie/unjira/internal/tasktracker"
)

// worklogPropertyKey mirrors the jira package's unexported key (R11).
const worklogPropertyKey = "unjira.attribution"

// liveGet issues a raw authenticated GET for the probes the client has no
// method for (configuration, mypermissions), decoding JSON into result.
func liveGet(t *testing.T, path string, result any) {
	t.Helper()

	site := os.Getenv("UNJIRA_JIRA_SITE")
	if site == "" {
		site = "https://unjira.atlassian.net"
	}

	cred := testCredential(t)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, strings.TrimRight(site, "/")+"/"+path, nil)
	require.NoError(t, err)
	req.SetBasicAuth(cred.Email, cred.Token)
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, "GET %s: %s", path, body)
	require.NoError(t, json.Unmarshal(body, result))
}

func findWorklog(t *testing.T, worklogs []jira.Worklog, id string) (jira.Worklog, bool) {
	t.Helper()

	for _, wl := range worklogs {
		if wl.ID == id {
			return wl, true
		}
	}

	return jira.Worklog{}, false
}

func worklogProperty(t *testing.T, wl jira.Worklog) map[string]any {
	t.Helper()

	for _, p := range wl.Properties {
		if p.Key == worklogPropertyKey {
			var v map[string]any
			require.NoError(t, json.Unmarshal(p.Value, &v))

			return v
		}
	}

	t.Fatalf("worklog %s carries no %s property: %+v", wl.ID, worklogPropertyKey, wl.Properties)

	return nil
}

// TestLiveWorklogLifecycle measures the three facts the worklog sink rests on
// (time tracking, permissions, minimum duration) and drives one worklog through
// add, find, update and delete, including R10's re-applied delete.
func TestLiveWorklogLifecycle(t *testing.T) {
	client := testClient(t)
	project := testProject()
	tracker := jira.NewTracker(client)

	// 1. Time tracking.
	var cfg struct {
		TimeTrackingEnabled bool `json:"timeTrackingEnabled"`
	}
	liveGet(t, "rest/api/2/configuration", &cfg)
	t.Logf("time tracking enabled: %v", cfg.TimeTrackingEnabled)

	// 2. Permissions.
	var perms struct {
		Permissions map[string]struct {
			HavePermission bool `json:"havePermission"`
		} `json:"permissions"`
	}
	liveGet(t, "rest/api/2/mypermissions?permissions=WORK_ON_ISSUES,EDIT_OWN_WORKLOGS,DELETE_OWN_WORKLOGS&projectKey="+project, &perms)
	for _, name := range []string{"WORK_ON_ISSUES", "EDIT_OWN_WORKLOGS", "DELETE_OWN_WORKLOGS"} {
		p, ok := perms.Permissions[name]
		t.Logf("permission %s: present=%v havePermission=%v", name, ok, p.HavePermission)
	}

	// 3. Seed issue, and the minimum accepted duration.
	key, err := client.CreateIssue(
		project,
		"[seed] live worklog lifecycle probe",
		"Task",
		"Created by internal/live; deleted by this test's cleanup.",
		[]string{jira.SeedLabel},
	)
	require.NoError(t, err)
	deleteIssueOnCleanup(t, client, key)

	started := time.Now().Add(-time.Hour)
	minimum := 0
	for _, seconds := range []int{1, 30, 60} {
		id, err := client.AddWorklog(key, jira.WorklogInput{Started: started, TimeSpentSeconds: seconds, Comment: "unjira live probe"})
		if err != nil {
			t.Logf("minimum probe %ds: rejected: %v", seconds, err)
			continue
		}

		t.Logf("minimum probe %ds: accepted (id %s)", seconds, id)
		require.NoError(t, client.DeleteWorklog(key, id))

		if minimum == 0 {
			minimum = seconds
		}
	}
	require.NotZero(t, minimum, "no probe duration was accepted")
	t.Logf("measured minimum worklog duration: %ds", minimum)

	// 4. Add with a property; list returns it; the tracker finds it.
	idem := fmt.Sprintf("live-%d", time.Now().UnixNano())
	entry := tasktracker.WorklogEntry{
		Started:          started,
		TimeSpentSeconds: 600,
		Comment:          "unjira live lifecycle",
		Property:         json.RawMessage(fmt.Sprintf(`{"version":1,"idempotency_key":%q,"message_ids":["live-m1","live-m2"],"step":"add"}`, idem)),
	}
	id, err := tracker.AddWorklog(key, entry)
	require.NoError(t, err)
	require.NotEmpty(t, id)

	worklogs, err := client.ListWorklogs(key)
	require.NoError(t, err)
	wl, ok := findWorklog(t, worklogs, id)
	require.True(t, ok, "added worklog %s not listed", id)
	prop := worklogProperty(t, wl)
	assert.Equal(t, idem, prop["idempotency_key"])
	assert.Equal(t, "add", prop["step"])

	foundID, found, err := tracker.FindWorklog(key, idem, nil)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, id, foundID)

	// F1's overlap refusal: a different message list sharing live-m2 with the
	// stored worklog must error before any POST, against a real worklog.
	_, _, err = tracker.FindWorklog(key, "another-unjira-run", []string{"live-m2", "live-x"})
	require.Error(t, err, "an overlapping message list must be refused against the stored worklog")

	// 5. Update to 300s with a new property; both are replaced. ListWorklogs
	// does not decode seconds, so read them from the raw worklog.
	entry.TimeSpentSeconds = 300
	entry.Property = json.RawMessage(fmt.Sprintf(`{"version":1,"idempotency_key":%q,"step":"update"}`, idem))
	require.NoError(t, tracker.UpdateWorklog(key, id, entry))

	worklogs, err = client.ListWorklogs(key)
	require.NoError(t, err)
	wl, ok = findWorklog(t, worklogs, id)
	require.True(t, ok, "updated worklog %s not listed", id)
	prop = worklogProperty(t, wl)
	assert.Equal(t, "update", prop["step"], "PUT did not replace the property: re-plan R11")
	assert.Equal(t, idem, prop["idempotency_key"])

	var raw struct {
		TimeSpentSeconds int `json:"timeSpentSeconds"`
	}
	liveGet(t, fmt.Sprintf("rest/api/2/issue/%s/worklog/%s", key, id), &raw)
	assert.Equal(t, 300, raw.TimeSpentSeconds)

	// 6. Delete; it is gone; a second delete returns nil (R10).
	require.NoError(t, tracker.DeleteWorklog(key, id))

	worklogs, err = client.ListWorklogs(key)
	require.NoError(t, err)
	_, ok = findWorklog(t, worklogs, id)
	assert.False(t, ok, "deleted worklog %s still listed", id)

	assert.NoError(t, tracker.DeleteWorklog(key, id), "a re-applied delete of an absent worklog must succeed (R10)")
}
