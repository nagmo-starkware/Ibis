package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/NethermindEth/juno/core/felt"

	"github.com/b-j-roberts/ibis/internal/abi"
	"github.com/b-j-roberts/ibis/internal/config"
	"github.com/b-j-roberts/ibis/internal/provider"
	"github.com/b-j-roberts/ibis/internal/schema"
	"github.com/b-j-roberts/ibis/internal/store"
	"github.com/b-j-roberts/ibis/internal/store/memory"
	"github.com/b-j-roberts/ibis/internal/types"
)

// --- Discovery test helpers ---

// testDiscoverConfig creates a DiscoverConfig for testing.
func testDiscoverConfig(classHash string) config.DiscoverConfig {
	return config.DiscoverConfig{
		ClassHash: classHash,
		ABI:       "fetch",
		Events: []config.EventConfig{
			{Name: "*", Table: config.TableConfig{Type: "log"}},
		},
	}
}

// testDiscoverConfigShared creates a DiscoverConfig with shared_tables enabled.
func testDiscoverConfigShared(classHash string) config.DiscoverConfig {
	return config.DiscoverConfig{
		ClassHash:    classHash,
		ABI:          "OptionToken",
		SharedTables: true,
		Events: []config.EventConfig{
			{Name: "*", Table: config.TableConfig{Type: "log"}},
		},
	}
}

// makeUDCEvent creates a ContractDeployed event from the UDC.
// keys: [selector, deployed_address, deployer, unique]
// data: [classHash, calldata_len, ...calldata, salt]
func makeUDCEvent(deployedAddr, classHash *felt.Felt, blockNumber uint64) provider.RawEvent {
	selector := abi.ComputeSelector("ContractDeployed")
	deployer := new(felt.Felt).SetUint64(0xDE910E8)
	unique := new(felt.Felt).SetUint64(1)
	salt := new(felt.Felt).SetUint64(0x5A17)
	calldataLen := new(felt.Felt).SetUint64(0) // Empty calldata

	udcAddr, _ := new(felt.Felt).SetString(UDCAddress)
	txHash := new(felt.Felt).SetUint64(blockNumber*1000 + 1)
	blockHash := new(felt.Felt).SetUint64(blockNumber * 100)

	return provider.RawEvent{
		BlockNumber:     blockNumber,
		BlockHash:       blockHash,
		TransactionHash: txHash,
		ContractAddress: udcAddr,
		Keys:            []*felt.Felt{selector, deployedAddr, deployer, unique},
		Data:            []*felt.Felt{classHash, calldataLen, salt},
		FinalityStatus:  "ACCEPTED_ON_L2",
	}
}

// newDiscoveryTestEngine creates an Engine configured for discovery testing.
// It injects a pre-cached ABI for the given class hash to avoid network calls.
func newDiscoveryTestEngine(st store.Store, classHash string) *Engine {
	discoverCfg := testDiscoverConfig(classHash)
	cfg := &config.Config{
		Indexer:  config.IndexerConfig{StartBlock: config.Uint64Ptr(0), UDCAddress: UDCAddress},
		Discover: []config.DiscoverConfig{discoverCfg},
	}

	e := &Engine{
		cfg:          cfg,
		store:        st,
		logger:       noopLogger(),
		pending:      NewPendingTracker(),
		logIndices:   make(map[uint64]uint64),
		contracts:    []*contractState{},
		confirmDepth: 100,
	}

	// Set up discovery state manually (bypasses setupDiscovery which is tested separately).
	udcAddr, _ := new(felt.Felt).SetString(UDCAddress)
	classHashFelt, _ := new(felt.Felt).SetString(classHash)

	e.discovery = &discoveryState{
		udcAddress:  udcAddr,
		udcSelector: abi.ComputeSelector("ContractDeployed"),
		classHashes: map[felt.Felt]*config.DiscoverConfig{
			*classHashFelt: &cfg.Discover[0],
		},
		cachedABI: map[string]*abi.ABI{
			classHash: testChildABI(), // Pre-cache to avoid network calls.
		},
		sharedSchemas: make(map[string]map[string]*types.TableSchema),
	}

	return e
}

// newSharedDiscoveryTestEngine creates an Engine configured for shared-table discovery testing.
func newSharedDiscoveryTestEngine(st store.Store, classHash string) *Engine {
	discoverCfg := testDiscoverConfigShared(classHash)
	cfg := &config.Config{
		Indexer:  config.IndexerConfig{StartBlock: config.Uint64Ptr(0), UDCAddress: UDCAddress},
		Discover: []config.DiscoverConfig{discoverCfg},
	}

	e := &Engine{
		cfg:          cfg,
		store:        st,
		logger:       noopLogger(),
		pending:      NewPendingTracker(),
		logIndices:   make(map[uint64]uint64),
		contracts:    []*contractState{},
		confirmDepth: 100,
	}

	udcAddr, _ := new(felt.Felt).SetString(UDCAddress)
	classHashFelt, _ := new(felt.Felt).SetString(classHash)

	e.discovery = &discoveryState{
		udcAddress:  udcAddr,
		udcSelector: abi.ComputeSelector("ContractDeployed"),
		classHashes: map[felt.Felt]*config.DiscoverConfig{
			*classHashFelt: &cfg.Discover[0],
		},
		cachedABI: map[string]*abi.ABI{
			classHash: testChildABI(),
		},
		sharedSchemas: make(map[string]map[string]*types.TableSchema),
	}

	return e
}

// --- BuildDiscoveredName Tests ---

func TestBuildDiscoveredName_DefaultTemplate(t *testing.T) {
	dc := &config.DiscoverConfig{ClassHash: "0xabc123"}
	name := buildDiscoveredName(dc, "0x00000000000000000000000000000000000000000000000000000000abc12345", "0x0000000000000000000000000000000000000000000000000000000012345678")

	if name != "abc12345_12345678" {
		t.Fatalf("expected abc12345_12345678, got %s", name)
	}
}

func TestBuildDiscoveredName_CustomTemplate(t *testing.T) {
	dc := &config.DiscoverConfig{
		ClassHash:    "0xabc123",
		Group:        "my-tokens",
		NameTemplate: "{group}_{address_short}",
	}
	name := buildDiscoveredName(dc, "0xabc123", "0x0000000000000000000000000000000000000000000000000000000012345678")

	if name != "my-tokens_12345678" {
		t.Fatalf("expected my-tokens_12345678, got %s", name)
	}
}

func TestBuildDiscoveredName_ShortHash(t *testing.T) {
	dc := &config.DiscoverConfig{ClassHash: "0xabc"}
	// Class hash shorter than 8 chars: use full value.
	name := buildDiscoveredName(dc, "0xabc", "0xdef")

	if name != "abc_def" {
		t.Fatalf("expected abc_def, got %s", name)
	}
}

func TestBuildDiscoveredName_FullValues(t *testing.T) {
	dc := &config.DiscoverConfig{
		ClassHash:    "0x123",
		NameTemplate: "discovered_{class_hash}_{address}",
	}
	name := buildDiscoveredName(dc, "0x123", "0x456")

	if name != "discovered_0x123_0x456" {
		t.Fatalf("expected discovered_0x123_0x456, got %s", name)
	}
}

// --- SetupDiscovery Tests ---

func TestSetupDiscovery_NoConfig(t *testing.T) {
	e := &Engine{
		cfg:    &config.Config{},
		logger: noopLogger(),
	}

	if err := e.setupDiscovery(); err != nil {
		t.Fatal(err)
	}
	if e.discovery != nil {
		t.Fatal("expected nil discovery when no config")
	}
}

