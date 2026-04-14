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
	// WSS reconnection backoff bounds.
	minBackoff = 1 * time.Second
	maxBackoff = 30 * time.Second

	// Max consecutive WSS dial failures before falling back to polling.
	maxWSSDialFailures = 3

	// Max consecutive WSS session failures (dial succeeds, session drops with
	// zero events) before falling back to polling. Higher than dial failures
	// since session drops can be transient.
	maxWSSSessionFailures = 5

	// Polling intervals.
	catchupPollInterval = 100 * time.Millisecond
	tipPollInterval     = 2 * time.Second

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
}

// EventSubscriber manages per-contract event subscriptions with automatic
// WSS reconnection and HTTP polling fallback.
type EventSubscriber struct {
	provider       *StarknetProvider
	contracts      []ContractSubscription
	events         chan<- RawEvent
	reorgs         chan<- ReorgNotification
	logger         *slog.Logger
	blocksPerQuery uint64

	// dialWSS creates a WSS session. Override in tests.
	dialWSS wssDialer

	// Per-contract cancel functions for dynamic management.
	mu      sync.Mutex
	cancels map[string]context.CancelFunc
}

// NewSubscriber creates an EventSubscriber for the given contracts.
// Events are delivered to the provided channel.
func (p *StarknetProvider) NewSubscriber(contracts []ContractSubscription, events chan<- RawEvent, cfg *SubscriberConfig) *EventSubscriber {
	blocksPerQuery := defaultBlocksPerQuery
	if cfg != nil && cfg.BlocksPerQuery > 0 {
		blocksPerQuery = cfg.BlocksPerQuery
	}

	return &EventSubscriber{
		provider:       p,
		contracts:      contracts,
		events:         events,
		logger:         p.logger.With("component", "subscriber"),
		blocksPerQuery: blocksPerQuery,
		dialWSS:        defaultWSSDialer,
		cancels:        make(map[string]context.CancelFunc),
	}
}

// SetReorgChan sets the channel for reorg notifications. Must be called before Start.
func (s *EventSubscriber) SetReorgChan(ch chan<- ReorgNotification) {
	s.reorgs = ch
}

// Start begins event subscription for all contracts. Blocks until ctx is canceled.
// Each contract gets its own goroutine with independent WSS/polling lifecycle.
func (s *EventSubscriber) Start(ctx context.Context) error {
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

// subscribeContract handles the full subscription lifecycle for one contract:
// try WSS first, fall back to polling if WSS fails.
func (s *EventSubscriber) subscribeContract(ctx context.Context, contract ContractSubscription) error {
	logger := s.logger.With("contract", contract.Address)
	lastBlock := contract.StartBlock

	err := s.subscribeWSS(ctx, contract, &lastBlock, logger)
	if err != nil && ctx.Err() == nil {
		logger.Warn("WSS subscription failed, falling back to polling", "error", err)
		return s.pollEvents(ctx, contract, &lastBlock, logger)
	}

	return err
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

			rawEvent := RawEvent{
				BlockNumber:     evt.BlockNumber,
				BlockHash:       evt.BlockHash,
				TransactionHash: evt.TransactionHash,
				ContractAddress: evt.FromAddress,
				Keys:            evt.Keys,
				Data:            evt.Data,
				FinalityStatus:  string(evt.FinalityStatus),
			}

			select {
			case s.events <- rawEvent:
				eventsProcessed++
				if evt.BlockNumber > *lastBlock {
					*lastBlock = evt.BlockNumber
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

		rpcCtx, cancel := context.WithTimeout(ctx, rpcCallTimeout)
		latestBlock, err := s.provider.BlockNumber(rpcCtx)
		cancel()
		if err != nil {
			logger.Warn("failed to get block number", "error", err)
			select {
			case <-ctx.Done():
				logger.Debug("polling stopped", "reason", ctx.Err())
				return ctx.Err()
			case <-time.After(tipPollInterval):
				continue
			}
		}

		// At chain tip — wait before polling again.
		if *lastBlock > latestBlock {
			select {
			case <-ctx.Done():
				logger.Debug("polling stopped", "reason", ctx.Err())
				return ctx.Err()
			case <-time.After(tipPollInterval):
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
		if err != nil {
			logger.Warn("failed to get events", "error", err,
				"from", *lastBlock, "to", endBlock)
			select {
			case <-ctx.Done():
				logger.Debug("polling stopped", "reason", ctx.Err())
				return ctx.Err()
			case <-time.After(tipPollInterval):
				continue
			}
		}

		logger.Debug("polled block range", "from", *lastBlock, "to", endBlock, "events", len(events))

		for _, evt := range events {
			select {
			case s.events <- evt:
			case <-ctx.Done():
				logger.Debug("polling stopped", "reason", ctx.Err())
				return ctx.Err()
			}
		}

		*lastBlock = endBlock + 1

		// Adaptive timing: fast catchup vs slow at tip.
		interval := tipPollInterval
		if latestBlock-*lastBlock > catchupThreshold {
			interval = catchupPollInterval
		}

		select {
		case <-ctx.Done():
			logger.Debug("polling stopped", "reason", ctx.Err())
			return ctx.Err()
		case <-time.After(interval):
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

		for _, evt := range events {
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
