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

// UDCAddress is the Universal Deployer Contract address on mainnet/Sepolia.
const UDCAddress = "0x04a64cd09a853868621d94cae9952b106f2c36a3f81260f85de6696c6b050221"

// discoveryCursorName is the cursor key used to persist UDC scan progress.
const discoveryCursorName = "_ibis_discover"

// udcVersion represents a resolved UDC event format.
type udcVersion int

const (
	udcVersionV1 udcVersion = iota // modern Cairo: keys[1]=address, data[0]=classHash
	udcVersionV0                   // Cairo 0: data[0]=address, data[3]=classHash
)

// udcLayout describes where to find the deployed address and class hash in a UDC event.
type udcLayout struct {
	version       udcVersion
	addressInKeys bool // true = address is in keys, false = in data
	addressIndex  int
	hashInKeys    bool // true = class hash is in keys, false = in data
	hashIndex     int
}

// v1 and v0 default layouts.
var (
	udcLayoutV1 = udcLayout{version: udcVersionV1, addressInKeys: true, addressIndex: 1, hashInKeys: false, hashIndex: 0}
	udcLayoutV0 = udcLayout{version: udcVersionV0, addressInKeys: false, addressIndex: 0, hashInKeys: false, hashIndex: 3}
)

// discoveryState holds pre-computed values for class-hash-based contract discovery.
type discoveryState struct {
	udcAddress  *felt.Felt
	udcSelector *felt.Felt                           // sn_keccak("ContractDeployed")
	classHashes map[felt.Felt]*config.DiscoverConfig // class hash felt -> discover config
	cachedABI   map[string]*abi.ABI                  // class hash hex string -> resolved ABI
	udcFormat   *config.UDCEventFormat               // configured format (may be nil for auto)

	// sharedSchemas caches shared table schemas for discovered contracts with shared_tables: true.
	// Keyed by class hash hex string. First discovery builds schemas and creates tables;
	// subsequent discoveries reuse the cached schemas.
	sharedSchemas map[string]map[string]*types.TableSchema
}

// setupDiscovery initializes discovery state from config. Called during engine setup.
func (e *Engine) setupDiscovery() error {
	if len(e.cfg.Discover) == 0 {
		return nil
	}

	udcAddr, err := new(felt.Felt).SetString(e.cfg.Indexer.UDCAddress)
	if err != nil {
		return fmt.Errorf("parsing UDC address: %w", err)
	}

	selector := abi.ComputeSelector("ContractDeployed")

	classHashes := make(map[felt.Felt]*config.DiscoverConfig, len(e.cfg.Discover))
	for i := range e.cfg.Discover {
		dc := &e.cfg.Discover[i]
		classHashFelt, err := new(felt.Felt).SetString(dc.ClassHash)
		if err != nil {
			return fmt.Errorf("parsing discover class hash %s: %w", dc.ClassHash, err)
		}
		classHashes[*classHashFelt] = dc
	}

	e.discovery = &discoveryState{
		udcAddress:    udcAddr,
		udcSelector:   selector,
		classHashes:   classHashes,
		cachedABI:     make(map[string]*abi.ABI),
		udcFormat:     e.cfg.Indexer.UDCEvent,
		sharedSchemas: make(map[string]map[string]*types.TableSchema),
	}

	// Log the configured UDC event format.
	formatDesc := "auto-detect"
	if e.cfg.Indexer.UDCEvent != nil {
		switch e.cfg.Indexer.UDCEvent.Version {
		case "v0":
			formatDesc = "v0 (Cairo 0)"
		case "v1":
			formatDesc = "v1 (modern Cairo)"
		default:
			if e.cfg.Indexer.UDCEvent.AddressKey != nil || e.cfg.Indexer.UDCEvent.AddressData != nil ||
				e.cfg.Indexer.UDCEvent.ClassHashKey != nil || e.cfg.Indexer.UDCEvent.ClassHashData != nil {
				formatDesc = "auto-detect with overrides"
			}
		}
	}

	e.logger.Info("contract discovery enabled",
		"class_hashes", len(e.cfg.Discover),
		"udc_event_format", formatDesc,
	)

	return nil
}

