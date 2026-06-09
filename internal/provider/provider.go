package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/NethermindEth/juno/core/felt"
	"github.com/NethermindEth/starknet.go/rpc"
)

// RawEvent is an unprocessed event received from the Starknet chain.
// The downstream engine is responsible for decoding it via the ABI system.
type RawEvent struct {
	BlockNumber     uint64
	BlockHash       *felt.Felt
	TransactionHash *felt.Felt
	ContractAddress *felt.Felt
	Keys            []*felt.Felt
	Data            []*felt.Felt
	FinalityStatus  string
	Timestamp       uint64
	// IsCatchup is true for events replayed during historical backfill/catchup
	// (before the subscriber reaches the chain tip). Downstream consumers use
	// it to suppress per-event side effects that only make sense for live
	// events — e.g. reactive view re-reads, which would otherwise fire once per
	// replayed historical event. Live (at-tip / WSS) events have it false.
	IsCatchup bool
}

// ContractSubscription defines event subscription parameters for a contract.
type ContractSubscription struct {
	Address    *felt.Felt
	StartBlock uint64
	Keys       [][]*felt.Felt // Optional event key filters
}

// GetEventsOptions configures a GetEvents request.
type GetEventsOptions struct {
	FromBlock uint64
	ToBlock   uint64
	Address   *felt.Felt
	Keys      [][]*felt.Felt
	ChunkSize int // Default: 1000
}

// StarknetProvider manages Starknet RPC communication via HTTP and WebSocket.
type StarknetProvider struct {
	httpRPC *rpc.Provider
	httpURL string
	wsURL   string
	logger  *slog.Logger

	// Block timestamp cache: block number -> Unix timestamp.
	tsMu    sync.RWMutex
	tsCache map[uint64]uint64
}

// New creates a StarknetProvider from an RPC URL.
// Auto-detects the URL scheme and derives both HTTP and WS URLs.
func New(ctx context.Context, rpcURL string, logger *slog.Logger) (*StarknetProvider, error) {
	if logger == nil {
		logger = slog.Default()
	}

	httpURL := ToHTTPURL(rpcURL)
	wsURL := ToWSURL(rpcURL)

	httpRPC, err := rpc.NewProvider(ctx, httpURL)
	if err != nil {
		// starknet.go returns a valid provider alongside ErrIncompatibleVersion
		// when the node's RPC spec version differs from the SDK's expected version.
		// The provider is still usable, so treat this as a warning, not a failure.
		if errors.Is(err, rpc.ErrIncompatibleVersion) && httpRPC != nil {
			logger.Warn("RPC spec version mismatch (provider still usable)", "error", err)
		} else {
			return nil, fmt.Errorf("creating HTTP provider: %w", err)
		}
	}

	return &StarknetProvider{
		httpRPC: httpRPC,
		httpURL: httpURL,
		wsURL:   wsURL,
		logger:  logger,
		tsCache: make(map[uint64]uint64),
	}, nil
}

// BlockNumber returns the latest block number from the chain.
func (p *StarknetProvider) BlockNumber(ctx context.Context) (uint64, error) {
	return p.httpRPC.BlockNumber(ctx)
}

// GetBlockTimestamp returns the Unix timestamp for a block, using a cache
// to avoid redundant RPC calls. Multiple events in the same block share one fetch.
func (p *StarknetProvider) GetBlockTimestamp(ctx context.Context, blockNumber uint64) (uint64, error) {
	p.tsMu.RLock()
	if ts, ok := p.tsCache[blockNumber]; ok {
		p.tsMu.RUnlock()
		return ts, nil
	}
	p.tsMu.RUnlock()

	blockID := rpc.BlockID{Number: &blockNumber}
	result, err := p.httpRPC.BlockWithTxHashes(ctx, blockID)
	if err != nil {
		return 0, fmt.Errorf("fetching block %d header: %w", blockNumber, err)
	}

	var ts uint64
	switch b := result.(type) {
	case *rpc.BlockTxHashes:
		ts = b.Timestamp
	case *rpc.PreConfirmedBlockTxHashes:
		ts = b.Timestamp
	default:
		return 0, fmt.Errorf("unexpected block type for block %d", blockNumber)
	}

	p.tsMu.Lock()
	p.tsCache[blockNumber] = ts
	// Evict old entries to bound memory (keep last 10000 blocks).
	if len(p.tsCache) > 10000 {
		for k := range p.tsCache {
			if k < blockNumber-10000 {
				delete(p.tsCache, k)
			}
		}
	}
	p.tsMu.Unlock()

	return ts, nil
}

