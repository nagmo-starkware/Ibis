package provider

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/NethermindEth/starknet.go/client"
	"github.com/NethermindEth/starknet.go/rpc"
)

const (
	// maxConcurrentCatchup is the default bound on how many contract goroutines
	// can run RPC-heavy catchup or HTTP-polling iterations simultaneously. On
	// restart with ~300 live contracts all goroutines fire their first GetEvents
	// query at once, tripping Alchemy's rate limit (429) and cascading into CU
	// exhaustion. The semaphore turns that thundering herd into a bounded, steady
	// stream. Matches the view-poller's maxConcurrentPolls value in
	// internal/engine/poller.go. Overridable via indexer.max_concurrent_catchup.
	maxConcurrentCatchup = 16

	// WSS reconnection backoff bounds.
	minBackoff = 1 * time.Second
	maxBackoff = 30 * time.Second

	// Max consecutive WSS dial failures before falling back to polling.
	maxWSSDialFailures = 3

	// Max consecutive WSS session failures (dial succeeds, session drops with
	// zero events) before falling back to polling. Higher than dial failures
	// since session drops can be transient.
	maxWSSSessionFailures = 5

	// Default polling intervals. Overridable per deployment via SubscriberConfig
	// (wired from indexer.catchup_poll_interval / indexer.tip_poll_interval).
	defaultCatchupPollInterval = 100 * time.Millisecond
	defaultTipPollInterval     = 2 * time.Second

	// Blocks behind chain tip that triggers fast catchup polling.
	catchupThreshold uint64 = 50

	// Default number of blocks per polling query.
	defaultBlocksPerQuery uint64 = 100

	// rpcCallTimeout is the maximum duration for individual RPC calls.
	rpcCallTimeout = 30 * time.Second
)

// wssSession represents an active WebSocket subscription session.
// This abstraction allows injecting mock sessions in tests.
type wssSession struct {
	events <-chan *rpc.EmittedEventWithFinalityStatus
	errs   <-chan error
	reorgs <-chan *client.ReorgEvent
	close  func()
}

// wssDialer creates a WSS subscription session for the given parameters.
type wssDialer func(ctx context.Context, wsURL string, input *rpc.EventSubscriptionInput) (*wssSession, error)

// defaultWSSDialer creates a real WSS subscription using starknet.go.
func defaultWSSDialer(ctx context.Context, wsURL string, input *rpc.EventSubscriptionInput) (*wssSession, error) {
	ws, err := rpc.NewWebsocketProvider(ctx, wsURL)
	if err != nil {
		return nil, fmt.Errorf("connecting websocket: %w", err)
	}

	eventCh := make(chan *rpc.EmittedEventWithFinalityStatus, 100)
	sub, err := ws.SubscribeEvents(ctx, eventCh, input)
	if err != nil {
		ws.Close()
		return nil, fmt.Errorf("subscribing to events: %w", err)
	}

	return &wssSession{
		events: eventCh,
		errs:   sub.Err(),
		reorgs: sub.Reorg(),
		close: func() {
			sub.Unsubscribe()
			ws.Close()
		},
	}, nil
}

// ReorgNotification informs the engine about a chain reorganization.
// StartBlock is the first orphaned block; EndBlock is the last.
type ReorgNotification struct {
	StartBlock uint64
	EndBlock   uint64
}

