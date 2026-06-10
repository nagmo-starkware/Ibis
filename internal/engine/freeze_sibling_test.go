package engine

import (
	"context"
	"testing"

	"github.com/NethermindEth/juno/core/felt"

	"github.com/b-j-roberts/ibis/internal/abi"
	"github.com/b-j-roberts/ibis/internal/config"
	"github.com/b-j-roberts/ibis/internal/store"
	"github.com/b-j-roberts/ibis/internal/store/memory"
	"github.com/b-j-roberts/ibis/internal/types"
)

// A sibling trigger freezes a contract PER-INSTANCE: only when the event fires
// on the address recorded in its factory_meta — e.g. an OrderBook freezes when
// ITS option token (factory_meta["option_token"]) settles, not when any option
// token settles.
func TestEngine_EvaluateFreeze_SiblingEvent(t *testing.T) {
	tokAddr := new(felt.Felt).SetUint64(0x1111)
	obAddr := new(felt.Felt).SetUint64(0x2222)

	ob := testContractStateWithViews(obAddr, "OrderBook_2222", map[string]*abi.FunctionDef{
		"total_supply": testFunctionDef("total_supply"),
	})
	ob.config.Freeze = &config.FreezeConfig{
		OnSibling: []config.SiblingTrigger{{Event: "Settled", MetaField: "option_token"}},
	}
	ob.config.FactoryMeta = map[string]any{"option_token": tokAddr.String()}

	st := memory.New()
	vp := NewViewPoller(&mockProvider{blockNumber: 1}, st, noopLogger())
	if _, err := vp.Setup([]*contractState{ob}); err != nil {
		t.Fatal(err)
	}
	e := &Engine{store: st, logger: noopLogger(), poller: vp, contracts: []*contractState{ob}}

	// Settled from an UNRELATED option token → must not freeze.
	e.evaluateFreeze("OptionTok", new(felt.Felt).SetUint64(0x9999), "Settled")
	if ob.config.Frozen {
		t.Fatal("order book froze on an unrelated option token's Settled")
	}

	// Settled from THIS order book's sibling option token → freeze.
	e.evaluateFreeze("OptionTok", tokAddr, "Settled")
	if !ob.config.Frozen {
		t.Fatal("order book did not freeze on its sibling option token's Settled")
	}
	if vp.HasEntries() {
		t.Error("view polling not torn down after sibling freeze")
	}
}

// Startup reconcile must freeze a child whose sibling's terminal event was
// already indexed in a prior run (e.g. an OrderBook whose OptionToken settled
// before this restart, so the event sits below the cursor and won't replay).
func TestEngine_ReconcileFrozen_Sibling(t *testing.T) {
	st := memory.New()
	ctx := context.Background()

	settled := testEventDef("Settled")
	const sharedTable = "optionfactory_Settled"
	mkSchema := func() *types.TableSchema {
		return &types.TableSchema{
			Name: sharedTable, Contract: "OptionFactory", Event: "Settled",
			TableType: types.TableTypeLog, SharedTable: true,
			Columns: []types.Column{
				{Name: "block_number", Type: "uint64"},
				{Name: "contract_address", Type: "string"},
				{Name: "event_name", Type: "string"},
			},
		}
	}
	if err := st.CreateTable(ctx, mkSchema()); err != nil {
		t.Fatal(err)
	}

	tokAddr := new(felt.Felt).SetUint64(0xA11CE)
	obAddr := new(felt.Felt).SetUint64(0xB0B)

	// The OptionToken's Settled is already indexed.
	if err := st.ApplyOperations(ctx, []store.Operation{{
		Type: store.OpInsert, Table: sharedTable, Key: "50:0", BlockNumber: 50,
		Data: map[string]any{"block_number": uint64(50), "contract_address": tokAddr.String(), "event_name": "Settled"},
	}}); err != nil {
		t.Fatal(err)
	}

	// Sibling OptionToken (holds the Settled schema); freezes locally.
	tok := &contractState{
		config:  config.ContractConfig{Name: "tok", Address: tokAddr.String(), Dynamic: true, Freeze: &config.FreezeConfig{On: []string{"Settled"}}},
		address: tokAddr,
		abi:     &abi.ABI{Types: map[string]*abi.TypeDef{}, Events: []*abi.EventDef{settled}},
		schemas: map[string]*types.TableSchema{"Settled": mkSchema()},
	}
	// OrderBook child: no Settled of its own; freezes via its sibling option token.
	ob := &contractState{
		config: config.ContractConfig{
			Name: "ob", Address: obAddr.String(), Dynamic: true,
			Freeze:      &config.FreezeConfig{OnSibling: []config.SiblingTrigger{{Event: "Settled", MetaField: "option_token"}}},
			FactoryMeta: map[string]any{"option_token": tokAddr.String()},
		},
		address: obAddr,
		abi:     &abi.ABI{Types: map[string]*abi.TypeDef{}},
		schemas: map[string]*types.TableSchema{},
	}

	e := &Engine{store: st, logger: noopLogger(), contracts: []*contractState{tok, ob}}
	e.reconcileFrozenContracts(ctx)

	if !tok.config.Frozen {
		t.Error("option token should freeze on reconcile (its Settled is indexed)")
	}
	if !ob.config.Frozen {
		t.Error("order book should freeze on reconcile (its sibling option token settled)")
	}
}

// The resync must propagate ChildFreeze, so freeze config added after a child
// was first registered reaches already-deployed children on restart.
func TestResyncDynamicChildConfig_PropagatesFreeze(t *testing.T) {
	freeze := &config.FreezeConfig{OnSibling: []config.SiblingTrigger{{Event: "Settled", MetaField: "option_token"}}}
	e := &Engine{
		logger: noopLogger(),
		cfg: &config.Config{Contracts: []config.ContractConfig{{
			Name: "OptionFactoryETH", Address: "0x2",
			Factories: []config.FactoryConfig{{
				Event: "DeploymentCreated", ChildAddressField: "order_book", ChildABI: "OrderBook",
				ChildViews:  []config.ViewConfig{{Function: "get_depth", Refresh: &config.ViewRefreshConfig{On: []string{"OrderFilled"}}, Table: uniqueTbl()}},
				ChildFreeze: freeze, SharedTables: true,
			}},
		}}},
	}
	child := &config.ContractConfig{Name: "OrderBook_x", ABI: "OrderBook", FactoryName: "OptionFactoryETH"}
	e.resyncDynamicChildConfig(child)
	if child.Freeze == nil || len(child.Freeze.OnSibling) != 1 || child.Freeze.OnSibling[0].MetaField != "option_token" {
		t.Fatalf("resync did not propagate ChildFreeze: %+v", child.Freeze)
	}
}