// GetEvents fetches events in a block range, automatically paginating via
// continuation tokens. ChunkSize defaults to 1000 if not set.
func (p *StarknetProvider) GetEvents(ctx context.Context, opts GetEventsOptions) ([]RawEvent, error) {
	chunkSize := opts.ChunkSize
	if chunkSize <= 0 {
		chunkSize = 1000
	}

	fromBlock := opts.FromBlock
	toBlock := opts.ToBlock

	var allEvents []RawEvent
	token := ""

	for {
		if ctx.Err() != nil {
			return allEvents, ctx.Err()
		}

		input := rpc.EventsInput{
			EventFilter: rpc.EventFilter{
				FromBlock: rpc.BlockID{Number: &fromBlock},
				ToBlock:   rpc.BlockID{Number: &toBlock},
				Address:   opts.Address,
				Keys:      opts.Keys,
			},
			ResultPageRequest: rpc.ResultPageRequest{
				ChunkSize:         chunkSize,
				ContinuationToken: token,
			},
		}

		chunk, err := p.httpRPC.Events(ctx, input)
		if err != nil {
			return allEvents, fmt.Errorf("fetching events: %w", err)
		}

		for i := range chunk.Events {
			e := &chunk.Events[i]
			allEvents = append(allEvents, RawEvent{
				BlockNumber:     e.BlockNumber,
				BlockHash:       e.BlockHash,
				TransactionHash: e.TransactionHash,
				ContractAddress: e.FromAddress,
				Keys:            e.Keys,
				Data:            e.Data,
			})
		}

		if chunk.ContinuationToken == "" {
			break
		}
		token = chunk.ContinuationToken
	}

	return allEvents, nil
}

// Call executes a read-only function call on a Starknet contract.
// Returns the raw felt array result from starknet_call.
func (p *StarknetProvider) Call(ctx context.Context, contractAddress, entryPointSelector *felt.Felt, calldata []*felt.Felt, blockID rpc.BlockID) ([]*felt.Felt, error) {
	return p.httpRPC.Call(ctx, rpc.FunctionCall{
		ContractAddress:    contractAddress,
		EntryPointSelector: entryPointSelector,
		Calldata:           calldata,
	}, blockID)
}

// ClassAt fetches the contract class at the given address.
// Satisfies the config.ABIFetcher interface for chain-based ABI resolution.
func (p *StarknetProvider) ClassAt(ctx context.Context, blockID rpc.BlockID, contractAddress *felt.Felt) (rpc.ClassOutput, error) {
	return p.httpRPC.ClassAt(ctx, blockID, contractAddress)
}

// GetClassAt fetches the contract class (ABI) at the given address as raw JSON.
func (p *StarknetProvider) GetClassAt(ctx context.Context, address *felt.Felt) (json.RawMessage, error) {
	result, err := p.httpRPC.ClassAt(ctx, rpc.BlockID{Tag: rpc.BlockTagLatest}, address)
	if err != nil {
		return nil, fmt.Errorf("fetching class at %s: %w", address, err)
	}

	raw, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("marshaling class: %w", err)
	}

	return raw, nil
}

// WSURL returns the WebSocket URL derived from the configured RPC URL.
func (p *StarknetProvider) WSURL() string {
	return p.wsURL
}

// HTTPURL returns the HTTP URL derived from the configured RPC URL.
func (p *StarknetProvider) HTTPURL() string {
	return p.httpURL
}

// Close releases provider resources.
func (p *StarknetProvider) Close() error {
	return nil
}

// ToHTTPURL converts any RPC URL to its HTTP equivalent.
func ToHTTPURL(url string) string {
	if strings.HasPrefix(url, "wss://") {
		return "https://" + strings.TrimPrefix(url, "wss://")
	}
	if strings.HasPrefix(url, "ws://") {
		return "http://" + strings.TrimPrefix(url, "ws://")
	}
	return url
}

// ToWSURL converts any RPC URL to its WebSocket equivalent.
func ToWSURL(url string) string {
	if strings.HasPrefix(url, "https://") {
		return "wss://" + strings.TrimPrefix(url, "https://")
	}
	if strings.HasPrefix(url, "http://") {
		return "ws://" + strings.TrimPrefix(url, "http://")
	}
	return url
}
