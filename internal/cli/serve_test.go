package cli

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/b-j-roberts/ibis/internal/api"
	"github.com/b-j-roberts/ibis/internal/config"
	"github.com/b-j-roberts/ibis/internal/engine"
	"github.com/b-j-roberts/ibis/internal/store/memory"
)

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