// SubscriberConfig configures the event subscriber behavior.
type SubscriberConfig struct {
	// BlocksPerQuery is the max block range per polling request. Default: 100.
	BlocksPerQuery uint64

	// ForcePolling skips WSS and uses HTTP polling directly.
	ForcePolling bool

	// CatchupWithPolling enables a hybrid mode: each contract first polls
	// historical blocks from StartBlock until within catchupThreshold blocks
	// of chain tip, then switches to WSS for real-time streaming. Useful when
	// the WSS provider accepts a block_id but does not replay older events
	// (observed on Alchemy Starknet Sepolia).
	CatchupWithPolling bool

	// SharedFirehose replaces the per-contract subscription model with a SINGLE
	// all-events WSS subscription, demultiplexed by from_address in-process. Each
	// contract is first caught up over HTTP (as in CatchupWithPolling); then one
	// subscription streams the whole chain's events and the router forwards only
	// those from tracked addresses. Collapses ~N connections + N tip polls into 1.
	// See firehose.go. Mutually exclusive with ForcePolling/CatchupWithPolling.
	SharedFirehose bool

	// TipPollInterval is how often the subscriber re-checks for new blocks while
	// already at chain tip. 0 = defaultTipPollInterval. Should match the interval
	// passed to StarknetProvider.StartTipPoller so the shared tip cache is fresh.
	TipPollInterval time.Duration

	// CatchupPollInterval is the delay between catchup/poll iterations while still
	// more than catchupThreshold blocks behind tip. 0 = defaultCatchupPollInterval.
	CatchupPollInterval time.Duration

	// MaxConcurrentPolls bounds how many contract goroutines may run an RPC-heavy
	// catchup/poll iteration simultaneously. 0 = maxConcurrentCatchup.
	MaxConcurrentPolls int

	// SharedTipPoller makes the subscriber read the chain tip from the provider's
	// shared cache (CachedBlockNumber) instead of issuing a direct BlockNumber per
	// poll. The caller must also start StarknetProvider.StartTipPoller. Implied by
	// SharedFirehose. When false, the subscriber uses direct per-poll BlockNumber
	// (legacy behavior).
	SharedTipPoller bool
}

// EventSubscriber manages per-contract event subscriptions with automatic
// WSS reconnection and HTTP polling fallback.
type EventSubscriber struct {
	provider            *StarknetProvider
	contracts           []ContractSubscription
	events              chan<- RawEvent
	reorgs              chan<- ReorgNotification
	logger              *slog.Logger
	blocksPerQuery      uint64
	forcePolling        bool
	catchupWithPolling  bool
	sharedFirehose      bool
	sharedTipPoller     bool
	tipPollInterval     time.Duration
	catchupPollInterval time.Duration

	// dialWSS creates a WSS session. Override in tests.
	dialWSS wssDialer

	// sem bounds the number of goroutines that can execute RPC-heavy catchup or
	// HTTP-polling iterations concurrently. See maxConcurrentCatchup.
	sem chan struct{}

	// Per-contract cancel functions for dynamic management (per-contract modes).
	mu      sync.Mutex
	cancels map[string]context.CancelFunc

	// router holds per-address forwarding state for the SharedFirehose transport,
	// keyed by from_address hex. Nil in per-contract modes. See firehose.go.
	routerMu sync.RWMutex
	router   map[string]*firehoseSink
}

// NewSubscriber creates an EventSubscriber for the given contracts.
// Events are delivered to the provided channel.
func (p *StarknetProvider) NewSubscriber(contracts []ContractSubscription, events chan<- RawEvent, cfg *SubscriberConfig) *EventSubscriber {
	blocksPerQuery := defaultBlocksPerQuery
	if cfg != nil && cfg.BlocksPerQuery > 0 {
		blocksPerQuery = cfg.BlocksPerQuery
	}

	forcePolling := false
	catchupWithPolling := false
	sharedFirehose := false
	sharedTipPoller := false
	tipInterval := defaultTipPollInterval
	catchupInterval := defaultCatchupPollInterval
	maxConcurrent := maxConcurrentCatchup
	if cfg != nil {
		forcePolling = cfg.ForcePolling
		catchupWithPolling = cfg.CatchupWithPolling
		sharedFirehose = cfg.SharedFirehose
		// The firehose relies on the shared tip cache, so it implies the poller.
		sharedTipPoller = cfg.SharedTipPoller || cfg.SharedFirehose
		if cfg.TipPollInterval > 0 {
			tipInterval = cfg.TipPollInterval
		}
		if cfg.CatchupPollInterval > 0 {
			catchupInterval = cfg.CatchupPollInterval
		}
		if cfg.MaxConcurrentPolls > 0 {
			maxConcurrent = cfg.MaxConcurrentPolls
		}
	}

	return &EventSubscriber{
		provider:            p,
		contracts:           contracts,
		events:              events,
		logger:              p.logger.With("component", "subscriber"),
		blocksPerQuery:      blocksPerQuery,
		forcePolling:        forcePolling,
		catchupWithPolling:  catchupWithPolling,
		sharedFirehose:      sharedFirehose,
		sharedTipPoller:     sharedTipPoller,
		tipPollInterval:     tipInterval,
		catchupPollInterval: catchupInterval,
		dialWSS:             defaultWSSDialer,
		sem:                 make(chan struct{}, maxConcurrent),
		cancels:             make(map[string]context.CancelFunc),
		router:              make(map[string]*firehoseSink),
	}
}

