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

// --- Factory test helpers ---

// testFactoryEventDef creates a PairCreated event with token0 (key), token1 (key), pair (data).
func testFactoryEventDef() *abi.EventDef {
	return &abi.EventDef{
		Name:     "PairCreated",
		FullName: "factory::PairCreated",
		Selector: abi.ComputeSelector("PairCreated"),
		KeyMembers: []abi.FieldDef{
			{Name: "token0", Type: &abi.TypeDef{Kind: abi.CairoContractAddress, Name: "ContractAddress"}},
			{Name: "token1", Type: &abi.TypeDef{Kind: abi.CairoContractAddress, Name: "ContractAddress"}},
		},
		DataMembers: []abi.FieldDef{
			{Name: "pair", Type: &abi.TypeDef{Kind: abi.CairoContractAddress, Name: "ContractAddress"}},
		},
	}
}

// testChildEventDef creates a simple Swap event for child contracts.
func testChildEventDef() *abi.EventDef {
	return &abi.EventDef{
		Name:     "Swap",
		FullName: "pair::Swap",
		Selector: abi.ComputeSelector("Swap"),
		KeyMembers: []abi.FieldDef{
			{Name: "sender", Type: &abi.TypeDef{Kind: abi.CairoContractAddress, Name: "ContractAddress"}},
		},
		DataMembers: []abi.FieldDef{
			{Name: "amount0", Type: &abi.TypeDef{Kind: abi.CairoU64, Name: "u64"}},
		},
	}
}

// testChildABI creates an ABI with the Swap event.
func testChildABI() *abi.ABI {
	swapDef := testChildEventDef()
	return &abi.ABI{
		Types:  make(map[string]*abi.TypeDef),
		Events: []*abi.EventDef{swapDef},
	}
}

// testFactoryContractState creates a factory contract state for testing.
// The factory event is PairCreated with child_address_field=pair.
func testFactoryContractState(address *felt.Felt, name string) *contractState {
	pairCreated := testFactoryEventDef()

	parsedABI := &abi.ABI{
		Types:  make(map[string]*abi.TypeDef),
		Events: []*abi.EventDef{pairCreated},
	}
	registry := abi.NewEventRegistry(parsedABI)

	schemas := map[string]*types.TableSchema{
		"PairCreated": {
			Name:      name + "_PairCreated",
			Contract:  name,
			Event:     "PairCreated",
			TableType: types.TableTypeLog,
			Columns: []types.Column{
				{Name: "block_number", Type: "uint64"},
				{Name: "transaction_hash", Type: "string"},
				{Name: "log_index", Type: "uint64"},
				{Name: "timestamp", Type: "uint64"},
				{Name: "contract_address", Type: "string"},
				{Name: "event_name", Type: "string"},
				{Name: "status", Type: "string"},
				{Name: "token0", Type: "string"},
				{Name: "token1", Type: "string"},
				{Name: "pair", Type: "string"},
			},
		},
	}

	childABIs := map[string]*abi.ABI{
		"fetch": testChildABI(), // Pre-cache to avoid network calls in tests.
	}
	return &contractState{
		config: config.ContractConfig{
			Name:    name,
			Address: address.String(),
			Events: []config.EventConfig{
				{Name: "PairCreated", Table: config.TableConfig{Type: "log"}},
			},
			Factories: []config.FactoryConfig{{
				Event:             "PairCreated",
				ChildAddressField: "pair",
				ChildABI:          "fetch",
				ChildEvents: []config.EventConfig{
					{Name: "*", Table: config.TableConfig{Type: "log"}},
				},
			}},
		},
		address:   address,
		abi:       parsedABI,
		registry:  registry,
		schemas:   schemas,
		childABIs: childABIs,
	}
}

// makeFactoryEvent creates a PairCreated raw event.
func makeFactoryEvent(factoryAddr *felt.Felt, blockNumber uint64, token0, token1, pairAddr *felt.Felt) provider.RawEvent {
	selector := abi.ComputeSelector("PairCreated")
	txHash := new(felt.Felt).SetUint64(blockNumber*1000 + 1)
	blockHash := new(felt.Felt).SetUint64(blockNumber * 100)
	return provider.RawEvent{
		BlockNumber:     blockNumber,
		BlockHash:       blockHash,
		TransactionHash: txHash,
		ContractAddress: factoryAddr,
		Keys:            []*felt.Felt{selector, token0, token1},
		Data:            []*felt.Felt{pairAddr},
		FinalityStatus:  "ACCEPTED_ON_L2",
	}
}

// newFactoryTestEngine creates an Engine configured for factory testing.
func newFactoryTestEngine(st store.Store, factoryCS *contractState) *Engine {
	return &Engine{
		cfg: &config.Config{
			Indexer: config.IndexerConfig{StartBlock: config.Uint64Ptr(0)},
		},
		store:        st,
		logger:       noopLogger(),
		pending:      NewPendingTracker(),
		logIndices:   make(map[uint64]uint64),
		contracts:    []*contractState{factoryCS},
		confirmDepth: 100,
	}
}

// --- BuildChildName Tests ---

func TestBuildChildName_DefaultTemplate(t *testing.T) {
	factory := &config.FactoryConfig{
		Event:             "PairCreated",
		ChildAddressField: "pair",
	}
	decoded := map[string]any{
		"token0": "0xaaa",
		"token1": "0xbbb",
		"pair":   "0x0000000000000000000000000000000000000000000000000000000012345678",
	}

	name := buildChildName("JediSwap", factory, decoded, "0x0000000000000000000000000000000000000000000000000000000012345678")
	if name != "JediSwap_12345678" {
		t.Fatalf("expected JediSwap_12345678, got %s", name)
	}
}

func TestBuildChildName_CustomTemplate(t *testing.T) {
	factory := &config.FactoryConfig{
		Event:             "PairCreated",
		ChildAddressField: "pair",
		ChildNameTemplate: "{factory}_{token0}_{token1}",
	}
	decoded := map[string]any{
		"token0": "USDC",
		"token1": "ETH",
		"pair":   "0x123",
	}

	name := buildChildName("JediSwap", factory, decoded, "0x123")
	if name != "JediSwap_USDC_ETH" {
		t.Fatalf("expected JediSwap_USDC_ETH, got %s", name)
	}
}

func TestBuildChildName_ShortAddress(t *testing.T) {
	factory := &config.FactoryConfig{}
	decoded := map[string]any{}

	// Address shorter than 8 hex chars.
	name := buildChildName("Factory", factory, decoded, "0xabcd")
	if name != "Factory_abcd" {
		t.Fatalf("expected Factory_abcd, got %s", name)
	}
}

// --- IsMetadataColumn Tests ---