func TestSetupDiscovery_WithConfig(t *testing.T) {
	classHash := "0x01234567890abcdef01234567890abcdef01234567890abcdef0123456789ab"
	e := &Engine{
		cfg: &config.Config{
			Indexer: config.IndexerConfig{UDCAddress: UDCAddress},
			Discover: []config.DiscoverConfig{
				testDiscoverConfig(classHash),
			},
		},
		logger: noopLogger(),
	}

	if err := e.setupDiscovery(); err != nil {
		t.Fatal(err)
	}
	if e.discovery == nil {
		t.Fatal("expected non-nil discovery")
	}
	if len(e.discovery.classHashes) != 1 {
		t.Fatalf("expected 1 class hash, got %d", len(e.discovery.classHashes))
	}
	if e.discovery.udcAddress == nil {
		t.Fatal("expected non-nil UDC address")
	}
	if e.discovery.udcSelector == nil {
		t.Fatal("expected non-nil UDC selector")
	}
}

func TestSetupDiscovery_InvalidClassHash(t *testing.T) {
	e := &Engine{
		cfg: &config.Config{
			Discover: []config.DiscoverConfig{
				{ClassHash: "not-a-hex-hash", ABI: "fetch",
					Events: []config.EventConfig{{Name: "*", Table: config.TableConfig{Type: "log"}}}},
			},
		},
		logger: noopLogger(),
	}

	if err := e.setupDiscovery(); err == nil {
		t.Fatal("expected error for invalid class hash")
	}
}

func TestSetupDiscovery_CustomUDCAddress(t *testing.T) {
	classHash := "0x01234567890abcdef01234567890abcdef01234567890abcdef0123456789ab"
	customUDC := "0x041a78e741e5af2fec34b695679bc6891742439f7afb8484ecd7766661ad02bf"
	e := &Engine{
		cfg: &config.Config{
			Indexer: config.IndexerConfig{UDCAddress: customUDC},
			Discover: []config.DiscoverConfig{
				testDiscoverConfig(classHash),
			},
		},
		logger: noopLogger(),
	}

	if err := e.setupDiscovery(); err != nil {
		t.Fatal(err)
	}
	if e.discovery == nil {
		t.Fatal("expected non-nil discovery")
	}

	// Verify the custom UDC address was used.
	expectedUDC, _ := new(felt.Felt).SetString(customUDC)
	if !e.discovery.udcAddress.Equal(expectedUDC) {
		t.Fatalf("expected custom UDC address %s, got %s", expectedUDC, e.discovery.udcAddress)
	}
}

func TestSetupDiscovery_DefaultUDCAddress(t *testing.T) {
	classHash := "0x01234567890abcdef01234567890abcdef01234567890abcdef0123456789ab"
	e := &Engine{
		cfg: &config.Config{
			Indexer: config.IndexerConfig{UDCAddress: UDCAddress},
			Discover: []config.DiscoverConfig{
				testDiscoverConfig(classHash),
			},
		},
		logger: noopLogger(),
	}

	if err := e.setupDiscovery(); err != nil {
		t.Fatal(err)
	}

	expectedUDC, _ := new(felt.Felt).SetString(UDCAddress)
	if !e.discovery.udcAddress.Equal(expectedUDC) {
		t.Fatalf("expected default UDC address, got %s", e.discovery.udcAddress)
	}
}

// --- IsDiscoveryEvent Tests ---

func TestIsDiscoveryEvent_MatchesUDC(t *testing.T) {
	st := memory.New()
	classHash := "0xABCDEF"
	e := newDiscoveryTestEngine(st, classHash)

	udcAddr, _ := new(felt.Felt).SetString(UDCAddress)
	raw := provider.RawEvent{ContractAddress: udcAddr}

	if !e.isDiscoveryEvent(&raw) {
		t.Fatal("expected UDC event to be recognized as discovery event")
	}
}

func TestIsDiscoveryEvent_IgnoresOtherAddresses(t *testing.T) {
	st := memory.New()
	classHash := "0xABCDEF"
	e := newDiscoveryTestEngine(st, classHash)

	otherAddr := new(felt.Felt).SetUint64(0x999)
	raw := provider.RawEvent{ContractAddress: otherAddr}

	if e.isDiscoveryEvent(&raw) {
		t.Fatal("expected non-UDC event to not be a discovery event")
	}
}

func TestIsDiscoveryEvent_NilDiscovery(t *testing.T) {
	e := &Engine{}
	raw := provider.RawEvent{ContractAddress: new(felt.Felt).SetUint64(1)}

	if e.isDiscoveryEvent(&raw) {
		t.Fatal("expected false when discovery is nil")
	}
}

// --- HandleDiscoveryEvent Tests ---

func TestHandleDiscoveryEvent_RegistersMatchingContract(t *testing.T) {
	st := memory.New()
	ctx := context.Background()

	classHash := "0xABCDEF"
	e := newDiscoveryTestEngine(st, classHash)

	deployedAddr := new(felt.Felt).SetUint64(0x12345)
	classHashFelt, _ := new(felt.Felt).SetString(classHash)
	raw := makeUDCEvent(deployedAddr, classHashFelt, 100)

	e.handleDiscoveryEvent(ctx, &raw)

	// Verify the contract was registered.
	e.mu.RLock()
	count := len(e.contracts)
	e.mu.RUnlock()

	if count != 1 {
		t.Fatalf("expected 1 registered contract, got %d", count)
	}

	// Verify contract properties.
	e.mu.RLock()
	cs := e.contracts[0]
	e.mu.RUnlock()

	if cs.config.DiscoverClassHash != classHash {
		t.Fatalf("expected DiscoverClassHash %s, got %s", classHash, cs.config.DiscoverClassHash)
	}
	if cs.config.StartBlock == nil || *cs.config.StartBlock != 100 {
		t.Fatalf("expected StartBlock 100, got %v", cs.config.StartBlock)
	}
	if !cs.config.Dynamic {
		t.Fatal("expected Dynamic=true")
	}

	// Verify discovery cursor was updated.
	cursor, _ := st.GetCursor(ctx, discoveryCursorName)
	if cursor != 100 {
		t.Fatalf("expected discovery cursor 100, got %d", cursor)
	}
}

func TestHandleDiscoveryEvent_IgnoresNonMatchingClassHash(t *testing.T) {
	st := memory.New()
	ctx := context.Background()

	classHash := "0xABCDEF"
	e := newDiscoveryTestEngine(st, classHash)

	deployedAddr := new(felt.Felt).SetUint64(0x12345)
	wrongClassHash := new(felt.Felt).SetUint64(0x999999) // Not the watched class hash
	raw := makeUDCEvent(deployedAddr, wrongClassHash, 100)

	e.handleDiscoveryEvent(ctx, &raw)

	// No contract should be registered.
	e.mu.RLock()
	count := len(e.contracts)
	e.mu.RUnlock()

	if count != 0 {
		t.Fatalf("expected 0 registered contracts, got %d", count)
	}

	// Discovery cursor should still be updated (we processed the event).
	cursor, _ := st.GetCursor(ctx, discoveryCursorName)
	if cursor != 100 {
		t.Fatalf("expected discovery cursor 100 even for non-matching event, got %d", cursor)
	}
}

func TestHandleDiscoveryEvent_SkipsDuplicateContract(t *testing.T) {
	st := memory.New()
	ctx := context.Background()

	classHash := "0xABCDEF"
	e := newDiscoveryTestEngine(st, classHash)

	deployedAddr := new(felt.Felt).SetUint64(0x12345)
	classHashFelt, _ := new(felt.Felt).SetString(classHash)

	// Process same deployment twice.
	raw1 := makeUDCEvent(deployedAddr, classHashFelt, 100)
	e.handleDiscoveryEvent(ctx, &raw1)

	raw2 := makeUDCEvent(deployedAddr, classHashFelt, 101)
	e.handleDiscoveryEvent(ctx, &raw2)

	// Should still only have 1 contract.
	e.mu.RLock()
	count := len(e.contracts)
	e.mu.RUnlock()

	if count != 1 {
		t.Fatalf("expected 1 contract (no duplicates), got %d", count)
	}
}

