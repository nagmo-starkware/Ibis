package engine

import (
	"context"
	"fmt"

	"github.com/NethermindEth/juno/core/felt"

	"github.com/b-j-roberts/ibis/internal/abi"
	"github.com/b-j-roberts/ibis/internal/config"
	"github.com/b-j-roberts/ibis/internal/schema"
	"github.com/b-j-roberts/ibis/internal/types"
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

	vp := NewViewPoller(e.provider, e.store, e.logger)
	viewSchemas, err := vp.Setup(e.contracts)
	if err != nil {
		return fmt.Errorf("view poller setup: %w", err)
	}
	for _, vs := range viewSchemas {
		for _, cs := range e.contracts {
			if cs.config.Name == vs.Contract ||
				(cs.config.SharedTables && cs.config.FactoryName == vs.Contract) {
				cs.schemas[vs.Event] = vs
				break
			}
		}
	}
	e.poller = vp

	return nil
}

// RefreshDynamicContracts re-reads dynamic contracts from the store and
// registers, in memory only, any this engine has not seen yet: it resolves
// the new contract's ABI and builds its table schemas, but never persists
// anything to the store (the writer already did, when it first registered
// the contract) and never starts a subscription or view polling. It is the
// read-only reader's way of picking up contracts the writer registers after
// the reader's initial SetupReadOnly — the reader has no other signal that a
// new dynamic contract (e.g. a factory child) has appeared.
//
// Returns the newly seen contracts and, for each, the schemas built for it —
// the caller (the `serve` command) wires these into the running API server
// via Server.AddSchemas.
func (e *Engine) RefreshDynamicContracts(ctx context.Context) ([]*config.ContractConfig, [][]*types.TableSchema, error) {
	dynamicContracts, err := e.store.GetDynamicContracts(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("loading dynamic contracts: %w", err)
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	known := make(map[string]bool, len(e.contracts))
	for _, cs := range e.contracts {
		known[cs.config.Name] = true
	}

	resolver := config.NewABIResolver(e.provider)

	var newConfigs []*config.ContractConfig
	var newSchemas [][]*types.TableSchema

	for i := range dynamicContracts {
		dc := &dynamicContracts[i]
		if known[dc.Name] {
			continue
		}
		dc.Dynamic = true
		e.resyncDynamicChildConfig(dc)

		contractABI, err := resolver.Resolve(ctx, dc)
		if err != nil {
			return nil, nil, fmt.Errorf("resolve ABI for %s: %w", dc.Name, err)
		}
		registry := abi.NewEventRegistry(contractABI)

		var buildOpts *schema.BuildOptions
		if dc.SharedTables && dc.FactoryName != "" {
			buildOpts = &schema.BuildOptions{
				SharedTable: true,
				FactoryName: dc.FactoryName,
			}
		}
		schemas := schema.BuildSchemas(dc, contractABI, registry, buildOpts)

		address, err := new(felt.Felt).SetString(dc.Address)
		if err != nil {
			return nil, nil, fmt.Errorf("parsing address for %s: %w", dc.Name, err)
		}

		cs := &contractState{
			config:   *dc,
			address:  address,
			abi:      contractABI,
			registry: registry,
			schemas:  schemas,
		}
		e.contracts = append(e.contracts, cs)
		known[dc.Name] = true

		schemaList := make([]*types.TableSchema, 0, len(schemas))
		for _, s := range schemas {
			schemaList = append(schemaList, s)
		}

		newConfigs = append(newConfigs, dc)
		newSchemas = append(newSchemas, schemaList)
	}

	return newConfigs, newSchemas, nil
}
