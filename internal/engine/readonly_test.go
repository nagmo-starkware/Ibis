package engine

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/b-j-roberts/ibis/internal/config"
	"github.com/b-j-roberts/ibis/internal/store"
	"github.com/b-j-roberts/ibis/internal/store/memory"
	"github.com/b-j-roberts/ibis/internal/types"
)

// testContractClassJSON is a minimal Sierra contract class with a single
// Transfer event, used as an explicit ABI file path so ABI resolution never
// needs RPC or scarb.
const testContractClassJSON = `{
	"abi": "[{\"type\":\"event\",\"name\":\"test::Transfer\",\"kind\":\"struct\",\"members\":[{\"name\":\"from\",\"type\":\"core::starknet::contract_address::ContractAddress\",\"kind\":\"key\"},{\"name\":\"to\",\"type\":\"core::starknet::contract_address::ContractAddress\",\"kind\":\"key\"},{\"name\":\"value\",\"type\":\"core::felt252\",\"kind\":\"data\"}]}]",
	"contract_class_version": "0.1.0",
	"sierra_program": []
}`

// writeTestABI writes testContractClassJSON to a temp file and returns its path.
func writeTestABI(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.contract_class.json")
	if err := os.WriteFile(path, []byte(testContractClassJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// createTableTrackingStore wraps a store.Store and counts CreateTable calls,
// so a test can assert a read-only setup never attempts to create a table.
type createTableTrackingStore struct {
	store.Store
	createTableCalls atomic.Int64
}

func (s *createTableTrackingStore) CreateTable(ctx context.Context, schema *types.TableSchema) error {
	s.createTableCalls.Add(1)
	return s.Store.CreateTable(ctx, schema)
}

func TestEngine_SetupReadOnly_NeverCreatesTables(t *testing.T) {
	abiPath := writeTestABI(t)
	st := &createTableTrackingStore{Store: memory.New()}

	cfg := &config.Config{
		Contracts: []config.ContractConfig{
			{
				Name:    "Token",
				Address: "0x1",
				ABI:     abiPath,
				Events: []config.EventConfig{
					{Name: "Transfer", Table: config.TableConfig{Type: "log"}},
				},
			},
		},
	}

	e := New(cfg, st, nil, noopLogger())

	if err := e.SetupReadOnly(context.Background()); err != nil {
		t.Fatalf("SetupReadOnly: %v", err)
	}

	if calls := st.createTableCalls.Load(); calls != 0 {
		t.Fatalf("expected 0 CreateTable calls in read-only setup, got %d", calls)
	}

	schemas := e.Schemas()
	if len(schemas) != 1 {
		t.Fatalf("expected 1 schema, got %d", len(schemas))
	}
	if schemas[0].Contract != "Token" || schemas[0].Event != "Transfer" {
		t.Fatalf("unexpected schema: %+v", schemas[0])
	}

	contracts := e.AllContracts()
	if len(contracts) != 1 || contracts[0].Name != "Token" {
		t.Fatalf("expected AllContracts to include Token, got %+v", contracts)
	}

	// Calling SetupReadOnly again must be a no-op (setupDone guard), not a
	// second pass that would duplicate e.contracts.
	if err := e.SetupReadOnly(context.Background()); err != nil {
		t.Fatalf("second SetupReadOnly: %v", err)
	}
	if len(e.AllContracts()) != 1 {
		t.Fatalf("SetupReadOnly is not idempotent: got %d contracts", len(e.AllContracts()))
	}
}

func TestEngine_RefreshDynamicContracts_PicksUpNewContract(t *testing.T) {
	abiPath := writeTestABI(t)
	st := memory.New()
	ctx := context.Background()

	cfg := &config.Config{} // no static contracts
	e := New(cfg, st, nil, noopLogger())

	if err := e.SetupReadOnly(ctx); err != nil {
		t.Fatalf("SetupReadOnly: %v", err)
	}
	if len(e.AllContracts()) != 0 {
		t.Fatalf("expected no contracts before any dynamic contract is registered, got %d", len(e.AllContracts()))
	}

	// Simulate the writer registering a new dynamic contract (e.g. a factory
	// child) after this reader's initial setup.
	if err := st.SaveDynamicContract(ctx, &config.ContractConfig{
		Name:    "Child_1",
		Address: "0x2",
		ABI:     abiPath,
		Events: []config.EventConfig{
			{Name: "Transfer", Table: config.TableConfig{Type: "log"}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	newConfigs, newSchemas, err := e.RefreshDynamicContracts(ctx)
	if err != nil {
		t.Fatalf("RefreshDynamicContracts: %v", err)
	}
	if len(newConfigs) != 1 || newConfigs[0].Name != "Child_1" {
		t.Fatalf("expected Child_1 to be reported as newly seen, got %+v", newConfigs)
	}
	if len(newSchemas) != 1 || len(newSchemas[0]) != 1 {
		t.Fatalf("expected 1 schema for Child_1, got %+v", newSchemas)
	}
	if newSchemas[0][0].Contract != "Child_1" || newSchemas[0][0].Event != "Transfer" {
		t.Fatalf("unexpected schema: %+v", newSchemas[0][0])
	}

	// The engine's own state now includes it.
	if len(e.AllContracts()) != 1 {
		t.Fatalf("expected Child_1 to be added to engine state, got %d contracts", len(e.AllContracts()))
	}

	// A second refresh with no new store entries reports nothing new.
	newConfigs, newSchemas, err = e.RefreshDynamicContracts(ctx)
	if err != nil {
		t.Fatalf("second RefreshDynamicContracts: %v", err)
	}
	if len(newConfigs) != 0 || len(newSchemas) != 0 {
		t.Fatalf("expected no newly-seen contracts on second refresh, got configs=%+v schemas=%+v", newConfigs, newSchemas)
	}
}
