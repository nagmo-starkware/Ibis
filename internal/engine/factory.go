package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/NethermindEth/juno/core/felt"

	"github.com/b-j-roberts/ibis/internal/abi"
	"github.com/b-j-roberts/ibis/internal/config"
	"github.com/b-j-roberts/ibis/internal/provider"
	"github.com/b-j-roberts/ibis/internal/schema"
	"github.com/b-j-roberts/ibis/internal/types"
)

// handleFactoryEvent processes a factory creation event for a specific factory entry.
// Each factory entry specifies which field in the event carries the child address.
// Called once per matching factory entry, so a single event can register multiple children.
func (e *Engine) handleFactoryEvent(ctx context.Context, cs *contractState, factory *config.FactoryConfig, decoded map[string]any, raw *provider.RawEvent) {
	// Extract child address from the decoded event.
	childAddrRaw, ok := decoded[factory.ChildAddressField]
	if !ok {
		e.logger.Error("factory event missing child address field",
			"factory", cs.config.Name,
			"field", factory.ChildAddressField,
		)
		return
	}
	childAddr := fmt.Sprint(childAddrRaw)

	// Normalize address to 0x-prefixed hex.
	if !strings.HasPrefix(childAddr, "0x") {
		childAddr = "0x" + childAddr
	}

	// Build child name.
	childName := buildChildName(cs.config.Name, factory, decoded, childAddr)

	// Check if child is already registered (e.g., from a previous run or restart).
	if e.isContractRegistered(childName) {
		e.logger.Debug("factory child already registered, skipping",
			"factory", cs.config.Name,
			"child", childName,
		)
		return
	}

	// Collect additional factory event fields as child metadata.
	meta := make(map[string]any)
	for k, v := range decoded {
		if k == factory.ChildAddressField || isMetadataColumn(k) {
			continue
		}
		meta[k] = v
	}

	// Build child contract config from factory template.
	childABI := factory.ChildABI
	if childABI == "" {
		childABI = "fetch"
	}

	cc := &config.ContractConfig{
		Name:         childName,
		Address:      childAddr,
		ABI:          childABI,
		Events:       factory.ChildEvents,
		Views:        factory.ChildViews,
		Freeze:       factory.ChildFreeze,
		StartBlock:   config.Uint64Ptr(raw.BlockNumber),
		FactoryName:  cs.config.Name,
		FactoryMeta:  meta,
		SharedTables: factory.SharedTables,
	}

	// Register the child using the fast path with cached ABI if available.
	if err := e.registerFactoryChild(ctx, cs, factory, cc); err != nil {
		e.logger.Error("failed to register factory child",
			"factory", cs.config.Name,
			"child", childName,
			"error", err,
		)
		return
	}

	e.logger.Info("registered factory child",
		"factory", cs.config.Name,
		"child", childName,
		"address", childAddr,
		"deploy_block", raw.BlockNumber,
	)

	// A child can be discovered after it is already dead (e.g. an option whose
	// expiry+grace passed before this run reached its deploy block). Freeze it at
	// registration so it never starts polling.
	e.evaluatePredicateContract(childName)
}

// registerFactoryChild registers a factory child contract. It uses a per-ChildABI
// cache so different factory entries (with different ChildABI values) each get their
// own resolved ABI without unnecessary network calls.
func (e *Engine) registerFactoryChild(ctx context.Context, factoryCS *contractState, factory *config.FactoryConfig, cc *config.ContractConfig) error {
	// Ensure the ABI cache map is initialized.
	if factoryCS.childABIs == nil {
		factoryCS.childABIs = make(map[string]*abi.ABI)
	}

	// Try to use cached child ABI for this factory's ChildABI type.
	childABI := factoryCS.childABIs[factory.ChildABI]
	if childABI != nil {
		if factory.SharedTables {
			return e.registerSharedChild(ctx, factoryCS, factory, cc, childABI)
		}
		return e.registerWithABI(ctx, cc, childABI)
	}

	// First child of this type: resolve ABI and cache it for subsequent children.
	resolver := config.NewABIResolver(e.provider)
	abis, err := resolver.ResolveAll(ctx, []config.ContractConfig{*cc})
	if err != nil {
		return fmt.Errorf("resolve ABI for factory child %s: %w", cc.Name, err)
	}

	childABI = abis[cc.Address]
	if childABI == nil {
		return fmt.Errorf("no ABI resolved for factory child %s (%s)", cc.Name, cc.Address)
	}

	// Cache for future children of the same type.
	factoryCS.childABIs[factory.ChildABI] = childABI

	if factory.SharedTables {
		return e.registerSharedChild(ctx, factoryCS, factory, cc, childABI)
	}
	return e.registerWithABI(ctx, cc, childABI)
}