func TestHandleDiscoveryEvent_MultipleContracts(t *testing.T) {
	st := memory.New()
	ctx := context.Background()

	classHash := "0xABCDEF"
	e := newDiscoveryTestEngine(st, classHash)
	classHashFelt, _ := new(felt.Felt).SetString(classHash)

	// Deploy 3 different contracts with the same class hash.
	for i := uint64(1); i <= 3; i++ {
		deployedAddr := new(felt.Felt).SetUint64(0x1000 + i)
		raw := makeUDCEvent(deployedAddr, classHashFelt, 100+i)
		e.handleDiscoveryEvent(ctx, &raw)
	}

	e.mu.RLock()
	count := len(e.contracts)
	e.mu.RUnlock()

	if count != 3 {
		t.Fatalf("expected 3 discovered contracts, got %d", count)
	}

	// Verify all are dynamic and have the class hash set.
	e.mu.RLock()
	for _, cs := range e.contracts {
		if !cs.config.Dynamic {
			t.Fatalf("contract %s should be dynamic", cs.config.Name)
		}
		if cs.config.DiscoverClassHash != classHash {
			t.Fatalf("contract %s: expected DiscoverClassHash %s, got %s",
				cs.config.Name, classHash, cs.config.DiscoverClassHash)
		}
	}
	e.mu.RUnlock()
}

func TestHandleDiscoveryEvent_InvalidEvent(t *testing.T) {
	st := memory.New()
	ctx := context.Background()

	classHash := "0xABCDEF"
	e := newDiscoveryTestEngine(st, classHash)

	// Missing keys - should be silently ignored.
	udcAddr, _ := new(felt.Felt).SetString(UDCAddress)
	raw := provider.RawEvent{
		BlockNumber:     100,
		ContractAddress: udcAddr,
		Keys:            []*felt.Felt{}, // Empty keys
		Data:            []*felt.Felt{},
	}
	e.handleDiscoveryEvent(ctx, &raw)

	e.mu.RLock()
	count := len(e.contracts)
	e.mu.RUnlock()
	if count != 0 {
		t.Fatalf("expected 0 contracts after invalid event, got %d", count)
	}
}

// --- Reorg Tests ---

func TestReorgDiscoveredContracts_DeregistersInRange(t *testing.T) {
	st := memory.New()
	ctx := context.Background()

	classHash := "0xABCDEF"
	e := newDiscoveryTestEngine(st, classHash)
	classHashFelt, _ := new(felt.Felt).SetString(classHash)

	// Register 3 contracts at blocks 100, 101, 102.
	for i := uint64(0); i < 3; i++ {
		deployedAddr := new(felt.Felt).SetUint64(0x2000 + i)
		raw := makeUDCEvent(deployedAddr, classHashFelt, 100+i)
		e.handleDiscoveryEvent(ctx, &raw)
	}

	e.mu.RLock()
	if len(e.contracts) != 3 {
		t.Fatalf("expected 3 contracts before reorg, got %d", len(e.contracts))
	}
	e.mu.RUnlock()

	// Reorg blocks 101-102.
	e.reorgDiscoveredContracts(ctx, 101, 102)

	e.mu.RLock()
	remaining := len(e.contracts)
	e.mu.RUnlock()

	if remaining != 1 {
		t.Fatalf("expected 1 contract after reorg (deployed at block 100), got %d", remaining)
	}
}

func TestReorgDiscoveredContracts_NilDiscovery(t *testing.T) {
	e := &Engine{logger: noopLogger()}

	// Should not panic when discovery is nil.
	e.reorgDiscoveredContracts(context.Background(), 100, 200)
}

// --- DiscoveredContracts Listing ---

func TestDiscoveredContracts_ReturnsOnlyDiscovered(t *testing.T) {
	st := memory.New()
	ctx := context.Background()

	classHash := "0xABCDEF"
	e := newDiscoveryTestEngine(st, classHash)
	classHashFelt, _ := new(felt.Felt).SetString(classHash)

	// Add a regular (non-discovered) contract.
	regularAddr := new(felt.Felt).SetUint64(0x999)
	transferDef := testEventDef("Transfer")
	regularCS := testContractState(regularAddr, "regular", []*abi.EventDef{transferDef}, types.TableTypeLog)
	e.mu.Lock()
	e.contracts = append(e.contracts, regularCS)
	e.mu.Unlock()

	// Discover 2 contracts.
	for i := uint64(1); i <= 2; i++ {
		addr := new(felt.Felt).SetUint64(0x3000 + i)
		raw := makeUDCEvent(addr, classHashFelt, 200+i)
		e.handleDiscoveryEvent(ctx, &raw)
	}

	discovered := e.DiscoveredContracts()
	if len(discovered) != 2 {
		t.Fatalf("expected 2 discovered contracts, got %d", len(discovered))
	}

	for _, d := range discovered {
		if !d.Dynamic {
			t.Fatalf("discovered contract %s should be dynamic", d.Name)
		}
	}
}

// --- DiscoveryStartBlock Tests ---

func TestDiscoveryStartBlock_FromCursor(t *testing.T) {
	st := memory.New()
	ctx := context.Background()
	st.SetCursor(ctx, discoveryCursorName, 500)

	e := &Engine{
		cfg:   &config.Config{Indexer: config.IndexerConfig{StartBlock: config.Uint64Ptr(100)}},
		store: st,
	}

	startBlock := e.discoveryStartBlock(ctx)
	if startBlock != 501 {
		t.Fatalf("expected start block 501 (cursor+1), got %d", startBlock)
	}
}

func TestDiscoveryStartBlock_FromConfig(t *testing.T) {
	st := memory.New()
	ctx := context.Background()

	e := &Engine{
		cfg:   &config.Config{Indexer: config.IndexerConfig{StartBlock: config.Uint64Ptr(200)}},
		store: st,
	}

	startBlock := e.discoveryStartBlock(ctx)
	if startBlock != 200 {
		t.Fatalf("expected start block 200 (from config), got %d", startBlock)
	}
}

func TestDiscoveryStartBlock_DefaultZero(t *testing.T) {
	st := memory.New()
	ctx := context.Background()

	e := &Engine{
		cfg:   &config.Config{Indexer: config.IndexerConfig{StartBlock: config.Uint64Ptr(0)}},
		store: st,
	}

	startBlock := e.discoveryStartBlock(ctx)
	if startBlock != 0 {
		t.Fatalf("expected start block 0, got %d", startBlock)
	}
}

// --- BuildSubscriptions with Discovery ---