// SetReorgChan sets the channel for reorg notifications. Must be called before Start.
func (s *EventSubscriber) SetReorgChan(ch chan<- ReorgNotification) {
	s.reorgs = ch
}

// Start begins event subscription for all contracts. Blocks until ctx is canceled.
// Each contract gets its own goroutine with independent WSS/polling lifecycle.
func (s *EventSubscriber) Start(ctx context.Context) error {
	if s.sharedFirehose {
		return s.startFirehose(ctx)
	}

	if len(s.contracts) == 0 {
		// No initial contracts is valid when using dynamic registration.
		<-ctx.Done()
		return ctx.Err()
	}

	var wg sync.WaitGroup
	errCh := make(chan error, len(s.contracts))

	for _, contract := range s.contracts {
		wg.Add(1)
		contractCtx, cancel := context.WithCancel(ctx)
		addrHex := contract.Address.String()
		s.mu.Lock()
		s.cancels[addrHex] = cancel
		s.mu.Unlock()

		go func(c ContractSubscription, cCtx context.Context) {
			defer wg.Done()
			if err := s.subscribeContract(cCtx, c); err != nil && ctx.Err() == nil {
				errCh <- fmt.Errorf("contract %s: %w", c.Address, err)
			}
		}(contract, contractCtx)
	}

	wg.Wait()

	select {
	case err := <-errCh:
		return err
	default:
		return ctx.Err()
	}
}

// AddContract dynamically adds a contract subscription to a running subscriber.
// The new subscription runs in its own goroutine with independent lifecycle.
func (s *EventSubscriber) AddContract(ctx context.Context, sub ContractSubscription) {
	if s.sharedFirehose {
		s.addContractFirehose(ctx, sub)
		return
	}

	contractCtx, cancel := context.WithCancel(ctx)
	addrHex := sub.Address.String()

	s.mu.Lock()
	s.cancels[addrHex] = cancel
	s.mu.Unlock()

	go func() {
		if err := s.subscribeContract(contractCtx, sub); err != nil && ctx.Err() == nil {
			s.logger.Error("dynamic contract subscription failed",
				"contract", sub.Address,
				"error", err,
			)
		}
	}()
}

// RemoveContract stops the subscription for a contract by its address hex string.
func (s *EventSubscriber) RemoveContract(addressHex string) {
	if s.sharedFirehose {
		s.removeSink(addressHex)
		return
	}

	s.mu.Lock()
	cancel, ok := s.cancels[addressHex]
	if ok {
		delete(s.cancels, addressHex)
	}
	s.mu.Unlock()

	if ok {
		cancel()
	}
}

// tipBlockNumber returns the current chain tip: from the provider's shared cache
// when the tip poller is enabled (one RPC per interval, shared across contracts),
// otherwise a direct per-poll BlockNumber call (legacy per-contract behavior).
func (s *EventSubscriber) tipBlockNumber(ctx context.Context) (uint64, error) {
	if s.sharedTipPoller {
		return s.provider.CachedBlockNumber(ctx)
	}
	return s.provider.BlockNumber(ctx)
}