// isDiscoveryEvent returns true if the event comes from the UDC.
func (e *Engine) isDiscoveryEvent(raw *provider.RawEvent) bool {
	if e.discovery == nil || raw.ContractAddress == nil {
		return false
	}
	return raw.ContractAddress.Equal(e.discovery.udcAddress)
}

// resolveUDCLayout determines the UDC event layout for a given event.
// It uses explicit config when set, otherwise auto-detects from key/data counts.
func (ds *discoveryState) resolveUDCLayout(keys, data []*felt.Felt) (*udcLayout, error) {
	// Explicit version set in config.
	if ds.udcFormat != nil {
		switch ds.udcFormat.Version {
		case "v0":
			return &udcLayoutV0, nil
		case "v1":
			return &udcLayoutV1, nil
		}
	}

	// Auto-detect based on key count.
	layout, err := autoDetectUDCLayout(keys, data)
	if err != nil {
		return nil, err
	}

	// Apply fine-grained overrides if configured.
	if ds.udcFormat != nil {
		if ds.udcFormat.AddressKey != nil {
			layout.addressInKeys = true
			layout.addressIndex = *ds.udcFormat.AddressKey
		}
		if ds.udcFormat.AddressData != nil {
			layout.addressInKeys = false
			layout.addressIndex = *ds.udcFormat.AddressData
		}
		if ds.udcFormat.ClassHashKey != nil {
			layout.hashInKeys = true
			layout.hashIndex = *ds.udcFormat.ClassHashKey
		}
		if ds.udcFormat.ClassHashData != nil {
			layout.hashInKeys = false
			layout.hashIndex = *ds.udcFormat.ClassHashData
		}
	}

	return layout, nil
}

// autoDetectUDCLayout auto-detects UDC layout based on key/data counts.
//   - len(keys) >= 4 → v1 (modern Cairo)
//   - len(keys) == 1 && len(data) >= 4 → v0 (Cairo 0)
func autoDetectUDCLayout(keys, data []*felt.Felt) (*udcLayout, error) {
	if len(keys) >= 4 {
		l := udcLayoutV1
		return &l, nil
	}
	if len(keys) == 1 && len(data) >= 4 {
		l := udcLayoutV0
		return &l, nil
	}
	return nil, fmt.Errorf("unrecognized UDC event layout: keys=%d, data=%d", len(keys), len(data))
}

// extractDeployedAddress extracts the deployed contract address from a UDC event.
func extractDeployedAddress(layout *udcLayout, keys, data []*felt.Felt) (*felt.Felt, error) {
	if layout.addressInKeys {
		if layout.addressIndex >= len(keys) {
			return nil, fmt.Errorf("address_key index %d out of range (keys length %d)", layout.addressIndex, len(keys))
		}
		return keys[layout.addressIndex], nil
	}
	if layout.addressIndex >= len(data) {
		return nil, fmt.Errorf("address_data index %d out of range (data length %d)", layout.addressIndex, len(data))
	}
	return data[layout.addressIndex], nil
}

// extractClassHash extracts the class hash from a UDC event.
func extractClassHash(layout *udcLayout, keys, data []*felt.Felt) (*felt.Felt, error) {
	if layout.hashInKeys {
		if layout.hashIndex >= len(keys) {
			return nil, fmt.Errorf("class_hash_key index %d out of range (keys length %d)", layout.hashIndex, len(keys))
		}
		return keys[layout.hashIndex], nil
	}
	if layout.hashIndex >= len(data) {
		return nil, fmt.Errorf("class_hash_data index %d out of range (data length %d)", layout.hashIndex, len(data))
	}
	return data[layout.hashIndex], nil
}

