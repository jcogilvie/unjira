package pipeline_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/clients/jira"
	"github.com/jcogilvie/unjira/internal/gate"
	"github.com/jcogilvie/unjira/internal/pipeline"
	"github.com/jcogilvie/unjira/internal/store"
	"github.com/jcogilvie/unjira/internal/store/storetest"
)

// lossyJira holds worklogs in memory and answers every POST with a 500 after storing
// it: a POST that reached Jira but whose outcome the caller never learned.
type lossyJira struct {
	mu       sync.Mutex
	worklogs []map[string]any
	posts    int
}

func (j *lossyJira) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	j.mu.Lock()
	defer j.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if !strings.Contains(r.URL.Path, "/worklog") {
		http.NotFound(w, r)

		return
	}
	switch r.Method {
	case http.MethodGet:
		if err := json.NewEncoder(w).Encode(map[string]any{"worklogs": j.worklogs, "total": len(j.worklogs)}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	case http.MethodPost:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		j.posts++
		j.worklogs = append(j.worklogs, map[string]any{"id": strconv.Itoa(j.posts), "properties": body["properties"]})
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"errorMessages":["gateway timeout"]}`))
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func TestRunSinks_AddWhoseOutcomeWasLostIsNotLoggedTwice(t *testing.T) {
	s := openTokenStore(t)
	seedTracked(t, s, "PROJ-1")
	addMessages(t, s, "PROJ-1", storetest.UTC(12, 0, 0), "m1")
	cfg := sinkConfig(nil)
	opts := pipeline.SinkOptions{Sinks: worklogSinks(cfg), Now: sinkStart}
	fake := &lossyJira{}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	client, err := jira.New(srv.URL, "e", "t")
	require.NoError(t, err)
	applier := gate.NewApplier(s, jira.NewTracker(client), "PROJ", cfg.Trackers)

	r, err := pipeline.RunSinks(s, cfg, opts)
	require.NoError(t, err)
	require.Len(t, r.Persisted, 1)
	require.Error(t, applier.Apply(r.Persisted[0]), "the POST stored the worklog but answered 500")
	require.Equal(t, 1, fake.posts)

	addMessages(t, s, "PROJ-1", storetest.UTC(13, 0, 0), "m2")
	r, err = pipeline.RunSinks(s, cfg, opts)
	require.NoError(t, err)
	require.Len(t, r.Persisted, 1)
	assert.Equal(t, []string{"m1", "m2"}, payload(t, r.Persisted[0]).MessageIDs, "a new key over m1 again")

	err = applier.Apply(r.Persisted[0])
	require.Error(t, err)
	assert.Contains(t, err.Error(), "worklog 1 on jira issue PROJ-1 already logs some of these messages")
	assert.Equal(t, 1, fake.posts, "no second POST: m1's time is already on the issue")
	got, err := s.GetAction(r.Persisted[0].ID)
	require.NoError(t, err)
	assert.Equal(t, store.StatusFailed, got.Status)
}
