package api_test

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/b-j-roberts/ibis/internal/api"
	"github.com/b-j-roberts/ibis/internal/config"
	"github.com/b-j-roberts/ibis/internal/engine"
	"github.com/b-j-roberts/ibis/internal/store/memory"
	"github.com/b-j-roberts/ibis/internal/types"
)

// An engine error that embeds an RPC URL never reaches the HTTP client with its key.
func TestAdminErrorMasksRPCURL(t *testing.T) {
	const name = "https://node.example/rpc/SECRETTOKEN"
	eng := engine.New(&config.Config{}, memory.New(), nil, slog.Default())
	eng.InjectContractForTest(&config.ContractConfig{Name: name, Address: "0x1"}, map[string]*types.TableSchema{})
	srv := api.New(&api.ServerConfig{
		Store:     memory.New(),
		APIConfig: &config.APIConfig{Host: "127.0.0.1", Port: 1},
		Engine:    eng,
		Logger:    slog.Default(),
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := `{"name":"` + name + `","address":"0x1"}`
	resp, err := http.Post(ts.URL+"/v1/admin/contracts", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusInternalServerError || !strings.Contains(string(got), "already registered") {
		t.Fatalf("want 500 already registered, got %d %s", resp.StatusCode, got)
	}
	if strings.Contains(string(got), "SECRETTOKEN") || !strings.Contains(string(got), "node.example") {
		t.Errorf("not masked: %s", got)
	}
}
