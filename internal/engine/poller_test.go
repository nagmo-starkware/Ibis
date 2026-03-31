package engine

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NethermindEth/juno/core/felt"
	"github.com/NethermindEth/starknet.go/rpc"

	"github.com/b-j-roberts/ibis/internal/abi"
	"github.com/b-j-roberts/ibis/internal/config"
	"github.com/b-j-roberts/ibis/internal/store"
	"github.com/b-j-roberts/ibis/internal/store/memory"
	"github.com/b-j-roberts/ibis/internal/types"
)

// --- Mock Provider ---

type mockProvider struct {
	mu          sync.Mutex
	blockNumber uint64
	callResult  []*felt.Felt
	callErr     error
	callCount   atomic.Int64
}

func (m *mockProvider) BlockNumber(_ context.Context) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.blockNumber, nil
}

func (m *mockProvider) Call(_ context.Context, _, _ *felt.Felt, _ []*felt.Felt, _ rpc.BlockID) ([]*felt.Felt, error) {
	m.callCount.Add(1)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.callErr != nil {
		return nil, m.callErr
	}
	// Return a copy to avoid races.
	result := make([]*felt.Felt, len(m.callResult))
	copy(result, m.callResult)
	return result, nil
}

func (m *mockProvider) setBlockNumber(n uint64) {
	m.mu.Lock()
	m.blockNumber = n
	m.mu.Unlock()
}

// --- Helpers ---

// testFunctionDef creates a simple view function definition for testing.
func testFunctionDef(name string) *abi.FunctionDef {
	return &abi.FunctionDef{
		Name:            name,
		FullName:        "test::" + name,
		Selector:        abi.ComputeSelector(name),
		Inputs:          nil,
		Outputs:         []abi.FieldDef{{Name: "value", Type: &abi.TypeDef{Kind: abi.CairoU64, Name: "u64"}}},
		StateMutability: "view",
	}
}

// testFunctionDefMultiOutput creates a view function with multiple outputs.
func testFunctionDefMultiOutput(name string) *abi.FunctionDef {
	return &abi.FunctionDef{
		Name:     name,
		FullName: "test::" + name,
		Selector: abi.ComputeSelector(name),
		Outputs: []abi.FieldDef{
			{Name: "price", Type: &abi.TypeDef{Kind: abi.CairoU128, Name: "u128"}},
			{Name: "decimals", Type: &abi.TypeDef{Kind: abi.CairoU8, Name: "u8"}},
		},
		StateMutability: "view",
	}
}

// testContractStateWithViews creates a contractState with view functions.
func testContractStateWithViews(addr *felt.Felt, name string, funcDefs map[string]*abi.FunctionDef) *contractState {
	return &contractState{
		config: config.ContractConfig{
			Name:    name,
			Address: addr.String(),
			Views: []config.ViewConfig{
				{
					Function: "total_supply",
					Interval: "100ms",
					Table: config.TableConfig{
						Type:      "unique",
						UniqueKey: "_view_key",
					},
				},
			},
		},
		address: addr,
		abi: &abi.ABI{
			Types:     make(map[string]*abi.TypeDef),
			Events:    nil,
			Functions: funcDefs,
		},
		schemas: make(map[string]*types.TableSchema),
	}
}

// --- ViewPoller Setup Tests ---

func TestViewPoller_Setup_ResolvesFunction(t *testing.T) {
	addr := new(felt.Felt).SetUint64(0xABC)
	funcDef := testFunctionDef("total_supply")
	cs := testContractStateWithViews(addr, "MyToken", map[string]*abi.FunctionDef{
		"total_supply": funcDef,
	})

	mp := &mockProvider{blockNumber: 100}
	st := memory.New()
	vp := NewViewPoller(mp, st, noopLogger())

	schemas, err := vp.Setup([]*contractState{cs})
	if err != nil {
		t.Fatalf("Setup failed: %v", err)
	}

	if len(schemas) != 1 {
		t.Fatalf("expected 1 schema, got %d", len(schemas))
	}
	if schemas[0].Name != "mytoken_total_supply" {
		t.Fatalf("expected table name 'mytoken_total_supply', got %s", schemas[0].Name)
	}
	if schemas[0].TableType != types.TableTypeUnique {
		t.Fatal("expected unique table type")
	}
	if schemas[0].UniqueKey != "_view_key" {
		t.Fatalf("expected unique key '_view_key', got %s", schemas[0].UniqueKey)
	}
	if !vp.HasEntries() {
		t.Fatal("expected poller to have entries")
	}
}

