package api_test

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/b-j-roberts/ibis/internal/api"
	"github.com/b-j-roberts/ibis/internal/config"
	"github.com/b-j-roberts/ibis/internal/store/memory"
	"github.com/b-j-roberts/ibis/internal/types"
)

// A server built before Engine.Setup() has no schemas to pass; SetSchemas
// publishes them once setup returns.
func TestSetSchemasPublishesAfterConstruction(t *testing.T) {
	srv := api.New(&api.ServerConfig{
		Store:     memory.New(),
		APIConfig: &config.APIConfig{Host: "localhost", Port: 8080},
		Logger:    slog.Default(),
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	if code := getStatus(t, ts, "/v1/MyToken/Transfer"); code != http.StatusNotFound {
		t.Errorf("expected 404 before SetSchemas, got %d", code)
	}

	srv.SetSchemas(
		[]*types.TableSchema{{
			Name: "mytoken_transfer", Contract: "MyToken", Event: "Transfer",
			TableType: types.TableTypeLog,
			Columns:   []types.Column{{Name: "block_number", Type: "uint64"}},
		}},
		[]config.ContractConfig{{Name: "MyToken", Address: "0x123"}},
	)

	if code := getStatus(t, ts, "/v1/MyToken/Transfer"); code != http.StatusOK {
		t.Errorf("expected 200 after SetSchemas, got %d", code)
	}
	if got := len(getJSON(t, ts, "/v1/status")["contracts"].([]any)); got != 1 {
		t.Errorf("expected 1 contract on /v1/status after SetSchemas, got %d", got)
	}
}