// subscribeContract handles the full subscription lifecycle for one contract:
// try WSS first, fall back to polling if WSS fails.
func (s *EventSubscriber) subscribeContract(ctx context.Context, contract ContractSubscription) error {
	logger := s.logger.With("contract", contract.Address)
	lastBlock := contract.StartBlock

	if s.forcePolling {
		return s.pollEvents(ctx, contract, &lastBlock, logger)
	}

	if s.catchupWithPolling {
		if err := s.pollUntilCaughtUp(ctx, contract, &lastBlock, logger); err != nil {
			return err
		}
		logger.Info("catchup complete, switching to WSS", "last_block", lastBlock)
	}

	err := s.subscribeWSS(ctx, contract, &lastBlock, logger)
	if err != nil && ctx.Err() == nil {
		logger.Warn("WSS subscription failed, falling back to polling", "error", err)
		return s.pollEvents(ctx, contract, &lastBlock, logger)
	}

	return err
}

// pollUntilCaughtUp polls events from *lastBlock forward using starknet_getEvents
// and returns once *lastBlock is within catchupThreshold of the chain tip.
// On return, *lastBlock points at the next unprocessed block so the caller can
// hand off to subscribeWSS without gaps. Used by the "catchup" transport mode
// to backfill history when the WSS provider does not replay old events.
func (s *EventSubscriber) pollUntilCaughtUp(ctx context.Context, contract ContractSubscription, lastBlock *uint64, logger *slog.Logger) error {
	logger.Info("starting catchup poll", "from_block", *lastBlock)

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Bound catchup concurrency: acquire the semaphore for the RPC-heavy
		// GetEvents call (the chain tip is served from the shared cache, not an
		// RPC). Released before the inter-iteration sleep so others can proceed.
		select {
		case s.sem <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}

		events, endBlock, latestBlock, rpcErr := s.catchupIteration(ctx, contract, *lastBlock, logger)
		<-s.sem // release before sleeping so other goroutines can run

		if rpcErr != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(s.tipPollInterval):
				continue
			}
		}

		// Close enough to tip — hand off to WSS.
		if latestBlock < catchupThreshold || *lastBlock+catchupThreshold >= latestBlock {
			return nil
		}

		s.resolveTimestamps(ctx, events, logger)

		for _, evt := range events {
			// These are historical events replayed before reaching the tip.
			evt.IsCatchup = true
			select {
			case s.events <- evt:
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		*lastBlock = endBlock + 1

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.catchupPollInterval):
		}
	}
}

// catchupIteration executes one catchup loop step: fetch the chain tip and
// query events for the next block range. Called with the catchup semaphore held.
// Returns the fetched events, the endBlock used, the current chain tip, and any
// RPC error. On error, events is nil; the caller should retry after a delay.
// A nil error with latestBlock within catchupThreshold of *lastBlock signals
// that catchup is complete (check before consuming events).
func (s *EventSubscriber) catchupIteration(ctx context.Context, contract ContractSubscription, lastBlock uint64, logger *slog.Logger) ([]RawEvent, uint64, uint64, error) {
	rpcCtx, cancel := context.WithTimeout(ctx, rpcCallTimeout)
	latestBlock, err := s.tipBlockNumber(rpcCtx)
	cancel()
	if err != nil {
		logger.Warn("failed to get block number during catchup", "error", err)
		return nil, 0, 0, err
	}

	// Signal tip reached without fetching events.
	if latestBlock < catchupThreshold || lastBlock+catchupThreshold >= latestBlock {
		return nil, 0, latestBlock, nil
	}

	endBlock := lastBlock + s.blocksPerQuery
	if endBlock > latestBlock {
		endBlock = latestBlock
	}

	rpcCtx, cancel = context.WithTimeout(ctx, rpcCallTimeout)
	events, err := s.provider.GetEvents(rpcCtx, GetEventsOptions{
		FromBlock: lastBlock,
		ToBlock:   endBlock,
		Address:   contract.Address,
		Keys:      contract.Keys,
		ChunkSize: 1000,
	})
	cancel()
	if err != nil {
		logger.Warn("failed to get events during catchup",
			"error", err, "from", lastBlock, "to", endBlock)
		return nil, 0, 0, err
	}

	logger.Debug("catchup range polled",
		"from", lastBlock, "to", endBlock, "events", len(events), "chain_tip", latestBlock)

	return events, endBlock, latestBlock, nil
}

