package config

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/NethermindEth/juno/core/felt"
	"github.com/NethermindEth/starknet.go/contracts"
	"github.com/NethermindEth/starknet.go/rpc"
)

// --- Mock ABIFetcher ---

type mockFetcher struct {
	classOutput rpc.ClassOutput
	err         error
	called      bool
}

func (m *mockFetcher) ClassAt(ctx context.Context, blockID rpc.BlockID, contractAddress *felt.Felt) (rpc.ClassOutput, error) {
	m.called = true
	return m.classOutput, m.err
}

// --- Test ABI JSON fixture ---

const testABIJSON = `[
	{
		"type": "event",
		"name": "test::Transfer",
		"kind": "struct",
		"members": [
			{"name": "from", "type": "core::starknet::contract_address::ContractAddress", "kind": "key"},
			{"name": "to", "type": "core::starknet::contract_address::ContractAddress", "kind": "key"},
			{"name": "value", "type": "core::felt252", "kind": "data"}
		]
	}
]`

const testContractClassJSON = `{
	"abi": "[{\"type\":\"event\",\"name\":\"test::Transfer\",\"kind\":\"struct\",\"members\":[{\"name\":\"from\",\"type\":\"core::starknet::contract_address::ContractAddress\",\"kind\":\"key\"},{\"name\":\"to\",\"type\":\"core::starknet::contract_address::ContractAddress\",\"kind\":\"key\"},{\"name\":\"value\",\"type\":\"core::felt252\",\"kind\":\"data\"}]}]",
	"contract_class_version": "0.1.0",
	"sierra_program": []
}`

// --- Tests ---