func TestBuildSubscriptions_IncludesUDC(t *testing.T) {
	st := memory.New()
	classHash := "0xABCDEF"
	e := newDiscoveryTestEngine(st, classHash)

	// Add a regular contract for comparison.
	contractAddr := new(felt.Felt).SetUint64(0xABC)
	transferDef := testEventDef("Transfer")
	cs := testContractState(contractAddr, "mytoken", []*abi.EventDef{transferDef}, types.TableTypeLog)
	e.contracts = []*contractState{cs}

	subs := e.buildSubscriptions(map[string]uint64{"mytoken": 50})

	// Should have 2 subs: 1 regular + 1 UDC.
	if len(subs) != 2 {
		t.Fatalf("expected 2 subscriptions (1 contract + 1 UDC), got %d", len(subs))
	}

	// Find the UDC subscription.
	udcAddr, _ := new(felt.Felt).SetString(UDCAddress)
	var udcSub *provider.ContractSubscription
	for i := range subs {
		if subs[i].Address.Equal(udcAddr) {
			udcSub = &subs[i]
			break
		}
	}

	if udcSub == nil {
		t.Fatal("expected UDC subscription to be present")
	}

	// UDC should be filtered by ContractDeployed selector.
	if udcSub.Keys == nil || len(udcSub.Keys) != 1 || len(udcSub.Keys[0]) != 1 {
		t.Fatal("expected UDC subscription to have ContractDeployed key filter")
	}
	expectedSelector := abi.ComputeSelector("ContractDeployed")
	if !udcSub.Keys[0][0].Equal(expectedSelector) {
		t.Fatal("UDC key filter should be ContractDeployed selector")
	}
}

func TestBuildSubscriptions_NoUDCWithoutDiscovery(t *testing.T) {
	contractAddr := new(felt.Felt).SetUint64(0xABC)
	transferDef := testEventDef("Transfer")
	cs := testContractState(contractAddr, "mytoken", []*abi.EventDef{transferDef}, types.TableTypeLog)

	e := &Engine{contracts: []*contractState{cs}}
	subs := e.buildSubscriptions(map[string]uint64{"mytoken": 50})

	if len(subs) != 1 {
		t.Fatalf("expected 1 subscription (no UDC), got %d", len(subs))
	}
}

// --- Integration: Discovery with EventLoop ---

func TestEventLoop_DiscoveryRegistersContractFromUDCEvent(t *testing.T) {
	st := memory.New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	classHash := "0xABCDEF"
	e := newDiscoveryTestEngine(st, classHash)

	eventsCh := make(chan provider.RawEvent, 20)
	reorgsCh := make(chan provider.ReorgNotification, 10)
	e.events = eventsCh
	e.reorgs = reorgsCh

	// Run event loop in background.
	done := make(chan error, 1)
	go func() {
		done <- e.eventLoop(ctx)
	}()

	// Send a UDC ContractDeployed event.
	deployedAddr := new(felt.Felt).SetUint64(0xDA1E)
	classHashFelt, _ := new(felt.Felt).SetString(classHash)
	eventsCh <- makeUDCEvent(deployedAddr, classHashFelt, 50)

	// Wait for discovery processing.
	time.Sleep(100 * time.Millisecond)

	// Verify the contract was discovered and registered.
	e.mu.RLock()
	count := len(e.contracts)
	e.mu.RUnlock()

	if count != 1 {
		t.Fatalf("expected 1 discovered contract, got %d", count)
	}

	// Verify a regular event from a known contract also works through the loop.
	contractAddr := new(felt.Felt).SetUint64(0xABC)
	transferDef := testEventDef("Transfer")
	cs := testContractState(contractAddr, "mytoken", []*abi.EventDef{transferDef}, types.TableTypeLog)
	if err := st.CreateTable(ctx, cs.schemas["Transfer"]); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	e.contracts = append(e.contracts, cs)
	e.mu.Unlock()

	sender := new(felt.Felt).SetUint64(0xDEAD)
	amount := new(felt.Felt).SetUint64(1000)
	eventsCh <- makeRawEvent(transferDef.Selector, contractAddr, 60, sender, amount)

	time.Sleep(100 * time.Millisecond)

	events, _ := st.GetEvents(ctx, "mytoken_Transfer", store.Query{Limit: 10})
	if len(events) != 1 {
		t.Fatalf("expected 1 regular event after discovery event, got %d", len(events))
	}

	cancel()
	<-done
}

// --- Persistence / JSON Round-Trip ---

func TestDiscoveredContractConfig_JSONRoundTrip(t *testing.T) {
	original := config.ContractConfig{
		Name:              "abcd1234_5678abcd",
		Address:           "0x5678abcd",
		ABI:               "fetch",
		Events:            []config.EventConfig{{Name: "*", Table: config.TableConfig{Type: "log"}}},
		StartBlock:        config.Uint64Ptr(100),
		Dynamic:           true,
		DiscoverClassHash: "0xabcd1234",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var restored config.ContractConfig
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if restored.DiscoverClassHash != original.DiscoverClassHash {
		t.Fatalf("DiscoverClassHash: expected %s, got %s",
			original.DiscoverClassHash, restored.DiscoverClassHash)
	}
	if restored.Name != original.Name {
		t.Fatalf("Name: expected %s, got %s", original.Name, restored.Name)
	}
	if restored.StartBlock == nil || *restored.StartBlock != *original.StartBlock {
		t.Fatalf("StartBlock: expected %v, got %v", original.StartBlock, restored.StartBlock)
	}
	if !restored.Dynamic {
		t.Fatal("Dynamic should survive JSON round-trip")
	}
}

// --- UDC Event Format Detection Tests ---

// makeUDCEventV0 creates a Cairo 0 ContractDeployed event from the UDC.
// v0 layout: keys[0]=selector, data[0]=address, data[1]=deployer, data[2]=unique, data[3]=classHash, data[4]=calldata_len, data[5]=salt
func makeUDCEventV0(deployedAddr, classHash *felt.Felt, blockNumber uint64) provider.RawEvent {
	selector := abi.ComputeSelector("ContractDeployed")
	deployer := new(felt.Felt).SetUint64(0xDE910E8)
	unique := new(felt.Felt).SetUint64(1)
	calldataLen := new(felt.Felt).SetUint64(0)
	salt := new(felt.Felt).SetUint64(0x5A17)

	udcAddr, _ := new(felt.Felt).SetString(UDCAddress)
	txHash := new(felt.Felt).SetUint64(blockNumber*1000 + 1)
	blockHash := new(felt.Felt).SetUint64(blockNumber * 100)

	return provider.RawEvent{
		BlockNumber:     blockNumber,
		BlockHash:       blockHash,
		TransactionHash: txHash,
		ContractAddress: udcAddr,
		Keys:            []*felt.Felt{selector},
		Data:            []*felt.Felt{deployedAddr, deployer, unique, classHash, calldataLen, salt},
		FinalityStatus:  "ACCEPTED_ON_L2",
	}
}

func intPtr(v int) *int { return &v }

func TestAutoDetectUDCLayout_V1(t *testing.T) {
	selector := abi.ComputeSelector("ContractDeployed")
	addr := new(felt.Felt).SetUint64(0x123)
	deployer := new(felt.Felt).SetUint64(0x456)
	unique := new(felt.Felt).SetUint64(1)
	classHash := new(felt.Felt).SetUint64(0xABC)

	keys := []*felt.Felt{selector, addr, deployer, unique}
	data := []*felt.Felt{classHash}

	layout, err := autoDetectUDCLayout(keys, data)
	if err != nil {
		t.Fatal(err)
	}
	if layout.version != udcVersionV1 {
		t.Fatal("expected v1 layout")
	}
	if !layout.addressInKeys || layout.addressIndex != 1 {
		t.Fatal("expected address in keys[1]")
	}
	if layout.hashInKeys || layout.hashIndex != 0 {
		t.Fatal("expected class hash in data[0]")
	}
}

func TestAutoDetectUDCLayout_V0(t *testing.T) {
	selector := abi.ComputeSelector("ContractDeployed")
	addr := new(felt.Felt).SetUint64(0x123)
	deployer := new(felt.Felt).SetUint64(0x456)
	unique := new(felt.Felt).SetUint64(1)
	classHash := new(felt.Felt).SetUint64(0xABC)

	keys := []*felt.Felt{selector}
	data := []*felt.Felt{addr, deployer, unique, classHash}

	layout, err := autoDetectUDCLayout(keys, data)
	if err != nil {
		t.Fatal(err)
	}
	if layout.version != udcVersionV0 {
		t.Fatal("expected v0 layout")
	}
	if layout.addressInKeys || layout.addressIndex != 0 {
		t.Fatal("expected address in data[0]")
	}
	if layout.hashInKeys || layout.hashIndex != 3 {
		t.Fatal("expected class hash in data[3]")
	}
}

func TestAutoDetectUDCLayout_Unrecognized(t *testing.T) {
	// 2 keys, 2 data — matches neither pattern.
	keys := []*felt.Felt{new(felt.Felt).SetUint64(1), new(felt.Felt).SetUint64(2)}
	data := []*felt.Felt{new(felt.Felt).SetUint64(3), new(felt.Felt).SetUint64(4)}

	_, err := autoDetectUDCLayout(keys, data)
	if err == nil {
		t.Fatal("expected error for unrecognized layout")
	}
	if !strings.Contains(err.Error(), "unrecognized") {
		t.Fatalf("expected 'unrecognized' in error, got: %v", err)
	}
}

func TestResolveUDCLayout_ExplicitV0(t *testing.T) {
	ds := &discoveryState{
		udcFormat: &config.UDCEventFormat{Version: "v0"},
	}
	// Even with v1-shaped keys, explicit v0 should win.
	keys := []*felt.Felt{new(felt.Felt), new(felt.Felt), new(felt.Felt), new(felt.Felt)}
	data := []*felt.Felt{new(felt.Felt)}

	layout, err := ds.resolveUDCLayout(keys, data)
	if err != nil {
		t.Fatal(err)
	}
	if layout.version != udcVersionV0 {
		t.Fatal("expected v0 layout when explicitly configured")
	}
}

func TestResolveUDCLayout_ExplicitV1(t *testing.T) {
	ds := &discoveryState{
		udcFormat: &config.UDCEventFormat{Version: "v1"},
	}
	// Even with v0-shaped keys, explicit v1 should win.
	keys := []*felt.Felt{new(felt.Felt)}
	data := []*felt.Felt{new(felt.Felt), new(felt.Felt), new(felt.Felt), new(felt.Felt)}

	layout, err := ds.resolveUDCLayout(keys, data)
	if err != nil {
		t.Fatal(err)
	}
	if layout.version != udcVersionV1 {
		t.Fatal("expected v1 layout when explicitly configured")
	}
}

func TestResolveUDCLayout_FineGrainedOverrides(t *testing.T) {
	ds := &discoveryState{
		udcFormat: &config.UDCEventFormat{
			AddressKey:    intPtr(2),
			ClassHashData: intPtr(1),
		},
	}
	// v1-shaped event.
	keys := []*felt.Felt{new(felt.Felt), new(felt.Felt), new(felt.Felt), new(felt.Felt)}
	data := []*felt.Felt{new(felt.Felt), new(felt.Felt)}

	layout, err := ds.resolveUDCLayout(keys, data)
	if err != nil {
		t.Fatal(err)
	}
	if !layout.addressInKeys || layout.addressIndex != 2 {
		t.Fatalf("expected address in keys[2], got inKeys=%v index=%d", layout.addressInKeys, layout.addressIndex)
	}
	if layout.hashInKeys || layout.hashIndex != 1 {
		t.Fatalf("expected class hash in data[1], got inKeys=%v index=%d", layout.hashInKeys, layout.hashIndex)
	}
}

func TestExtractDeployedAddress_BoundsCheck(t *testing.T) {
	layout := &udcLayout{addressInKeys: true, addressIndex: 5}
	keys := []*felt.Felt{new(felt.Felt), new(felt.Felt)}
	data := []*felt.Felt{}

	_, err := extractDeployedAddress(layout, keys, data)
	if err == nil {
		t.Fatal("expected bounds error")
	}
	if !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("expected 'out of range' in error, got: %v", err)
	}
}