// registerSharedChild registers a factory child that writes to shared tables.
// On the first child of a given factory type (keyed by factory.ChildABI), shared
// schemas are built and tables created. Subsequent children reuse cached schemas.
//
// Critical-section discipline: e.mu is held only for in-memory mutations
// (duplicate check, schema caching, e.contracts append). All store I/O,
// subscriber setup, and view poller startup run outside the lock so the
// event loop is not stalled behind a write-lock during slow DDL or DB inserts.
func (e *Engine) registerSharedChild(ctx context.Context, factoryCS *contractState, factory *config.FactoryConfig, cc *config.ContractConfig, childABI *abi.ABI) error {
	cc.Dynamic = true

	registry := abi.NewEventRegistry(childABI)

	// ── Critical section: in-memory mutations only ──────────────────────────
	var schemas map[string]*types.TableSchema
	var needsTableCreation bool

	e.mu.Lock()

	for _, cs := range e.contracts {
		if cs.config.Name == cc.Name {
			e.mu.Unlock()
			return fmt.Errorf("contract %q already registered", cc.Name)
		}
	}

	if factoryCS.sharedSchemas == nil {
		factoryCS.sharedSchemas = make(map[string]map[string]*types.TableSchema)
	}

	schemas = factoryCS.sharedSchemas[factory.ChildABI]
	if schemas == nil {
		// First child of this ABI type: build and cache schemas under lock so
		// a concurrent registration of the same type cannot race-build.
		opts := &schema.BuildOptions{
			SharedTable: true,
			FactoryName: factoryCS.config.Name,
		}
		schemas = schema.BuildSchemas(cc, childABI, registry, opts)
		factoryCS.sharedSchemas[factory.ChildABI] = schemas
		needsTableCreation = true
	}

	address, err := new(felt.Felt).SetString(cc.Address)
	if err != nil {
		e.mu.Unlock()
		return fmt.Errorf("parsing address for %s: %w", cc.Name, err)
	}

	cs := &contractState{
		config:   *cc,
		address:  address,
		abi:      childABI,
		registry: registry,
		schemas:  schemas, // All children of this type share the same schema references.
	}
	e.contracts = append(e.contracts, cs)

	e.mu.Unlock()
	// ── End critical section ─────────────────────────────────────────────────

	// ── I/O outside the lock ─────────────────────────────────────────────────

	var schemaList []*types.TableSchema

	if needsTableCreation {
		for _, sch := range schemas {
			if err := e.store.CreateTable(ctx, sch); err != nil {
				return fmt.Errorf("create shared table %s: %w", sch.Name, err)
			}
			e.logger.Info("created shared table",
				"name", sch.Name,
				"type", sch.TableType,
				"columns", len(sch.Columns),
				"factory", factoryCS.config.Name,
			)
			schemaList = append(schemaList, sch)
		}
	}

	// Best-effort: if persistence fails the engine will rediscover the child on
	// restart because its deploy event is still in the durable event log.
	if err := e.store.SaveDynamicContract(ctx, cc); err != nil {
		e.logger.Error("failed to persist factory child", "name", cc.Name, "error", err)
	}

	if e.subscriber != nil && e.runCtx != nil {
		sub := provider.ContractSubscription{
			Address:    address,
			StartBlock: derefUint64(cc.StartBlock),
		}

		if !hasWildcardEvent(cc) {
			var selectors []*felt.Felt
			for _, ec := range cc.Events {
				if ev := registry.MatchName(ec.Name); ev != nil {
					selectors = append(selectors, ev.Selector)
				}
			}
			if len(selectors) > 0 {
				sub.Keys = [][]*felt.Felt{selectors}
			}
		}

		e.subscriber.AddContract(e.runCtx, sub)
	}

	if e.runCtx != nil {
		viewSchemas, err := e.startViewsForContract(e.runCtx, cs)
		if err != nil {
			e.logger.Error("failed to start views for shared factory child",
				"contract", cc.Name, "error", err)
		} else {
			schemaList = append(schemaList, viewSchemas...)
		}
	}

	// Notify API server (schemas only for first child when tables were created).
	if e.onContractRegistered != nil {
		e.onContractRegistered(cc, schemaList)
	}

	return nil
}

