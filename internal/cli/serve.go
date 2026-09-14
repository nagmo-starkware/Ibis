package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/b-j-roberts/ibis/internal/api"
	"github.com/b-j-roberts/ibis/internal/config"
	"github.com/b-j-roberts/ibis/internal/engine"
	"github.com/b-j-roberts/ibis/internal/provider"
)

// defaultServeRefreshInterval is how often `serve` re-reads dynamic contracts
// from the store to notice ones the indexing writer registered after this
// reader started (e.g. a new factory child, which appears roughly every 12
// minutes in production).
const defaultServeRefreshInterval = 30 * time.Second

var serveRefreshInterval time.Duration

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Serve the read-only REST API from an existing database, without indexing",
	Long: `serve starts only the REST API server, reading from a database that a
separate "ibis run" process is indexing into. It never indexes and never
writes to the database: no contract discovery, no event subscriptions, no
view-function polling, no table creation.

This splits read-serving from indexing so a scalable reader fleet can absorb
API traffic without multiplying RPC load on the upstream node — with "ibis
run", every autoscaled instance is a full indexer, so read traffic scaling
the instance count scales RPC load by the same factor.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(cfgPath)
		if err != nil {
			return err
		}

		logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelInfo,
		}))

		fmt.Fprintf(cmd.OutOrStdout(), "Loaded config from %s\n", cfgPath)
		fmt.Fprintf(cmd.OutOrStdout(), "  Network:  %s\n", cfg.Network)
		fmt.Fprintf(cmd.OutOrStdout(), "  RPC:      %s\n", cfg.RPC)
		fmt.Fprintf(cmd.OutOrStdout(), "  Backend:  %s\n", cfg.Database.Backend)
		fmt.Fprintf(cmd.OutOrStdout(), "  API:      %s:%d\n", cfg.API.Host, cfg.API.Port)
		fmt.Fprintln(cmd.OutOrStdout(), "  Mode:     read-only (serve)")

		// Create Starknet provider. Only needed for ABI resolution when a
		// contract's `abi` config is "fetch" (chain fetch via RPC); local
		// ABI files/contract names need no RPC. Still required here because
		// engine.New takes a concrete provider and RegisterContract-adjacent
		// paths expect one.
		ctx := cmd.Context()
		prov, err := provider.New(ctx, cfg.RPC, logger)
		if err != nil {
			return fmt.Errorf("creating provider: %w", err)
		}
		defer prov.Close()

		// Create store backend. In production this should be configured
		// with read-only database credentials.
		st, err := createStore(cfg, logger)
		if err != nil {
			return fmt.Errorf("creating store: %w", err)
		}
		defer st.Close()

		ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer stop()

		eng := engine.New(cfg, st, prov, logger)

		// Read-only setup: resolves ABIs and builds table schemas without
		// creating tables, running discovery, or reconciling frozen
		// contracts — see engine.SetupReadOnly.
		if err := eng.SetupReadOnly(ctx); err != nil {
			return fmt.Errorf("engine read-only setup: %w", err)
		}

		apiServer := api.New(&api.ServerConfig{
			Store:     st,
			Schemas:   eng.Schemas(),
			APIConfig: &cfg.API,
			Contracts: eng.AllContracts(),
			Logger:    logger,
			EventBus:  api.NewEventBus(),
			Engine:    eng,
		})

		// Periodically pick up dynamic contracts (e.g. factory children) the
		// indexing writer registers after this reader started.
		go refreshDynamicContractsLoop(ctx, eng, apiServer, logger, serveRefreshInterval)

		fmt.Fprintf(cmd.OutOrStdout(), "\nAPI server listening on %s:%d (read-only)\n", cfg.API.Host, cfg.API.Port)
		if err := apiServer.Start(ctx); err != nil {
			return fmt.Errorf("API server: %w", err)
		}

		return nil
	},
}

func init() {
	serveCmd.Flags().DurationVar(&serveRefreshInterval, "refresh-interval", defaultServeRefreshInterval,
		"how often to re-check the store for dynamic contracts registered by the indexing writer")
}

// refreshDynamicContractsLoop periodically calls eng.RefreshDynamicContracts
// and wires any newly discovered contracts into the running API server. A
// store error is logged and retried on the next tick — it never crashes the
// reader, since a transient DB hiccup shouldn't take down read serving.
func refreshDynamicContractsLoop(ctx context.Context, eng *engine.Engine, apiServer *api.Server, logger *slog.Logger, interval time.Duration) {
	if interval <= 0 {
		interval = defaultServeRefreshInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			configs, schemas, err := eng.RefreshDynamicContracts(ctx)
			if err != nil {
				logger.Error("dynamic contract refresh failed", "error", err)
				continue
			}
			for i, cc := range configs {
				apiServer.AddSchemas(cc, schemas[i])
				logger.Info("registered new dynamic contract", "name", cc.Name, "address", cc.Address)
			}
		}
	}
}