func TestExtractClassHash_BoundsCheck(t *testing.T) {
	layout := &udcLayout{hashInKeys: false, hashIndex: 10}
	keys := []*felt.Felt{}
	data := []*felt.Felt{new(felt.Felt)}

	_, err := extractClassHash(layout, keys, data)
	if err == nil {
		t.Fatal("expected bounds error")
	}
	if !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("expected 'out of range' in error, got: %v", err)
	}
}

func TestHandleDiscoveryEvent_V0EventAutoDetected(t *testing.T) {
	st := memory.New()
	ctx := context.Background()

	classHash := "0xABCDEF"
	e := newDiscoveryTestEngine(st, classHash)

	deployedAddr := new(felt.Felt).SetUint64(0x12345)
	classHashFelt, _ := new(felt.Felt).SetString(classHash)

	// Send a v0-shaped event.
	raw := makeUDCEventV0(deployedAddr, classHashFelt, 100)
	e.handleDiscoveryEvent(ctx, &raw)

	e.mu.RLock()
	count := len(e.contracts)
	e.mu.RUnlock()

	if count != 1 {
		t.Fatalf("expected 1 registered contract from v0 event, got %d", count)
	}

	// Verify address is correct (from data[0], not keys[1]).
	e.mu.RLock()
	cs := e.contracts[0]
	e.mu.RUnlock()

	if cs.config.Address != deployedAddr.String() {
		t.Fatalf("expected address %s, got %s", deployedAddr.String(), cs.config.Address)
	}
}

func TestHandleDiscoveryEvent_ExplicitV1Override(t *testing.T) {
	st := memory.New()
	ctx := context.Background()

	classHash := "0xABCDEF"
	e := newDiscoveryTestEngine(st, classHash)
	e.discovery.udcFormat = &config.UDCEventFormat{Version: "v1"}

	deployedAddr := new(felt.Felt).SetUint64(0x12345)
	classHashFelt, _ := new(felt.Felt).SetString(classHash)

	// Send a v1-shaped event with explicit v1 config.
	raw := makeUDCEvent(deployedAddr, classHashFelt, 100)
	e.handleDiscoveryEvent(ctx, &raw)

	e.mu.RLock()
	count := len(e.contracts)
	e.mu.RUnlock()

	if count != 1 {
		t.Fatalf("expected 1 registered contract with explicit v1, got %d", count)
	}
}

func TestHandleDiscoveryEvent_ExplicitV0Override(t *testing.T) {
	st := memory.New()
	ctx := context.Background()

	classHash := "0xABCDEF"
	e := newDiscoveryTestEngine(st, classHash)
	e.discovery.udcFormat = &config.UDCEventFormat{Version: "v0"}

	deployedAddr := new(felt.Felt).SetUint64(0x12345)
	classHashFelt, _ := new(felt.Felt).SetString(classHash)

	// Send a v0-shaped event with explicit v0 config.
	raw := makeUDCEventV0(deployedAddr, classHashFelt, 100)
	e.handleDiscoveryEvent(ctx, &raw)

	e.mu.RLock()
	count := len(e.contracts)
	e.mu.RUnlock()

	if count != 1 {
		t.Fatalf("expected 1 registered contract with explicit v0, got %d", count)
	}
}

func TestHandleDiscoveryEvent_MalformedEvent_NeitherPattern(t *testing.T) {
	st := memory.New()
	ctx := context.Background()

	classHash := "0xABCDEF"
	e := newDiscoveryTestEngine(st, classHash)

	// 2 keys, 2 data — matches neither v0 nor v1.
	udcAddr, _ := new(felt.Felt).SetString(UDCAddress)
	selector := abi.ComputeSelector("ContractDeployed")
	raw := provider.RawEvent{
		BlockNumber:     100,
		ContractAddress: udcAddr,
		Keys:            []*felt.Felt{selector, new(felt.Felt).SetUint64(1)},
		Data:            []*felt.Felt{new(felt.Felt).SetUint64(2), new(felt.Felt).SetUint64(3)},
	}

	e.handleDiscoveryEvent(ctx, &raw)

	e.mu.RLock()
	count := len(e.contracts)
	e.mu.RUnlock()

	if count != 0 {
		t.Fatalf("expected 0 contracts for unrecognized event shape, got %d", count)
	}
}

