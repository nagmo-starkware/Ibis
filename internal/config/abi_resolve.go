package config

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/NethermindEth/juno/core/felt"
	"github.com/NethermindEth/starknet.go/contracts"
	"github.com/NethermindEth/starknet.go/rpc"

	"github.com/b-j-roberts/ibis/internal/abi"
)

// ABIFetcher abstracts the RPC call needed for chain-based ABI resolution.
// Implementations should call starknet_getClassAt under the hood.
type ABIFetcher interface {
	ClassAt(ctx context.Context, blockID rpc.BlockID, contractAddress *felt.Felt) (rpc.ClassOutput, error)
}

// ABIResolver resolves and caches contract ABIs using a three-tier strategy:
//  1. Explicit file path
//  2. Smart local discovery (search target/dev/, optionally run scarb build)
//  3. Chain fetch via RPC
type ABIResolver struct {
	fetcher ABIFetcher

	mu    sync.RWMutex
	cache map[string]*abi.ABI // keyed by contract address
}

// NewABIResolver creates a resolver. fetcher may be nil if chain fetch is not needed.
func NewABIResolver(fetcher ABIFetcher) *ABIResolver {
	return &ABIResolver{
		fetcher: fetcher,
		cache:   make(map[string]*abi.ABI),
	}
}

// ResolveAll resolves ABIs for all contracts in the config. Returns a map of
// contract address -> parsed ABI. This should be called once at startup.
func (r *ABIResolver) ResolveAll(ctx context.Context, contracts []ContractConfig) (map[string]*abi.ABI, error) {
	result := make(map[string]*abi.ABI, len(contracts))

	for i := range contracts {
		parsed, err := r.Resolve(ctx, &contracts[i])
		if err != nil {
			return nil, fmt.Errorf("contract %s (%s): %w", contracts[i].Name, contracts[i].Address, err)
		}
		result[contracts[i].Address] = parsed
	}

	return result, nil
}

// Resolve resolves a single contract's ABI using the three-tier strategy.
// Results are cached in memory for the session.
func (r *ABIResolver) Resolve(ctx context.Context, contract *ContractConfig) (*abi.ABI, error) {
	// Check cache first.
	r.mu.RLock()
	if cached, ok := r.cache[contract.Address]; ok {
		r.mu.RUnlock()
		return cached, nil
	}
	r.mu.RUnlock()

	parsed, err := r.resolve(ctx, contract)
	if err != nil {
		return nil, err
	}

	// Cache the result.
	r.mu.Lock()
	r.cache[contract.Address] = parsed
	r.mu.Unlock()

	return parsed, nil
}

// resolve performs the actual three-tier resolution without caching.
func (r *ABIResolver) resolve(ctx context.Context, contract *ContractConfig) (*abi.ABI, error) {
	abiSpec := contract.ABI

	// Tier 1: Explicit file path.
	if isFilePath(abiSpec) {
		return r.resolveFromFile(abiSpec)
	}

	// Tier 2: Smart local discovery by contract name.
	if abiSpec != "fetch" {
		return r.resolveLocal(abiSpec)
	}

	// Tier 3: Chain fetch via RPC.
	return r.resolveFromChain(ctx, contract.Address)
}

// resolveFromFile loads an ABI from an explicit file path.
func (r *ABIResolver) resolveFromFile(path string) (*abi.ABI, error) {
	parsed, err := abi.ParseFile(path)
	if err != nil {
		return nil, fmt.Errorf("loading ABI from file %s: %w", path, err)
	}
	return parsed, nil
}

// resolveLocal searches target/dev/ for a matching contract_class.json file.
// If not found, attempts scarb build and retries. If multiple matches are found,
// returns an error immediately without attempting scarb build.
func (r *ABIResolver) resolveLocal(contractName string) (*abi.ABI, error) {
	// First attempt: search existing build artifacts.
	path, err := discoverABI(contractName)
	if err == nil {
		return abi.ParseFile(path)
	}

	// If the error is ambiguity (multiple matches), don't try scarb build.
	if isAmbiguousMatch(err) {
		return nil, err
	}

	// Second attempt: run scarb build, then retry discovery.
	if buildErr := runScarbBuild(); buildErr != nil {
		return nil, fmt.Errorf(
			"ABI for %q not found in target/dev/ and scarb build failed: %w\n"+
				"  discovery error: %v",
			contractName, buildErr, err,
		)
	}

	path, err = discoverABI(contractName)
	if err != nil {
		return nil, fmt.Errorf(
			"ABI for %q not found in target/dev/ even after scarb build: %w",
			contractName, err,
		)
	}

	return abi.ParseFile(path)
}