// subscribeWSS manages a WSS subscription with automatic reconnection using
// exponential backoff (1s → 30s). Falls back to polling after maxWSSDialFailures
// consecutive dial failures or maxWSSSessionFailures consecutive session failures
// (sessions that connect but drop without processing any events).
func (s *EventSubscriber) subscribeWSS(ctx context.Context, contract ContractSubscription, lastBlock *uint64, logger *slog.Logger) error {
	backoff := minBackoff
	consecutiveDialFails := 0
	consecutiveSessionFails := 0

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		subInput := &rpc.EventSubscriptionInput{
			FromAddress: contract.Address,
			Keys:        contract.Keys,
		}
		if *lastBlock > 0 {
			blockNum := *lastBlock
			subInput.SubBlockID = rpc.SubscriptionBlockID{Number: &blockNum}
		}

		// Attempt to dial WSS.
		session, err := s.dialWSS(ctx, s.provider.wsURL, subInput)
		if err != nil {
			consecutiveDialFails++
			if consecutiveDialFails >= maxWSSDialFailures {
				return fmt.Errorf("WSS dial failed %d times: %w", consecutiveDialFails, err)
			}

			logger.Warn("WSS dial failed, retrying",
				"error", err,
				"backoff", backoff,
				"attempt", consecutiveDialFails,
			)

			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}

			backoff = time.Duration(math.Min(float64(backoff)*2, float64(maxBackoff)))
			continue
		}

		// Connected successfully — reset dial failure tracking.
		consecutiveDialFails = 0
		backoff = minBackoff

		logger.Info("WSS subscription active", "from_block", *lastBlock)

		// Process events until session error.
		eventsProcessed, err := s.processWSSEvents(ctx, session, lastBlock, logger)
		session.close()

		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Track session stability: sessions that process zero events are
		// considered unstable (e.g. WSS connects but drops with 1013 timeout).
		if eventsProcessed == 0 {
			consecutiveSessionFails++
			if consecutiveSessionFails >= maxWSSSessionFailures {
				logger.Warn("WSS sessions unstable, falling back to polling",
					"session_failures", consecutiveSessionFails,
					"error", err,
				)
				return fmt.Errorf("WSS session failed %d consecutive times: %w", consecutiveSessionFails, err)
			}
		} else {
			// Session was healthy (processed events) — reset counter.
			consecutiveSessionFails = 0
		}

		logger.Warn("WSS session ended, reconnecting",
			"error", err,
			"events_processed", eventsProcessed,
			"backoff", backoff,
			"resume_block", *lastBlock,
		)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}

		backoff = time.Duration(math.Min(float64(backoff)*2, float64(maxBackoff)))
	}
}

