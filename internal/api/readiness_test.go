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

// TestCatchupStatusAnswersWhileNotReady: a promote gate polls this endpoint to
// decide whether a standby slot may be flipped into production. It has to
// answer during startup -- a gate cannot tell "still catching up" from
// "unreachable" if the endpoint 503s -- and it must stay 200 while catch-up is
// incomplete, since that is a healthy state, not an error.
func TestCatchupStatusAnswersWhileNotReady(t *testing.T) {
	ts, srv := setupReadinessServer(t)
	srv.SetReady(false)

	if code := getStatus(t, ts, "/v1/catchup_status"); code != http.StatusOK {
		t.Fatalf("expected /v1/catchup_status to answer 200 while not ready, got %d", code)
	}

	body := getJSON(t, ts, "/v1/catchup_status")
	for _, k := range []string{"ready", "streams_live", "streams_total", "catchup_complete"} {
		if _, ok := body[k]; !ok {
			t.Errorf("catchup_status must expose %q - the promote gate reads it", k)
		}
	}
	if ready, _ := body["ready"].(bool); ready {
		t.Error("expected ready=false while not ready")
	}
	// No engine wired here, so no streams: that must read as NOT caught up
	// rather than trivially true, or a gate would pass on an idle instance.
	if done, _ := body["catchup_complete"].(bool); done {
		t.Error("catchup_complete must be false when no streams are running")
	}
}