func TestHandleDiscoveryEvent_FineGrainedIndexOverride(t *testing.T) {
	st := memory.New()
	ctx := context.Background()

	classHash := "0xABCDEF"
	e := newDiscoveryTestEngine(st, classHash)

	// Custom layout: address in keys[2], class hash in data[1].
	e.discovery.udcFormat = &config.UDCEventFormat{
		AddressKey:    intPtr(2),
		ClassHashData: intPtr(1),
	}

	classHashFelt, _ := new(felt.Felt).SetString(classHash)
	deployedAddr := new(felt.Felt).SetUint64(0xBEEF)

	// Build a v1-shaped event but put the address at keys[2] and classHash at data[1].
	selector := abi.ComputeSelector("ContractDeployed")
	udcAddr, _ := new(felt.Felt).SetString(UDCAddress)
	raw := provider.RawEvent{
		BlockNumber:     200,
		ContractAddress: udcAddr,
		Keys:            []*felt.Felt{selector, new(felt.Felt).SetUint64(0), deployedAddr, new(felt.Felt).SetUint64(1)},
		Data:            []*felt.Felt{new(felt.Felt).SetUint64(0), classHashFelt},
	}

	e.handleDiscoveryEvent(ctx, &raw)

	e.mu.RLock()
	count := len(e.contracts)
	e.mu.RUnlock()

	if count != 1 {
		t.Fatalf("expected 1 contract with fine-grained overrides, got %d", count)
	}

	e.mu.RLock()
	cs := e.contracts[0]
	e.mu.RUnlock()

	if cs.config.Address != deployedAddr.String() {
		t.Fatalf("expected address from keys[2], got %s", cs.config.Address)
	}
}

func TestSetupDiscovery_LogsUDCFormat(t *testing.T) {
	// Ensure setupDiscovery does not fail when UDCEvent is configured.
	classHash := "0x01234567890abcdef01234567890abcdef01234567890abcdef0123456789ab"
	e := &Engine{
		cfg: &config.Config{
			Indexer: config.IndexerConfig{
				UDCAddress: UDCAddress,
				UDCEvent:   &config.UDCEventFormat{Version: "v0"},
			},
			Discover: []config.DiscoverConfig{
				testDiscoverConfig(classHash),
			},
		},
		logger: noopLogger(),
	}

	if err := e.setupDiscovery(); err != nil {
		t.Fatal(err)
	}
	if e.discovery.udcFormat == nil {
		t.Fatal("expected udcFormat to be stored in discovery state")
	}
	if e.discovery.udcFormat.Version != "v0" {
		t.Fatalf("expected version v0, got %s", e.discovery.udcFormat.Version)
	}
}

// --- UDC Event Config Validation Tests ---

func TestValidate_UDCEventFormat(t *testing.T) {
	baseCfg := config.Config{
		Network:  "mainnet",
		RPC:      "wss://example.com",
		Database: config.DatabaseConfig{Backend: "memory"},
		Contracts: []config.ContractConfig{
			{Name: "test", Address: "0xabc", ABI: "fetch",
				Events: []config.EventConfig{{Name: "*", Table: config.TableConfig{Type: "log"}}}},
		},
	}

	tests := []struct {
		name    string
		udc     *config.UDCEventFormat
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid auto version",
			udc:  &config.UDCEventFormat{Version: "auto"},
		},
		{
			name: "valid v0",
			udc:  &config.UDCEventFormat{Version: "v0"},
		},
		{
			name: "valid v1",
			udc:  &config.UDCEventFormat{Version: "v1"},
		},
		{
			name: "valid empty version (defaults to auto)",
			udc:  &config.UDCEventFormat{},
		},
		{
			name:    "invalid version",
			udc:     &config.UDCEventFormat{Version: "v2"},
			wantErr: true,
			errMsg:  "version",
		},
		{
			name:    "mutual exclusivity: address_key and address_data",
			udc:     &config.UDCEventFormat{AddressKey: intPtr(1), AddressData: intPtr(0)},
			wantErr: true,
			errMsg:  "mutually exclusive",
		},
		{
			name:    "mutual exclusivity: class_hash_key and class_hash_data",
			udc:     &config.UDCEventFormat{ClassHashKey: intPtr(1), ClassHashData: intPtr(0)},
			wantErr: true,
			errMsg:  "mutually exclusive",
		},
		{
			name:    "negative index: address_key",
			udc:     &config.UDCEventFormat{AddressKey: intPtr(-1)},
			wantErr: true,
			errMsg:  "non-negative",
		},
		{
			name:    "negative index: class_hash_data",
			udc:     &config.UDCEventFormat{ClassHashData: intPtr(-2)},
			wantErr: true,
			errMsg:  "non-negative",
		},
		{
			name:    "overrides with explicit v0",
			udc:     &config.UDCEventFormat{Version: "v0", AddressKey: intPtr(2)},
			wantErr: true,
			errMsg:  "not allowed",
		},
		{
			name:    "overrides with explicit v1",
			udc:     &config.UDCEventFormat{Version: "v1", ClassHashData: intPtr(1)},
			wantErr: true,
			errMsg:  "not allowed",
		},
		{
			name: "overrides with auto version is valid",
			udc:  &config.UDCEventFormat{Version: "auto", AddressKey: intPtr(2), ClassHashData: intPtr(1)},
		},
		{
			name: "overrides with empty version is valid",
			udc:  &config.UDCEventFormat{AddressKey: intPtr(2)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := baseCfg
			c.Indexer.UDCEvent = tt.udc
			err := config.Validate(&c)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected validation error")
				}
				if tt.errMsg != "" && !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tt.errMsg)) {
					t.Fatalf("expected error containing %q, got: %v", tt.errMsg, err)
				}
			} else if err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}

// --- Config Validation Tests ---

