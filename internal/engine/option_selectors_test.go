package engine

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/NethermindEth/juno/core/felt"

	"github.com/b-j-roberts/ibis/internal/abi"
	"github.com/b-j-roberts/ibis/internal/config"
	"github.com/b-j-roberts/ibis/internal/types"
)

// optionTokenContractClassJSON is a minimal Scarb-style contract class
// fixture whose ABI declares Transfer, Approval, and Settled events. Used to
// verify computeOptionSelectors resolves a factory's declared ChildABI by
// name (via config.NewABIResolver(...).ResolveByName) and correctly excludes
// Transfer/Approval from the option-family selector union it builds for the
// firehose-keys transport's keys-sub filter.
const optionTokenContractClassJSON = `{
	"abi": "[{\"type\":\"event\",\"name\":\"test::Transfer\",\"kind\":\"struct\",\"members\":[{\"name\":\"from\",\"type\":\"core::starknet::contract_address::ContractAddress\",\"kind\":\"key\"},{\"name\":\"to\",\"type\":\"core::starknet::contract_address::ContractAddress\",\"kind\":\"key\"},{\"name\":\"value\",\"type\":\"core::felt252\",\"kind\":\"data\"}]},{\"type\":\"event\",\"name\":\"test::Approval\",\"kind\":\"struct\",\"members\":[{\"name\":\"owner\",\"type\":\"core::starknet::contract_address::ContractAddress\",\"kind\":\"key\"},{\"name\":\"spender\",\"type\":\"core::starknet::contract_address::ContractAddress\",\"kind\":\"key\"},{\"name\":\"value\",\"type\":\"core::felt252\",\"kind\":\"data\"}]},{\"type\":\"event\",\"name\":\"test::Settled\",\"kind\":\"struct\",\"members\":[{\"name\":\"amount\",\"type\":\"core::felt252\",\"kind\":\"data\"}]}]",
	"contract_class_version": "0.1.0",
	"sierra_program": []
}`

// TestComputeOptionSelectors exercises the full union computation: one
// already-registered wildcard contract (OptionManager, own ABI has Written +
// Transfer) plus a factory entry declaring ChildABI "OptionToken", resolved
// by name from a local target/dev/ fixture (its ABI has Transfer, Approval,
// Settled). The expected union is exactly {Written, Settled} — Transfer and
// Approval excluded from BOTH sources, no duplicates.
func TestComputeOptionSelectors(t *testing.T) {
	// Set up target/dev/ so ResolveByName("OptionToken") finds the fixture —
	// mirrors internal/config/abi_resolve_test.go's TestLocalDiscovery.
	dir := t.TempDir()
	devDir := filepath.Join(dir, "target", "dev")
	if err := os.MkdirAll(devDir, 0o755); err != nil {
		t.Fatal(err)
	}
	abiFile := filepath.Join(devDir, "supervega_OptionToken.contract_class.json")
	if err := os.WriteFile(abiFile, []byte(optionTokenContractClassJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	origDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(origDir) }()

	managerEvents := []*abi.EventDef{
		testEventDef("Written"),
		testEventDef("Transfer"),
	}
	managerCS := testContractState(new(felt.Felt).SetUint64(0x1), "OptionManager", managerEvents, types.TableTypeLog)
	managerCS.config.Events = []config.EventConfig{{Name: "*"}}
	managerCS.config.Factories = []config.FactoryConfig{
		{Event: "DeploymentCreated", ChildAddressField: "option_token", ChildABI: "OptionToken"},
	}

	e := &Engine{
		cfg:       &config.Config{},
		logger:    slog.Default(),
		contracts: []*contractState{managerCS},
	}

	e.computeOptionSelectors()

	if len(e.optionSelectors) != 2 {
		t.Fatalf("optionSelectors count = %d, want 2 (Written + Settled)", len(e.optionSelectors))
	}

	got := make(map[string]bool, len(e.optionSelectors))
	for _, sel := range e.optionSelectors {
		got[sel.String()] = true
	}

	if !got[abi.ComputeSelector("Written").String()] {
		t.Error("missing Written selector (manager's own wildcard event)")
	}
	if !got[abi.ComputeSelector("Settled").String()] {
		t.Error("missing Settled selector (factory ChildABI event, resolved by name)")
	}
	if got[abi.ComputeSelector("Transfer").String()] {
		t.Error("Transfer selector must be excluded from the option-family union")
	}
	if got[abi.ComputeSelector("Approval").String()] {
		t.Error("Approval selector must be excluded from the option-family union")
	}
}

// TestComputeOptionSelectors_DedupesRepeatedChildABI verifies that two
// factory entries (even on different parent contracts) declaring the SAME
// ChildABI resolve it only once and don't produce duplicate selectors.
func TestComputeOptionSelectors_DedupesRepeatedChildABI(t *testing.T) {
	dir := t.TempDir()
	devDir := filepath.Join(dir, "target", "dev")
	if err := os.MkdirAll(devDir, 0o755); err != nil {
		t.Fatal(err)
	}
	abiFile := filepath.Join(devDir, "supervega_OptionToken.contract_class.json")
	if err := os.WriteFile(abiFile, []byte(optionTokenContractClassJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	origDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(origDir) }()

	factory := config.FactoryConfig{Event: "DeploymentCreated", ChildAddressField: "option_token", ChildABI: "OptionToken"}

	cs1 := testContractState(new(felt.Felt).SetUint64(0x1), "OptionManagerA", nil, types.TableTypeLog)
	cs1.config.Factories = []config.FactoryConfig{factory}
	cs2 := testContractState(new(felt.Felt).SetUint64(0x2), "OptionManagerB", nil, types.TableTypeLog)
	cs2.config.Factories = []config.FactoryConfig{factory}

	e := &Engine{
		cfg:       &config.Config{},
		logger:    slog.Default(),
		contracts: []*contractState{cs1, cs2},
	}

	e.computeOptionSelectors()

	if len(e.optionSelectors) != 1 {
		t.Fatalf("optionSelectors count = %d, want 1 (Settled, deduped across 2 factory entries)", len(e.optionSelectors))
	}
	if e.optionSelectors[0].String() != abi.ComputeSelector("Settled").String() {
		t.Errorf("optionSelectors[0] = %s, want Settled selector", e.optionSelectors[0].String())
	}
}
