package engine

import (
	"context"
	"testing"
	"time"

	"github.com/NethermindEth/juno/core/felt"

	"github.com/b-j-roberts/ibis/internal/abi"
	"github.com/b-j-roberts/ibis/internal/config"
	"github.com/b-j-roberts/ibis/internal/store"
	"github.com/b-j-roberts/ibis/internal/store/memory"
	"github.com/b-j-roberts/ibis/internal/types"
)

// TestViewPoller_RemoveContract_StopsPolling verifies that RemoveContract
// cancels a contract's view goroutines so it stops consuming RPC, and drops
// its entries. This is the teardown the freeze lifecycle relies on.
func TestViewPoller_RemoveContract_StopsPolling(t *testing.T) {
	addr := new(felt.Felt).SetUint64(0xABC)
	funcDef := testFunctionDef("total_supply")
	cs := testContractStateWithViews(addr, "MyToken", map[string]*abi.FunctionDef{
		"total_supply": funcDef,
	})

	mp := &mockProvider{blockNumber: 500, callResult: []*felt.Felt{new(felt.Felt).SetUint64(42)}}
	st := memory.New()
	vp := NewViewPoller(mp, st, noopLogger())

	schemas, err := vp.Setup([]*contractState{cs})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, s := range schemas {
		if err := st.CreateTable(ctx, s); err != nil {
			t.Fatal(err)
		}
	}

	done := make(chan struct{})
	go func() {
		vp.Run(ctx)
		close(done)
	}()

	// Let it poll a few times (100ms interval + up to 100ms jitter).
	time.Sleep(400 * time.Millisecond)
	before := mp.callCount.Load()
	if before == 0 {
		t.Fatal("expected the view to have polled at least once before removal")
	}

	removed := vp.RemoveContract("MyToken")
	if removed != 1 {
		t.Fatalf("RemoveContract removed %d entries, want 1", removed)
	}
	if vp.HasEntries() {
		t.Fatal("poller still has entries after RemoveContract")
	}

	// After removal, polling must stop. Allow at most one in-flight poll.
	time.Sleep(400 * time.Millisecond)
	after := mp.callCount.Load()
	if after > before+1 {
		t.Fatalf("polling continued after RemoveContract: before=%d after=%d", before, after)
	}

	// Run() returns once its only entry's goroutine is canceled.
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after all entries removed")
	}
}

// TestEngine_FreezeContract_TearsDownAndPersists verifies a freeze stops view
// polling and persists the frozen flag for a dynamic contract.
func TestEngine_FreezeContract_TearsDownAndPersists(t *testing.T) {
	addr := new(felt.Felt).SetUint64(0xCAFE)
	funcDef := testFunctionDef("total_supply")
	cs := testContractStateWithViews(addr, "Child_cafe", map[string]*abi.FunctionDef{
		"total_supply": funcDef,
	})
	cs.config.Dynamic = true // factory child → persisted in the dynamic-contract store

	st := memory.New()
	vp := NewViewPoller(&mockProvider{blockNumber: 1}, st, noopLogger())
	if _, err := vp.Setup([]*contractState{cs}); err != nil {
		t.Fatal(err)
	}

	e := &Engine{
		store:     st,
		logger:    noopLogger(),
		poller:    vp,
		contracts: []*contractState{cs},
	}

	if err := e.FreezeContract(context.Background(), "Child_cafe"); err != nil {
		t.Fatalf("FreezeContract: %v", err)
	}

	if !cs.config.Frozen {
		t.Error("contract not marked Frozen")
	}
	if vp.HasEntries() {
		t.Error("view poller still has entries after freeze")
	}

	// Persisted with Frozen=true so rehydration skips re-subscribing.
	saved, err := st.GetDynamicContracts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var found *config.ContractConfig
	for i := range saved {
		if saved[i].Name == "Child_cafe" {
			found = &saved[i]
			break
		}
	}
	if found == nil {
		t.Fatal("frozen contract not persisted to dynamic-contract store")
	}
	if !found.Frozen {
		t.Error("persisted dynamic contract is not marked Frozen")
	}

	// Idempotent: freezing again is a no-op, not an error.
	if err := e.FreezeContract(context.Background(), "Child_cafe"); err != nil {
		t.Fatalf("second FreezeContract should be a no-op, got: %v", err)
	}
}

// TestEngine_EvaluateFreeze_LocalEvent verifies that a contract's own terminal
// event freezes it, while an unrelated event does not.
func TestEngine_EvaluateFreeze_LocalEvent(t *testing.T) {
	addr := new(felt.Felt).SetUint64(0xBEEF)
	cs := testContractStateWithViews(addr, "OptionTok", map[string]*abi.FunctionDef{
		"total_supply": testFunctionDef("total_supply"),
	})
	cs.config.Freeze = &config.FreezeConfig{On: []string{"Settled"}}

	st := memory.New()
	vp := NewViewPoller(&mockProvider{blockNumber: 1}, st, noopLogger())
	if _, err := vp.Setup([]*contractState{cs}); err != nil {
		t.Fatal(err)
	}
	e := &Engine{store: st, logger: noopLogger(), poller: vp, contracts: []*contractState{cs}}

	// Non-trigger event: no freeze.
	e.evaluateFreeze("OptionTok", addr, "Transfer")
	if cs.config.Frozen {
		t.Fatal("contract froze on a non-trigger event")
	}

	// Trigger event on this contract: freeze.
	e.evaluateFreeze("OptionTok", addr, "Settled")
	if !cs.config.Frozen {
		t.Fatal("contract did not freeze on its terminal event")
	}
	if vp.HasEntries() {
		t.Error("view polling not torn down after local-event freeze")
	}
}

