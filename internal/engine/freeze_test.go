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

// (compile-time) ensure store import is used in this file.
var _ = store.Query{}