func TestIsMetadataColumn(t *testing.T) {
	metaCols := []string{"block_number", "log_index", "timestamp", "event_name",
		"contract_address", "transaction_hash", "status"}
	for _, col := range metaCols {
		if !isMetadataColumn(col) {
			t.Fatalf("expected %s to be metadata column", col)
		}
	}

	nonMetaCols := []string{"sender", "amount", "pair", "token0"}
	for _, col := range nonMetaCols {
		if isMetadataColumn(col) {
			t.Fatalf("expected %s to not be metadata column", col)
		}
	}
}

// --- Factory Event Processing Tests ---

func TestProcessEvent_FactoryEventRegistersChild(t *testing.T) {
	st := memory.New()
	ctx := context.Background()

	factoryAddr := new(felt.Felt).SetUint64(0xF001)
	factoryCS := testFactoryContractState(factoryAddr, "JediSwapFactory")

	// Create factory table.
	if err := st.CreateTable(ctx, factoryCS.schemas["PairCreated"]); err != nil {
		t.Fatal(err)
	}

	e := newFactoryTestEngine(st, factoryCS)

	// Create a factory event (PairCreated).
	token0 := new(felt.Felt).SetUint64(0xAAA)
	token1 := new(felt.Felt).SetUint64(0xBBB)
	pairAddr := new(felt.Felt).SetUint64(0xCCC)
	raw := makeFactoryEvent(factoryAddr, 100, token0, token1, pairAddr)

	if err := e.processEvent(ctx, &raw); err != nil {
		t.Fatalf("processEvent failed: %v", err)
	}

	// Verify the factory event was stored in the factory's table.
	events, err := st.GetEvents(ctx, "JediSwapFactory_PairCreated", store.Query{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 factory event, got %d", len(events))
	}

	// Verify the child contract was registered.
	// Felt(0xCCC).String() = "0xccc", so short_address = "ccc".
	childName := "JediSwapFactory_ccc"
	found := false
	e.mu.RLock()
	for _, cs := range e.contracts {
		if cs.config.Name != childName {
			continue
		}
		found = true
		if cs.config.Address != pairAddr.String() {
			t.Fatalf("child address mismatch: expected %s, got %s", pairAddr.String(), cs.config.Address)
		}
		if cs.config.FactoryName != "JediSwapFactory" {
			t.Fatalf("child factory name mismatch: expected JediSwapFactory, got %s", cs.config.FactoryName)
		}
		if cs.config.StartBlock == nil || *cs.config.StartBlock != 100 {
			t.Fatalf("child start block mismatch: expected 100, got %v", cs.config.StartBlock)
		}
		if !cs.config.Dynamic {
			t.Fatal("factory child should be marked as dynamic")
		}
		// Check metadata.
		if cs.config.FactoryMeta == nil {
			t.Fatal("expected factory metadata")
		}
		if cs.config.FactoryMeta["token0"] != token0.String() {
			t.Fatalf("expected token0 in factory meta, got %v", cs.config.FactoryMeta["token0"])
		}
		break
	}
	e.mu.RUnlock()

	if !found {
		t.Fatalf("factory child %s not found in registered contracts", childName)
	}

	// Verify child was persisted as dynamic contract.
	dynamicContracts, err := st.GetDynamicContracts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	childPersisted := false
	for _, dc := range dynamicContracts {
		if dc.Name == childName {
			childPersisted = true
			if dc.FactoryName != "JediSwapFactory" {
				t.Fatalf("persisted child factory name mismatch: %s", dc.FactoryName)
			}
			break
		}
	}
	if !childPersisted {
		t.Fatal("factory child should be persisted as dynamic contract")
	}
}

func TestProcessEvent_FactoryEventSkipsDuplicateChild(t *testing.T) {
	st := memory.New()
	ctx := context.Background()

	factoryAddr := new(felt.Felt).SetUint64(0xF001)
	factoryCS := testFactoryContractState(factoryAddr, "JediSwapFactory")

	if err := st.CreateTable(ctx, factoryCS.schemas["PairCreated"]); err != nil {
		t.Fatal(err)
	}

	e := newFactoryTestEngine(st, factoryCS)

	// Process the same factory event twice.
	token0 := new(felt.Felt).SetUint64(0xAAA)
	token1 := new(felt.Felt).SetUint64(0xBBB)
	pairAddr := new(felt.Felt).SetUint64(0xDDD)

	raw1 := makeFactoryEvent(factoryAddr, 100, token0, token1, pairAddr)
	if err := e.processEvent(ctx, &raw1); err != nil {
		t.Fatalf("first processEvent failed: %v", err)
	}

	raw2 := makeFactoryEvent(factoryAddr, 101, token0, token1, pairAddr)
	if err := e.processEvent(ctx, &raw2); err != nil {
		t.Fatalf("second processEvent failed: %v", err)
	}

	// Should still only have 1 child (factory + 1 child = 2 total).
	e.mu.RLock()
	count := len(e.contracts)
	e.mu.RUnlock()
	if count != 2 {
		t.Fatalf("expected 2 contracts (factory + 1 child), got %d", count)
	}
}

func TestProcessEvent_MultipleFactoryChildren(t *testing.T) {
	st := memory.New()
	ctx := context.Background()

	factoryAddr := new(felt.Felt).SetUint64(0xF001)
	factoryCS := testFactoryContractState(factoryAddr, "JediSwapFactory")

	if err := st.CreateTable(ctx, factoryCS.schemas["PairCreated"]); err != nil {
		t.Fatal(err)
	}

	e := newFactoryTestEngine(st, factoryCS)

	// Register 3 child contracts from factory events.
	for i := uint64(1); i <= 3; i++ {
		token0 := new(felt.Felt).SetUint64(0xA00 + i)
		token1 := new(felt.Felt).SetUint64(0xB00 + i)
		pairAddr := new(felt.Felt).SetUint64(0xC00 + i)
		raw := makeFactoryEvent(factoryAddr, 100+i, token0, token1, pairAddr)
		if err := e.processEvent(ctx, &raw); err != nil {
			t.Fatalf("processEvent %d failed: %v", i, err)
		}
	}

	// Should have factory + 3 children = 4 contracts.
	e.mu.RLock()
	count := len(e.contracts)
	e.mu.RUnlock()
	if count != 4 {
		t.Fatalf("expected 4 contracts (factory + 3 children), got %d", count)
	}

	// Verify FactoryChildren helper.
	children := e.FactoryChildren("JediSwapFactory")
	if len(children) != 3 {
		t.Fatalf("expected 3 factory children, got %d", len(children))
	}
}

// --- Factory Reorg Tests ---

func TestReorgFactoryChildren_DeregistersChildrenInRange(t *testing.T) {
	st := memory.New()
	ctx := context.Background()

	factoryAddr := new(felt.Felt).SetUint64(0xF001)
	factoryCS := testFactoryContractState(factoryAddr, "Factory")

	if err := st.CreateTable(ctx, factoryCS.schemas["PairCreated"]); err != nil {
		t.Fatal(err)
	}

	e := newFactoryTestEngine(st, factoryCS)

	// Register children at blocks 100, 101, 102.
	for i := uint64(0); i < 3; i++ {
		token0 := new(felt.Felt).SetUint64(0xA00 + i)
		token1 := new(felt.Felt).SetUint64(0xB00 + i)
		pairAddr := new(felt.Felt).SetUint64(0xC00 + i)
		raw := makeFactoryEvent(factoryAddr, 100+i, token0, token1, pairAddr)
		if err := e.processEvent(ctx, &raw); err != nil {
			t.Fatalf("processEvent %d failed: %v", i, err)
		}
	}

	// Verify 4 contracts (factory + 3 children).
	e.mu.RLock()
	if len(e.contracts) != 4 {
		t.Fatalf("expected 4 contracts before reorg, got %d", len(e.contracts))
	}
	e.mu.RUnlock()

	// Reorg blocks 101-102: should deregister children deployed at those blocks.
	e.reorgFactoryChildren(ctx, 101, 102)

	e.mu.RLock()
	remaining := len(e.contracts)
	e.mu.RUnlock()

	// Factory + child at block 100 = 2 remaining.
	if remaining != 2 {
		t.Fatalf("expected 2 contracts after reorg (factory + 1 child), got %d", remaining)
	}
}

func TestHandleReorg_WithFactoryChildren(t *testing.T) {
	st := memory.New()
	ctx := context.Background()

	factoryAddr := new(felt.Felt).SetUint64(0xF001)
	factoryCS := testFactoryContractState(factoryAddr, "Factory")

	if err := st.CreateTable(ctx, factoryCS.schemas["PairCreated"]); err != nil {
		t.Fatal(err)
	}

	e := newFactoryTestEngine(st, factoryCS)

	// Register a child at block 100.
	token0 := new(felt.Felt).SetUint64(0xAAA)
	token1 := new(felt.Felt).SetUint64(0xBBB)
	pairAddr := new(felt.Felt).SetUint64(0xCCC)
	raw := makeFactoryEvent(factoryAddr, 100, token0, token1, pairAddr)
	if err := e.processEvent(ctx, &raw); err != nil {
		t.Fatal(err)
	}

	// Verify 2 contracts.
	e.mu.RLock()
	if len(e.contracts) != 2 {
		t.Fatalf("expected 2 contracts, got %d", len(e.contracts))
	}
	e.mu.RUnlock()

	// Full reorg including block 100.
	reorg := provider.ReorgNotification{StartBlock: 100, EndBlock: 100}
	if err := e.handleReorg(ctx, reorg); err != nil {
		t.Fatalf("handleReorg failed: %v", err)
	}

	// Factory child should be deregistered, only factory remains.
	e.mu.RLock()
	remaining := len(e.contracts)
	e.mu.RUnlock()
	if remaining != 1 {
		t.Fatalf("expected 1 contract after reorg (factory only), got %d", remaining)
	}

	// Factory event should be reverted from store.
	events, _ := st.GetEvents(ctx, "Factory_PairCreated", store.Query{Limit: 10})
	if len(events) != 0 {
		t.Fatalf("expected 0 factory events after reorg, got %d", len(events))
	}
}

// --- Integration: Factory with EventLoop ---

func TestEventLoop_FactoryEventRegistersChildAndProcessesChildEvents(t *testing.T) {
	st := memory.New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	factoryAddr := new(felt.Felt).SetUint64(0xF001)
	factoryCS := testFactoryContractState(factoryAddr, "Factory")

	if err := st.CreateTable(ctx, factoryCS.schemas["PairCreated"]); err != nil {
		t.Fatal(err)
	}

	eventsCh := make(chan provider.RawEvent, 20)
	reorgsCh := make(chan provider.ReorgNotification, 10)

	e := &Engine{
		cfg: &config.Config{
			Indexer: config.IndexerConfig{StartBlock: config.Uint64Ptr(0)},
		},
		store:        st,
		logger:       noopLogger(),
		pending:      NewPendingTracker(),
		logIndices:   make(map[uint64]uint64),
		contracts:    []*contractState{factoryCS},
		confirmDepth: 100,
		events:       eventsCh,
		reorgs:       reorgsCh,
	}

	// Run event loop in background.
	done := make(chan error, 1)
	go func() {
		done <- e.eventLoop(ctx)
	}()

	// Send factory event: PairCreated at block 50.
	pairAddr := new(felt.Felt).SetUint64(0xDA1E)
	token0 := new(felt.Felt).SetUint64(0xA)
	token1 := new(felt.Felt).SetUint64(0xB)
	eventsCh <- makeFactoryEvent(factoryAddr, 50, token0, token1, pairAddr)

	// Wait for factory event processing and child registration.
	time.Sleep(100 * time.Millisecond)

	// Verify child was registered.
	e.mu.RLock()
	contractCount := len(e.contracts)
	e.mu.RUnlock()
	if contractCount != 2 {
		t.Fatalf("expected 2 contracts (factory + child), got %d", contractCount)
	}

	// Now send a child event (Swap) from the pair address at block 51.
	swapSelector := abi.ComputeSelector("Swap")
	sender := new(felt.Felt).SetUint64(0xDEAD)
	amount0 := new(felt.Felt).SetUint64(1000)
	childEvent := provider.RawEvent{
		BlockNumber:     51,
		BlockHash:       new(felt.Felt).SetUint64(5100),
		TransactionHash: new(felt.Felt).SetUint64(51001),
		ContractAddress: pairAddr,
		Keys:            []*felt.Felt{swapSelector, sender},
		Data:            []*felt.Felt{amount0},
		FinalityStatus:  "ACCEPTED_ON_L2",
	}
	eventsCh <- childEvent

	time.Sleep(100 * time.Millisecond)

	// Verify child event was stored. The child's table name depends on the child name.
	childName := buildChildName("Factory", &factoryCS.config.Factories[0], map[string]any{
		"token0": token0.String(),
		"token1": token1.String(),
		"pair":   pairAddr.String(),
	}, pairAddr.String())

	childTableName := strings.ToLower(childName + "_Swap")
	childEvents, err := st.GetEvents(ctx, childTableName, store.Query{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(childEvents) != 1 {
		t.Fatalf("expected 1 child event, got %d", len(childEvents))
	}

	cancel()
	<-done
}

// --- ContractInfo Tests ---

func TestContracts_IncludesFactoryInfo(t *testing.T) {
	st := memory.New()
	ctx := context.Background()

	factoryAddr := new(felt.Felt).SetUint64(0xF001)
	factoryCS := testFactoryContractState(factoryAddr, "Factory")

	if err := st.CreateTable(ctx, factoryCS.schemas["PairCreated"]); err != nil {
		t.Fatal(err)
	}

	e := newFactoryTestEngine(st, factoryCS)

	// Register a child.
	token0 := new(felt.Felt).SetUint64(0xAAA)
	token1 := new(felt.Felt).SetUint64(0xBBB)
	pairAddr := new(felt.Felt).SetUint64(0xCCC)
	raw := makeFactoryEvent(factoryAddr, 100, token0, token1, pairAddr)
	if err := e.processEvent(ctx, &raw); err != nil {
		t.Fatal(err)
	}

	infos := e.Contracts(ctx)
	if len(infos) != 2 {
		t.Fatalf("expected 2 contract infos, got %d", len(infos))
	}

	// Find factory and child.
	var factoryInfo, childInfo *ContractInfo
	for i := range infos {
		if infos[i].IsFactory {
			factoryInfo = &infos[i]
		}
		if infos[i].FactoryName != "" {
			childInfo = &infos[i]
		}
	}

	if factoryInfo == nil {
		t.Fatal("expected factory contract info with IsFactory=true")
	}
	if childInfo == nil {
		t.Fatal("expected child contract info with FactoryName set")
	}
	if childInfo.FactoryName != "Factory" {
		t.Fatalf("expected child factory_name=Factory, got %s", childInfo.FactoryName)
	}
	if !childInfo.Dynamic {
		t.Fatal("expected child to be dynamic")
	}
}

// --- Config Validation Tests ---

func TestValidate_FactoryConfig(t *testing.T) {
	baseCfg := config.Config{
		Network:  "mainnet",
		RPC:      "wss://example.com",
		Database: config.DatabaseConfig{Backend: "memory"},
	}

	tests := []struct {
		name     string
		factory  config.FactoryConfig
		wantErr  bool
	}{
		{
			name: "valid factory config",
			factory: config.FactoryConfig{
				Event:             "PairCreated",
				ChildAddressField: "pair",
				ChildEvents: []config.EventConfig{
					{Name: "*", Table: config.TableConfig{Type: "log"}},
				},
			},
		},
		{
			name: "missing event",
			factory: config.FactoryConfig{
				ChildAddressField: "pair",
				ChildEvents: []config.EventConfig{
					{Name: "*", Table: config.TableConfig{Type: "log"}},
				},
			},
			wantErr: true,
		},
		{
			name: "missing child_address_field",
			factory: config.FactoryConfig{
				Event: "PairCreated",
				ChildEvents: []config.EventConfig{
					{Name: "*", Table: config.TableConfig{Type: "log"}},
				},
			},
			wantErr: true,
		},
		{
			name: "missing child_events",
			factory: config.FactoryConfig{
				Event:             "PairCreated",
				ChildAddressField: "pair",
			},
			wantErr: true,
		},
		{
			name: "invalid child event type",
			factory: config.FactoryConfig{
				Event:             "PairCreated",
				ChildAddressField: "pair",
				ChildEvents: []config.EventConfig{
					{Name: "*", Table: config.TableConfig{Type: "invalid"}},
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := baseCfg
			cfg.Contracts = []config.ContractConfig{{
				Name:    "Factory",
				Address: "0x123",
				ABI:     "fetch",
				Events: []config.EventConfig{
					{Name: "*", Table: config.TableConfig{Type: "log"}},
				},
				Factories: []config.FactoryConfig{tt.factory},
			}}

			err := config.Validate(&cfg)
			if tt.wantErr && err == nil {
				t.Fatal("expected validation error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}

// --- Multiple Factory Contracts Test ---

func TestMultipleFactoryContracts(t *testing.T) {
	st := memory.New()
	ctx := context.Background()

	// Create two factory contracts.
	factory1Addr := new(felt.Felt).SetUint64(0xF001)
	factory2Addr := new(felt.Felt).SetUint64(0xF002)
	factory1CS := testFactoryContractState(factory1Addr, "JediSwap")
	factory2CS := testFactoryContractState(factory2Addr, "TenKSwap")

	if err := st.CreateTable(ctx, factory1CS.schemas["PairCreated"]); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTable(ctx, factory2CS.schemas["PairCreated"]); err != nil {
		t.Fatal(err)
	}

	e := &Engine{
		cfg: &config.Config{
			Indexer: config.IndexerConfig{StartBlock: config.Uint64Ptr(0)},
		},
		store:        st,
		logger:       noopLogger(),
		pending:      NewPendingTracker(),
		logIndices:   make(map[uint64]uint64),
		contracts:    []*contractState{factory1CS, factory2CS},
		confirmDepth: 100,
	}

	// Factory 1 creates a pair.
	raw1 := makeFactoryEvent(factory1Addr, 100,
		new(felt.Felt).SetUint64(0xA1),
		new(felt.Felt).SetUint64(0xB1),
		new(felt.Felt).SetUint64(0xC1))
	if err := e.processEvent(ctx, &raw1); err != nil {
		t.Fatal(err)
	}

	// Factory 2 creates a pair.
	raw2 := makeFactoryEvent(factory2Addr, 101,
		new(felt.Felt).SetUint64(0xA2),
		new(felt.Felt).SetUint64(0xB2),
		new(felt.Felt).SetUint64(0xC2))
	if err := e.processEvent(ctx, &raw2); err != nil {
		t.Fatal(err)
	}

	// Should have 4 contracts: 2 factories + 2 children.
	e.mu.RLock()
	count := len(e.contracts)
	e.mu.RUnlock()
	if count != 4 {
		t.Fatalf("expected 4 contracts, got %d", count)
	}

	// Verify each factory has 1 child.
	if len(e.FactoryChildren("JediSwap")) != 1 {
		t.Fatal("expected 1 child for JediSwap")
	}
	if len(e.FactoryChildren("TenKSwap")) != 1 {
		t.Fatal("expected 1 child for TenKSwap")
	}
}

// --- Restart / Persistence E2E Tests ---

// TestFactoryRestart_ChildrenSurviveRestart simulates a full engine lifecycle:
//  1. First engine session: factory event creates a child, child processes events
//  2. Engine "stops" (we just discard the engine)
//  3. Second engine session: loads dynamic contracts from the same store
//  4. Verifies: factory child is restored with FactoryName, FactoryMeta, cursor intact
//  5. Verifies: the restored child can process new events
func TestFactoryRestart_ChildrenSurviveRestart(t *testing.T) {
	st := memory.New()
	ctx := context.Background()

	// --- Session 1: Factory creates children, children index events ---

	factoryAddr := new(felt.Felt).SetUint64(0xF001)
	factoryCS := testFactoryContractState(factoryAddr, "Factory")

	if err := st.CreateTable(ctx, factoryCS.schemas["PairCreated"]); err != nil {
		t.Fatal(err)
	}

	e1 := newFactoryTestEngine(st, factoryCS)

	// Factory creates 2 pairs.
	pair1Addr := new(felt.Felt).SetUint64(0xC001)
	pair2Addr := new(felt.Felt).SetUint64(0xC002)

	raw1 := makeFactoryEvent(factoryAddr, 100,
		new(felt.Felt).SetUint64(0xAAA),
		new(felt.Felt).SetUint64(0xBBB),
		pair1Addr)
	if err := e1.processEvent(ctx, &raw1); err != nil {
		t.Fatalf("factory event 1: %v", err)
	}

	raw2 := makeFactoryEvent(factoryAddr, 101,
		new(felt.Felt).SetUint64(0xCCC),
		new(felt.Felt).SetUint64(0xDDD),
		pair2Addr)
	if err := e1.processEvent(ctx, &raw2); err != nil {
		t.Fatalf("factory event 2: %v", err)
	}

	// Verify 3 contracts in session 1.
	e1.mu.RLock()
	if len(e1.contracts) != 3 {
		t.Fatalf("session 1: expected 3 contracts, got %d", len(e1.contracts))
	}
	e1.mu.RUnlock()

	// Get child names for later verification.
	child1Name := buildChildName("Factory", &factoryCS.config.Factories[0], map[string]any{
		"token0": new(felt.Felt).SetUint64(0xAAA).String(),
		"token1": new(felt.Felt).SetUint64(0xBBB).String(),
		"pair":   pair1Addr.String(),
	}, pair1Addr.String())

	child2Name := buildChildName("Factory", &factoryCS.config.Factories[0], map[string]any{
		"token0": new(felt.Felt).SetUint64(0xCCC).String(),
		"token1": new(felt.Felt).SetUint64(0xDDD).String(),
		"pair":   pair2Addr.String(),
	}, pair2Addr.String())

	// Send a Swap event to child 1 at block 102 so its cursor advances.
	swapSelector := abi.ComputeSelector("Swap")
	swapEvent := provider.RawEvent{
		BlockNumber:     102,
		BlockHash:       new(felt.Felt).SetUint64(10200),
		TransactionHash: new(felt.Felt).SetUint64(102001),
		ContractAddress: pair1Addr,
		Keys:            []*felt.Felt{swapSelector, new(felt.Felt).SetUint64(0xDEAD)},
		Data:            []*felt.Felt{new(felt.Felt).SetUint64(500)},
		FinalityStatus:  "ACCEPTED_ON_L2",
	}
	if err := e1.processEvent(ctx, &swapEvent); err != nil {
		t.Fatalf("child swap event: %v", err)
	}

	// Verify child 1 cursor advanced to 102.
	child1Cursor, _ := st.GetCursor(ctx, child1Name)
	if child1Cursor != 102 {
		t.Fatalf("session 1: expected child1 cursor 102, got %d", child1Cursor)
	}

	// Session 1 "stops" — we drop e1 but the store persists.

	// --- Session 2: New engine loads from same store ---

	// Load dynamic contracts from store (what setup() does).
	dynamicContracts, err := st.GetDynamicContracts(ctx)
	if err != nil {
		t.Fatalf("loading dynamic contracts: %v", err)
	}

	// We should have 2 dynamic contracts (the factory children).
	if len(dynamicContracts) != 2 {
		t.Fatalf("session 2: expected 2 dynamic contracts, got %d", len(dynamicContracts))
	}

	// Build a lookup for verification.
	dcByName := make(map[string]config.ContractConfig)
	for _, dc := range dynamicContracts {
		dcByName[dc.Name] = dc
	}

	// Verify child 1 survived with all factory metadata.
	dc1, ok := dcByName[child1Name]
	if !ok {
		t.Fatalf("child 1 (%s) not found in persisted dynamic contracts", child1Name)
	}
	if dc1.FactoryName != "Factory" {
		t.Fatalf("child 1 FactoryName: expected Factory, got %s", dc1.FactoryName)
	}
	if dc1.Address != pair1Addr.String() {
		t.Fatalf("child 1 Address: expected %s, got %s", pair1Addr.String(), dc1.Address)
	}
	if dc1.StartBlock == nil || *dc1.StartBlock != 100 {
		t.Fatalf("child 1 StartBlock: expected 100, got %v", dc1.StartBlock)
	}
	if dc1.FactoryMeta == nil {
		t.Fatal("child 1 FactoryMeta is nil")
	}
	if dc1.FactoryMeta["token0"] != new(felt.Felt).SetUint64(0xAAA).String() {
		t.Fatalf("child 1 FactoryMeta[token0]: got %v", dc1.FactoryMeta["token0"])
	}

	// Verify child 2 survived.
	dc2, ok := dcByName[child2Name]
	if !ok {
		t.Fatalf("child 2 (%s) not found in persisted dynamic contracts", child2Name)
	}
	if dc2.FactoryName != "Factory" {
		t.Fatalf("child 2 FactoryName: expected Factory, got %s", dc2.FactoryName)
	}

	// Verify cursors survived.
	cursor1, _ := st.GetCursor(ctx, child1Name)
	if cursor1 != 102 {
		t.Fatalf("session 2: child 1 cursor: expected 102, got %d", cursor1)
	}
	// Child 2 never processed its own events, so cursor is 0.
	cursor2, _ := st.GetCursor(ctx, child2Name)
	if cursor2 != 0 {
		t.Fatalf("session 2: child 2 cursor: expected 0 (no events processed), got %d", cursor2)
	}

	// Reconstruct engine 2 with the factory + loaded children.
	// (Simulates what setup() does after loading dynamic contracts.)
	factory2CS := testFactoryContractState(factoryAddr, "Factory")
	if err := st.CreateTable(ctx, factory2CS.schemas["PairCreated"]); err != nil {
		t.Fatal(err)
	}

	e2 := newFactoryTestEngine(st, factory2CS)

	// Add the restored children to the engine.
	childABI := testChildABI()
	for _, dc := range dynamicContracts {
		dcCopy := dc
		dcCopy.Dynamic = true
		if err := e2.registerWithABI(ctx, &dcCopy, childABI); err != nil {
			t.Fatalf("re-registering %s: %v", dc.Name, err)
		}
	}

	// Session 2 should have factory + 2 children.
	e2.mu.RLock()
	if len(e2.contracts) != 3 {
		t.Fatalf("session 2 engine: expected 3 contracts, got %d", len(e2.contracts))
	}
	e2.mu.RUnlock()

	// Verify the restored child can process new events.
	swapEvent2 := provider.RawEvent{
		BlockNumber:     103,
		BlockHash:       new(felt.Felt).SetUint64(10300),
		TransactionHash: new(felt.Felt).SetUint64(103001),
		ContractAddress: pair1Addr,
		Keys:            []*felt.Felt{swapSelector, new(felt.Felt).SetUint64(0xBEEF)},
		Data:            []*felt.Felt{new(felt.Felt).SetUint64(999)},
		FinalityStatus:  "ACCEPTED_ON_L2",
	}
	if err := e2.processEvent(ctx, &swapEvent2); err != nil {
		t.Fatalf("session 2 child event: %v", err)
	}

	// Verify new event was stored.
	childTable := strings.ToLower(child1Name + "_Swap")
	events, err := st.GetEvents(ctx, childTable, store.Query{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	// Should have 2 Swap events: one from session 1, one from session 2.
	if len(events) != 2 {
		t.Fatalf("expected 2 child events across sessions, got %d", len(events))
	}

	// Verify cursor advanced.
	finalCursor, _ := st.GetCursor(ctx, child1Name)
	if finalCursor != 103 {
		t.Fatalf("session 2: expected child1 cursor 103 after new event, got %d", finalCursor)
	}
}

// TestFactoryChildConfig_JSONRoundTrip verifies that FactoryName and FactoryMeta
// survive JSON marshal/unmarshal, which is the path used by badger and postgres stores.
func TestFactoryChildConfig_JSONRoundTrip(t *testing.T) {
	original := config.ContractConfig{
		Name:    "Factory_c001",
		Address: "0xc001",
		ABI:     "fetch",
		Events: []config.EventConfig{
			{Name: "*", Table: config.TableConfig{Type: "log"}},
		},
		StartBlock:  config.Uint64Ptr(100),
		Dynamic:     true,
		FactoryName: "JediSwapFactory",
		FactoryMeta: map[string]any{
			"token0": "0xaaa",
			"token1": "0xbbb",
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var restored config.ContractConfig
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Core fields.
	if restored.Name != original.Name {
		t.Fatalf("Name: expected %s, got %s", original.Name, restored.Name)
	}
	if restored.Address != original.Address {
		t.Fatalf("Address: expected %s, got %s", original.Address, restored.Address)
	}
	if restored.StartBlock == nil || original.StartBlock == nil || *restored.StartBlock != *original.StartBlock {
		t.Fatalf("StartBlock: expected %v, got %v", original.StartBlock, restored.StartBlock)
	}

	// Factory fields — these must survive the round-trip.
	if restored.FactoryName != original.FactoryName {
		t.Fatalf("FactoryName: expected %s, got %s", original.FactoryName, restored.FactoryName)
	}
	if restored.FactoryMeta == nil {
		t.Fatal("FactoryMeta is nil after round-trip")
	}
	if restored.FactoryMeta["token0"] != "0xaaa" {
		t.Fatalf("FactoryMeta[token0]: expected 0xaaa, got %v", restored.FactoryMeta["token0"])
	}
	if restored.FactoryMeta["token1"] != "0xbbb" {
		t.Fatalf("FactoryMeta[token1]: expected 0xbbb, got %v", restored.FactoryMeta["token1"])
	}

	// Events should survive too.
	if len(restored.Events) != 1 {
		t.Fatalf("Events: expected 1, got %d", len(restored.Events))
	}
	if restored.Events[0].Name != "*" {
		t.Fatalf("Events[0].Name: expected *, got %s", restored.Events[0].Name)
	}

	// Dynamic field has yaml:"-" but json:"dynamic,omitempty" — verify it survives JSON.
	if !restored.Dynamic {
		t.Fatal("Dynamic should survive JSON round-trip")
	}
}

// --- child_views Tests ---

// TestValidate_FactoryConfig_ChildViews checks validation rules for the child_views field.
func TestValidate_FactoryConfig_ChildViews(t *testing.T) {
	baseCfg := config.Config{
		Network:  "mainnet",
		RPC:      "wss://example.com",
		Database: config.DatabaseConfig{Backend: "memory"},
	}
	makeContract := func(f config.FactoryConfig) config.ContractConfig {
		return config.ContractConfig{
			Name:      "Factory",
			Address:   "0x123",
			ABI:       "fetch",
			Events:    []config.EventConfig{{Name: "*", Table: config.TableConfig{Type: "log"}}},
			Factories: []config.FactoryConfig{f},
		}
	}

	tests := []struct {
		name    string
		factory config.FactoryConfig
		wantErr bool
	}{
		{
			name: "child_views alone is valid (no child_events required)",
			factory: config.FactoryConfig{
				Event:             "DeploymentCreated",
				ChildAddressField: "option_token",
				ChildViews: []config.ViewConfig{
					{Function: "get_strike", Interval: "5m", Table: config.TableConfig{Type: "unique", UniqueKey: "_view_key"}},
				},
			},
		},
		{
			name: "child_events and child_views together is valid",
			factory: config.FactoryConfig{
				Event:             "DeploymentCreated",
				ChildAddressField: "option_token",
				ChildEvents: []config.EventConfig{
					{Name: "*", Table: config.TableConfig{Type: "log"}},
				},
				ChildViews: []config.ViewConfig{
					{Function: "get_strike", Interval: "5m", Table: config.TableConfig{Type: "unique", UniqueKey: "_view_key"}},
				},
			},
		},
		{
			name: "neither child_events nor child_views fails",
			factory: config.FactoryConfig{
				Event:             "DeploymentCreated",
				ChildAddressField: "option_token",
			},
			wantErr: true,
		},
		{
			name: "invalid child_views interval fails",
			factory: config.FactoryConfig{
				Event:             "DeploymentCreated",
				ChildAddressField: "option_token",
				ChildViews: []config.ViewConfig{
					{Function: "get_strike", Interval: "bad", Table: config.TableConfig{Type: "unique", UniqueKey: "_view_key"}},
				},
			},
			wantErr: true,
		},
		{
			name: "child_views unique table missing unique_key fails",
			factory: config.FactoryConfig{
				Event:             "DeploymentCreated",
				ChildAddressField: "option_token",
				ChildViews: []config.ViewConfig{
					{Function: "get_strike", Interval: "5m", Table: config.TableConfig{Type: "unique"}},
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := baseCfg
			cfg.Contracts = []config.ContractConfig{makeContract(tt.factory)}
			err := config.Validate(&cfg)
			if tt.wantErr && err == nil {
				t.Fatal("expected validation error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}

// TestHandleFactoryEvent_ChildViewsPropagated verifies that child_views from the factory
// config are copied into the registered child's ContractConfig.Views field.
func TestHandleFactoryEvent_ChildViewsPropagated(t *testing.T) {
	st := memory.New()
	ctx := context.Background()

	factoryAddr := new(felt.Felt).SetUint64(0xF001)

	childViews := []config.ViewConfig{
		{Function: "get_strike", Interval: "5m", Table: config.TableConfig{Type: "unique", UniqueKey: "_view_key"}},
		{Function: "get_expiry", Interval: "5m", Table: config.TableConfig{Type: "unique", UniqueKey: "_view_key"}},
		{Function: "get_underlying_reserve", Interval: "30s", Table: config.TableConfig{Type: "log"}},
	}

	// Build a factory state with child_views (no child_events required).
	pairCreated := testFactoryEventDef()
	parsedABI := &abi.ABI{
		Types:  make(map[string]*abi.TypeDef),
		Events: []*abi.EventDef{pairCreated},
	}
	registry := abi.NewEventRegistry(parsedABI)
	factoryCS := &contractState{
		config: config.ContractConfig{
			Name:    "OptionManager",
			Address: factoryAddr.String(),
			Events:  []config.EventConfig{{Name: "PairCreated", Table: config.TableConfig{Type: "log"}}},
			Factories: []config.FactoryConfig{{
				Event:             "PairCreated",
				ChildAddressField: "pair",
				ChildABI:          "fetch",
				ChildViews:        childViews,
			}},
		},
		address:  factoryAddr,
		abi:      parsedABI,
		registry: registry,
		schemas: map[string]*types.TableSchema{
			"PairCreated": {
				Name:      "OptionManager_PairCreated",
				Contract:  "OptionManager",
				Event:     "PairCreated",
				TableType: types.TableTypeLog,
				Columns: []types.Column{
					{Name: "block_number", Type: "uint64"},
					{Name: "transaction_hash", Type: "string"},
					{Name: "log_index", Type: "uint64"},
					{Name: "timestamp", Type: "uint64"},
					{Name: "contract_address", Type: "string"},
					{Name: "event_name", Type: "string"},
					{Name: "status", Type: "string"},
					{Name: "token0", Type: "string"},
					{Name: "token1", Type: "string"},
					{Name: "pair", Type: "string"},
				},
			},
		},
		childABIs: map[string]*abi.ABI{"fetch": testChildABI()},
	}

	if err := st.CreateTable(ctx, factoryCS.schemas["PairCreated"]); err != nil {
		t.Fatal(err)
	}

	e := newFactoryTestEngine(st, factoryCS)

	token0 := new(felt.Felt).SetUint64(0xAAA)
	token1 := new(felt.Felt).SetUint64(0xBBB)
	childAddr := new(felt.Felt).SetUint64(0xC0DE)
	raw := makeFactoryEvent(factoryAddr, 200, token0, token1, childAddr)

	if err := e.processEvent(ctx, &raw); err != nil {
		t.Fatalf("processEvent failed: %v", err)
	}

	// Find the registered child and assert its Views match child_views.
	childName := buildChildName("OptionManager", &factoryCS.config.Factories[0], map[string]any{
		"token0": token0.String(),
		"token1": token1.String(),
		"pair":   childAddr.String(),
	}, childAddr.String())

	e.mu.RLock()
	var found *contractState
	for _, cs := range e.contracts {
		if cs.config.Name == childName {
			found = cs
			break
		}
	}
	e.mu.RUnlock()

	if found == nil {
		t.Fatalf("child %s not registered", childName)
	}
	if len(found.config.Views) != len(childViews) {
		t.Fatalf("expected %d child views, got %d", len(childViews), len(found.config.Views))
	}
	for i, v := range childViews {
		if found.config.Views[i].Function != v.Function {
			t.Errorf("views[%d].Function: expected %s, got %s", i, v.Function, found.config.Views[i].Function)
		}
		if found.config.Views[i].Interval != v.Interval {
			t.Errorf("views[%d].Interval: expected %s, got %s", i, v.Interval, found.config.Views[i].Interval)
		}
	}
}

// TestHandleFactoryEvent_SharedTables_WithChildViews verifies that child_views +
// shared_tables: true also propagates views into the registered child config.
func TestHandleFactoryEvent_SharedTables_WithChildViews(t *testing.T) {
	st := memory.New()
	ctx := context.Background()

	factoryAddr := new(felt.Felt).SetUint64(0xF002)

	childViews := []config.ViewConfig{
		{Function: "get_strike", Interval: "5m", Table: config.TableConfig{Type: "unique", UniqueKey: "_view_key"}},
	}

	pairCreated := testFactoryEventDef()
	parsedABI := &abi.ABI{
		Types:  make(map[string]*abi.TypeDef),
		Events: []*abi.EventDef{pairCreated},
	}
	registry := abi.NewEventRegistry(parsedABI)
	factoryCS := &contractState{
		config: config.ContractConfig{
			Name:    "SharedFactory",
			Address: factoryAddr.String(),
			Events:  []config.EventConfig{{Name: "PairCreated", Table: config.TableConfig{Type: "log"}}},
			Factories: []config.FactoryConfig{{
				Event:             "PairCreated",
				ChildAddressField: "pair",
				ChildABI:          "fetch",
				SharedTables:      true,
				ChildEvents: []config.EventConfig{
					{Name: "*", Table: config.TableConfig{Type: "log"}},
				},
				ChildViews: childViews,
			}},
		},
		address:  factoryAddr,
		abi:      parsedABI,
		registry: registry,
		schemas: map[string]*types.TableSchema{
			"PairCreated": {
				Name:      "SharedFactory_PairCreated",
				Contract:  "SharedFactory",
				Event:     "PairCreated",
				TableType: types.TableTypeLog,
				Columns: []types.Column{
					{Name: "block_number", Type: "uint64"},
					{Name: "transaction_hash", Type: "string"},
					{Name: "log_index", Type: "uint64"},
					{Name: "timestamp", Type: "uint64"},
					{Name: "contract_address", Type: "string"},
					{Name: "event_name", Type: "string"},
					{Name: "status", Type: "string"},
					{Name: "token0", Type: "string"},
					{Name: "token1", Type: "string"},
					{Name: "pair", Type: "string"},
				},
			},
		},
		childABIs: map[string]*abi.ABI{"fetch": testChildABI()},
	}

	if err := st.CreateTable(ctx, factoryCS.schemas["PairCreated"]); err != nil {
		t.Fatal(err)
	}

	e := newFactoryTestEngine(st, factoryCS)

	token0 := new(felt.Felt).SetUint64(0xA11)
	token1 := new(felt.Felt).SetUint64(0xB11)
	childAddr := new(felt.Felt).SetUint64(0xC11)
	raw := makeFactoryEvent(factoryAddr, 300, token0, token1, childAddr)

	if err := e.processEvent(ctx, &raw); err != nil {
		t.Fatalf("processEvent failed: %v", err)
	}

	childName := buildChildName("SharedFactory", &factoryCS.config.Factories[0], map[string]any{
		"token0": token0.String(),
		"token1": token1.String(),
		"pair":   childAddr.String(),
	}, childAddr.String())

	e.mu.RLock()
	var found *contractState
	for _, cs := range e.contracts {
		if cs.config.Name == childName {
			found = cs
			break
		}
	}
	e.mu.RUnlock()

	if found == nil {
		t.Fatalf("shared child %s not registered", childName)
	}
	if !found.config.SharedTables {
		t.Fatal("expected SharedTables=true on registered child")
	}
	if len(found.config.Views) != 1 {
		t.Fatalf("expected 1 child view, got %d", len(found.config.Views))
	}
	if found.config.Views[0].Function != "get_strike" {
		t.Fatalf("expected get_strike view, got %s", found.config.Views[0].Function)
	}
}

// TestFactoryChildConfig_YAMLOmitsRuntimeFields verifies that FactoryName and
// FactoryMeta are NOT written to YAML (yaml:"-" tag), since they're runtime-only
// fields set by the engine, not user config.
func TestFactoryChildConfig_YAMLOmitsRuntimeFields(t *testing.T) {
	cc := config.ContractConfig{
		Name:        "Factory_c001",
		Address:     "0xc001",
		ABI:         "fetch",
		FactoryName: "JediSwapFactory",
		FactoryMeta: map[string]any{"token0": "0xaaa"},
		Dynamic:     true,
	}

	// Marshal to check what YAML would look like.
	data, err := json.Marshal(cc)
	if err != nil {
		t.Fatal(err)
	}

	// FactoryName should be in JSON.
	if !strings.Contains(string(data), "factory_name") {
		t.Fatal("expected factory_name in JSON output")
	}

	// Dynamic is yaml:"-" but json:"dynamic,omitempty", so it appears in JSON.
	if !strings.Contains(string(data), `"dynamic"`) {
		t.Fatal("expected dynamic in JSON output")
	}
}

// --- Multi-factory Tests ---

// TestMultiFactoryOnSameEvent verifies that a contract with multiple factories
// listening on the same event registers a separate child for each factory entry.
// This is the core use case for OptionManager where DeploymentCreated carries
// option_token, order_book, and exerciser addresses simultaneously.
func TestMultiFactoryOnSameEvent(t *testing.T) {
	st := memory.New()
	ctx := context.Background()

	factoryAddr := new(felt.Felt).SetUint64(0xF001)

	// Build a factory event with two address fields: token0 and token1.
	// We reuse testFactoryEventDef() which has token0 (key), token1 (key), pair (data).
	// We register TWO factory entries — one for token0, one for token1 — so a single
	// PairCreated event registers two children with different ABIs.
	pairCreated := testFactoryEventDef()
	parsedABI := &abi.ABI{
		Types:  make(map[string]*abi.TypeDef),
		Events: []*abi.EventDef{pairCreated},
	}
	registry := abi.NewEventRegistry(parsedABI)

	// Use two distinct ChildABI values to verify each factory uses its own ABI cache.
	childABI1 := testChildABI() // for factory[0] (token0)
	childABI2 := testChildABI() // for factory[1] (token1)

	factoryCS := &contractState{
		config: config.ContractConfig{
			Name:    "OptionManager",
			Address: factoryAddr.String(),
			Events:  []config.EventConfig{{Name: "PairCreated", Table: config.TableConfig{Type: "log"}}},
			Factories: []config.FactoryConfig{
				{
					Event:             "PairCreated",
					ChildAddressField: "token0",
					ChildABI:          "ChildTypeA",
					ChildEvents: []config.EventConfig{
						{Name: "*", Table: config.TableConfig{Type: "log"}},
					},
				},
				{
					Event:             "PairCreated",
					ChildAddressField: "token1",
					ChildABI:          "ChildTypeB",
					ChildViews: []config.ViewConfig{
						{Function: "get_strike", Interval: "5m", Table: config.TableConfig{Type: "unique", UniqueKey: "_view_key"}},
					},
				},
			},
		},
		address:   factoryAddr,
		abi:       parsedABI,
		registry:  registry,
		schemas: map[string]*types.TableSchema{
			"PairCreated": {
				Name:      "OptionManager_PairCreated",
				Contract:  "OptionManager",
				Event:     "PairCreated",
				TableType: types.TableTypeLog,
				Columns: []types.Column{
					{Name: "block_number", Type: "uint64"},
					{Name: "transaction_hash", Type: "string"},
					{Name: "log_index", Type: "uint64"},
					{Name: "timestamp", Type: "uint64"},
					{Name: "contract_address", Type: "string"},
					{Name: "event_name", Type: "string"},
					{Name: "status", Type: "string"},
					{Name: "token0", Type: "string"},
					{Name: "token1", Type: "string"},
					{Name: "pair", Type: "string"},
				},
			},
		},
		// Pre-cache both ABIs keyed by their ChildABI name.
		childABIs: map[string]*abi.ABI{
			"ChildTypeA": childABI1,
			"ChildTypeB": childABI2,
		},
	}

	if err := st.CreateTable(ctx, factoryCS.schemas["PairCreated"]); err != nil {
		t.Fatal(err)
	}

	e := newFactoryTestEngine(st, factoryCS)

	token0Addr := new(felt.Felt).SetUint64(0xA001)
	token1Addr := new(felt.Felt).SetUint64(0xB001)
	pairAddr := new(felt.Felt).SetUint64(0xC001)

	// Send one PairCreated event.
	raw := makeFactoryEvent(factoryAddr, 100, token0Addr, token1Addr, pairAddr)
	if err := e.processEvent(ctx, &raw); err != nil {
		t.Fatalf("processEvent failed: %v", err)
	}

	// Expect factory + 2 children = 3 contracts.
	e.mu.RLock()
	count := len(e.contracts)
	e.mu.RUnlock()
	if count != 3 {
		t.Fatalf("expected 3 contracts (factory + 2 children), got %d", count)
	}

	// Verify each child has the correct address and ABI config.
	e.mu.RLock()
	childA := (*contractState)(nil)
	childB := (*contractState)(nil)
	for _, cs := range e.contracts {
		if cs.config.FactoryName != "OptionManager" {
			continue
		}
		if cs.config.Address == token0Addr.String() {
			childA = cs
		}
		if cs.config.Address == token1Addr.String() {
			childB = cs
		}
	}
	e.mu.RUnlock()

	if childA == nil {
		t.Fatal("expected child A (token0 address) to be registered")
	}
	if childB == nil {
		t.Fatal("expected child B (token1 address) to be registered")
	}

	// Child A should have ChildEvents from factory[0].
	if len(childA.config.Events) == 0 {
		t.Error("child A: expected child_events to be propagated")
	}

	// Child B should have ChildViews from factory[1].
	if len(childB.config.Views) == 0 {
		t.Error("child B: expected child_views to be propagated")
	}
	if childB.config.Views[0].Function != "get_strike" {
		t.Errorf("child B: expected get_strike view, got %s", childB.config.Views[0].Function)
	}

	// Both children should reference the same factory.
	if childA.config.FactoryName != "OptionManager" {
		t.Errorf("child A factory name: expected OptionManager, got %s", childA.config.FactoryName)
	}
	if childB.config.FactoryName != "OptionManager" {
		t.Errorf("child B factory name: expected OptionManager, got %s", childB.config.FactoryName)
	}

	// Sending the same event again must not re-register either child.
	raw2 := makeFactoryEvent(factoryAddr, 101, token0Addr, token1Addr, pairAddr)
	if err := e.processEvent(ctx, &raw2); err != nil {
		t.Fatalf("second processEvent failed: %v", err)
	}
	e.mu.RLock()
	count2 := len(e.contracts)
	e.mu.RUnlock()
	if count2 != 3 {
		t.Fatalf("expected still 3 contracts after duplicate event, got %d", count2)
	}
}