// ResolveByName resolves an ABI by contract/type name using ONLY local
// discovery (target/dev/ search, optionally running scarb build) — no chain
// fetch, no cache lookup by address. Used to resolve a factory's declared
// ChildABI (e.g. "OptionToken") independent of any specific deployed
// instance, so callers can learn a factory child's event set before any
// child has actually been discovered on chain.
func (r *ABIResolver) ResolveByName(name string) (*abi.ABI, error) {
	return r.resolveLocal(name)
}

// resolveFromChain fetches the contract class ABI from the chain via RPC.
func (r *ABIResolver) resolveFromChain(ctx context.Context, address string) (*abi.ABI, error) {
	if r.fetcher == nil {
		return nil, fmt.Errorf(
			"ABI resolution set to \"fetch\" for contract %s but no RPC provider is available\n"+
				"  hint: provide an explicit ABI path or contract name instead",
			address,
		)
	}

	addressFelt, err := new(felt.Felt).SetString(address)
	if err != nil {
		return nil, fmt.Errorf("invalid contract address %q: %w", address, err)
	}

	classOutput, err := r.fetcher.ClassAt(
		ctx,
		rpc.BlockID{Tag: rpc.BlockTagLatest},
		addressFelt,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"fetching ABI from chain for contract %s: %w\n"+
				"  hint: if this is a proxy contract, use a local ABI file instead",
			address, err,
		)
	}

	return parseClassOutput(classOutput)
}

// parseClassOutput extracts and parses the ABI from an RPC ClassOutput response.
func parseClassOutput(classOutput rpc.ClassOutput) (*abi.ABI, error) {
	switch class := classOutput.(type) {
	case *contracts.ContractClass:
		abiStr := string(class.ABI)
		if abiStr == "" {
			return nil, fmt.Errorf("contract class has no ABI field")
		}
		// The ABI field in a Sierra contract class is a JSON string of ABI entries.
		return abi.Parse([]byte(abiStr))

	case *contracts.DeprecatedContractClass:
		if class.ABI == nil {
			return nil, fmt.Errorf("deprecated contract class has no ABI field")
		}
		abiJSON, err := json.Marshal(class.ABI)
		if err != nil {
			return nil, fmt.Errorf("marshaling deprecated ABI: %w", err)
		}
		return abi.Parse(abiJSON)

	default:
		return nil, fmt.Errorf("unexpected class output type: %T", classOutput)
	}
}

// errAmbiguousMatch is a sentinel used to distinguish "multiple matches" from "not found".
var errAmbiguousMatch = fmt.Errorf("ambiguous ABI match")

// isAmbiguousMatch returns true if the error originated from multiple files matching.
func isAmbiguousMatch(err error) bool {
	return err != nil && strings.Contains(err.Error(), errAmbiguousMatch.Error())
}

// discoverABI searches target/dev/ for a file matching the contract name pattern:
// target/dev/*_{ContractName}.contract_class.json
func discoverABI(contractName string) (string, error) {
	pattern := filepath.Join("target", "dev", fmt.Sprintf("*_%s.contract_class.json", contractName))
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return "", fmt.Errorf("searching for ABI files: %w", err)
	}

	if len(matches) == 0 {
		return "", fmt.Errorf("no ABI file found matching %s", pattern)
	}

	if len(matches) > 1 {
		return "", fmt.Errorf(
			"%w: multiple ABI files found for %q: %s\n"+
				"  hint: use an explicit path in the abi config field",
			errAmbiguousMatch, contractName, strings.Join(matches, ", "),
		)
	}

	return matches[0], nil
}

// runScarbBuild executes `scarb build` in the current directory.
func runScarbBuild() error {
	cmd := exec.Command("scarb", "build")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// isFilePath returns true if the ABI spec looks like a file path rather than
// a contract name or the "fetch" keyword.
func isFilePath(spec string) bool {
	return strings.HasPrefix(spec, "./") ||
		strings.HasPrefix(spec, "/") ||
		strings.HasPrefix(spec, "../") ||
		strings.HasSuffix(spec, ".json")
}