// handleDiscoveryEvent processes a UDC ContractDeployed event. If the deployed
// contract's class hash matches a discover config, the contract is auto-registered.
//
// Two known UDC event layouts:
//
// v1 (modern Cairo):
//
//	keys[0]=selector, keys[1]=address, keys[2]=deployer, keys[3]=unique
//	data[0]=classHash, data[1..n]=calldata, data[n+1]=salt
//
// v0 (Cairo 0):
//
//	keys[0]=selector
//	data[0]=address, data[1]=deployer, data[2]=unique, data[3]=classHash, data[4..]=calldata, data[last]=salt
func (e *Engine) handleDiscoveryEvent(ctx context.Context, raw *provider.RawEvent) {
	// Verify this is a ContractDeployed event (need at least the selector key).
	if len(raw.Keys) < 1 || !raw.Keys[0].Equal(e.discovery.udcSelector) {
		return
	}

	// Resolve the layout for this event.
	layout, err := e.discovery.resolveUDCLayout(raw.Keys, raw.Data)
	if err != nil {
		e.logger.Warn("skipping UDC event: "+err.Error(),
			"block", raw.BlockNumber,
			"keys", len(raw.Keys),
			"data", len(raw.Data),
		)
		return
	}

	deployedAddress, err := extractDeployedAddress(layout, raw.Keys, raw.Data)
	if err != nil {
		e.logger.Warn("malformed ContractDeployed event: "+err.Error(),
			"block", raw.BlockNumber,
		)
		return
	}

	classHash, err := extractClassHash(layout, raw.Keys, raw.Data)
	if err != nil {
		e.logger.Warn("malformed ContractDeployed event: "+err.Error(),
			"block", raw.BlockNumber,
		)
		return
	}

	// Match class hash against discover configs.
	dc, ok := e.discovery.classHashes[*classHash]
	if !ok {
		// Not a watched class hash, update cursor and move on.
		if err := e.store.SetCursor(ctx, discoveryCursorName, raw.BlockNumber); err != nil {
			e.logger.Warn("failed to update discovery cursor", "error", err)
		}
		return
	}

	contractAddr := deployedAddress.String()
	contractName := buildDiscoveredName(dc, classHash.String(), contractAddr)

	// Check if already registered (e.g., from a previous run or restart).
	if e.isContractRegistered(contractName) {
		e.logger.Debug("discovered contract already registered",
			"name", contractName,
			"address", contractAddr,
		)
		if err := e.store.SetCursor(ctx, discoveryCursorName, raw.BlockNumber); err != nil {
			e.logger.Warn("failed to update discovery cursor", "error", err)
		}
		return
	}

	// Build contract config from discover template.
	cc := &config.ContractConfig{
		Name:              contractName,
		Address:           contractAddr,
		ABI:               dc.ABI,
		Events:            dc.Events,
		Views:             dc.Views,
		StartBlock:        config.Uint64Ptr(raw.BlockNumber),
		DiscoverClassHash: dc.ClassHash,
	}

	// When shared_tables is enabled, set the flags so schemas use shared table naming.
	if dc.SharedTables {
		cc.SharedTables = true
		cc.FactoryName = dc.ABI
	}

	// Register using cached ABI if available (same class hash = same code = same ABI).
	if cachedABI, hasCached := e.discovery.cachedABI[dc.ClassHash]; hasCached {
		var regErr error
		if dc.SharedTables {
			regErr = e.registerSharedDiscoveredChild(ctx, dc, cc, cachedABI)
		} else {
			regErr = e.registerWithABI(ctx, cc, cachedABI)
		}
		if regErr != nil {
			e.logger.Error("failed to register discovered contract",
				"name", contractName, "error", regErr,
			)
		} else {
			e.logger.Info("registered discovered contract",
				"name", contractName,
				"address", contractAddr,
				"class_hash", dc.ClassHash,
				"shared_tables", dc.SharedTables,
				"block", raw.BlockNumber,
			)
		}
	} else {
		// First discovery with this class hash: resolve ABI and cache it.
		if err := e.registerDiscoveredContract(ctx, dc, cc); err != nil {
			e.logger.Error("failed to register discovered contract",
				"name", contractName, "error", err,
			)
		} else {
			e.logger.Info("registered discovered contract (first instance)",
				"name", contractName,
				"address", contractAddr,
				"class_hash", dc.ClassHash,
				"shared_tables", dc.SharedTables,
				"block", raw.BlockNumber,
			)
		}
	}

	// Update discovery cursor.
	if err := e.store.SetCursor(ctx, discoveryCursorName, raw.BlockNumber); err != nil {
		e.logger.Warn("failed to update discovery cursor", "error", err)
	}
}

