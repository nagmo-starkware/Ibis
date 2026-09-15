package cli

import (
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/b-j-roberts/ibis/internal/config"
	"github.com/b-j-roberts/ibis/internal/provider"
	"github.com/b-j-roberts/ibis/internal/store"
)

// cliPrologue holds the config/logger/provider/store setup shared by `run` and `serve`.
type cliPrologue struct {
	cfg    *config.Config
	logger *slog.Logger
	prov   *provider.StarknetProvider
	store  store.Store
}

// loadCLIPrologue loads the config, logger, provider and store shared by
// `run`/`serve`, printing the common banner then extraBanner's own line(s).
func loadCLIPrologue(cmd *cobra.Command, extraBanner func(out io.Writer, cfg *config.Config)) (*cliPrologue, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Loaded config from %s\n", cfgPath)
	fmt.Fprintf(out, "  Network:  %s\n", cfg.Network)
	fmt.Fprintf(out, "  RPC:      %s\n", cfg.RPC)
	fmt.Fprintf(out, "  Backend:  %s\n", cfg.Database.Backend)
	fmt.Fprintf(out, "  API:      %s:%d\n", cfg.API.Host, cfg.API.Port)
	extraBanner(out, cfg)

	prov, err := provider.New(cmd.Context(), cfg.RPC, logger)
	if err != nil {
		return nil, fmt.Errorf("creating provider: %w", err)
	}

	st, err := createStore(cfg, logger)
	if err != nil {
		prov.Close()
		return nil, fmt.Errorf("creating store: %w", err)
	}

	return &cliPrologue{cfg: cfg, logger: logger, prov: prov, store: st}, nil
}
