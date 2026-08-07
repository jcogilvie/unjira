package devtools_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/clients/jira"
	"github.com/jcogilvie/unjira/internal/devtools"
)

func TestSeed_CreatesLabeledIssuesAndReturnsKeys(t *testing.T) {
	var created int

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/rest/api/2/issue":
			created++

			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			fields, _ := body["fields"].(map[string]any)
			labels, _ := fields["labels"].([]any)
			require.Contains(t, labels, jira.SeedLabel)

			json.NewEncoder(w).Encode(map[string]any{"key": "P-" + strconv.Itoa(created)})
		case r.URL.Path == "/rest/api/2/issue/P-1/transitions" || r.URL.Path == "/rest/api/2/issue/P-2/transitions":
			if r.Method == http.MethodGet {
				json.NewEncoder(w).Encode(map[string]any{"transitions": []map[string]any{}})
			} else {
				w.WriteHeader(http.StatusNoContent)
			}
		default:
			if r.Method == http.MethodPost {
				w.WriteHeader(http.StatusCreated)
				json.NewEncoder(w).Encode(map[string]any{"id": "1"})
			} else {
				json.NewEncoder(w).Encode(map[string]any{"transitions": []map[string]any{}})
			}
		}
	}))
	defer server.Close()

	client, err := jira.New(server.URL, "e", "t")
	require.NoError(t, err)

	keys, err := devtools.Seed(client, "P", 2, devtools.WithSeed(11))

	require.NoError(t, err)
	assert.Equal(t, []string{"P-1", "P-2"}, keys)
	assert.Equal(t, 2, created)
}

func TestReset_DeletesEverySeedLabeledIssue(t *testing.T) {
	var deleted []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/rest/api/3/search/jql":
			json.NewEncoder(w).Encode(map[string]any{
				"issues": []map[string]any{{"key": "P-1"}, {"key": "P-2"}},
			})
		case r.Method == http.MethodDelete:
			deleted = append(deleted, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	client, err := jira.New(server.URL, "e", "t")
	require.NoError(t, err)

	keys, err := devtools.Reset(client, "P")

	require.NoError(t, err)
	assert.Equal(t, []string{"P-1", "P-2"}, keys)
	assert.Len(t, deleted, 2)
}
