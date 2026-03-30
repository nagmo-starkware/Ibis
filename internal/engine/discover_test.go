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
		Indexer:  config.IndexerConfig{StartBlock: 0},
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
	if cs.config.StartBlock != 100 {
		t.Fatalf("expected StartBlock 100, got %d", cs.config.StartBlock)
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
		cfg:   &config.Config{Indexer: config.IndexerConfig{StartBlock: 100}},
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
		cfg:   &config.Config{Indexer: config.IndexerConfig{StartBlock: 200}},
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
		cfg:   &config.Config{Indexer: config.IndexerConfig{StartBlock: 0}},
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
		StartBlock:        100,
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
	if restored.StartBlock != original.StartBlock {
		t.Fatalf("StartBlock: expected %d, got %d", original.StartBlock, restored.StartBlock)
	}
	if !restored.Dynamic {
		t.Fatal("Dynamic should survive JSON round-trip")
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
	if e.contracts[0].config.StartBlock != 100 {
		t.Fatalf("remaining contract should be from block 100, got %d", e.contracts[0].config.StartBlock)
	}
	e.mu.RUnlock()
}