// processWSSEvents reads events from an active WSS session until an error
// occurs or the context is canceled. Returns the number of events successfully
// processed and the error that ended the session.
func (s *EventSubscriber) processWSSEvents(ctx context.Context, session *wssSession, lastBlock *uint64, logger *slog.Logger) (int, error) {
	eventsProcessed := 0
	lastLogBlock := *lastBlock
	for {
		select {
		case <-ctx.Done():
			return eventsProcessed, ctx.Err()

		case err := <-session.errs:
			return eventsProcessed, fmt.Errorf("subscription error: %w", err)

		case reorg := <-session.reorgs:
			if reorg != nil {
				logger.Warn("chain reorganization detected",
					"start_block", reorg.StartBlockNum,
					"end_block", reorg.EndBlockNum,
				)
				// Notify the engine so it can revert stored data.
				if s.reorgs != nil {
					select {
					case s.reorgs <- ReorgNotification{
						StartBlock: reorg.StartBlockNum,
						EndBlock:   reorg.EndBlockNum,
					}:
					case <-ctx.Done():
						return eventsProcessed, ctx.Err()
					}
				}
				// Reset to reorg start so the subscriber re-fetches.
				if reorg.StartBlockNum < *lastBlock {
					*lastBlock = reorg.StartBlockNum
				}
			}

		case evt := <-session.events:
			if evt == nil {
				continue
			}

			var ts uint64
			if t, err := s.provider.GetBlockTimestamp(ctx, evt.BlockNumber); err == nil {
				ts = t
			}

			rawEvent := RawEvent{
				BlockNumber:     evt.BlockNumber,
				BlockHash:       evt.BlockHash,
				TransactionHash: evt.TransactionHash,
				ContractAddress: evt.FromAddress,
				Keys:            evt.Keys,
				Data:            evt.Data,
				FinalityStatus:  string(evt.FinalityStatus),
				Timestamp:       ts,
			}

			select {
			case s.events <- rawEvent:
				eventsProcessed++
				if evt.BlockNumber > *lastBlock {
					*lastBlock = evt.BlockNumber
				}
				if evt.BlockNumber >= lastLogBlock+1000 {
					logger.Info("WSS sync progress", "block", evt.BlockNumber, "events_total", eventsProcessed)
					lastLogBlock = evt.BlockNumber
				}
			case <-ctx.Done():
				return eventsProcessed, ctx.Err()
			}
		}
	}
}

