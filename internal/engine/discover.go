package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/NethermindEth/juno/core/felt"

	"github.com/b-j-roberts/ibis/internal/abi"
	"github.com/b-j-roberts/ibis/internal/config"
	"github.com/b-j-roberts/ibis/internal/provider"
)

// UDCAddress is the Universal Deployer Contract address on mainnet/Sepolia.
const UDCAddress = "0x04a64cd09a853868621d94cae9952b106f2c36a3f81260f85de6696c6b050221"

// discoveryCursorName is the cursor key used to persist UDC scan progress.
const discoveryCursorName = "_ibis_discover"

// discoveryState holds pre-computed values for class-hash-based contract discovery.
type discoveryState struct {
	udcAddress  *felt.Felt
	udcSelector *felt.Felt                           // sn_keccak("ContractDeployed")
	classHashes map[felt.Felt]*config.DiscoverConfig // class hash felt -> discover config
	cachedABI   map[string]*abi.ABI                  // class hash hex string -> resolved ABI
}

// setupDiscovery initializes discovery state from config. Called during engine setup.
func (e *Engine) setupDiscovery() error {
	if len(e.cfg.Discover) == 0 {
		return nil
	}

	udcAddr, err := new(felt.Felt).SetString(UDCAddress)
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
		udcAddress:  udcAddr,
		udcSelector: selector,
		classHashes: classHashes,
		cachedABI:   make(map[string]*abi.ABI),
	}

	e.logger.Info("contract discovery enabled",
		"class_hashes", len(e.cfg.Discover),
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

// handleDiscoveryEvent processes a UDC ContractDeployed event. If the deployed
// contract's class hash matches a discover config, the contract is auto-registered.
//
// UDC event layout:
//
//	keys[0] = sn_keccak("ContractDeployed")
//	keys[1] = deployed contract address
//	keys[2] = deployer address
//	keys[3] = unique (0 or 1)
//	data[0] = classHash
//	data[1..n] = calldata (length-prefixed span)
//	data[n+1] = salt
func (e *Engine) handleDiscoveryEvent(ctx context.Context, raw *provider.RawEvent) {
	// Verify this is a ContractDeployed event.
	if len(raw.Keys) < 2 || !raw.Keys[0].Equal(e.discovery.udcSelector) {
		return
	}

	// Need at least keys[1] (address) and data[0] (classHash).
	if len(raw.Data) < 1 {
		e.logger.Warn("malformed ContractDeployed event: missing data fields",
			"block", raw.BlockNumber,
		)
		return
	}

	deployedAddress := raw.Keys[1]
	classHash := raw.Data[0]

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
		StartBlock:        raw.BlockNumber,
		DiscoverClassHash: dc.ClassHash,
	}

	// Register using cached ABI if available (same class hash = same code = same ABI).
	if cachedABI, hasCached := e.discovery.cachedABI[dc.ClassHash]; hasCached {
		if err := e.registerWithABI(ctx, cc, cachedABI); err != nil {
			e.logger.Error("failed to register discovered contract",
				"name", contractName, "error", err,
			)
		} else {
			e.logger.Info("registered discovered contract",
				"name", contractName,
				"address", contractAddr,
				"class_hash", dc.ClassHash,
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

	return e.registerWithABI(ctx, cc, contractABI)
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
	return e.cfg.Indexer.StartBlock
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
		if cs.config.DiscoverClassHash != "" &&
			cs.config.StartBlock >= startBlock &&
			cs.config.StartBlock <= endBlock {
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
			StartBlock:   cs.config.StartBlock,
			Status:       ContractStatusActive,
			Dynamic:      true,
		})
	}
	return discovered
}