func TestViewPoller_Setup_ErrorOnMissingFunction(t *testing.T) {
	addr := new(felt.Felt).SetUint64(0xABC)
	cs := testContractStateWithViews(addr, "MyToken", map[string]*abi.FunctionDef{
		// "total_supply" is NOT in the ABI
	})

	mp := &mockProvider{blockNumber: 100}
	st := memory.New()
	vp := NewViewPoller(mp, st, noopLogger())

	_, err := vp.Setup([]*contractState{cs})
	if err == nil {
		t.Fatal("expected error for missing function")
	}
}

func TestViewPoller_Setup_NoViews(t *testing.T) {
	addr := new(felt.Felt).SetUint64(0xABC)
	cs := &contractState{
		config: config.ContractConfig{
			Name:    "NoViews",
			Address: addr.String(),
		},
		address: addr,
		abi: &abi.ABI{
			Types:     make(map[string]*abi.TypeDef),
			Functions: make(map[string]*abi.FunctionDef),
		},
		schemas: make(map[string]*types.TableSchema),
	}

	mp := &mockProvider{blockNumber: 100}
	st := memory.New()
	vp := NewViewPoller(mp, st, noopLogger())

	schemas, err := vp.Setup([]*contractState{cs})
	if err != nil {
		t.Fatalf("Setup failed: %v", err)
	}
	if len(schemas) != 0 {
		t.Fatalf("expected 0 schemas, got %d", len(schemas))
	}
	if vp.HasEntries() {
		t.Fatal("expected no entries")
	}
}

// --- ViewPoller Run/Poll Tests ---