func TestResolveFromExplicitPath(t *testing.T) {
	// Write a temporary ABI file.
	dir := t.TempDir()
	abiPath := filepath.Join(dir, "test.contract_class.json")
	if err := os.WriteFile(abiPath, []byte(testContractClassJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	resolver := NewABIResolver(nil)
	contract := ContractConfig{
		Name:    "TestContract",
		Address: "0x1234",
		ABI:     abiPath,
	}

	parsed, err := resolver.Resolve(context.Background(), &contract)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if len(parsed.Events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(parsed.Events))
	}
	if parsed.Events[0].Name != "Transfer" {
		t.Errorf("expected event name Transfer, got %s", parsed.Events[0].Name)
	}
}

func TestResolveFromExplicitPath_RelativePath(t *testing.T) {
	// Write a temporary ABI file in a relative-looking path.
	dir := t.TempDir()
	abiPath := filepath.Join(dir, "my_abi.json")
	if err := os.WriteFile(abiPath, []byte(testABIJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	resolver := NewABIResolver(nil)
	contract := ContractConfig{
		Name:    "TestContract",
		Address: "0x1234",
		ABI:     abiPath, // absolute but ends in .json
	}

	parsed, err := resolver.Resolve(context.Background(), &contract)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if len(parsed.Events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(parsed.Events))
	}
}

func TestResolveFromExplicitPath_NotFound(t *testing.T) {
	resolver := NewABIResolver(nil)
	contract := ContractConfig{
		Name:    "TestContract",
		Address: "0x1234",
		ABI:     "./nonexistent/path.json",
	}

	_, err := resolver.Resolve(context.Background(), &contract)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestResolveFromChain_SierraClass(t *testing.T) {
	abiStr := contracts.NestedString(testABIJSON)
	fetcher := &mockFetcher{
		classOutput: &contracts.ContractClass{
			ContractClassVersion: "0.1.0",
			ABI:                  abiStr,
		},
	}

	resolver := NewABIResolver(fetcher)
	contract := ContractConfig{
		Name:    "ChainContract",
		Address: "0x049d36570d4e46f48e99674bd3fcc84644ddd6b96f7c741b1562b82f9e004dc7",
		ABI:     "fetch",
	}

	parsed, err := resolver.Resolve(context.Background(), &contract)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if !fetcher.called {
		t.Error("expected fetcher.ClassAt to be called")
	}
	if len(parsed.Events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(parsed.Events))
	}
	if parsed.Events[0].Name != "Transfer" {
		t.Errorf("expected event name Transfer, got %s", parsed.Events[0].Name)
	}
}

func TestResolveFromChain_NoProvider(t *testing.T) {
	resolver := NewABIResolver(nil) // nil fetcher
	contract := ContractConfig{
		Name:    "ChainContract",
		Address: "0x1234",
		ABI:     "fetch",
	}

	_, err := resolver.Resolve(context.Background(), &contract)
	if err == nil {
		t.Fatal("expected error when fetcher is nil")
	}
}

func TestResolveFromChain_RPCError(t *testing.T) {
	fetcher := &mockFetcher{
		err: fmt.Errorf("connection refused"),
	}

	resolver := NewABIResolver(fetcher)
	contract := ContractConfig{
		Name:    "ChainContract",
		Address: "0x1234",
		ABI:     "fetch",
	}

	_, err := resolver.Resolve(context.Background(), &contract)
	if err == nil {
		t.Fatal("expected error on RPC failure")
	}
}

func TestResolveFromChain_EmptyABI(t *testing.T) {
	fetcher := &mockFetcher{
		classOutput: &contracts.ContractClass{
			ContractClassVersion: "0.1.0",
			ABI:                  "", // empty
		},
	}

	resolver := NewABIResolver(fetcher)
	contract := ContractConfig{
		Name:    "EmptyABI",
		Address: "0x1234",
		ABI:     "fetch",
	}

	_, err := resolver.Resolve(context.Background(), &contract)
	if err == nil {
		t.Fatal("expected error for empty ABI")
	}
}

func TestResolveFromChain_DeprecatedClassNilABI(t *testing.T) {
	fetcher := &mockFetcher{
		classOutput: &contracts.DeprecatedContractClass{
			ABI: nil,
		},
	}

	resolver := NewABIResolver(fetcher)
	contract := ContractConfig{
		Name:    "OldContract",
		Address: "0x1234",
		ABI:     "fetch",
	}

	_, err := resolver.Resolve(context.Background(), &contract)
	if err == nil {
		t.Fatal("expected error for deprecated class with nil ABI")
	}
}

func TestCaching(t *testing.T) {
	abiStr := contracts.NestedString(testABIJSON)
	fetcher := &mockFetcher{
		classOutput: &contracts.ContractClass{
			ContractClassVersion: "0.1.0",
			ABI:                  abiStr,
		},
	}

	resolver := NewABIResolver(fetcher)
	contract := ContractConfig{
		Name:    "CachedContract",
		Address: "0x049d36570d4e46f48e99674bd3fcc84644ddd6b96f7c741b1562b82f9e004dc7",
		ABI:     "fetch",
	}

	// First call.
	_, err := resolver.Resolve(context.Background(), &contract)
	if err != nil {
		t.Fatal(err)
	}

	// Reset the fetcher to track if it's called again.
	fetcher.called = false

	// Second call should hit cache.
	parsed, err := resolver.Resolve(context.Background(), &contract)
	if err != nil {
		t.Fatal(err)
	}
	if fetcher.called {
		t.Error("expected second call to use cache, but fetcher was called again")
	}
	if len(parsed.Events) != 1 {
		t.Errorf("expected 1 event from cache, got %d", len(parsed.Events))
	}
}

func TestResolveAll(t *testing.T) {
	// Write temp ABI files.
	dir := t.TempDir()
	path1 := filepath.Join(dir, "a.json")
	path2 := filepath.Join(dir, "b.json")
	if err := os.WriteFile(path1, []byte(testABIJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path2, []byte(testABIJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	resolver := NewABIResolver(nil)
	contracts := []ContractConfig{
		{Name: "A", Address: "0xaaa", ABI: path1},
		{Name: "B", Address: "0xbbb", ABI: path2},
	}

	result, err := resolver.ResolveAll(context.Background(), contracts)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if len(result) != 2 {
		t.Fatalf("expected 2 ABIs, got %d", len(result))
	}
	if result["0xaaa"] == nil || result["0xbbb"] == nil {
		t.Error("expected both ABIs to be present")
	}
}

func TestResolveAll_FailsOnFirstError(t *testing.T) {
	resolver := NewABIResolver(nil)
	contracts := []ContractConfig{
		{Name: "Good", Address: "0xaaa", ABI: "./nonexistent.json"},
		{Name: "Bad", Address: "0xbbb", ABI: "fetch"},
	}

	_, err := resolver.ResolveAll(context.Background(), contracts)
	if err == nil {
		t.Fatal("expected error when one contract fails")
	}
}

func TestLocalDiscovery(t *testing.T) {
	// Set up target/dev/ directory structure in a temp dir and cd into it.
	dir := t.TempDir()
	devDir := filepath.Join(dir, "target", "dev")
	if err := os.MkdirAll(devDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Write a contract class file with the Scarb naming convention.
	abiFile := filepath.Join(devDir, "mypackage_MyToken.contract_class.json")
	if err := os.WriteFile(abiFile, []byte(testContractClassJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	// Change to the temp dir so target/dev/ is relative.
	origDir, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(origDir)

	resolver := NewABIResolver(nil)
	contract := ContractConfig{
		Name:    "MyToken",
		Address: "0x1234",
		ABI:     "MyToken", // contract name triggers local discovery
	}

	parsed, err := resolver.Resolve(context.Background(), &contract)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if len(parsed.Events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(parsed.Events))
	}
}

// TestResolveByName exercises the firehose-keys transport's need to resolve
// a factory's declared ChildABI by name alone (no ContractConfig, no
// address) — used by engine.computeOptionSelectors to learn a factory
// child's event set before any child has actually been deployed/discovered.
func TestResolveByName(t *testing.T) {
	dir := t.TempDir()
	devDir := filepath.Join(dir, "target", "dev")
	if err := os.MkdirAll(devDir, 0o755); err != nil {
		t.Fatal(err)
	}
	abiFile := filepath.Join(devDir, "mypackage_MyToken.contract_class.json")
	if err := os.WriteFile(abiFile, []byte(testContractClassJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	origDir, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(origDir)

	resolver := NewABIResolver(nil)
	parsed, err := resolver.ResolveByName("MyToken")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if len(parsed.Events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(parsed.Events))
	}
}

// TestResolveByName_NotFound mirrors TestLocalDiscovery_NotFound for the
// ResolveByName entry point.
func TestResolveByName_NotFound(t *testing.T) {
	dir := t.TempDir()
	origDir, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(origDir)

	resolver := NewABIResolver(nil)
	if _, err := resolver.ResolveByName("MissingContract"); err == nil {
		t.Fatal("expected error for missing local ABI")
	}
}

func TestLocalDiscovery_NotFound(t *testing.T) {
	// cd to a temp dir without target/dev/.
	dir := t.TempDir()
	origDir, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(origDir)

	resolver := NewABIResolver(nil)
	contract := ContractConfig{
		Name:    "MissingContract",
		Address: "0x1234",
		ABI:     "MissingContract",
	}

	_, err := resolver.Resolve(context.Background(), &contract)
	if err == nil {
		t.Fatal("expected error for missing local ABI")
	}
}

func TestLocalDiscovery_MultipleMatches(t *testing.T) {
	dir := t.TempDir()
	devDir := filepath.Join(dir, "target", "dev")
	if err := os.MkdirAll(devDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Write two files that match the same contract name.
	for _, pkg := range []string{"pkg1", "pkg2"} {
		path := filepath.Join(devDir, fmt.Sprintf("%s_MyToken.contract_class.json", pkg))
		if err := os.WriteFile(path, []byte(testContractClassJSON), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	origDir, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(origDir)

	resolver := NewABIResolver(nil)
	contract := ContractConfig{
		Name:    "MyToken",
		Address: "0x1234",
		ABI:     "MyToken",
	}

	_, err := resolver.Resolve(context.Background(), &contract)
	if err == nil {
		t.Fatal("expected error for multiple matching ABI files")
	}
}

func TestIsFilePath(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"./target/dev/contract.json", true},
		{"/absolute/path.json", true},
		{"../relative.json", true},
		{"custom_name.json", true},
		{"fetch", false},
		{"MyContract", false},
		{"ERC20Token", false},
	}

	for _, tt := range tests {
		got := isFilePath(tt.input)
		if got != tt.want {
			t.Errorf("isFilePath(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}
