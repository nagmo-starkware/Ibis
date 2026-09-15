package cli

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/b-j-roberts/ibis/internal/api"
	"github.com/b-j-roberts/ibis/internal/config"
	"github.com/b-j-roberts/ibis/internal/engine"
)

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

		fmt.Fprintf(cmd.OutOrStdout(), "\nAPI server listening on %s:%d (read-only)\n", cfg.API.Host, cfg.API.Port)
		if err := apiServer.Start(ctx); err != nil {
			return fmt.Errorf("API server: %w", err)
		}

		return nil
	},
}