// registerDiscoveredContract resolves the ABI for a newly discovered contract,
// caches it for future discoveries with the same class hash, and registers the contract.
func (e *Engine) registerDiscoveredContract(ctx context.Context, dc *config.DiscoverConfig, cc *config.ContractConfig) error {
	resolver := config.NewABIResolver(e.provider)
	abis, err := resolver.ResolveAll(ctx, []config.ContractConfig{*cc})
	if err != nil {
		return fmt.Errorf("resolve ABI for discovered contract %s: %w", cc.Name, err)
	}

	contractABI := abis[cc.Address]
	if contractABI == nil {
		return fmt.Errorf("no ABI resolved for discovered contract %s (%s)", cc.Name, cc.Address)
	}

	// Cache for future discoveries with same class hash.
	e.discovery.cachedABI[dc.ClassHash] = contractABI

	if dc.SharedTables {
		return e.registerSharedDiscoveredChild(ctx, dc, cc, contractABI)
	}
	return e.registerWithABI(ctx, cc, contractABI)
}

// registerSharedDiscoveredChild registers a discovered contract that writes to shared tables.
// On the first discovery of a class hash with shared_tables, shared schemas are built and
// tables are created. Subsequent discoveries reuse the cached schemas (no new tables).
func (e *Engine) registerSharedDiscoveredChild(ctx context.Context, dc *config.DiscoverConfig, cc *config.ContractConfig, contractABI *abi.ABI) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	// Check for duplicate name.
	for _, cs := range e.contracts {
		if cs.config.Name == cc.Name {
			return fmt.Errorf("contract %q already registered", cc.Name)
		}
	}

	cc.Dynamic = true

	registry := abi.NewEventRegistry(contractABI)

	// Build shared schemas on first discovery, reuse for subsequent ones.
	schemas := e.discovery.sharedSchemas[dc.ClassHash]
	var schemaList []*types.TableSchema

	if schemas == nil {
		opts := &schema.BuildOptions{
			SharedTable: true,
			FactoryName: dc.ABI,
		}
		schemas = schema.BuildSchemas(cc, contractABI, registry, opts)

		// Create shared tables in store.
		for _, sch := range schemas {
			if err := e.store.CreateTable(ctx, sch); err != nil {
				return fmt.Errorf("create shared table %s: %w", sch.Name, err)
			}
			e.logger.Info("created shared table for discovery",
				"name", sch.Name,
				"type", sch.TableType,
				"columns", len(sch.Columns),
				"class_hash", dc.ClassHash,
				"abi_name", dc.ABI,
			)
			schemaList = append(schemaList, sch)
		}

		e.discovery.sharedSchemas[dc.ClassHash] = schemas
	}

	// Parse contract address.
	address, err := new(felt.Felt).SetString(cc.Address)
	if err != nil {
		return fmt.Errorf("parsing address for %s: %w", cc.Name, err)
	}

	// Persist dynamic contract config.
	if err := e.store.SaveDynamicContract(ctx, cc); err != nil {
		return fmt.Errorf("persisting discovered contract %s: %w", cc.Name, err)
	}

	cs := &contractState{
		config:   *cc,
		address:  address,
		abi:      contractABI,
		registry: registry,
		schemas:  schemas, // All discovered instances share the same schema references.
	}
	e.contracts = append(e.contracts, cs)

	// Spawn subscription if the engine is running.
	if e.subscriber != nil && e.runCtx != nil {
		sub := provider.ContractSubscription{
			Address:    address,
			StartBlock: derefUint64(cc.StartBlock),
			Wildcard:   hasWildcardEvent(cc),
			ERC20:      registry.MatchName("Transfer") != nil,
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

	// Start view polling for this contract if it has views configured.
	if e.runCtx != nil {
		viewSchemas, err := e.startViewsForContract(e.runCtx, cs)
		if err != nil {
			e.logger.Error("failed to start views for discovered contract",
				"contract", cc.Name, "error", err)
		} else {
			schemaList = append(schemaList, viewSchemas...)
		}
	}

	// Notify API server (schemas only for first discovery when tables were created).
	if e.onContractRegistered != nil {
		e.onContractRegistered(cc, schemaList)
	}

	return nil
}

// buildDiscoveredName generates a name for a discovered contract.
// Uses the NameTemplate if set, otherwise defaults to "{class_hash_short}_{address_short}".
func buildDiscoveredName(dc *config.DiscoverConfig, classHash, address string) string {
	tmpl := dc.NameTemplate
	if tmpl == "" {
		tmpl = "{class_hash_short}_{address_short}"
	}

	result := tmpl

	// Short class hash: last 8 hex chars.
	shortClassHash := strings.TrimPrefix(classHash, "0x")
	if len(shortClassHash) > 8 {
		shortClassHash = shortClassHash[len(shortClassHash)-8:]
	}
	result = strings.ReplaceAll(result, "{class_hash_short}", shortClassHash)

	// Short address: last 8 hex chars.
	shortAddr := strings.TrimPrefix(address, "0x")
	if len(shortAddr) > 8 {
		shortAddr = shortAddr[len(shortAddr)-8:]
	}
	result = strings.ReplaceAll(result, "{address_short}", shortAddr)

	// Full values.
	result = strings.ReplaceAll(result, "{class_hash}", classHash)
	result = strings.ReplaceAll(result, "{address}", address)

	// Group substitution.
	if dc.Group != "" {
		result = strings.ReplaceAll(result, "{group}", dc.Group)
	}

	return result
}

// discoveryStartBlock determines the starting block for the UDC subscription.
// Uses the persisted discovery cursor if available, otherwise falls back to
// the global indexer start block.
func (e *Engine) discoveryStartBlock(ctx context.Context) uint64 {
	cursor, err := e.store.GetCursor(ctx, discoveryCursorName)
	if err == nil && cursor > 0 {
		return cursor + 1
	}
	return derefUint64(e.cfg.Indexer.StartBlock)
}

// reorgDiscoveredContracts deregisters discovered contracts whose deploy block
// falls within the reverted block range.
func (e *Engine) reorgDiscoveredContracts(ctx context.Context, startBlock, endBlock uint64) {
	if e.discovery == nil {
		return
	}

	var toDeregister []string
	e.mu.RLock()
	for _, cs := range e.contracts {
		sb := derefUint64(cs.config.StartBlock)
		if cs.config.DiscoverClassHash != "" &&
			sb >= startBlock &&
			sb <= endBlock {
			toDeregister = append(toDeregister, cs.config.Name)
		}
	}
	e.mu.RUnlock()

	for _, name := range toDeregister {
		if err := e.DeregisterContract(ctx, name, true); err != nil {
			e.logger.Error("failed to deregister discovered contract during reorg",
				"contract", name,
				"error", err,
			)
		} else {
			e.logger.Info("deregistered discovered contract during reorg",
				"contract", name,
			)
		}
	}
}

// DiscoveredContracts returns information about all contracts discovered via class hash watching.
func (e *Engine) DiscoveredContracts() []ContractInfo {
	e.mu.RLock()
	defer e.mu.RUnlock()

	var discovered []ContractInfo
	for _, cs := range e.contracts {
		if cs.config.DiscoverClassHash == "" {
			continue
		}
		cursor, _ := e.store.GetCursor(context.Background(), cs.config.Name)
		discovered = append(discovered, ContractInfo{
			Name:         cs.config.Name,
			Address:      cs.config.Address,
			Events:       len(cs.config.Events),
			CurrentBlock: cursor,
			StartBlock:   derefUint64(cs.config.StartBlock),
			Status:       ContractStatusActive,
			Dynamic:      true,
		})
	}
	return discovered
}

// DiscoveredContractsByClassHash returns information about contracts discovered
// via class hash watching that match the given class hash.
func (e *Engine) DiscoveredContractsByClassHash(classHash string) []ContractInfo {
	e.mu.RLock()
	defer e.mu.RUnlock()

	filtered := []ContractInfo{}
	for _, cs := range e.contracts {
		if cs.config.DiscoverClassHash != classHash {
			continue
		}
		cursor, _ := e.store.GetCursor(context.Background(), cs.config.Name)
		filtered = append(filtered, ContractInfo{
			Name:         cs.config.Name,
			Address:      cs.config.Address,
			Events:       len(cs.config.Events),
			CurrentBlock: cursor,
			StartBlock:   derefUint64(cs.config.StartBlock),
			Status:       ContractStatusActive,
			Dynamic:      true,
		})
	}
	return filtered
}
