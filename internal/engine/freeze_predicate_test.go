package engine

import (
	"context"
	"testing"
	"time"

	"github.com/NethermindEth/juno/core/felt"

	"github.com/b-j-roberts/ibis/internal/abi"
	"github.com/b-j-roberts/ibis/internal/config"
	"github.com/b-j-roberts/ibis/internal/store/memory"
	"github.com/b-j-roberts/ibis/internal/types"
)

// predicateChild builds a factory-child contractState whose freeze policy is a
// single predicate "expiry < now() - 2d", with the given expiry captured in
// factory_meta (as it would be from DeploymentCreated).
func predicateChild(addr *felt.Felt, name string, expiry int64) *contractState {
	return &contractState{
		config: config.ContractConfig{
			Name: name, Address: addr.String(), Dynamic: true,
			Freeze: &config.FreezeConfig{Any: []config.FreezeRule{
				{Predicate: &config.FreezePredicate{MetaField: "expiry", Op: "lt", Value: "now() - 2d"}},
			}},
			FactoryMeta: map[string]any{"expiry": expiry},
		},
		address: addr,
		abi:     &abi.ABI{Types: map[string]*abi.TypeDef{}},
		schemas: map[string]*types.TableSchema{},
	}
}

// The periodic predicate tick freezes a child past expiry+grace and leaves a
// not-yet-expired one live — proving direction (freeze AFTER expiry, never
// before).
func TestEngine_PredicateFreeze_PeriodicTick(t *testing.T) {
	now := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)
	expired := predicateChild(new(felt.Felt).SetUint64(0x1), "opt_expired", now.Add(-3*24*time.Hour).Unix())
	live := predicateChild(new(felt.Felt).SetUint64(0x2), "opt_live", now.Add(24*time.Hour).Unix())

	e := &Engine{store: memory.New(), logger: noopLogger(), contracts: []*contractState{expired, live}}
	e.evaluatePredicateFreezes(now)

	if !expired.config.Frozen {
		t.Error("expired child (expiry < now()-2d) should freeze on the periodic tick")
	}
	if live.config.Frozen {
		t.Error("not-yet-expired child must stay live (no early freeze)")
	}
}

// Boot reconcile freezes the never-sold backlog: a child whose expiry+grace
// already elapsed in a prior run freezes on restart with no terminal event and
// no manual DB work, so buildSubscriptions skips it.
func TestEngine_ReconcileFrozen_Predicate(t *testing.T) {
	ctx := context.Background()
	// Expiry in 2001 — robustly < now()-2d regardless of the test machine clock.
	expired := predicateChild(new(felt.Felt).SetUint64(0x1), "opt_expired", 1000000000)
	live := predicateChild(new(felt.Felt).SetUint64(0x2), "opt_live", time.Now().Add(72*time.Hour).Unix())

	e := &Engine{store: memory.New(), logger: noopLogger(), contracts: []*contractState{expired, live}}
	e.reconcileFrozenContracts(ctx)

	if !expired.config.Frozen {
		t.Error("reconcile should freeze a child already past expiry+grace")
	}
	if live.config.Frozen {
		t.Error("reconcile must not freeze a not-yet-expired child")
	}
}

// Backward compatibility: a Settled-driven freeze still fires on the event,
// whether expressed in the legacy on: form or the new any:[{event}] form.
func TestEngine_EvaluateFreeze_EventBackCompat(t *testing.T) {
	mk := func(addr *felt.Felt, name string, f *config.FreezeConfig) *contractState {
		return &contractState{
			config:  config.ContractConfig{Name: name, Address: addr.String(), Dynamic: true, Freeze: f},
			address: addr,
			abi:     &abi.ABI{Types: map[string]*abi.TypeDef{}},
			schemas: map[string]*types.TableSchema{},
		}
	}
	legacyAddr := new(felt.Felt).SetUint64(0x10)
	anyAddr := new(felt.Felt).SetUint64(0x20)
	legacy := mk(legacyAddr, "legacy", &config.FreezeConfig{On: []string{"Settled"}})
	anyForm := mk(anyAddr, "anyform", &config.FreezeConfig{Any: []config.FreezeRule{{Event: "Settled"}}})

	e := &Engine{store: memory.New(), logger: noopLogger(), contracts: []*contractState{legacy, anyForm}}

	e.evaluateFreeze("legacy", legacyAddr, "Settled")
	e.evaluateFreeze("anyform", anyAddr, "Settled")

	if !legacy.config.Frozen {
		t.Error("legacy on:[Settled] must still freeze on the Settled event")
	}
	if !anyForm.config.Frozen {
		t.Error("any:[{event: Settled}] must freeze on the Settled event")
	}
}
