package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/b-j-roberts/ibis/internal/api"
	"github.com/b-j-roberts/ibis/internal/config"
	"github.com/b-j-roberts/ibis/internal/engine"
	"github.com/b-j-roberts/ibis/internal/provider"
	"github.com/b-j-roberts/ibis/internal/store"
	"github.com/b-j-roberts/ibis/internal/store/badger"
	"github.com/b-j-roberts/ibis/internal/store/memory"
	"github.com/b-j-roberts/ibis/internal/store/postgres"
	"github.com/b-j-roberts/ibis/internal/types"
)

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Start the indexer with the given config",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(cfgPath)
		if err != nil {
			return err
		}

		// IBIS_TRANSPORT overrides indexer.transport at runtime (e.g. "firehose"),
		// so the transport can be flipped per-deployment via an env var — no shared
		// config edit or image rebuild. This is the firehose A/B enable switch and
		// the instant rollback lever (unset/change the env var + redeploy).
		if t := os.Getenv("IBIS_TRANSPORT"); t != "" {
			cfg.Indexer.Transport = t
		}

		logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelInfo,
		}))

		fmt.Fprintf(cmd.OutOrStdout(), "Loaded config from %s\n", cfgPath)
		fmt.Fprintf(cmd.OutOrStdout(), "  Network:  %s\n", cfg.Network)
		fmt.Fprintf(cmd.OutOrStdout(), "  RPC:      %s\n", cfg.RPC)
		fmt.Fprintf(cmd.OutOrStdout(), "  Backend:  %s\n", cfg.Database.Backend)
		fmt.Fprintf(cmd.OutOrStdout(), "  API:      %s:%d\n", cfg.API.Host, cfg.API.Port)
		fmt.Fprintf(cmd.OutOrStdout(), "  Contracts: %d\n", len(cfg.Contracts))
		for _, c := range cfg.Contracts {
			fmt.Fprintf(cmd.OutOrStdout(), "    - %s (%s): %d events\n", c.Name, c.Address, len(c.Events))
		}

		// Create Starknet provider.
		ctx := cmd.Context()
		prov, err := provider.New(ctx, cfg.RPC, logger)
		if err != nil {
			return fmt.Errorf("creating provider: %w", err)
		}
		defer prov.Close()

		// Create store backend.
		st, err := createStore(cfg, logger)
		if err != nil {
			return fmt.Errorf("creating store: %w", err)
		}
		defer st.Close()

		// Create and run engine with signal handling.
		ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer stop()

		eng := engine.New(cfg, st, prov, logger)

		// Setup engine (resolve ABIs, build schemas, create tables).
		if err := eng.Setup(ctx); err != nil {
			return fmt.Errorf("engine setup: %w", err)
		}

		// Create event bus for SSE streaming.
		bus := api.NewEventBus()
		eng.SetOnEvent(func(contract, event, table string, blockNumber, logIndex uint64, data map[string]any) {
			bus.Publish(api.StreamEvent{
				Table:       table,
				Contract:    contract,
				Event:       event,
				BlockNumber: blockNumber,
				LogIndex:    logIndex,
				Data:        data,
			})
		})

		// Start API server in background, with engine reference for dynamic contract management.
		apiServer := api.New(&api.ServerConfig{
			Store:     st,
			Schemas:   eng.Schemas(),
			APIConfig: &cfg.API,
			Contracts: eng.AllContracts(),
			Logger:    logger,
			EventBus:  bus,
			Engine:    eng,
		})

		// Wire engine callbacks to API server for dynamic registration.
		eng.SetOnContractRegistered(func(cc *config.ContractConfig, schemas []*types.TableSchema) {
			apiServer.AddSchemas(cc, schemas)
		})
		eng.SetOnContractDeregistered(func(name string) {
			apiServer.RemoveSchemas(name)
		})

		go func() {
			if err := apiServer.Start(ctx); err != nil {
				logger.Error("API server error", "error", err)
			}
		}()

		fmt.Fprintf(cmd.OutOrStdout(), "\nAPI server listening on %s:%d\n", cfg.API.Host, cfg.API.Port)
		fmt.Fprintln(cmd.OutOrStdout(), "Starting indexer...")
		if err := eng.Run(ctx); err != nil {
			return fmt.Errorf("engine: %w", err)
		}

		return nil
	},
}

// createStore initializes the appropriate store backend from config.
func createStore(cfg *config.Config, logger *slog.Logger) (store.Store, error) {
	switch cfg.Database.Backend {
	case "memory":
		logger.Info("using in-memory store")
		return memory.New(), nil
	case "badger":
		path := cfg.Database.Badger.Path
		if path == "" {
			path = "./data/ibis"
		}
		logger.Info("using BadgerDB store", "path", path)
		return badger.New(path)
	case "postgres":
		logger.Info("using PostgreSQL store",
			"host", cfg.Database.Postgres.Host,
			"port", cfg.Database.Postgres.Port,
			"database", cfg.Database.Postgres.Name,
		)
		ctx := context.Background()
		return postgres.New(ctx, cfg.Database.Postgres)
	default:
		return nil, fmt.Errorf("unknown database backend: %s", cfg.Database.Backend)
	}
}