// TestEngine_EvaluateFreeze_ForeignEvent verifies a foreign (cross-contract)
// trigger freezes the declaring contract.
func TestEngine_EvaluateFreeze_ForeignEvent(t *testing.T) {
	tokAddr := new(felt.Felt).SetUint64(0x1111)
	cs := testContractStateWithViews(tokAddr, "OptionTok", map[string]*abi.FunctionDef{
		"total_supply": testFunctionDef("total_supply"),
	})
	cs.config.Freeze = &config.FreezeConfig{
		OnForeign: []config.ForeignTrigger{{Contract: "OptionManager", Event: "ActiveDeploymentChanged"}},
	}

	st := memory.New()
	vp := NewViewPoller(&mockProvider{blockNumber: 1}, st, noopLogger())
	if _, err := vp.Setup([]*contractState{cs}); err != nil {
		t.Fatal(err)
	}
	e := &Engine{store: st, logger: noopLogger(), poller: vp, contracts: []*contractState{cs}}

	// Event emitted by a different contract/address than the token.
	mgrAddr := new(felt.Felt).SetUint64(0x2222)
	e.evaluateFreeze("OptionManager", mgrAddr, "ActiveDeploymentChanged")

	if !cs.config.Frozen {
		t.Fatal("contract did not freeze on its declared foreign trigger")
	}
}

// TestEngine_ReconcileFrozenContracts_SharedTable verifies the startup
// reconciliation: a child whose terminal event was already indexed (in a
// previous run, now below its cursor) is frozen on boot, scoped per-instance via
// contract_address; a sibling without that event stays active.
func TestEngine_ReconcileFrozenContracts_SharedTable(t *testing.T) {
	st := memory.New()
	ctx := context.Background()

	settled := testEventDef("Settled")
	const sharedTable = "optionfactory_Settled"
	mkSchema := func() *types.TableSchema {
		return &types.TableSchema{
			Name:        sharedTable,
			Contract:    "OptionFactory",
			Event:       "Settled",
			TableType:   types.TableTypeLog,
			SharedTable: true,
			Columns: []types.Column{
				{Name: "block_number", Type: "uint64"},
				{Name: "log_index", Type: "uint64"},
				{Name: "timestamp", Type: "uint64"},
				{Name: "contract_address", Type: "string"},
				{Name: "contract_name", Type: "string"},
				{Name: "event_name", Type: "string"},
			},
		}
	}
	if err := st.CreateTable(ctx, mkSchema()); err != nil {
		t.Fatal(err)
	}

	addrA := new(felt.Felt).SetUint64(0xA11CE)
	addrB := new(felt.Felt).SetUint64(0xB0B)

	// Only child A has an already-indexed Settled row.
	op := store.Operation{
		Type:  store.OpInsert,
		Table: sharedTable,
		Key:   "50:0",
		Data: map[string]any{
			"block_number":     uint64(50),
			"log_index":        uint64(0),
			"contract_address": addrA.String(),
			"contract_name":    "child_a",
			"event_name":       "Settled",
		},
		BlockNumber: 50,
	}
	if err := st.ApplyOperations(ctx, []store.Operation{op}); err != nil {
		t.Fatal(err)
	}

	mkCS := func(name string, addr *felt.Felt) *contractState {
		return &contractState{
			config: config.ContractConfig{
				Name:    name,
				Address: addr.String(),
				Dynamic: true,
				Freeze:  &config.FreezeConfig{On: []string{"Settled"}},
			},
			address: addr,
			abi:     &abi.ABI{Types: map[string]*abi.TypeDef{}, Events: []*abi.EventDef{settled}},
			schemas: map[string]*types.TableSchema{"Settled": mkSchema()},
		}
	}
	csA := mkCS("child_a", addrA)
	csB := mkCS("child_b", addrB)

	e := &Engine{store: st, logger: noopLogger(), contracts: []*contractState{csA, csB}}
	e.reconcileFrozenContracts(ctx)

	if !csA.config.Frozen {
		t.Error("child_a should be frozen — its Settled event is already indexed")
	}
	if csB.config.Frozen {
		t.Error("child_b should NOT be frozen — no Settled event indexed for it")
	}

	// The frozen child is persisted so the next restart keeps it frozen.
	saved, err := st.GetDynamicContracts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var frozenPersisted bool
	for i := range saved {
		if saved[i].Name == "child_a" && saved[i].Frozen {
			frozenPersisted = true
		}
		if saved[i].Name == "child_b" {
			t.Error("child_b should not have been persisted as frozen")
		}
	}
	if !frozenPersisted {
		t.Error("child_a frozen state was not persisted")
	}
}

// (compile-time) ensure store import is used in this file.
var _ = store.Query{}
