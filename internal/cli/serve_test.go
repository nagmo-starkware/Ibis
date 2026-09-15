package cli

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/b-j-roberts/ibis/internal/api"
	"github.com/b-j-roberts/ibis/internal/config"
	"github.com/b-j-roberts/ibis/internal/engine"
	"github.com/b-j-roberts/ibis/internal/store/memory"
)

// testContractClassJSON is a minimal Sierra contract class with a single
// Transfer event, used as an explicit ABI file path so ABI resolution never
// needs RPC or scarb.
const testContractClassJSON = `{
	"abi": "[{\"type\":\"event\",\"name\":\"test::Transfer\",\"kind\":\"struct\",\"members\":[{\"name\":\"from\",\"type\":\"core::starknet::contract_address::ContractAddress\",\"kind\":\"key\"},{\"name\":\"to\",\"type\":\"core::starknet::contract_address::ContractAddress\",\"kind\":\"key\"},{\"name\":\"value\",\"type\":\"core::felt252\",\"kind\":\"data\"}]}]",
	"contract_class_version": "0.1.0",
	"sierra_program": []
}`

// TestRefreshDynamicContractsLoop_PicksUpNewContract verifies the periodic
// refresh loop started by `serve` notices a dynamic contract registered
// after the reader's initial setup and wires it into the running API server,
// so its route starts serving without a restart.
func TestRefreshDynamicContractsLoop_PicksUpNewContract(t *testing.T) {
	dir := t.TempDir()
	abiPath := filepath.Join(dir, "test.contract_class.json")
	if err := os.WriteFile(abiPath, []byte(testContractClassJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	st := memory.New()
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	eng := engine.New(&config.Config{}, st, nil, logger)
	if err := eng.SetupReadOnly(ctx); err != nil {
		t.Fatalf("SetupReadOnly: %v", err)
	}

	apiServer := api.New(&api.ServerConfig{
		Store:     st,
		Schemas:   eng.Schemas(),
		APIConfig: &config.APIConfig{Host: "127.0.0.1", Port: 0},
		Contracts: eng.AllContracts(),
		Logger:    logger,
	})

	ts := httptest.NewServer(apiServer.Handler())
	defer ts.Close()

	// Before the writer has registered the child, its route is unknown.
	resp, err := http.Get(ts.URL + "/v1/child/transfer")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 before registration, got %d", resp.StatusCode)
	}

	loopCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go refreshDynamicContractsLoop(loopCtx, eng, apiServer, logger, 20*time.Millisecond)

	// Simulate the indexing writer registering a new dynamic (factory child)
	// contract after this reader started.
	if err := st.SaveDynamicContract(ctx, &config.ContractConfig{
		Name:    "Child",
		Address: "0x2",
		ABI:     abiPath,
		Events: []config.EventConfig{
			{Name: "Transfer", Table: config.TableConfig{Type: "log"}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, err := http.Get(ts.URL + "/v1/child/transfer")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("route for dynamically registered contract never became available (last status %d)", resp.StatusCode)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRefreshDynamicContractsLoop_StopsOnContextCancel(t *testing.T) {
	st := memory.New()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	eng := engine.New(&config.Config{}, st, nil, logger)
	if err := eng.SetupReadOnly(context.Background()); err != nil {
		t.Fatalf("SetupReadOnly: %v", err)
	}
	apiServer := api.New(&api.ServerConfig{
		Store:     st,
		Schemas:   eng.Schemas(),
		APIConfig: &config.APIConfig{Host: "127.0.0.1", Port: 0},
		Contracts: eng.AllContracts(),
		Logger:    logger,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		refreshDynamicContractsLoop(ctx, eng, apiServer, logger, 5*time.Millisecond)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("refreshDynamicContractsLoop did not return after context cancellation")
	}
}
