package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/b-j-roberts/ibis/internal/api"
	"github.com/b-j-roberts/ibis/internal/engine"
)

// startAPIServer runs the shared "bind, then setup, then go ready" sequence
// used by both `run` and `serve`: the listener answers immediately (so a
// slow setup can't blow a platform startup deadline — see #40), data
// endpoints 503 until setup finishes, then the real schemas are published
// and the gate lifts. The listener is always backgrounded in its own
// goroutine; the caller owns waiting for it (or ctx) afterwards.
//
// setup is the caller's engine setup step (eng.Setup for `run`,
// eng.SetupReadOnly for `serve`). listeningSuffix is appended to the
// "API server listening" line (e.g. " (read-only)" for `serve`). On a setup
// error, the raw error is returned unwrapped so each caller can apply its
// own wrapping message.
func startAPIServer(ctx context.Context, out io.Writer, p *cliPrologue, eng *engine.Engine,
	bus *api.EventBus, listeningSuffix string, setup func(context.Context) error) (*api.Server, error) {
	apiServer := api.New(&api.ServerConfig{
		Store:     p.store,
		APIConfig: &p.cfg.API,
		Logger:    p.logger,
		EventBus:  bus,
		Engine:    eng,
	})
	// Data endpoints 503 until setup finishes, so a consumer gets a loud
	// failure rather than a silently empty result. /v1/health and
	// /v1/status keep answering.
	apiServer.SetReady(false)

	go func() {
		if err := apiServer.Start(ctx); err != nil {
			p.logger.Error("API server error", "error", err)
		}
	}()

	fmt.Fprintf(out, "\nAPI server listening on %s:%d%s\n", p.cfg.API.Host, p.cfg.API.Port, listeningSuffix)

	fmt.Fprintln(out, "Setting up engine...")
	if err := setup(ctx); err != nil {
		return nil, err
	}

	apiServer.SetSchemas(eng.Schemas(), eng.AllContracts())
	apiServer.SetReady(true)

	return apiServer, nil
}