// registerWithABI registers a contract using a pre-resolved ABI, skipping the
// ABI resolution step. This is the fast path for factory children (non-shared path).
//
// Same critical-section discipline as registerSharedChild: e.mu covers only
// in-memory mutations; all store I/O runs outside the lock.
func (e *Engine) registerWithABI(ctx context.Context, cc *config.ContractConfig, contractABI *abi.ABI) error {
	cc.Dynamic = true

	registry := abi.NewEventRegistry(contractABI)
	schemas := schema.BuildSchemas(cc, contractABI, registry, nil)

	// ── Critical section: in-memory mutations only ──────────────────────────
	e.mu.Lock()

	for _, cs := range e.contracts {
		if cs.config.Name == cc.Name {
			e.mu.Unlock()
			return fmt.Errorf("contract %q already registered", cc.Name)
		}
	}

	address, err := new(felt.Felt).SetString(cc.Address)
	if err != nil {
		e.mu.Unlock()
		return fmt.Errorf("parsing address for %s: %w", cc.Name, err)
	}

	cs := &contractState{
		config:   *cc,
		address:  address,
		abi:      contractABI,
		registry: registry,
		schemas:  schemas,
	}
	e.contracts = append(e.contracts, cs)

	e.mu.Unlock()
	// ── End critical section ─────────────────────────────────────────────────

	// ── I/O outside the lock ─────────────────────────────────────────────────

	var schemaList []*types.TableSchema
	for _, sch := range schemas {
		if err := e.store.CreateTable(ctx, sch); err != nil {
			return fmt.Errorf("create table %s: %w", sch.Name, err)
		}
		schemaList = append(schemaList, sch)
	}

	// Best-effort persistence; engine rediscovers children on restart.
	if err := e.store.SaveDynamicContract(ctx, cc); err != nil {
		e.logger.Error("failed to persist factory child", "name", cc.Name, "error", err)
	}

	if e.subscriber != nil && e.runCtx != nil {
		sub := provider.ContractSubscription{
			Address:    address,
			StartBlock: derefUint64(cc.StartBlock),
		}

		if !hasWildcardEvent(cc) {
			var selectors []*felt.Felt
			for _, ec := range cc.Events {
				if ev := registry.MatchName(ec.Name); ev != nil {
					selectors = append(selectors, ev.Selector)
				}
			}
			if len(selectors) > 0 {
				sub.Keys = [][]*felt.Felt{selectors}
			}
		}

		e.subscriber.AddContract(e.runCtx, sub)
	}

	if e.runCtx != nil {
		viewSchemas, err := e.startViewsForContract(e.runCtx, cs)
		if err != nil {
			e.logger.Error("failed to start views for factory child",
				"contract", cc.Name, "error", err)
		} else {
			schemaList = append(schemaList, viewSchemas...)
		}
	}

	if e.onContractRegistered != nil {
		e.onContractRegistered(cc, schemaList)
	}

	return nil
}

// isContractRegistered checks if a contract with the given name is already registered.
func (e *Engine) isContractRegistered(name string) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	for _, cs := range e.contracts {
		if cs.config.Name == name {
			return true
		}
	}
	return false
}

// buildChildName generates a name for a factory child contract.
// Uses the ChildNameTemplate if set, otherwise defaults to "{factory}_{short_address}".
func buildChildName(factoryName string, factory *config.FactoryConfig, decoded map[string]any, childAddr string) string {
	tmpl := factory.ChildNameTemplate
	if tmpl == "" {
		tmpl = "{factory}_{short_address}"
	}

	// Replace template variables.
	result := strings.ReplaceAll(tmpl, "{factory}", factoryName)

	// Short address: last 8 hex chars.
	shortAddr := strings.TrimPrefix(childAddr, "0x")
	if len(shortAddr) > 8 {
		shortAddr = shortAddr[len(shortAddr)-8:]
	}
	result = strings.ReplaceAll(result, "{short_address}", shortAddr)

	// Replace factory event field references.
	for k, v := range decoded {
		placeholder := "{" + k + "}"
		if strings.Contains(result, placeholder) {
			result = strings.ReplaceAll(result, placeholder, fmt.Sprint(v))
		}
	}

	return result
}

// isMetadataColumn returns true if the field is a standard metadata column
// added by the event processor (not an ABI-decoded field).
func isMetadataColumn(field string) bool {
	switch field {
	case "block_number", "log_index", "timestamp", "event_name",
		"contract_address", "contract_name", "transaction_hash", "status":
		return true
	}
	return false
}

// reorgFactoryChildren deregisters factory children whose deploy block falls
// within the reverted block range. Called during reorg handling.
func (e *Engine) reorgFactoryChildren(ctx context.Context, startBlock, endBlock uint64) {
	// Collect children to deregister (can't modify e.contracts while iterating).
	var toDeregister []string
	e.mu.RLock()
	for _, cs := range e.contracts {
		sb := derefUint64(cs.config.StartBlock)
		if cs.config.FactoryName != "" && sb >= startBlock && sb <= endBlock {
			toDeregister = append(toDeregister, cs.config.Name)
		}
	}
	e.mu.RUnlock()

	for _, name := range toDeregister {
		if err := e.DeregisterContract(ctx, name, true); err != nil {
			e.logger.Error("failed to deregister factory child during reorg",
				"child", name,
				"error", err,
			)
		} else {
			e.logger.Info("deregistered factory child during reorg",
				"child", name,
				"start_block", startBlock,
			)
		}
	}
}

// FactoryChildren returns information about all factory children for a given factory.
func (e *Engine) FactoryChildren(factoryName string) []ContractInfo {
	e.mu.RLock()
	defer e.mu.RUnlock()

	var children []ContractInfo
	for _, cs := range e.contracts {
		if cs.config.FactoryName == factoryName {
			cursor, _ := e.store.GetCursor(context.Background(), cs.config.Name)
			children = append(children, ContractInfo{
				Name:         cs.config.Name,
				Address:      cs.config.Address,
				Events:       len(cs.config.Events),
				CurrentBlock: cursor,
				StartBlock:   derefUint64(cs.config.StartBlock),
				Status:       ContractStatusActive,
				Dynamic:      true,
				FactoryName:  cs.config.FactoryName,
				FactoryMeta:  cs.config.FactoryMeta,
			})
		}
	}
	return children
}
