package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/b-j-roberts/ibis/internal/api"
	"github.com/b-j-roberts/ibis/internal/config"
	"github.com/b-j-roberts/ibis/internal/engine"
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
		p, err := loadCLIPrologue(cmd, func(out io.Writer, cfg *config.Config) {
			fmt.Fprintln(out, "  Mode:     read-only (serve)")
		})
		if err != nil {
			return err
		}
		cfg, logger, prov, st := p.cfg, p.logger, p.prov, p.store
		defer prov.Close()
		defer st.Close()

		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		eng := engine.New(cfg, st, prov, logger)
		bus := api.NewEventBus()

		// Read-only setup (see engine.SetupReadOnly): resolves ABIs and builds
		// table schemas without creating tables, running discovery, or
		// reconciling frozen contracts. Runs via the same bind-listener-first
		// sequence as `run` (startAPIServer), so data endpoints 503 rather
		// than block the listener until setup completes.
		apiServer, err := startAPIServer(ctx, cmd.OutOrStdout(), p, eng, bus, " (read-only)", eng.SetupReadOnly)
		if err != nil {
			return fmt.Errorf("engine read-only setup: %w", err)
		}

		// Periodically pick up dynamic contracts (e.g. factory children) the
		// indexing writer registers after this reader started. Started only
		// after startAPIServer returns (i.e. after SetSchemas), since
		// SetSchemas replaces the schema set wholesale and would otherwise
		// wipe a contract this loop had already added.
		go refreshDynamicContractsLoop(ctx, eng, apiServer, logger, serveRefreshInterval)

		<-ctx.Done()
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