func TestValidate_DiscoverConfig(t *testing.T) {
	baseCfg := config.Config{
		Network:  "mainnet",
		RPC:      "wss://example.com",
		Database: config.DatabaseConfig{Backend: "memory"},
	}

	tests := []struct {
		name    string
		cfg     config.Config
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid discover config",
			cfg: func() config.Config {
				c := baseCfg
				c.Discover = []config.DiscoverConfig{
					{
						ClassHash: "0xabc123",
						ABI:       "fetch",
						Events:    []config.EventConfig{{Name: "*", Table: config.TableConfig{Type: "log"}}},
					},
				}
				return c
			}(),
		},
		{
			name: "valid discover with group",
			cfg: func() config.Config {
				c := baseCfg
				c.Discover = []config.DiscoverConfig{
					{
						ClassHash: "0xabc123",
						ABI:       "fetch",
						Group:     "my-tokens",
						Events:    []config.EventConfig{{Name: "*", Table: config.TableConfig{Type: "log"}}},
					},
				}
				return c
			}(),
		},
		{
			name: "missing class_hash",
			cfg: func() config.Config {
				c := baseCfg
				c.Discover = []config.DiscoverConfig{
					{ABI: "fetch", Events: []config.EventConfig{{Name: "*", Table: config.TableConfig{Type: "log"}}}},
				}
				return c
			}(),
			wantErr: true,
			errMsg:  "class_hash",
		},
		{
			name: "invalid class_hash format",
			cfg: func() config.Config {
				c := baseCfg
				c.Discover = []config.DiscoverConfig{
					{ClassHash: "not-hex", ABI: "fetch",
						Events: []config.EventConfig{{Name: "*", Table: config.TableConfig{Type: "log"}}}},
				}
				return c
			}(),
			wantErr: true,
			errMsg:  "class_hash",
		},
		{
			name: "duplicate class_hash",
			cfg: func() config.Config {
				c := baseCfg
				c.Discover = []config.DiscoverConfig{
					{ClassHash: "0xabc", ABI: "fetch",
						Events: []config.EventConfig{{Name: "*", Table: config.TableConfig{Type: "log"}}}},
					{ClassHash: "0xabc", ABI: "fetch",
						Events: []config.EventConfig{{Name: "*", Table: config.TableConfig{Type: "log"}}}},
				}
				return c
			}(),
			wantErr: true,
			errMsg:  "duplicate",
		},
		{
			name: "missing ABI",
			cfg: func() config.Config {
				c := baseCfg
				c.Discover = []config.DiscoverConfig{
					{ClassHash: "0xabc",
						Events: []config.EventConfig{{Name: "*", Table: config.TableConfig{Type: "log"}}}},
				}
				return c
			}(),
			wantErr: true,
			errMsg:  "abi",
		},
		{
			name: "missing events",
			cfg: func() config.Config {
				c := baseCfg
				c.Discover = []config.DiscoverConfig{
					{ClassHash: "0xabc", ABI: "fetch"},
				}
				return c
			}(),
			wantErr: true,
			errMsg:  "events",
		},
		{
			name: "invalid group name",
			cfg: func() config.Config {
				c := baseCfg
				c.Discover = []config.DiscoverConfig{
					{ClassHash: "0xabc", ABI: "fetch", Group: "UPPER_CASE",
						Events: []config.EventConfig{{Name: "*", Table: config.TableConfig{Type: "log"}}}},
				}
				return c
			}(),
			wantErr: true,
			errMsg:  "group",
		},
		{
			name: "discover without contracts is valid",
			cfg: func() config.Config {
				c := baseCfg
				c.Discover = []config.DiscoverConfig{
					{ClassHash: "0xabc", ABI: "fetch",
						Events: []config.EventConfig{{Name: "*", Table: config.TableConfig{Type: "log"}}}},
				}
				// No contracts!
				return c
			}(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := config.Validate(&tt.cfg)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected validation error")
				}
				if tt.errMsg != "" && !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tt.errMsg)) {
					t.Fatalf("expected error containing %q, got: %v", tt.errMsg, err)
				}
			} else if err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}

// --- HandleReorg Integration ---

// --- Shared Tables for Discovery Tests ---

func TestDiscovery_SharedTables_TwoContractsOneTableSet(t *testing.T) {
	st := memory.New()
	ctx := context.Background()

	classHash := "0xABCDEF"
	e := newSharedDiscoveryTestEngine(st, classHash)
	classHashFelt, _ := new(felt.Felt).SetString(classHash)

	// Discover two contracts with the same class hash.
	addr1 := new(felt.Felt).SetUint64(0x1111)
	addr2 := new(felt.Felt).SetUint64(0x2222)

	raw1 := makeUDCEvent(addr1, classHashFelt, 100)
	e.handleDiscoveryEvent(ctx, &raw1)

	raw2 := makeUDCEvent(addr2, classHashFelt, 101)
	e.handleDiscoveryEvent(ctx, &raw2)

	// Both contracts should be registered.
	e.mu.RLock()
	count := len(e.contracts)
	e.mu.RUnlock()
	if count != 2 {
		t.Fatalf("expected 2 registered contracts, got %d", count)
	}

	// Both should share the same schema objects (same pointer).
	e.mu.RLock()
	schemas1 := e.contracts[0].schemas
	schemas2 := e.contracts[1].schemas
	e.mu.RUnlock()

	if len(schemas1) == 0 {
		t.Fatal("expected non-empty schemas")
	}

	// Verify schemas are the same map (shared references).
	for eventName, s1 := range schemas1 {
		s2, ok := schemas2[eventName]
		if !ok {
			t.Fatalf("second contract missing schema for event %s", eventName)
		}
		if s1 != s2 {
			t.Fatalf("schemas for event %s should be the same shared reference", eventName)
		}
	}

	// Verify shared table naming: {abi_name}_{event_name}, not {contract_name}_{event_name}.
	for _, sch := range schemas1 {
		if !strings.HasPrefix(sch.Name, "optiontoken_") {
			t.Fatalf("shared table name should start with 'optiontoken_', got %s", sch.Name)
		}
		if !sch.SharedTable {
			t.Fatalf("schema %s should have SharedTable=true", sch.Name)
		}
	}

	// Verify sharedSchemas cache was populated.
	cachedSchemas := e.discovery.sharedSchemas[classHash]
	if cachedSchemas == nil {
		t.Fatal("expected sharedSchemas cache to be populated for class hash")
	}
}

func TestDiscovery_SharedTables_ContractAddressColumnPresent(t *testing.T) {
	st := memory.New()
	ctx := context.Background()

	classHash := "0xABCDEF"
	e := newSharedDiscoveryTestEngine(st, classHash)
	classHashFelt, _ := new(felt.Felt).SetString(classHash)

	addr := new(felt.Felt).SetUint64(0x1111)
	raw := makeUDCEvent(addr, classHashFelt, 100)
	e.handleDiscoveryEvent(ctx, &raw)

	e.mu.RLock()
	defer e.mu.RUnlock()

	if len(e.contracts) != 1 {
		t.Fatalf("expected 1 contract, got %d", len(e.contracts))
	}

	// Verify contract_address column is present in shared schemas.
	for _, sch := range e.contracts[0].schemas {
		hasContractAddr := false
		hasContractName := false
		for _, col := range sch.Columns {
			if col.Name == "contract_address" {
				hasContractAddr = true
			}
			if col.Name == "contract_name" {
				hasContractName = true
			}
		}
		if !hasContractAddr {
			t.Fatalf("shared table %s missing contract_address column", sch.Name)
		}
		if !hasContractName {
			t.Fatalf("shared table %s missing contract_name column (added for shared tables)", sch.Name)
		}
	}
}

func TestDiscovery_SharedTables_ContractConfigFlags(t *testing.T) {
	st := memory.New()
	ctx := context.Background()

	classHash := "0xABCDEF"
	e := newSharedDiscoveryTestEngine(st, classHash)
	classHashFelt, _ := new(felt.Felt).SetString(classHash)

	addr := new(felt.Felt).SetUint64(0x3333)
	raw := makeUDCEvent(addr, classHashFelt, 100)
	e.handleDiscoveryEvent(ctx, &raw)

	e.mu.RLock()
	defer e.mu.RUnlock()

	if len(e.contracts) != 1 {
		t.Fatalf("expected 1 contract, got %d", len(e.contracts))
	}

	cs := e.contracts[0]
	if !cs.config.SharedTables {
		t.Fatal("expected SharedTables=true on discovered contract config")
	}
	if cs.config.FactoryName != "OptionToken" {
		t.Fatalf("expected FactoryName='OptionToken', got %s", cs.config.FactoryName)
	}
	if !cs.config.Dynamic {
		t.Fatal("expected Dynamic=true")
	}
}

