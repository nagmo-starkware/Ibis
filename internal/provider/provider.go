package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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

	// Wildcard and ERC20 are used ONLY by the firehose-keys transport (option
	// D — see firehose_keys.go) to classify a contract's routing: Wildcard is
	// true for a contract configured with a "*" event (no key filter, e.g. an
	// option-family contract); ERC20 is true if the contract's ABI has a
	// Transfer event. Ignored by every other transport.
	Wildcard bool
	ERC20    bool
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

	// Chain-tip cache. A single background poller (StartTipPoller) refreshes the
	// latest block number so the per-contract subscriber goroutines read one
	// shared value via CachedBlockNumber instead of each issuing their own
	// starknet_blockNumber call — which previously scaled tip-polling RPC by the
	// number of live contracts (the dominant CU consumer). tipUpdated is the
	// UnixNano of the last successful refresh; tipIntervalNanos drives the
	// staleness guard in CachedBlockNumber.
	tipBlock         atomic.Uint64
	tipUpdated       atomic.Int64
	tipIntervalNanos atomic.Int64
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

	p := &StarknetProvider{
		httpRPC: httpRPC,
		httpURL: httpURL,
		wsURL:   wsURL,
		logger:  logger,
		tsCache: make(map[uint64]uint64),
	}
	p.tipIntervalNanos.Store(int64(defaultTipPollInterval))
	return p, nil
}

// BlockNumber returns the latest block number directly from the chain (one RPC
// call). Prefer CachedBlockNumber on hot paths — see StartTipPoller.
func (p *StarknetProvider) BlockNumber(ctx context.Context) (uint64, error) {
	return p.httpRPC.BlockNumber(ctx)
}

// StartTipPoller primes the chain-tip cache with one synchronous fetch, then
// launches a background goroutine that refreshes it every interval until ctx is
// canceled. Subscriber goroutines read the cached value via CachedBlockNumber,
// so the process issues a single starknet_blockNumber per interval regardless of
// how many contracts are indexed — previously it was one call per contract per
// poll iteration, the dominant source of RPC/CU consumption. Call once at
// startup, before the subscriber begins, so the cache is warm.
func (p *StarknetProvider) StartTipPoller(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = defaultTipPollInterval
	}
	p.tipIntervalNanos.Store(int64(interval))

	// Prime synchronously so subscribers never observe a zero tip at startup.
	rpcCtx, cancel := context.WithTimeout(ctx, rpcCallTimeout)
	if bn, err := p.httpRPC.BlockNumber(rpcCtx); err == nil {
		p.tipBlock.Store(bn)
		p.tipUpdated.Store(time.Now().UnixNano())
	} else if ctx.Err() == nil {
		p.logger.Warn("tip poller: initial block number fetch failed", "error", err)
	}
	cancel()

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				rpcCtx, cancel := context.WithTimeout(ctx, rpcCallTimeout)
				bn, err := p.httpRPC.BlockNumber(rpcCtx)
				cancel()
				if err != nil {
					if ctx.Err() == nil {
						p.logger.Warn("tip poller: block number refresh failed", "error", err)
					}
					continue
				}
				p.tipBlock.Store(bn)
				p.tipUpdated.Store(time.Now().UnixNano())
			}
		}
	}()
}

// CachedBlockNumber returns the most recently polled chain tip without issuing an
// RPC call in the common case. It falls back to a direct fetch when the cache has
// never been primed, or when the background poller has gone stale (older than 4×
// the poll interval), so callers never read a zero tip or stall behind a wedged
// poller. When a fallback fetch fails but a prior value exists, the stale value
// is returned in preference to an error: a slightly old tip only delays event
// delivery by one poll, whereas an error aborts the caller's iteration.
func (p *StarknetProvider) CachedBlockNumber(ctx context.Context) (uint64, error) {
	bn := p.tipBlock.Load()
	if bn != 0 {
		maxStale := 4 * time.Duration(p.tipIntervalNanos.Load())
		if maxStale <= 0 || time.Since(time.Unix(0, p.tipUpdated.Load())) < maxStale {
			return bn, nil
		}
	}

	fresh, err := p.httpRPC.BlockNumber(ctx)
	if err != nil {
		if bn != 0 {
			return bn, nil
		}
		return 0, err
	}
	p.tipBlock.Store(fresh)
	p.tipUpdated.Store(time.Now().UnixNano())
	return fresh, nil
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
