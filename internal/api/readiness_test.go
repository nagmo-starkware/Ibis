package api_test

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/b-j-roberts/ibis/internal/api"
	"github.com/b-j-roberts/ibis/internal/config"
	"github.com/b-j-roberts/ibis/internal/store/memory"
)

// setupTestServer doesn't return the *api.Server, and the gate needs no data.
func setupReadinessServer(t *testing.T) (*httptest.Server, *api.Server) {
	t.Helper()
	srv := api.New(&api.ServerConfig{
		Store:     memory.New(),
		APIConfig: &config.APIConfig{Host: "localhost", Port: 8080},
		Logger:    slog.Default(),
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, srv
}

func TestReadyByDefault(t *testing.T) {
	ts, _ := setupReadinessServer(t)
	// A server that never calls SetReady behaves exactly as before.
	if code := getStatus(t, ts, "/v1/Nope/Missing"); code == http.StatusServiceUnavailable {
		t.Error("a server that never called SetReady must not gate its data endpoints")
	}
	if ready, _ := getJSON(t, ts, "/v1/status")["ready"].(bool); !ready {
		t.Error("expected ready=true by default")
	}
}

func TestNotReadyGatesDataButNotHealthOrStatus(t *testing.T) {
	ts, srv := setupReadinessServer(t)
	srv.SetReady(false)

	if code := getStatus(t, ts, "/v1/Nope/Missing"); code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 on a data endpoint while not ready, got %d", code)
	}

	// Health and status keep answering: promote-ibis.sh can only tell "not
	// ready yet" from "unreachable" if /v1/status responds.
	if code := getStatus(t, ts, "/v1/health"); code != http.StatusOK {
		t.Errorf("expected /v1/health to answer while not ready, got %d", code)
	}
	status := getJSON(t, ts, "/v1/status")
	if ready, _ := status["ready"].(bool); ready {
		t.Error("expected ready=false while not ready")
	}
	if _, ok := status["indexed_block_number"]; !ok {
		t.Error("status must expose indexed_block_number — promote-ibis.sh reads it")
	}

	srv.SetReady(true)
	if code := getStatus(t, ts, "/v1/Nope/Missing"); code == http.StatusServiceUnavailable {
		t.Error("expected the gate to lift after SetReady(true)")
	}
}
