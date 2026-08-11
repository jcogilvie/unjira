// Package openai_test exercises the facade against a real HTTP server
// (httptest), rather than stubbing the SDK's internals — the SDK's own wire
// behavior is covered by its own tests; these tests cover the logic that is
// unjira's own: request construction, response extraction, error
// translation.
package openai_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jcogilvie/unjira/internal/clients/openai"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) *openai.Client {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	return openai.New(server.URL, "test-key", "gpt-5-2")
}

func TestNew_ReturnsUsableClient(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	require.NotNil(t, client)
}
