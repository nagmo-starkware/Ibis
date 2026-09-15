package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/b-j-roberts/ibis/internal/api"
	"github.com/b-j-roberts/ibis/internal/config"
	"github.com/b-j-roberts/ibis/internal/engine"
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
		p, err := loadCLIPrologue(cmd, func(out io.Writer, cfg *config.Config) {
			fmt.Fprintf(out, "  Contracts: %d\n", len(cfg.Contracts))
			for _, c := range cfg.Contracts {
				fmt.Fprintf(out, "    - %s (%s): %d events\n", c.Name, c.Address, len(c.Events))
			}
		})
		if err != nil {
			return err
		}
		cfg, logger, prov, st := p.cfg, p.logger, p.prov, p.store
		defer prov.Close()
		defer st.Close()

		// IBIS_TRANSPORT overrides indexer.transport at runtime (e.g. "firehose"),
		// so the transport can be flipped per-deployment via an env var — no shared
		// config edit or image rebuild. This is the firehose A/B enable switch and
		// the instant rollback lever (unset/change the env var + redeploy).
		if t := os.Getenv("IBIS_TRANSPORT"); t != "" {
			cfg.Indexer.Transport = t
		}
		// IBIS_SHARED_TIP_POLLER enables the shared chain-tip poller independently of
		// transport (same flip/rollback ergonomics as IBIS_TRANSPORT).
		if v := os.Getenv("IBIS_SHARED_TIP_POLLER"); v != "" {
			cfg.Indexer.SharedTipPoller = v == "1" || v == "true"
		}

		// Create and run engine with signal handling.
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		eng := engine.New(cfg, st, prov, logger)

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
		// Schemas are empty here on purpose: the listener binds BEFORE
		// Engine.Setup() so a slow setup can't blow a platform startup deadline
		// (Cloud Run enforces ~600s and no probe setting extends it). The real
		// schemas are published by SetSchemas once setup returns.
		apiServer := api.New(&api.ServerConfig{
			Store:     st,
			APIConfig: &cfg.API,
			Logger:    logger,
			EventBus:  bus,
			Engine:    eng,
		})
		// Data endpoints 503 until setup finishes, so a consumer gets a loud
		// failure rather than a silently empty result. /v1/health and
		// /v1/status keep answering.
		apiServer.SetReady(false)

		go func() {
			if err := apiServer.Start(ctx); err != nil {
				logger.Error("API server error", "error", err)
			}
		}()

		fmt.Fprintf(cmd.OutOrStdout(), "\nAPI server listening on %s:%d\n", cfg.API.Host, cfg.API.Port)

		// Setup engine (resolve ABIs, build schemas, create tables). Runs with
		// the listener already bound. Still synchronous: an error here returns
		// and exits the process, so a broken instance dies rather than serving
		// 503s forever.
		fmt.Fprintln(cmd.OutOrStdout(), "Setting up engine...")
		if err := eng.Setup(ctx); err != nil {
			return fmt.Errorf("engine setup: %w", err)
		}

		apiServer.SetSchemas(eng.Schemas(), eng.AllContracts())
		apiServer.SetReady(true)

		// Wire engine callbacks AFTER SetSchemas: it replaces the schema set
		// wholesale, so a contract registered mid-setup would be wiped. Same
		// order relative to Setup as before this reorder.
		eng.SetOnContractRegistered(func(cc *config.ContractConfig, schemas []*types.TableSchema) {
			apiServer.AddSchemas(cc, schemas)
		})
		eng.SetOnContractDeregistered(func(name string) {
			apiServer.RemoveSchemas(name)
		})

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