// pollEvents implements the HTTP polling fallback with adaptive timing:
// 100ms when catching up, 2s at chain tip.
func (s *EventSubscriber) pollEvents(ctx context.Context, contract ContractSubscription, lastBlock *uint64, logger *slog.Logger) error {
	logger.Info("starting polling fallback", "from_block", *lastBlock)

	for {
		if ctx.Err() != nil {
			logger.Debug("polling stopped", "reason", ctx.Err())
			return ctx.Err()
		}

		// Bound polling concurrency: acquire the semaphore for the RPC-heavy
		// GetEvents call (the chain tip is served from the shared cache, not an
		// RPC). Released before the inter-iteration sleep so others can run.
		select {
		case s.sem <- struct{}{}:
		case <-ctx.Done():
			logger.Debug("polling stopped", "reason", ctx.Err())
			return ctx.Err()
		}

		rpcCtx, cancel := context.WithTimeout(ctx, rpcCallTimeout)
		latestBlock, err := s.tipBlockNumber(rpcCtx)
		cancel()
		if err != nil {
			<-s.sem
			logger.Warn("failed to get block number", "error", err)
			select {
			case <-ctx.Done():
				logger.Debug("polling stopped", "reason", ctx.Err())
				return ctx.Err()
			case <-time.After(s.tipPollInterval):
				continue
			}
		}

		// At chain tip — release and wait before polling again.
		if *lastBlock > latestBlock {
			<-s.sem
			select {
			case <-ctx.Done():
				logger.Debug("polling stopped", "reason", ctx.Err())
				return ctx.Err()
			case <-time.After(s.tipPollInterval):
				continue
			}
		}

		// Process blocks in chunks of blocksPerQuery.
		endBlock := *lastBlock + s.blocksPerQuery
		if endBlock > latestBlock {
			endBlock = latestBlock
		}

		rpcCtx, cancel = context.WithTimeout(ctx, rpcCallTimeout)
		events, err := s.provider.GetEvents(rpcCtx, GetEventsOptions{
			FromBlock: *lastBlock,
			ToBlock:   endBlock,
			Address:   contract.Address,
			Keys:      contract.Keys,
			ChunkSize: 1000,
		})
		cancel()
		<-s.sem // release before sleeping
		if err != nil {
			logger.Warn("failed to get events", "error", err,
				"from", *lastBlock, "to", endBlock)
			select {
			case <-ctx.Done():
				logger.Debug("polling stopped", "reason", ctx.Err())
				return ctx.Err()
			case <-time.After(s.tipPollInterval):
				continue
			}
		}

		logger.Debug("polled block range", "from", *lastBlock, "to", endBlock, "events", len(events), "chain_tip", latestBlock)

		// Enrich events with block timestamps.
		s.resolveTimestamps(ctx, events, logger)

		// Events are still "catchup" while the polled range trails the chain
		// tip by more than the catchup threshold; once within threshold we
		// treat them as live (same boundary as the catchup->WSS handoff).
		isCatchup := endBlock+catchupThreshold < latestBlock

		for _, evt := range events {
			evt.IsCatchup = isCatchup
			select {
			case s.events <- evt:
			case <-ctx.Done():
				logger.Debug("polling stopped", "reason", ctx.Err())
				return ctx.Err()
			}
		}

		*lastBlock = endBlock + 1

		// Adaptive timing: fast catchup vs slow at tip.
		interval := s.tipPollInterval
		if latestBlock-*lastBlock > catchupThreshold {
			interval = s.catchupPollInterval
		}

		select {
		case <-ctx.Done():
			logger.Debug("polling stopped", "reason", ctx.Err())
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// resolveTimestamps enriches a batch of events with block timestamps.
// Fetches each unique block header once via the provider's cached method.
// Failures are logged but do not block event delivery (timestamp stays 0).
func (s *EventSubscriber) resolveTimestamps(ctx context.Context, events []RawEvent, logger *slog.Logger) {
	// Collect unique block numbers.
	blocks := make(map[uint64]struct{})
	for i := range events {
		blocks[events[i].BlockNumber] = struct{}{}
	}

	// Fetch timestamps (provider caches results).
	timestamps := make(map[uint64]uint64, len(blocks))
	for bn := range blocks {
		ts, err := s.provider.GetBlockTimestamp(ctx, bn)
		if err != nil {
			logger.Debug("failed to fetch block timestamp", "block", bn, "error", err)
			continue
		}
		timestamps[bn] = ts
	}

	// Apply timestamps to events.
	for i := range events {
		if ts, ok := timestamps[events[i].BlockNumber]; ok {
			events[i].Timestamp = ts
		}
	}
}

// Backfill fetches historical events for a contract in the given block range
// and sends them to the events channel. Uses configurable block-range chunking
// (default: 100 blocks per query) with continuation token pagination.
func (s *EventSubscriber) Backfill(ctx context.Context, contract ContractSubscription, fromBlock, toBlock uint64) error {
	logger := s.logger.With("contract", contract.Address, "action", "backfill")
	logger.Info("starting backfill", "from", fromBlock, "to", toBlock)

	for current := fromBlock; current <= toBlock; {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		end := current + s.blocksPerQuery - 1
		if end > toBlock {
			end = toBlock
		}

		rpcCtx, cancel := context.WithTimeout(ctx, rpcCallTimeout)
		events, err := s.provider.GetEvents(rpcCtx, GetEventsOptions{
			FromBlock: current,
			ToBlock:   end,
			Address:   contract.Address,
			Keys:      contract.Keys,
			ChunkSize: 1000,
		})
		cancel()
		if err != nil {
			return fmt.Errorf("backfill events [%d, %d]: %w", current, end, err)
		}

		// Enrich events with block timestamps.
		s.resolveTimestamps(ctx, events, logger)

		for _, evt := range events {
			// Backfill is historical by definition.
			evt.IsCatchup = true
			select {
			case s.events <- evt:
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		logger.Debug("backfill progress", "block", end, "events", len(events))
		current = end + 1
	}

	logger.Info("backfill complete", "from", fromBlock, "to", toBlock)
	return nil
}
