package engine

import (
	"context"
	"fmt"
)

// SetupReadOnly resolves ABIs and builds table schemas for a read-only API
// reader (see the `serve` CLI command, which serves the REST API from
// Postgres without indexing). Unlike Setup, it never creates tables,
// initializes contract discovery, or reconciles frozen contracts — those
// remain the indexing writer's responsibility, and the reader is expected to
// connect to the database as a read-only user. Call RefreshDynamicContracts
// afterwards (on a timer) to pick up dynamic contracts the writer registers
// later.
func (e *Engine) SetupReadOnly(ctx context.Context) error {
	if e.setupDone {
		return nil
	}
	if err := e.setupReadOnly(ctx); err != nil {
		return fmt.Errorf("engine read-only setup: %w", err)
	}
	e.setupDone = true
	return nil
}

// setupReadOnly builds the contract state a reader needs to serve the REST
// API — resolved ABIs and table schemas, including view-backed tables — but
// skips discovery, table creation, and freeze reconciliation. The view poller
// schemas are built (so view routes are served) but never run: Run() is
// never called in read-only mode, so no starknet_call polling starts.
func (e *Engine) setupReadOnly(ctx context.Context) error {
	if err := e.buildContractStates(ctx, false); err != nil {
		return err
	}

	if err := e.setupViewPoller(ctx, false); err != nil {
		return err
	}

	return nil
}