func TestDiscovery_SharedTables_DeregistrationSkipsSharedTables(t *testing.T) {
	st := memory.New()
	ctx := context.Background()

	classHash := "0xABCDEF"
	e := newSharedDiscoveryTestEngine(st, classHash)
	classHashFelt, _ := new(felt.Felt).SetString(classHash)

	// Discover two contracts sharing tables.
	addr1 := new(felt.Felt).SetUint64(0x4444)
	addr2 := new(felt.Felt).SetUint64(0x5555)

	raw1 := makeUDCEvent(addr1, classHashFelt, 100)
	e.handleDiscoveryEvent(ctx, &raw1)

	raw2 := makeUDCEvent(addr2, classHashFelt, 101)
	e.handleDiscoveryEvent(ctx, &raw2)

	e.mu.RLock()
	if len(e.contracts) != 2 {
		t.Fatalf("expected 2 contracts, got %d", len(e.contracts))
	}
	firstName := e.contracts[0].config.Name
	e.mu.RUnlock()

	// Deregister the first contract with dropTables=true.
	if err := e.DeregisterContract(ctx, firstName, true); err != nil {
		t.Fatalf("deregister failed: %v", err)
	}

	// Verify only 1 contract remains.
	e.mu.RLock()
	remaining := len(e.contracts)
	e.mu.RUnlock()
	if remaining != 1 {
		t.Fatalf("expected 1 remaining contract, got %d", remaining)
	}

	// The shared table should NOT have been dropped (SharedTable flag prevents it).
	// Verify the second contract can still see its schemas.
	e.mu.RLock()
	for _, sch := range e.contracts[0].schemas {
		if !sch.SharedTable {
			t.Fatalf("remaining contract's schema %s should still have SharedTable=true", sch.Name)
		}
	}
	e.mu.RUnlock()
}

// --- RegisterContract with SharedTables Tests ---

func TestRegisterContract_SharedTables_ProducesSharedSchemas(t *testing.T) {
	// Simulate an admin-registered contract with shared tables.
	childABI := testChildABI()
	cc := &config.ContractConfig{
		Name:         "NewOptionToken",
		Address:      "0x123",
		ABI:          "fetch",
		SharedTables: true,
		FactoryName:  "OptionToken",
		Events:       []config.EventConfig{{Name: "*", Table: config.TableConfig{Type: "log"}}},
	}

	// Test that RegisterContract's schema building path uses BuildOptions
	// when SharedTables and FactoryName are set.
	registry := abi.NewEventRegistry(childABI)

	var buildOpts *schema.BuildOptions
	if cc.SharedTables && cc.FactoryName != "" {
		buildOpts = &schema.BuildOptions{
			SharedTable: true,
			FactoryName: cc.FactoryName,
		}
	}
	schemas := schema.BuildSchemas(cc, childABI, registry, buildOpts)

	// Verify shared table naming.
	for _, sch := range schemas {
		if !strings.HasPrefix(sch.Name, "optiontoken_") {
			t.Fatalf("expected shared table name starting with 'optiontoken_', got %s", sch.Name)
		}
		if !sch.SharedTable {
			t.Fatalf("expected SharedTable=true on schema %s", sch.Name)
		}
		if sch.Contract != "OptionToken" {
			t.Fatalf("expected Contract='OptionToken', got %s", sch.Contract)
		}
	}
}

func TestRegisterContract_NoSharedTables_NormalSchemas(t *testing.T) {
	// Verify that without SharedTables+FactoryName, nil buildOpts are used.
	childABI := testChildABI()
	cc := &config.ContractConfig{
		Name:   "MyContract",
		Events: []config.EventConfig{{Name: "*", Table: config.TableConfig{Type: "log"}}},
	}

	registry := abi.NewEventRegistry(childABI)
	schemas := schema.BuildSchemas(cc, childABI, registry, nil)

	for _, sch := range schemas {
		if !strings.HasPrefix(sch.Name, "mycontract_") {
			t.Fatalf("expected normal table name starting with 'mycontract_', got %s", sch.Name)
		}
		if sch.SharedTable {
			t.Fatalf("expected SharedTable=false on schema %s", sch.Name)
		}
	}
}

// --- Config Validation for shared_tables ---

func TestValidate_DiscoverSharedTables(t *testing.T) {
	baseCfg := config.Config{
		Network:  "mainnet",
		RPC:      "wss://example.com",
		Database: config.DatabaseConfig{Backend: "memory"},
	}

	tests := []struct {
		name    string
		cfg     config.Config
		wantErr bool
		errMsg  string
	}{
		{
			name: "shared_tables with named ABI is valid",
			cfg: func() config.Config {
				c := baseCfg
				c.Discover = []config.DiscoverConfig{
					{
						ClassHash:    "0xabc123",
						ABI:          "OptionToken",
						SharedTables: true,
						Events:       []config.EventConfig{{Name: "*", Table: config.TableConfig{Type: "log"}}},
					},
				}
				return c
			}(),
		},
		{
			name: "shared_tables with fetch ABI is rejected",
			cfg: func() config.Config {
				c := baseCfg
				c.Discover = []config.DiscoverConfig{
					{
						ClassHash:    "0xabc123",
						ABI:          "fetch",
						SharedTables: true,
						Events:       []config.EventConfig{{Name: "*", Table: config.TableConfig{Type: "log"}}},
					},
				}
				return c
			}(),
			wantErr: true,
			errMsg:  "named ABI",
		},
		{
			name: "shared_tables with file path ABI is rejected",
			cfg: func() config.Config {
				c := baseCfg
				c.Discover = []config.DiscoverConfig{
					{
						ClassHash:    "0xabc123",
						ABI:          "./abis/token.json",
						SharedTables: true,
						Events:       []config.EventConfig{{Name: "*", Table: config.TableConfig{Type: "log"}}},
					},
				}
				return c
			}(),
			wantErr: true,
			errMsg:  "named ABI",
		},
		{
			name: "shared_tables=false with fetch is fine",
			cfg: func() config.Config {
				c := baseCfg
				c.Discover = []config.DiscoverConfig{
					{
						ClassHash: "0xabc123",
						ABI:       "fetch",
						Events:    []config.EventConfig{{Name: "*", Table: config.TableConfig{Type: "log"}}},
					},
				}
				return c
			}(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := config.Validate(&tt.cfg)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected validation error")
				}
				if tt.errMsg != "" && !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tt.errMsg)) {
					t.Fatalf("expected error containing %q, got: %v", tt.errMsg, err)
				}
			} else if err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}

func TestHandleReorg_WithDiscoveredContracts(t *testing.T) {
	st := memory.New()
	ctx := context.Background()

	classHash := "0xABCDEF"
	e := newDiscoveryTestEngine(st, classHash)
	classHashFelt, _ := new(felt.Felt).SetString(classHash)

	// Discover contracts at blocks 100 and 101.
	for i := uint64(0); i < 2; i++ {
		addr := new(felt.Felt).SetUint64(0x4000 + i)
		raw := makeUDCEvent(addr, classHashFelt, 100+i)
		e.handleDiscoveryEvent(ctx, &raw)
	}

	e.mu.RLock()
	if len(e.contracts) != 2 {
		t.Fatalf("expected 2 contracts before reorg, got %d", len(e.contracts))
	}
	e.mu.RUnlock()

	// Reorg block 101 — should deregister the contract discovered at block 101.
	e.reorgDiscoveredContracts(ctx, 101, 101)

	e.mu.RLock()
	remaining := len(e.contracts)
	e.mu.RUnlock()

	if remaining != 1 {
		t.Fatalf("expected 1 contract after reorg, got %d", remaining)
	}

	// Verify the remaining contract was from block 100.
	e.mu.RLock()
	if e.contracts[0].config.StartBlock == nil || *e.contracts[0].config.StartBlock != 100 {
		t.Fatalf("remaining contract should be from block 100, got %v", e.contracts[0].config.StartBlock)
	}
	e.mu.RUnlock()
}