func TestViewPoller_PollStoresResult(t *testing.T) {
	addr := new(felt.Felt).SetUint64(0xABC)
	funcDef := testFunctionDef("total_supply")
	cs := testContractStateWithViews(addr, "MyToken", map[string]*abi.FunctionDef{
		"total_supply": funcDef,
	})

	// Mock provider returns felt value 42.
	returnValue := new(felt.Felt).SetUint64(42)
	mp := &mockProvider{
		blockNumber: 500,
		callResult:  []*felt.Felt{returnValue},
	}
	st := memory.New()
	vp := NewViewPoller(mp, st, noopLogger())

	schemas, err := vp.Setup([]*contractState{cs})
	if err != nil {
		t.Fatal(err)
	}

	// Create table in store.
	ctx := context.Background()
	for _, s := range schemas {
		if err := st.CreateTable(ctx, s); err != nil {
			t.Fatal(err)
		}
	}

	// Run the poller briefly then cancel.
	ctx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	vp.Run(ctx)

	// Verify data was stored.
	events, err := st.GetUniqueEvents(context.Background(), "mytoken_total_supply", store.Query{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("expected at least 1 poll result stored")
	}

	// Check the decoded value.
	data := events[0].Data
	if data["value"] != uint64(42) {
		t.Fatalf("expected value 42, got %v", data["value"])
	}
	if data["block_number"] != uint64(500) {
		t.Fatalf("expected block_number 500, got %v", data["block_number"])
	}
	if data["_view_key"] != "latest" {
		t.Fatalf("expected _view_key 'latest', got %v", data["_view_key"])
	}
}

func TestViewPoller_LogTableAppendsRows(t *testing.T) {
	addr := new(felt.Felt).SetUint64(0xABC)
	funcDef := testFunctionDef("total_supply")

	cs := &contractState{
		config: config.ContractConfig{
			Name:    "MyToken",
			Address: addr.String(),
			Views: []config.ViewConfig{
				{
					Function: "total_supply",
					Interval: "50ms",
					Table:    config.TableConfig{Type: "log"},
				},
			},
		},
		address: addr,
		abi: &abi.ABI{
			Types:     make(map[string]*abi.TypeDef),
			Functions: map[string]*abi.FunctionDef{"total_supply": funcDef},
		},
		schemas: make(map[string]*types.TableSchema),
	}

	returnValue := new(felt.Felt).SetUint64(100)
	mp := &mockProvider{
		blockNumber: 200,
		callResult:  []*felt.Felt{returnValue},
	}
	st := memory.New()
	vp := NewViewPoller(mp, st, noopLogger())

	schemas, err := vp.Setup([]*contractState{cs})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range schemas {
		if err := st.CreateTable(context.Background(), s); err != nil {
			t.Fatal(err)
		}
	}

	// 50ms interval + initial poll; 500ms gives time for multiple ticks.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	vp.Run(ctx)

	// Log table should have multiple rows (one per poll tick).
	events, _ := st.GetEvents(context.Background(), "mytoken_total_supply", store.Query{Limit: 100})
	if len(events) < 2 {
		t.Fatalf("expected multiple poll results for log table, got %d", len(events))
	}
}

func TestViewPoller_OnEventCallback(t *testing.T) {
	addr := new(felt.Felt).SetUint64(0xABC)
	funcDef := testFunctionDef("total_supply")
	cs := testContractStateWithViews(addr, "MyToken", map[string]*abi.FunctionDef{
		"total_supply": funcDef,
	})

	returnValue := new(felt.Felt).SetUint64(99)
	mp := &mockProvider{
		blockNumber: 300,
		callResult:  []*felt.Felt{returnValue},
	}
	st := memory.New()
	vp := NewViewPoller(mp, st, noopLogger())

	schemas, _ := vp.Setup([]*contractState{cs})
	for _, s := range schemas {
		st.CreateTable(context.Background(), s)
	}

	var callbackFired atomic.Bool
	var callbackContract, callbackEvent string
	vp.SetOnEvent(func(contract, event, table string, blockNumber, logIndex uint64, data map[string]any) {
		callbackFired.Store(true)
		callbackContract = contract
		callbackEvent = event
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	vp.Run(ctx)

	if !callbackFired.Load() {
		t.Fatal("expected onEvent callback to fire")
	}
	if callbackContract != "MyToken" {
		t.Fatalf("expected contract 'MyToken', got %s", callbackContract)
	}
	if callbackEvent != "total_supply" {
		t.Fatalf("expected event 'total_supply', got %s", callbackEvent)
	}
}

// --- Reorg Re-Poll Tests ---

func TestViewPoller_ReorgTriggerRePoll(t *testing.T) {
	addr := new(felt.Felt).SetUint64(0xABC)
	funcDef := testFunctionDef("total_supply")
	cs := testContractStateWithViews(addr, "MyToken", map[string]*abi.FunctionDef{
		"total_supply": funcDef,
	})
	// Use 2s interval so the ticker won't fire during the test,
	// only the reorg notification triggers the second poll.
	cs.config.Views[0].Interval = "2s"

	returnValue := new(felt.Felt).SetUint64(42)
	mp := &mockProvider{
		blockNumber: 100,
		callResult:  []*felt.Felt{returnValue},
	}
	st := memory.New()
	vp := NewViewPoller(mp, st, noopLogger())

	schemas, _ := vp.Setup([]*contractState{cs})
	for _, s := range schemas {
		st.CreateTable(context.Background(), s)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Run poller in background.
	done := make(chan struct{})
	go func() {
		vp.Run(ctx)
		close(done)
	}()

	// Wait for initial poll (jitter is up to 10% of 2s = 200ms, so wait 500ms).
	time.Sleep(500 * time.Millisecond)
	initialCalls := mp.callCount.Load()
	if initialCalls < 1 {
		t.Fatalf("expected at least 1 initial call, got %d", initialCalls)
	}

	// Trigger reorg re-poll.
	mp.setBlockNumber(95) // block went back due to reorg
	vp.NotifyReorg()

	// Wait for re-poll.
	time.Sleep(300 * time.Millisecond)
	afterReorgCalls := mp.callCount.Load()
	if afterReorgCalls <= initialCalls {
		t.Fatalf("expected additional call after reorg, initial=%d, after=%d", initialCalls, afterReorgCalls)
	}

	cancel()
	<-done
}

// --- Status Tests ---

func TestViewPoller_Status(t *testing.T) {
	addr := new(felt.Felt).SetUint64(0xABC)
	funcDef := testFunctionDef("total_supply")
	cs := testContractStateWithViews(addr, "MyToken", map[string]*abi.FunctionDef{
		"total_supply": funcDef,
	})

	returnValue := new(felt.Felt).SetUint64(42)
	mp := &mockProvider{
		blockNumber: 500,
		callResult:  []*felt.Felt{returnValue},
	}
	st := memory.New()
	vp := NewViewPoller(mp, st, noopLogger())

	schemas, _ := vp.Setup([]*contractState{cs})
	for _, s := range schemas {
		st.CreateTable(context.Background(), s)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	vp.Run(ctx)

	statuses := vp.Status()
	if len(statuses) != 1 {
		t.Fatalf("expected 1 status entry, got %d", len(statuses))
	}
	if statuses[0].FunctionName != "total_supply" {
		t.Fatalf("expected function_name 'total_supply', got %s", statuses[0].FunctionName)
	}
	if statuses[0].Contract != "MyToken" {
		t.Fatalf("expected contract 'MyToken', got %s", statuses[0].Contract)
	}
	if statuses[0].LastPollBlock != 500 {
		t.Fatalf("expected last_poll_block 500, got %d", statuses[0].LastPollBlock)
	}
	if statuses[0].ConsecutiveErrors != 0 {
		t.Fatalf("expected 0 consecutive errors, got %d", statuses[0].ConsecutiveErrors)
	}
}

// --- Error Resilience Tests ---

func TestViewPoller_ErrorResilience(t *testing.T) {
	addr := new(felt.Felt).SetUint64(0xABC)
	funcDef := testFunctionDef("total_supply")
	cs := testContractStateWithViews(addr, "MyToken", map[string]*abi.FunctionDef{
		"total_supply": funcDef,
	})
	cs.config.Views[0].Interval = "50ms"

	mp := &mockProvider{
		blockNumber: 100,
		callErr:     context.DeadlineExceeded, // simulate transient RPC error
	}
	st := memory.New()
	vp := NewViewPoller(mp, st, noopLogger())

	schemas, _ := vp.Setup([]*contractState{cs})
	for _, s := range schemas {
		st.CreateTable(context.Background(), s)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	vp.Run(ctx)

	// Poller should continue despite errors (not panic or exit early).
	statuses := vp.Status()
	if statuses[0].ConsecutiveErrors == 0 {
		t.Fatal("expected consecutive errors > 0")
	}

	// Verify no data was stored (all polls failed).
	events, _ := st.GetUniqueEvents(context.Background(), "mytoken_total_supply", store.Query{Limit: 10})
	if len(events) != 0 {
		t.Fatalf("expected 0 events stored (all polls failed), got %d", len(events))
	}
}

// --- Schema Tests ---

func TestViewPoller_SchemaColumns(t *testing.T) {
	addr := new(felt.Felt).SetUint64(0xABC)
	funcDef := testFunctionDefMultiOutput("get_price")

	cs := &contractState{
		config: config.ContractConfig{
			Name:    "Oracle",
			Address: addr.String(),
			Views: []config.ViewConfig{
				{
					Function: "get_price",
					Calldata: []string{"0x1234"},
					Interval: "30s",
					Table: config.TableConfig{
						Type:      "unique",
						UniqueKey: "_view_key",
					},
				},
			},
		},
		address: addr,
		abi: &abi.ABI{
			Types:     make(map[string]*abi.TypeDef),
			Functions: map[string]*abi.FunctionDef{"get_price": funcDef},
		},
		schemas: make(map[string]*types.TableSchema),
	}

	mp := &mockProvider{blockNumber: 100}
	st := memory.New()
	vp := NewViewPoller(mp, st, noopLogger())

	schemas, err := vp.Setup([]*contractState{cs})
	if err != nil {
		t.Fatal(err)
	}

	if len(schemas) != 1 {
		t.Fatalf("expected 1 schema, got %d", len(schemas))
	}

	s := schemas[0]
	if s.Name != "oracle_get_price" {
		t.Fatalf("expected table name 'oracle_get_price', got %s", s.Name)
	}

	colNames := make(map[string]string)
	for _, col := range s.Columns {
		colNames[col.Name] = col.Type
	}

	// View metadata columns.
	if colNames["block_number"] != "uint64" {
		t.Fatal("missing block_number column")
	}
	if colNames["timestamp"] != "uint64" {
		t.Fatal("missing timestamp column")
	}
	if colNames["contract_address"] != "string" {
		t.Fatal("missing contract_address column")
	}
	if colNames["_view_key"] != "string" {
		t.Fatal("missing _view_key column")
	}

	// Function output columns.
	if colNames["price"] != "string" { // u128 maps to string
		t.Fatalf("expected price as string (u128), got %s", colNames["price"])
	}
	if colNames["decimals"] != "int64" { // u8 maps to int64
		t.Fatalf("expected decimals as int64 (u8), got %s", colNames["decimals"])
	}

	// Should NOT have event metadata columns (log_index, event_name, etc.).
	if _, ok := colNames["log_index"]; ok {
		t.Fatal("view tables should not have log_index column")
	}
	if _, ok := colNames["event_name"]; ok {
		t.Fatal("view tables should not have event_name column")
	}
}

// --- Multi-Output Decode Test ---

func TestViewPoller_MultiOutputDecode(t *testing.T) {
	addr := new(felt.Felt).SetUint64(0xABC)
	funcDef := testFunctionDefMultiOutput("get_price")

	cs := &contractState{
		config: config.ContractConfig{
			Name:    "Oracle",
			Address: addr.String(),
			Views: []config.ViewConfig{
				{
					Function: "get_price",
					Calldata: []string{"0x1234"},
					Interval: "100ms",
					Table: config.TableConfig{
						Type:      "unique",
						UniqueKey: "_view_key",
					},
				},
			},
		},
		address: addr,
		abi: &abi.ABI{
			Types:     make(map[string]*abi.TypeDef),
			Functions: map[string]*abi.FunctionDef{"get_price": funcDef},
		},
		schemas: make(map[string]*types.TableSchema),
	}

	// Mock returns: price=1000000 (u128), decimals=18 (u8)
	mp := &mockProvider{
		blockNumber: 400,
		callResult: []*felt.Felt{
			new(felt.Felt).SetUint64(1000000),
			new(felt.Felt).SetUint64(18),
		},
	}
	st := memory.New()
	vp := NewViewPoller(mp, st, noopLogger())

	schemas, err := vp.Setup([]*contractState{cs})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range schemas {
		st.CreateTable(context.Background(), s)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	vp.Run(ctx)

	events, _ := st.GetUniqueEvents(context.Background(), "oracle_get_price", store.Query{Limit: 10})
	if len(events) == 0 {
		t.Fatal("expected at least 1 poll result")
	}

	data := events[0].Data
	// u128 is decoded as string.
	if data["price"] != "1000000" {
		t.Fatalf("expected price '1000000', got %v", data["price"])
	}
	// u8 is decoded as uint64.
	if data["decimals"] != uint64(18) {
		t.Fatalf("expected decimals 18, got %v", data["decimals"])
	}
}

// --- AddContract Tests ---

func TestViewPoller_AddContract_SpawnsPolling(t *testing.T) {
	addr := new(felt.Felt).SetUint64(0xDEF)
	funcDef := testFunctionDef("total_supply")
	cs := testContractStateWithViews(addr, "DynamicToken", map[string]*abi.FunctionDef{
		"total_supply": funcDef,
	})

	returnValue := new(felt.Felt).SetUint64(999)
	mp := &mockProvider{
		blockNumber: 800,
		callResult:  []*felt.Felt{returnValue},
	}
	st := memory.New()
	vp := NewViewPoller(mp, st, noopLogger())

	// Poller starts with no entries.
	if vp.HasEntries() {
		t.Fatal("expected no entries before AddContract")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// AddContract should return view schemas and spawn polling goroutines.
	schemas, err := vp.AddContract(ctx, cs)
	if err != nil {
		t.Fatalf("AddContract failed: %v", err)
	}
	if len(schemas) != 1 {
		t.Fatalf("expected 1 schema, got %d", len(schemas))
	}
	if schemas[0].Name != "dynamictoken_total_supply" {
		t.Fatalf("expected table name 'dynamictoken_total_supply', got %s", schemas[0].Name)
	}

	// Create table so the poll can store data.
	for _, s := range schemas {
		if err := st.CreateTable(ctx, s); err != nil {
			t.Fatal(err)
		}
	}

	// HasEntries should be true now.
	if !vp.HasEntries() {
		t.Fatal("expected entries after AddContract")
	}

	// Wait for the spawned goroutine to poll at least once.
	time.Sleep(300 * time.Millisecond)

	if mp.callCount.Load() < 1 {
		t.Fatalf("expected at least 1 poll call, got %d", mp.callCount.Load())
	}

	// Verify data was stored.
	events, err := st.GetUniqueEvents(context.Background(), "dynamictoken_total_supply", store.Query{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("expected at least 1 poll result stored after AddContract")
	}
	if events[0].Data["value"] != uint64(999) {
		t.Fatalf("expected value 999, got %v", events[0].Data["value"])
	}

	cancel()
}

func TestViewPoller_AddContract_NoViews(t *testing.T) {
	addr := new(felt.Felt).SetUint64(0xABC)
	cs := &contractState{
		config: config.ContractConfig{
			Name:    "NoViews",
			Address: addr.String(),
		},
		address: addr,
		abi: &abi.ABI{
			Types:     make(map[string]*abi.TypeDef),
			Functions: make(map[string]*abi.FunctionDef),
		},
		schemas: make(map[string]*types.TableSchema),
	}

	mp := &mockProvider{blockNumber: 100}
	st := memory.New()
	vp := NewViewPoller(mp, st, noopLogger())

	schemas, err := vp.AddContract(context.Background(), cs)
	if err != nil {
		t.Fatalf("AddContract failed: %v", err)
	}
	if len(schemas) != 0 {
		t.Fatalf("expected 0 schemas for contract without views, got %d", len(schemas))
	}
	if vp.HasEntries() {
		t.Fatal("expected no entries for contract without views")
	}
}

func TestViewPoller_AddContract_OnEventCallback(t *testing.T) {
	addr := new(felt.Felt).SetUint64(0xDEF)
	funcDef := testFunctionDef("total_supply")
	cs := testContractStateWithViews(addr, "DynToken", map[string]*abi.FunctionDef{
		"total_supply": funcDef,
	})

	returnValue := new(felt.Felt).SetUint64(42)
	mp := &mockProvider{
		blockNumber: 100,
		callResult:  []*felt.Felt{returnValue},
	}
	st := memory.New()
	vp := NewViewPoller(mp, st, noopLogger())

	// Set onEvent before AddContract (mirrors engine behavior).
	var callbackFired atomic.Bool
	vp.SetOnEvent(func(contract, event, table string, blockNumber, logIndex uint64, data map[string]any) {
		callbackFired.Store(true)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	schemas, err := vp.AddContract(ctx, cs)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range schemas {
		st.CreateTable(ctx, s)
	}

	// Wait for polling goroutine to fire.
	time.Sleep(300 * time.Millisecond)

	if !callbackFired.Load() {
		t.Fatal("expected onEvent callback to fire from dynamically added view")
	}

	cancel()
}

func TestViewPoller_AddContract_ConcurrentWithStatus(t *testing.T) {
	funcDef := testFunctionDef("total_supply")

	returnValue := new(felt.Felt).SetUint64(1)
	mp := &mockProvider{
		blockNumber: 100,
		callResult:  []*felt.Felt{returnValue},
	}
	st := memory.New()
	vp := NewViewPoller(mp, st, noopLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Concurrently call AddContract and Status to verify no races.
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			cs := testContractStateWithViews(
				new(felt.Felt).SetUint64(uint64(0x100+idx)),
				fmt.Sprintf("Token%d", idx),
				map[string]*abi.FunctionDef{"total_supply": funcDef},
			)
			schemas, err := vp.AddContract(ctx, cs)
			if err != nil {
				t.Errorf("AddContract[%d] failed: %v", idx, err)
				return
			}
			for _, s := range schemas {
				st.CreateTable(ctx, s)
			}
		}(i)
	}

	// Concurrently call Status.
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = vp.Status()
		}()
	}

	wg.Wait()

	// Should have 5 entries.
	statuses := vp.Status()
	if len(statuses) != 5 {
		t.Fatalf("expected 5 status entries, got %d", len(statuses))
	}

	cancel()
}
